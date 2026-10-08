package cronx

import (
	"testing"
	"time"
)

func mustParse(t *testing.T, expr string) *Spec {
	t.Helper()
	s, err := Parse(expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	return s
}

func TestParseValid(t *testing.T) {
	for _, expr := range []string{
		"0 * * * * *",
		"*/15 * * * * *",
		"0 */5 * * * *",
		"30 0 2 * * *",
		"0 0 9-17 * * 1-5",
		"0,30 * * * * *",
	} {
		if _, err := Parse(expr); err != nil {
			t.Fatalf("expected %q valid, got %v", expr, err)
		}
	}
}

func TestParseInvalid(t *testing.T) {
	for _, expr := range []string{
		"* * * * *",    // 5 段
		"60 * * * * *", // 秒越界
		"* * * 0 * *",  // 日越界
		"a * * * * *",  // 非数字
	} {
		if _, err := Parse(expr); err == nil {
			t.Fatalf("expected %q invalid", expr)
		}
	}
}

func TestNextEveryMinute(t *testing.T) {
	s := mustParse(t, "0 * * * * *")
	base := time.Date(2026, 9, 27, 10, 30, 15, 0, time.UTC)
	got := s.Next(base)
	want := time.Date(2026, 9, 27, 10, 31, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestNextEvery15Seconds(t *testing.T) {
	s := mustParse(t, "*/15 * * * * *")
	base := time.Date(2026, 9, 27, 10, 0, 16, 0, time.UTC)
	got := s.Next(base)
	want := time.Date(2026, 9, 27, 10, 0, 30, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestNextStrictlyAfter(t *testing.T) {
	s := mustParse(t, "*/15 * * * * *")
	// 恰好在匹配点上：下一次应是 +15s 而不是自身
	base := time.Date(2026, 9, 27, 10, 0, 30, 0, time.UTC)
	got := s.Next(base)
	want := time.Date(2026, 9, 27, 10, 0, 45, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}

func TestNextWeekday(t *testing.T) {
	s := mustParse(t, "0 0 9 * * 1-5") // 周一至周五 9 点
	// 2026-09-27 是周日 → 下一次应是周一 2026-09-28 09:00
	base := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)
	got := s.Next(base)
	want := time.Date(2026, 9, 28, 9, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("want %v, got %v", want, got)
	}
}
