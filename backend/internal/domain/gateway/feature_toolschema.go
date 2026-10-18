package gateway

import (
	"bytes"
	"encoding/json"

	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// toolSchemaNormalizeFeature 请求侧特性 tool-schema-normalize：
// 扫描请求 `tools`，对缺失/非法 schema 的工具补齐一份合法的最小 schema——修复
// 上游因 web_search 类工具（如 anthropic server 工具 web_search_20250305）缺少
// input_schema 而返回 422 的问题。
//
// 兼容两种工具形态：
//   - OpenAI chat 风格：`{"type":"function","function":{"name":…}}` → 补齐 function.parameters；
//   - Anthropic 风格 / server 工具：补齐顶层 input_schema。
//
// 参数（Params）：
//   - schema：自定义补齐用的 schema（object）；缺省用内置最小 web_search schema。
type toolSchemaNormalizeFeature struct {
	schema map[string]any
}

// defaultToolSchema 内置的最小 web_search 工具入参 schema（可被 Params.schema 覆盖）。
var defaultToolSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"query": map[string]any{
			"type":        "string",
			"description": "Search query",
		},
	},
	"required": []any{"query"},
}

// newToolSchemaNormalizeFeature 依据挂载参数构造 tool-schema-normalize 特性。
func newToolSchemaNormalizeFeature(params map[string]any) RequestFeature {
	f := &toolSchemaNormalizeFeature{schema: defaultToolSchema}
	if params != nil {
		if s, ok := params["schema"].(map[string]any); ok && len(s) > 0 {
			f.schema = s
		}
	}
	return f
}

// Key 返回特性标识。
func (f *toolSchemaNormalizeFeature) Key() string { return channelprotocol.FeatureToolSchemaNormalize }

// RewriteRequest 为缺失/非法 schema 的工具补齐合法 schema；无可补/无 tools 时原样返回（零差异）。
func (f *toolSchemaNormalizeFeature) RewriteRequest(req *UpstreamRequest) error {
	if req == nil || len(req.Body) == 0 {
		return nil
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(req.Body, &body); err != nil {
		return nil // 非 JSON 对象：交由既有流程兜底
	}
	rawTools, ok := body["tools"]
	if !ok || len(bytes.TrimSpace(rawTools)) == 0 || string(bytes.TrimSpace(rawTools)) == "null" {
		return nil
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(rawTools, &tools); err != nil {
		return nil
	}
	schemaJSON, err := json.Marshal(f.schema)
	if err != nil {
		return err
	}
	changed := false
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		// OpenAI chat 风格：补齐 function.parameters。
		if rawFn, ok := tool["function"]; ok {
			var fn map[string]json.RawMessage
			if json.Unmarshal(rawFn, &fn) != nil || fn == nil {
				continue
			}
			if schemaValid(fn["parameters"]) {
				continue
			}
			fn["parameters"] = schemaJSON
			nb, merr := json.Marshal(fn)
			if merr != nil {
				return merr
			}
			tool["function"] = nb
			changed = true
			continue
		}
		// Anthropic 风格 / server 工具：补齐顶层 input_schema。
		if !schemaValid(tool["input_schema"]) {
			tool["input_schema"] = schemaJSON
			changed = true
		}
	}
	if !changed {
		return nil
	}
	newTools, err := json.Marshal(tools)
	if err != nil {
		return err
	}
	body["tools"] = newTools
	newBody, err := json.Marshal(body)
	if err != nil {
		return err
	}
	req.Body = newBody
	return nil
}

// schemaValid 判断工具 schema 是否为合法 JSON 对象（缺失/null/非对象视为非法）。
// 空对象 `{}` 视为**合法**：工具显式声明无入参时不得被注入内置 schema（否则会带上 required query）。
func schemaValid(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || bytes.Equal(t, []byte("null")) {
		return false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(t, &obj) != nil {
		return false
	}
	return true
}
