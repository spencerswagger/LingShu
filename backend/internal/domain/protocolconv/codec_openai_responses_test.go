package protocolconv

import (
	"reflect"
	"testing"
)

func TestOpenAIResponsesCodec(t *testing.T) {
	codec := openaiResponsesCodec{}

	requests := []struct {
		name string
		body string
	}{
		{
			name: "string_input_with_instructions",
			body: `{
				"model": "gpt-4o",
				"instructions": "You are a helpful assistant.",
				"input": "Hello there",
				"temperature": 0.4,
				"top_p": 0.8,
				"max_output_tokens": 128,
				"stream": false,
				"user": "u-123"
			}`,
		},
		{
			name: "array_input_parts",
			body: `{
				"model": "gpt-4o",
				"input": [
					{"type": "message", "role": "user", "content": [
						{"type": "input_text", "text": "What is in this image?"},
						{"type": "input_image", "image_url": "https://example.com/cat.png"}
					]},
					{"type": "message", "role": "assistant", "content": [
						{"type": "output_text", "text": "A cat."}
					]}
				]
			}`,
		},
		{
			name: "function_tools_call_and_output",
			body: `{
				"model": "gpt-4o",
				"input": [
					{"type": "message", "role": "user", "content": [
						{"type": "input_text", "text": "What's the weather in Paris?"}
					]},
					{"type": "function_call", "name": "get_weather", "arguments": "{\"city\":\"Paris\"}", "call_id": "call_1"},
					{"type": "function_call_output", "call_id": "call_1", "output": "sunny, 25C"}
				],
				"tools": [
					{"type": "function", "name": "get_weather", "description": "Get the weather", "parameters": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}
				],
				"tool_choice": {"type": "function", "name": "get_weather"}
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

	t.Run("response/message_and_function_call", func(t *testing.T) {
		body := `{
			"id": "resp_abc",
			"model": "gpt-4o",
			"status": "completed",
			"output": [
				{"type": "message", "role": "assistant", "content": [
					{"type": "output_text", "text": "Let me check the weather."}
				]},
				{"type": "function_call", "name": "get_weather", "arguments": "{\"city\":\"Paris\"}", "call_id": "call_1"}
			],
			"usage": {"input_tokens": 12, "output_tokens": 5, "total_tokens": 17, "input_tokens_details": {"cached_tokens": 2}}
		}`
		resp1, _, err := codec.ParseResponse([]byte(body))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if resp1.FinishReason != "tool_calls" {
			t.Fatalf("unexpected finish reason: %q", resp1.FinishReason)
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

	t.Run("response/reasoning_incomplete", func(t *testing.T) {
		body := `{
			"id": "resp_reason",
			"model": "gpt-4o",
			"status": "incomplete",
			"incomplete_details": {"reason": "max_output_tokens"},
			"output": [
				{"type": "reasoning", "summary": [
					{"type": "summary_text", "text": "Let me think."},
					{"type": "summary_text", "text": "Step two."}
				]},
				{"type": "message", "role": "assistant", "content": [
					{"type": "output_text", "text": "Partial answer."}
				]}
			],
			"usage": {"input_tokens": 20, "output_tokens": 30, "total_tokens": 50, "input_tokens_details": {"cached_tokens": 4}}
		}`
		resp1, _, err := codec.ParseResponse([]byte(body))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if resp1.FinishReason != "max_output_tokens" {
			t.Fatalf("unexpected finish reason: %q", resp1.FinishReason)
		}
		if resp1.Usage.Input != 16 || resp1.Usage.CacheRead != 4 || resp1.Usage.Output != 30 {
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
}
