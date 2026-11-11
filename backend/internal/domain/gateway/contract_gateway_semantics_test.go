package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 本文件是**对外契约的基线守卫**：期望值全部按既有网关语义手写（不看实现里的映射函数），
// 用来钉住「本 PR 没有改对外契约」这条红线。
//
// 基线（与 main 一致）：
//   - HTTP 状态码：不可转移的 4xx（400/409/413/422/504）→ 502；可转移失败耗尽
//     （401/402/403/404/5xx）→ 503；全部候选被上游 429 限流 → 429；
//   - 响应体结构：只有 {"error":{"message","type","code"},"request_id"}，
//     没有 upstream_status，也没有把上游 error 子对象搬过来；
//   - error.type / error.code 由**对外状态码**按 OpenAI 语义归类（不取上游业务码 / 上游 type）；
//   - 唯一允许的自由文本差异是 error.message：里面带上游原文（遮蔽本次凭据、8KB 截断）。
//   - 上游真实状态码只落运营侧（call_logs.upstream_status），不进响应体。

// TestContractRevert_StatusCodesFollowGatewaySemantics 上游各种状态码 → 对外状态码/type/code 的映射。
func TestContractRevert_StatusCodesFollowGatewaySemantics(t *testing.T) {
	cases := []struct {
		name     string
		upstream int
		wantDown int
		wantType string // 基线手写值，不调实现函数
		wantCode string
	}{
		{"400 不可转移", http.StatusBadRequest, http.StatusBadGateway, "server_error", "upstream_error"},
		{"401 可转移耗尽", http.StatusUnauthorized, http.StatusServiceUnavailable, "server_error", "service_unavailable"},
		{"402 可转移耗尽", http.StatusPaymentRequired, http.StatusServiceUnavailable, "server_error", "service_unavailable"},
		{"403 可转移耗尽", http.StatusForbidden, http.StatusServiceUnavailable, "server_error", "service_unavailable"},
		{"404 可转移耗尽", http.StatusNotFound, http.StatusServiceUnavailable, "server_error", "service_unavailable"},
		{"409 不可转移", http.StatusConflict, http.StatusBadGateway, "server_error", "upstream_error"},
		{"413 不可转移", http.StatusRequestEntityTooLarge, http.StatusBadGateway, "server_error", "upstream_error"},
		{"422 不可转移", http.StatusUnprocessableEntity, http.StatusBadGateway, "server_error", "upstream_error"},
		{"429 全部限流", http.StatusTooManyRequests, http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded"},
		{"500 可转移耗尽", http.StatusInternalServerError, http.StatusServiceUnavailable, "server_error", "service_unavailable"},
		{"503 可转移耗尽", http.StatusServiceUnavailable, http.StatusServiceUnavailable, "server_error", "service_unavailable"},
		{"504 不可转移", http.StatusGatewayTimeout, http.StatusBadGateway, "server_error", "upstream_error"},
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
				body := `{"error":{"type":"invalid_request_error","message":"upstream says: region not enabled",` +
					`"code":"upstream_biz_code","details":[{"field":"region","trace":"tr-1"}]}}`
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
				t.Logf("上游 %d → 对外 %d, error.type=%v, error.code=%v", tc.upstream, rec.Code,
					errField(out, "type"), errField(out, "code"))

				// ① 状态码 = 基线。
				if rec.Code != tc.wantDown {
					t.Fatalf("对外状态码 want %d（基线）, got %d（上游 %d）: %s",
						tc.wantDown, rec.Code, tc.upstream, rec.Body.String())
				}
				// ② 结构 = 基线：顶层只有 error + request_id；error 只有 message/type/code。
				if len(out) != 2 {
					t.Fatalf("响应顶层只能有 error + request_id, got %v", keysOf(out))
				}
				if _, exists := out["upstream_status"]; exists {
					t.Fatal("响应体不得带 upstream_status（基线没有该字段）")
				}
				errObj, ok := out["error"].(map[string]any)
				if !ok || len(errObj) != 3 {
					t.Fatalf("error 只能有 message/type/code, got %#v", out["error"])
				}
				for _, k := range []string{"message", "type", "code"} {
					if _, exists := errObj[k]; !exists {
						t.Fatalf("error 缺少字段 %q: %#v", k, errObj)
					}
				}
				// ③ type/code = 基线（网关按对外状态码归类，不取上游 type/业务码）。
				if got := errField(out, "type"); got != tc.wantType {
					t.Errorf("error.type want %q（基线）, got %v", tc.wantType, got)
				}
				if got := errField(out, "code"); got != tc.wantCode {
					t.Errorf("error.code want %q（基线）, got %v", tc.wantCode, got)
				}
				if got := errField(out, "code"); got == "upstream_biz_code" {
					t.Error("不得透出上游业务码")
				}
				// ④ message 是自由文本：允许带上游原文（这是唯一的差异点）。
				if msg, _ := errField(out, "message").(string); !strings.Contains(msg, "upstream says: region not enabled") {
					t.Errorf("error.message 必须带上游原文, got %q", msg)
				}
				// ⑤ request_id 与中间件同源。
				if out["request_id"] != rid {
					t.Errorf("request_id want %q, got %v", rid, out["request_id"])
				}
				// ⑥ 上游真实码不丢：落在运营侧调用日志里。
				if len(h.clogs.inserted) != 1 {
					t.Fatalf("want 1 条调用日志, got %d", len(h.clogs.inserted))
				}
				cl := h.clogs.inserted[0]
				if cl.Status != "failed" || cl.UpstreamStatus == nil || *cl.UpstreamStatus != tc.upstream {
					t.Errorf("调用日志 want status=failed/upstream_status=%d, got %q/%v",
						tc.upstream, cl.Status, cl.UpstreamStatus)
				}
			})
		}
	}
}

// keysOf 取 map 的键（失败信息展示用）。
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// errField 取对外响应 error 对象里的某个字段（缺失返回 nil）。
func errField(out map[string]any, key string) any {
	e, ok := out["error"].(map[string]any)
	if !ok {
		return nil
	}
	return e[key]
}
