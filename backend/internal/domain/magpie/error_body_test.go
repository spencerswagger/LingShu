package magpie

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestSummarizeUpstreamError_OpenAIShape 断言 OpenAI 形状取 error.message + code 前缀。
func TestSummarizeUpstreamError_OpenAIShape(t *testing.T) {
	raw := []byte(`{"error":{"message":"model\n not   found","type":"invalid_request_error","code":"model_not_found"}}`)
	got := summarizeUpstreamError(raw, "")
	if got != "model_not_found: model not found" {
		t.Fatalf("got %q", got)
	}
	// Anthropic 形状：顶层 type=error，error 里没有 code 时用 type 作前缀。
	anthropic := []byte(`{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens: too large"}}`)
	if got := summarizeUpstreamError(anthropic, ""); got != "invalid_request_error: max_tokens: too large" {
		t.Fatalf("Anthropic 形状摘要错误：%q", got)
	}
	// 纯文本（反代 502 页）原样保留。
	if got := summarizeUpstreamError([]byte("upstream connect error"), ""); got != "upstream connect error" {
		t.Fatalf("纯文本摘要错误：%q", got)
	}
}

// TestSummarizeUpstreamError_MasksCallerKey 断言上游回显的本次凭据被遮蔽（含 Bearer 形态）。
func TestSummarizeUpstreamError_MasksCallerKey(t *testing.T) {
	raw := []byte(`{"error":{"message":"bad key Bearer sk-live-123 (sk-live-123)"}}`)
	got := summarizeUpstreamError(raw, "sk-live-123")
	if strings.Contains(got, "sk-live-123") {
		t.Fatalf("摘要泄漏凭据：%q", got)
	}
	if !strings.Contains(got, "<redacted>") {
		t.Fatalf("应留下遮蔽标记：%q", got)
	}
}

// TestSummarizeUpstreamError_NonJSONAndTruncation 断言 HTML/纯文本按原文，且 UTF-8 安全截断。
func TestSummarizeUpstreamError_NonJSONAndTruncation(t *testing.T) {
	html := []byte("<html><body>502 Bad Gateway</body></html>")
	if got := summarizeUpstreamError(html, ""); got != "<html><body>502 Bad Gateway</body></html>" {
		t.Fatalf("HTML 应保留原文，got %q", got)
	}
	long := strings.Repeat("错", 400) // 1200 字节
	got := summarizeUpstreamError([]byte(long), "")
	if len(got) > upstreamErrorSummaryMaxBytes+len("…") {
		t.Fatalf("截断后仍超长：%d 字节", len(got))
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("截断应带省略号：%q", got[maxInt(0, len(got)-3):])
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("按 UTF-8 边界截断不应产生替换字符")
	}
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// TestForward_ErrorSummaryCaptured 断言 4xx 转发后 Result 带错误体摘要，
// 且下游收到的响应体与上游**逐字节相同**（摘要只是旁路，不改写透传字节）。
func TestForward_ErrorSummaryCaptured(t *testing.T) {
	const upstreamBody = `{"error":{"message":"model not found","type":"invalid_request_error","code":"model_not_found"}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer srv.Close()

	body := []byte(`{"model":"m","messages":[]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	res := sseTestProxy(srv.URL).Forward(rec, req,
		Route{Protocol: ProtocolOpenAIChat, Endpoint: "chat.completions", UpstreamPath: "/v1/chat/completions", Billable: true},
		Prepared{TokenID: 1, UserID: 1, ExternalModel: "m", MagpieModel: "m-up",
			UpstreamPath: "/v1/chat/completions", Body: body})

	if res.Status != http.StatusBadRequest {
		t.Fatalf("status = %d", res.Status)
	}
	if res.UpstreamErrorSummary != "model_not_found: model not found" {
		t.Fatalf("错误体摘要 = %q", res.UpstreamErrorSummary)
	}
	if rec.Body.String() != upstreamBody {
		t.Fatalf("透传字节被改写：%q", rec.Body.String())
	}
}

// TestForward_ErrorSummaryMasksCredential 端到端：上游把 callerKey 写进错误体，
// 摘要里不能出现明文（下游响应体本身仍按契约遮蔽）。
func TestForward_ErrorSummaryMasksCredential(t *testing.T) {
	const secret = "sk-live-abc123"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"invalid key ` + secret + `","code":"invalid_api_key"}}`))
	}))
	defer srv.Close()

	body := []byte(`{"model":"m"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	res := sseTestProxy(srv.URL).Forward(rec, req,
		Route{Protocol: ProtocolOpenAIChat, Endpoint: "chat.completions", UpstreamPath: "/v1/chat/completions", Billable: true},
		Prepared{TokenID: 1, UserID: 1, ExternalModel: "m", MagpieModel: "m-up",
			CallerKey: secret, UpstreamPath: "/v1/chat/completions", Body: body})

	if strings.Contains(res.UpstreamErrorSummary, secret) {
		t.Fatalf("摘要泄漏凭据：%q", res.UpstreamErrorSummary)
	}
	if strings.Contains(rec.Body.String(), secret) {
		t.Fatalf("下游响应泄漏凭据：%q", rec.Body.String())
	}
}

// TestForward_ErrorSummarySkipsCompressedBody 断言没配凭据时的 gzip 错误体不去解析
// （捕到的是压缩字节，写摘要只会误导运营）。
func TestForward_ErrorSummarySkipsCompressedBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte{0x1f, 0x8b, 0x08, 0x00}) // 假 gzip 头
	}))
	defer srv.Close()

	body := []byte(`{"model":"m"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	res := sseTestProxy(srv.URL).Forward(rec, req,
		Route{Protocol: ProtocolOpenAIChat, Endpoint: "chat.completions", UpstreamPath: "/v1/chat/completions", Billable: true},
		Prepared{TokenID: 1, UserID: 1, ExternalModel: "m", MagpieModel: "m-up",
			UpstreamPath: "/v1/chat/completions", Body: body})

	if !strings.Contains(res.UpstreamErrorSummary, "未解析") {
		t.Fatalf("压缩错误体应标注未解析，实际 %q", res.UpstreamErrorSummary)
	}
}
