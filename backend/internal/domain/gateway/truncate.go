package gateway

import "unicode/utf8"

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
// 这是网关内**唯一**的字符串截断实现（errlog 的 truncateLog 也走它），不要再写第二套按字节切的版本。
func truncateUTF8(s string, maxBytes int, suffix string) (string, bool) {
	if maxBytes <= 0 {
		return "", len(s) > 0
	}
	if len(s) <= maxBytes {
		return s, false
	}
	if len(suffix) >= maxBytes {
		return cutRunePrefix(suffix, maxBytes), true
	}
	cut := cutRunePrefix(s, maxBytes-len(suffix))
	return cut + suffix, true
}

// cutRunePrefix 返回 s 的最长前缀，其字节数 ≤ maxBytes 且以完整字符结尾。
// 从 maxBytes 处往回退到最近的 UTF-8 起始字节，保证不会切出半个字符。
func cutRunePrefix(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}
