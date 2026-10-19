package protocolconv

import (
	"reflect"
	"strconv"
	"strings"
	"testing"
)

const openAIChatStreamSample = `data: {"id":"chatcmpl-1","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":""},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"content":"Hello "},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"content":"world"},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather","arguments":""}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Paris\"}"}}]},"finish_reason":null}]}

data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":7,"total_tokens":19}}

data: [DONE]
`

const anthropicStreamSample = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude","content":[],"usage":{"input_tokens":12,"output_tokens":0,"cache_read_input_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello "}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"Paris\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}
`

const openAIResponsesStreamSample = `event: response.output_item.added
data: {"type":"response.output_item.added","output_index":0,"item":{"type":"message","role":"assistant"}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"delta":"Hello "}

event: response.output_text.delta
data: {"type":"response.output_text.delta","output_index":0,"delta":"world"}

event: response.output_item.added
data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","name":"get_weather","call_id":"call_1"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"{\"city\":"}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","output_index":1,"delta":"\"Paris\"}"}

event: response.completed
data: {"type":"response.completed","response":{"status":"completed","usage":{"input_tokens":12,"output_tokens":7,"total_tokens":19}}}

data: [DONE]
`

func TestStream(t *testing.T) {
	cases := []struct {
		name   string
		codec  StreamCodec
		sample string
	}{
		{"anthropic", anthropicCodec{}, anthropicStreamSample},
		{"openai_chat", openAIChatCodec{}, openAIChatStreamSample},
		{"openai_responses", openaiResponsesCodec{}, openAIResponsesStreamSample},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			first := decodeSSE(t, tc.codec, tc.sample)
			if len(first) == 0 {
				t.Fatalf("decode produced no canonical events")
			}
			frames := encodeFrames(t, tc.codec, first)
			second := decodeFrames(t, tc.codec, frames)
			if !reflect.DeepEqual(first, second) {
				t.Fatalf("round-trip mismatch\nfirst:  %s\nsecond: %s", dumpEvents(first), dumpEvents(second))
			}
		})
	}

	t.Run("openai_chat_to_anthropic", func(t *testing.T) {
		first := decodeSSE(t, openAIChatCodec{}, openAIChatStreamSample)
		frames := encodeFrames(t, anthropicCodec{}, first)
		second := decodeFrames(t, anthropicCodec{}, frames)
		if !reflect.DeepEqual(first, second) {
			t.Fatalf("cross-protocol mismatch\nfirst:  %s\nsecond: %s", dumpEvents(first), dumpEvents(second))
		}
	})
}

func TestEmptyStopReasonNotLeaking(t *testing.T) {
	usageOnly := `{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`
	evs, _, err := openAIChatCodec{}.DecodeEvent([]byte(usageOnly))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(evs) != 1 || evs[0].Type != EventMessageDelta || !evs[0].HasUsage || evs[0].StopReason != "" {
		t.Fatalf("unexpected events: %+v", evs)
	}

	anthFrames, _, err := anthropicCodec{}.EncodeEvent(evs[0])
	if err != nil {
		t.Fatalf("encode anthropic: %v", err)
	}
	for _, f := range anthFrames {
		if strings.Contains(string(f), "stop_reason") {
			t.Fatalf("anthropic message_delta leaked empty stop_reason: %s", f)
		}
	}

	oaiFrames, _, err := openAIChatCodec{}.EncodeEvent(evs[0])
	if err != nil {
		t.Fatalf("encode openai: %v", err)
	}
	for _, f := range oaiFrames {
		if strings.Contains(string(f), "finish_reason") {
			t.Fatalf("openai chunk leaked empty finish_reason: %s", f)
		}
	}
}

func decodeSSE(t *testing.T, c StreamCodec, s string) []CanonicalEvent {
	t.Helper()
	out := []CanonicalEvent{}
	for _, payload := range sseDataPayloads(s) {
		evs, _, err := c.DecodeEvent([]byte(payload))
		if err != nil {
			t.Fatalf("decode payload %q: %v", payload, err)
		}
		out = append(out, evs...)
	}
	return out
}

func encodeFrames(t *testing.T, c StreamCodec, evs []CanonicalEvent) [][]byte {
	t.Helper()
	frames := [][]byte{}
	for _, ev := range evs {
		fs, _, err := c.EncodeEvent(ev)
		if err != nil {
			t.Fatalf("encode %+v: %v", ev, err)
		}
		frames = append(frames, fs...)
	}
	return frames
}

func decodeFrames(t *testing.T, c StreamCodec, frames [][]byte) []CanonicalEvent {
	t.Helper()
	out := []CanonicalEvent{}
	for _, f := range frames {
		for _, payload := range sseDataPayloads(string(f)) {
			evs, _, err := c.DecodeEvent([]byte(payload))
			if err != nil {
				t.Fatalf("decode frame payload %q: %v", payload, err)
			}
			out = append(out, evs...)
		}
	}
	return out
}

func sseDataPayloads(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimRight(line, "\r")
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		p := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if p == "" || p == "[DONE]" {
			continue
		}
		out = append(out, p)
	}
	return out
}

func dumpEvents(evs []CanonicalEvent) string {
	var sb strings.Builder
	for i, ev := range evs {
		if i > 0 {
			sb.WriteString(" | ")
		}
		sb.WriteString(ev.Type)
		sb.WriteString("{")
		sb.WriteString(strings.Join([]string{
			"idx=" + strconv.Itoa(ev.Index),
			"kind=" + ev.BlockKind,
			"delta=" + ev.DeltaKind,
			"text=" + ev.Text,
			"id=" + ev.ToolUseID,
			"name=" + ev.ToolName,
			"stop=" + ev.StopReason,
		}, ","))
		sb.WriteString("}")
	}
	return sb.String()
}
