package protocolconv

import (
	"reflect"
	"strings"
	"testing"
)

func TestAnthropicSerializeRequestMaxTokensDefault(t *testing.T) {
	codec := anthropicCodec{}

	req, _, err := codec.ParseRequest([]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if req.MaxTokens != nil {
		t.Fatalf("expected nil max_tokens, got %v", *req.MaxTokens)
	}
	out, _, err := codec.SerializeRequest(req)
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	if !strings.Contains(string(out), `"max_tokens"`) {
		t.Fatalf("serialized anthropic body missing max_tokens: %s", out)
	}

	explicit := int64(99)
	req.MaxTokens = &explicit
	out, _, err = codec.SerializeRequest(req)
	if err != nil {
		t.Fatalf("serialize explicit: %v", err)
	}
	if !strings.Contains(string(out), `"max_tokens":99`) {
		t.Fatalf("serialized anthropic body missing explicit max_tokens: %s", out)
	}
}

func TestAnthropicCodec(t *testing.T) {
	codec := anthropicCodec{}

	requests := []struct {
		name string
		body string
	}{
		{
			name: "plain_text_system_string",
			body: `{
				"model": "claude-3-5-sonnet-20241022",
				"system": "You are a helpful assistant.",
				"messages": [
					{"role": "user", "content": "Hello there"},
					{"role": "assistant", "content": "Hi! How can I help?"},
					{"role": "user", "content": "Tell me a joke."}
				],
				"max_tokens": 256,
				"temperature": 0.5,
				"top_p": 0.9,
				"stop_sequences": ["END"],
				"stream": false,
				"metadata": {"user_id": "u-123"}
			}`,
		},
		{
			name: "system_blocks_and_image",
			body: `{
				"model": "claude-3-5-sonnet-20241022",
				"system": [
					{"type": "text", "text": "Line one."},
					{"type": "text", "text": "Line two."}
				],
				"max_tokens": 300,
				"messages": [
					{"role": "user", "content": [
						{"type": "text", "text": "What is in this image?"},
						{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "aGVsbG8="}}
					]}
				]
			}`,
		},
		{
			name: "tools_and_tool_result",
			body: `{
				"model": "claude-3-5-sonnet-20241022",
				"max_tokens": 512,
				"tools": [
					{"name": "get_weather", "description": "Get the weather", "input_schema": {"type": "object", "properties": {"city": {"type": "string"}}, "required": ["city"]}}
				],
				"tool_choice": {"type": "auto"},
				"messages": [
					{"role": "user", "content": "What's the weather in Paris?"},
					{"role": "assistant", "content": [
						{"type": "text", "text": "Let me check."},
						{"type": "tool_use", "id": "toolu_1", "name": "get_weather", "input": {"city": "Paris"}}
					]},
					{"role": "user", "content": [
						{"type": "tool_result", "tool_use_id": "toolu_1", "content": "sunny, 25C"}
					]}
				]
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

	t.Run("response/text_thinking_tool_use", func(t *testing.T) {
		body := `{
			"id": "msg_123",
			"type": "message",
			"role": "assistant",
			"model": "claude-3-5-sonnet-20241022",
			"content": [
				{"type": "thinking", "thinking": "Let me think...", "signature": "sig-abc"},
				{"type": "text", "text": "Here is the answer."},
				{"type": "tool_use", "id": "toolu_2", "name": "get_weather", "input": {"city": "Rome"}}
			],
			"stop_reason": "tool_use",
			"usage": {
				"input_tokens": 10,
				"cache_creation_input_tokens": 3,
				"cache_read_input_tokens": 2,
				"output_tokens": 20
			}
		}`
		resp1, _, err := codec.ParseResponse([]byte(body))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if resp1.Usage.Input != 13 || resp1.Usage.CacheRead != 2 || resp1.Usage.Output != 20 {
			t.Fatalf("unexpected usage: %+v", resp1.Usage)
		}
		if resp1.FinishReason != "tool_use" {
			t.Fatalf("unexpected finish reason: %q", resp1.FinishReason)
		}
		if len(resp1.Blocks) != 3 || resp1.Blocks[0].Kind != BlockThinking || resp1.Blocks[2].Kind != BlockToolUse {
			t.Fatalf("unexpected blocks: %+v", resp1.Blocks)
		}
		out, _, err := codec.SerializeResponse(resp1)
		if err != nil {
			t.Fatalf("serialize: %v", err)
		}
		resp2, _, err := codec.ParseResponse(out)
		if err != nil {
			t.Fatalf("reparse: %v", err)
		}
		// cache_creation_input_tokens 已并入 input_tokens（SerializeResponse 附有损提示），
		// 反序列化后无法还原该区分；对齐该瞬态字段后再整体等价断言。
		resp2.CacheCreation = resp1.CacheCreation
		if !reflect.DeepEqual(resp1, resp2) {
			t.Fatalf("round trip mismatch\nresp1=%+v\nresp2=%+v\nserialized=%s", resp1, resp2, out)
		}
	})

	t.Run("lossy/cache_control", func(t *testing.T) {
		body := `{
			"model": "claude-3-5-sonnet-20241022",
			"max_tokens": 100,
			"messages": [
				{"role": "user", "content": [
					{"type": "text", "text": "hi", "cache_control": {"type": "ephemeral"}}
				]}
			]
		}`
		req, rep, err := codec.ParseRequest([]byte(body))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		found := false
		for _, d := range rep.Dropped {
			if d == "cache_control" {
				found = true
			}
		}
		if !found {
			t.Fatalf("expected cache_control lossy marker, got %+v", rep)
		}
		out, _, err := codec.SerializeRequest(req)
		if err != nil {
			t.Fatalf("serialize: %v", err)
		}
		req2, _, err := codec.ParseRequest(out)
		if err != nil {
			t.Fatalf("reparse: %v", err)
		}
		if !reflect.DeepEqual(req, req2) {
			t.Fatalf("round trip mismatch\nreq1=%+v\nreq2=%+v", req, req2)
		}
	})
}
