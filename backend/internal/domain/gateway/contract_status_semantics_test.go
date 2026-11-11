package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 本文件钉住「对外 HTTP 状态码 = origin/main 既有映射（本单不改任何一条）」这条契约。
//
// **期望值不是按实现函数推出来的，而是 2026-11-11 用临时分支跑 origin/main 实测得到的基线**
// （12 个上游码 × 单/双候选 × 流式/非流式 = 48 组组合，全部一致）：
//
//	400 / 409 / 413 / 422 / 504 → 502
//	401 / 402 / 403 / 404 / 500 / 503 → 503（可转移失败，候选耗尽）
//	429 → 429（全部候选被上游限流）
//
// 一句话：main 并非「4xx/5xx 一律 502」——可转移失败耗尽走 503、全限流走 429 都是既有行为。
// 本用例的作用是**防止任何人在本单里顺手改映射**：改一条这里就红。
//
// 与 upstream_error_passthrough_test.go 的分工：那边测「内容透传」（message/type/原文逐字节），
// 这里测「状态码语义」（上游码 → 对外码映射），两者互不依赖。

// TestContract_StatusCodesFollowGatewaySemantics 上游各种状态码 → 对外状态码的实测映射。
func TestContract_StatusCodesFollowGatewaySemantics(t *testing.T) {
	cases := []struct {
		name     string
		upstream int
		wantDown int
		wantCode string
	}{
		{name: "400 不可转移", upstream: http.StatusBadRequest, wantDown: http.StatusBadGateway, wantCode: "upstream_error"},
		{name: "401 可转移耗尽", upstream: http.StatusUnauthorized, wantDown: http.StatusServiceUnavailable, wantCode: "service_unavailable"},
		{name: "402 可转移耗尽", upstream: http.StatusPaymentRequired, wantDown: http.StatusServiceUnavailable, wantCode: "service_unavailable"},
		{name: "403 可转移耗尽", upstream: http.StatusForbidden, wantDown: http.StatusServiceUnavailable, wantCode: "service_unavailable"},
		{name: "404 可转移耗尽", upstream: http.StatusNotFound, wantDown: http.StatusServiceUnavailable, wantCode: "service_unavailable"},
		{name: "422 不可转移", upstream: http.StatusUnprocessableEntity, wantDown: http.StatusBadGateway, wantCode: "upstream_error"},
		{name: "429 全部限流", upstream: http.StatusTooManyRequests, wantDown: http.StatusTooManyRequests, wantCode: "rate_limit_exceeded"},
		{name: "500 可转移耗尽", upstream: http.StatusInternalServerError, wantDown: http.StatusServiceUnavailable, wantCode: "service_unavailable"},
		{name: "503 可转移耗尽", upstream: http.StatusServiceUnavailable, wantDown: http.StatusServiceUnavailable, wantCode: "service_unavailable"},
		{name: "504 不可转移", upstream: http.StatusGatewayTimeout, wantDown: http.StatusBadGateway, wantCode: "upstream_error"},
		{name: "409 不可转移", upstream: http.StatusConflict, wantDown: http.StatusBadGateway, wantCode: "upstream_error"},
		{name: "413 不可转移", upstream: http.StatusRequestEntityTooLarge, wantDown: http.StatusBadGateway, wantCode: "upstream_error"},
	}
	for _, tc := range cases {
		tc := tc
		for _, twoKeys := range []bool{false, true} {
			for _, stream := range []bool{false, true} {
				name := tc.name + "/非流式"
				if stream {
					name = tc.name + "/流式"
				}
				if twoKeys {
					name += "/双候选"
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
					if twoKeys {
						// 两把密钥打到同一个上游（同一状态码）：候选耗尽后的结论必须与单候选一致。
						addSecondKey(h, up.URL, "k2-secret")
					}
					const rid = "req-contract"
					rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(stream), rid)

					var out map[string]any
					if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
						t.Fatalf("对外响应必须是 JSON: %v (%s)", err, rec.Body.String())
					}
					t.Logf("上游 %d → 对外 %d（keys=%d stream=%v）, error.code=%v, error.upstream_status=%v",
						tc.upstream, rec.Code, map[bool]int{false: 1, true: 2}[twoKeys], stream,
						errField(out, "code"), errField(out, "upstream_status"))

					if rec.Code != tc.wantDown {
						t.Fatalf("对外状态码 want %d（main 基线）, got %d（上游 %d）: %s",
							tc.wantDown, rec.Code, tc.upstream, rec.Body.String())
					}
					if got := errField(out, "code"); got != tc.wantCode {
						t.Errorf("error.code want %q, got %v", tc.wantCode, got)
					}
					if got := errField(out, "code"); got == "upstream_biz_code" {
						t.Error("不得透出上游业务码")
					}
					if _, exists := out["upstream_status"]; exists {
						t.Error("upstream_status 不得出现在响应体顶层（应放 error 对象内）")
					}
					if got := errField(out, "upstream_status"); got != float64(tc.upstream) {
						t.Errorf("error.upstream_status want %d, got %v", tc.upstream, got)
					}
					if msg, _ := errField(out, "message").(string); msg != "upstream says: region not enabled" {
						t.Errorf("error.message 必须是上游原文, got %q", msg)
					}
					if typ := errField(out, "type"); typ != "invalid_request_error" {
						t.Errorf("error.type 应保留上游原文, got %v", typ)
					}
					if out["request_id"] != rid {
						t.Errorf("request_id want %q, got %v", rid, out["request_id"])
					}

					// 每次候选尝试各落一条调用日志；每条都记上游真实码（不是对外码）。
					// 双候选时只有**可转移**失败才会真的换候选（不可转移的 4xx 直接返回，只试一次）。
					wantLogs := 1
					if twoKeys && retryableUpstreamStatus(tc.upstream) {
						wantLogs = 2
					}
					if len(h.clogs.inserted) != wantLogs {
						t.Fatalf("want %d 条调用日志（每候选一条）, got %d", wantLogs, len(h.clogs.inserted))
					}
					for i, cl := range h.clogs.inserted {
						if cl.Status != "failed" {
							t.Errorf("调用日志[%d] want status=failed, got %q", i, cl.Status)
						}
						if cl.UpstreamStatus == nil || *cl.UpstreamStatus != tc.upstream {
							t.Errorf("调用日志[%d] upstream_status want %d, got %v", i, tc.upstream, cl.UpstreamStatus)
						}
					}
				})
			}
		}
	}
}
