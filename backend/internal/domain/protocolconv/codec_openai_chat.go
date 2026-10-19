package protocolconv

import (
	"bytes"
	"encoding/json"

	"github.com/team/llmgateway/internal/domain/billing"
)

type openAIChatCodec struct{}

var (
	_ RequestCodec  = openAIChatCodec{}
	_ ResponseCodec = openAIChatCodec{}
	_ StreamCodec   = openAIChatCodec{}
)

const (
	oaiRoleSystem    = "system"
	oaiRoleUser      = "user"
	oaiRoleAssistant = "assistant"
	oaiRoleTool      = "tool"
)

type oaiChatMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []oaiToolCall   `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}

type oaiToolCall struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

type oaiContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL *struct {
		URL string `json:"url"`
	} `json:"image_url"`
}

func isJSONNull(raw json.RawMessage) bool {
	t := bytes.TrimSpace(raw)
	return len(t) == 0 || string(t) == "null"
}

func canonicalRaw(raw json.RawMessage) json.RawMessage {
	if len(raw) == 0 {
		return raw
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return raw
	}
	return json.RawMessage(buf.Bytes())
}

func oaiKnownRequestField(k string) bool {
	switch k {
	case "model", "messages", "tools", "tool_choice",
		"temperature", "top_p", "max_tokens", "stop", "stream", "metadata":
		return true
	}
	return false
}

func oaiKnownResponseField(k string) bool {
	switch k {
	case "id", "model", "choices", "usage", "object", "created", "system_fingerprint":
		return true
	}
	return false
}

func (openAIChatCodec) ParseRequest(body []byte) (*CanonicalRequest, *LossyReport, error) {
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil {
		return nil, nil, err
	}

	rep := &LossyReport{}
	req := &CanonicalRequest{}

	if raw, ok := top["model"]; ok && !isJSONNull(raw) {
		if err := json.Unmarshal(raw, &req.Model); err != nil {
			return nil, rep, err
		}
	}

	if raw, ok := top["messages"]; ok && !isJSONNull(raw) {
		if err := parseOAIMessages(raw, req, rep); err != nil {
			return nil, rep, err
		}
	}

	if raw, ok := top["tools"]; ok && !isJSONNull(raw) {
		if err := parseOAITools(raw, req, rep); err != nil {
			return nil, rep, err
		}
	}

	if raw, ok := top["tool_choice"]; ok && !isJSONNull(raw) {
		parseOAIToolChoice(raw, req)
	}

	if raw, ok := top["temperature"]; ok && !isJSONNull(raw) {
		if v, err := rawFloat64(raw); err == nil {
			req.Temperature = &v
		}
	}

	if raw, ok := top["top_p"]; ok && !isJSONNull(raw) {
		if v, err := rawFloat64(raw); err == nil {
			req.TopP = &v
		}
	}

	if raw, ok := top["max_tokens"]; ok && !isJSONNull(raw) {
		if v, ok := rawInt64(raw); ok {
			req.MaxTokens = &v
		}
	}

	if raw, ok := top["stop"]; ok && !isJSONNull(raw) {
		req.Stop = parseOAIStop(raw)
	}

	if raw, ok := top["stream"]; ok && !isJSONNull(raw) {
		_ = json.Unmarshal(raw, &req.Stream)
	}

	if raw, ok := top["metadata"]; ok && !isJSONNull(raw) {
		var md map[string]string
		if err := json.Unmarshal(raw, &md); err == nil {
			req.Metadata = md
		} else {
			if req.Extra == nil {
				req.Extra = map[string]json.RawMessage{}
			}
			req.Extra["metadata"] = canonicalRaw(raw)
		}
	}

	for k, v := range top {
		if oaiKnownRequestField(k) {
			continue
		}
		if req.Extra == nil {
			req.Extra = map[string]json.RawMessage{}
		}
		req.Extra[k] = canonicalRaw(v)
	}

	return req, rep, nil
}

func (openAIChatCodec) SerializeRequest(req *CanonicalRequest) ([]byte, *LossyReport, error) {
	rep := &LossyReport{}
	if req == nil {
		return nil, rep, nil
	}

	out := map[string]interface{}{}
	if req.Model != "" {
		out["model"] = req.Model
	}

	msgs := make([]interface{}, 0, len(req.System)+len(req.Messages))
	for _, b := range req.System {
		if b.Kind == BlockText {
			msgs = append(msgs, map[string]interface{}{"role": oaiRoleSystem, "content": b.Text})
		} else {
			rep.Add("system." + b.Kind)
		}
	}
	for _, m := range req.Messages {
		msgs = append(msgs, serializeOAIMessage(m, rep))
	}
	out["messages"] = msgs

	if len(req.Tools) > 0 {
		tools := make([]interface{}, 0, len(req.Tools))
		for _, t := range req.Tools {
			fn := map[string]interface{}{"name": t.Name}
			if t.Description != "" {
				fn["description"] = t.Description
			}
			if len(t.InputSchema) > 0 {
				fn["parameters"] = t.InputSchema
			}
			tools = append(tools, map[string]interface{}{"type": "function", "function": fn})
		}
		out["tools"] = tools
	}

	if req.ToolChoice != nil {
		switch req.ToolChoice.Mode {
		case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
			out["tool_choice"] = req.ToolChoice.Mode
		case ToolChoiceTool:
			out["tool_choice"] = map[string]interface{}{
				"type":     "function",
				"function": map[string]interface{}{"name": req.ToolChoice.Name},
			}
		default:
			rep.Add("tool_choice.mode." + req.ToolChoice.Mode)
		}
	}

	if req.Temperature != nil {
		out["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		out["top_p"] = *req.TopP
	}
	if req.MaxTokens != nil {
		out["max_tokens"] = *req.MaxTokens
	}
	if len(req.Stop) == 1 {
		out["stop"] = req.Stop[0]
	} else if len(req.Stop) > 1 {
		out["stop"] = req.Stop
	}
	if req.Stream {
		out["stream"] = true
	}
	if len(req.Metadata) > 0 {
		out["metadata"] = req.Metadata
	}

	for k, v := range req.Extra {
		if _, exists := out[k]; exists {
			continue
		}
		out[k] = v
	}

	body, err := marshalJSONNoEscape(out)
	if err != nil {
		return nil, rep, err
	}
	return body, rep, nil
}

func parseOAIMessages(raw json.RawMessage, req *CanonicalRequest, rep *LossyReport) error {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return err
	}
	for _, item := range items {
		var m oaiChatMessage
		if err := json.Unmarshal(item, &m); err != nil {
			return err
		}
		switch m.Role {
		case oaiRoleSystem:
			blocks, err := parseOAIContent(m.Content, rep)
			if err != nil {
				return err
			}
			for _, b := range blocks {
				if b.Kind == BlockText {
					req.System = append(req.System, b)
				} else {
					rep.Add("system.content." + b.Kind)
				}
			}
		case oaiRoleUser, oaiRoleAssistant:
			blocks, err := parseOAIContent(m.Content, rep)
			if err != nil {
				return err
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, toolCallToBlock(tc, rep))
			}
			req.Messages = append(req.Messages, Message{Role: m.Role, Blocks: blocks})
		case oaiRoleTool:
			blocks, err := parseOAIContent(m.Content, rep)
			if err != nil {
				return err
			}
			req.Messages = append(req.Messages, Message{
				Role: oaiRoleTool,
				Blocks: []ContentBlock{{
					Kind:       BlockToolResult,
					ToolUseID:  m.ToolCallID,
					ToolResult: blocks,
				}},
			})
		default:
			rep.Add("message.role." + m.Role)
		}
	}
	return nil
}

// oaiImageBlock 解析 OpenAI image_url：data URL 拆为 base64 数据 + 媒体类型，否则保留为外链 URL。
func oaiImageBlock(url string) ContentBlock {
	if mt, data, ok := splitDataURL(url); ok {
		return ContentBlock{Kind: BlockImage, MediaType: mt, ImageB64: data}
	}
	return ContentBlock{Kind: BlockImage, ImageURL: url}
}

// oaiImageURL 由规范图片块生成 OpenAI image_url：base64 数据拼接为 data URL，否则用外链 URL。
func oaiImageURL(b ContentBlock) string {
	if b.ImageB64 != "" {
		return "data:" + b.MediaType + ";base64," + b.ImageB64
	}
	return b.ImageURL
}

func parseOAIContent(raw json.RawMessage, rep *LossyReport) ([]ContentBlock, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return nil, nil
	}
	switch t[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []ContentBlock{{Kind: BlockText, Text: s}}, nil
	case '[':
		var parts []json.RawMessage
		if err := json.Unmarshal(raw, &parts); err != nil {
			return nil, err
		}
		blocks := make([]ContentBlock, 0, len(parts))
		for _, p := range parts {
			var part oaiContentPart
			if err := json.Unmarshal(p, &part); err != nil {
				return nil, err
			}
			switch part.Type {
			case "text":
				blocks = append(blocks, ContentBlock{Kind: BlockText, Text: part.Text})
			case "image_url":
				url := ""
				if part.ImageURL != nil {
					url = part.ImageURL.URL
				}
				blocks = append(blocks, oaiImageBlock(url))
			default:
				rep.Add("content.part." + part.Type)
			}
		}
		return blocks, nil
	}
	return nil, nil
}

func parseOAITools(raw json.RawMessage, req *CanonicalRequest, rep *LossyReport) error {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return err
	}
	for _, item := range items {
		var t struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description"`
				Parameters  json.RawMessage `json:"parameters"`
			} `json:"function"`
		}
		if err := json.Unmarshal(item, &t); err != nil {
			return err
		}
		if t.Type != "" && t.Type != "function" {
			rep.Add("tools.type." + t.Type)
			continue
		}
		req.Tools = append(req.Tools, Tool{
			Name:        t.Function.Name,
			Description: t.Function.Description,
			InputSchema: canonicalRaw(t.Function.Parameters),
		})
	}
	return nil
}

func parseOAIToolChoice(raw json.RawMessage, req *CanonicalRequest) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return
	}
	switch t[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			req.ToolChoice = &ToolChoice{Mode: s}
		}
	case '{':
		var tc struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if err := json.Unmarshal(raw, &tc); err == nil {
			req.ToolChoice = &ToolChoice{Mode: ToolChoiceTool, Name: tc.Function.Name}
		}
	}
}

func toolCallToBlock(tc oaiToolCall, rep *LossyReport) ContentBlock {
	b := ContentBlock{
		Kind:      BlockToolUse,
		ToolUseID: tc.ID,
		ToolName:  tc.Function.Name,
	}
	args := tc.Function.Arguments
	if args == "" {
		return b
	}
	if json.Valid([]byte(args)) {
		b.ToolInput = canonicalRaw(json.RawMessage(args))
	} else {
		wrapped, err := json.Marshal(args)
		if err != nil {
			wrapped = []byte(`""`)
		}
		b.ToolInput = wrapped
		rep.Note("tool_call arguments not valid JSON; wrapped as JSON string")
	}
	return b
}

func serializeOAIMessage(m Message, rep *LossyReport) interface{} {
	if m.Role == oaiRoleTool {
		msg := map[string]interface{}{"role": oaiRoleTool}
		for _, b := range m.Blocks {
			if b.Kind == BlockToolResult {
				msg["tool_call_id"] = b.ToolUseID
				msg["content"] = blocksText(b.ToolResult)
				break
			}
		}
		return msg
	}

	msg := map[string]interface{}{"role": m.Role}

	allText := true
	hasToolUse := false
	for _, b := range m.Blocks {
		if b.Kind != BlockText {
			allText = false
		}
		if b.Kind == BlockToolUse {
			hasToolUse = true
		}
	}

	switch {
	case allText && len(m.Blocks) == 1:
		msg["content"] = m.Blocks[0].Text
	case allText && len(m.Blocks) > 1:
		parts := make([]interface{}, 0, len(m.Blocks))
		for _, b := range m.Blocks {
			parts = append(parts, map[string]interface{}{"type": "text", "text": b.Text})
		}
		msg["content"] = parts
	default:
		parts := make([]interface{}, 0, len(m.Blocks))
		include := false
		for _, b := range m.Blocks {
			switch b.Kind {
			case BlockText:
				parts = append(parts, map[string]interface{}{"type": "text", "text": b.Text})
				include = true
			case BlockImage:
				parts = append(parts, map[string]interface{}{
					"type":      "image_url",
					"image_url": map[string]interface{}{"url": oaiImageURL(b)},
				})
				include = true
			case BlockToolUse:
			default:
				rep.Add("message.content." + b.Kind)
			}
		}
		if include {
			msg["content"] = parts
		} else {
			msg["content"] = nil
		}
	}

	if hasToolUse {
		calls := make([]interface{}, 0)
		for _, b := range m.Blocks {
			if b.Kind != BlockToolUse {
				continue
			}
			calls = append(calls, map[string]interface{}{
				"id":   b.ToolUseID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      b.ToolName,
					"arguments": string(b.ToolInput),
				},
			})
		}
		msg["tool_calls"] = calls
	}

	return msg
}

func blocksText(blocks []ContentBlock) string {
	var sb bytes.Buffer
	for _, b := range blocks {
		if b.Kind == BlockText {
			sb.WriteString(b.Text)
		}
	}
	return sb.String()
}

func (openAIChatCodec) ParseResponse(body []byte) (*CanonicalResponse, *LossyReport, error) {
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil {
		return nil, nil, err
	}

	rep := &LossyReport{}
	resp := &CanonicalResponse{Role: oaiRoleAssistant}

	if raw, ok := top["id"]; ok && !isJSONNull(raw) {
		_ = json.Unmarshal(raw, &resp.ID)
	}
	if raw, ok := top["model"]; ok && !isJSONNull(raw) {
		_ = json.Unmarshal(raw, &resp.Model)
	}
	if raw, ok := top["choices"]; ok && !isJSONNull(raw) {
		var choices []json.RawMessage
		if err := json.Unmarshal(raw, &choices); err != nil {
			return nil, rep, err
		}
		if len(choices) > 0 {
			var ch struct {
				Message struct {
					Role      string          `json:"role"`
					Content   json.RawMessage `json:"content"`
					ToolCalls []oaiToolCall   `json:"tool_calls"`
				} `json:"message"`
				FinishReason string `json:"finish_reason"`
			}
			if err := json.Unmarshal(choices[0], &ch); err != nil {
				return nil, rep, err
			}
			if ch.Message.Role != "" {
				resp.Role = ch.Message.Role
			}
			blocks, err := parseOAIContent(ch.Message.Content, rep)
			if err != nil {
				return nil, rep, err
			}
			for _, tc := range ch.Message.ToolCalls {
				blocks = append(blocks, toolCallToBlock(tc, rep))
			}
			resp.Blocks = blocks
			resp.FinishReason = ch.FinishReason
		}
	}
	if raw, ok := top["usage"]; ok && !isJSONNull(raw) {
		resp.Usage = parseOAIUsage(raw)
	}

	for k, v := range top {
		if oaiKnownResponseField(k) {
			continue
		}
		if resp.Extra == nil {
			resp.Extra = map[string]json.RawMessage{}
		}
		resp.Extra[k] = canonicalRaw(v)
	}

	return resp, rep, nil
}

func (openAIChatCodec) SerializeResponse(resp *CanonicalResponse) ([]byte, *LossyReport, error) {
	rep := &LossyReport{}
	if resp == nil {
		return nil, rep, nil
	}

	role := resp.Role
	if role == "" {
		role = oaiRoleAssistant
	}

	msg := map[string]interface{}{"role": role}

	allText := true
	hasToolUse := false
	for _, b := range resp.Blocks {
		if b.Kind != BlockText {
			allText = false
		}
		if b.Kind == BlockToolUse {
			hasToolUse = true
		}
	}

	switch {
	case allText && len(resp.Blocks) == 1:
		msg["content"] = resp.Blocks[0].Text
	case allText && len(resp.Blocks) > 1:
		parts := make([]interface{}, 0, len(resp.Blocks))
		for _, b := range resp.Blocks {
			parts = append(parts, map[string]interface{}{"type": "text", "text": b.Text})
		}
		msg["content"] = parts
	default:
		parts := make([]interface{}, 0, len(resp.Blocks))
		include := false
		for _, b := range resp.Blocks {
			switch b.Kind {
			case BlockText:
				parts = append(parts, map[string]interface{}{"type": "text", "text": b.Text})
				include = true
			case BlockImage:
				parts = append(parts, map[string]interface{}{
					"type":      "image_url",
					"image_url": map[string]interface{}{"url": oaiImageURL(b)},
				})
				include = true
			case BlockToolUse:
			default:
				rep.Add("response.block." + b.Kind)
			}
		}
		if include {
			msg["content"] = parts
		} else {
			msg["content"] = nil
		}
	}

	if hasToolUse {
		calls := make([]interface{}, 0)
		for _, b := range resp.Blocks {
			if b.Kind != BlockToolUse {
				continue
			}
			calls = append(calls, map[string]interface{}{
				"id":   b.ToolUseID,
				"type": "function",
				"function": map[string]interface{}{
					"name":      b.ToolName,
					"arguments": string(b.ToolInput),
				},
			})
		}
		msg["tool_calls"] = calls
	}

	usage := map[string]interface{}{
		"prompt_tokens":     resp.Usage.Input + resp.Usage.CacheRead,
		"completion_tokens": resp.Usage.Output,
		"total_tokens":      resp.Usage.Input + resp.Usage.CacheRead + resp.Usage.Output,
	}
	if resp.Usage.CacheRead > 0 {
		usage["prompt_tokens_details"] = map[string]interface{}{
			"cached_tokens": resp.Usage.CacheRead,
		}
	}

	out := map[string]interface{}{
		"id":     resp.ID,
		"object": "chat.completion",
		"model":  resp.Model,
		"choices": []interface{}{
			map[string]interface{}{
				"index":         0,
				"message":       msg,
				"finish_reason": resp.FinishReason,
			},
		},
		"usage": usage,
	}

	for k, v := range resp.Extra {
		if _, exists := out[k]; exists {
			continue
		}
		out[k] = v
	}

	body, err := marshalJSONNoEscape(out)
	if err != nil {
		return nil, rep, err
	}
	return body, rep, nil
}

type oaiUsagePayload struct {
	PromptTokens         json.Number `json:"prompt_tokens"`
	CompletionTokens     json.Number `json:"completion_tokens"`
	PromptCacheHitTokens json.Number `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails  *struct {
		CachedTokens json.Number `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

func parseOAIUsage(raw json.RawMessage) billing.Usage {
	var u oaiUsagePayload
	if err := json.Unmarshal(raw, &u); err != nil {
		return billing.Usage{}
	}
	prompt := jsonNumberToInt64(u.PromptTokens)
	completion := jsonNumberToInt64(u.CompletionTokens)

	var cached int64
	if u.PromptTokensDetails != nil {
		cached = jsonNumberToInt64(u.PromptTokensDetails.CachedTokens)
	}
	if cached == 0 {
		cached = jsonNumberToInt64(u.PromptCacheHitTokens)
	}

	input := prompt - cached
	if input < 0 {
		input = 0
	}
	return billing.Usage{
		Input:     input,
		Output:    completion,
		CacheRead: cached,
	}
}

func jsonNumberToInt64(n json.Number) int64 {
	if n == "" {
		return 0
	}
	if v, err := n.Int64(); err == nil {
		return v
	}
	if f, err := n.Float64(); err == nil {
		return int64(f)
	}
	return 0
}

func rawFloat64(raw json.RawMessage) (float64, error) {
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return 0, err
	}
	return f, nil
}

func rawInt64(raw json.RawMessage) (int64, bool) {
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return jsonNumberToInt64(n), true
}

func parseOAIStop(raw json.RawMessage) []string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 {
		return nil
	}
	switch t[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return []string{s}
		}
	case '[':
		var a []string
		if err := json.Unmarshal(raw, &a); err == nil {
			return a
		}
	}
	return nil
}

// openAIToolCallIndex 将规范内容块下标映射为 OpenAI tool_call 下标（块 0 预留给文本）。
func openAIToolCallIndex(blockIndex int) int {
	if blockIndex > 0 {
		return blockIndex - 1
	}
	return 0
}

// oaiStreamToolCall 是流式分片中的 tool_calls 元素。
type oaiStreamToolCall struct {
	Index    int    `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}

// DecodeEvent 将单个 OpenAI Chat 流分片解析为 0..n 个规范事件。
//
// 说明：首个分片（delta 含 role）会产出 EventMessageStart；但纯文本块的
// content_block_start 与工具调用的开场帧需要跨分片状态，无法在此无状态合成，
// 交由网关 pump 在缺失时为目标为 anthropic 的下游补出 message_start / content_block_start。
func (openAIChatCodec) DecodeEvent(data []byte) ([]CanonicalEvent, *LossyReport, error) {
	rep := &LossyReport{}
	var top struct {
		Choices []struct {
			Delta struct {
				Role             string              `json:"role"`
				Content          string              `json:"content"`
				ReasoningContent string              `json:"reasoning_content"`
				ToolCalls        []oaiStreamToolCall `json:"tool_calls"`
			} `json:"delta"`
			FinishReason *string         `json:"finish_reason"`
			Usage        json.RawMessage `json:"usage"`
		} `json:"choices"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, rep, err
	}

	// usage 取值优先 choices[].usage，其次顶层 usage（对齐网关 parseStreamData 的优先级）。
	usageRaw := top.Usage
	for _, ch := range top.Choices {
		if len(ch.Usage) > 0 && !isJSONNull(ch.Usage) {
			usageRaw = ch.Usage
			break
		}
	}
	var events []CanonicalEvent
	hasUsage := len(usageRaw) > 0 && !isJSONNull(usageRaw)
	var usage billing.Usage
	if hasUsage {
		usage = parseOAIUsage(usageRaw)
	}
	usageAttached := false

	for _, ch := range top.Choices {
		if ch.Delta.Role != "" {
			events = append(events, CanonicalEvent{Type: EventMessageStart})
		}
		if ch.Delta.Content != "" {
			events = append(events, CanonicalEvent{
				Type:      EventContentBlockDelta,
				Index:     0,
				DeltaKind: DeltaText,
				Text:      ch.Delta.Content,
			})
		}
		if ch.Delta.ReasoningContent != "" {
			events = append(events, CanonicalEvent{
				Type:      EventContentBlockDelta,
				Index:     0,
				DeltaKind: DeltaThinking,
				Text:      ch.Delta.ReasoningContent,
			})
		}
		for _, tc := range ch.Delta.ToolCalls {
			idx := tc.Index + 1
			if tc.ID != "" || tc.Function.Name != "" {
				events = append(events, CanonicalEvent{
					Type:      EventContentBlockStart,
					Index:     idx,
					BlockKind: BlockToolUse,
					ToolUseID: tc.ID,
					ToolName:  tc.Function.Name,
				})
			}
			if tc.Function.Arguments != "" {
				events = append(events, CanonicalEvent{
					Type:      EventContentBlockDelta,
					Index:     idx,
					DeltaKind: DeltaToolJSON,
					Text:      tc.Function.Arguments,
				})
			}
		}
		if ch.FinishReason != nil {
			ev := CanonicalEvent{
				Type:       EventMessageDelta,
				StopReason: normalizeStopReason(*ch.FinishReason),
			}
			if hasUsage && !usageAttached {
				ev.Usage = usage
				ev.HasUsage = true
				usageAttached = true
			}
			events = append(events, ev)
		}
	}

	if hasUsage && !usageAttached {
		events = append(events, CanonicalEvent{Type: EventMessageDelta, Usage: usage, HasUsage: true})
	}

	return events, rep, nil
}

// EncodeEvent 将单个规范事件编码为 OpenAI Chat SSE 帧。
func (openAIChatCodec) EncodeEvent(ev CanonicalEvent) ([][]byte, *LossyReport, error) {
	rep := &LossyReport{}
	switch ev.Type {
	case EventMessageStart:
		return [][]byte{dataFrame(map[string]interface{}{
			"choices": []interface{}{
				map[string]interface{}{
					"index": 0,
					"delta": map[string]interface{}{"role": oaiRoleAssistant},
				},
			},
		})}, rep, nil
	case EventContentBlockStart:
		if ev.BlockKind != BlockToolUse {
			return nil, rep, nil
		}
		return [][]byte{dataFrame(map[string]interface{}{
			"choices": []interface{}{
				map[string]interface{}{
					"index": 0,
					"delta": map[string]interface{}{
						"tool_calls": []interface{}{
							map[string]interface{}{
								"index":    openAIToolCallIndex(ev.Index),
								"id":       ev.ToolUseID,
								"type":     "function",
								"function": map[string]interface{}{"name": ev.ToolName, "arguments": ""},
							},
						},
					},
				},
			},
		})}, rep, nil
	case EventContentBlockDelta:
		var delta map[string]interface{}
		switch ev.DeltaKind {
		case DeltaText:
			delta = map[string]interface{}{"content": ev.Text}
		case DeltaThinking:
			delta = map[string]interface{}{"reasoning_content": ev.Text}
		case DeltaToolJSON:
			delta = map[string]interface{}{
				"tool_calls": []interface{}{
					map[string]interface{}{
						"index":    openAIToolCallIndex(ev.Index),
						"function": map[string]interface{}{"arguments": ev.Text},
					},
				},
			}
		default:
			return nil, rep, nil
		}
		return [][]byte{dataFrame(map[string]interface{}{
			"choices": []interface{}{
				map[string]interface{}{"index": 0, "delta": delta},
			},
		})}, rep, nil
	case EventContentBlockStop:
		return nil, rep, nil
	case EventMessageDelta:
		if ev.StopReason == "" && !ev.HasUsage {
			return nil, rep, nil
		}
		choice := map[string]interface{}{
			"index": 0,
			"delta": map[string]interface{}{},
		}
		if ev.StopReason != "" {
			choice["finish_reason"] = openAIStopReason(ev.StopReason)
		}
		payload := map[string]interface{}{
			"choices": []interface{}{choice},
		}
		if ev.HasUsage {
			payload["usage"] = usageToOpenAI(ev.Usage)
		}
		return [][]byte{dataFrame(payload)}, rep, nil
	case EventMessageStop:
		return [][]byte{[]byte("data: [DONE]\n\n")}, rep, nil
	}
	return nil, rep, nil
}
