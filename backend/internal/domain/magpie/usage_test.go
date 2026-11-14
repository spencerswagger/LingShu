package magpie

import (
	"testing"
)

// TestUsageCollector_NonStream 覆盖四协议非流式的 usage 字段路径与三段归一化。
func TestUsageCollector_NonStream(t *testing.T) {
	cases := []struct {
		name     string
		protocol Protocol
		body     string
		want     Usage
		wantOK   bool
	}{
		{
			name:     "OpenAI Chat 含缓存",
			protocol: ProtocolOpenAIChat,
			body:     `{"id":"x","usage":{"prompt_tokens":100,"completion_tokens":40,"prompt_tokens_details":{"cached_tokens":30}}}`,
			want:     Usage{Input: 70, CacheRead: 30, Output: 40},
			wantOK:   true,
		},
		{
			name:     "OpenAI Chat 顶层缓存兜底键",
			protocol: ProtocolOpenAIChat,
			body:     `{"usage":{"prompt_tokens":50,"completion_tokens":10,"prompt_cache_hit_tokens":20}}`,
			want:     Usage{Input: 30, CacheRead: 20, Output: 10},
			wantOK:   true,
		},
		{
			name:     "OpenAI Responses",
			protocol: ProtocolOpenAIResponses,
			body:     `{"id":"r","usage":{"input_tokens":80,"output_tokens":20,"input_tokens_details":{"cached_tokens":8}}}`,
			want:     Usage{Input: 72, CacheRead: 8, Output: 20},
			wantOK:   true,
		},
		{
			name:     "Anthropic 缓存写并入输入",
			protocol: ProtocolAnthropic,
			body:     `{"type":"message","usage":{"input_tokens":100,"output_tokens":25,"cache_read_input_tokens":40,"cache_creation_input_tokens":6}}`,
			want:     Usage{Input: 106, CacheRead: 40, Output: 25},
			wantOK:   true,
		},
		{
			name:     "Gemini thoughts 计入输出",
			protocol: ProtocolGemini,
			body:     `{"candidates":[],"usageMetadata":{"promptTokenCount":90,"candidatesTokenCount":12,"cachedContentTokenCount":10,"thoughtsTokenCount":8}}`,
			want:     Usage{Input: 80, CacheRead: 10, Output: 20},
			wantOK:   true,
		},
		{
			name:     "缺失 usage → 未取到",
			protocol: ProtocolOpenAIChat,
			body:     `{"id":"x","choices":[]}`,
			wantOK:   false,
		},
		{
			name:     "全零 usage 视为未取到（零值守卫）",
			protocol: ProtocolOpenAIChat,
			body:     `{"usage":{"prompt_tokens":0,"completion_tokens":0}}`,
			wantOK:   false,
		},
		{
			name:     "坏 JSON 不影响透传，只是没取到",
			protocol: ProtocolOpenAIChat,
			body:     `<html>502 Bad Gateway</html>`,
			wantOK:   false,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			col := newUsageCollector(c.protocol, false)
			col.Feed([]byte(c.body))
			col.finish()
			got, ok := col.Result()
			if ok != c.wantOK {
				t.Fatalf("HasUsage = %v, 期望 %v (usage=%+v)", ok, c.wantOK, got)
			}
			if ok && got != c.want {
				t.Fatalf("usage = %+v, 期望 %+v", got, c.want)
			}
		})
	}
}

// TestUsageCollector_Stream 覆盖四协议流式的采集点（含跨行分包）。
func TestUsageCollector_Stream(t *testing.T) {
	openAIChat := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"prompt_tokens_details\":{\"cached_tokens\":2}}}\n\n" +
		"data: [DONE]\n\n"
	// Anthropic：输入在 message_start，输出在 message_delta（累计）。
	anthropic := "event: message_start\n" +
		"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":100,\"cache_read_input_tokens\":30}}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: message_delta\n" +
		"data: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\n"
	responses := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"usage\":null}}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":60,\"output_tokens\":9,\"input_tokens_details\":{\"cached_tokens\":6}}}}\n\n"
	gemini := "data: {\"candidates\":[],\"usageMetadata\":{\"promptTokenCount\":100,\"candidatesTokenCount\":1}}\n\n" +
		"data: {\"candidates\":[],\"usageMetadata\":{\"promptTokenCount\":100,\"candidatesTokenCount\":20,\"cachedContentTokenCount\":4,\"thoughtsTokenCount\":2}}\n\n"

	cases := []struct {
		name     string
		protocol Protocol
		stream   string
		want     Usage
	}{
		{name: "OpenAI Chat 末帧 usage", protocol: ProtocolOpenAIChat, stream: openAIChat, want: Usage{Input: 8, CacheRead: 2, Output: 5}},
		{name: "Anthropic start+delta 合并", protocol: ProtocolAnthropic, stream: anthropic, want: Usage{Input: 100, CacheRead: 30, Output: 42}},
		{name: "Responses completed 事件", protocol: ProtocolOpenAIResponses, stream: responses, want: Usage{Input: 54, CacheRead: 6, Output: 9}},
		{name: "Gemini 累计取最后", protocol: ProtocolGemini, stream: gemini, want: Usage{Input: 96, CacheRead: 4, Output: 22}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			col := newUsageCollector(c.protocol, true)
			// 故意按 7 字节切片喂入：验证跨 chunk 的行拼接。
			b := []byte(c.stream)
			for i := 0; i < len(b); i += 7 {
				end := i + 7
				if end > len(b) {
					end = len(b)
				}
				col.Feed(b[i:end])
			}
			col.finish()
			got, ok := col.Result()
			if !ok {
				t.Fatalf("未取到 usage")
			}
			if got != c.want {
				t.Fatalf("usage = %+v, 期望 %+v", got, c.want)
			}
		})
	}
}

// TestUsageCollector_NonStreamCap 断言非流式超过采集上限即放弃解析（绝不影响透传）。
func TestUsageCollector_NonStreamCap(t *testing.T) {
	col := newUsageCollector(ProtocolOpenAIChat, false)
	chunk := make([]byte, 1<<20)
	col.Feed(chunk)
	col.Feed(chunk)
	col.Feed(chunk)
	col.Feed(chunk)
	col.Feed(chunk) // 超过 4MB
	col.Feed([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	col.finish()
	if _, ok := col.Result(); ok {
		t.Fatal("超限后不应再解析出 usage")
	}
}