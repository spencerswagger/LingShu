// proxy.go 是转发本体：把入站请求原样发给 magpie（只换凭证头 + model），
// 再把上游响应原样写给下游，并在旁路读 usage。
//
// 超时口径（契约）：
//   - 连不上 / 建连失败 / DNS        → 503 + Retry-After: 5
//   - 首包超时（收不到响应头）        → 504
//   - 流空闲超时（连续无数据）        → 中断连接（响应头已发出，无法再改状态码）
//   - 客户端中途取消                  → 立即取消上游，保留已读到的 usage
package magpie

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Result 是一次转发的事实汇总（供第③件计费/对账挂钩使用；本件只产出、不消费）。
type Result struct {
	Endpoint      string
	Protocol      Protocol
	TokenID       int64
	UserID        int64
	ExternalModel string
	MagpieModel   string
	CallerKeyName string

	// Status 是上游返回的 HTTP 状态码（0 = 没拿到响应）。
	Status int
	// Usage/HasUsage 是旁路读到的归一化用量及其是否存在。
	Usage    Usage
	HasUsage bool

	Streamed bool
	// Canceled 表示客户端中途断开（usage 仍可用于计费）。
	Canceled bool
	// TimedOut 表示首包或流空闲超时。
	TimedOut bool
	// NoContent 表示上游返回 204/304（无内容可计费，不计费也不算失败成功）。
	NoContent bool
	// Billable 表示该端点参与计费（count_tokens 类为 false）。
	Billable bool
	// CredentialMasked 表示响应体命中了「本次凭据回显」并已替换为 <redacted>
	// （契约 §8.1 的唯一豁免项；未命中时字节完全不变）。
	CredentialMasked bool
	// UnmaskableEncoding 非空表示上游用了 stdlib 解不了的压缩编码（如 br），
	// 只能按原字节透传，遮蔽豁免在该响应上不成立——handler 会落告警。
	UnmaskableEncoding string

	Duration time.Duration
	Err      error
}

// Hook 是转发结束回调（第③件的接入点：计费/退预扣/对账在这里接）。
// 本件不注入（nil），因此不会产生任何账单或扣费动作。
type Hook func(ctx context.Context, res Result)

// Proxy 是无状态的转发执行器（并发安全）。
type Proxy struct {
	cfg   Config
	plain *http.Client
	sse   *http.Client
	hook  Hook
}

// NewProxy 装配转发执行器：两个 client 只在「响应头超时」上不同
// （非流式用总超时，流式用首包超时），建连超时与连接池共用。
func NewProxy(cfg Config) *Proxy {
	cfg = cfg.normalized()
	dialer := &net.Dialer{Timeout: cfg.Connect, KeepAlive: 30 * time.Second}
	newTransport := func(headerTimeout time.Duration) *http.Transport {
		return &http.Transport{
			DialContext:           dialer.DialContext,
			ResponseHeaderTimeout: headerTimeout,
			// 不启用自动压缩：请求侧已固定 Accept-Encoding: identity，响应侧也不能二次解压，
			// 否则下游收到的字节与上游发出的不一致（违反「响应字节原样」契约）。
			DisableCompression:    true,
			MaxIdleConns:          128,
			MaxIdleConnsPerHost:   64,
			IdleConnTimeout:       90 * time.Second,
			ExpectContinueTimeout: time.Second,
			ForceAttemptHTTP2:     false,
		}
	}
	return &Proxy{
		cfg:   cfg,
		plain: &http.Client{Transport: newTransport(cfg.Request)},
		sse:   &http.Client{Transport: newTransport(cfg.StreamFirstByte)},
	}
}

// SetOnComplete 注入转发结束回调（第③件使用；本件保持 nil）。
func (p *Proxy) SetOnComplete(fn Hook) { p.hook = fn }

// Forward 执行一次转发。prep 是准入/改写后的产物。
//
// 注意：无论成功失败，本函数都会调用一次 hook（若已注入），Billable=false 的端点除外。
func (p *Proxy) Forward(w http.ResponseWriter, r *http.Request, route Route, prep Prepared) Result {
	start := time.Now()
	res := Result{
		Endpoint:      route.Endpoint,
		Protocol:      route.Protocol,
		TokenID:       prep.TokenID,
		UserID:        prep.UserID,
		ExternalModel: prep.ExternalModel,
		MagpieModel:   prep.MagpieModel,
		CallerKeyName: prep.CallerKeyName,
		Billable:      route.Billable,
	}
	defer func() {
		res.Duration = time.Since(start)
		if p.hook != nil && route.Billable {
			p.hook(r.Context(), res)
		}
	}()

	// 1) 组出站请求：path 已按协议改写，query 原样保留——
	// 唯一例外是「用作我们鉴权的 ?key=」：那是租户令牌，必须剔除，其余参数逐字保留。
	u := p.cfg.BaseURL + prep.UpstreamPath
	if q := r.URL.RawQuery; q != "" {
		if prep.StripQueryKey {
			q = stripQueryParam(q, "key")
		}
		if q != "" {
			u += "?" + q
		}
	}

	streaming := route.StreamHint || (!route.ModelInPath && prep.StreamRequested)
	ctx := r.Context()
	var cancel context.CancelFunc
	switch {
	case !route.Billable:
		ctx, cancel = context.WithTimeout(ctx, p.cfg.CountTokens)
	case streaming:
		// 流式不设总超时：由「首包超时 + 读空闲超时」两头夹住，长输出不会被误杀。
		ctx, cancel = context.WithCancel(ctx)
	default:
		ctx, cancel = context.WithTimeout(ctx, p.cfg.Request)
	}
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(prep.Body))
	if err != nil {
		res.Err = fmt.Errorf("build upstream request: %w", err)
		WriteDeny(w, route.Protocol, Deny{Status: http.StatusBadGateway, Code: CodeServiceUnavailable, Message: "转发请求构建失败"})
		return res
	}
	copyRequestHeaders(req.Header, r.Header)
	req.Header.Set(p.cfg.AuthHeader, p.cfg.credentialHeaderValue(prep.CallerKey))
	if len(prep.Body) > 0 {
		req.ContentLength = int64(len(prep.Body))
	}

	client := p.plain
	if streaming {
		client = p.sse
	}
	resp, err := client.Do(req)
	if err != nil {
		// 传值返回会丢掉失败标记（TimedOut/Canceled），必须回写外层 res。
		res = p.failTransport(w, route, res, err, r.Context())
		return res
	}
	defer resp.Body.Close()

	// 2) 凭据回显遮蔽（契约 §8.1，是「响应字节原样」的唯一豁免项）：
	//    只把**本次请求实际用到的那把**凭据（含 Authorization 形态）在响应体里命中的子串
	//    替换成 <redacted>，全部状态码生效；无命中则零改动（字节与 Content-Length 都保持原样）。
	//    不做正则泛化，避免误伤正常返回内容。
	//
	//    上游无视 Accept-Encoding: identity 仍然压缩时先解压再遮蔽：压缩字节里定位不到明文，
	//    硬遮蔽只能靠解压。stdlib 解不了的编码（如 br）记 UnmaskableEncoding 后按原字节透传，
	//    handler 会落一条告警（边界见设计稿风险段）。
	needle := prep.CallerKey
	body := io.Reader(resp.Body)
	decoded := false
	if needle != "" {
		switch enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); enc {
		case "", "identity":
		case "gzip", "x-gzip":
			if gz, err := gzip.NewReader(resp.Body); err == nil {
				body, decoded = gz, true
			} else {
				res.UnmaskableEncoding = enc
			}
		case "deflate":
			if zr, err := zlib.NewReader(resp.Body); err == nil {
				body, decoded = zr, true
			} else {
				res.UnmaskableEncoding = enc
			}
		default:
			res.UnmaskableEncoding = enc
		}
	}

	// 3) 透传响应头与状态码（上游错误同样原样回流，不二次包装）。
	res.Status = resp.StatusCode
	res.NoContent = resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified
	isSSE := isEventStream(resp.Header.Get("Content-Type"))
	res.Streamed = isSSE
	collector := newUsageCollector(route.Protocol, isSSE)
	copyResponseHeaders(w.Header(), resp.Header)
	if decoded {
		// 已解压：编码头与压缩前长度都不再成立。
		w.Header().Del("Content-Encoding")
		w.Header().Del("Content-Length")
	}
	if res.NoContent {
		w.WriteHeader(resp.StatusCode)
		return res
	}

	if needle != "" && !isSSE {
		// 非流式：整段读，才知道是否命中——没命中要连 Content-Length 一起保持原样。
		p.serveMaskedBuffered(w, body, collector, needle, route, &res)
		return res
	}
	if needle != "" {
		// 流式：长度可能变（遮蔽后变长），交 chunked，避免长度头与实际字节不符。
		w.Header().Del("Content-Length")
	}
	w.WriteHeader(resp.StatusCode)

	// 4) 旁路读 usage（只读原始字节：遮蔽只作用于下行，不能污染 usage 解析）。
	if route.Billable {
		if err := p.copyBody(w, body, collector, isSSE, cancel, r.Context(), &res, needle); err != nil {
			res.Err = err
		}
		collector.finish()
		if u, ok := collector.Result(); ok {
			res.Usage, res.HasUsage = u, true
		}
	} else {
		// 计数端点不计费、不读 usage：直接透传，避免无谓的内存与解析开销。
		if err := p.copyBody(w, body, nil, isSSE, cancel, r.Context(), &res, needle); err != nil {
			res.Err = err
		}
	}
	return res
}

// redactedMarker 是凭据回显的替换物（契约 §8.1 固定写法 <redacted>）。
const redactedMarker = "<redacted>"

// maskBufferLimit 是非流式遮蔽的整段读取上限；超过则退回流式遮蔽（长度交 chunked）。
const maskBufferLimit = 32 << 20

// serveMaskedBuffered 处理非流式响应：整段读 → 命中才替换 → 重算 Content-Length；
// 没命中则字节与上游完全一致（零改动）。header 由本函数写（要在知道长度之后）。
func (p *Proxy) serveMaskedBuffered(w http.ResponseWriter, body io.Reader, collector *usageCollector,
	needle string, route Route, res *Result) {
	raw, err := io.ReadAll(io.LimitReader(body, maskBufferLimit+1))
	if err != nil {
		res.Err = fmt.Errorf("读取上游响应失败: %w", err)
		WriteDeny(w, route.Protocol, Deny{Status: http.StatusBadGateway, Code: CodeServiceUnavailable, Message: "读取上游响应失败"})
		return
	}
	if collector != nil {
		collector.Feed(raw)
	}
	if len(raw) > maskBufferLimit {
		// 超大响应：退回流式遮蔽（已读部分先写，剩余继续流式）。
		w.Header().Del("Content-Length")
		w.WriteHeader(res.Status)
		masked := maskCredentialEcho(raw, needle, res)
		if _, werr := w.Write(masked); werr != nil {
			res.Canceled = true
			return
		}
		if collector != nil {
			collector.finish()
			if u, ok := collector.Result(); ok {
				res.Usage, res.HasUsage = u, true
			}
		}
		return
	}

	masked := maskCredentialEcho(raw, needle, res)
	if len(masked) == len(raw) && bytes.Equal(masked, raw) {
		// 零改动：保留上游的 Content-Length（未命中时不重算）。
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Length", strconv.Itoa(len(raw)))
	} else {
		w.Header().Set("Content-Length", strconv.Itoa(len(masked)))
	}
	w.WriteHeader(res.Status)
	if _, werr := w.Write(masked); werr != nil {
		res.Canceled = true
		return
	}
	if collector != nil {
		collector.finish()
		if u, ok := collector.Result(); ok {
			res.Usage, res.HasUsage = u, true
		}
	}
}

// maskCredentialEcho 只替换本次请求实际使用的那把凭据（及其 Authorization 形态）；
// 命中时记 res.CredentialMasked，未命中返回原切片（调用方按长度判定零改动）。
func maskCredentialEcho(raw []byte, needle string, res *Result) []byte {
	if needle == "" || len(raw) == 0 {
		return raw
	}
	out := maskCredentialIn(raw, needle)
	// Authorization 形态（`Bearer <key>`）也要遮：上游会把整段头回显出来。
	out = maskCredentialIn(out, "Bearer "+needle)
	if !bytes.Equal(out, raw) {
		res.CredentialMasked = true
	}
	return out
}

// maskCredentialIn 做单次全量替换（needle 为空时原样返回）。
func maskCredentialIn(raw []byte, needle string) []byte {
	if needle == "" || !bytes.Contains(raw, []byte(needle)) {
		return raw
	}
	return bytes.ReplaceAll(raw, []byte(needle), []byte(redactedMarker))
}

// credentialMasker 做**流式**替换：SSE 分包会把凭据切成两半，
// 于是尾部留下「可能是凭据前缀」的字节，等下一包到齐再判断；未命中的字节原样发出。
type credentialMasker struct {
	needle string
	carry  []byte
}

// feed 处理一段上游字节，返回可立即写给下游的字节（可能为空）。
func (m *credentialMasker) feed(chunk []byte) []byte {
	if m.needle == "" {
		return chunk
	}
	buf := chunk
	if len(m.carry) > 0 {
		buf = append(append([]byte(nil), m.carry...), chunk...)
		m.carry = m.carry[:0]
	}
	var out []byte
	for {
		idx := bytes.Index(buf, []byte(m.needle))
		if idx < 0 {
			break
		}
		out = append(out, buf[:idx]...)
		out = append(out, redactedMarker...)
		buf = buf[idx+len(m.needle):]
	}
	// 尾部若可能是凭据前缀就先留着（最多 len(needle)-1 字节）。
	keep := partialPrefixLen(buf, m.needle)
	if keep > 0 {
		m.carry = append(m.carry, buf[len(buf)-keep:]...)
	}
	return append(out, buf[:len(buf)-keep]...)
}

// flush 在流结束时把暂存的尾部发完（此处已不可能是凭据前缀）。
func (m *credentialMasker) flush() []byte {
	if len(m.carry) == 0 {
		return nil
	}
	out := append([]byte(nil), m.carry...)
	m.carry = m.carry[:0]
	return out
}

// partialPrefixLen 返回 data 尾部与 needle 前缀重合的最大长度（不含整段）。
func partialPrefixLen(data []byte, needle string) int {
	max := len(needle) - 1
	if max > len(data) {
		max = len(data)
	}
	for k := max; k > 0; k-- {
		if bytes.HasSuffix(data, []byte(needle[:k])) {
			return k
		}
	}
	return 0
}

// copyBody 把上游响应体写下游：流式逐块 flush，同时旁路喂 usage 采集器。
// 读空闲超时通过 cancel 触发（响应头已发出，只能中断连接）。
func (p *Proxy) copyBody(w http.ResponseWriter, body io.Reader, collector *usageCollector,
	isSSE bool, cancel context.CancelFunc, downstream context.Context, res *Result, needle string) error {
	// 流式遮蔽：SSE 分包可能把凭据切成两半，尾部暂存等下一包（未命中时字节不变）。
	masker := &credentialMasker{needle: needle}
	buf := make([]byte, 32*1024)
	var idleFired atomic.Bool
	var idle *time.Timer
	if isSSE {
		idle = time.AfterFunc(p.cfg.StreamIdle, func() {
			idleFired.Store(true)
			cancel()
		})
		defer idle.Stop()
	}
	flusher, _ := w.(http.Flusher)
	for {
		if idle != nil {
			idle.Reset(p.cfg.StreamIdle)
		}
		n, err := body.Read(buf)
		if n > 0 {
			if collector != nil {
				// 采集器吃原始字节：遮蔽只作用于下行，不能污染 usage 解析。
				collector.Feed(buf[:n])
			}
			out := masker.feed(buf[:n])
			if len(out) > 0 {
				if !bytes.Equal(out, buf[:n]) {
					res.CredentialMasked = true
				}
				if _, werr := w.Write(out); werr != nil {
					// 下游断开（客户端取消/超时）：立即停止并标记，保留已读到的 usage。
					res.Canceled = true
					cancel()
					return nil
				}
			}
			if isSSE && flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				// 流结束：把暂存的尾部（不再可能是凭据前缀）发完。
				if tail := masker.flush(); len(tail) > 0 {
					if _, werr := w.Write(tail); werr != nil {
						res.Canceled = true
						cancel()
						return nil
					}
					if isSSE && flusher != nil {
						flusher.Flush()
					}
				}
				return nil
			}
			switch {
			case idleFired.Load():
				res.TimedOut = true
				return fmt.Errorf("上游流空闲超时（>%s 无数据）", p.cfg.StreamIdle)
			case downstream.Err() != nil:
				res.Canceled = true
				res.TimedOut = errors.Is(downstream.Err(), context.DeadlineExceeded)
				return nil
			default:
				return fmt.Errorf("读取上游响应失败: %w", err)
			}
		}
	}
}

// failTransport 把传输层失败映射为契约里的状态码与错误体。
func (p *Proxy) failTransport(w http.ResponseWriter, route Route, res Result, err error, downstream context.Context) Result {
	res.Err = err
	// 下游已断开：无需（也无法）写响应。
	if downstream.Err() != nil {
		res.Canceled = true
		res.TimedOut = errors.Is(downstream.Err(), context.DeadlineExceeded)
		return res
	}

	d := Deny{Status: http.StatusServiceUnavailable, Code: CodeServiceUnavailable, Message: "代理侧不可用，请稍后重试", RetryAfter: "5"}
	switch {
	case isDialError(err):
		// 连不上 / DNS / 建连超时：可重试。
	case isTimeoutError(err):
		res.TimedOut = true
		d = Deny{Status: http.StatusGatewayTimeout, Code: CodeUpstreamTimeout, Message: "代理侧响应超时，请稍后重试"}
	}
	WriteDeny(w, route.Protocol, d)
	return res
}

// isDialError 判定建连阶段失败（含 DNS 解析、TCP 连接、建连超时）。
func isDialError(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) && opErr.Op == "dial" {
		return true
	}
	var dnsErr *net.DNSError
	return errors.As(err, &dnsErr)
}

// isTimeoutError 判定「已建连但等不到响应」的超时（首包超时 / 总超时）。
func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}
	return false
}

// isEventStream 判定上游响应是否 SSE。
func isEventStream(contentType string) bool {
	return len(contentType) >= 17 && contentType[:17] == "text/event-stream"
}

// stripQueryParam 从原始 query 串里剔掉指定参数（按 & 切段、保持其余段原样，
// 不重新编码：调用方可能依赖原始顺序/转义形式）。
func stripQueryParam(raw, name string) string {
	if raw == "" {
		return ""
	}
	parts := strings.Split(raw, "&")
	out := make([]string, 0, len(parts))
	for _, seg := range parts {
		if seg == "" {
			continue
		}
		key := seg
		if i := strings.IndexByte(seg, '='); i >= 0 {
			key = seg[:i]
		}
		if key == name {
			continue
		}
		out = append(out, seg)
	}
	return strings.Join(out, "&")
}
