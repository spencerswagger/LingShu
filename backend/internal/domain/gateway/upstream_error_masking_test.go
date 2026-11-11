package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件钉住「对外透传不得把**渠道凭据**转手给下游」这条安全红线。
//
// 背景：上游偶尔会把它收到的鉴权头回显在自己的错误体里（如 "Incorrect API key provided: sk-…"）。
// 透传上游原文是本单的目标，但把 `Authorization: Bearer <渠道密钥>` 原样发给下游 API 用户，
// 等于把渠道密钥交给任何持有平台 token 的租户——所以对外只遮蔽**本次实际使用的那一把**，
// 其余字节（含 request-id、模型名、正文）仍逐字节保留。

// upstreamEchoBody 上游把收到的凭据回显在错误体里（两种常见位置）。
func upstreamEchoBody(cred string) string {
	return `{"error":{"type":"invalid_request_error",` +
		`"message":"Incorrect API key provided: ` + cred + `. Check your account.",` +
		`"echo":"Bearer ` + cred + `"}}`
}

// TestServeUpstreamError_DownstreamNeverSeesChannelCredential 对外响应与落库都不得出现渠道凭据，
// 且除凭据那一段外内容仍逐字节保留。
func TestServeUpstreamError_DownstreamNeverSeesChannelCredential(t *testing.T) {
	const cred = "sk-live-CHANNELKEY-9f2c4b7e1d"
	for _, tc := range []struct {
		name          string
		status        int
		body          string
		wantFragments []string
	}{
		{name: "OpenAI 形状（400 不可转移）", status: http.StatusBadRequest, body: upstreamEchoBody(cred),
			wantFragments: []string{"Incorrect API key provided", "Check your account"}},
		{name: "纯文本（500 可转移→终结）", status: http.StatusInternalServerError,
			body: "upstream failed: auth=Bearer " + cred + " rid=abc", wantFragments: []string{"upstream failed", "rid=abc"}},
		{name: "非对象 JSON（429 终结）", status: http.StatusTooManyRequests,
			body: `[{"detail":"quota exhausted for ` + cred + `"}]`, wantFragments: []string{"quota exhausted"}},
	} {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer up.Close()

			h := build(t, up.URL)
			h.chans.keys[1].CredentialPlain = cred // 本次转发实际使用的渠道凭据
			const rid = "req-mask"
			rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), rid)

			// ① 对外响应不得出现渠道凭据，且凭据位置应替换成 ***（不是整段丢弃内容）。
			if strings.Contains(rec.Body.String(), cred) {
				t.Fatalf("对外响应泄漏渠道凭据: %s", rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "***") {
				t.Fatalf("凭据位置应替换成 ***（内容不得整段丢弃）: %s", rec.Body.String())
			}
			// ② 除凭据外内容仍在：message 里必须能看到上游文案的可辨认片段（不是整段被抹掉）。
			var out map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			msg, _ := errField(out, "message").(string)
			if msg == "" {
				t.Fatal("message 不能为空")
			}
			for _, frag := range tc.wantFragments {
				if !strings.Contains(msg, frag) {
					t.Fatalf("message 必须保留上游原文片段 %q（除凭据外不得丢内容）: %q", frag, head(msg, 200))
				}
			}
			// ③ 落库同样不得出现凭据（resp_body / error_message 两条都要干净）。
			if len(h.clogs.inserted) != 1 {
				t.Fatalf("want 1 条调用日志, got %d", len(h.clogs.inserted))
			}
			cl := h.clogs.inserted[0]
			if strings.Contains(cl.RespBody, cred) || strings.Contains(cl.ErrorMessage, cred) {
				t.Fatalf("落库泄漏渠道凭据: resp=%q err=%q", cl.RespBody, cl.ErrorMessage)
			}
			if len(h.billing.reqs) != 1 || strings.Contains(h.billing.reqs[0].ErrorMessage, cred) {
				t.Fatalf("账单 error_message 泄漏渠道凭据: %+v", h.billing.reqs)
			}
		})
	}
}

// TestServeUpstreamError_OutwardMessageHasNoUpstreamAddress 断网/构建失败时对外文案不得带渠道地址
// （主机名/IP/端口/URL）：下游租户不该知道平台把请求打到了哪台机器。
//
// 说明：本分支对外恒为网关文案（不含 err 原文），本用例是**回归守卫**——
// 后续若有人把 err.Error() 直接塞进对外 message，会在这里红。
func TestServeUpstreamError_OutwardMessageHasNoUpstreamAddress(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL
	dead.Close()

	h := build(t, deadURL)
	rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), "req-noaddr")
	body := rec.Body.String()
	for _, bad := range []string{"127.0.0.1", "http://", "https://", deadURL} {
		if strings.Contains(body, bad) {
			t.Fatalf("对外文案不得含渠道地址片段 %q: %s", bad, body)
		}
	}
	// 但运营侧（调用日志）要能定位是哪台机器：完整错误（含地址）必须留在 error_message。
	if len(h.clogs.inserted) != 1 {
		t.Fatalf("want 1 条调用日志, got %d", len(h.clogs.inserted))
	}
	if msg := h.clogs.inserted[0].ErrorMessage; !strings.Contains(msg, "127.0.0.1") && !strings.Contains(msg, "connection refused") {
		t.Fatalf("调用日志应保留完整连接错误（含地址）便于运营定位, got %q", msg)
	}
}

// errField 取对外错误体里 error 对象的字段（各用例共用）。
func errField(out map[string]any, key string) any {
	e, ok := out["error"].(map[string]any)
	if !ok {
		return nil
	}
	return e[key]
}
