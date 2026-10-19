package protocolconv

import (
	"reflect"
	"strings"
	"testing"
)

func TestOpenAIChatCodec(t *testing.T) {
	codec := openAIChatCodec{}

	requests := []struct {
		name string
		body string
	}{
		{
			name: "plain_text",
			body: `{
				"model": "gpt-4o",
				"messages": [
					{"role": "system", "content": "You are a helpful assistant."},
					{"role": "user", "content": "Hello there"},
					{"role": "assistant", "content": "Hi! How can I help?"},
					{"role": "user", "content": "Tell me a joke."}
				],
				"temperature": 0.5,
				"top_p": 0.9,
				"max_tokens": 256,
				"stop": "END",
				"stream": false,
				"user": "u-123"
			}`,
		},
		{
			name: "multimodal",
			body: `{
				"model": "gpt-4o",
				"messages": [
					{"role": "system", "content": "Describe images."},
					{"role": "user", "content": [
						{"type": "text", "text": "What is in this image?"},
						{"type": "image_url", "image_url": {"url": "https://example.com/cat.png"}}
					]}
				]
			}`,
		},
		{
			name: "tools",
			body: `{
				"model": "gpt-4o",
				"messages": [
					{"role": "user", "content": "What's the weather in Paris?"},
					{"role": "assistant", "content": null, "tool_calls": [
						{"id": "call_1", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}}
					]},
					{"role": "tool", "tool_call_id": "call_1", "content": "sunny, 25C"}
				],
				"tools": [
					{"type": "function", "function": {"name": "get_weather", "description": "Get the weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}}
				],
				"tool_choice": "auto"
			}`,
		},
	}

	for _, tc := range requests {
		t.Run("request/"+tc.name, func(t *testing.T) {
			req1, _, err := codec.ParseRequest([]byte(tc.body))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			out, _, err := codec.SerializeRequest(req1)
			if err != nil {
				t.Fatalf("serialize: %v", err)
			}
			req2, _, err := codec.ParseRequest(out)
			if err != nil {
				t.Fatalf("reparse: %v", err)
			}
			if !reflect.DeepEqual(req1, req2) {
				t.Fatalf("round trip mismatch\nreq1=%+v\nreq2=%+v\nserialized=%s", req1, req2, out)
			}
		})
	}

	t.Run("response/text", func(t *testing.T) {
		body := `{
			"id": "chatcmpl-abc",
			"object": "chat.completion",
			"model": "gpt-4o",
			"choices": [
				{"index": 0, "message": {"role": "assistant", "content": "Hello!"}, "finish_reason": "stop"}
			],
			"usage": {"prompt_tokens": 12, "completion_tokens": 5, "total_tokens": 17, "prompt_tokens_details": {"cached_tokens": 2}}
		}`
		resp1, _, err := codec.ParseResponse([]byte(body))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if resp1.Usage.Input != 10 || resp1.Usage.CacheRead != 2 || resp1.Usage.Output != 5 {
			t.Fatalf("unexpected usage: %+v", resp1.Usage)
		}
		out, _, err := codec.SerializeResponse(resp1)
		if err != nil {
			t.Fatalf("serialize: %v", err)
		}
		resp2, _, err := codec.ParseResponse(out)
		if err != nil {
			t.Fatalf("reparse: %v", err)
		}
		if !reflect.DeepEqual(resp1, resp2) {
			t.Fatalf("round trip mismatch\nresp1=%+v\nresp2=%+v\nserialized=%s", resp1, resp2, out)
		}
	})

	t.Run("response/tool_calls", func(t *testing.T) {
		body := `{
			"id": "chatcmpl-def",
			"object": "chat.completion",
			"model": "gpt-4o",
			"choices": [
				{"index": 0, "message": {"role": "assistant", "content": null, "tool_calls": [
					{"id": "call_9", "type": "function", "function": {"name": "get_weather", "arguments": "{\"city\":\"Paris\"}"}}
				]}, "finish_reason": "tool_calls"}
			],
			"usage": {"prompt_tokens": 8, "completion_tokens": 3, "prompt_cache_hit_tokens": 4}
		}`
		resp1, _, err := codec.ParseResponse([]byte(body))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if resp1.Usage.Input != 4 || resp1.Usage.CacheRead != 4 || resp1.Usage.Output != 3 {
			t.Fatalf("unexpected usage: %+v", resp1.Usage)
		}
		out, _, err := codec.SerializeResponse(resp1)
		if err != nil {
			t.Fatalf("serialize: %v", err)
		}
		resp2, _, err := codec.ParseResponse(out)
		if err != nil {
			t.Fatalf("reparse: %v", err)
		}
		if !reflect.DeepEqual(resp1, resp2) {
			t.Fatalf("round trip mismatch\nresp1=%+v\nresp2=%+v\nserialized=%s", resp1, resp2, out)
		}
	})

	t.Run("response/reasoning_content", func(t *testing.T) {
		body := `{
			"id": "chatcmpl-ghi",
			"object": "chat.completion",
			"model": "gpt-4o",
			"choices": [
				{"index": 0, "message": {"role": "assistant", "reasoning_content": "Let me think.", "content": "Hello!"}, "finish_reason": "stop"}
			],
			"usage": {"prompt_tokens": 4, "completion_tokens": 2, "total_tokens": 6}
		}`
		resp1, _, err := codec.ParseResponse([]byte(body))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(resp1.Blocks) != 2 || resp1.Blocks[0].Kind != BlockThinking || resp1.Blocks[0].Text != "Let me think." {
			t.Fatalf("expected leading thinking block, got %+v", resp1.Blocks)
		}
		if resp1.Blocks[1].Kind != BlockText || resp1.Blocks[1].Text != "Hello!" {
			t.Fatalf("expected trailing text block, got %+v", resp1.Blocks)
		}
		out, _, err := codec.SerializeResponse(resp1)
		if err != nil {
			t.Fatalf("serialize: %v", err)
		}
		if !strings.Contains(string(out), `"reasoning_content":"Let me think."`) {
			t.Fatalf("serialized chat response missing reasoning_content: %s", out)
		}
		if !strings.Contains(string(out), `"text":"Hello!"`) {
			t.Fatalf("serialized chat response suffered content regression: %s", out)
		}
		if strings.Contains(string(out), `"Let me think."`) && strings.Contains(string(out), `"content":"Let me think."`) {
			t.Fatalf("thinking text leaked into content: %s", out)
		}
		resp2, _, err := codec.ParseResponse(out)
		if err != nil {
			t.Fatalf("reparse: %v", err)
		}
		if !reflect.DeepEqual(resp1, resp2) {
			t.Fatalf("round trip mismatch\nresp1=%+v\nresp2=%+v\nserialized=%s", resp1, resp2, out)
		}
	})
}
