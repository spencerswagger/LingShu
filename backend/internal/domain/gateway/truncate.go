package gateway

import "github.com/team/llmgateway/internal/pkg/errbody"

// truncateUTF8 按 **UTF-8 字符边界**把 s 截断到不超过 maxBytes 字节（含 suffix），
// 返回 (结果, 是否发生截断)。
//
// 为什么必须按 rune 边界切：`s[:n]` 是**按字节**切，多字节字符（中文 3 字节、emoji 4 字节、
// 组合 emoji 更多）被切成半个时，encoding/json 会把非法字节替换成 U+FFFD（�）——
// 用户看到的快照就是花的（PR #63 Gate 复审 P3 建议项）。
//
// 约定：
//   - 结果字节数 **≤ maxBytes**（suffix 计入预算，不会因加后缀而超限）；
//   - 结果永远是合法 UTF-8（不含半个字符、不含 U+FFFD）；
//   - 未截断时原样返回（零分配、零差异）；
//   - suffix 本身比 maxBytes 还长时只保留 suffix 的合法前缀（极端防御，不返回超限串）。
//
// 实现收口在 internal/pkg/errbody：渠道探活（channel 包）同样要截断上游原文，
// 两边共用一套，避免出现「按字节切」的第二套实现。
func truncateUTF8(s string, maxBytes int, suffix string) (string, bool) {
	return errbody.TruncateUTF8(s, maxBytes, suffix)
}
