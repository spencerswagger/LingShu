package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// 本文件钉住「上游错误可见性」三件事（契约见 docs/designs/2026-11-11-upstream-error-visibility-design.md）：
//  1. 对外透传：上游非 2xx 的 message/type 原文出现在调用方拿到的错误体里（不再统一替换成中文文案）；
//  2. 落库：同一次失败在账单 + 调用日志里都能按 request_id 查到，且带结构化上游状态码；
//  3. 流式覆盖：stream=true 与非流式行为一致（同一分支代码，用例两侧各跑一遍）。
//
// 为什么这组用例必须走 HTTP 层而不是只调 serveNonStream/serveStream：
// request_id 来自中间件（context + 响应头），上游报文透传发生在写出响应的最后一步，
// 只调内部函数会绕过「响应头 x-request-id → 错误体 request_id」这条链路。

// openCodeGoRegionErrBody 是运营反馈的真实上游报文（OpenCode Go 区域未开通）。
// 运营看到这句就能自己去控制台把区域切 Global，因此它必须原样到达调用方。
const openCodeGoRegionErrBody = `{"error":{"type":"invalid_request_error","message":"This OpenCode Go model is hosted outside Europe and the United States. Select Global at https://opencode.ai/console/xxx/settings/privacy#regions to use it."}}`

// openCodeGoRegionErrMsg 上游 message 原文（断言用）。
const openCodeGoRegionErrMsg = "This OpenCode Go model is hosted outside Europe and the United States."

// doRequestWithID 以「中间件已生效」的姿态发一次请求：request_id 同时进 context 与响应头
// （对齐 server.WithRequestID 的真实行为），从而能断言错误体 request_id 与调用日志 request_id 同源。
func doRequestWithID(t *testing.T, gw *Gateway, path, token, body, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	req = resp.WithRequestID(req, requestID)
	rec := httptest.NewRecorder()
	rec.Header().Set("x-request-id", requestID)
	gw.Handler().ServeHTTP(rec, req)
	return rec
}

// streamFlagged 返回非流式/流式两份请求体（同一用例两侧各跑一遍）。
func streamFlagged(stream bool) string {
	if stream {
		return `{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	}
	return `{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`
}

// TestServeUpstreamError_PassThroughMessageAndType 验收①：上游 400 的 message/type 原文透出到调用方，
// 网关自己的 code/request_id 照常给出，并带上游真实状态码；同时账单与调用日志都留痕（验收②）。
func TestServeUpstreamError_PassThroughMessageAndType(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "非流式"
		if stream {
			name = "流式"
		}
		t.Run(name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(openCodeGoRegionErrBody))
			}))
			defer up.Close()

			h := build(t, up.URL)
			const rid = "req-opencode-region"
			rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(stream), rid)

			// 下游状态码沿用既有语义（未命中错误策略时上游失败统一 502），本次不改状态码契约。
			if rec.Code != http.StatusBadGateway {
				t.Fatalf("want 502, got %d: %s", rec.Code, rec.Body.String())
			}
			rb := decodeOpenAIError(t, rec)
			if !strings.Contains(rb.Error.Message, openCodeGoRegionErrMsg) {
				t.Fatalf("错误体必须带上游 message 原文，got %q", rb.Error.Message)
			}
			if rb.Error.Type != "invalid_request_error" {
				t.Errorf("type 应保留上游原文 invalid_request_error，got %q", rb.Error.Type)
			}
			if rb.Error.Code != "upstream_error" {
				t.Errorf("code 应为网关归类（upstream_error），got %q", rb.Error.Code)
			}
			if rb.Error.UpstreamStatus != http.StatusBadRequest {
				t.Errorf("upstream_status want 400，got %d", rb.Error.UpstreamStatus)
			}
			if rb.RequestID != rid {
				t.Errorf("request_id want %q，got %q", rid, rb.RequestID)
			}

			// 验收②：同一 request_id 在账单与调用日志里都能查到这次失败。
			if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail {
				t.Fatalf("want 1 条 failed 账单, got %+v", h.billing.reqs)
			}
			if !strings.Contains(h.billing.reqs[0].ErrorMessage, openCodeGoRegionErrMsg) {
				t.Errorf("账单 error_message 应含上游原文, got %q", h.billing.reqs[0].ErrorMessage)
			}
			if len(h.clogs.inserted) != 1 {
				t.Fatalf("want 1 条调用日志, got %d", len(h.clogs.inserted))
			}
			cl := h.clogs.inserted[0]
			if cl.RequestID != rid {
				t.Errorf("调用日志 request_id want %q，got %q（账单页按 request_id 检索依赖它）", rid, cl.RequestID)
			}
			if cl.UpstreamStatus == nil || *cl.UpstreamStatus != http.StatusBadRequest {
				t.Errorf("调用日志 upstream_status want 400，got %v", cl.UpstreamStatus)
			}
			if cl.Status != "failed" || cl.RespKind != "error" {
				t.Errorf("调用日志 want status=failed/resp_kind=error, got %s/%s", cl.Status, cl.RespKind)
			}
			if !strings.Contains(cl.RespBody, openCodeGoRegionErrMsg) || !strings.Contains(cl.ErrorMessage, openCodeGoRegionErrMsg) {
				t.Errorf("调用日志应保留上游错误体原文: resp=%q err=%q", cl.RespBody, cl.ErrorMessage)
			}
			if cl.ChannelID == nil || *cl.ChannelID == 0 || cl.Model != "gpt-4" {
				t.Errorf("调用日志应记渠道与模型, got channel=%v model=%q", cl.ChannelID, cl.Model)
			}
		})
	}
}

// TestServeUpstreamError_TruncatesAndKeepsValidUTF8 上游塞超长/中文文案时：对外 message 有上限、
// 按 UTF-8 边界截断（不出半个字符），且仍保留可读前缀。
func TestServeUpstreamError_TruncatesAndKeepsValidUTF8(t *testing.T) {
	longMsg := strings.Repeat("上游限流说明：区域未开通，请到控制台切换 Global。", 200)
	body, err := json.Marshal(map[string]any{"error": map[string]any{
		"type": "invalid_request_error", "message": longMsg,
	}})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(body)
	}))
	defer up.Close()

	h := build(t, up.URL)
	rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), "req-long")
	rb := decodeOpenAIError(t, rec)
	if len(rb.Error.Message) > errSummaryMaxBytes {
		t.Fatalf("对外 message 应截断到 %d 字节内，got %d", errSummaryMaxBytes, len(rb.Error.Message))
	}
	if !utf8.ValidString(rb.Error.Message) {
		t.Fatal("截断后的 message 必须是合法 UTF-8（不能切出半个字符）")
	}
	if !strings.Contains(rb.Error.Message, "上游限流说明") {
		t.Fatalf("截断应保留可读前缀，got %q", rb.Error.Message)
	}
}

// TestServeUpstreamError_FallsBackWhenUpstreamBodyUnusable 上游没给可读文案（空体）时：
// message 回落到网关文案（不留空 message），upstream_status 仍照实给出。
func TestServeUpstreamError_FallsBackWhenUpstreamBodyUnusable(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest) // 无响应体
	}))
	defer up.Close()

	h := build(t, up.URL)
	rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(true), "req-empty-body")
	rb := decodeOpenAIError(t, rec)
	if rb.Error.Message != msgUpstreamDown {
		t.Fatalf("空上游体应回落到网关文案 %q，got %q", msgUpstreamDown, rb.Error.Message)
	}
	if rb.Error.UpstreamStatus != http.StatusBadRequest {
		t.Errorf("upstream_status 仍应为 400，got %d", rb.Error.UpstreamStatus)
	}
	if len(h.clogs.inserted) != 1 || h.clogs.inserted[0].UpstreamStatus == nil {
		t.Fatalf("调用日志仍须记上游状态码, got %+v", h.clogs.inserted)
	}
}

// TestServeUpstreamError_AggregateKeepsLastUpstreamBody 5xx 属可转移失败：单候选耗尽后终结 503，
// 但上游报文不能丢——终结响应仍要带上游原文与真实状态码（验收：5xx 情形错误体不丢）。
func TestServeUpstreamError_AggregateKeepsLastUpstreamBody(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "非流式"
		if stream {
			name = "流式"
		}
		t.Run(name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(`{"error":{"type":"server_error","message":"upstream overloaded: retry after 30s"}}`))
			}))
			defer up.Close()

			h := build(t, up.URL)
			rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(stream), "req-5xx")
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("全候选失败 want 503, got %d: %s", rec.Code, rec.Body.String())
			}
			rb := decodeOpenAIError(t, rec)
			if !strings.Contains(rb.Error.Message, "upstream overloaded") {
				t.Fatalf("终结响应应带最近一次上游原文，got %q", rb.Error.Message)
			}
			if rb.Error.UpstreamStatus != http.StatusInternalServerError {
				t.Errorf("upstream_status want 500，got %d", rb.Error.UpstreamStatus)
			}
			if rb.RequestID != "req-5xx" {
				t.Errorf("request_id want req-5xx，got %q", rb.RequestID)
			}
			if len(h.clogs.inserted) != 1 {
				t.Fatalf("want 1 条调用日志, got %d", len(h.clogs.inserted))
			}
			if st := h.clogs.inserted[0].UpstreamStatus; st == nil || *st != http.StatusInternalServerError {
				t.Errorf("调用日志 upstream_status want 500，got %v", st)
			}
		})
	}
}

// TestServeUpstreamError_ConnectionFailureKeepsGenericMessage 断网（连接失败）：
// 没有上游报文可透传，对外仍是既有网关文案（不得凭空造上游原文），
// 但账单 + 调用日志必须留痕，且调用日志不写 upstream_status（无上游状态码）。
func TestServeUpstreamError_ConnectionFailureKeepsGenericMessage(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // 关闭监听 → 连接被拒

	h := build(t, deadURL)
	rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), "req-dead")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503, got %d: %s", rec.Code, rec.Body.String())
	}
	rb := decodeOpenAIError(t, rec)
	if rb.Error.Message != msgNoAvailable {
		t.Fatalf("连接失败应保持网关文案 %q，got %q", msgNoAvailable, rb.Error.Message)
	}
	if rb.Error.UpstreamStatus != 0 {
		t.Errorf("连接失败无上游状态码，upstream_status 应为空，got %d", rb.Error.UpstreamStatus)
	}
	if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail {
		t.Fatalf("连接失败须落 failed 账单, got %+v", h.billing.reqs)
	}
	if len(h.clogs.inserted) != 1 {
		t.Fatalf("连接失败须落调用日志, got %d", len(h.clogs.inserted))
	}
	cl := h.clogs.inserted[0]
	if cl.RequestID != "req-dead" {
		t.Errorf("调用日志 request_id want req-dead，got %q", cl.RequestID)
	}
	if cl.UpstreamStatus != nil {
		t.Errorf("连接失败不应有 upstream_status，got %v", *cl.UpstreamStatus)
	}
	if !strings.Contains(cl.ErrorMessage, "连接失败") {
		t.Errorf("调用日志应写清连接失败原因，got %q", cl.ErrorMessage)
	}
}

// TestServeUpstreamError_SuccessStaysSuccess 回归守卫：上游 2xx 时不得被改成错误
// （透传逻辑只在非 2xx 分支生效），且调用日志不写 upstream_status。
func TestServeUpstreamError_SuccessStaysSuccess(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(testOKBody))
	}))
	defer up.Close()

	h := build(t, up.URL)
	rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), "req-ok")
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("成功响应不得带错误体, got %s", rec.Body.String())
	}
	if len(h.clogs.inserted) != 1 || h.clogs.inserted[0].Status != "completed" {
		t.Fatalf("want 1 条 completed 调用日志, got %+v", h.clogs.inserted)
	}
	if h.clogs.inserted[0].UpstreamStatus != nil {
		t.Errorf("成功记录不应写 upstream_status, got %v", *h.clogs.inserted[0].UpstreamStatus)
	}
}

// TestServeRejectPaths_RecordBillingAndCallLog 网关自身拒绝（未进入上游转发）同样必须「账单 + 调用日志」成对：
// 账单页按 request_id 检索是经 call_logs 关联的（billing/query.go），只落账单不落调用日志时，
// 调用方拿着 response 里的 request_id 在账单页搜不到这次失败——这正是「失败查不到」的成因之一。
func TestServeRejectPaths_RecordBillingAndCallLog(t *testing.T) {
	cases := []struct {
		name      string
		body      string
		wantCode  int
		wantLogID string
		setup     func(h *harness)
	}{
		{
			name:     "请求体格式错误",
			body:     `{not-json`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "请求体缺少 model",
			body:     `{"stream":false}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "余额不足(预扣拒绝)",
			body:     `{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`,
			wantCode: http.StatusPaymentRequired,
			setup: func(h *harness) {
				fc := &fakeCredit{balance: 0}
				h.gw.credit = fc
			},
		},
		{
			name:     "模型未配置",
			body:     `{"model":"ghost","stream":false}`,
			wantCode: http.StatusNotFound,
			setup: func(h *harness) {
				h.rrouter.result = nil
				h.rrouter.err = &router.RouteError{Kind: router.KindModelNotFound,
					HTTPStatus: http.StatusNotFound, Code: resp.CodeNotFound, Message: "model not found"}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := build(t, "http://unused")
			if tc.setup != nil {
				tc.setup(h)
			}
			const rid = "req-reject"
			rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", tc.body, rid)
			if rec.Code != tc.wantCode {
				t.Fatalf("want %d, got %d: %s", tc.wantCode, rec.Code, rec.Body.String())
			}
			if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail {
				t.Fatalf("拒绝路径应落 1 条 failed 账单, got %+v", h.billing.reqs)
			}
			if len(h.clogs.inserted) != 1 {
				t.Fatalf("拒绝路径应落 1 条调用日志（否则账单页按 request_id 搜不到）, got %d", len(h.clogs.inserted))
			}
			cl := h.clogs.inserted[0]
			if cl.RequestID != rid {
				t.Errorf("调用日志 request_id want %q，got %q", rid, cl.RequestID)
			}
			if cl.Status != "failed" || cl.ErrorMessage == "" {
				t.Errorf("调用日志 want status=failed 且带原因, got status=%q err=%q", cl.Status, cl.ErrorMessage)
			}
			if cl.BillingID != h.billing.bids[0] {
				t.Errorf("调用日志应与账单 1:1 关联, got billing_id=%q want %q", cl.BillingID, h.billing.bids[0])
			}
			if rb := decodeOpenAIError(t, rec); rb.RequestID != rid {
				t.Errorf("错误体 request_id want %q，got %q", rid, rb.RequestID)
			}
		})
	}
}

// TestServeUpstreamError_TimeoutKeepsGenericMessage 超时（请求已送达、上游无响应状态码）：
// 没有上游报文可透传 → 保持既有 502 网关文案；但调用日志必须留痕（可按 request_id 定位），
// 且不写 upstream_status（本次没有上游状态码，不能编造）。
func TestServeUpstreamError_TimeoutKeepsGenericMessage(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer live.Close()

	h := build(t, live.URL)
	h.gw.client = &http.Client{Transport: &nonStreamDeliveredFailTransport{}}

	rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), "req-timeout")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("已送达超时 want 502, got %d: %s", rec.Code, rec.Body.String())
	}
	rb := decodeOpenAIError(t, rec)
	if rb.Error.Message != msgUpstreamDown {
		t.Fatalf("超时应保持网关文案 %q，got %q", msgUpstreamDown, rb.Error.Message)
	}
	if rb.Error.UpstreamStatus != 0 {
		t.Errorf("超时无上游状态码，upstream_status 应为空，got %d", rb.Error.UpstreamStatus)
	}
	if len(h.clogs.inserted) != 1 {
		t.Fatalf("超时须落调用日志, got %d", len(h.clogs.inserted))
	}
	cl := h.clogs.inserted[0]
	if cl.RequestID != "req-timeout" || cl.Status != "failed" {
		t.Errorf("调用日志 want request_id=req-timeout/status=failed, got %q/%q", cl.RequestID, cl.Status)
	}
	if cl.UpstreamStatus != nil {
		t.Errorf("超时不应写 upstream_status, got %v", *cl.UpstreamStatus)
	}
}

// TestCallLogOptWithUpstreamStatus 可选字段填充器不改变默认行为（未传 opt 时 UpstreamStatus 为 nil）。
func TestCallLogOptWithUpstreamStatus(t *testing.T) {
	cl := &CallLog{}
	for _, opt := range []callLogOpt{nil, withUpstreamStatus(http.StatusTooManyRequests)} {
		if opt != nil {
			opt(cl)
		}
	}
	if cl.UpstreamStatus == nil || *cl.UpstreamStatus != http.StatusTooManyRequests {
		t.Fatalf("withUpstreamStatus 未生效: %+v", cl.UpstreamStatus)
	}
	var other CallLog
	if other.UpstreamStatus != nil {
		t.Fatalf("未传 opt 时应为空, got %v", *other.UpstreamStatus)
	}
}
