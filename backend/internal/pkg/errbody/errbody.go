// Package errbody 提供「上游错误响应体」在**运营可见**链路上的两个基础操作：
// 精确凭据遮蔽（MaskSecrets）与按 UTF-8 边界截断（TruncateUTF8）。
//
// 为什么要有这个包：上游错误体要「原文可见」才能让运营判断到底是上游拒绝了什么，
// 但报文里可能回显我们发过去的凭据。历史做法是按**样式**清洗（前缀 sk-/Bearer/KV/高熵长串），
// 结果把 request-id、模型名、错误标识一并抹成 ***——运营照样看不到原文。
// 这里改成**只遮蔽本次实际使用的凭据原值**（精确匹配，不看形状、不做白名单），
// 于是除凭据之外的一切内容逐字节保真，凭据本身仍然不落盘。
package errbody

import (
	"strings"
	"unicode/utf8"
)

// MaskToken 是遮蔽后的占位符（与历史日志/落库口径一致，便于检索）。
const MaskToken = "***"

// minSecretLen 参与遮蔽的凭据最短长度：过短的"凭据"（如本地测试用的 3~7 字符）
// 在正常文本里极易是无意义子串，遮蔽它反而会把原文打成筛子。真实渠道密钥远长于此。
const minSecretLen = 8

// MaskSecrets 把 s 里出现的**已知凭据原值**替换成 MaskToken。
//
// 规则（刻意做到最窄）：
//   - 只按精确子串匹配 secrets 里给出的值（长度 ≥ minSecretLen，去空白）；
//   - 不去猜「长得像凭据」的串：没有前缀规则、没有高熵兜底、没有字段白名单；
//   - "Bearer <cred>" 里的 <cred> 会被替换，前缀 Bearer 原样保留（读起来仍是「用了什么鉴权方式」）。
//
// 因此除凭据本身外，上游原文（含 <>&、中文、随机 request-id、模型名、URL query）逐字节不变。
func MaskSecrets(s string, secrets ...string) string {
	if s == "" {
		return s
	}
	// 先长后短：短值是长值子串时先替换长值，避免把长值切成碎片导致长值再也匹配不上。
	ordered := make([]string, 0, len(secrets))
	for _, sec := range secrets {
		sec = strings.TrimSpace(sec)
		if len(sec) >= minSecretLen {
			ordered = append(ordered, sec)
		}
	}
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && len(ordered[j]) > len(ordered[j-1]); j-- {
			ordered[j], ordered[j-1] = ordered[j-1], ordered[j]
		}
	}
	for _, sec := range ordered {
		s = strings.ReplaceAll(s, sec, MaskToken)
	}
	return s
}

// TruncateUTF8 按 **UTF-8 字符边界**把 s 截断到不超过 maxBytes 字节（suffix 计入预算），
// 返回 (结果, 是否发生截断)。结果永远是合法 UTF-8（不含半个字符）。
//
// 约定：未超限时原样返回（零改动）；suffix 本身比 maxBytes 还长时只保留 suffix 的合法前缀
// （极端防御，不返回超限串）。
func TruncateUTF8(s string, maxBytes int, suffix string) (string, bool) {
	if maxBytes <= 0 {
		return "", len(s) > 0
	}
	if len(s) <= maxBytes {
		return s, false
	}
	if len(suffix) >= maxBytes {
		return CutRunePrefix(suffix, maxBytes), true
	}
	return CutRunePrefix(s, maxBytes-len(suffix)) + suffix, true
}

// CutRunePrefix 返回 s 的最长前缀，其字节数 ≤ maxBytes 且以完整字符结尾。
// 从 maxBytes 处往回退到最近的 UTF-8 起始字节，保证不会切出半个字符。
func CutRunePrefix(s string, maxBytes int) string {
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

// NULPlaceholder 是 0x00 字节在 text 列里的可见占位符（PG 的 text 物理上不能存 NUL）。
const NULPlaceholder = `<0x00>`

// ToValidText 把任意字节串整理成 PostgreSQL `text` 列**一定能写入**的合法 UTF-8，返回替换后的文本与「是否发生过替换」。
//
// 为什么必须有这一层：上游错误体是**任意字节**，可能带非法 UTF-8 序列（GBK 错误页、截断的多字节字符）
// 或 0x00。原样写库时 PG 会拒绝整行 INSERT（invalid byte sequence / invalid message format），
// 于是一次「上游报错」把这次请求的账单与调用日志**整行弄丢**——正好是本任务要消灭的「查不到」。
// 这里只做两件事，且都不静默：
//   - 非法 UTF-8 字节序列 → U+FFFD（与 Go 标准库/浏览器同口径，调用方看得见「这里原本有坏字节」）；
//   - NUL（0x00）→ `NULPlaceholder`（可见占位符），因为 text 列无法保存它。
//
// 其余每个字节（含 <>&、中文、随机串）原样保留——不做样式清洗、不做字段裁剪。
func ToValidText(s string) (string, bool) {
	if s == "" {
		return s, false
	}
	// 快路径：合法且无 NUL 时零分配。
	if utf8.ValidString(s) && !strings.ContainsRune(s, 0) {
		return s, false
	}
	var b strings.Builder
	b.Grow(len(s) + 16)
	for i := 0; i < len(s); {
		c := s[i]
		if c == 0 {
			b.WriteString(NULPlaceholder)
			i++
			continue
		}
		if c < utf8.RuneSelf {
			b.WriteByte(c)
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			b.WriteRune(utf8.RuneError) // 非法字节 → U+FFFD
			i++
			continue
		}
		b.WriteString(s[i : i+size])
		i += size
	}
	return b.String(), true
}
