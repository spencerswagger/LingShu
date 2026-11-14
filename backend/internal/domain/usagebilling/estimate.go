// estimate.go 是无 usage 时的用量估算（契约 §3.4「沿用 missingUsageEstimate」）。
//
// 口径与 V1 网关一致（`gateway/tokens.go` 的字符级估算 + tiktoken 计数）：
//   - 输入：优先按 messages 结构用 tiktoken 精确计数（叠加结构开销），
//     其次 responses 的 input 文本，最后按请求体字符数兜底；
//   - 输出：按旁路累计到的「生成内容」字符数估算（约 2 字符/token）。
//
// 估算只为「上游没给 usage」时兜底出账，不影响正常结算口径。
package usagebilling

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/tiktoken-go/tokenizer"
)

// promptMessagesCap 参与输入估算的最大消息条数（防超大对话拖慢预扣）。
const promptMessagesCap = 128

// EstimateInputTokens 估算请求输入 token 数。
func EstimateInputTokens(body []byte, model string) int {
	var probe struct {
		Messages json.RawMessage `json:"messages"`
		Input    json.RawMessage `json:"input"`
		// Anthropic / Gemini：contents 与 system
		Contents json.RawMessage `json:"contents"`
		System   json.RawMessage `json:"system"`
		Tools    json.RawMessage `json:"tools"`
	}
	_ = json.Unmarshal(body, &probe)

	if len(probe.Messages) > 0 && string(probe.Messages) != "null" {
		if n, err := countPromptTokens(body, model); err == nil && n > 0 {
			return n
		}
	}
	var sb strings.Builder
	for _, raw := range []json.RawMessage{probe.Input, probe.System, probe.Contents} {
		if len(raw) == 0 || string(raw) == "null" {
			continue
		}
		sb.Write(raw)
	}
	if sb.Len() > 0 {
		if n := countTextTokens(sb.String(), model); n > 0 {
			return n
		}
	}
	// 兜底：请求体整体按字符估算（含 JSON 结构开销，宁可略高）。
	return estimateRuneTokens(string(body))
}

// EstimateOutputTokens 由旁路累计的生成内容字符数估算输出 token（约 2 字符/token）。
func EstimateOutputTokens(observedRunes int) int64 {
	if observedRunes <= 0 {
		return 0
	}
	return int64((observedRunes + 1) / 2)
}

// countPromptTokens 按 messages 结构估算（tiktoken 精确计数 + 结构开销）。
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
	tkm := countTextTokens(sb.String(), model)
	if tkm == 0 {
		return 0, nil
	}
	tkm += n * 3 // 每条消息结构开销
	tkm += 3     // 请求整体开销
	tkm += len(req.Tools) * 8
	return tkm, nil
}

// countTextTokens 估算一段文本的 token 数（未知模型回退字符级）。
func countTextTokens(text, model string) int {
	if text == "" {
		return 0
	}
	if enc, err := tokenizer.ForModel(tokenizer.Model(model)); err == nil {
		if c, cerr := enc.Count(text); cerr == nil {
			return c
		}
	}
	return estimateRuneTokens(text)
}

// estimateRuneTokens 字符级估算（约 2 字符/token）。
func estimateRuneTokens(text string) int {
	if text == "" {
		return 0
	}
	return (utf8.RuneCountInString(text) + 1) / 2
}
