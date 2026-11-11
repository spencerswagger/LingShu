package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 本文件钉住「对外 HTTP 状态码 = 网关语义」这条契约（v3 回退后）：
// 上游 4xx/5xx 一律不原样透出（401/402/403 原样透出会被调用方判成「我方 key 无效 / 我方余额不足」），
// 上游真实码只出现在 error.upstream_status 与调用日志里。
//
// 与 upstream_error_passthrough_test.go 的分工：那边测「内容透传」（message/type/原文逐字节），
// 这里测「状态码语义」（上游码 → 对外码的映射），两者互不依赖。

// TestContractRevert_StatusCodesFollowGatewaySemantics 上游各种状态码 → 对外状态码的实测映射。
func TestContractRevert_StatusCodesFollowGatewaySemantics(t *testing.T) {
	cases := []struct {
		name       string
		upstream   int
		wantDown   int
		wantCode   string
		upstreamOK bool // 是否应带 error.upstream_status
	}{
		{name: "400 不可转移", upstream: http.StatusBadRequest, wantDown: http.StatusBadGateway, wantCode: "upstream_error", upstreamOK: true},
		{name: "401 可转移耗尽", upstream: http.StatusUnauthorized, wantDown: http.StatusServiceUnavailable, wantCode: "service_unavailable", upstreamOK: true},
		{name: "402 可转移耗尽", upstream: http.StatusPaymentRequired, wantDown: http.StatusServiceUnavailable, wantCode: "service_unavailable", upstreamOK: true},
		{name: "403 可转移耗尽", upstream: http.StatusForbidden, wantDown: http.StatusServiceUnavailable, wantCode: "service_unavailable", upstreamOK: true},
		{name: "404 可转移耗尽", upstream: http.StatusNotFound, wantDown: http.StatusServiceUnavailable, wantCode: "service_unavailable", upstreamOK: true},
		{name: "422 不可转移", upstream: http.StatusUnprocessableEntity, wantDown: http.StatusBadGateway, wantCode: "upstream_error", upstreamOK: true},
		{name: "429 全部限流", upstream: http.StatusTooManyRequests, wantDown: http.StatusTooManyRequests, wantCode: "rate_limit_exceeded", upstreamOK: true},
		{name: "500 可转移耗尽", upstream: http.StatusInternalServerError, wantDown: http.StatusServiceUnavailable, wantCode: "service_unavailable", upstreamOK: true},
		{name: "503 可转移耗尽", upstream: http.StatusServiceUnavailable, wantDown: http.StatusServiceUnavailable, wantCode: "service_unavailable", upstreamOK: true},
		{name: "504 不可转移", upstream: http.StatusGatewayTimeout, wantDown: http.StatusBadGateway, wantCode: "upstream_error", upstreamOK: true},
	}
	for _, tc := range cases {
		tc := tc
		for _, stream := range []bool{false, true} {
			name := tc.name + "/非流式"
			if stream {
				name = tc.name + "/流式"
			}
			t.Run(name, func(t *testing.T) {
				body := `{"error":{"type":"invalid_request_error","message":"upstream says: region not enabled","code":"upstream_biz_code"}}`
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tc.upstream)
					_, _ = w.Write([]byte(body))
				}))
				defer up.Close()

				h := build(t, up.URL)
				const rid = "req-contract"
				rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(stream), rid)

				var out map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
					t.Fatalf("对外响应必须是 JSON: %v (%s)", err, rec.Body.String())
				}
				t.Logf("上游 %d → 对外 %d, error.code=%v, error.upstream_status=%v, message=%q",
					tc.upstream, rec.Code, errField(out, "code"), errField(out, "upstream_status"), errField(out, "message"))

				// ① 对外状态码 = 网关语义（不沿用上游）。
				if rec.Code != tc.wantDown {
					t.Fatalf("对外状态码 want %d（网关语义）, got %d（上游 %d）: %s", tc.wantDown, rec.Code, tc.upstream, rec.Body.String())
				}
				// ② error.code 由网关归类，不透出上游业务码。
				if got := errField(out, "code"); got != tc.wantCode {
					t.Errorf("error.code want %q, got %v", tc.wantCode, got)
				}
				if got := errField(out, "code"); got == "upstream_biz_code" {
					t.Error("不得透出上游业务码")
				}
				// ③ 上游真实码不丢：写在 error 对象内，不在顶层。
				if _, exists := out["upstream_status"]; exists {
					t.Error("upstream_status 不得出现在响应体顶层")
				}
				if tc.upstreamOK {
					if got := errorUpstreamStatus(t, out); got != tc.upstream {
						t.Errorf("error.upstream_status want %d, got %d", tc.upstream, got)
					}
				}
				// ④ 内容仍透传：上游 message 原文可见，type 保留上游原文。
				if msg, _ := errField(out, "message").(string); msg != "upstream says: region not enabled" {
					t.Errorf("error.message 必须是上游原文, got %q", msg)
				}
				if typ := errField(out, "type"); typ != "invalid_request_error" {
					t.Errorf("error.type 应保留上游原文, got %v", typ)
				}
				if out["request_id"] != rid {
					t.Errorf("request_id want %q, got %v", rid, out["request_id"])
				}
				// ⑤ 调用日志记上游真实码（不是对外码）。
				if len(h.clogs.inserted) != 1 {
					t.Fatalf("want 1 条调用日志, got %d", len(h.clogs.inserted))
				}
				cl := h.clogs.inserted[0]
				if cl.Status != "failed" {
					t.Errorf("want status=failed, got %q", cl.Status)
				}
				if cl.UpstreamStatus == nil || *cl.UpstreamStatus != tc.upstream {
					t.Errorf("调用日志 upstream_status want %d, got %v", tc.upstream, cl.UpstreamStatus)
				}
			})
		}
	}
}

// errField 取对外错误体里 error 对象的字段（两种形状同一路径）。
func errField(out map[string]any, key string) any {
	e, ok := out["error"].(map[string]any)
	if !ok {
		return nil
	}
	return e[key]
}
