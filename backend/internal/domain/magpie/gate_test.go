package magpie

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/team/llmgateway/internal/domain/model"
)

// 降级开关（sys_configs：upstream.degrade.passthrough_enabled）必须有真实消费方。
//
// 背景（Owl 复审 #84 P1-2）：这个键写进配置表后如果没人读，运维「关掉转发」却照旧转发，
// 开关等于假的。另一条同样致命的方向：如果拿 `upstream.base_url` 当门（它是空串默认值、
// 又没有管理端写入口），会把所有 /v1 请求关成 503 —— 所以门只看开关本身。

// TestMount_GateClosedReturns503AndNeverTouchesUpstream 关掉开关时：503 + 不请求上游 + 不产生计费事实。
func TestMount_GateClosedReturns503AndNeverTouchesUpstream(t *testing.T) {
	env := newTestEnv(t, baseTestConfig(), map[string]*model.ExternalModel{"m1": gptModel("m1", "upstream/m1")}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	env.handler.SetGate(func() bool { return false })

	resp, body := env.doPost(t, "/v1/chat/completions", `{"model":"m1","messages":[]}`, nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("开关关闭时 /v1 必须 503，实际 %d body=%s", resp.StatusCode, string(body))
	}
	var payload struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("错误体不是 JSON: %v body=%s", err, string(body))
	}
	if payload.Error.Code != CodeServiceUnavailable {
		t.Fatalf("错误码应为 %q，实际 %q", CodeServiceUnavailable, payload.Error.Code)
	}
	if payload.Error.Message == "" {
		t.Fatalf("503 必须带可读原因，body=%s", string(body))
	}
	if got := env.captured.snapshot().count; got != 0 {
		t.Fatalf("开关关闭时不许打上游，实际请求了 %d 次", got)
	}
	env.mu.Lock()
	results := len(env.results)
	env.mu.Unlock()
	if results != 0 {
		t.Fatalf("开关关闭时不许产生转发/计费事实，实际 %d 条", results)
	}
}

// TestMount_GateOpenAndNilBothPass 开关打开 / 未注入（nil）时必须照常转发。
func TestMount_GateOpenAndNilBothPass(t *testing.T) {
	cases := []struct {
		name string
		gate func() bool
	}{
		{"gate-nil", nil},
		{"gate-true", func() bool { return true }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			env := newTestEnv(t, baseTestConfig(), map[string]*model.ExternalModel{"m1": gptModel("m1", "upstream/m1")}, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"x","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
			})
			if c.gate != nil {
				env.handler.SetGate(c.gate)
			}
			resp, body := env.doPost(t, "/v1/chat/completions", `{"model":"m1","messages":[]}`, nil)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("开关打开时必须正常转发，实际 %d body=%s", resp.StatusCode, string(body))
			}
			if got := env.captured.snapshot().count; got != 1 {
				t.Fatalf("应打上游 1 次，实际 %d", got)
			}
		})
	}
}

// TestMount_GateIsReadPerRequest 开关在请求时读取（不是 Mount 时快照）：改开关立即生效。
func TestMount_GateIsReadPerRequest(t *testing.T) {
	env := newTestEnv(t, baseTestConfig(), map[string]*model.ExternalModel{"m1": gptModel("m1", "upstream/m1")}, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	})
	closed := false
	env.handler.SetGate(func() bool { return !closed })

	resp, body := env.doPost(t, "/v1/chat/completions", `{"model":"m1","messages":[]}`, nil)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("第一次应放行，实际 %d body=%s", resp.StatusCode, string(body))
	}

	closed = true
	resp2, body2 := env.doPost(t, "/v1/chat/completions", `{"model":"m1","messages":[]}`, nil)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("关掉开关后必须立即 503（不许等重启），实际 %d body=%s", resp2.StatusCode, string(body2))
	}
}
