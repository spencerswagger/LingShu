// Package cronx 提供最小 6 段 cron 表达式的解析与「下次执行时间」计算。
//
// 格式：秒 分 时 日 月 周（2 位域，如 "0 * * * * *" = 每分钟第 0 秒）。
// 每段支持：*、*/n、a-b、a,b,c、定值。
// 仅用于网关探测调度，不做 Quartz 级别特性；无解时 Next 返回零值。
package cronx

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Spec 解析后的 cron 规格。
type Spec struct {
	seconds  []int
	minutes  []int
	hours    []int
	days     []int // 日（1-31）
	months   []int // 月（1-12）
	weekdays []int // 周（0-6，0=周日）
	full     bool  // 是否等价于每秒（* * * * * *）
}

// Parse 解析 6 段 cron 表达式。
func Parse(expr string) (*Spec, error) {
	fields := strings.Fields(strings.TrimSpace(expr))
	if len(fields) != 6 {
		return nil, fmt.Errorf("cron 需要 6 段（秒 分 时 日 月 周），得到 %d 段: %q", len(fields), expr)
	}
	ranges := []struct {
		min, max int
		name     string
	}{
		{0, 59, "秒"},
		{0, 59, "分"},
		{0, 23, "时"},
		{1, 31, "日"},
		{1, 12, "月"},
		{0, 7, "周"}, // 0/7 均表示周日
	}
	// 周 7 → 0
	fields[5] = strings.ReplaceAll(fields[5], "7", "0")
	var sets [6][]int
	full := true
	for i, f := range fields {
		set, err := parseField(f, ranges[i].min, ranges[i].max)
		if err != nil {
			return nil, fmt.Errorf("cron %s段非法: %w", ranges[i].name, err)
		}
		if len(set) != (ranges[i].max - ranges[i].min + 1) {
			full = false
		}
		sets[i] = set
	}
	return &Spec{
		seconds: sets[0], minutes: sets[1], hours: sets[2],
		days: sets[3], months: sets[4], weekdays: sets[5],
		full: full,
	}, nil
}

// parseField 解析单段（含 */n、a-b、列表、定值）。
func parseField(f string, min, max int) ([]int, error) {
	f = strings.TrimSpace(f)
	if f == "" {
		return nil, fmt.Errorf("空字段")
	}
	if f == "*" {
		return seq(min, max, 1), nil
	}
	// 逗号列表
	if strings.Contains(f, ",") {
		var out []int
		for _, part := range strings.Split(f, ",") {
			s, err := parseField(part, min, max)
			if err != nil {
				return nil, err
			}
			out = append(out, s...)
		}
		return dedup(out), nil
	}
	// 步长
	step := 1
	if idx := strings.Index(f, "/"); idx >= 0 {
		v, err := strconv.Atoi(f[idx+1:])
		if err != nil || v < 1 {
			return nil, fmt.Errorf("步长非法: %q", f)
		}
		step = v
		f = f[:idx]
		if f == "*" {
			return seq(min, max, step), nil
		}
	}
	// 范围或定值
	var lo, hi int
	if idx := strings.Index(f, "-"); idx >= 0 {
		a, err1 := strconv.Atoi(f[:idx])
		b, err2 := strconv.Atoi(f[idx+1:])
		if err1 != nil || err2 != nil {
			return nil, fmt.Errorf("范围非法: %q", f)
		}
		lo, hi = a, b
	} else {
		v, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("值非法: %q", f)
		}
		lo, hi = v, v
	}
	if lo < min || lo > max || hi < min || hi > max || lo > hi {
		return nil, fmt.Errorf("值越界: %q (范围 %d-%d)", f, min, max)
	}
	return seq(lo, hi, step), nil
}

func seq(lo, hi, step int) []int {
	var out []int
	for v := lo; v <= hi; v += step {
		out = append(out, v)
	}
	return out
}

func dedup(in []int) []int {
	m := map[int]bool{}
	var out []int
	for _, v := range in {
		if !m[v] {
			m[v] = true
			out = append(out, v)
		}
	}
	return out
}

// Next 返回 after 之后（严格大于）的下一次匹配时间；5 年内无解返回零值。
func (s *Spec) Next(after time.Time) time.Time {
	if s == nil {
		return time.Time{}
	}
	base := after.Truncate(time.Second)
	day := time.Date(base.Year(), base.Month(), base.Day(), 0, 0, 0, 0, base.Location())
	for i := 0; i < 365*5+2; i++ {
		if s.matchDay(day) {
			if cand := s.firstOn(day, base); !cand.IsZero() {
				return cand
			}
		}
		day = day.AddDate(0, 0, 1)
	}
	return time.Time{}
}

// firstOn 在匹配的某天内，返回严格晚于 base 的第一个时间点。
func (s *Spec) firstOn(day time.Time, base time.Time) time.Time {
	for _, h := range s.hours {
		for _, mi := range s.minutes {
			for _, sec := range s.seconds {
				cand := time.Date(day.Year(), day.Month(), day.Day(), h, mi, sec, 0, day.Location())
				if cand.After(base) {
					return cand
				}
			}
		}
	}
	return time.Time{}
}

func (s *Spec) matchDay(day time.Time) bool {
	mon := int(day.Month())
	dom := day.Day()
	dow := int(day.Weekday())
	if !contains(s.months, mon) {
		return false
	}
	// cron 语义：日月同有限时取「或」；仅一方有限时取该方。
	domAny := s.isFullRangeDays()
	hasDom := !domAny
	hasDow := !s.isFullRangeWeek()
	switch {
	case !hasDom && !hasDow:
		return true
	case hasDom && !hasDow:
		return contains(s.days, dom)
	case !hasDom && hasDow:
		return contains(s.weekdays, dow)
	default:
		return contains(s.days, dom) || contains(s.weekdays, dow)
	}
}

func (s *Spec) isFullRangeDays() bool {
	return len(s.days) == 31
}

func (s *Spec) isFullRangeWeek() bool {
	return len(s.weekdays) == 7
}

func contains(list []int, v int) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
