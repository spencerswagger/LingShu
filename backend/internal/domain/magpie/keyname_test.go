// keyname_test.go 锁名字生成的全部契约口径（契约定稿 §1.1 + 报告 §2.2）：
// 格式（用户名转写-令牌名转写）/ 字符集 / 64 封顶（截转写段、尾号永不截）/
// 段空退化（u<uid> / t<tokenID>）/ PII 不出现 / 确定性。
package magpie

import (
	"regexp"
	"strings"
	"testing"
)

var nameShape = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

func TestKeyNameFor_Shape(t *testing.T) {
	got := KeyNameFor(12, 7, "zhangsan", "办公室台式机")
	// 中文令牌名无 ASCII → 令牌段退化 w-+4hex（tokenID 确定性派生）；用户名转写保留。
	if got != "zhangsan-w-5866" {
		t.Fatalf("定稿示例不符：%q", got)
	}
	if !nameShape.MatchString(got) {
		t.Fatalf("形状不符：%q", got)
	}
}

func TestKeyNameFor_ReportExamples(t *testing.T) {
	// 报告 §2.2 示例：zhangsan-bangongshi 形态（用户段转写保留）。
	if got := KeyNameFor(1, 2, "zhangsan", "办公室台式机-ascii"); strings.HasPrefix(got, "zhangsan-") == false {
		t.Fatalf("用户段应保留转写：%q", got)
	}
	// 纯中文用户名 → 该段退化 u<uid>（不依赖 magpie 撞名重试），令牌段转写保留。
	if got := KeyNameFor(3, 4, "李四", "Claude Code"); got != "u3-claude-code" {
		t.Fatalf("纯中文用户名段退化不符：%q", got)
	}
}

func TestKeyNameFor_SegmentFallback(t *testing.T) {
	// 用户名/令牌名均无 ASCII 可用 → 各段退化 ID（不依赖 magpie 撞名重试）。
	got := KeyNameFor(9, 8, "王五", "机密项目")
	if got != "u9-w-8433" {
		t.Fatalf("双段退化不符：%q", got)
	}
	// 空 display_name 与空 username 同样走退化。
	if got := KeyNameFor(11, 13, "", ""); got != "u11-w-0ef2" {
		t.Fatalf("空串退化不符：%q", got)
	}
}

func TestKeyNameFor_LengthCapped64(t *testing.T) {
	// 超长先截转写段（ID 退化段是坐标不截），整名 ≤64，形状仍合法。
	long := strings.Repeat("a", 200)
	got := KeyNameFor(987654, 123456789, "verylongusername"+long, long)
	if len(got) > MaxAutoKeyNameLen {
		t.Fatalf("超封顶：%q (len=%d)", got, len(got))
	}
	if !nameShape.MatchString(got) {
		t.Fatalf("形状不符：%q", got)
	}
}

func TestKeyNameFor_Deterministic(t *testing.T) {
	a := KeyNameFor(12, 7, "zhangsan", "Claude Code")
	b := KeyNameFor(12, 7, "zhangsan", "Claude Code")
	if a != b || a != "zhangsan-claude-code" {
		t.Fatalf("同输入应得同名：%q vs %q", a, b)
	}
}

func TestTranscribeSegment_Normalization(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Claude Code", "claude-code"},
		{"  我的--机器人!! ", ""},
		{"A_B.C", "a-b-c"},
		{"---", ""},
		{"", ""},
		{"123456789012345678901234567890", "123456789012345678901234567890"},
		{"tab\tsep", "tab-sep"},
	}
	for _, c := range cases {
		if got := transcribeSegment(c.in); got != c.want {
			t.Fatalf("%q → %q，期望 %q", c.in, got, c.want)
		}
	}
}

func TestKeyNameFor_NoPII(t *testing.T) {
	// 邮箱/手机号/secret 片段不得原样出现（非 [a-z0-9] 全折 '-'）。
	for _, in := range []string{"user@corp.com", "13800138000", "sk-magpie-key-Abc123", "口令=p@ss"} {
		got := transcribeSegment(in)
		if strings.ContainsAny(got, "@._") || regexp.MustCompile(`[A-Z]`).MatchString(got) {
			t.Fatalf("转写含未归一字符：%q ← %q", got, in)
		}
		if regexp.MustCompile(`[^\x00-\x7F]`).MatchString(got) {
			t.Fatalf("转写含非 ASCII：%q ← %q", got, in)
		}
	}
}

func TestLegacyAutoName_Format(t *testing.T) {
	// 旧口径自动名（改名资格判定的确定性锚点，改它会破坏存量回填判定）。
	if LegacyAutoName(42) != "ls-token-42" {
		t.Fatalf("旧格式漂移：%q", LegacyAutoName(42))
	}
}

func TestMagpieKeyNameLimit_RealMachineValue(t *testing.T) {
	// 真机实测上限（0.1.4353/0.1.4454）：120 字符，>120 拒。锁常量防回退。
	if MagpieKeyNameLimit != 120 {
		t.Fatalf("MagpieKeyNameLimit = %d，期望 120（真机实测）", MagpieKeyNameLimit)
	}
}
