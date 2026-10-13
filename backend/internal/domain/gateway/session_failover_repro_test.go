package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/model"
	"github.com/team/llmgateway/internal/domain/router"
)

// ===== 真实路由 + 真实渠道运行时 的会话满 failover 复现 =====
//
// 复现目标：3 个渠道（各 1 密钥，并发会话上限=1），第一个渠道会话满时，
// 新会话是否 failover 到下一个渠道（而非直接 503）。
// 与既有单测不同：这里用真实 router.Engine（含候选排序/会话计数）+ 真实 channel.Manager，
// 排除 fake 固定候选顺序带来的假象。

// reproCMSource 满足 router 的 channelModelSource（ListByExternal）。
type reproCMSource struct{ cms []channel.ChannelModel }

func (r *reproCMSource) ListByExternal(_ context.Context, _ int64) ([]channel.ChannelModel, error) {
	return r.cms, nil
}

// reproHarness 真实路由链路的测试装配。
type reproHarness struct {
	gw      *Gateway
	mgr     *channel.Manager
	engine  *router.Engine
	sess    *router.SessionRegistry
	billing *fakeBilling
	clogs   *fakeCallLogs
	clock   *testClock
}

func digit(i int) string { return string(rune('0' + i)) }

// buildReal 装配「真实 router.Engine + 真实 channel.Manager + mock 上游」的网关。
// n 个渠道：ID/密钥 ID/模型行 ID 依次为 1..n，各绑定同一对外模型，优先级由 priorities 指定。
func buildReal(t *testing.T, upstreamURL string, n int, priorities map[int64]int, maxSessions int) *reproHarness {
	t.Helper()
	clock := &testClock{t: time.Now()}
	sess := router.NewSessionRegistry(clock.now)

	mgr := channel.NewManager(nil, nil, []byte("0123456789abcdef"), nil,
		slog.New(slog.NewTextHandler(io.Discard, nil)), clock.now)

	var cms []channel.ChannelModel
	for i := 1; i <= n; i++ {
		id := int64(i)
		rl := channel.DefaultRateLimit()
		rl.MaxConcurrent = maxSessions
		ch := &channel.Channel{
			ID: id, Name: "ch" + digit(i), Protocol: channel.ProtocolOpenAICompat,
			BaseURL: upstreamURL, Priority: priorities[id], State: channel.StateNormal,
			RateLimit: rl,
		}
		mgr.Upsert(ch)

		cm := channel.ChannelModel{
			ID: 100*id + 1, ChannelID: id, InternalModelID: "gpt-4-internal",
			ExternalModelID: 1, State: channel.StateNormal,
			CostRates: billing.Rates{"input": 1, "output": 1, "cache_read": 1, "cache_write": 1, "reasoning": 1},
		}
		mgr.UpsertModel(&cm)
		cms = append(cms, cm)

		mgr.UpsertKey(channel.ChannelKey{ID: id, ChannelID: id, Name: "k" + digit(i), State: channel.StateNormal})
		// 密钥凭据明文直接注入（跳过 SM4 加解密，聚焦会话满路径）。
		if kr, ok := mgr.GetKeyRuntime(id); ok {
			kr.CredentialPlain = "sk-test-" + digit(i)
		}
	}

	extModel := func() *model.ExternalModel {
		return &model.ExternalModel{ID: 1, ExternalName: "gpt-4", Enabled: true,
			SaleRates: billing.Rates{"input": 1, "output": 2, "cache_read": 1, "cache_write": 1, "reasoning": 1}}
	}
	engine := router.NewEngine(&fakeModel{mapping: extModel()}, &reproCMSource{cms: cms}, mgr, sess)

	bf := &fakeBilling{}
	gw := NewGateway(GatewayConfig{
		Tokens:        &fakeToken{tok: &identity.Token{ID: 42, UserID: 7, Status: identity.StatusTokenActive}},
		Router:        engine,
		Channels:      mgr,
		Billing:       bf,
		Models:        &fakeModel{mapping: extModel()},
		ChannelModels: &fakeChannelModel{cm: &cms[0]},
		TagKV:         func(_ context.Context, _ *int64) (map[string]string, error) { return nil, nil },
		Pricing:       func(_ context.Context, _ int64) (string, error) { return identity.PricingModeSale, nil },
		Now:           clock.now,
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	clog := &fakeCallLogs{}
	gw.SetCallLogs(clog)

	return &reproHarness{gw: gw, mgr: mgr, engine: engine, sess: sess,
		billing: bf, clogs: clog, clock: clock}
}

// mockUpstreamSleep 模拟 OpenAI 空后端：sleep 后返回带 usage 的 completion。
const mockUpstreamSleep = `{"id":"chatcmpl-test","object":"chat.completion","created":1,"model":"gpt-4-internal","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`

// startSleepUpstream 启动一个 sleep 指定时长后返回的 mock OpenAI 上游。
func startSleepUpstream(t *testing.T, d time.Duration) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(d)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, mockUpstreamSleep)
	}))
	t.Cleanup(s.Close)
	return s
}

func reasons(ats []map[string]any) []string {
	out := make([]string, 0, len(ats))
	for _, a := range ats {
		out = append(out, a["reason"].(string))
	}
	return out
}

// lastReasons 提取最近一次调用日志的决策轨迹原因序列。
func lastReasons(h *reproHarness) []string {
	if len(h.clogs.inserted) == 0 {
		return nil
	}
	d, _ := h.clogs.inserted[len(h.clogs.inserted)-1].Decision.(map[string]any)
	ats, _ := d["attempts"].([]map[string]any)
	return reasons(ats)
}

// TestRepro_SessionFull_Failover_RealRouter：渠道 1 优先级最高（永远排首候选），
// 其唯一会话槽被占满后，新会话应 failover 到渠道 2，而不是 503。
func TestRepro_SessionFull_Failover_RealRouter(t *testing.T) {
	up := startSleepUpstream(t, 0)
	h := buildReal(t, up.URL, 3, map[int64]int{1: 10}, 1)

	rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"a"}]}`,
		map[string]string{"x-session-id": "s1"})
	if rec.Code != http.StatusOK {
		t.Fatalf("req1 want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if got := h.billing.reqs[0].ChannelKeyID; got != 1 {
		t.Fatalf("req1 应落渠道1(key1)，got key=%d", got)
	}

	// 渠道 1 唯一会话槽已被 s1 占用 → 新会话 s2 需 failover。
	rec = doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"b"}]}`,
		map[string]string{"x-session-id": "s2"})
	t.Logf("req2 决策轨迹 reasons = %v", lastReasons(h))
	if rec.Code != http.StatusOK {
		t.Fatalf("会话满应 failover 到下一渠道，want 200, got %d: %s；reasons=%v",
			rec.Code, rec.Body.String(), lastReasons(h))
	}
	if got := h.billing.reqs[1].ChannelKeyID; got != 2 {
		t.Fatalf("req2 应落渠道2(key2)，got key=%d", got)
	}
}

// TestRepro_SessionFull_AllFull_503：3 个渠道会话槽全占满 → 终结 503。
func TestRepro_SessionFull_AllFull_503(t *testing.T) {
	up := startSleepUpstream(t, 0)
	h := buildReal(t, up.URL, 3, map[int64]int{1: 10}, 1)

	for _, sid := range []string{"s1", "s2", "s3"} {
		rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t",
			`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"x"}]}`,
			map[string]string{"x-session-id": sid})
		if rec.Code != http.StatusOK {
			t.Fatalf("占位请求 %s want 200, got %d: %s", sid, rec.Code, rec.Body.String())
		}
	}
	rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"y"}]}`,
		map[string]string{"x-session-id": "s4"})
	t.Logf("all-full 决策轨迹 reasons = %v", lastReasons(h))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("全部候选会话满应 503, got %d: %s", rec.Code, rec.Body.String())
	}
}

// TestRepro_SameSession_Sticky：同一 x-session-id 重复调用 → 粘性复用渠道（不产生会话满，不迁移）。
func TestRepro_SameSession_Sticky(t *testing.T) {
	up := startSleepUpstream(t, 0)
	h := buildReal(t, up.URL, 3, map[int64]int{1: 10, 2: 9, 3: 8}, 1)

	for i := 0; i < 3; i++ {
		rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t",
			`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"same"}]}`,
			map[string]string{"x-session-id": "sticky"})
		if rec.Code != http.StatusOK {
			t.Fatalf("第%d次 want 200, got %d: %s；reasons=%v", i+1, rec.Code, rec.Body.String(), lastReasons(h))
		}
		if got := h.billing.reqs[i].ChannelKeyID; got != 1 {
			t.Fatalf("第%d次应粘性在 key1，got key=%d", i+1, got)
		}
	}
}

// TestRepro_ConcurrentDistinctSessions_Distribute：并发不同会话 → 应分散到多个渠道，不应 503。
func TestRepro_ConcurrentDistinctSessions_Distribute(t *testing.T) {
	up := startSleepUpstream(t, 200*time.Millisecond)
	h := buildReal(t, up.URL, 3, nil, 1)

	type res struct {
		code int
		body string
	}
	const n = 3
	ch := make(chan res, n)
	for i := 0; i < n; i++ {
		sid := "c" + digit(i)
		go func() {
			rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t",
				`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"z"}]}`,
				map[string]string{"x-session-id": sid})
			ch <- res{rec.Code, rec.Body.String()}
		}()
	}
	for i := 0; i < n; i++ {
		r := <-ch
		if r.code != http.StatusOK {
			t.Fatalf("并发请求 want 200, got %d: %s", r.code, r.body)
		}
	}
	seen := map[int64]bool{}
	for _, rq := range h.billing.reqs {
		seen[rq.ChannelKeyID] = true
	}
	if len(seen) != 3 {
		t.Fatalf("3 个不同会话应分散到 3 个渠道，实际命中渠道 = %v", seen)
	}
}
