package magpie

// keyname.go 是 callerKey 存储名（key_name）的唯一生成与校验口径（V3 改名批，契约定稿 §1.1）。
//
// 契约（magpie-passthrough-contract-final.md §1.1 + 报告 §2.2，2026-11-20 spencer 定案①）：
//   - key 名 = 「用户名转写 - 令牌名转写」；uid/tokenID 不进 key 名（唯一性由
//     magpie_keys UNIQUE(token_id) 承担，key_name 只是账本连接键）；
//   - 转写：小写 → 非 [a-z0-9] 折 '-' → 连续 '-' 合一 → 去首尾 '-'；段空退化
//     用户名→u<uid>、令牌名→w-+4hex（tokenID 确定性派生，无 PII；不依赖 magpie 撞名重试）；
//   - 重名消歧：同用户重名或跨用户撞名 → 尾部 -2/-3 顺序追加；分配后固化不回收；
//     64 封顶先截转写段再拼尾号，尾号永不截；
//   - 不含 PII：无 secret 片段、无 token_hash、无邮箱/手机号（用户名转写属定案口径允许项）。
//
// 真机口径（magpie-cli 0.1.4353/0.1.4454 实测，证据 notes/magpie-keyname-limit-2026-11-20.md）：
// magpie 接受 1..120 字符名（按字符数）；灵枢自动名 64 封顶留余量；手工绑定按真上限 120 拦。

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"unicode/utf8"
)

const (
	// MaxAutoKeyNameLen 是灵枢自动生成名字的硬封顶（契约 §1.1：64）。
	MaxAutoKeyNameLen = 64
	// MagpieKeyNameLimit 是 magpie 侧名字长度真上限（0.1.4353 实测 120，按字符数）。
	// 管理端手工绑定用它拦截，超过即在 magpie 侧必然失败，提前 400。
	MagpieKeyNameLimit = 120
)

// legacySegment 是「段无 ASCII 可用」时的 ID 退化格式（u<uid> / t<tokenID>）。
func legacySegment(prefix, id string) string { return prefix + id }

// transcribeSegment 把一段（用户名 / 令牌名）压成 [a-z0-9-]：
// 小写 → 非 [a-z0-9] 折 '-' → 连续 '-' 合一 → 去首尾 '-'。压完为空返回空串，
// 由调用方决定段级退化（契约：段空 → ID 段，不依赖 magpie 撞名重试）。
func transcribeSegment(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(collapseHyphens(b.String()), "-")
}

// truncateSegment 在字节边界内截段且不留尾 '-'（段已被 transcribeSegment 压成 ASCII，
// 普通切片即是字符边界；回退只为防御调用方绕过转写传入非 ASCII 串）。
func truncateSegment(s string, maxBytes int) string {
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

// baseNameFor 拼「用户名转写-令牌名转写」基础名（无消歧尾号）。
// 段空退化（契约 §1.1 转写规则）：用户名→u<uid> 段、令牌名→w-+4hex 段
// （4hex 由 tokenID 确定性派生——重算可比对、无 PII；uid/tokenID 不以明文进名）；
// 两段全空（防御分支）→ 整名 w-<随机4hex>。
// 超过 64 封顶时先截转写段（退化段是坐标不截）。
func baseNameFor(userID, tokenID int64, username, displayName string) string {
	u := transcribeSegment(username)
	t := transcribeSegment(displayName)
	if u == "" {
		u = legacySegment("u", fmt.Sprintf("%d", userID))
	}
	if t == "" {
		t = tokenSegmentFallback(tokenID)
	}
	// 64 封顶预算：总长 - 预留尾号。超长先截「令牌名段」再截「用户名段」，
	// 保证尾号（-2/-3）拼上后仍 ≤64 且尾号永不截。
	name := u + "-" + t
	const reserve = 4 // "-2"…"-99" 的预留（契约：尾号永不截）
	if max := MaxAutoKeyNameLen - reserve; len(name) > max {
		over := len(name) - max
		t = truncateSegment(t, len(t)-over)
		if t == "" {
			// 令牌段全截掉则整段退化为 w-4hex 段（仍受封顶约束：ID 段在截用户段时兜底）。
			t = tokenSegmentFallback(tokenID)
		}
		name = u + "-" + t
		if len(name) > max {
			u = truncateSegment(u, len(u)-(len(name)-max))
			if u == "" {
				u = legacySegment("u", fmt.Sprintf("%d", userID))
			}
			name = u + "-" + t
		}
	}
	if u == "" && t == "" {
		// 两段全空（防御：退化段必非空，此分支只防调用方传负/异常 ID 组合）。
		return "w-" + randomSuffix()
	}
	return name
}

// tokenSegmentFallback 令牌名段无 ASCII 可用时的退化段（契约 §1.1：w-+4hex）。
// 4hex 由 tokenID 确定性派生（sha256 截断）：回填/重算可比对，不含 PII。
func tokenSegmentFallback(tokenID int64) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("lingshu:caller-key:%d", tokenID)))
	return fmt.Sprintf("w-%x", sum[:2])
}

// KeyNameFor 计算该令牌按新口径应有的存储名（无消歧尾号的基础名）。
// 消歧尾号由分配器 EnsureNameUnique 在撞名时追加（分配后固化不回收）。
func KeyNameFor(userID, tokenID int64, username, displayName string) string {
	return baseNameFor(userID, tokenID, username, displayName)
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
