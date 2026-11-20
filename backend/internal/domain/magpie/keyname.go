package magpie

// keyname.go 是 callerKey 名字的唯一生成与校验口径（V3 改名批）。
//
// 真机口径（magpie-cli 0.1.4353 实测，证据 notes/magpie-keyname-limit-2026-11-20.md）：
//   - magpie 接受 1..120 **字符**的名字，>120 报 "Use a name between 1 and 120 characters"；
//   - 名字按字符数不按字节（120 个中文字符通过、121 个被拒）；
//   - 大写/下划线/空格 magpie 都收，同名可重复（生成两把 key），同值（secret）全局唯一。
// 灵枢生成侧比 magpie 更严（定稿口径）：只发 [a-z0-9-]、长度封顶 64 ——
// 远低于 120 上限，为 magpie 未来加校验留余量；手工绑定端点按 magpie 真上限 120 拦。
//
// 定稿命名格式（2026-11-20 spencer 定案①）：ls-u<uid>-t<tid>-<slug>
//   - uid 段：用户名可改、可重复、可能含中文 → 只用数字 uid；
//   - t 段：tokenID 全仓唯一稳定键，唯一性由它保证（magpie_keys UNIQUE(token_id) 兜底）；
//   - slug：令牌 display_name 的 ASCII 摘要（≤16），空/无可用字符时退化 w-<4hex>；
//   - 不含 PII：不塞用户名/邮箱/昵称原文（会随 caller_key_name 进账本与导出）。

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// MaxAutoKeyNameLen 是灵枢自动生成名字的封顶（定稿口径，64 << magpie 实测 120）。
	MaxAutoKeyNameLen = 64
	// maxSlugLen 是可读段的字符上限。
	maxSlugLen = 16
	// MagpieKeyNameLimit 是 magpie 侧名字长度真上限（0.1.4353 实测 120，按字符数）。
	// 管理端手工绑定用它拦截，超过即在 magpie 侧必然失败，提前 400。
	MagpieKeyNameLimit = 120
)

// keyNameFor 拼新名：ls-u<uid>-t<tid>-<slug>。
// 超长时从 slug 侧截（uid/tid 是坐标不能动）；封顶见 MaxAutoKeyNameLen。
func keyNameFor(userID, tokenID int64, slug string) string {
	base := fmt.Sprintf("ls-u%d-t%d", userID, tokenID)
	s := slug
	// 预算 = 封顶 - "ls-u…-t…" - 连接符。uid/tid 是坐标不能截，超长只截 slug。
	if budget := MaxAutoKeyNameLen - len(base) - 1; len(s) > budget {
		s = truncateSlugTo(s, budget)
	}
	if s == "" {
		// slug 全被截掉时兜底随机段，保证名字永远非空段。
		s = "w-" + randomSuffix()
	}
	return base + "-" + s
}

// slugFromDisplayName 把用户自起的令牌用途标注压成 [a-z0-9-] 的可读段：
// 小写 → 非 [a-z0-9] 折成 '-' → 连续 '-' 合一 → 去首尾 '-' → 截 16 字符。
// 压完为空（全是非 ASCII/标点）返回 w-<4hex> 随机段，保证名字永远可用。
func slugFromDisplayName(display string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(display) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	s := b.String()
	s = collapseHyphens(s)
	s = strings.Trim(s, "-")
	if len(s) > maxSlugLen {
		s = strings.TrimRight(s[:maxSlugLen], "-")
	}
	if s == "" {
		return "w-" + randomSuffix()
	}
	return s
}

// truncateSlugTo 在字节边界内截 slug 且不留下尾部 '-'。
// slug 已被 slugFromDisplayName 压成 ASCII（[a-z0-9-]），普通切片即是字符边界；
// 这里的回退只为防御调用方绕过 slugFromDisplayName 传入非 ASCII 串。
func truncateSlugTo(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	cut := s[:maxBytes]
	for len(cut) > 0 && !utf8.ValidString(cut) {
		cut = cut[:len(cut)-1]
	}
	return strings.TrimRight(cut, "-")
}

func collapseHyphens(s string) string {
	var b strings.Builder
	prev := false
	for _, r := range s {
		if r == '-' {
			if !prev {
				b.WriteByte('-')
			}
			prev = true
			continue
		}
		prev = false
		b.WriteRune(r)
	}
	return b.String()
}

func randomSuffix() string {
	var buf [2]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "0000"
	}
	return hex.EncodeToString(buf[:])
}
