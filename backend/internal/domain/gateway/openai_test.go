package gateway

import (
	"testing"

	"github.com/team/llmgateway/internal/domain/billing"
)

// TestParseUsageObj_Reasoning 断言推理从 completion 中剔除，且畸形上报（reasoning>completion）被 clamp。
func TestParseUsageObj_Reasoning(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want billing.Usage
	}{
		{
			name: "推理从输出剥离",
			raw:  `{"prompt_tokens":1000,"completion_tokens":100,"total_tokens":1100,"prompt_tokens_details":{"cached_tokens":100},"completion_tokens_details":{"reasoning_tokens":40}}`,
			want: billing.Usage{Input: 1000, Output: 60, CacheRead: 100, Reasoning: 40},
		},
		{
			name: "推理越界clamp到output",
			raw:  `{"prompt_tokens":100,"completion_tokens":30,"total_tokens":130,"completion_tokens_details":{"reasoning_tokens":50}}`,
			want: billing.Usage{Input: 100, Output: 0, Reasoning: 30},
		},
		{
			name: "无缓存字段",
			raw:  `{"prompt_tokens":1000,"completion_tokens":200,"total_tokens":1200}`,
			want: billing.Usage{Input: 1000, Output: 200},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseUsageObj([]byte(tc.raw))
			if err != nil {
				t.Fatalf("parseUsageObj: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestExtractUsage_Empty 断言响应体无 usage 时返回零值且不报错。
func TestExtractUsage_Empty(t *testing.T) {
	p := NewOpenAIProvider(EndpointChatCompletions)
	u, err := p.ExtractUsage([]byte(`{"id":"x","object":"chat.completion"}`))
	if err != nil {
		t.Fatalf("ExtractUsage: %v", err)
	}
	want := billing.Usage{}
	if u != want {
		t.Fatalf("got %+v, want %+v", u, want)
	}
}