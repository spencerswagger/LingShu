package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestBuildReqParams_KeepsProtectedDropsRest 断言 8KB 预算下的裁剪策略：
//   - 受保护参数（temperature/max_tokens/top_p/response_format…）优先保留；
//   - 非受保护的大字段被裁掉并**记入 dropped**（不静默丢）；
//   - 整体序列化 ≤ 8KB，核心字段（端点/协议/上游 URL/token_id/model/stream）始终在。
func TestBuildReqParams_KeepsProtectedDropsRest(t *testing.T) {
	// 每块 1500B（低于单字段 2KB 上限，不会被换成省略标记），8 块共 12KB > 8KB 预算，
	// 因此必然触发「按优先级丢字段」的路径。
	block := strings.Repeat("x", 1500)
	var extra strings.Builder
	for i := 0; i < 8; i++ {
		extra.WriteString(`,"blob_`)
		extra.WriteString(string(rune('a' + i)))
		extra.WriteString(`":"` + block + `"`)
	}
	body := []byte(`{"model":"gpt-4","stream":true,"temperature":0.3,"max_tokens":1024,"top_p":0.9,
		"response_format":{"type":"json_object"},"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"get_weather"}}],
		"metadata":{"k":"` + block + `"},"logit_bias":{"1":1}` + extra.String() + `}`)

	src := newReqParamsSource(body)
	raw := src.buildReqParams(reqParamsInput{
		Endpoint:         "/v1/chat/completions",
		InboundProtocol:  "openai-chat",
		UpstreamProtocol: "openai-chat",
		UpstreamURL:      "https://up.example.com/v1/chat/completions?api-version=2024-10-21",
		TokenID:          42,
		Model:            "gpt-4",
		Stream:           true,
	})
	if raw == nil {
		t.Fatal("snapshot is nil")
	}
	if len(raw) > reqParamsMaxBytes {
		t.Fatalf("snapshot=%d bytes, want <= %d", len(raw), reqParamsMaxBytes)
	}
	var snap reqParamsSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	// 核心字段。
	if snap.Endpoint != "/v1/chat/completions" || snap.InboundProtocol != "openai-chat" ||
		snap.UpstreamProtocol != "openai-chat" || snap.TokenID != 42 || snap.Model != "gpt-4" || !snap.Stream {
		t.Fatalf("core fields wrong: %+v", snap)
	}
	// 上游 URL 必须已脱敏（去 query）。
	if snap.UpstreamURL != "https://up.example.com/v1/chat/completions" {
		t.Fatalf("upstream_url=%q, want query stripped", snap.UpstreamURL)
	}
	// 消息体不进 params，只留计数。
	if snap.MessageCount != 1 {
		t.Fatalf("message_count=%d, want 1", snap.MessageCount)
	}
	if _, ok := snap.Params["messages"]; ok {
		t.Fatal("messages must not be part of req_params")
	}
	// 受保护参数必须在。
	for _, k := range []string{"temperature", "max_tokens", "top_p", "response_format"} {
		if _, ok := snap.Params[k]; !ok {
			t.Fatalf("protected param %q missing: %v", k, snap.Params)
		}
	}
	// 大字段被裁且被标记（绝不静默丢）。
	if len(snap.Dropped) == 0 {
		t.Fatalf("want dropped fields recorded, got %+v", snap)
	}
	// tools 只留摘要。
	if snap.Tools == nil || snap.Tools.Count != 1 || len(snap.Tools.Names) != 1 || snap.Tools.Names[0] != "get_weather" {
		t.Fatalf("tools summary wrong: %+v", snap.Tools)
	}
	if _, ok := snap.Params["tools"]; ok {
		t.Fatal("raw tools must not be stored (summary only)")
	}
}

// TestBuildReqParams_FieldCapKeepsKey 断言单字段超 2KB 时「键保留、值换成省略标记」：
// 这样「至少含 response_format」的契约不会因为该字段本身很大而破坏。
func TestBuildReqParams_FieldCapKeepsKey(t *testing.T) {
	hugeSchema := strings.Repeat("s", 5000)
	body := []byte(`{"model":"gpt-4","messages":[],"response_format":{"json_schema":{"schema":"` + hugeSchema + `"}}}`)
	src := newReqParamsSource(body)
	raw := src.buildReqParams(reqParamsInput{Endpoint: "/v1/chat/completions", InboundProtocol: "openai-chat"})
	var snap reqParamsSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	v, ok := snap.Params["response_format"]
	if !ok {
		t.Fatalf("response_format key must be kept, got %v", snap.Params)
	}
	if !strings.Contains(string(v), "omitted:") {
		t.Fatalf("oversized value want omitted marker, got %s", v)
	}
	found := false
	for _, k := range snap.Summarized {
		if k == "response_format" {
			found = true
		}
	}
	if !found {
		t.Fatalf("summarized should list response_format, got %v", snap.Summarized)
	}
	if len(raw) > reqParamsMaxBytes {
		t.Fatalf("snapshot=%d bytes, want <= %d", len(raw), reqParamsMaxBytes)
	}
}

// TestBuildReqParams_NeverExceedsBudget 极端输入（多个受保护大字段）也不得超过 8KB：
// 兜底路径会清空 params 并标记 over_budget，而不是写一条超限记录。
func TestBuildReqParams_NeverExceedsBudget(t *testing.T) {
	chunk := strings.Repeat("y", 1900)
	body := []byte(`{"model":"m","messages":[],"temperature":1,"max_tokens":1,"top_p":1,
		"response_format":{"a":"` + chunk + `"},"tool_choice":{"b":"` + chunk + `"},
		"thinking":{"c":"` + chunk + `"},"stop":["` + chunk + `"]}`)
	src := newReqParamsSource(body)
	raw := src.buildReqParams(reqParamsInput{Endpoint: "/v1/messages", InboundProtocol: "anthropic-messages"})
	if len(raw) > reqParamsMaxBytes {
		t.Fatalf("snapshot=%d bytes, want <= %d", len(raw), reqParamsMaxBytes)
	}
	var snap reqParamsSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if snap.Endpoint != "/v1/messages" || snap.InboundProtocol != "anthropic-messages" {
		t.Fatalf("core fields must survive over-budget path: %+v", snap)
	}
}

// TestSanitizeUpstreamURL 断言上游 URL 脱敏：去 userinfo/query/fragment（query 可能带 api-key）。
func TestSanitizeUpstreamURL(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", ""},
		{"https://up.example.com/v1/chat/completions", "https://up.example.com/v1/chat/completions"},
		{"https://user:pass@up.example.com/v1/chat/completions?api-key=sk-1&api-version=2024", "https://up.example.com/v1/chat/completions"},
		{"https://up.example.com/x#frag", "https://up.example.com/x"},
		{"not a url", ""},
	}
	for _, c := range cases {
		if got := sanitizeUpstreamURL(c.in); got != c.want {
			t.Fatalf("sanitizeUpstreamURL(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

// TestWriteOpenAIError_CarriesRequestID 断言网关错误体顶层带 request_id，
// 取值来自响应头 x-request-id（与响应头同值，调用方可直接把 ID 报给平台）。
func TestWriteOpenAIError_CarriesRequestID(t *testing.T) {
	rec := httptest.NewRecorder()
	rec.Header().Set("x-request-id", "req-abc-123")
	writeOpenAIError(rec, http.StatusBadGateway, "上游服务暂时不可用")

	var body struct {
		Error struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.RequestID != "req-abc-123" {
		t.Fatalf("request_id=%q, want req-abc-123; body=%s", body.RequestID, rec.Body.String())
	}
	if body.Error.Code == "" {
		t.Fatalf("error.code must stay: %s", rec.Body.String())
	}
	if rec.Header().Get("x-request-id") != "req-abc-123" {
		t.Fatal("response header x-request-id must be preserved")
	}

	// 无 request id 时省略字段（不写空串），保持既有客户端解析不受影响。
	rec2 := httptest.NewRecorder()
	writeOpenAIError(rec2, http.StatusBadRequest, "bad")
	if strings.Contains(rec2.Body.String(), "request_id") {
		t.Fatalf("empty request id should be omitted, got %s", rec2.Body.String())
	}
}

// TestServe_WritesReqParams 端到端断言：真实转发路径写入的 call_logs.req_params 含
// 端点 / 实际上游协议 / 上游 URL / token_id / model / stream 与入站采样参数。
func TestServe_WritesReqParams(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	body := `{"model":"gpt-4","stream":false,"temperature":0.3,"max_tokens":256,"top_p":0.9,
		"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f1"}}]}`
	if rec := doRequestH(t, h.gw, "/v1/chat/completions", "sk-gw-t", body, nil); rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	if len(h.clogs.inserted) == 0 {
		t.Fatal("no call log inserted")
	}
	raw := h.clogs.inserted[len(h.clogs.inserted)-1].ReqParams
	if len(raw) == 0 {
		t.Fatal("req_params not written")
	}
	var snap reqParamsSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("unmarshal req_params: %v", err)
	}
	if snap.Endpoint != "/v1/chat/completions" || snap.InboundProtocol != "openai-chat" || snap.UpstreamProtocol != "openai-chat" {
		t.Fatalf("endpoint/protocol wrong: %+v", snap)
	}
	if snap.TokenID != 42 {
		t.Fatalf("token_id=%d, want 42", snap.TokenID)
	}
	if snap.Model != "gpt-4" || snap.Stream {
		t.Fatalf("model/stream wrong: %+v", snap)
	}
	if !strings.HasSuffix(snap.UpstreamURL, "/chat/completions") {
		t.Fatalf("upstream_url=%q, want .../chat/completions", snap.UpstreamURL)
	}
	for _, k := range []string{"temperature", "max_tokens", "top_p"} {
		if _, ok := snap.Params[k]; !ok {
			t.Fatalf("param %q missing: %v", k, snap.Params)
		}
	}
	if snap.Tools == nil || snap.Tools.Count != 1 || snap.Tools.Names[0] != "f1" {
		t.Fatalf("tools summary wrong: %+v", snap.Tools)
	}
}

// TestParseRequestMessages_FullAndResponsesShape 断言请求消息的取法：
//   - OpenAI/Anthropic 形状取 messages 全量（不抽样）；
//   - OpenAI Responses 形状取 input（条目数组 / 纯字符串），此前这类入站的调用日志是空的。
func TestParseRequestMessages_FullAndResponsesShape(t *testing.T) {
	// 1) messages 全量（含 system），首轮不再只留最后一条 user。
	msgs, ok := parseRequestMessages([]byte(`{"messages":[{"role":"system","content":"s"},{"role":"user","content":"a"},{"role":"user","content":"b"}]}`))
	if !ok || len(msgs) != 3 {
		t.Fatalf("messages: ok=%v len=%d, want 3", ok, len(msgs))
	}
	// 2) Responses 数组形状。
	msgs, ok = parseRequestMessages([]byte(`{"input":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]}`))
	if !ok || len(msgs) != 2 {
		t.Fatalf("responses array: ok=%v len=%d, want 2", ok, len(msgs))
	}
	// 3) Responses 字符串形状 → 归一成一条 user 消息。
	msgs, ok = parseRequestMessages([]byte(`{"input":"hello"}`))
	if !ok || len(msgs) != 1 || !strings.Contains(string(msgs[0]), `"role":"user"`) {
		t.Fatalf("responses string: ok=%v msgs=%v", ok, msgs)
	}
	// 4) 无消息：返回 false（调用日志留空数组，而不是伪造内容）。
	if _, ok = parseRequestMessages([]byte(`{"model":"x"}`)); ok {
		t.Fatal("no messages should return false")
	}
	// 5) 全量快照是拷贝（修改返回值不影响入参）。
	src := []json.RawMessage{json.RawMessage(`{"role":"user","content":"a"}`)}
	out := fullReqSnapshot(src)
	if len(out) != 1 {
		t.Fatalf("fullReqSnapshot len=%d, want 1", len(out))
	}
	out[0] = json.RawMessage(`{"role":"user","content":"changed"}`)
	if strings.Contains(string(src[0]), "changed") {
		t.Fatal("fullReqSnapshot must copy, not alias")
	}
}
