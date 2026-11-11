package protocolconv

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
)

// 本文件是 Anthropic thinking 块签名（signature）的生成器。
//
// Anthropic 严格要求 thinking / redacted_thinking 块携带非空 signature（否则严格客户端
// 丢弃该块，下一轮不回传 → 上游 400）。从 openai-chat 上游转换而来时上游没有签名概念
// （reasoning_content 是纯文本），灵枢需要为下发的 thinking 块生成一个稳定的不透明签名。
//
// 签名语义（有意设计，勿改成可逆/可验证）：
//   - 对客户端是不透明串：客户端只把它原样回传（anthropic→openai 请求侧我们本就丢弃
//     签名，只按 reasoning_content 回填），无需上游可验证；
//   - 稳定性：同一份推理内容生成同一签名（基于内容哈希），避免同一会话反复生成不同签名
//     导致客户端历史里同一轮出现不一致；
//   - 不可逆：哈希 + base64，不含明文，不泄漏推理内容。
//
// 仅在「目标协议为 anthropic-messages」的下发侧使用；anthropic→anthropic 透传保留原始签名。

// OpaqueThinkingSignature 为给定推理文本生成稳定的不透明签名（内容哈希 → base64）。
// 文本为空时仍生成一个固定盐值的签名（空 thinking 块同样需要签名，否则被客户端丢弃）。
func OpaqueThinkingSignature(text string) string {
	payload, _ := json.Marshal(map[string]string{"v": "1", "t": text})
	sum := sha256.Sum256(payload)
	return "lsig_" + base64.RawURLEncoding.EncodeToString(sum[:])
}
