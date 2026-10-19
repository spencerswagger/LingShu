package protocolconv

import (
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

var matrixProtocols = []string{
	channelprotocol.ProtocolOpenAIChat,
	channelprotocol.ProtocolOpenAIResponses,
	channelprotocol.ProtocolAnthropicMessages,
}

const matrixToolName = "get_weather"

var matrixRequestSamples = map[string]string{
	channelprotocol.ProtocolOpenAIChat: `{
		"model": "gpt-x",
		"messages": [
			{"role": "system", "content": "sys"},
			{"role": "user", "content": "hello"}
		],
		"tools": [{"type": "function", "function": {"name": "get_weather", "parameters": {"type": "object"}}}]
	}`,
	channelprotocol.ProtocolOpenAIResponses: `{
		"model": "gpt-x",
		"instructions": "sys",
		"input": [{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "hello"}]}],
		"tools": [{"type": "function", "name": "get_weather", "parameters": {"type": "object"}}]
	}`,
	channelprotocol.ProtocolAnthropicMessages: `{
		"model": "gpt-x",
		"system": "sys",
		"max_tokens": 128,
		"messages": [{"role": "user", "content": "hello"}],
		"tools": [{"name": "get_weather", "input_schema": {"type": "object"}}]
	}`,
}

var matrixResponseSamples = map[string]string{
	channelprotocol.ProtocolOpenAIChat: `{
		"id": "c1",
		"model": "m",
		"choices": [{"index": 0, "message": {"role": "assistant", "content": "hi"}, "finish_reason": "length"}]
	}`,
	channelprotocol.ProtocolOpenAIResponses: `{
		"id": "r1",
		"object": "response",
		"model": "m",
		"status": "incomplete",
		"incomplete_details": {"reason": "max_output_tokens"},
		"output": [{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "hi"}]}]
	}`,
	channelprotocol.ProtocolAnthropicMessages: `{
		"id": "msg1",
		"type": "message",
		"role": "assistant",
		"model": "m",
		"content": [{"type": "text", "text": "hi"}],
		"stop_reason": "max_tokens"
	}`,
}

var matrixStreamEvents = []CanonicalEvent{
	{Type: EventContentBlockDelta, Index: 0, DeltaKind: DeltaText, Text: "hello stream"},
	{Type: EventMessageDelta, StopReason: stopReasonStop, Usage: billing.Usage{Input: 12, Output: 7}, HasUsage: true},
}

func TestMatrix(t *testing.T) {
	for _, from := range matrixProtocols {
		for _, to := range matrixProtocols {
			if from == to {
				continue
			}
			from, to := from, to
			name := from + "_to_" + to

			t.Run("request/"+name, func(t *testing.T) {
				fromReq, _, err := mustCodec(t, from).ParseRequest([]byte(matrixRequestSamples[from]))
				if err != nil {
					t.Fatalf("parse source request: %v", err)
				}

				out, _, err := ConvertRequest(from, to, []byte(matrixRequestSamples[from]))
				if err != nil {
					t.Fatalf("ConvertRequest: %v", err)
				}
				gotReq, _, err := mustCodec(t, to).ParseRequest(out)
				if err != nil {
					t.Fatalf("parse converted request: %v\nbody=%s", err, out)
				}

				if gotReq.Model != fromReq.Model {
					t.Fatalf("model mismatch: got %q want %q", gotReq.Model, fromReq.Model)
				}
				if got, want := firstMessageText(gotReq), firstMessageText(fromReq); got != want {
					t.Fatalf("message text mismatch: got %q want %q", got, want)
				}
				if got, want := firstToolName(gotReq), firstToolName(fromReq); got != want {
					t.Fatalf("tool name mismatch: got %q want %q", got, want)
				}
				assertTargetRequestShape(t, to, out)
			})

			t.Run("response/"+name, func(t *testing.T) {
				fromResp, _, err := mustCodec(t, from).ParseResponse([]byte(matrixResponseSamples[from]))
				if err != nil {
					t.Fatalf("parse source response: %v", err)
				}
				wantCanonical := stopReasonToCanonical(from, fromResp.FinishReason)

				out, _, err := ConvertResponse(from, to, []byte(matrixResponseSamples[from]))
				if err != nil {
					t.Fatalf("ConvertResponse: %v", err)
				}
				gotResp, _, err := mustCodec(t, to).ParseResponse(out)
				if err != nil {
					t.Fatalf("parse converted response: %v\nbody=%s", err, out)
				}

				if got := stopReasonToCanonical(to, gotResp.FinishReason); got != wantCanonical {
					t.Fatalf("finish reason mismatch: got %q want %q (native=%q)", got, wantCanonical, gotResp.FinishReason)
				}
				if got, want := responseText(gotResp), responseText(fromResp); got != want {
					t.Fatalf("response text mismatch: got %q want %q", got, want)
				}
				if to == channelprotocol.ProtocolOpenAIResponses {
					if !strings.Contains(string(out), `"incomplete_details":{"reason":"max_output_tokens"}`) {
						t.Fatalf("responses target missing incomplete_details.reason=max_output_tokens: %s", out)
					}
				}
			})

			t.Run("stream/"+name, func(t *testing.T) {
				toCodec := mustCodec(t, to)
				for _, ev := range matrixStreamEvents {
					frames, _, err := mustCodec(t, from).EncodeEvent(ev)
					if err != nil {
						t.Fatalf("encode source event %+v: %v", ev, err)
					}
					for _, fr := range frames {
						for _, payload := range sseDataPayloads(string(fr)) {
							events, decRep, err := DecodeStreamEvents(from, []byte(payload))
							if err != nil {
								t.Fatalf("DecodeStreamEvents: %v", err)
							}
							outFrames, encRep, err := EncodeStreamEvents(to, events)
							if err != nil {
								t.Fatalf("EncodeStreamEvents: %v", err)
							}
							rep := &LossyReport{}
							rep.Merge(decRep)
							rep.Merge(encRep)
							if !rep.IsEmpty() {
								t.Fatalf("unexpected lossy report: %+v", rep)
							}
							got := decodeFrames(t, toCodec, outFrames)
							if ev.Type == EventContentBlockDelta && eventText(got) != ev.Text {
								t.Fatalf("text not preserved: got %q want %q", eventText(got), ev.Text)
							}
							if ev.HasUsage {
								usage, ok := eventUsage(got)
								if !ok || usage != ev.Usage {
									t.Fatalf("usage not preserved: got %+v ok=%v want %+v", usage, ok, ev.Usage)
								}
							}
						}
					}
				}
			})
		}
	}
}

func TestSelectUpstreamProtocol(t *testing.T) {
	t.Run("preference_order", func(t *testing.T) {
		got, ok := SelectUpstreamProtocol(
			channelprotocol.ProtocolOpenAIResponses,
			[]string{channelprotocol.ProtocolOpenAIResponses, channelprotocol.ProtocolAnthropicMessages},
			nil,
		)
		if !ok || got != channelprotocol.ProtocolAnthropicMessages {
			t.Fatalf("got (%q,%v), want anthropic-messages", got, ok)
		}
	})

	t.Run("override_wins", func(t *testing.T) {
		got, ok := SelectUpstreamProtocol(
			channelprotocol.ProtocolOpenAIResponses,
			[]string{channelprotocol.ProtocolOpenAIChat, channelprotocol.ProtocolAnthropicMessages},
			[]string{channelprotocol.ProtocolAnthropicMessages},
		)
		if !ok || got != channelprotocol.ProtocolAnthropicMessages {
			t.Fatalf("got (%q,%v), want anthropic-messages", got, ok)
		}
	})

	t.Run("override_skips_unsupported", func(t *testing.T) {
		got, ok := SelectUpstreamProtocol(
			channelprotocol.ProtocolOpenAIChat,
			[]string{channelprotocol.ProtocolOpenAIResponses},
			[]string{channelprotocol.ProtocolAnthropicMessages, channelprotocol.ProtocolOpenAIResponses},
		)
		if !ok || got != channelprotocol.ProtocolOpenAIResponses {
			t.Fatalf("got (%q,%v), want openai-responses", got, ok)
		}
	})

	t.Run("empty_supported", func(t *testing.T) {
		if got, ok := SelectUpstreamProtocol(channelprotocol.ProtocolOpenAIChat, nil, nil); ok || got != "" {
			t.Fatalf("got (%q,%v), want (\"\",false)", got, ok)
		}
	})

	t.Run("skip_inbound_only_supported", func(t *testing.T) {
		if got, ok := SelectUpstreamProtocol(channelprotocol.ProtocolOpenAIChat, []string{channelprotocol.ProtocolOpenAIChat}, nil); ok || got != "" {
			t.Fatalf("got (%q,%v), want (\"\",false)", got, ok)
		}
	})

	t.Run("override_skip_inbound", func(t *testing.T) {
		if got, ok := SelectUpstreamProtocol(
			channelprotocol.ProtocolAnthropicMessages,
			[]string{channelprotocol.ProtocolAnthropicMessages},
			[]string{channelprotocol.ProtocolAnthropicMessages},
		); ok || got != "" {
			t.Fatalf("got (%q,%v), want (\"\",false)", got, ok)
		}
	})
}

func assertTargetRequestShape(t *testing.T, protocol string, body []byte) {
	t.Helper()
	s := string(body)
	switch protocol {
	case channelprotocol.ProtocolAnthropicMessages:
		if !strings.Contains(s, `"max_tokens"`) {
			t.Fatalf("anthropic target body missing max_tokens: %s", s)
		}
	case channelprotocol.ProtocolOpenAIResponses:
		if !strings.Contains(s, `"input"`) || !strings.Contains(s, `"instructions"`) {
			t.Fatalf("responses target body missing input/instructions: %s", s)
		}
	case channelprotocol.ProtocolOpenAIChat:
		if !strings.Contains(s, `"messages"`) {
			t.Fatalf("openai-chat target body missing messages: %s", s)
		}
	}
}

func TestConvertBase64ImageRoundTrip(t *testing.T) {
	const b64 = "aGVsbG8="

	openaiBody := `{
		"model": "gpt-x",
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "what is this?"},
				{"type": "image_url", "image_url": {"url": "data:image/png;base64,aGVsbG8="}}
			]}
		]
	}`
	anthropicOut, _, err := ConvertRequest(channelprotocol.ProtocolOpenAIChat, channelprotocol.ProtocolAnthropicMessages, []byte(openaiBody))
	if err != nil {
		t.Fatalf("openai->anthropic: %v", err)
	}
	if !strings.Contains(string(anthropicOut), `"type":"base64"`) || !strings.Contains(string(anthropicOut), b64) {
		t.Fatalf("anthropic body missing base64 image: %s", anthropicOut)
	}
	reqA, _, err := anthropicCodec{}.ParseRequest(anthropicOut)
	if err != nil {
		t.Fatalf("reparse anthropic: %v", err)
	}
	if img := findImageBlock(reqA); img == nil || img.ImageB64 != b64 || img.MediaType != "image/png" {
		t.Fatalf("anthropic image mismatch: %+v", img)
	}

	anthropicBody := `{
		"model": "gpt-x",
		"max_tokens": 128,
		"messages": [
			{"role": "user", "content": [
				{"type": "text", "text": "what is this?"},
				{"type": "image", "source": {"type": "base64", "media_type": "image/png", "data": "aGVsbG8="}}
			]}
		]
	}`
	openaiOut, _, err := ConvertRequest(channelprotocol.ProtocolAnthropicMessages, channelprotocol.ProtocolOpenAIChat, []byte(anthropicBody))
	if err != nil {
		t.Fatalf("anthropic->openai: %v", err)
	}
	if !strings.Contains(string(openaiOut), "data:image/png;base64,"+b64) {
		t.Fatalf("openai body missing data url image: %s", openaiOut)
	}
	reqO, _, err := openAIChatCodec{}.ParseRequest(openaiOut)
	if err != nil {
		t.Fatalf("reparse openai: %v", err)
	}
	if img := findImageBlock(reqO); img == nil || img.ImageB64 != b64 || img.MediaType != "image/png" {
		t.Fatalf("openai image mismatch: %+v", img)
	}
}

func findImageBlock(req *CanonicalRequest) *ContentBlock {
	for i := range req.Messages {
		for j := range req.Messages[i].Blocks {
			if req.Messages[i].Blocks[j].Kind == BlockImage {
				return &req.Messages[i].Blocks[j]
			}
		}
	}
	return nil
}

func TestConvertAdjacentUserMergeToAnthropic(t *testing.T) {
	body := `{
		"model": "gpt-x",
		"messages": [
			{"role": "user", "content": "first"},
			{"role": "user", "content": "second"}
		]
	}`
	out, _, err := ConvertRequest(channelprotocol.ProtocolOpenAIChat, channelprotocol.ProtocolAnthropicMessages, []byte(body))
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	req, _, err := anthropicCodec{}.ParseRequest(out)
	if err != nil {
		t.Fatalf("reparse: %v\nbody=%s", err, out)
	}
	prev := ""
	for _, m := range req.Messages {
		if m.Role == prev {
			t.Fatalf("adjacent same role %q not merged: %s", m.Role, out)
		}
		prev = m.Role
	}
	if len(req.Messages) != 1 || req.Messages[0].Role != anthropicRoleUser {
		t.Fatalf("expected one merged user message, got %+v (body=%s)", req.Messages, out)
	}
}

func TestResponsesFinishReasonVocabulary(t *testing.T) {
	body := `{"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"length"}]}`
	out, _, err := ConvertResponse(channelprotocol.ProtocolOpenAIChat, channelprotocol.ProtocolOpenAIResponses, []byte(body))
	if err != nil {
		t.Fatalf("ConvertResponse: %v", err)
	}
	if !strings.Contains(string(out), `"incomplete_details":{"reason":"max_output_tokens"}`) {
		t.Fatalf("expected incomplete_details.reason=max_output_tokens, got %s", out)
	}
	resp, _, err := openaiResponsesCodec{}.ParseResponse(out)
	if err != nil {
		t.Fatalf("parse responses: %v", err)
	}
	if resp.FinishReason != "max_output_tokens" {
		t.Fatalf("responses finish reason = %q", resp.FinishReason)
	}
	if got := stopReasonToCanonical(channelprotocol.ProtocolOpenAIResponses, resp.FinishReason); got != stopReasonLength {
		t.Fatalf("canonical finish reason = %q, want %q", got, stopReasonLength)
	}
	if got := stopReasonFromCanonical(channelprotocol.ProtocolOpenAIResponses, stopReasonLength, &LossyReport{}); got != "max_output_tokens" {
		t.Fatalf("native length = %q, want max_output_tokens", got)
	}
}

func mustCodec(t *testing.T, protocol string) Codec {
	t.Helper()
	c, ok := codecFor(protocol)
	if !ok {
		t.Fatalf("no codec for protocol %q", protocol)
	}
	return c
}

func firstMessageText(req *CanonicalRequest) string {
	for _, m := range req.Messages {
		if txt := blocksText(m.Blocks); txt != "" {
			return txt
		}
	}
	return ""
}

func firstToolName(req *CanonicalRequest) string {
	if len(req.Tools) == 0 {
		return ""
	}
	return req.Tools[0].Name
}

func responseText(resp *CanonicalResponse) string {
	return blocksText(resp.Blocks)
}

func eventText(evs []CanonicalEvent) string {
	var sb strings.Builder
	for _, ev := range evs {
		if ev.Type == EventContentBlockDelta && ev.DeltaKind == DeltaText {
			sb.WriteString(ev.Text)
		}
	}
	return sb.String()
}

func eventUsage(evs []CanonicalEvent) (billing.Usage, bool) {
	for _, ev := range evs {
		if ev.HasUsage {
			return ev.Usage, true
		}
	}
	return billing.Usage{}, false
}
