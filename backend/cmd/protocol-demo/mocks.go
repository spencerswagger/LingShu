package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 三个 mock 上游监听的地址（与网关渠道 BaseURL 对应）。
const (
	mockChatAddr = "127.0.0.1:19001"
	mockRespAddr = "127.0.0.1:19002"
	mockAnthAddr = "127.0.0.1:19003"

	protoChat = "openai-chat"
	protoResp = "openai-responses"
	protoAnth = "anthropic-messages"
)

// 关键内容标记：请求体里嵌入下列标记，mock 据此决定返回行为。
// 这些标记同时是下游内容断言的目标串（文本/推理必须包含它们）。
const (
	textMarker      = "DEMOTEXT"
	reasonMarker    = "DEMOREASON"
	imageURLDemo    = "https://example.com/protocol-demo.png"
	caseMarkPlain   = "PLAINCASE"
	caseMarkTool    = "TOOLCASE"
	caseMarkReason  = "REASONCASE"
	caseMarkImage   = "IMAGECASE"
	toolNameDemo    = "demo_tool"
	upstreamModelID = "demo-internal"
)

type caseKind string

const (
	kindPlain  caseKind = "plain"
	kindTool   caseKind = "tool"
	kindReason caseKind = "reason"
	kindImage  caseKind = "image"
)

var caseMarkers = []struct {
	mark string
	kind caseKind
}{
	{caseMarkImage, kindImage},
	{caseMarkReason, kindReason},
	{caseMarkTool, kindTool},
	{caseMarkPlain, kindPlain},
}

// detectCase 扫描原始请求体，识别本次请求的行为标记。
func detectCase(body []byte) caseKind {
	s := string(body)
	for _, m := range caseMarkers {
		if strings.Contains(s, m.mark) {
			return m.kind
		}
	}
	return kindPlain
}

// mockObserved 一次 mock 收到请求的观测结果（供用例断言「上游收到目标协议形状」）。
type mockObserved struct {
	Path       string
	Protocol   string
	Case       caseKind
	Stream     bool
	ShapeOK    bool
	ShapeNote  string
	ImageSeen  bool
	BodySample string
	RespSample string
}

// mock 是一个「只讲一种协议」的上游：路径固定，形状校验严格，收到异种协议形状返回 400。
type mock struct {
	name         string
	protocol     string
	expectedPath string
	addr         string

	mu       sync.Mutex
	last     mockObserved
	total    int
	accepted int
	rejected int
}

func newMock(name, protocol, expectedPath, addr string) *mock {
	return &mock{name: name, protocol: protocol, expectedPath: expectedPath, addr: addr}
}

// start 启动 mock HTTP 服务（阻塞前先监听，确保端口就绪）。
func (m *mock) start() (func(), error) {
	ln, err := net.Listen("tcp", m.addr)
	if err != nil {
		return nil, fmt.Errorf("mock %s listen %s: %w", m.name, m.addr, err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(m.handle)}
	go func() { _ = srv.Serve(ln) }()
	stop := func() {
		_ = srv.Close()
	}
	return stop, nil
}

func (m *mock) snapshot() mockObserved {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.last
}

func (m *mock) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	if r.URL.Path != m.expectedPath {
		m.record(mockObserved{
			Path: r.URL.Path, Protocol: m.protocol, Case: detectCase(body),
			ShapeOK: false, ShapeNote: fmt.Sprintf("上游路径错误：期望 %s，实际 %s", m.expectedPath, r.URL.Path),
			BodySample: sample(string(body)),
		})
		writeJSON(w, http.StatusNotFound, map[string]any{
			"error": "unexpected upstream path", "want": m.expectedPath, "got": r.URL.Path,
		})
		return
	}
	kind := detectCase(body)
	stream := requestStream(body)
	shapeOK, note := m.validate(body)
	obs := mockObserved{
		Path:       r.URL.Path,
		Protocol:   m.protocol,
		Case:       kind,
		Stream:     stream,
		ShapeOK:    shapeOK,
		ShapeNote:  note,
		ImageSeen:  m.imageSeen(body),
		BodySample: sample(string(body)),
	}
	var respBody []byte
	if stream {
		respBody = []byte(m.buildStream(kind))
	} else {
		respBody = m.buildNonStream(kind)
	}
	obs.RespSample = sample(string(respBody))
	m.record(obs)
	if !shapeOK {
		m.mu.Lock()
		m.rejected++
		m.mu.Unlock()
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"error": "wrong protocol shape", "note": note, "got": sample(string(body)),
		})
		return
	}
	m.mu.Lock()
	m.accepted++
	m.mu.Unlock()
	if stream {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = w.Write(respBody)
		if fl != nil {
			fl.Flush()
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBody)
}

func (m *mock) record(o mockObserved) {
	m.mu.Lock()
	m.last = o
	m.total++
	m.mu.Unlock()
}

// callCount 返回 mock 累计收到的请求数（用于判定本次用例确实触达了 mock）。
func (m *mock) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.total
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func sample(s string) string {
	if len(s) > 600 {
		return s[:600] + "..."
	}
	return s
}

func requestStream(body []byte) bool {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return false
	}
	var b bool
	if raw, ok := top["stream"]; ok {
		_ = json.Unmarshal(raw, &b)
	}
	return b
}

// ===== 协议形状校验 =====

// obj 解析 JSON 对象；失败返回 nil。
func obj(body []byte) map[string]json.RawMessage {
	var top map[string]json.RawMessage
	if json.Unmarshal(body, &top) != nil {
		return nil
	}
	return top
}

// inspectMessages 返回 messages 数组里的角色集合与内容块类型集合。
func inspectMessages(raw json.RawMessage) (roles map[string]bool, blockTypes map[string]bool) {
	roles = map[string]bool{}
	blockTypes = map[string]bool{}
	if len(raw) == 0 {
		return
	}
	var msgs []map[string]json.RawMessage
	if json.Unmarshal(raw, &msgs) != nil {
		return
	}
	for _, msg := range msgs {
		var role string
		_ = json.Unmarshal(msg["role"], &role)
		if role != "" {
			roles[role] = true
		}
		var parts []map[string]json.RawMessage
		if json.Unmarshal(msg["content"], &parts) == nil {
			for _, p := range parts {
				var t string
				_ = json.Unmarshal(p["type"], &t)
				if t != "" {
					blockTypes[t] = true
				}
			}
		}
	}
	return
}

func keysSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// validateOpenAIChat 校验请求体确为 openai-chat 形状。
func validateOpenAIChat(body []byte) (bool, string) {
	top := obj(body)
	if top == nil {
		return false, "请求体不是 JSON 对象"
	}
	if _, ok := top["messages"]; !ok {
		return false, "缺少 messages 字段"
	}
	for _, foreign := range []string{"input", "instructions", "system", "max_output_tokens"} {
		if _, ok := top[foreign]; ok {
			return false, "残留异种协议字段 " + foreign
		}
	}
	_, bt := inspectMessages(top["messages"])
	allowed := map[string]bool{"text": true, "image_url": true}
	for t := range bt {
		if !allowed[t] {
			return false, "含非 openai-chat 内容块类型 " + t
		}
	}
	return true, ""
}

// validateResponses 校验请求体确为 openai-responses 形状。
func validateResponses(body []byte) (bool, string) {
	top := obj(body)
	if top == nil {
		return false, "请求体不是 JSON 对象"
	}
	if _, ok := top["input"]; !ok {
		return false, "缺少 input 字段"
	}
	for _, foreign := range []string{"messages", "system", "max_tokens"} {
		if _, ok := top[foreign]; ok {
			return false, "残留异种协议字段 " + foreign
		}
	}
	return true, ""
}

// validateAnthropic 校验请求体确为 anthropic-messages 形状。
func validateAnthropic(body []byte) (bool, string) {
	top := obj(body)
	if top == nil {
		return false, "请求体不是 JSON 对象"
	}
	if _, ok := top["messages"]; !ok {
		return false, "缺少 messages 字段"
	}
	if _, ok := top["max_tokens"]; !ok {
		return false, "缺少 max_tokens 字段（anthropic 必填）"
	}
	for _, foreign := range []string{"input", "instructions", "max_output_tokens"} {
		if _, ok := top[foreign]; ok {
			return false, "残留异种协议字段 " + foreign
		}
	}
	roles, bt := inspectMessages(top["messages"])
	for role := range roles {
		if role != "user" && role != "assistant" {
			return false, "含非 anthropic 消息角色 " + role
		}
	}
	allowed := map[string]bool{"text": true, "image": true, "tool_use": true, "tool_result": true, "thinking": true}
	for t := range bt {
		if !allowed[t] {
			return false, "含非 anthropic 内容块类型 " + t
		}
	}
	return true, ""
}

func (m *mock) validate(body []byte) (bool, string) {
	switch m.protocol {
	case protoChat:
		return validateOpenAIChat(body)
	case protoResp:
		return validateResponses(body)
	case protoAnth:
		return validateAnthropic(body)
	}
	return false, "unknown protocol " + m.protocol
}

// imageSeen 判断目标协议形状里是否含图片块。
func (m *mock) imageSeen(body []byte) bool {
	top := obj(body)
	if top == nil {
		return false
	}
	switch m.protocol {
	case protoChat:
		_, bt := inspectMessages(top["messages"])
		return bt["image_url"]
	case protoAnth:
		_, bt := inspectMessages(top["messages"])
		return bt["image"]
	case protoResp:
		var items []map[string]json.RawMessage
		if json.Unmarshal(top["input"], &items) != nil {
			return false
		}
		for _, it := range items {
			var parts []map[string]json.RawMessage
			if json.Unmarshal(it["content"], &parts) != nil {
				continue
			}
			for _, p := range parts {
				var t string
				_ = json.Unmarshal(p["type"], &t)
				if t == "input_image" {
					return true
				}
			}
		}
	}
	return false
}

// ===== 响应构造（非流式） =====

func (m *mock) buildNonStream(kind caseKind) []byte {
	var v any
	switch m.protocol {
	case protoChat:
		v = chatNonStream(kind)
	case protoResp:
		v = respNonStream(kind)
	case protoAnth:
		v = anthNonStream(kind)
	}
	b, _ := json.Marshal(v)
	return b
}

func chatUsage() map[string]any {
	return map[string]any{
		"prompt_tokens": 12, "completion_tokens": 7, "total_tokens": 19,
		"prompt_tokens_details": map[string]any{"cached_tokens": 2},
	}
}

func chatNonStream(kind caseKind) map[string]any {
	msg := map[string]any{"role": "assistant"}
	finish := "stop"
	switch kind {
	case kindTool:
		finish = "tool_calls"
		msg["content"] = textMarker
		msg["tool_calls"] = []any{map[string]any{
			"id": "call_demo_1", "type": "function",
			"function": map[string]any{"name": toolNameDemo, "arguments": `{"city":"SF"}`},
		}}
	case kindReason:
		msg["content"] = textMarker
		msg["reasoning_content"] = reasonMarker
	default:
		msg["content"] = textMarker
	}
	return map[string]any{
		"id": "chatcmpl-demo", "object": "chat.completion", "model": upstreamModelID,
		"created": time.Now().Unix(),
		"choices": []any{map[string]any{"index": 0, "message": msg, "finish_reason": finish}},
		"usage":   chatUsage(),
	}
}

func respUsage() map[string]any {
	return map[string]any{
		"input_tokens": 14, "output_tokens": 9, "total_tokens": 23,
		"input_tokens_details": map[string]any{"cached_tokens": 3},
	}
}

func respNonStream(kind caseKind) map[string]any {
	out := []any{}
	switch kind {
	case kindTool:
		out = append(out, map[string]any{
			"type": "function_call", "name": toolNameDemo, "arguments": `{"city":"SF"}`, "call_id": "call_demo_1",
		})
	case kindReason:
		out = append(out, map[string]any{
			"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": reasonMarker}},
		})
		out = append(out, messageItem())
	default:
		out = append(out, messageItem())
	}
	return map[string]any{
		"id": "resp-demo", "object": "response", "model": upstreamModelID, "status": "completed",
		"output": out, "usage": respUsage(),
	}
}

func messageItem() map[string]any {
	return map[string]any{
		"type": "message", "role": "assistant",
		"content": []any{map[string]any{"type": "output_text", "text": textMarker}},
	}
}

func anthUsage() map[string]any {
	return map[string]any{
		"input_tokens": 11, "output_tokens": 6,
		"cache_creation_input_tokens": 0, "cache_read_input_tokens": 2,
	}
}

func anthNonStream(kind caseKind) map[string]any {
	content := []any{}
	stop := "end_turn"
	switch kind {
	case kindTool:
		stop = "tool_use"
		content = append(content, map[string]any{
			"type": "tool_use", "id": "toolu_demo_1", "name": toolNameDemo,
			"input": map[string]any{"city": "SF"},
		})
	case kindReason:
		content = append(content, map[string]any{"type": "thinking", "thinking": reasonMarker, "signature": "sig-demo"})
		content = append(content, map[string]any{"type": "text", "text": textMarker})
	default:
		content = append(content, map[string]any{"type": "text", "text": textMarker})
	}
	return map[string]any{
		"id": "msg-demo", "type": "message", "role": "assistant", "model": upstreamModelID,
		"content": content, "stop_reason": stop, "usage": anthUsage(),
	}
}

// ===== 响应构造（流式 SSE） =====

func (m *mock) buildStream(kind caseKind) string {
	switch m.protocol {
	case protoChat:
		return chatStream(kind)
	case protoResp:
		return respStream(kind)
	case protoAnth:
		return anthStream(kind)
	}
	return ""
}

func chatStream(kind caseKind) string {
	var b strings.Builder
	frame := func(v any) {
		j, _ := json.Marshal(v)
		b.WriteString("data: ")
		b.Write(j)
		b.WriteString("\n\n")
	}
	delta := func(d any) {
		frame(map[string]any{
			"id": "chatcmpl-demo", "object": "chat.completion.chunk", "model": upstreamModelID,
			"choices": []any{map[string]any{"index": 0, "delta": d}},
		})
	}
	delta(map[string]any{"role": "assistant", "content": ""})
	switch kind {
	case kindTool:
		delta(map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "id": "call_demo_1", "type": "function",
			"function": map[string]any{"name": toolNameDemo, "arguments": ""},
		}}})
		delta(map[string]any{"tool_calls": []any{map[string]any{
			"index": 0, "function": map[string]any{"arguments": `{"city":"SF"}`},
		}}})
		frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}})
	case kindReason:
		delta(map[string]any{"reasoning_content": reasonMarker})
		delta(map[string]any{"content": textMarker})
		frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
	default:
		delta(map[string]any{"content": textMarker})
		frame(map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})
	}
	frame(map[string]any{"choices": []any{}, "usage": chatUsage()})
	b.WriteString("data: [DONE]\n\n")
	return b.String()
}

func respStream(kind caseKind) string {
	var b strings.Builder
	frame := func(v any) {
		j, _ := json.Marshal(v)
		b.WriteString("data: ")
		b.Write(j)
		b.WriteString("\n\n")
	}
	frame(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp-demo", "status": "in_progress"}})
	switch kind {
	case kindTool:
		frame(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{
			"type": "function_call", "name": toolNameDemo, "call_id": "call_demo_1",
		}})
		frame(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": `{"city"`})
		frame(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "delta": `:"SF"}`})
	case kindReason:
		frame(map[string]any{"type": "response.reasoning_summary_text.delta", "output_index": 0, "delta": reasonMarker})
		frame(map[string]any{"type": "response.output_item.added", "output_index": 1, "item": map[string]any{"type": "message", "role": "assistant"}})
		frame(map[string]any{"type": "response.output_text.delta", "output_index": 1, "delta": textMarker})
	default:
		frame(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": map[string]any{"type": "message", "role": "assistant"}})
		frame(map[string]any{"type": "response.output_text.delta", "output_index": 0, "delta": textMarker})
	}
	frame(map[string]any{"type": "response.completed", "response": map[string]any{"status": "completed", "usage": respUsage()}})
	return b.String()
}

func anthStream(kind caseKind) string {
	var b strings.Builder
	ev := func(event string, payload any) {
		j, _ := json.Marshal(payload)
		b.WriteString("event: ")
		b.WriteString(event)
		b.WriteString("\ndata: ")
		b.Write(j)
		b.WriteString("\n\n")
	}
	ev("message_start", map[string]any{"type": "message_start", "message": map[string]any{
		"id": "msg-demo", "type": "message", "role": "assistant", "model": upstreamModelID, "content": []any{},
		"usage": map[string]any{"input_tokens": 11, "output_tokens": 0, "cache_read_input_tokens": 2},
	}})
	switch kind {
	case kindTool:
		ev("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "tool_use", "id": "toolu_demo_1", "name": toolNameDemo, "input": map[string]any{}}})
		ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `{"city"`}})
		ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": `:"SF"}`}})
		ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		ev("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 6}})
	case kindReason:
		ev("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "thinking", "thinking": ""}})
		ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "thinking_delta", "thinking": reasonMarker}})
		ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		ev("content_block_start", map[string]any{"type": "content_block_start", "index": 1,
			"content_block": map[string]any{"type": "text", "text": ""}})
		ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 1,
			"delta": map[string]any{"type": "text_delta", "text": textMarker}})
		ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": 1})
		ev("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 6}})
	default:
		ev("content_block_start", map[string]any{"type": "content_block_start", "index": 0,
			"content_block": map[string]any{"type": "text", "text": ""}})
		ev("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0,
			"delta": map[string]any{"type": "text_delta", "text": textMarker}})
		ev("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		ev("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": map[string]any{"output_tokens": 6}})
	}
	ev("message_stop", map[string]any{"type": "message_stop"})
	return b.String()
}