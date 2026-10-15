package console

import "testing"

// TestParseRFC3339Lenient 守护「查询串中未编码的 '+' 被解码为空格」的兼容解析：
// 客户端/代理把 RFC3339 的 '+08:00' 传成 ' 08:00' 时仍应解析成功，且与标准形式等值。
func TestParseRFC3339Lenient(t *testing.T) {
	canonical := "2026-10-15T23:59:59+08:00"
	spaced := "2026-10-15T23:59:59 08:00"

	got, err := parseRFC3339Lenient(canonical)
	if err != nil {
		t.Fatalf("canonical parse failed: %v", err)
	}
	got2, err := parseRFC3339Lenient(spaced)
	if err != nil {
		t.Fatalf("spaced parse failed: %v", err)
	}
	if !got.Equal(got2) {
		t.Fatalf("lenient parse mismatch: %v vs %v", got, got2)
	}

	// UTC 'Z' 形式不受影响。
	if _, err := parseRFC3339Lenient("2026-10-15T15:59:59Z"); err != nil {
		t.Fatalf("Z parse failed: %v", err)
	}

	// 非法值仍应报错（不掩盖真正的格式错误）。
	if _, err := parseRFC3339Lenient("not-a-time"); err == nil {
		t.Fatalf("expected error for invalid input")
	}
}
