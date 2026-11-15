package upstream

import "strings"

// 上游协议枚举：V2 归因列 `billing_records.upstream_protocol` 的取值域。
//
// 与迁移里的 CHECK 约束必须逐字一致（四个值，别改名、别加值）：
//   - openai-chat       /v1/chat/completions
//   - openai-responses  /v1/responses
//   - anthropic-messages /v1/messages
//   - gemini            /v1beta/models/{model}:generateContent
//
// 本期**不做** Embeddings（已批准口径）。值为空串表示「上游没给/没取到」，
// 落库留 NULL 而不是猜一个默认值。
const (
	ProtocolOpenAIChat        = "openai-chat"
	ProtocolOpenAIResponses   = "openai-responses"
	ProtocolAnthropicMessages = "anthropic-messages"
	ProtocolGemini            = "gemini"
)

// AttributionProtocols 返回协议取值域（供 CHECK 约束对齐用例与前端下拉使用）。
func AttributionProtocols() []string {
	return []string{
		ProtocolOpenAIChat,
		ProtocolOpenAIResponses,
		ProtocolAnthropicMessages,
		ProtocolGemini,
	}
}

// ValidAttributionProtocol 判断协议取值是否合法；空串合法（表示缺失 → 落 NULL）。
func ValidAttributionProtocol(v string) bool {
	if strings.TrimSpace(v) == "" {
		return true
	}
	for _, p := range AttributionProtocols() {
		if v == p {
			return true
		}
	}
	return false
}
