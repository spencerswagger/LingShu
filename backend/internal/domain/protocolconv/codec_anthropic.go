package protocolconv

import (
	"bytes"
	"encoding/json"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

type anthropicCodec struct{}

var (
	_ RequestCodec  = anthropicCodec{}
	_ ResponseCodec = anthropicCodec{}
	_ StreamCodec   = anthropicCodec{}
)

const (
	anthropicRoleUser      = "user"
	anthropicRoleAssistant = "assistant"
	anthropicRoleTool      = "tool"
)

// anthropicDefaultMaxTokens 是规范请求未显式给出 max_tokens 时注入的安全默认值（Anthropic 必填）。
const anthropicDefaultMaxTokens = 4096

func anthropicKnownRequestField(k string) bool {
	switch k {
	case "model", "system", "messages", "tools", "tool_choice",
		"max_tokens", "temperature", "top_p", "stop_sequences", "stream":
		return true
	}
	return false
}

func anthropicKnownResponseField(k string) bool {
	switch k {
	case "id", "type", "role", "model", "content", "stop_reason", "usage":
		return true
	}
	return false
}

// ParseRequest 将 Anthropic Messages 请求体解析为规范请求。
func (anthropicCodec) ParseRequest(body []byte) (*CanonicalRequest, *LossyReport, error) {
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

	if raw, ok := top["system"]; ok && !isJSONNull(raw) {
		blocks, err := parseAnthropicContent(raw, rep)
		if err != nil {
			return nil, rep, err
		}
		for _, b := range blocks {
			if b.Kind == BlockText {
				req.System = append(req.System, b)
			} else {
				rep.Add("system." + b.Kind)
			}
		}
	}

	if raw, ok := top["messages"]; ok && !isJSONNull(raw) {
		if err := parseAnthropicMessages(raw, req, rep); err != nil {
			return nil, rep, err
		}
	}

	if raw, ok := top["tools"]; ok && !isJSONNull(raw) {
		if err := parseAnthropicTools(raw, req); err != nil {
			return nil, rep, err
		}
	}

	if raw, ok := top["tool_choice"]; ok && !isJSONNull(raw) {
		parseAnthropicToolChoice(raw, req)
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

	if raw, ok := top["stop_sequences"]; ok && !isJSONNull(raw) {
		req.Stop = parseOAIStop(raw)
	}

	if raw, ok := top["stream"]; ok && !isJSONNull(raw) {
		_ = json.Unmarshal(raw, &req.Stream)
	}

	for k, v := range top {
		if anthropicKnownRequestField(k) {
			continue
		}
		if req.Extra == nil {
			req.Extra = map[string]json.RawMessage{}
		}
		req.Extra[k] = canonicalRaw(v)
	}

	return req, rep, nil
}

// SerializeRequest 将规范请求序列化为 Anthropic Messages 请求体。
func (anthropicCodec) SerializeRequest(req *CanonicalRequest) ([]byte, *LossyReport, error) {
	rep := &LossyReport{}
	if req == nil {
		return nil, rep, nil
	}

	out := map[string]interface{}{}
	if req.Model != "" {
		out["model"] = req.Model
	}

	if len(req.System) > 0 {
		if len(req.System) == 1 && req.System[0].Kind == BlockText {
			out["system"] = req.System[0].Text
		} else {
			parts := make([]interface{}, 0, len(req.System))
			for _, b := range req.System {
				if b.Kind == BlockText {
					parts = append(parts, map[string]interface{}{"type": BlockText, "text": b.Text})
				} else {
					rep.Add("system." + b.Kind)
				}
			}
			out["system"] = parts
		}
	}

	out["messages"] = serializeAnthropicMessages(req.Messages, rep)

	if len(req.Tools) > 0 {
		tools := make([]interface{}, 0, len(req.Tools))
		for _, t := range req.Tools {
			item := map[string]interface{}{"name": t.Name}
			if t.Description != "" {
				item["description"] = t.Description
			}
			if len(t.InputSchema) > 0 {
				item["input_schema"] = t.InputSchema
			}
			tools = append(tools, item)
		}
		out["tools"] = tools
	}

	if req.ToolChoice != nil {
		switch req.ToolChoice.Mode {
		case ToolChoiceAuto:
			out["tool_choice"] = map[string]interface{}{"type": "auto"}
		case ToolChoiceRequired:
			out["tool_choice"] = map[string]interface{}{"type": "any"}
		case ToolChoiceTool:
			out["tool_choice"] = map[string]interface{}{"type": "tool", "name": req.ToolChoice.Name}
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
	} else {
		out["max_tokens"] = anthropicDefaultMaxTokens
	}
	if len(req.Stop) > 0 {
		out["stop_sequences"] = req.Stop
	}
	if req.Stream {
		out["stream"] = true
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

// ParseResponse 将 Anthropic Messages 响应体解析为规范响应。
func (anthropicCodec) ParseResponse(body []byte) (*CanonicalResponse, *LossyReport, error) {
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil {
		return nil, nil, err
	}

	rep := &LossyReport{}
	resp := &CanonicalResponse{Role: anthropicRoleAssistant}

	if raw, ok := top["id"]; ok && !isJSONNull(raw) {
		_ = json.Unmarshal(raw, &resp.ID)
	}
	if raw, ok := top["model"]; ok && !isJSONNull(raw) {
		_ = json.Unmarshal(raw, &resp.Model)
	}
	if raw, ok := top["role"]; ok && !isJSONNull(raw) {
		var role string
		if err := json.Unmarshal(raw, &role); err == nil && role != "" {
			resp.Role = role
		}
	}
	if raw, ok := top["content"]; ok && !isJSONNull(raw) {
		blocks, err := parseAnthropicContent(raw, rep)
		if err != nil {
			return nil, rep, err
		}
		resp.Blocks = blocks
	}
	if raw, ok := top["stop_reason"]; ok && !isJSONNull(raw) {
		_ = json.Unmarshal(raw, &resp.FinishReason)
	}
	if raw, ok := top["usage"]; ok && !isJSONNull(raw) {
		resp.Usage = parseAnthropicUsage(raw)
		resp.CacheCreation = anthropicCacheCreation(raw)
	}

	for k, v := range top {
		if anthropicKnownResponseField(k) {
			continue
		}
		if resp.Extra == nil {
			resp.Extra = map[string]json.RawMessage{}
		}
		resp.Extra[k] = canonicalRaw(v)
	}

	return resp, rep, nil
}

// SerializeResponse 将规范响应序列化为 Anthropic Messages 响应体。
func (anthropicCodec) SerializeResponse(resp *CanonicalResponse) ([]byte, *LossyReport, error) {
	rep := &LossyReport{}
	if resp == nil {
		return nil, rep, nil
	}

	role := resp.Role
	if role == "" {
		role = anthropicRoleAssistant
	}

	content := make([]interface{}, 0, len(resp.Blocks))
	for _, b := range resp.Blocks {
		if v := serializeAnthropicBlock(b, rep); v != nil {
			content = append(content, v)
		}
	}

	// 仅当规范输入确含缓存写信息时才提示（否则为无意义噪声）。
	if resp.CacheCreation > 0 {
		rep.Note("anthropic usage: cache_creation_input_tokens not preserved (merged into input_tokens)")
	}

	out := map[string]interface{}{
		"id":      resp.ID,
		"type":    "message",
		"role":    role,
		"model":   resp.Model,
		"content": content,
		"usage": map[string]interface{}{
			"input_tokens":            resp.Usage.Input,
			"cache_read_input_tokens": resp.Usage.CacheRead,
			"output_tokens":           resp.Usage.Output,
		},
	}
	if resp.FinishReason != "" {
		out["stop_reason"] = resp.FinishReason
	}

	for k, v := range resp.Extra {
		if _, exists := out[k]; exists {
			continue
		}
		// Anthropic Message 响应不支持顶层 metadata：丢弃并留痕（避免下发非法字段）。
		if k == "metadata" {
			rep.Note("anthropic response: metadata not supported; dropped")
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

func parseAnthropicMessages(raw json.RawMessage, req *CanonicalRequest, rep *LossyReport) error {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return err
	}
	for _, item := range items {
		var m struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		}
		if err := json.Unmarshal(item, &m); err != nil {
			return err
		}
		blocks, err := parseAnthropicContent(m.Content, rep)
		if err != nil {
			return err
		}
		switch m.Role {
		case anthropicRoleUser:
			var rest, results []ContentBlock
			for _, b := range blocks {
				if b.Kind == BlockToolResult {
					results = append(results, b)
					continue
				}
				rest = append(rest, b)
			}
			if len(rest) > 0 || len(results) == 0 {
				req.Messages = append(req.Messages, Message{Role: anthropicRoleUser, Blocks: rest})
			}
			for _, b := range results {
				req.Messages = append(req.Messages, Message{
					Role:   anthropicRoleTool,
					Blocks: []ContentBlock{b},
				})
			}
		case anthropicRoleAssistant:
			req.Messages = append(req.Messages, Message{Role: anthropicRoleAssistant, Blocks: blocks})
		default:
			rep.Add("message.role." + m.Role)
		}
	}
	return nil
}

func parseAnthropicContent(raw json.RawMessage, rep *LossyReport) ([]ContentBlock, error) {
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
			b, ok, err := parseAnthropicBlock(p, rep)
			if err != nil {
				return nil, err
			}
			if ok {
				blocks = append(blocks, b)
			}
		}
		return blocks, nil
	}
	return nil, nil
}

func parseAnthropicBlock(raw json.RawMessage, rep *LossyReport) (ContentBlock, bool, error) {
	var b struct {
		Type         string          `json:"type"`
		Text         string          `json:"text"`
		CacheControl json.RawMessage `json:"cache_control"`
		Source       *struct {
			Type      string `json:"type"`
			MediaType string `json:"media_type"`
			Data      string `json:"data"`
			URL       string `json:"url"`
		} `json:"source"`
		ID        string          `json:"id"`
		Name      string          `json:"name"`
		Input     json.RawMessage `json:"input"`
		ToolUseID string          `json:"tool_use_id"`
		Content   json.RawMessage `json:"content"`
		IsError   bool            `json:"is_error"`
		Thinking  string          `json:"thinking"`
		Signature string          `json:"signature"`
	}
	if err := json.Unmarshal(raw, &b); err != nil {
		return ContentBlock{}, false, err
	}
	switch b.Type {
	case BlockText:
		if len(b.CacheControl) > 0 && !isJSONNull(b.CacheControl) {
			rep.Add("cache_control")
		}
		return ContentBlock{Kind: BlockText, Text: b.Text}, true, nil
	case BlockImage:
		blk := ContentBlock{Kind: BlockImage}
		if b.Source != nil {
			switch b.Source.Type {
			case "url":
				if mt, data, ok := splitDataURL(b.Source.URL); ok {
					blk.MediaType = mt
					blk.ImageB64 = data
				} else {
					blk.ImageURL = b.Source.URL
				}
			default:
				blk.MediaType = b.Source.MediaType
				blk.ImageB64 = b.Source.Data
			}
		}
		return blk, true, nil
	case BlockToolUse:
		return ContentBlock{
			Kind:      BlockToolUse,
			ToolUseID: b.ID,
			ToolName:  b.Name,
			ToolInput: canonicalRaw(b.Input),
		}, true, nil
	case BlockToolResult:
		nested, err := parseAnthropicContent(b.Content, rep)
		if err != nil {
			return ContentBlock{}, false, err
		}
		return ContentBlock{
			Kind:       BlockToolResult,
			ToolUseID:  b.ToolUseID,
			ToolResult: nested,
			IsError:    b.IsError,
		}, true, nil
	case BlockThinking:
		return ContentBlock{Kind: BlockThinking, Text: b.Thinking, Signature: b.Signature}, true, nil
	default:
		rep.Add("content.block." + b.Type)
		return ContentBlock{}, false, nil
	}
}

func parseAnthropicTools(raw json.RawMessage, req *CanonicalRequest) error {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return err
	}
	for _, item := range items {
		var t struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if err := json.Unmarshal(item, &t); err != nil {
			return err
		}
		req.Tools = append(req.Tools, Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: canonicalRaw(t.InputSchema),
		})
	}
	return nil
}

func parseAnthropicToolChoice(raw json.RawMessage, req *CanonicalRequest) {
	var tc struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &tc); err != nil {
		return
	}
	switch tc.Type {
	case "auto":
		req.ToolChoice = &ToolChoice{Mode: ToolChoiceAuto}
	case "any":
		req.ToolChoice = &ToolChoice{Mode: ToolChoiceRequired}
	case "tool":
		req.ToolChoice = &ToolChoice{Mode: ToolChoiceTool, Name: tc.Name}
	}
}

func serializeAnthropicMessages(msgs []Message, rep *LossyReport) []interface{} {
	type mergedMessage struct {
		role   string
		blocks []ContentBlock
	}

	merged := make([]mergedMessage, 0, len(msgs))
	for _, m := range msgs {
		role, blocks := m.Role, m.Blocks
		if role == anthropicRoleTool {
			role = anthropicRoleUser
			blocks = nil
			for _, b := range m.Blocks {
				if b.Kind == BlockToolResult {
					blocks = append(blocks, b)
				} else {
					rep.Add("message.block." + b.Kind)
				}
			}
		}
		if n := len(merged); n > 0 && merged[n-1].role == role {
			merged[n-1].blocks = append(merged[n-1].blocks, blocks...)
			continue
		}
		merged = append(merged, mergedMessage{role: role, blocks: blocks})
	}

	out := make([]interface{}, 0, len(merged))
	for _, m := range merged {
		out = append(out, serializeAnthropicMessage(Message{Role: m.role, Blocks: m.blocks}, rep))
	}
	return out
}

func serializeAnthropicMessage(m Message, rep *LossyReport) interface{} {
	msg := map[string]interface{}{"role": m.Role}
	if len(m.Blocks) == 1 && m.Blocks[0].Kind == BlockText {
		msg["content"] = m.Blocks[0].Text
		return msg
	}
	parts := make([]interface{}, 0, len(m.Blocks))
	for _, b := range m.Blocks {
		if v := serializeAnthropicBlock(b, rep); v != nil {
			parts = append(parts, v)
		}
	}
	msg["content"] = parts
	return msg
}

func serializeAnthropicBlock(b ContentBlock, rep *LossyReport) interface{} {
	switch b.Kind {
	case BlockText:
		return map[string]interface{}{"type": BlockText, "text": b.Text}
	case BlockImage:
		if b.ImageB64 != "" {
			return anthropicBase64Image(b.MediaType, b.ImageB64)
		}
		if mt, data, ok := splitDataURL(b.ImageURL); ok {
			return anthropicBase64Image(mt, data)
		}
		return map[string]interface{}{
			"type":   BlockImage,
			"source": map[string]interface{}{"type": "url", "url": b.ImageURL},
		}
	case BlockToolUse:
		input := b.ToolInput
		if len(input) == 0 {
			input = json.RawMessage("{}")
		}
		return map[string]interface{}{
			"type":  BlockToolUse,
			"id":    b.ToolUseID,
			"name":  b.ToolName,
			"input": input,
		}
	case BlockToolResult:
		return serializeAnthropicToolResult(b, rep)
	case BlockThinking:
		return map[string]interface{}{
			"type":      BlockThinking,
			"thinking":  b.Text,
			"signature": b.Signature,
		}
	default:
		rep.Add("content.block." + b.Kind)
		return nil
	}
}

// anthropicBase64Image 构造 Anthropic base64 图片块。
func anthropicBase64Image(mediaType, data string) map[string]interface{} {
	return map[string]interface{}{
		"type": BlockImage,
		"source": map[string]interface{}{
			"type":       "base64",
			"media_type": mediaType,
			"data":       data,
		},
	}
}

func serializeAnthropicToolResult(b ContentBlock, rep *LossyReport) interface{} {
	out := map[string]interface{}{
		"type":        BlockToolResult,
		"tool_use_id": b.ToolUseID,
	}
	if len(b.ToolResult) == 1 && b.ToolResult[0].Kind == BlockText {
		out["content"] = b.ToolResult[0].Text
	} else {
		parts := make([]interface{}, 0, len(b.ToolResult))
		for _, c := range b.ToolResult {
			if c.Kind == BlockText {
				parts = append(parts, map[string]interface{}{"type": BlockText, "text": c.Text})
			} else {
				rep.Add("tool_result.content." + c.Kind)
			}
		}
		out["content"] = parts
	}
	if b.IsError {
		out["is_error"] = true
	}
	return out
}

type anthropicUsagePayload struct {
	InputTokens              json.Number `json:"input_tokens"`
	OutputTokens             json.Number `json:"output_tokens"`
	CacheCreationInputTokens json.Number `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     json.Number `json:"cache_read_input_tokens"`
}

func parseAnthropicUsage(raw json.RawMessage) billing.Usage {
	var u anthropicUsagePayload
	if err := json.Unmarshal(raw, &u); err != nil {
		return billing.Usage{}
	}
	input := jsonNumberToInt64(u.InputTokens) + jsonNumberToInt64(u.CacheCreationInputTokens)
	return billing.Usage{
		Input:     input,
		Output:    jsonNumberToInt64(u.OutputTokens),
		CacheRead: jsonNumberToInt64(u.CacheReadInputTokens),
	}
}

// anthropicCacheCreation 读取 cache_creation_input_tokens（缺失/非法返回 0）。
func anthropicCacheCreation(raw json.RawMessage) int64 {
	var u anthropicUsagePayload
	if err := json.Unmarshal(raw, &u); err != nil {
		return 0
	}
	return jsonNumberToInt64(u.CacheCreationInputTokens)
}

// DecodeEvent 将单个 Anthropic 流事件解析为 0..n 个规范事件。
func (anthropicCodec) DecodeEvent(data []byte) ([]CanonicalEvent, *LossyReport, error) {
	rep := &LossyReport{}
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil {
		return nil, rep, err
	}

	var typ string
	if raw, ok := top["type"]; ok && !isJSONNull(raw) {
		_ = json.Unmarshal(raw, &typ)
	}

	switch typ {
	case "message_start":
		ev := CanonicalEvent{Type: EventMessageStart}
		if raw, ok := top["message"]; ok && !isJSONNull(raw) {
			var msg struct {
				Usage json.RawMessage `json:"usage"`
			}
			if err := json.Unmarshal(raw, &msg); err == nil && len(msg.Usage) > 0 && !isJSONNull(msg.Usage) {
				ev.Usage = parseAnthropicUsage(msg.Usage)
				ev.HasUsage = true
			}
		}
		return []CanonicalEvent{ev}, rep, nil
	case "content_block_start":
		ev := CanonicalEvent{Type: EventContentBlockStart, Index: rawIntValue(top["index"])}
		if raw, ok := top["content_block"]; ok && !isJSONNull(raw) {
			var cb struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			_ = json.Unmarshal(raw, &cb)
			ev.BlockKind = cb.Type
			ev.ToolUseID = cb.ID
			ev.ToolName = cb.Name
		}
		return []CanonicalEvent{ev}, rep, nil
	case "content_block_delta":
		ev := CanonicalEvent{Type: EventContentBlockDelta, Index: rawIntValue(top["index"])}
		if raw, ok := top["delta"]; ok && !isJSONNull(raw) {
			var d struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				Thinking    string `json:"thinking"`
				PartialJSON string `json:"partial_json"`
			}
			_ = json.Unmarshal(raw, &d)
			switch d.Type {
			case "text_delta":
				ev.DeltaKind = DeltaText
				ev.Text = d.Text
			case "thinking_delta":
				ev.DeltaKind = DeltaThinking
				ev.Text = d.Thinking
			case "input_json_delta":
				ev.DeltaKind = DeltaToolJSON
				ev.Text = d.PartialJSON
			default:
				ev.DeltaKind = d.Type
				ev.Text = d.Text
			}
		}
		if ev.DeltaKind == "" {
			return nil, rep, nil
		}
		return []CanonicalEvent{ev}, rep, nil
	case "content_block_stop":
		return []CanonicalEvent{{Type: EventContentBlockStop, Index: rawIntValue(top["index"])}}, rep, nil
	case "message_delta":
		ev := CanonicalEvent{Type: EventMessageDelta}
		if raw, ok := top["delta"]; ok && !isJSONNull(raw) {
			var d struct {
				StopReason string `json:"stop_reason"`
			}
			_ = json.Unmarshal(raw, &d)
			ev.StopReason = stopReasonToCanonical(channelprotocol.ProtocolAnthropicMessages, d.StopReason)
		}
		if raw, ok := top["usage"]; ok && !isJSONNull(raw) {
			ev.Usage = parseAnthropicUsage(raw)
			ev.HasUsage = true
		}
		return []CanonicalEvent{ev}, rep, nil
	case "message_stop":
		return []CanonicalEvent{{Type: EventMessageStop}}, rep, nil
	case "ping":
		return []CanonicalEvent{{Type: EventPing}}, rep, nil
	case "error":
		ev := CanonicalEvent{Type: EventError}
		if raw, ok := top["error"]; ok && !isJSONNull(raw) {
			var e struct {
				Message string `json:"message"`
			}
			_ = json.Unmarshal(raw, &e)
			ev.ErrMessage = e.Message
		}
		return []CanonicalEvent{ev}, rep, nil
	}
	return nil, rep, nil
}

// EncodeEvent 将单个规范事件编码为 Anthropic SSE 帧。
func (anthropicCodec) EncodeEvent(ev CanonicalEvent) ([][]byte, *LossyReport, error) {
	rep := &LossyReport{}
	switch ev.Type {
	case EventMessageStart:
		msg := map[string]interface{}{
			"id":      "",
			"type":    "message",
			"role":    anthropicRoleAssistant,
			"model":   "",
			"content": []interface{}{},
		}
		if ev.HasUsage {
			msg["usage"] = usageToAnthropic(ev.Usage)
		}
		return [][]byte{anthropicFrame("message_start", map[string]interface{}{
			"type":    "message_start",
			"message": msg,
		})}, rep, nil
	case EventContentBlockStart:
		cb := map[string]interface{}{"type": ev.BlockKind}
		if ev.BlockKind == BlockToolUse {
			cb["id"] = ev.ToolUseID
			cb["name"] = ev.ToolName
			cb["input"] = map[string]interface{}{}
		}
		return [][]byte{anthropicFrame("content_block_start", map[string]interface{}{
			"type":          "content_block_start",
			"index":         ev.Index,
			"content_block": cb,
		})}, rep, nil
	case EventContentBlockDelta:
		delta := map[string]interface{}{}
		switch ev.DeltaKind {
		case DeltaText:
			delta["type"] = "text_delta"
			delta["text"] = ev.Text
		case DeltaThinking:
			delta["type"] = "thinking_delta"
			delta["thinking"] = ev.Text
		case DeltaToolJSON:
			delta["type"] = "input_json_delta"
			delta["partial_json"] = ev.Text
		default:
			return nil, rep, nil
		}
		return [][]byte{anthropicFrame("content_block_delta", map[string]interface{}{
			"type":  "content_block_delta",
			"index": ev.Index,
			"delta": delta,
		})}, rep, nil
	case EventContentBlockStop:
		return [][]byte{anthropicFrame("content_block_stop", map[string]interface{}{
			"type":  "content_block_stop",
			"index": ev.Index,
		})}, rep, nil
	case EventMessageDelta:
		if ev.StopReason == "" && !ev.HasUsage {
			return nil, rep, nil
		}
		delta := map[string]interface{}{}
		if ev.StopReason != "" {
			delta["stop_reason"] = stopReasonFromCanonical(channelprotocol.ProtocolAnthropicMessages, ev.StopReason, rep)
		}
		payload := map[string]interface{}{
			"type":  "message_delta",
			"delta": delta,
		}
		if ev.HasUsage {
			payload["usage"] = usageToAnthropic(ev.Usage)
		}
		return [][]byte{anthropicFrame("message_delta", payload)}, rep, nil
	case EventMessageStop:
		return [][]byte{anthropicFrame("message_stop", map[string]interface{}{
			"type": "message_stop",
		})}, rep, nil
	case EventPing:
		return [][]byte{anthropicFrame("ping", map[string]interface{}{
			"type": "ping",
		})}, rep, nil
	case EventError:
		return [][]byte{anthropicFrame("error", map[string]interface{}{
			"type":  "error",
			"error": map[string]interface{}{"type": "error", "message": ev.ErrMessage},
		})}, rep, nil
	}
	return nil, rep, nil
}
