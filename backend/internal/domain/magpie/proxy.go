// proxy.go 是转发本体：把入站请求发给上游（只换凭证头 + model），
// 再把上游响应**字节原样**写给下游，旁路读 usage。
//
// 本包的职责边界（D1 裁定，逐字执行）：
//   - **只做**协议识别（protocols.go）、路径/请求体里的 model 改写（rewrite.go）、头策略（headers.go）、
//     字节转发与 usage 旁路读取；
//   - **不做**预扣、不写 billing_records、不读归因以外的账务数据 —— 这些留在主链
//     （internal/domain/gateway/upstream.go）。本包只把事实交给两个钩子：
//     OnUpstreamResponse：回程头到达时（归因定稿）；
//     OnDone：转发结束时（usage/状态定稿，主链据此落一条账单）。
//
// 字节原样（D12 裁定）：响应体不做任何内容改写——不按白名单裁剪字段、不做文案映射、
// 不裁剪错误体。唯一允许的差异是「hop-by-hop / 上游标识头被剔除」（headers.go）。
//
// 超时口径（F1 的两段超时）：
//   - 等响应头超过 StreamFirstByte → 504（此时还没写下游响应头，改得动状态码）；
//   - 流式/非流式读体过程中连续 StreamIdle 无数据 → 断流（响应头已发出，改不了状态码）；
//   - 客户端中途断开 → 取消上游，但已读到的 usage 仍然交给 OnDone（已发生的调用要计费）。
package magpie

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Timeouts 是转发链的两段超时（与 sys_configs 的 upstream.timeouts 同口径）。
type Timeouts struct {
	// StreamFirstByte 是等响应头的上限。
	StreamFirstByte time.Duration
	// StreamIdle 是读体过程中「连续多久没有数据」的上限。
	StreamIdle time.Duration
}

// ForwardRequest 是一次转发的入参（全部由主链准备好）。
type ForwardRequest struct {
	// BaseURL 是上游基址（来自 sys_configs，不带尾部斜杠）。
	BaseURL string
	// Path 是改写后的上游 path（Gemini 的 model 段已替换；其余协议与入站一致）。
	Path string
	// Query 是入站 query 原样（F6）；只有 asQueryKey 为 true 时才剔除 `key`。
	Query string
	// StripQueryKey 表示本次鉴权用的是 query 里的 `key`（Gemini 原生写法），
	// 转发时必须剔除：否则把租户令牌带给上游。
	StripQueryKey bool
	// Header 是入站 header（本包负责按黑名单剔除后透传）。
	Header http.Header
	// Body 是改写后的请求体（除 model 外与入站逐字节一致）。
	Body []byte
	// Streaming 表示响应按 SSE 处理（流式：不设总超时，靠首包 + 空闲夹住）。
	Streaming bool
	// Credential 在出站请求上注入上游鉴权头（凭据只在主链手里，不进本包字段）。
	Credential func(h http.Header)
	// RequestID 是灵枢自己的链路 ID，回写给下游（覆盖上游的 x-request-id）。
	RequestID string
	// Billable 表示该端点参与计费（count_tokens 类为 false：不读 usage）。
	Billable bool
	// Protocol 决定 usage 字段路径。
	Protocol Protocol
	// OnUpstreamResponse 在回程头到达时调用（归因定稿；nil = 不关心）。
	OnUpstreamResponse func(ctx context.Context, f ResponseFacts)
	// OnDone 在转发结束时调用一次（含失败；nil = 不关心）。
	OnDone func(ctx context.Context, res Result)
}

// ResponseFacts 是回程头到达时的已知事实（D1：OnUpstreamResponse 交主链消费）。
type ResponseFacts struct {
	Status   int
	Header   http.Header
	Boundary Attribution
	Streamed bool
}

// Result 是一次转发的事实汇总（交给主链落账单）。
type Result struct {
	Status   int
	Streamed bool
	// Usage / HasUsage 是旁路读到的归一化用量（全零视为未取到）。
	Usage    Usage
	HasUsage bool
	// UsageRaw 是上游 usage 子树（重新序列化；落账单留痕，绝不回流响应）。
	UsageRaw []byte
	// ObservedTextRunes 是旁路累计的生成内容字符数（无 usage 时估算输出用）。
	ObservedTextRunes int
	// Boundary 是归因事实（与 ResponseFacts 同源）。
	Boundary Attribution
	// TimedOut 表示首包或空闲超时；Canceled 表示客户端断开。
	TimedOut bool
	Canceled bool
	// NoContent 表示上游 204/304（无内容可计费）。
	NoContent bool
	// FirstByteMs 是到首字节的耗时（账单 first_token_ms）。
	FirstByteMs *int64
	// Duration 是整次转发耗时。
	Duration time.Duration
	// Err 是传输层错误（nil = 拿到了上游响应）。
	Err error
}

// Forwarder 执行转发（无状态，并发安全；客户端按超时配置缓存）。
type Forwarder struct {
	// Now 供用例注入时钟；nil 用 time.Now。
	Now func() time.Time
	// Logger 为 nil 时用 slog.Default。
	Logger *slog.Logger

	clients map[Timeouts]*http.Client
}

// NewForwarder 创建转发执行器。
func NewForwarder() *Forwarder { return &Forwarder{clients: map[Timeouts]*http.Client{}} }

// client 按超时配置取（并缓存）HTTP 客户端。
//
// 为什么按 Timeouts 做键：超时来自 sys_configs，改一次配置就该换一套客户端；
// 反过来，不改配置时不该每请求 new 一个 Transport（连接池会废掉）。
func (f *Forwarder) client(t Timeouts) *http.Client {
	if c, ok := f.clients[t]; ok {
		return c
	}
	dialer := &net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}
	c := &http.Client{Transport: &http.Transport{
		DialContext:           dialer.DialContext,
		ResponseHeaderTimeout: t.StreamFirstByte,
		// 不启用自动压缩：请求侧固定 Accept-Encoding: identity，响应侧也不能二次解压，
		// 否则下游收到的字节与上游发出的不一致（违反「字节原样」）。
		DisableCompression:    true,
		MaxIdleConns:          128,
		MaxIdleConnsPerHost:   64,
		IdleConnTimeout:       90 * time.Second,
		ExpectContinueTimeout: time.Second,
		ForceAttemptHTTP2:     false,
	}}
	f.clients[t] = c
	return c
}

// now 取当前时刻。
func (f *Forwarder) now() time.Time {
	if f != nil && f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

// log 取 logger。
func (f *Forwarder) log() *slog.Logger {
	if f != nil && f.Logger != nil {
		return f.Logger
	}
	return slog.Default()
}

// ErrUpstreamTimeout 表示等响应头超时（对下游是 504）。
var ErrUpstreamTimeout = errors.New("上游首包超时")

// Forward 执行一次转发。成功路径已把响应写给 w；失败时**不写**任何响应，
// 由主链决定状态码与错误体（本包不知道对外错误体形状，也不该知道）。
func (f *Forwarder) Forward(ctx context.Context, w http.ResponseWriter, req ForwardRequest, t Timeouts) Result {
	start := f.now()
	res := Result{}

	// 1) 组 URL：path 已由主链改写；query 原样（F6），唯一例外是 Gemini 的 `?key=`。
	u := strings.TrimRight(req.BaseURL, "/") + req.Path
	if q := req.Query; q != "" {
		if req.StripQueryKey {
			q = StripQueryParam(q, "key")
		}
		if q != "" {
			u += "?" + q
		}
	}
	if _, err := url.Parse(u); err != nil {
		res.Err = fmt.Errorf("上游地址非法：%w", err)
		f.done(ctx, req, &res, start)
		return res
	}

	// 2) 上游调用的 ctx：客户端断开要能取消上游（省额度），但计费在 OnDone 里用
	//    WithoutCancel 处理（调用已发生，不能因为断开就不落账）。
	upCtx := ctx
	var cancel context.CancelFunc
	if req.Streaming {
		// 流式不设总超时：长输出靠「首包 + 读空闲」两头夹，总超时会把正常长回答打断。
		upCtx, cancel = context.WithCancel(ctx)
	} else {
		upCtx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	httpReq, err := http.NewRequestWithContext(upCtx, http.MethodPost, u, bytes.NewReader(req.Body))
	if err != nil {
		res.Err = fmt.Errorf("构建上游请求失败：%w", err)
		f.done(ctx, req, &res, start)
		return res
	}
	CopyRequestHeaders(httpReq.Header, req.Header)
	if req.Credential != nil {
		req.Credential(httpReq.Header)
	}
	if len(req.Body) > 0 {
		httpReq.ContentLength = int64(len(req.Body))
	}

	resp, err := f.client(t).Do(httpReq)
	if err != nil {
		res.TimedOut = isTimeout(err)
		res.Canceled = ctx.Err() != nil
		res.Err = err
		f.done(ctx, req, &res, start)
		return res
	}
	defer resp.Body.Close()

	// 3) 归因：必须在剥头之前读（这些头本身要交给主链，然后被剥掉）。
	res.Status = resp.StatusCode
	res.Boundary = ReadAttribution(resp.Header)
	res.Streamed = IsEventStream(resp.Header.Get("Content-Type"))
	res.NoContent = resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified
	if fb := f.now().Sub(start).Milliseconds(); fb >= 0 {
		res.FirstByteMs = &fb
	}
	if req.OnUpstreamResponse != nil {
		req.OnUpstreamResponse(ctx, ResponseFacts{
			Status:   res.Status,
			Header:   resp.Header.Clone(),
			Boundary: res.Boundary,
			Streamed: res.Streamed,
		})
	}

	// 4) 写响应头与状态码（按 F8 剥头 + 写入我们自己的 x-request-id）。
	CopyResponseHeaders(w.Header(), resp.Header, req.RequestID)
	if res.NoContent {
		w.WriteHeader(resp.StatusCode)
		f.done(ctx, req, &res, start)
		return res
	}
	w.WriteHeader(resp.StatusCode)

	// 5) 读体：旁路 usage（只读不改），空闲超时用「读超时看门狗」实现。
	collector := newUsageCollector(req.Protocol, res.Streamed)
	reader := io.Reader(resp.Body)
	if t.StreamIdle > 0 {
		wd := newIdleWatchdog(t.StreamIdle, cancel)
		defer wd.Stop()
		reader = wd.Wrap(reader)
	}
	buf := make([]byte, 32<<10)
	for {
		n, rerr := reader.Read(buf)
		if n > 0 {
			if req.Billable {
				collector.Feed(buf[:n])
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				// 下游断开：已读到的 usage 仍要计费，所以只记标记不丢弃结果。
				res.Canceled = true
				break
			}
		}
		if rerr != nil {
			if !errors.Is(rerr, io.EOF) {
				res.Err = rerr
				if isTimeout(rerr) {
					res.TimedOut = true
				}
			}
			break
		}
	}

	// 6) usage 定稿。
	if req.Billable {
		collector.finish()
		if u, ok := collector.Result(); ok {
			res.Usage, res.HasUsage = u, true
		}
		res.UsageRaw = collector.RawUsage()
		res.ObservedTextRunes = collector.ObservedRunes()
	}
	f.done(ctx, req, &res, start)
	return res
}

// done 收尾并回调 OnDone（调用已发生，计费不能因下游断开而丢）。
func (f *Forwarder) done(ctx context.Context, req ForwardRequest, res *Result, start time.Time) {
	res.Duration = f.now().Sub(start)
	if req.OnDone != nil {
		// WithoutCancel：客户端断开时 ctx 已取消，结算/退款必须仍然落库。
		req.OnDone(context.WithoutCancel(ctx), *res)
	}
	if res.Err != nil {
		f.log().Warn("上游转发失败",
			"path", req.Path, "status", res.Status, "timed_out", res.TimedOut,
			"canceled", res.Canceled, "err", res.Err)
	}
}

// isTimeout 判断错误是否为超时（含 net 层的 timeout）。
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

// idleWatchdog 是「连续 N 时间无数据就取消上游」的看门狗。
//
// 为什么不用 http.Client.Timeout：那是**总时长**，长回答会被正常打断；
// 需要的语义是「卡住才断」，只能按每次 Read 重置计时器。
type idleWatchdog struct {
	d      time.Duration
	cancel context.CancelFunc
	timer  *time.Timer
	fired  bool
}

// newIdleWatchdog 启动看门狗（首次等待也算一次空闲）。
func newIdleWatchdog(d time.Duration, cancel context.CancelFunc) *idleWatchdog {
	return &idleWatchdog{d: d, cancel: cancel, timer: time.AfterFunc(d, cancel)}
}

// Wrap 包装 reader：每次读到数据就重置计时器。
func (w *idleWatchdog) Wrap(r io.Reader) io.Reader { return &idleReader{src: r, wd: w} }

// Stop 停掉计时器。
func (w *idleWatchdog) Stop() {
	if w.timer != nil {
		w.timer.Stop()
	}
}

// idleReader 在 Read 成功后重置看门狗。
type idleReader struct {
	src io.Reader
	wd  *idleWatchdog
}

// Read 实现 io.Reader。
func (r *idleReader) Read(p []byte) (int, error) {
	n, err := r.src.Read(p)
	if n > 0 && r.wd.timer != nil {
		r.wd.timer.Reset(r.wd.d)
	}
	return n, err
}

// StripQueryParam 从 raw query 里剔除指定键，其余参数逐字保留（不重新编码）。
//
// 不重新编码是刻意的：`?alt=sse&prettyPrint=false` 这类参数重新编码后可能变形状，
// 上游按 query 做缓存/签名校验时会立刻暴露。
func StripQueryParam(raw, key string) string {
	if raw == "" || key == "" {
		return raw
	}
	parts := strings.Split(raw, "&")
	out := parts[:0]
	for _, p := range parts {
		if p == "" {
			continue
		}
		name := p
		if i := strings.Index(p, "="); i >= 0 {
			name = p[:i]
		}
		if name == key {
			continue
		}
		out = append(out, p)
	}
	return strings.Join(out, "&")
}
