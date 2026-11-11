package gateway

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// 本文件钉住「上游在 SSE 中途下发 error 帧」的口径（评审核出 + Gate/验收复验两轮）：
//
//  1. error 帧 + usage 齐备：内容层失败必须记 failed（此前整条记 completed，失败完全消失）；
//     计费口径**不变**——按上游实际 usage 结算，不估算、不新增扣费/退费、不落额外 failed 账单。
//  2. error 帧 + 无 usage：除记 failed 外，上游原文必须写进 call_logs.error_message
//     （此前只有「上游流式响应中断」，运营按 request_id 仍看不到上游报了什么）。
//  3. 两种情形都要把上游真实状态码写进 call_logs.upstream_status（此路恒为 200），
//     且下游收到的 SSE 里仍是上游 error 帧原文（网关不改写）。
//
// 四条流式实现（openai-chat 直通 / anthropic / responses / 转换泵）共用同一结论，
// 故此处覆盖「直通 openai-chat」与「anthropic 入站 → openai-chat 上游（转换泵）」两条代表性路径，
// 并对 error 帧与 usage 的**先后顺序**做表驱动（顺序不同不得改变结论）。

// regionBlockedMsg 是运营反馈的真实上游文案（区域未开通），必须原样出现在调用日志与下游响应里。
const regionBlockedMsg = "This model is hosted outside Europe and the United States. Select Global in the console."

// regionBlockedFrame 上游 error 帧原文（下游响应里必须仍是它）。
const regionBlockedFrame = `{"error":{"type":"invalid_request_error","message":"` + regionBlockedMsg + `"}}`

// streamSSE 拼 SSE 帧（每帧以空行结束）。
func streamSSE(frames ...string) string {
	var b strings.Builder
	for _, f := range frames {
		b.WriteString(f)
		b.WriteString("\n\n")
	}
	return b.String()
}

// sseWithErrorFrame 按「error 帧位置」拼出四种真实可能出现的顺序：
// usage 在 error 帧之前 / 之后、有无 [DONE] 收尾。
func sseWithErrorFrame(order string) string {
	content := `data: {"choices":[{"index":0,"delta":{"content":"partial answer"}}]}`
	usage := `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	errFrame := `data: ` + regionBlockedFrame
	switch order {
	case "usage_before_error":
		return streamSSE(content, usage, errFrame, `data: [DONE]`)
	case "error_before_usage":
		return streamSSE(content, errFrame, usage, `data: [DONE]`)
	case "usage_no_done":
		return streamSSE(content, usage, errFrame)
	default: // error_then_done：error 帧后只补一个 [DONE]（代理常见兜底），usage 已在前面给过
		return streamSSE(content, usage, errFrame, `data: [DONE]`)
	}
}

// TestServeStream_ErrorFrameWithUsageRecordedFailed error 帧 + usage（四种顺序）：
//   - 调用日志记 failed（此前记 completed），error_message 含上游原文，upstream_status=200；
//   - 计费按上游实际 usage（10/5），不估算、只有一条非 failed 账单；
//   - 下游 SSE 里仍是上游 error 帧原文。
func TestServeStream_ErrorFrameWithUsageRecordedFailed(t *testing.T) {
	for _, order := range []string{"usage_before_error", "error_before_usage", "usage_no_done"} {
		t.Run(order, func(t *testing.T) {
			sse := sseWithErrorFrame(order)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(sse))
			}))
			defer upstream.Close()

			h := build(t, upstream.URL)
			dump := withLogCapture(h)
			rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
				`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
			if rec.Code != http.StatusOK {
				t.Fatalf("want 200（流式响应已下发）, got %d: %s", rec.Code, rec.Body.String())
			}
			// 下游收到的 error 帧必须是上游原文（网关不改写、不替换成中文）。
			if !strings.Contains(rec.Body.String(), regionBlockedMsg) {
				t.Fatalf("下游 SSE 必须保留上游 error 帧原文, got %q", head(rec.Body.String(), 400))
			}

			if len(h.clogs.inserted) != 1 {
				t.Fatalf("want 1 条调用日志, got %d", len(h.clogs.inserted))
			}
			cl := h.clogs.inserted[0]
			if cl.Status != "failed" {
				t.Fatalf("error 帧 + usage 必须记 failed（此前误记 completed）, got status=%q", cl.Status)
			}
			if cl.RespKind != "stream" {
				t.Errorf("resp_kind want stream, got %q", cl.RespKind)
			}
			if !strings.Contains(cl.ErrorMessage, regionBlockedMsg) {
				t.Fatalf("error_message 必须带上游原文, got %q", cl.ErrorMessage)
			}
			if !strings.Contains(cl.ErrorMessage, "上游流式响应中断") {
				t.Errorf("error_message 应保留中断归因前缀, got %q", cl.ErrorMessage)
			}
			if cl.UpstreamStatus == nil || *cl.UpstreamStatus != http.StatusOK {
				t.Errorf("upstream_status 应写上游真实码 200, got %v", cl.UpstreamStatus)
			}

			// 计费口径不变：仍按上游实际 usage 结算（Input=10/Output=5），且只有一条成功账单。
			if len(h.billing.reqs) != 1 {
				t.Fatalf("want 1 条账单（不得为 error 帧额外落 failed 账单）, got %d: %+v", len(h.billing.reqs), h.billing.reqs)
			}
			if h.billing.reqs[0].Fail {
				t.Errorf("计费口径不变：error 帧 + usage 仍按实际 usage 结算，不得记 failed 账单, got %+v", h.billing.reqs[0])
			}
			if u := h.billing.reqs[0].Tokens; u.Input != 10 || u.Output != 5 {
				t.Errorf("应按上游实际 usage 计费（10/5），不是估算, got %+v", u)
			}

			// 渠道健康回喂保持改造前口径（本单只改记录，健康惩罚/选路口径不动）。
			if len(h.chans.feeds) != 1 || !h.chans.feeds[0].fb.IsSuccess {
				t.Errorf("本单不改健康口径，应保持既有成功回喂, got %+v", h.chans.feeds)
			}

			// 请求级结论：语义上失败（内容层已失败），不得记成 success。
			if line := logLineContaining(dump(), "流式中断"); line == "" {
				t.Fatalf("请求级结论应计为流式中断（语义失败）")
			}
		})
	}
}

// TestServeStream_ErrorFrameWithoutUsageKeepsUpstreamDetail error 帧 + 无 usage：
// 记 failed 的同时，error_message 必须带出上游原文，upstream_status 写上游真实码 200，
// 下游仍是上游 error 帧原文。
func TestServeStream_ErrorFrameWithoutUsageKeepsUpstreamDetail(t *testing.T) {
	sse := streamSSE(`data: `+regionBlockedFrame, `data: [DONE]`)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), regionBlockedMsg) {
		t.Fatalf("下游 SSE 必须保留上游 error 帧原文, got %q", head(rec.Body.String(), 400))
	}
	if len(h.clogs.inserted) != 1 {
		t.Fatalf("want 1 条调用日志, got %d", len(h.clogs.inserted))
	}
	cl := h.clogs.inserted[0]
	if cl.Status != "failed" {
		t.Fatalf("want status=failed, got %q", cl.Status)
	}
	if !strings.Contains(cl.ErrorMessage, regionBlockedMsg) {
		t.Fatalf("error_message 必须带上游 error 帧原文, got %q", cl.ErrorMessage)
	}
	if cl.UpstreamStatus == nil || *cl.UpstreamStatus != http.StatusOK {
		t.Errorf("upstream_status 应写上游真实码 200, got %v", cl.UpstreamStatus)
	}
	// usage 缺失仍按估算计费（口径不变）：不得是 failed 账单。
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Fail {
		t.Fatalf("usage 缺失应按估算计费（成功账单）, got %+v", h.billing.reqs)
	}
	if len(h.chans.feeds) != 1 || h.chans.feeds[0].fb.IsSuccess {
		t.Fatalf("usage 缺失 + error 帧应计入渠道健康失败（既有口径）, got %+v", h.chans.feeds)
	}
}

// TestStreamConversionErrorFrameWithUsageRecordedFailed 转换泵（anthropic 入站 → openai-chat 上游）
// 同样适用：error 帧 + usage → 调用日志 failed、带上游原文与 upstream_status=200，计费按实际 usage。
func TestStreamConversionErrorFrameWithUsageRecordedFailed(t *testing.T) {
	sse := sseWithErrorFrame("error_before_usage")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sse))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	h.chans.mrt.Model.Protocols = channelprotocol.Protocols{channelprotocol.ProtocolOpenAIChat: {}}
	rec := postWithHeaders(t, h.gw, "/v1/messages",
		`{"model":"gpt-4","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"ping"}]}`,
		map[string]string{"x-api-key": "sk-gw-t"})
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200（流式响应已下发）, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.clogs.inserted) != 1 {
		t.Fatalf("want 1 条调用日志, got %d", len(h.clogs.inserted))
	}
	cl := h.clogs.inserted[0]
	if cl.Status != "failed" || cl.RespKind != "stream" {
		t.Fatalf("转换泵 error 帧 + usage 也应记 failed/stream, got status=%q kind=%q", cl.Status, cl.RespKind)
	}
	if !strings.Contains(cl.ErrorMessage, regionBlockedMsg) {
		t.Fatalf("error_message 必须带上游原文, got %q", cl.ErrorMessage)
	}
	if cl.UpstreamStatus == nil || *cl.UpstreamStatus != http.StatusOK {
		t.Errorf("upstream_status 应写上游真实码 200, got %v", cl.UpstreamStatus)
	}
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].Fail {
		t.Fatalf("计费口径不变：按上游实际 usage 结算, got %+v", h.billing.reqs)
	}
	if u := h.billing.reqs[0].Tokens; u.Input != 10 || u.Output != 5 {
		t.Errorf("应按上游实际 usage 计费（10/5）, got %+v", u)
	}
}

// TestUpstreamErrorFrameError_Semantics 错误类型语义：可 errors.Is 识别，且**不**冒充 usage 缺失
// （否则会走估算计费分支，改动计费口径）。
func TestUpstreamErrorFrameError_Semantics(t *testing.T) {
	err := error(&UpstreamErrorFrameError{Detail: "server_error: boom"})
	if !isUpstreamErrorFrame(err) {
		t.Fatal("UpstreamErrorFrameError 必须可被 errors.As 识别")
	}
	if errors.Is(err, ErrUsageMissing) {
		t.Fatal("error 帧 + usage 不得被当成 usage 缺失（会改计费口径）")
	}
	if got := upstreamErrorFrameDetail(err); got != "server_error: boom" {
		t.Fatalf("detail want server_error: boom, got %q", got)
	}
	// 空负载（{"error":{}}）时摘要为空，但仍是失败。
	if got := upstreamErrorFrameDetail(&UpstreamErrorFrameError{}); got != "" {
		t.Fatalf("空摘要应返回空串, got %q", got)
	}
	if (&UpstreamErrorFrameError{}).Error() != "upstream stream error frame" {
		t.Fatalf("空摘要时 Error() 文案不符: %q", (&UpstreamErrorFrameError{}).Error())
	}
}

// isUpstreamErrorFrame 判定错误是否为「上游流内 error 帧」类型（与 handler 同一判据语义）。
func isUpstreamErrorFrame(err error) bool {
	var fe *UpstreamErrorFrameError
	return errors.As(err, &fe)
}
