package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// parsedResp 是从下游响应解析出的内容特征（用于断言）。
type parsedResp struct {
	parseable bool
	note      string
	text      string
	reasoning string
	tool      bool
}

// testRow 是结果表的一行。
type testRow struct {
	path     string
	caseName string
	ok       bool
	note     string
}

func userText(kind caseKind) string {
	switch kind {
	case kindTool:
		return "Please call the demo tool now. " + caseMarkTool
	case kindReason:
		return "Think carefully, then answer. " + caseMarkReason
	case kindImage:
		return "Describe this picture. " + caseMarkImage
	default:
		return "Say hello back. " + caseMarkPlain
	}
}

func chatToolDef() map[string]any {
	return map[string]any{
		"type": "function",
		"function": map[string]any{
			"name":        toolNameDemo,
			"description": "demo tool",
			"parameters": map[string]any{
				"type":       "object",
				"properties": map[string]any{"city": map[string]any{"type": "string"}},
				"required":   []any{"city"},
			},
		},
	}
}

func respToolDef() map[string]any {
	return map[string]any{
		"type": "function", "name": toolNameDemo, "description": "demo tool",
		"parameters": map[string]any{
			"type":       "object",
			"properties": map[string]any{"city": map[string]any{"type": "string"}},
			"required":   []any{"city"},
		},
	}
}

func anthToolDef() map[string]any {
	return map[string]any{
		"name": toolNameDemo, "description": "demo tool",
		"input_schema": map[string]any{
			"type":       "object",
			"properties": map[string]any{"city": map[string]any{"type": "string"}},
			"required":   []any{"city"},
		},
	}
}

// buildInbound 按入站协议构造请求（路径/请求体/鉴权头）。
func buildInbound(inbound, externalModel string, kind caseKind, stream bool) (string, []byte, map[string]string, error) {
	headers := map[string]string{}
	var body map[string]any
	switch inbound {
	case protoChat:
		content := any(userText(kind))
		if kind == kindImage {
			content = []any{
				map[string]any{"type": "text", "text": userText(kind)},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": imageURLDemo}},
			}
		}
		body = map[string]any{
			"model":  externalModel,
			"stream": stream,
			"messages": []any{
				map[string]any{"role": "system", "content": "You are a protocol demo assistant."},
				map[string]any{"role": "user", "content": content},
			},
		}
		if kind == kindTool {
			body["tools"] = []any{chatToolDef()}
		}
	case protoResp:
		parts := []any{map[string]any{"type": "input_text", "text": userText(kind)}}
		if kind == kindImage {
			parts = append(parts, map[string]any{"type": "input_image", "image_url": imageURLDemo})
		}
		body = map[string]any{
			"model":        externalModel,
			"stream":       stream,
			"instructions": "You are a protocol demo assistant.",
			"input":        []any{map[string]any{"type": "message", "role": "user", "content": parts}},
		}
		if kind == kindTool {
			body["tools"] = []any{respToolDef()}
		}
	case protoAnth:
		content := any(userText(kind))
		if kind == kindImage {
			content = []any{
				map[string]any{"type": "text", "text": userText(kind)},
				map[string]any{"type": "image", "source": map[string]any{"type": "url", "url": imageURLDemo}},
			}
		}
		body = map[string]any{
			"model":      externalModel,
			"stream":     stream,
			"max_tokens": 256,
			"system":     "You are a protocol demo assistant.",
			"messages":   []any{map[string]any{"role": "user", "content": content}},
		}
		if kind == kindTool {
			body["tools"] = []any{anthToolDef()}
		}
	default:
		return "", nil, nil, fmt.Errorf("unknown inbound protocol %q", inbound)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return "", nil, nil, err
	}
	path := inboundPath(inbound)
	return path, raw, headers, nil
}

func inboundPath(inbound string) string {
	switch inbound {
	case protoChat:
		return "/v1/chat/completions"
	case protoResp:
		return "/v1/responses"
	case protoAnth:
		return "/v1/messages"
	}
	return "/v1/chat/completions"
}

// sendGateway 向网关发出一次客户端请求。
func sendGateway(client *http.Client, base, path string, body []byte, inbound, token string) (int, http.Header, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return 0, nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	if inbound == protoAnth {
		req.Header.Set("x-api-key", token)
	} else {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, resp.Header, raw, nil
}

// ===== 下游响应解析 =====

func textFromRaw(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

func parseNonStream(inbound string, raw []byte) parsedResp {
	switch inbound {
	case protoChat:
		var r struct {
			Choices []struct {
				Message struct {
					Content          json.RawMessage   `json:"content"`
					ReasoningContent string            `json:"reasoning_content"`
					ToolCalls        []json.RawMessage `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return parsedResp{note: "非流式响应非 openai-chat JSON: " + err.Error()}
		}
		if len(r.Choices) == 0 {
			return parsedResp{note: "choices 为空"}
		}
		m := r.Choices[0].Message
		return parsedResp{
			parseable: true,
			text:      textFromRaw(m.Content),
			reasoning: m.ReasoningContent,
			tool:      len(m.ToolCalls) > 0,
		}
	case protoAnth:
		var r struct {
			Content []struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"content"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return parsedResp{note: "非流式响应非 anthropic JSON: " + err.Error()}
		}
		var b, rb strings.Builder
		tool := false
		for _, c := range r.Content {
			switch c.Type {
			case "text":
				b.WriteString(c.Text)
			case "thinking":
				rb.WriteString(c.Thinking)
			case "tool_use":
				tool = true
			}
		}
		return parsedResp{parseable: true, text: b.String(), reasoning: rb.String(), tool: tool}
	case protoResp:
		var r struct {
			Output []struct {
				Type    string `json:"type"`
				Name    string `json:"name"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
				Summary []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"summary"`
			} `json:"output"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return parsedResp{note: "非流式响应非 openai-responses JSON: " + err.Error()}
		}
		var b, rb strings.Builder
		tool := false
		for _, it := range r.Output {
			switch it.Type {
			case "message":
				for _, c := range it.Content {
					b.WriteString(c.Text)
				}
			case "function_call":
				tool = true
			case "reasoning":
				for _, s := range it.Summary {
					rb.WriteString(s.Text)
				}
			}
		}
		return parsedResp{parseable: true, text: b.String(), reasoning: rb.String(), tool: tool}
	}
	return parsedResp{note: "未知入站协议 " + inbound}
}

func parseStream(inbound string, raw []byte) parsedResp {
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var b, rb strings.Builder
	tool := false
	sawFrame := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		ev, ok := decodeStreamLine(inbound, payload)
		if !ok {
			continue
		}
		sawFrame = true
		b.WriteString(ev.text)
		rb.WriteString(ev.reasoning)
		if ev.tool {
			tool = true
		}
	}
	if !sawFrame {
		return parsedResp{note: "流式响应未解析出任何 " + inbound + " 数据帧"}
	}
	return parsedResp{parseable: true, text: b.String(), reasoning: rb.String(), tool: tool}
}

type streamEvent struct {
	text      string
	reasoning string
	tool      bool
}

func decodeStreamLine(inbound, payload string) (streamEvent, bool) {
	switch inbound {
	case protoChat:
		var ev struct {
			Choices []struct {
				Delta struct {
					Content          string            `json:"content"`
					ReasoningContent string            `json:"reasoning_content"`
					ToolCalls        []json.RawMessage `json:"tool_calls"`
				} `json:"delta"`
				Message struct {
					Content          string            `json:"content"`
					ReasoningContent string            `json:"reasoning_content"`
					ToolCalls        []json.RawMessage `json:"tool_calls"`
				} `json:"message"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			return streamEvent{}, false
		}
		var out streamEvent
		for _, c := range ev.Choices {
			out.text += c.Delta.Content + c.Message.Content
			out.reasoning += c.Delta.ReasoningContent + c.Message.ReasoningContent
			if len(c.Delta.ToolCalls) > 0 || len(c.Message.ToolCalls) > 0 {
				out.tool = true
			}
		}
		return out, true
	case protoAnth:
		var ev struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			return streamEvent{}, false
		}
		var out streamEvent
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				out.tool = true
			}
		case "content_block_delta":
			switch ev.Delta.Type {
			case "text_delta":
				out.text = ev.Delta.Text
			case "thinking_delta":
				out.reasoning = ev.Delta.Thinking
			case "input_json_delta":
				out.tool = true
			}
		}
		return out, true
	case protoResp:
		var ev struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
			Item  struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			return streamEvent{}, false
		}
		var out streamEvent
		switch ev.Type {
		case "response.output_text.delta":
			out.text = ev.Delta
		case "response.reasoning_summary_text.delta":
			out.reasoning = ev.Delta
		case "response.function_call_arguments.delta":
			out.tool = true
		case "response.output_item.added":
			if ev.Item.Type == "function_call" {
				out.tool = true
			}
			if ev.Item.Type == "reasoning" {
				out.reasoning = "1"
			}
		}
		return out, true
	}
	return streamEvent{}, false
}

// ===== 用例执行 =====

type matrixEnv struct {
	base       string
	token      string
	admin      *gwClient
	client     *http.Client
	mocks      map[string]*mock
	externalID map[string]string // channel protocol -> external model name
}

// runMatrix 执行 9 路径 × 8 用例的完整矩阵。
func runMatrix(env *matrixEnv) []testRow {
	protos := []string{protoChat, protoResp, protoAnth}
	cases := []struct {
		name string
		kind caseKind
		strm bool
	}{
		{"text", kindPlain, false},
		{"text-stream", kindPlain, true},
		{"tool", kindTool, false},
		{"tool-stream", kindTool, true},
		{"reason", kindReason, false},
		{"reason-stream", kindReason, true},
		{"image", kindImage, false},
		{"image-stream", kindImage, true},
	}
	var rows []testRow
	for _, chanProto := range protos {
		externalModel := env.externalID[chanProto]
		for _, inbound := range protos {
			for _, tc := range cases {
				path := inbound + " >> " + chanProto
				rows = append(rows, runCase(env, path, inbound, chanProto, externalModel, tc.name, tc.kind, tc.strm))
			}
		}
	}
	return rows
}

func runCase(env *matrixEnv, path, inbound, chanProto, externalModel, caseName string, kind caseKind, stream bool) testRow {
	row := testRow{path: path, caseName: caseName}
	reqPath, body, headers, err := buildInbound(inbound, externalModel, kind, stream)
	if err != nil {
		row.note = "构建请求失败: " + err.Error()
		return row
	}
	_ = headers
	m := env.mocks[chanProto]
	before := m.callCount()

	status, hdr, raw, err := sendGateway(env.client, env.base, reqPath, body, inbound, env.token)
	if err != nil {
		row.note = "请求网关失败: " + err.Error()
		return row
	}
	obs := m.snapshot()
	after := m.callCount()

	if status != http.StatusOK {
		row.note = fmt.Sprintf("下游 HTTP %d（期望 200）；响应片段=%s", status, sample(string(raw)))
		if after > before && !obs.ShapeOK {
			row.note += "；上游拒绝: " + obs.ShapeNote + " 上游收到=" + obs.BodySample
		}
		return row
	}
	if stream {
		if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
			row.note = "流式响应的 Content-Type 非 text/event-stream: " + ct
			return row
		}
	}
	if after <= before {
		row.note = "目标渠道的上游 mock 未收到请求（未路由到预期渠道）"
		return row
	}
	if !obs.ShapeOK {
		row.note = "上游收到非目标协议(" + chanProto + ")形状: " + obs.ShapeNote + "；上游收到=" + obs.BodySample
		return row
	}
	if kind == kindImage && !obs.ImageSeen {
		row.note = "上游未收到目标协议的图片块；上游收到=" + obs.BodySample
		return row
	}

	var p parsedResp
	if stream {
		p = parseStream(inbound, raw)
	} else {
		p = parseNonStream(inbound, raw)
	}
	if !p.parseable {
		row.note = "下游响应无法按 " + inbound + " 解析: " + p.note + "；响应片段=" + sample(string(raw))
		return row
	}

	switch kind {
	case kindTool:
		if !p.tool {
			row.note = "下游未出现工具调用；下游=" + sample(string(raw)) + "；上游 mock 返回=" + obs.RespSample
			return row
		}
	case kindReason:
		if !strings.Contains(p.reasoning, reasonMarker) {
			row.note = "下游未出现推理内容(" + reasonMarker + ")；下游=" + sample(string(raw)) + "；上游 mock 返回=" + obs.RespSample
			return row
		}
	case kindImage:
		if !strings.Contains(p.text, textMarker) {
			row.note = "下游未出现文本(" + textMarker + ")；下游=" + sample(string(raw)) + "；上游 mock 返回=" + obs.RespSample
			return row
		}
	default:
		if !strings.Contains(p.text, textMarker) {
			row.note = "下游未出现文本(" + textMarker + ")；下游=" + sample(string(raw)) + "；上游 mock 返回=" + obs.RespSample
			return row
		}
	}
	row.ok = true
	row.note = "ok（上游=" + chanProto + " 形状校验通过）"
	return row
}