package billingday

import (
	"testing"
	"time"
)

// 账期日界的三条不变式：DATE 值还原成日、瞬时落到日、右界与 PG 的 interval 同语义。
func TestCivilDayAndDayStart(t *testing.T) {
	loc := Location()
	// DATE 值（驱动给 UTC 0 点）→ 账期日 0 点
	utcDate := time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)
	if got := CivilDay(utcDate); !got.Equal(time.Date(2026, 11, 17, 0, 0, 0, 0, loc)) {
		t.Fatalf("CivilDay = %s", got)
	}
	// 北京 01:00（= 前一天 17:00Z）→ 账期日是 11-17，不是 11-16
	instant := time.Date(2026, 11, 16, 17, 0, 0, 0, time.UTC)
	if got := DayStart(instant); !got.Equal(time.Date(2026, 11, 17, 0, 0, 0, 0, loc)) {
		t.Fatalf("DayStart = %s", got)
	}
	if got := Date(utcDate); got != "2026-11-17" {
		t.Fatalf("Date = %s", got)
	}
}

// 右界必须与 PG 的 `+ interval '1 month'` 一致：月末**截断**，不溢出。
// （Go 原生 AddDate 会把 1-31 + 1 月算成 3-3，与 SQL 侧差 3 天。）
func TestCycleEnd_MatchesPostgresInterval(t *testing.T) {
	cases := []struct{ day, cycle, want string }{
		{"2026-01-31", "month", "2026-02-28"},
		{"2026-08-31", "month", "2026-09-30"},
		{"2026-11-01", "month", "2026-12-01"},
		{"2026-11-17", "year", "2027-11-17"},
		{"2028-02-29", "year", "2029-02-28"}, // 闰日 + 1 年 → 截断
		{"2026-11-17", "once", "2026-11-18"},
		{"2026-01-31", "quarter?!", "2026-02-28"}, // 未知周期按 1 月
	}
	for _, c := range cases {
		day, err := time.Parse("2006-01-02", c.day)
		if err != nil {
			t.Fatalf("解析 %s 失败：%v", c.day, err)
		}
		got := CycleEnd(day, c.cycle)
		if got.Format("2006-01-02") != c.want {
			t.Errorf("CycleEnd(%s, %s) = %s，期望 %s", c.day, c.cycle, got.Format("2006-01-02"), c.want)
		}
		if got.Location() != Location() {
			t.Errorf("CycleEnd(%s, %s) 时区 = %s，期望账期日界", c.day, c.cycle, got.Location())
		}
	}
}

// 右界必须是「账期日 0 点」而不是 UTC 0 点（否则窗口整体错位 8 小时）。
func TestCycleEnd_NormalizesToBillingZone(t *testing.T) {
	utcDate := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	got := CycleEnd(utcDate, "month")
	want := time.Date(2026, 12, 1, 0, 0, 0, 0, Location())
	if !got.Equal(want) || got.Location() != want.Location() {
		t.Fatalf("CycleEnd = %s（%s），期望 %s（%s）", got, got.Location(), want, want.Location())
	}
}

// 没有 tzdata 的精简镜像要能回落固定 UTC+8（中国无夏令时）。
func TestLocationFallbackIsUTC8(t *testing.T) {
	if _, err := time.LoadLocation(Zone); err == nil {
		t.Skip("本机有 tzdata，回落分支不触发")
	}
	if _, off := time.Now().In(Location()).Zone(); off != 8*60*60 {
		t.Fatalf("回落时区偏移 = %d", off)
	}
}
