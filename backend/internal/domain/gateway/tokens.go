package gateway

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/tiktoken-go/tokenizer"
)

// promptMessagesCap 参与 prompt 估算的最大消息条数（防超大对话拖慢预扣）。
const promptMessagesCap = 128

// countPromptTokens 估算请求 prompt token 数（对齐 new-api service/token_counter.go）：
//   - OpenAI 系模型：tiktoken 精确计数（未知模型回退字符级估算）；
//   - 叠加结构开销：每条消息 +3、请求整体 +3、每个 tool +8。
func countPromptTokens(body []byte, model string) (int, error) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return 0, err
	}
	n := len(req.Messages)
	if n > promptMessagesCap {
		n = promptMessagesCap
	}
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(req.Messages[i].Role)
		sb.WriteByte(':')
		sb.Write(req.Messages[i].Content)
		sb.WriteByte('|')
	}
	text := sb.String()

	var tkm int
	if enc, err := tokenizer.ForModel(tokenizer.Model(model)); err == nil {
		if c, cerr := enc.Count(text); cerr == nil {
			tkm = c
		} else {
			tkm = estimateRuneTokens(text)
		}
	} else {
		tkm = estimateRuneTokens(text)
	}
	tkm += n * 3 // 每条消息结构开销
	tkm += 3     // 请求整体开销
	tkm += len(req.Tools) * 8
	return tkm, nil
}

// estimateRuneTokens 非 tiktoken 模型的字符级估算（约 2 字符/token）。
func estimateRuneTokens(text string) int {
	return (utf8.RuneCountInString(text) + 1) / 2
}
