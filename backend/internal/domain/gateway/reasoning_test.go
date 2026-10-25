package gateway

import (
	"encoding/json"
	"testing"
	"time"
)

// reasoningAt 取请求体第 idx 条消息的指定推理字段值（消息或字段不存在返回空串）。
func reasoningAt(t *testing.T, body []byte, idx int, field string) string {
	t.Helper()
	var obj struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if idx >= len(obj.Messages) {
		t.Fatalf("messages len=%d, want idx=%d", len(obj.Messages), idx)
	}
	return reasoningText(obj.Messages[idx][field])
}

// reasoningContentOf 是默认字段 reasoning_content 的取值简写。
func reasoningContentOf(t *testing.T, body []byte, idx int) string {
	t.Helper()
	return reasoningAt(t, body, idx, "reasoning_content")
}

func TestReasoningCacheBackfillsMissingReasoning(t *testing.T) {
	c := NewReasoningCache()
	c.RememberMessage("s1", []byte(`{"role":"assistant","content":"","reasoning_content":"思考过程","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}`))

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`)
	out := c.Backfill("s1", "reasoning_content", body)

	if got := reasoningContentOf(t, out, 1); got != "思考过程" {
		t.Fatalf("reasoning_content = %q, want 思考过程", got)
	}
	// 非 assistant 消息不得被写入推理字段。
	if got := reasoningContentOf(t, out, 0); got != "" {
		t.Fatalf("user reasoning_content = %q, want empty", got)
	}
}

func TestReasoningCacheKeepsExistingReasoning(t *testing.T) {
	c := NewReasoningCache()
	c.RememberMessage("s1", []byte(`{"role":"assistant","reasoning_content":"缓存内容","tool_calls":[{"id":"call_1"}]}`))

	body := []byte(`{"messages":[{"role":"assistant","reasoning_content":"客户端内容","tool_calls":[{"id":"call_1"}]}]}`)
	out := c.Backfill("s1", "reasoning_content", body)
	if got := reasoningContentOf(t, out, 0); got != "客户端内容" {
		t.Fatalf("reasoning_content = %q, want 客户端内容（已有内容不得改写）", got)
	}
}

// TestReasoningCacheDeclaredFieldOnBackfill 覆盖两个命名空间的切分：
// 写入固定读响应侧字段 reasoning_content（网关捕获路径的归一字段），
// 回填则写渠道声明的请求侧字段名（此处为 reasoning）。
func TestReasoningCacheDeclaredFieldOnBackfill(t *testing.T) {
	c := NewReasoningCache()
	// 真实响应形态：上游/网关拼装的 assistant 消息只用 reasoning_content。
	c.RememberMessage("s1", []byte(`{"role":"assistant","reasoning_content":"声明字段推理","tool_calls":[{"id":"call_1"}]}`))

	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1"}]}]}`)
	out := c.Backfill("s1", "reasoning", body)
	if got := reasoningAt(t, out, 0, "reasoning"); got != "声明字段推理" {
		t.Fatalf("reasoning = %q, want 声明字段推理", got)
	}
	if got := reasoningAt(t, out, 0, "reasoning_content"); got != "" {
		t.Fatalf("不应写入未声明的字段，got %q", got)
	}
}

func TestReasoningCacheSessionIsolation(t *testing.T) {
	c := NewReasoningCache()
	c.RememberMessage("s1", []byte(`{"role":"assistant","reasoning_content":"s1 推理","tool_calls":[{"id":"call_1"}]}`))

	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1"}]}]}`)
	if out := c.Backfill("s2", "reasoning_content", body); string(out) != string(body) {
		t.Fatalf("跨会话不应命中缓存，got %s", out)
	}
}

func TestReasoningCacheTTLExpiry(t *testing.T) {
	base := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	c := NewReasoningCache()
	c.now = func() time.Time { return base }
	c.RememberMessage("s1", []byte(`{"role":"assistant","reasoning_content":"会过期","tool_calls":[{"id":"call_1"}]}`))

	c.now = func() time.Time { return base.Add(c.ttl + time.Second) }
	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1"}]}]}`)
	if out := c.Backfill("s1", "reasoning_content", body); string(out) != string(body) {
		t.Fatalf("过期条目不应回填，got %s", out)
	}
}

func TestReasoningCacheRememberResponse(t *testing.T) {
	c := NewReasoningCache()
	c.RememberResponse("s1", []byte(`{"choices":[{"message":{"role":"assistant","reasoning_content":"非流式推理","tool_calls":[{"id":"call_9"}]}}]}`))

	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_9"}]}]}`)
	if got := reasoningContentOf(t, c.Backfill("s1", "reasoning_content", body), 0); got != "非流式推理" {
		t.Fatalf("reasoning_content = %q, want 非流式推理", got)
	}
}

// TestReasoningCachePartialHitAndNullToolCalls 覆盖边界：多 assistant 消息仅其一命中、
// tool_calls 为 null / 非数组时安全跳过（不 panic、不改写）。
func TestReasoningCachePartialHitAndNullToolCalls(t *testing.T) {
	c := NewReasoningCache()
	c.RememberMessage("s1", []byte(`{"role":"assistant","reasoning_content":"命中推理","tool_calls":[{"id":"call_hit"}]}`))

	body := []byte(`{"messages":[` +
		`{"role":"assistant","tool_calls":[{"id":"call_miss"}]},` +
		`{"role":"assistant","tool_calls":null},` +
		`{"role":"assistant","tool_calls":[{"id":"call_hit"}]}]}`)
	out := c.Backfill("s1", "reasoning_content", body)

	if got := reasoningContentOf(t, out, 0); got != "" {
		t.Fatalf("未命中的 assistant 不应被改写, got %q", got)
	}
	if got := reasoningContentOf(t, out, 1); got != "" {
		t.Fatalf("tool_calls=null 不应被改写, got %q", got)
	}
	if got := reasoningContentOf(t, out, 2); got != "命中推理" {
		t.Fatalf("命中的 assistant 应回填, got %q", got)
	}
}

// TestReasoningCacheRememberResponseAlias 覆盖非流式上游原始响应体使用 reasoning 别名的情形：
// 响应侧按别名集合容错读取，避免缓存静默 miss。
func TestReasoningCacheRememberResponseAlias(t *testing.T) {
	c := NewReasoningCache()
	c.RememberResponse("s1", []byte(`{"choices":[{"message":{"role":"assistant","reasoning":"别名推理","tool_calls":[{"id":"call_alias"}]}}]}`))

	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_alias"}]}]}`)
	if got := reasoningContentOf(t, c.Backfill("s1", "reasoning_content", body), 0); got != "别名推理" {
		t.Fatalf("reasoning_content = %q, want 别名推理（应按别名容错读取）", got)
	}
}

// TestReasoningCachePrefersCanonicalField 断言字段优先级：两个别名同时存在时取 reasoning_content。
func TestReasoningCachePrefersCanonicalField(t *testing.T) {
	c := NewReasoningCache()
	c.RememberMessage("s1", []byte(`{"role":"assistant","reasoning_content":"首选","reasoning":"次选","tool_calls":[{"id":"call_1"}]}`))

	body := []byte(`{"messages":[{"role":"assistant","tool_calls":[{"id":"call_1"}]}]}`)
	if got := reasoningContentOf(t, c.Backfill("s1", "reasoning_content", body), 0); got != "首选" {
		t.Fatalf("reasoning_content = %q, want 首选（优先 canonical 字段）", got)
	}
}

func TestReasoningCacheIgnoresMessageWithoutToolCalls(t *testing.T) {
	c := NewReasoningCache()
	c.RememberMessage("s1", []byte(`{"role":"assistant","content":"a","reasoning_content":"无工具调用"}`))
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n != 0 {
		t.Fatalf("entries = %d, want 0（无 tool_calls 不缓存）", n)
	}
}

func TestReasoningCacheCapacityLimit(t *testing.T) {
	c := NewReasoningCache()
	c.max = 2
	for _, id := range []string{"call_1", "call_2", "call_3"} {
		c.RememberMessage("s1", []byte(`{"role":"assistant","reasoning_content":"r-`+id+`","tool_calls":[{"id":"`+id+`"}]}`))
	}
	c.mu.Lock()
	n := len(c.entries)
	c.mu.Unlock()
	if n > 2 {
		t.Fatalf("entries = %d, want <= 2（容量上限）", n)
	}
}

func TestReasoningCacheBackfillNoMessagesUnchanged(t *testing.T) {
	c := NewReasoningCache()
	body := []byte(`{"model":"m","input":"hello"}`)
	if out := c.Backfill("s1", "reasoning_content", body); string(out) != string(body) {
		t.Fatalf("无 messages 时应原样返回，got %s", out)
	}
}

func TestNormalizeReasoningField(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", "reasoning_content"},
		{"   ", "reasoning_content"},
		{"reasoning", "reasoning"},
		{"  reasoning  ", "reasoning"}, // 参数带空白必须去除，否则会产出非法 JSON 键
	} {
		if got := normalizeReasoningField(tc.in); got != tc.want {
			t.Fatalf("normalizeReasoningField(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
