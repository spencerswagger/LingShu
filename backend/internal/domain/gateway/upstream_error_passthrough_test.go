package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// 本文件钉住「上游错误对外透传」契约（设计见
// docs/designs/2026-11-11-upstream-error-visibility-design.md），覆盖**形状**而不是文案：
//
//  1. 对外 HTTP 状态码沿用上游（4xx → 4xx、5xx → 5xx）；
//  2. 上游错误体内容完整可见：OpenAI 形状整段原样（每键每值逐路径保留）、
//     其它 JSON 的原文文本、非 JSON 的原文文本、超长的前缀 + 同字段「已截断」标注；
//  3. 失败同样落账单 + 调用日志，按 request_id 可查，带上游状态码/错误体原文/渠道/模型/耗时；
//  4. 流式与非流式各跑一遍（同一分支代码，两侧断言一致）。
//
// 为什么必须走 HTTP 层：request_id 来自中间件（context + 响应头），透传发生在写出响应的最后一步，
// 只调内部函数会绕过「响应头 x-request-id → 错误体 request_id」这条链路。

// openCodeGoRegionErrBody 运营反馈过的真实上游报文（OpenCode Go 区域未开通），
// 只当样本用：用例断言的是「形状」，不是这段文案。
const openCodeGoRegionErrBody = `{"error":{"type":"invalid_request_error","message":"This OpenCode Go model is hosted outside Europe and the United States. Select Global at https://opencode.ai/console/xxx/settings/privacy#regions to use it."}}`

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

// upstreamErrCase 一种上游错误**形状**（不绑定文案）。
type upstreamErrCase struct {
	name        string
	status      int // 上游状态码（= 期望的对外状态码）
	contentType string
	body        string
	// mode 决定「内容完整可见」的判据：
	//   verbatim  —— JSON 且 error 为对象：对外 error 子树必须与上游逐路径相等；
	//   rawtext   —— 其它 JSON / 非 JSON：原文文本必须完整出现在 error.message；
	//   truncated —— 超长：message ≤ 上限、含可辨认前缀、同字段带「已截断」标注；
	//   empty     —— 空体：message 非空可读。
	mode string
}

func TestServeUpstreamError_ShapeTable(t *testing.T) {
	longText := strings.Repeat("a", 12000)
	cases := []upstreamErrCase{
		{
			name: "①OpenAI标准error形状", status: http.StatusBadRequest, contentType: "application/json",
			body: openCodeGoRegionErrBody, mode: "verbatim",
		},
		{
			name: "②带嵌套details", status: http.StatusUnprocessableEntity, contentType: "application/json",
			body: `{"error":{"message":"validation failed","type":"invalid_request_error","code":"invalid_value",` +
				`"details":[{"field":"temperature","issue":"out_of_range"},` +
				`{"field":"max_tokens","issue":"too_large","nested":{"limit":4096,"got":8192}}]}}`,
			mode: "verbatim",
		},
		{
			name: "③只有message无type", status: http.StatusBadRequest, contentType: "application/json",
			body: `{"error":{"message":"model is not supported in this region"}}`, mode: "verbatim",
		},
		{
			name: "④非JSON纯文本", status: http.StatusBadRequest, contentType: "text/plain",
			body: "upstream rejected: quota exhausted for project p-123 (code 4021)", mode: "rawtext",
		},
		{
			name: "⑤HTML错误页", status: http.StatusBadRequest, contentType: "text/html",
			body: `<html><body><h1>502 Bad Gateway</h1><p id="req">ray-id: 8f2c</p></body></html>`, mode: "rawtext",
		},
		{
			name: "⑥空body", status: http.StatusBadRequest, contentType: "application/json",
			body: "", mode: "empty",
		},
		{
			name: "⑦超长>8KB", status: http.StatusBadRequest, contentType: "application/json",
			body: `{"error":{"message":"` + longText + `"}}`, mode: "truncated",
		},
		{
			name: "⑧5xx", status: http.StatusServiceUnavailable, contentType: "application/json",
			body: `{"error":{"type":"server_error","message":"upstream overloaded: retry after 30s"}}`, mode: "verbatim",
		},
		{
			name: "⑨鉴权错401", status: http.StatusUnauthorized, contentType: "application/json",
			body: `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error","code":"invalid_api_key"}}`,
			mode: "verbatim",
		},
		{
			name: "⑩其它JSON(顶层数组)", status: http.StatusConflict, contentType: "application/json",
			body: `[{"code":"conflict","detail":"duplicate request"}]`, mode: "rawtext",
		},
	}

	for _, tc := range cases {
		tc := tc
		for _, stream := range []bool{false, true} {
			stream := stream
			name := tc.name + "/非流式"
			if stream {
				name = tc.name + "/流式"
			}
			t.Run(name, func(t *testing.T) {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if tc.contentType != "" {
						w.Header().Set("Content-Type", tc.contentType)
					}
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}))
				defer up.Close()

				h := build(t, up.URL)
				const rid = "req-shape"
				rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(stream), rid)

				// ① 状态码沿用上游（不再统一改写为 502/503）。
				if rec.Code != tc.status {
					t.Fatalf("对外状态码必须沿用上游 %d, got %d: %s", tc.status, rec.Code, rec.Body.String())
				}
				var out map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
					t.Fatalf("对外响应必须是 JSON: %v (%s)", err, rec.Body.String())
				}
				if out["request_id"] != rid {
					t.Errorf("顶层 request_id want %q，got %v", rid, out["request_id"])
				}
				if got, ok := out["upstream_status"].(float64); !ok || int(got) != tc.status {
					t.Errorf("顶层 upstream_status want %d，got %v", tc.status, out["upstream_status"])
				}
				msg := errorMessage(t, out)

				// ② 内容完整可见（按形状断言）。
				switch tc.mode {
				case "verbatim":
					assertErrorSubtreeVerbatim(t, tc.body, out)
				case "rawtext":
					if strings.TrimSpace(msg) != strings.TrimSpace(tc.body) {
						t.Fatalf("原文必须完整进 message，want %q，got %q", tc.body, msg)
					}
				case "truncated":
					if len(msg) > upstreamErrOutBytes {
						t.Fatalf("message 应截断到 %d 字节内，got %d", upstreamErrOutBytes, len(msg))
					}
					if !utf8.ValidString(msg) {
						t.Fatal("截断后的 message 必须是合法 UTF-8（不能切出半个字符）")
					}
					if !strings.Contains(msg, `"message":"`+longText[:4096]) {
						t.Fatal("截断必须保留可辨认的原文前缀（≥4KB）")
					}
					if !strings.Contains(msg, "已截断") {
						t.Fatalf("截断必须在**同一字段**里显式标注，got %q", tail(msg, 120))
					}
					if len(rec.Body.Bytes()) > 64<<10 {
						t.Fatalf("对外体量应 ≤64KB，got %d", len(rec.Body.Bytes()))
					}
				case "empty":
					if strings.TrimSpace(msg) == "" {
						t.Fatal("空上游体也必须给出可读 message，不能是空串")
					}
				}

				// ③ 失败必须同样落账单 + 调用日志，且按 request_id 可查。
				assertFailureTraced(t, h, rid, tc.status, contentOf(tc.body))
			})
		}
	}
}

// assertErrorSubtreeVerbatim 断言「OpenAI 形状整段原样」：对外 error 子树与上游逐路径相等
// （多字段、嵌套、未知字段一律不得丢改），且上游响应的其它顶层字段也照旧保留。
func assertErrorSubtreeVerbatim(t *testing.T, upBody string, out map[string]any) {
	t.Helper()
	var upDoc map[string]any
	if err := json.Unmarshal([]byte(upBody), &upDoc); err != nil {
		t.Fatalf("样本必须是 JSON: %v", err)
	}
	wantErr, ok := upDoc["error"]
	if !ok {
		t.Fatalf("样本必须是 OpenAI 形状: %s", upBody)
	}
	if !reflect.DeepEqual(out["error"], wantErr) {
		t.Fatalf("error 子树必须原样透传（不许删改任何字段）\nwant %#v\ngot  %#v", wantErr, out["error"])
	}
	for k, v := range upDoc {
		if k == "error" {
			continue
		}
		if !reflect.DeepEqual(out[k], v) {
			t.Errorf("上游顶层字段 %q 不得被丢弃/改写: want %#v got %#v", k, v, out[k])
		}
	}
}

// errorMessage 取对外错误体的 error.message（两种形状共用同一路径）。
func errorMessage(t *testing.T, out map[string]any) string {
	t.Helper()
	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("对外响应必须带 error 对象: %#v", out["error"])
	}
	m, _ := e["message"].(string)
	return m
}

// contentOf 取样本里「应出现在落库错误体」的稳定片段（超长样本只取前缀，落库有独立上限）。
func contentOf(body string) string {
	s := strings.TrimSpace(body)
	if len(s) > 256 {
		return s[:256]
	}
	return s
}

// assertFailureTraced 账单 + 调用日志成对留痕：按 request_id 可查，带上游状态码/错误体/渠道/模型/耗时。
func assertFailureTraced(t *testing.T, h *harness, rid string, upStatus int, bodyFragment string) {
	t.Helper()
	if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail {
		t.Fatalf("失败必须落 1 条 failed 账单, got %+v", h.billing.reqs)
	}
	if len(h.clogs.inserted) != 1 {
		t.Fatalf("失败必须落 1 条调用日志（账单页按 request_id 检索经 call_logs 关联）, got %d", len(h.clogs.inserted))
	}
	cl := h.clogs.inserted[0]
	if cl.RequestID != rid {
		t.Errorf("调用日志 request_id want %q，got %q", rid, cl.RequestID)
	}
	if cl.Status != "failed" || cl.RespKind != "error" {
		t.Errorf("调用日志 want status=failed/resp_kind=error, got %s/%s", cl.Status, cl.RespKind)
	}
	if cl.UpstreamStatus == nil || *cl.UpstreamStatus != upStatus {
		t.Errorf("调用日志 upstream_status want %d，got %v", upStatus, cl.UpstreamStatus)
	}
	if cl.ChannelID == nil || *cl.ChannelID == 0 {
		t.Errorf("调用日志必须记渠道, got %v", cl.ChannelID)
	}
	if cl.Model != "gpt-4" {
		t.Errorf("调用日志 model want gpt-4，got %q", cl.Model)
	}
	if cl.DurationMs == nil {
		t.Error("调用日志必须记耗时 duration_ms")
	}
	// created_at 由 SQLCallLogStore.Insert 在落库时补齐（内存 fake 拿不到），故此处只校验耗时字段。
	if bodyFragment != "" {
		if !strings.Contains(cl.RespBody, bodyFragment) {
			t.Errorf("调用日志 resp_body 必须保留上游错误体原文片段 %q，got %q", bodyFragment, cl.RespBody)
		}
		if !strings.Contains(cl.ErrorMessage, bodyFragment) {
			t.Errorf("账单 error_message 必须保留上游错误体原文片段 %q，got %q", bodyFragment, cl.ErrorMessage)
		}
	}
}

// TestServeUpstreamError_TerminalKeepsLastUpstream 可转移状态（401/403/404/429/5xx）在上游只有一个候选时
// 会走到「候选全部失败」的终结分支：此时对外必须是**上游状态码 + 上游报文**，
// 不能退化成 503「无可用渠道」聚合文案。
func TestServeUpstreamError_TerminalKeepsLastUpstream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "非流式"
		if stream {
			name = "流式"
		}
		t.Run(name, func(t *testing.T) {
			body := `{"error":{"type":"server_error","message":"upstream overloaded: retry after 30s","code":"overloaded"}}`
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(body))
			}))
			defer up.Close()

			h := build(t, up.URL)
			const rid = "req-terminal"
			rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(stream), rid)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("终结响应必须沿用上游 500, got %d: %s", rec.Code, rec.Body.String())
			}
			var out map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			assertErrorSubtreeVerbatim(t, body, out)
			if out["request_id"] != rid {
				t.Errorf("request_id want %q，got %v", rid, out["request_id"])
			}
			assertFailureTraced(t, h, rid, http.StatusInternalServerError, "upstream overloaded")
		})
	}
}

// TestServeUpstreamError_429TerminalKeepsUpstreamBody 上游 429 全候选限流时，
// 终结响应仍是 429，但必须带上游报文（是配额还是速率、何时重试，调用方需要知道）。
func TestServeUpstreamError_429TerminalKeepsUpstreamBody(t *testing.T) {
	body := `{"error":{"type":"rate_limit_error","message":"Rate limit reached for gpt-4: 20 req/min","code":"rate_limit_exceeded"}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(body))
	}))
	defer up.Close()

	h := build(t, up.URL)
	rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), "req-429")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertErrorSubtreeVerbatim(t, body, out)
}

// TestServeUpstreamError_ConnectionFailure 断网（连接被拒）：没有上游报文可透传，
// 对外 502 且**点名连接失败原因**（不再只说「服务暂时不可用」），调用日志不写 upstream_status。
func TestServeUpstreamError_ConnectionFailure(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // 关闭监听 → 连接被拒

	h := build(t, deadURL)
	rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), "req-dead")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", rec.Code, rec.Body.String())
	}
	rb := decodeOpenAIError(t, rec)
	if !strings.Contains(rb.Error.Message, "连接失败") {
		t.Fatalf("必须点名连接失败原因, got %q", rb.Error.Message)
	}
	if !strings.Contains(rb.Error.Message, "connect") && !strings.Contains(rb.Error.Message, "refused") {
		t.Errorf("应带上底层错误线索, got %q", rb.Error.Message)
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
		t.Errorf("连接失败没有上游状态码，不应编造, got %v", *cl.UpstreamStatus)
	}
	if !strings.Contains(cl.ErrorMessage, "连接失败") {
		t.Errorf("调用日志应写清连接失败原因，got %q", cl.ErrorMessage)
	}
}

// TestServeUpstreamError_Timeout 已送达后上游无响应（超时/断连）：对外 504 并点名超时，
// 调用日志留痕且不写 upstream_status（本次没有上游状态码）。
func TestServeUpstreamError_Timeout(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer live.Close()

	h := build(t, live.URL)
	h.gw.client = &http.Client{Transport: &timeoutFailTransport{}}

	rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), "req-timeout")
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("超时应回 504, got %d: %s", rec.Code, rec.Body.String())
	}
	rb := decodeOpenAIError(t, rec)
	if !strings.Contains(rb.Error.Message, "超时") {
		t.Fatalf("必须点名超时原因, got %q", rb.Error.Message)
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

// TestServeUpstreamError_SuccessStaysSuccess 回归守卫：上游 2xx 一律不得被改写成错误，
// 响应体逐字节不变（[DONE] 保留），且调用日志不写 upstream_status。
func TestServeUpstreamError_SuccessStaysSuccess(t *testing.T) {
	vendorJSON := `{"id":"x","object":"chat.completion","vendor_ext":{"region":"eu"},"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	cases := []struct {
		name        string
		stream      bool
		contentType string
		body        string
	}{
		{name: "普通JSON", contentType: "application/json", body: testOKBody},
		{name: "含厂商扩展字段", contentType: "application/json", body: vendorJSON},
		{name: "SSE流式", stream: true, contentType: "text/event-stream", body: sse},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer up.Close()

			h := build(t, up.URL)
			rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(tc.stream), "req-ok")
			if rec.Code != http.StatusOK {
				t.Fatalf("上游成功不得被改写, got %d: %s", rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); got != tc.body {
				t.Fatalf("成功响应必须逐字节透传\nwant %q\ngot  %q", tc.body, got)
			}
			if len(h.clogs.inserted) != 1 || h.clogs.inserted[0].Status != "completed" {
				t.Fatalf("want 1 条 completed 调用日志, got %+v", h.clogs.inserted)
			}
			if h.clogs.inserted[0].UpstreamStatus != nil {
				t.Errorf("成功记录不应写 upstream_status, got %v", *h.clogs.inserted[0].UpstreamStatus)
			}
		})
	}
}

// TestUpstreamErrorPayload_Shapes 直接对组装函数做形状级断言（不经 HTTP），
// 覆盖 HTTP 层不易构造的边界：截断后不再是合法 JSON、顶层数组、标量、只有 error 字符串。
func TestUpstreamErrorPayload_Shapes(t *testing.T) {
	// 截断后的 OpenAI 形状不再是合法 JSON → 退化为「原文文本进 message + 同字段标注」。
	longBody := `{"error":{"message":"` + strings.Repeat("x", 9000) + `"}}`
	payload, _ := upstreamErrorPayload(http.StatusBadRequest, longBody, "rid").(upstreamErrorEnvelope)
	if !strings.Contains(payload.Error.Message, "已截断") || len(payload.Error.Message) > upstreamErrOutBytes {
		t.Fatalf("超长必须在同一字段标注截断且不超上限, got len=%d", len(payload.Error.Message))
	}
	// 标量 JSON（非对象）→ 原文文本进 message。
	payload, _ = upstreamErrorPayload(http.StatusBadRequest, `"plain string error"`, "rid").(upstreamErrorEnvelope)
	if payload.Error.Message != `"plain string error"` {
		t.Fatalf("标量 JSON 应按原文文本透出, got %q", payload.Error.Message)
	}
	// error 是字符串（非 OpenAI 形状）→ 原文文本进 message，不硬解成 error 子对象。
	payload, _ = upstreamErrorPayload(http.StatusBadRequest, `{"error":"boom"}`, "rid").(upstreamErrorEnvelope)
	if payload.Error.Message != `{"error":"boom"}` {
		t.Fatalf("非 OpenAI 形状应按原文文本透出, got %q", payload.Error.Message)
	}
	// OpenAI 形状且上游自带 request_id 时不覆盖上游的（只追加我们缺的字段）。
	m, ok := upstreamErrorPayload(http.StatusBadRequest,
		`{"error":{"message":"x"},"request_id":"upstream-rid"}`, "gw-rid").(map[string]json.RawMessage)
	if !ok {
		t.Fatal("OpenAI 形状应走整段透传")
	}
	if string(m["request_id"]) != `"upstream-rid"` {
		t.Fatalf("不得改写上游自带的 request_id, got %s", m["request_id"])
	}
}

// TestServeRejectPaths_RecordBillingAndCallLog 网关自身拒绝（未进入上游转发）同样必须「账单 + 调用日志」成对：
// 账单页按 request_id 检索是经 call_logs 关联的（billing/query.go），只落账单不落调用日志时，
// 调用方拿着 response 里的 request_id 在账单页搜不到这次失败。
func TestServeRejectPaths_RecordBillingAndCallLog(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode int
		setup    func(h *harness)
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
				t.Errorf("错误体 request_id want %q，got %q", rb.RequestID, rid)
			}
		})
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

// timeoutErr 满足 net.Error 且 Timeout()=true（模拟上游响应超时）。
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// timeoutFailTransport 模拟「请求已送达后上游超时」：先触发 WroteRequest，再返回超时错误。
type timeoutFailTransport struct{}

func (timeoutFailTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if tr := httptrace.ContextClientTrace(req.Context()); tr != nil && tr.WroteRequest != nil {
		tr.WroteRequest(httptrace.WroteRequestInfo{Err: nil})
	}
	return nil, timeoutErr{}
}

// tail 取字符串末尾 n 字节（断言失败时展示截断标注用）。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
