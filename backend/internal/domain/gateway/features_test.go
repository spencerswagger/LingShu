package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// requestBody 读取构建出的上游请求体字节。
func requestBody(t *testing.T, req *http.Request) []byte {
	t.Helper()
	b, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatalf("read request body: %v", err)
	}
	return b
}

// Step 6 verify：thinking-roundtrip 请求侧改写（enabled 开关 / 回传 reasoning_content / onlyThinking）。
func TestThinkingRoundtrip(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"},` +
		`{"role":"assistant","content":"ok","reasoning":"chain"}]}`)

	// 缺省 enabled=false：不做任何改写（零差异）。
	f := newThinkingRoundtripFeature(map[string]any{})
	req := &UpstreamRequest{Body: append([]byte(nil), body...)}
	if err := f.RewriteRequest(req); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !bytes.Equal(req.Body, body) {
		t.Errorf("未启用时不应改写请求体，got %s", req.Body)
	}

	// enabled=true：把 reasoning 别名回传为上游要求的 reasoning_content。
	f = newThinkingRoundtripFeature(map[string]any{"enabled": true})
	req = &UpstreamRequest{Body: append([]byte(nil), body...)}
	if err := f.RewriteRequest(req); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var got struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(req.Body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Messages[1]["reasoning_content"] != "chain" {
		t.Errorf("assistant 消息应回传 reasoning_content=chain，got %v", got.Messages[1]["reasoning_content"])
	}

	// onlyThinking=true 且请求非 thinking：不改写。
	f = newThinkingRoundtripFeature(map[string]any{"enabled": true, "onlyThinking": true})
	req = &UpstreamRequest{Body: append([]byte(nil), body...)}
	if err := f.RewriteRequest(req); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !bytes.Equal(req.Body, body) {
		t.Errorf("非 thinking 模式且 onlyThinking=true 时不应改写，got %s", req.Body)
	}

	// field=reasoning 时：源字段只能取别名集合中不同于 field 的那个，不得把目标字段自身当源。
	f = newThinkingRoundtripFeature(map[string]any{"enabled": true, "field": "reasoning"})
	req = &UpstreamRequest{Body: append([]byte(nil), body...)}
	if err := f.RewriteRequest(req); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var gotField struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(req.Body, &gotField); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// 该消息本来只有 reasoning=chain：目标是 reasoning 本身，应无源可搬 → 不改写（键不存在）。
	if _, exists := gotField.Messages[1]["reasoning_content"]; exists {
		t.Errorf("目标字段为 reasoning 时不应再从自身取源，got %v", gotField.Messages[1])
	}

	// field=reasoning 且消息带 reasoning_content：应搬为 reasoning。
	aliasSrc := []byte(`{"model":"m","messages":[{"role":"assistant","reasoning_content":"chain"}]}`)
	req = &UpstreamRequest{Body: append([]byte(nil), aliasSrc...)}
	if err := f.RewriteRequest(req); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var gotAlias struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(req.Body, &gotAlias); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if gotAlias.Messages[0]["reasoning"] != "chain" {
		t.Errorf("应把别名 reasoning_content 搬为 reasoning，got %v", gotAlias.Messages[0])
	}
}

// TestReasoningSourceSkipsTargetField 单测 reasoningSource 的「跳过目标字段自身」规则：
// field 为别名集合成员时，只能从其余别名取源；无可取时返回 false（不凭空构造）。
func TestReasoningSourceSkipsTargetField(t *testing.T) {
	cases := []struct {
		name    string
		msg     string
		field   string
		want    string
		wantHit bool
	}{
		{name: "target_is_reasoning_takes_canonical", msg: `{"reasoning_content":"a","reasoning":"b"}`, field: "reasoning", want: "a", wantHit: true},
		{name: "target_is_canonical_takes_alias", msg: `{"reasoning":"b"}`, field: "reasoning_content", want: "b", wantHit: true},
		{name: "target_is_reasoning_only_self_present", msg: `{"reasoning":"b"}`, field: "reasoning", wantHit: false},
		// 目标字段自身即使有值也不能作为源（该值已在目标键上，无需搬运）。
		{name: "empty_canonical_does_not_block_alias", msg: `{"reasoning_content":"","reasoning":"b"}`, field: "reasoning_content", want: "b", wantHit: true},
		{name: "null_not_a_source", msg: `{"reasoning_content":null}`, field: "reasoning", wantHit: false},
		{name: "no_alias_absent", msg: `{"content":"x"}`, field: "reasoning", wantHit: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var m map[string]json.RawMessage
			if err := json.Unmarshal([]byte(tc.msg), &m); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			got, ok := reasoningSource(m, tc.field)
			if ok != tc.wantHit {
				t.Fatalf("命中=%v, want %v", ok, tc.wantHit)
			}
			if !ok {
				return
			}
			var s string
			if err := json.Unmarshal(got, &s); err != nil {
				t.Fatalf("源值应为字符串: %v", err)
			}
			if s != tc.want {
				t.Fatalf("源值=%q, want %q", s, tc.want)
			}
		})
	}
}

// Step 7 verify：tool-schema-normalize 为缺失/非法 schema 的工具补齐合法 schema。
func TestToolSchemaNormalize(t *testing.T) {
	body := []byte(`{"model":"m","tools":[` +
		`{"type":"web_search_20250305","name":"web_search"},` + // anthropic server 工具：缺 input_schema
		`{"type":"function","function":{"name":"f"}},` + // OpenAI 风格：缺 parameters
		`{"type":"function","function":{"name":"g","parameters":{"type":"object","properties":{"x":{"type":"string"}}}}},` + // 合法：不改
		`{"type":"web_search_20250305","name":"no_params","input_schema":{}}` + // 空对象：显式无入参，合法，不改
		`]}`)

	f := newToolSchemaNormalizeFeature(nil)
	req := &UpstreamRequest{Body: append([]byte(nil), body...)}
	if err := f.RewriteRequest(req); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	var got struct {
		Tools []map[string]json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(req.Body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Tools) != 4 {
		t.Fatalf("tools 数应为 4，got %d", len(got.Tools))
	}
	if _, ok := got.Tools[0]["input_schema"]; !ok {
		t.Errorf("web_search server 工具应补 input_schema，got %s", got.Tools[0])
	}
	var fn map[string]json.RawMessage
	if err := json.Unmarshal(got.Tools[1]["function"], &fn); err != nil {
		t.Fatalf("unmarshal function: %v", err)
	}
	if _, ok := fn["parameters"]; !ok {
		t.Errorf("缺 parameters 的 function 工具应补齐，got %s", got.Tools[1]["function"])
	}
	var fn2 map[string]json.RawMessage
	if err := json.Unmarshal(got.Tools[2]["function"], &fn2); err != nil {
		t.Fatalf("unmarshal function: %v", err)
	}
	if !bytes.Contains(fn2["parameters"], []byte(`"x"`)) {
		t.Errorf("合法 parameters 不应被覆盖，got %s", fn2["parameters"])
	}
	// 空对象 input_schema：视为合法，不得注入内置 web_search schema（否则会带 required query）。
	if s := bytes.TrimSpace(got.Tools[3]["input_schema"]); !bytes.Equal(s, []byte(`{}`)) {
		t.Errorf("空对象 input_schema 不应被改写，got %s", got.Tools[3]["input_schema"])
	}

	// Params.schema 覆盖默认 schema。
	override := map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}}
	f = newToolSchemaNormalizeFeature(map[string]any{"schema": override})
	req = &UpstreamRequest{Body: append([]byte(nil), []byte(`{"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)...)}
	if err := f.RewriteRequest(req); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	if !bytes.Contains(req.Body, []byte(`"q"`)) {
		t.Errorf("Params.schema 应覆盖默认 schema，got %s", req.Body)
	}
}

// Step 4 verify：未挂载特性时 buildUpstreamRequest 输出与改造前逐字节一致。
func TestFeatures_UnmountedUpstreamRequestByteIdentical(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"assistant","reasoning":"x"}]}`)
	base := func() UpstreamRequest {
		return UpstreamRequest{
			BaseURL:      "https://up.example.com",
			Body:         append([]byte(nil), body...),
			Protocol:     channelprotocol.ProtocolOpenAIChat,
			HeaderPolicy: channelprotocol.HeaderPolicy{},
		}
	}

	// 完全不挂载（Features 为零值）。
	r1, err := buildUpstreamRequest(context.Background(), base(), "chat/completions", "application/json")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := requestBody(t, r1); !bytes.Equal(got, body) {
		t.Errorf("未挂载特性时请求体应逐字节一致，got %s", got)
	}

	// 显式空切片（等价于未挂载，零候选特性）。
	req := base()
	req.Features = []channelprotocol.Feature{}
	r2, err := buildUpstreamRequest(context.Background(), req, "chat/completions", "application/json")
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if got := requestBody(t, r2); !bytes.Equal(got, body) {
		t.Errorf("空特性列表时请求体应逐字节一致，got %s", got)
	}
}

// stubResponseFeature 响应侧特性桩：验证扩展点的顺序与 nil 语义。
type stubResponseFeature struct {
	key string
	fn  func([]byte) ([]byte, error)
}

func (s stubResponseFeature) Key() string { return s.key }

func (s stubResponseFeature) NormalizeResponse(body []byte) ([]byte, error) { return s.fn(body) }

// Step 5 verify：响应侧特性按顺序归一；未挂载时原样返回。
func TestFeatures_NormalizeResponse(t *testing.T) {
	in := []byte(`{"a":1}`)
	feats := []ResponseFeature{
		stubResponseFeature{key: "one", fn: func(b []byte) ([]byte, error) { return append(b, []byte(" /*1*/")...), nil }},
		stubResponseFeature{key: "two", fn: func(b []byte) ([]byte, error) { return append(b, []byte(" /*2*/")...), nil }},
	}
	out, err := applyResponseFeatures(in, feats)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if want := []byte(`{"a":1} /*1*/ /*2*/`); !bytes.Equal(out, want) {
		t.Errorf("应按顺序依次归一，got %s want %s", out, want)
	}

	// 无特性：原样返回。
	out, err = applyResponseFeatures(in, nil)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if !bytes.Equal(out, in) {
		t.Errorf("无响应侧特性时应原样返回，got %s", out)
	}

	// normalizeUpstreamBody：渠道/协议为空 → 原样（默认安全态）。
	if got := normalizeUpstreamBody(in, nil); !bytes.Equal(got, in) {
		t.Errorf("无渠道协议时应原样返回，got %s", got)
	}
	rt := &channel.RuntimeChannel{Protocol: &channelprotocol.ChannelProtocol{}}
	if got := normalizeUpstreamBody(in, rt); !bytes.Equal(got, in) {
		t.Errorf("协议未挂载特性时应原样返回，got %s", got)
	}
}

// Step 8：注册表 / 校验 / 灰阶（仅挂载一个特性时另一个不生效）。
func TestFeatures_GrayScale(t *testing.T) {
	body := []byte(`{"messages":[{"role":"assistant","reasoning":"r"}],` +
		`"tools":[{"type":"web_search_20250305","name":"web_search"}]}`)

	// 仅挂载 thinking-roundtrip → tools 不应被补齐，reasoning 应回传。
	rtThinking := &channel.RuntimeChannel{Protocol: &channelprotocol.ChannelProtocol{
		Features: []channelprotocol.Feature{{Key: channelprotocol.FeatureThinkingRoundtrip, Params: map[string]any{"enabled": true}}},
	}}
	req := &UpstreamRequest{Body: append([]byte(nil), body...)}
	if err := applyRequestFeatures(req, requestFeaturesFor(protocolFeatures(rtThinking))); err != nil {
		t.Fatalf("apply: %v", err)
	}
	var got struct {
		Tools    []map[string]json.RawMessage `json:"tools"`
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(req.Body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got.Tools[0]["input_schema"]; ok {
		t.Errorf("灰阶：仅挂 thinking 时不应补 input_schema，got %s", req.Body)
	}
	if _, ok := got.Messages[0]["reasoning_content"]; !ok {
		t.Errorf("thinking-roundtrip 应回传 reasoning_content，got %s", req.Body)
	}

	// 仅挂载 tool-schema-normalize → reasoning 不应被改写，input_schema 应补齐。
	rtTools := &channel.RuntimeChannel{Protocol: &channelprotocol.ChannelProtocol{
		Features: []channelprotocol.Feature{{Key: channelprotocol.FeatureToolSchemaNormalize}},
	}}
	req = &UpstreamRequest{Body: append([]byte(nil), body...)}
	if err := applyRequestFeatures(req, requestFeaturesFor(protocolFeatures(rtTools))); err != nil {
		t.Fatalf("apply: %v", err)
	}
	got.Tools, got.Messages = nil, nil
	if err := json.Unmarshal(req.Body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, ok := got.Messages[0]["reasoning_content"]; ok {
		t.Errorf("灰阶：仅挂 tool-schema 时不应回传 reasoning_content，got %s", req.Body)
	}
	if _, ok := got.Tools[0]["input_schema"]; !ok {
		t.Errorf("tool-schema-normalize 应补 input_schema，got %s", req.Body)
	}

	// 合法特性 key 判定；未知 key 应被校验拒绝（requestFeaturesFor 亦对其降级忽略）。
	if !channelprotocol.IsValidFeatureKey(channelprotocol.FeatureThinkingRoundtrip) {
		t.Errorf("thinking-roundtrip 应为合法特性 key")
	}
	if !channelprotocol.IsValidFeatureKey(channelprotocol.FeatureToolSchemaNormalize) {
		t.Errorf("tool-schema-normalize 应为合法特性 key")
	}
	if channelprotocol.IsValidFeatureKey("unknown-feature") {
		t.Errorf("未知特性 key 应被判为非法（校验拒绝）")
	}
	unknown := requestFeaturesFor([]channelprotocol.Feature{{Key: "unknown-feature"}})
	if len(unknown) != 0 {
		t.Errorf("未知特性 key 不应解析出特性实例，got %d", len(unknown))
	}
}
