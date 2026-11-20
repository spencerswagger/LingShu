// keyname_test.go 锁名字生成的全部定稿口径：
// 格式 / 字符集 / 64 封顶（uid+t 段不动、只截 slug）/ slug 归一 / 空 slug 随机段 / PII 不出现。
package magpie

import (
	"regexp"
	"strings"
	"testing"
)

var nameShape = regexp.MustCompile(`^ls-u[0-9]+-t[0-9]+-[a-z0-9-]+$`)

func TestKeyNameFor_Shape(t *testing.T) {
	got := keyNameFor(12, 7, slugFromDisplayName("bot"))
	if !nameShape.MatchString(got) {
		t.Fatalf("形状不符：%q", got)
	}
	if got != "ls-u12-t7-bot" {
		t.Fatalf("定稿示例不符：%q", got)
	}
}

func TestKeyNameFor_LongSlugTruncatedNotUID(t *testing.T) {
	// uid/tokenID 是坐标段，超长只许截 slug。
	long := strings.Repeat("a", 200)
	got := keyNameFor(987654, 123456789, long)
	if !nameShape.MatchString(got) || len(got) > MaxAutoKeyNameLen {
		t.Fatalf("超封顶或形状不符：%q (len=%d)", got, len(got))
	}
	if !strings.HasPrefix(got, "ls-u987654-t123456789-") {
		t.Fatalf("坐标段被动了：%q", got)
	}
}

func TestSlugFromDisplayName_Normalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Claude Code", "claude-code"},
		{"  我的--机器人!! ", "w-"}, // 全非 ASCII/标点 → 随机段
		{"A_B.C", "a-b-c"},
		{"---", "w-"},
		{"", "w-"},
		{"12345678901234567890", "1234567890123456"}, // 截 16
		{"tab\tsep", "tab-sep"},
	}
	for _, c := range cases {
		got := slugFromDisplayName(c.in)
		if c.want == "w-" {
			// 随机段：只验形状与长度。
			if !strings.HasPrefix(got, "w-") || len(got) != len("w-")+4 {
				t.Fatalf("%q → %q，期望 w-<4hex>", c.in, got)
			}
			continue
		}
		if got != c.want {
			t.Fatalf("%q → %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestSlug_TrailingHyphenTrimmedAfterTruncate(t *testing.T) {
	// 截断点正好落在 '-' 上时不得留尾 '-'（否则出现 ls-u..-t..-abc- 形状违规）。
	got := slugFromDisplayName("abcdefgh-ijklmnop")
	if strings.HasSuffix(got, "-") {
		t.Fatalf("slug 尾部有 -：%q", got)
	}
}

func TestSlugFromDisplayName_NoPII(t *testing.T) {
	// 中文昵称/邮箱不得原样出现（会被折成 - 或丢弃）。
	for _, in := range []string{"张三@corp.com", "手机13800000000", "用户名_test"} {
		got := slugFromDisplayName(in)
		if strings.ContainsAny(got, "@._") || regexp.MustCompile(`[A-Z]`).MatchString(got) {
			t.Fatalf("slug 含未归一字符：%q ← %q", got, in)
		}
		if regexp.MustCompile(`[^\x00-\x7F]`).MatchString(got) {
			t.Fatalf("slug 含非 ASCII：%q ← %q", got, in)
		}
	}
}

func TestKeyNameFor_Deterministic(t *testing.T) {
	a := keyNameFor(12, 7, slugFromDisplayName("Claude Code"))
	b := keyNameFor(12, 7, slugFromDisplayName("Claude Code"))
	if a != b {
		t.Fatalf("同名同令牌应生成同一名字：%q vs %q", a, b)
	}
}

func TestLegacyAutoName_Format(t *testing.T) {
	if LegacyAutoName(42) != "ls-token-42" {
		t.Fatalf("旧格式漂移：%q", LegacyAutoName(42))
	}
}

func TestMagpieKeyNameLimit_RealMachineValue(t *testing.T) {
	// 真机实测上限（0.1.4353）：120 字符，>120 拒。锁常量防回退到报告假设的 64/原 128。
	if MagpieKeyNameLimit != 120 {
		t.Fatalf("MagpieKeyNameLimit = %d，期望 120（真机实测）", MagpieKeyNameLimit)
	}
}
