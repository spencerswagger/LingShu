package gateway

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// 本文件是 PR #63 Gate 复审 P3 建议项的回归：**截断必须按 UTF-8 字符边界**。
// 旧实现 `v[:max] + "..."` 是字节切，中文模型名/长 URL 被切成半个字符后，
// encoding/json 会把非法字节写成 U+FFFD（�），用户看到的快照是花的。

// assertCleanUTF8 断言 s 是合法 UTF-8 且不含替换符。
func assertCleanUTF8(t *testing.T, s, what string) {
	t.Helper()
	if !utf8.ValidString(s) {
		t.Fatalf("%s 不是合法 UTF-8: %q", what, s)
	}
	if strings.ContainsRune(s, utf8.RuneError) {
		t.Fatalf("%s 含 U+FFFD 替换符（截断切坏了字符）: %q", what, s)
	}
}

// TestTruncateUTF8_Chinese 多字节中文：不得切出半个字符，且字节数不超预算。
func TestTruncateUTF8_Chinese(t *testing.T) {
	s := strings.Repeat("中文模型名", 300) // 5 字 × 3B × 300 = 4500B
	const max = 1024
	out, truncated := truncateUTF8(s, max, "...")
	if !truncated {
		t.Fatal("长中文串应被截断")
	}
	assertCleanUTF8(t, out, "截断结果")
	if len(out) > max {
		t.Fatalf("截断后 %d 字节 > 预算 %d", len(out), max)
	}
	body := strings.TrimSuffix(out, "...")
	if body == "" {
		t.Fatal("截断后正文为空")
	}
	// 尾部必须是完整字符（去掉后缀后仍是合法 UTF-8，且是原串前缀）。
	assertCleanUTF8(t, body, "截断正文")
	if !strings.HasPrefix(s, body) {
		t.Fatal("截断结果不是原串前缀")
	}
	// 每个字符都完整：按 rune 遍历后拼回应与 body 相等。
	var sb strings.Builder
	for _, r := range body {
		sb.WriteRune(r)
	}
	if sb.String() != body {
		t.Fatalf("按 rune 重组与 body 不一致：%q vs %q", sb.String(), body)
	}
}

// TestTruncateUTF8_EmojiBoundary emoji/4 字节字符（含 ZWJ 组合）跨边界：不得切坏。
func TestTruncateUTF8_EmojiBoundary(t *testing.T) {
	cases := []string{
		strings.Repeat("😀", 200),    // 4B 单码点
		strings.Repeat("👨‍👩‍👧", 80), // ZWJ 组合（多跑马灯：多个 rune 组成一个视觉字符）
		strings.Repeat("🏳️‍🌈", 80),  // 变体选择符 + ZWJ
		strings.Repeat("中😀a", 150),  // 混排
	}
	for i, s := range cases {
		for _, max := range []int{7, 8, 9, 1024, 1025} {
			out, _ := truncateUTF8(s, max, "...")
			if len(out) > max {
				t.Fatalf("case %d max=%d: 结果 %d 字节超预算", i, max, len(out))
			}
			assertCleanUTF8(t, out, "emoji 截断结果")
			if !strings.HasPrefix(s, strings.TrimSuffix(out, "...")) {
				t.Fatalf("case %d max=%d: 截断结果不是原串前缀: %q", i, max, out)
			}
		}
	}
}

// TestTruncateUTF8_ExactBoundary 边界 ±1 字节：上限落在字符中间时，保留**前一个完整字符**。
func TestTruncateUTF8_ExactBoundary(t *testing.T) {
	// "a" + 三个 3 字节汉字 = 1 + 9 = 10 字节；字符起始位置为 1、4、7。
	s := "a" + "中中中"
	for _, c := range []struct {
		max  int
		want string // 期望保留的前缀（不含 suffix，suffix 用空串）
	}{
		{1, "a"},
		{2, "a"},  // 落在第一个「中」中间 → 退到前一个完整字符
		{3, "a"},  // 1+3=4 > 3 → 仍只保留 "a"
		{4, "a中"}, // 正好完整
		{5, "a中"},
		{6, "a中"},
		{7, "a中中"},
		{10, s}, // 恰好整串：不截断
	} {
		out, truncated := truncateUTF8(s, c.max, "")
		if out != c.want {
			t.Fatalf("max=%d: got %q, want %q", c.max, out, c.want)
		}
		if truncated != (c.max < len(s)) {
			t.Fatalf("max=%d: truncated=%v, want %v", c.max, truncated, c.max < len(s))
		}
		assertCleanUTF8(t, out, "边界截断结果")
	}
}

// TestTruncateUTF8_SuffixBudget 后缀计入预算：截断结果（含后缀）不超上限。
func TestTruncateUTF8_SuffixBudget(t *testing.T) {
	s := strings.Repeat("中", 100)
	for _, max := range []int{3, 4, 10, 64} {
		out, _ := truncateUTF8(s, max, "...")
		if len(out) > max {
			t.Fatalf("max=%d: 含后缀共 %d 字节，超预算", max, len(out))
		}
		assertCleanUTF8(t, out, "带后缀截断结果")
	}
	// 后缀本身比上限还长：不返回超限串，也不产生非法 UTF-8。
	out, _ := truncateUTF8(s, 2, "...")
	if len(out) > 2 {
		t.Fatalf("后缀超长时结果 %d 字节，应 ≤2", len(out))
	}
	assertCleanUTF8(t, out, "后缀超长结果")
	// 不超上限时原样返回（零差异）。
	if got, tr := truncateUTF8("短", 1024, "..."); tr || got != "短" {
		t.Fatalf("未超限应原样返回: %q truncated=%v", got, tr)
	}
}

// TestCapCoreString_ChineseModelName 走真实路径（req_params 核心字段）：中文模型名超长时
// 快照里不得出现 U+FFFD，且记名 + 预算不超。
func TestCapCoreString_ChineseModelName(t *testing.T) {
	longModel := strings.Repeat("长中文模型名", 400) // 1800B > coreStringMax(1024)
	if len(longModel) <= coreStringMax {
		t.Fatalf("构造值太短: %d", len(longModel))
	}
	src := newReqParamsSource([]byte(`{"model":"m","messages":[],"temperature":0.7,"max_tokens":16,"top_p":1}`))
	raw := src.buildReqParams(reqParamsInput{
		Endpoint:        "/v1/chat/completions",
		InboundProtocol: "openai-chat",
		Model:           longModel,
	})
	if raw == nil {
		t.Fatal("snapshot is nil")
	}
	assertCleanUTF8(t, string(raw), "快照 JSON")
	if strings.Contains(string(raw), "\ufffd") {
		t.Fatal("快照里出现 U+FFFD")
	}
	var snap reqParamsSnapshot
	if err := json.Unmarshal(raw, &snap); err != nil {
		t.Fatalf("反序列化失败: %v", err)
	}
	if len(snap.Model) > coreStringMax {
		t.Fatalf("model 截断后 %d 字节 > %d", len(snap.Model), coreStringMax)
	}
	assertCleanUTF8(t, snap.Model, "快照里的 model")
	found := false
	for _, k := range snap.Summarized {
		if k == "model" {
			found = true
		}
	}
	if !found {
		t.Fatalf("截断未记名: summarized=%v", snap.Summarized)
	}
	// 预算口径不变：仍须 ≤ 8KB（截断按 rune 不得放宽预算）。
	if bound := jsonbSizeBound(raw, coreKVCount+len(snap.Params)+len(snap.Summarized)+len(snap.Dropped)); bound > reqParamsMaxBytes {
		t.Fatalf("预算超限: bound=%d > %d", bound, reqParamsMaxBytes)
	}
}

// TestTruncateLog_UnifiedUTF8 truncateLog 收敛到同一实现后：合法 UTF-8、含后缀仍不超上限。
func TestTruncateLog_UnifiedUTF8(t *testing.T) {
	s := strings.Repeat("错误摘要：中文内容", 100)
	for _, max := range []int{1, 4, 16, 512} {
		out := truncateLog(s, max)
		if len(out) > max {
			t.Fatalf("max=%d: truncateLog 返回 %d 字节，超上限", max, len(out))
		}
		assertCleanUTF8(t, out, "truncateLog 结果")
	}
	if got := truncateLog("短", 512); got != "短" {
		t.Fatalf("未超限应原样返回，got %q", got)
	}
}
