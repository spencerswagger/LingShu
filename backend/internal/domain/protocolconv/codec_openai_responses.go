package protocolconv

import (
	"bytes"
	"encoding/json"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

type openaiResponsesCodec struct{}

var (
	_ RequestCodec  = openaiResponsesCodec{}
	_ ResponseCodec = openaiResponsesCodec{}
	_ StreamCodec   = openaiResponsesCodec{}
)

const (
	respRoleUser      = "user"
	respRoleAssistant = "assistant"
	respRoleTool      = "tool"
)

type respItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Summary   json.RawMessage `json:"summary"`
}

type respContentPart struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	ImageURL string `json:"image_url"`
}

func respKnownRequestField(k string) bool {
	switch k {
	case "model", "input", "instructions", "tools", "tool_choice",
		"temperature", "top_p", "max_output_tokens", "stream":
		return true
	}
	return false
}

func respKnownResponseField(k string) bool {
	switch k {
	case "id", "model", "output", "status", "incomplete_details",
		"usage", "object", "created_at":
		return true
	}
	return false
}

// ParseRequest 将 OpenAI Responses 请求体解析为规范请求。
func (openaiResponsesCodec) ParseRequest(body []byte) (*CanonicalRequest, *LossyReport, error) {
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

	if raw, ok := top["instructions"]; ok && !isJSONNull(raw) {
		var s string
		if err := json.Unmarshal(raw, &s); err == nil && s != "" {
			req.System = append(req.System, ContentBlock{Kind: BlockText, Text: s})
		}
	}

	if raw, ok := top["input"]; ok && !isJSONNull(raw) {
		if err := parseRespInput(raw, req, rep); err != nil {
			return nil, rep, err
		}
	}

	if raw, ok := top["tools"]; ok && !isJSONNull(raw) {
		if err := parseRespTools(raw, req, rep); err != nil {
			return nil, rep, err
		}
	}

	if raw, ok := top["tool_choice"]; ok && !isJSONNull(raw) {
		parseRespToolChoice(raw, req)
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

	if raw, ok := top["max_output_tokens"]; ok && !isJSONNull(raw) {
		if v, ok := rawInt64(raw); ok {
			req.MaxTokens = &v
		}
	}

	if raw, ok := top["stream"]; ok && !isJSONNull(raw) {
		_ = json.Unmarshal(raw, &req.Stream)
	}

	for k, v := range top {
		if respKnownRequestField(k) {
			continue
		}
		if req.Extra == nil {
			req.Extra = map[string]json.RawMessage{}
		}
		req.Extra[k] = canonicalRaw(v)
	}

	return req, rep, nil
}

// SerializeRequest 将规范请求序列化为 OpenAI Responses 请求体。
func (openaiResponsesCodec) SerializeRequest(req *CanonicalRequest) ([]byte, *LossyReport, error) {
	rep := &LossyReport{}
	if req == nil {
		return nil, rep, nil
	}

	out := map[string]interface{}{}
	if req.Model != "" {
		out["model"] = req.Model
	}

	inputItems := make([]interface{}, 0)
	for _, m := range req.Messages {
		inputItems = append(inputItems, serializeRespInputMessage(m, rep)...)
	}
	out["input"] = inputItems

	if len(req.System) > 0 {
		out["instructions"] = blocksText(req.System)
	}

	if len(req.Tools) > 0 {
		tools := make([]interface{}, 0, len(req.Tools))
		for _, t := range req.Tools {
			item := map[string]interface{}{"type": "function", "name": t.Name}
			if t.Description != "" {
				item["description"] = t.Description
			}
			if len(t.InputSchema) > 0 {
				item["parameters"] = t.InputSchema
			}
			tools = append(tools, item)
		}
		out["tools"] = tools
	}

	if req.ToolChoice != nil {
		switch req.ToolChoice.Mode {
		case ToolChoiceAuto, ToolChoiceNone, ToolChoiceRequired:
			out["tool_choice"] = req.ToolChoice.Mode
		case ToolChoiceTool:
			out["tool_choice"] = map[string]interface{}{
				"type": "function",
				"name": req.ToolChoice.Name,
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
		out["max_output_tokens"] = *req.MaxTokens
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

// ParseResponse 将 OpenAI Responses 响应体解析为规范响应。
func (openaiResponsesCodec) ParseResponse(body []byte) (*CanonicalResponse, *LossyReport, error) {
	var top map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	if err := dec.Decode(&top); err != nil {
		return nil, nil, err
	}

	rep := &LossyReport{}
	resp := &CanonicalResponse{Role: respRoleAssistant}

	if raw, ok := top["id"]; ok && !isJSONNull(raw) {
		_ = json.Unmarshal(raw, &resp.ID)
	}
	if raw, ok := top["model"]; ok && !isJSONNull(raw) {
		_ = json.Unmarshal(raw, &resp.Model)
	}

	hasToolUse := false
	if raw, ok := top["output"]; ok && !isJSONNull(raw) {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, rep, err
		}
		for _, item := range items {
			var it respItem
			if err := json.Unmarshal(item, &it); err != nil {
				return nil, rep, err
			}
			switch it.Type {
			case "message":
				blocks, err := parseRespParts(it.Content, rep)
				if err != nil {
					return nil, rep, err
				}
				resp.Blocks = append(resp.Blocks, blocks...)
			case "function_call":
				resp.Blocks = append(resp.Blocks, respFunctionCallBlock(it.Name, it.Arguments, it.CallID, rep))
				hasToolUse = true
			case "reasoning":
				resp.Blocks = append(resp.Blocks, ContentBlock{
					Kind: BlockThinking,
					Text: parseRespSummary(it.Summary),
				})
			default:
				rep.Add("output.item." + it.Type)
			}
		}
	}

	if raw, ok := top["status"]; ok && !isJSONNull(raw) {
		var status string
		_ = json.Unmarshal(raw, &status)
		switch status {
		case "completed":
			if hasToolUse {
				resp.FinishReason = "tool_calls"
			} else {
				resp.FinishReason = "stop"
			}
		case "incomplete":
			resp.FinishReason = parseRespIncompleteReason(top["incomplete_details"])
		default:
			resp.FinishReason = status
		}
	}

	if raw, ok := top["usage"]; ok && !isJSONNull(raw) {
		resp.Usage = parseRespUsage(raw)
	}

	for k, v := range top {
		if respKnownResponseField(k) {
			continue
		}
		if resp.Extra == nil {
			resp.Extra = map[string]json.RawMessage{}
		}
		resp.Extra[k] = canonicalRaw(v)
	}

	return resp, rep, nil
}

// SerializeResponse 将规范响应序列化为 OpenAI Responses 响应体。
func (openaiResponsesCodec) SerializeResponse(resp *CanonicalResponse) ([]byte, *LossyReport, error) {
	rep := &LossyReport{}
	if resp == nil {
		return nil, rep, nil
	}

	output := make([]interface{}, 0, len(resp.Blocks))
	hasToolUse := false
	for _, b := range resp.Blocks {
		switch b.Kind {
		case BlockText:
			output = append(output, map[string]interface{}{
				"type": "message",
				"role": respRoleAssistant,
				"content": []interface{}{
					map[string]interface{}{"type": "output_text", "text": b.Text},
				},
			})
		case BlockToolUse:
			hasToolUse = true
			output = append(output, map[string]interface{}{
				"type":      "function_call",
				"name":      b.ToolName,
				"arguments": string(b.ToolInput),
				"call_id":   b.ToolUseID,
			})
		case BlockThinking:
			output = append(output, map[string]interface{}{
				"type": "reasoning",
				"summary": []interface{}{
					map[string]interface{}{"type": "summary_text", "text": b.Text},
				},
			})
		default:
			rep.Add("response.block." + b.Kind)
		}
	}

	out := map[string]interface{}{
		"object": "response",
		"output": output,
	}
	if resp.ID != "" {
		out["id"] = resp.ID
	}
	if resp.Model != "" {
		out["model"] = resp.Model
	}

	if resp.FinishReason != "" {
		switch {
		case resp.FinishReason == "tool_calls":
			out["status"] = "completed"
		case resp.FinishReason == "stop" && !hasToolUse:
			out["status"] = "completed"
		default:
			out["status"] = "incomplete"
			out["incomplete_details"] = map[string]interface{}{"reason": resp.FinishReason}
		}
	}

	usage := usageToResponses(resp.Usage)
	out["usage"] = usage

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

func parseRespInput(raw json.RawMessage, req *CanonicalRequest, rep *LossyReport) error {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return nil
	}
	switch t[0] {
	case '"':
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		req.Messages = append(req.Messages, Message{
			Role:   respRoleUser,
			Blocks: []ContentBlock{{Kind: BlockText, Text: s}},
		})
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return err
		}
		for _, item := range items {
			if err := parseRespInputItem(item, req, rep); err != nil {
				return err
			}
		}
	}
	return nil
}

func parseRespInputItem(raw json.RawMessage, req *CanonicalRequest, rep *LossyReport) error {
	var it respItem
	if err := json.Unmarshal(raw, &it); err != nil {
		return err
	}
	switch it.Type {
	case "message":
		role := it.Role
		if role == "" {
			role = respRoleUser
		}
		blocks, err := parseRespParts(it.Content, rep)
		if err != nil {
			return err
		}
		req.Messages = append(req.Messages, Message{Role: role, Blocks: blocks})
	case "function_call":
		req.Messages = append(req.Messages, Message{
			Role:   respRoleAssistant,
			Blocks: []ContentBlock{respFunctionCallBlock(it.Name, it.Arguments, it.CallID, rep)},
		})
	case "function_call_output":
		text := ""
		ot := bytes.TrimSpace(it.Output)
		if len(ot) > 0 && string(ot) != "null" {
			if ot[0] == '"' {
				_ = json.Unmarshal(it.Output, &text)
			} else {
				text = string(ot)
			}
		}
		req.Messages = append(req.Messages, Message{
			Role: respRoleTool,
			Blocks: []ContentBlock{{
				Kind:       BlockToolResult,
				ToolUseID:  it.CallID,
				ToolResult: []ContentBlock{{Kind: BlockText, Text: text}},
			}},
		})
	default:
		rep.Add("input.item." + it.Type)
	}
	return nil
}

func parseRespParts(raw json.RawMessage, rep *LossyReport) ([]ContentBlock, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return nil, nil
	}
	if t[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return nil, err
		}
		return []ContentBlock{{Kind: BlockText, Text: s}}, nil
	}
	if t[0] != '[' {
		return nil, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return nil, err
	}
	blocks := make([]ContentBlock, 0, len(parts))
	for _, p := range parts {
		var part respContentPart
		if err := json.Unmarshal(p, &part); err != nil {
			return nil, err
		}
		switch part.Type {
		case "input_text", "output_text", "text":
			blocks = append(blocks, ContentBlock{Kind: BlockText, Text: part.Text})
		case "input_image":
			blocks = append(blocks, ContentBlock{Kind: BlockImage, ImageURL: part.ImageURL})
		default:
			rep.Add("content.part." + part.Type)
		}
	}
	return blocks, nil
}

func parseRespTools(raw json.RawMessage, req *CanonicalRequest, rep *LossyReport) error {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return err
	}
	for _, item := range items {
		var t struct {
			Type        string          `json:"type"`
			Name        string          `json:"name"`
			Description string          `json:"description"`
			Parameters  json.RawMessage `json:"parameters"`
		}
		if err := json.Unmarshal(item, &t); err != nil {
			return err
		}
		if t.Type != "" && t.Type != "function" {
			rep.Add("tools.type." + t.Type)
			continue
		}
		req.Tools = append(req.Tools, Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: canonicalRaw(t.Parameters),
		})
	}
	return nil
}

func parseRespToolChoice(raw json.RawMessage, req *CanonicalRequest) {
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
			Type string `json:"type"`
			Name string `json:"name"`
		}
		if err := json.Unmarshal(raw, &tc); err == nil {
			req.ToolChoice = &ToolChoice{Mode: ToolChoiceTool, Name: tc.Name}
		}
	}
}

func respFunctionCallBlock(name, args, callID string, rep *LossyReport) ContentBlock {
	b := ContentBlock{
		Kind:      BlockToolUse,
		ToolUseID: callID,
		ToolName:  name,
	}
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
		rep.Note("function_call arguments not valid JSON; wrapped as JSON string")
	}
	return b
}

func serializeRespInputMessage(m Message, rep *LossyReport) []interface{} {
	items := make([]interface{}, 0, 1)

	if m.Role == respRoleTool {
		for _, b := range m.Blocks {
			if b.Kind == BlockToolResult {
				items = append(items, map[string]interface{}{
					"type":    "function_call_output",
					"call_id": b.ToolUseID,
					"output":  blocksText(b.ToolResult),
				})
				break
			}
		}
		return items
	}

	parts := make([]interface{}, 0)
	for _, b := range m.Blocks {
		switch b.Kind {
		case BlockText:
			partType := "input_text"
			if m.Role == respRoleAssistant {
				partType = "output_text"
			}
			parts = append(parts, map[string]interface{}{"type": partType, "text": b.Text})
		case BlockImage:
			parts = append(parts, map[string]interface{}{
				"type":      "input_image",
				"image_url": b.ImageURL,
			})
		case BlockToolUse:
		default:
			rep.Add("message.content." + b.Kind)
		}
	}
	if len(parts) > 0 {
		items = append(items, map[string]interface{}{
			"type":    "message",
			"role":    m.Role,
			"content": parts,
		})
	}

	for _, b := range m.Blocks {
		if b.Kind != BlockToolUse {
			continue
		}
		items = append(items, map[string]interface{}{
			"type":      "function_call",
			"name":      b.ToolName,
			"arguments": string(b.ToolInput),
			"call_id":   b.ToolUseID,
		})
	}

	return items
}

func parseRespSummary(raw json.RawMessage) string {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || string(t) == "null" {
		return ""
	}
	var parts []struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var sb bytes.Buffer
	for i, p := range parts {
		if i > 0 {
			sb.WriteString("\n")
		}
		sb.WriteString(p.Text)
	}
	return sb.String()
}

func parseRespIncompleteReason(raw json.RawMessage) string {
	if isJSONNull(raw) {
		return ""
	}
	var d struct {
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal(raw, &d)
	return d.Reason
}

type respUsagePayload struct {
	InputTokens        json.Number `json:"input_tokens"`
	OutputTokens       json.Number `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens json.Number `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

func parseRespUsage(raw json.RawMessage) billing.Usage {
	var u respUsagePayload
	if err := json.Unmarshal(raw, &u); err != nil {
		return billing.Usage{}
	}
	in := jsonNumberToInt64(u.InputTokens)
	outTokens := jsonNumberToInt64(u.OutputTokens)

	var cached int64
	if u.InputTokensDetails != nil {
		cached = jsonNumberToInt64(u.InputTokensDetails.CachedTokens)
	}

	input := in - cached
	if input < 0 {
		input = 0
	}
	return billing.Usage{
		Input:     input,
		Output:    outTokens,
		CacheRead: cached,
	}
}

// DecodeEvent 将单个 OpenAI Responses 流事件解析为 0..n 个规范事件。
func (openaiResponsesCodec) DecodeEvent(data []byte) ([]CanonicalEvent, *LossyReport, error) {
	rep := &LossyReport{}
	var top struct {
		Type        string `json:"type"`
		OutputIndex *int   `json:"output_index"`
		Delta       string `json:"delta"`
		Item        struct {
			Type   string `json:"type"`
			Name   string `json:"name"`
			CallID string `json:"call_id"`
		} `json:"item"`
		Response struct {
			Status string          `json:"status"`
			Usage  json.RawMessage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		return nil, rep, err
	}

	idx := 0
	if top.OutputIndex != nil {
		idx = *top.OutputIndex
	}

	switch top.Type {
	case "response.output_text.delta":
		return []CanonicalEvent{{
			Type:      EventContentBlockDelta,
			Index:     idx,
			DeltaKind: DeltaText,
			Text:      top.Delta,
		}}, rep, nil
	case "response.output_item.added":
		if top.Item.Type != "function_call" {
			return nil, rep, nil
		}
		return []CanonicalEvent{{
			Type:      EventContentBlockStart,
			Index:     idx,
			BlockKind: BlockToolUse,
			ToolUseID: top.Item.CallID,
			ToolName:  top.Item.Name,
		}}, rep, nil
	case "response.function_call_arguments.delta":
		return []CanonicalEvent{{
			Type:      EventContentBlockDelta,
			Index:     idx,
			DeltaKind: DeltaToolJSON,
			Text:      top.Delta,
		}}, rep, nil
	case "response.completed", "response.incomplete":
		status := top.Response.Status
		if status == "" && top.Type == "response.incomplete" {
			status = "incomplete"
		}
		ev := CanonicalEvent{
			Type:       EventMessageDelta,
			StopReason: stopReasonToCanonical(channelprotocol.ProtocolOpenAIResponses, status),
		}
		if len(top.Response.Usage) > 0 && !isJSONNull(top.Response.Usage) {
			ev.Usage = parseRespUsage(top.Response.Usage)
			ev.HasUsage = true
		}
		return []CanonicalEvent{ev}, rep, nil
	}
	return nil, rep, nil
}

// EncodeEvent 将单个规范事件编码为 OpenAI Responses SSE 帧。
func (openaiResponsesCodec) EncodeEvent(ev CanonicalEvent) ([][]byte, *LossyReport, error) {
	rep := &LossyReport{}
	switch ev.Type {
	case EventMessageStart:
		return nil, rep, nil
	case EventContentBlockStart:
		if ev.BlockKind != BlockToolUse {
			return nil, rep, nil
		}
		return [][]byte{dataFrame(map[string]interface{}{
			"type":         "response.output_item.added",
			"output_index": ev.Index,
			"item": map[string]interface{}{
				"type":    "function_call",
				"name":    ev.ToolName,
				"call_id": ev.ToolUseID,
			},
		})}, rep, nil
	case EventContentBlockDelta:
		switch ev.DeltaKind {
		case DeltaText:
			return [][]byte{dataFrame(map[string]interface{}{
				"type":         "response.output_text.delta",
				"output_index": ev.Index,
				"delta":        ev.Text,
			})}, rep, nil
		case DeltaToolJSON:
			return [][]byte{dataFrame(map[string]interface{}{
				"type":         "response.function_call_arguments.delta",
				"output_index": ev.Index,
				"delta":        ev.Text,
			})}, rep, nil
		default:
			return nil, rep, nil
		}
	case EventContentBlockStop:
		return nil, rep, nil
	case EventMessageDelta:
		if ev.StopReason == "" && !ev.HasUsage {
			return nil, rep, nil
		}
		resp := map[string]interface{}{}
		if ev.StopReason != "" {
			resp["status"] = responsesStatus(ev.StopReason)
		}
		if ev.HasUsage {
			resp["usage"] = usageToResponses(ev.Usage)
		}
		return [][]byte{dataFrame(map[string]interface{}{
			"type":     "response.completed",
			"response": resp,
		})}, rep, nil
	case EventMessageStop:
		return [][]byte{[]byte("data: [DONE]\n\n")}, rep, nil
	}
	return nil, rep, nil
}
