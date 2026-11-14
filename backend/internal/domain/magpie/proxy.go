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
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
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

	// 1) 组出站请求：path 已按协议改写，query 原样保留。
	u := p.cfg.BaseURL + prep.UpstreamPath
	if q := r.URL.RawQuery; q != "" {
		u += "?" + q
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

	// 2) 透传响应头与状态码（上游错误同样原样回流，不二次包装）。
	res.Status = resp.StatusCode
	res.NoContent = resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotModified
	copyResponseHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)

	// 3) 旁路读 usage（只读：不改任何字节）。
	isSSE := isEventStream(resp.Header.Get("Content-Type"))
	res.Streamed = isSSE
	collector := newUsageCollector(route.Protocol, isSSE)

	if route.Billable {
		if err := p.copyBody(w, resp.Body, collector, isSSE, cancel, r.Context(), &res); err != nil {
			res.Err = err
		}
		collector.finish()
		if u, ok := collector.Result(); ok {
			res.Usage, res.HasUsage = u, true
		}
	} else {
		// 计数端点不计费、不读 usage：直接透传，避免无谓的内存与解析开销。
		if err := p.copyBody(w, resp.Body, nil, isSSE, cancel, r.Context(), &res); err != nil {
			res.Err = err
		}
	}
	return res
}

// copyBody 把上游响应体写下游：流式逐块 flush，同时旁路喂 usage 采集器。
// 读空闲超时通过 cancel 触发（响应头已发出，只能中断连接）。
func (p *Proxy) copyBody(w http.ResponseWriter, body io.Reader, collector *usageCollector,
	isSSE bool, cancel context.CancelFunc, downstream context.Context, res *Result) error {
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
			if _, werr := w.Write(buf[:n]); werr != nil {
				// 下游断开（客户端取消/超时）：立即停止并标记，保留已读到的 usage。
				res.Canceled = true
				cancel()
				return nil
			}
			if collector != nil {
				collector.Feed(buf[:n])
			}
			if isSSE && flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
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
