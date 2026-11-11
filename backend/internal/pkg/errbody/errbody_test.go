package errbody

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// 只遮蔽**已知凭据原值**：不看形状、不做高熵兜底，因此上游原文里的 request-id、
// 模型名、URL query、<>&"' 一律原样保留（这正是「运营能看到原文」的前提）。
func TestMaskSecrets_OnlyExactKnownValues(t *testing.T) {
	const cred = "sk-live-abcdef123456"
	body := `{"error":{"message":"invalid api key sk-live-abcdef123456",` +
		`"request_id":"A1b2C3d4E5f6G7h8I9j0K1l2M3n4P5",` +
		`"model":"claude-3-5-sonnet-20241022",` +
		`"url":"https://x.example.com/v1/chat?api-version=2024-10-01","note":"<html>&amp;</html>"}}`
	got := MaskSecrets(body, cred)
	if strings.Contains(got, cred) {
		t.Fatalf("凭据未被遮蔽: %q", got)
	}
	for _, keep := range []string{
		"A1b2C3d4E5f6G7h8I9j0K1l2M3n4P5", // 高熵随机串（历史样式清洗会整段抹成 ***）
		"claude-3-5-sonnet-20241022",     // 模型名
		"api-version=2024-10-01",         // URL query
		"<html>&amp;</html>",             // HTML 特殊字符
	} {
		if !strings.Contains(got, keep) {
			t.Fatalf("非凭据内容必须原样保留，%q 被抹掉了: %q", keep, got)
		}
	}
	// 遮蔽后其余部分逐字节一致。
	if want := strings.ReplaceAll(body, cred, MaskToken); got != want {
		t.Fatalf("除凭据外必须逐字节保留\nwant=%q\ngot =%q", want, got)
	}
}

// Bearer 形式：只替换凭据本身，鉴权方式（Bearer）保留。
func TestMaskSecrets_BearerPrefixKept(t *testing.T) {
	got := MaskSecrets("Authorization: Bearer sk-proj-wxyz543210 rejected", "sk-proj-wxyz543210")
	if got != "Authorization: Bearer *** rejected" {
		t.Fatalf("Bearer 前缀应保留: %q", got)
	}
}

// 过短的值不参与遮蔽：真实凭据远长于此，而 3~7 字符的「凭据」（本地测试用）
// 在正常文本里极易是无意义子串，遮蔽它会把原文打成筛子。
func TestMaskSecrets_ShortValuesIgnored(t *testing.T) {
	body := "key=abc and more abc"
	if got := MaskSecrets(body, "abc"); got != body {
		t.Fatalf("过短值不应参与遮蔽: %q", got)
	}
}

// 长值是短值超集时先替换长值（否则长值会被切碎、再也匹配不上）。
func TestMaskSecrets_LongestFirst(t *testing.T) {
	long := "sk-live-abcdef123456"
	got := MaskSecrets("token "+long, "sk-live-ab", long)
	if strings.Contains(got, long) {
		t.Fatalf("长凭据必须被完整遮蔽: %q", got)
	}
}

// 截断只能落在合法 UTF-8 边界上，且结果不超过上限（后缀计入预算）。
func TestTruncateUTF8(t *testing.T) {
	s := strings.Repeat("中", 10) // 30 字节
	out, cut := TruncateUTF8(s, 10, "…")
	if !cut {
		t.Fatal("应判定为截断")
	}
	if len(out) > 10 {
		t.Fatalf("结果不得超过上限: %d", len(out))
	}
	if !utf8.ValidString(out) {
		t.Fatalf("截断结果必须是合法 UTF-8: %q", out)
	}
	if !strings.HasSuffix(out, "…") {
		t.Fatalf("必须带后缀: %q", out)
	}
	if short, cut := TruncateUTF8("短", 100, "…"); cut || short != "短" {
		t.Fatalf("未超限必须原样返回: %q cut=%v", short, cut)
	}
}

// TestToValidText 落库前的字节整理：非法 UTF-8 → U+FFFD、NUL → <0x00>，其余逐字节保真。
// 这条守卫的存在理由：PG text 列拒绝非法字节序列与 0x00，原样写会让整行 INSERT 失败，
// 于是一次上游报错就把这次请求的账单/调用日志整行弄丢。
func TestToValidText(t *testing.T) {
	cases := []struct {
		name    string
		in      string
		want    string
		changed bool
	}{
		{"合法中文原样", "上游返回 400：区域未开通 <b>&</b>", "上游返回 400：区域未开通 <b>&</b>", false},
		{"空串", "", "", false},
		{"非法UTF8字节", "\xd6\xd0", "\uFFFD\uFFFD", true},
		{"NUL替换", "a\x00b", "a" + NULPlaceholder + "b", true},
		{"截断的多字节字符（每个坏字节一个替换符）", "中\xe6\x96", "中\uFFFD\uFFFD", true},
		{"emoji原样", "🙂", "🙂", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, changed := ToValidText(tc.in)
			if got != tc.want {
				t.Fatalf("ToValidText(%q) = %q, want %q", tc.in, got, tc.want)
			}
			if changed != tc.changed {
				t.Fatalf("changed = %v, want %v", changed, tc.changed)
			}
			if !utf8.ValidString(got) || strings.ContainsRune(got, 0) {
				t.Fatalf("结果必须是合法 UTF-8 且无 NUL: %q", got)
			}
		})
	}
}
