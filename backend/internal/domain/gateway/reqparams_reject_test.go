package gateway

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
	"github.com/team/llmgateway/internal/domain/router"
)

// 本文件是 PR #63 Owl 第二轮评审 P2-3 的回归：**拒绝类场景（503/429/协议不支持）也必须有请求参数快照**。
// 根因：serve 开头就取了 ctx := r.Context()，之后才把快照挂到 r 上；如果不刷新 ctx，
// 这些「没走到组包处」的拒绝路径写调用日志时用的是**未挂快照的旧 ctx** → req_params 为空，
// 而这恰恰是最需要看现场的三类失败。

// assertSnapshotOnLog 断言写入的调用日志带请求级快照，且关键字段齐全。
func assertSnapshotOnLog(t *testing.T, h *harness, wantEndpoint string) {
	t.Helper()
	if len(h.clogs.inserted) == 0 {
		t.Fatal("拒绝路径没有写调用日志")
	}
	cl := h.clogs.inserted[len(h.clogs.inserted)-1]
	raw := cl.ReqParams // CallLog.ReqParams 是 JSONB 原始字节（[]byte）
	if len(raw) == 0 {
		t.Fatal("拒绝路径的 req_params 为空（Owl P2-3）")
	}
	var snap reqParamsSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("req_params 反序列化失败: %v", err)
	}
	if snap.Endpoint != wantEndpoint {
		t.Fatalf("endpoint=%q, want %q", snap.Endpoint, wantEndpoint)
	}
	if snap.Model != "gpt-4" || snap.InboundProtocol != "openai-chat" || snap.TokenID != 42 {
		t.Fatalf("快照关键字段缺失: %+v", snap)
	}
}

// TestServe_RejectPathsCarryReqParams 三个拒绝分支逐一断言快照非空。
func TestServe_RejectPathsCarryReqParams(t *testing.T) {
	body := `{"model":"gpt-4","stream":false,"temperature":0.2,"max_tokens":64,
		"messages":[{"role":"user","content":"ping"}]}`

	t.Run("503 无可用渠道", func(t *testing.T) {
		h := build(t, "http://unused")
		h.rrouter.err = &router.RouteError{Kind: router.KindNoAvailable, HTTPStatus: http.StatusServiceUnavailable, Message: msgNoAvailable}
		rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", body)
		if rec.Code != http.StatusServiceUnavailable {
			t.Fatalf("want 503, got %d: %s", rec.Code, rec.Body.String())
		}
		assertSnapshotOnLog(t, h, "/v1/chat/completions")
	})

	t.Run("429 本地限流拒绝", func(t *testing.T) {
		h := build(t, "http://unused")
		// 模型级限流降到最小且 REJECT：请求必被本地限流拒绝（不发起上游调用）。
		h.chans.mrt.Model.RateLimit = channel.RateLimitConfig{OnExceed: channel.OnExceedReject}
		h.chans.mrt.Limiter = channel.NewLimiter(100, 1, 1.0)
		rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", body)
		if rec.Code != http.StatusTooManyRequests {
			t.Fatalf("want 429, got %d: %s", rec.Code, rec.Body.String())
		}
		assertSnapshotOnLog(t, h, "/v1/chat/completions")
	})

	t.Run("协议不支持", func(t *testing.T) {
		h := build(t, "http://unused")
		// 模型可达协议仅 anthropic-messages，且关闭跨协议转换 → 直通未命中、也不转换 → 固定 400。
		h.chans.mrt.Model.Protocols = channelprotocol.Protocols{channelprotocol.ProtocolAnthropicMessages: {}}
		h.gw.protocolConvEnabled = func() bool { return false }
		rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-t", body)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("want 400（协议不支持）, got %d: %s", rec.Code, rec.Body.String())
		}
		assertSnapshotOnLog(t, h, "/v1/chat/completions")
	})
}
