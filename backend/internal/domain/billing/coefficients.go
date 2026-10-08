package billing

import (
	"errors"
	"fmt"
	"time"
)

// TierRule 上下文分档规则。
type TierRule struct {
	Min   int64   `json:"Min"`
	Max   *int64  `json:"Max"` // nil 表示无上限
	Coeff float64 `json:"Coeff"`
}

// ContextTierCoeff 根据 inputTokens 落在的分档返回系数。
// 语义：Min 含端点；档与档之间用「上界含端点」衔接，保证 seed 边界
// （8K→S、8001→M、32K→M）符合预期，与测试/文档期望对齐。
// 未配置分档（空表）= 不启用分档，系数恒为 1；inputTokens 为负报错。
func ContextTierCoeff(inputTokens int64, tiers []TierRule) (float64, error) {
	if inputTokens < 0 {
		return 0, errors.New("input tokens 不能为负")
	}
	if len(tiers) == 0 {
		return 1, nil // 分档默认无：不启用分档
	}
	for _, t := range tiers {
		if inputTokens >= t.Min && (t.Max == nil || inputTokens <= *t.Max) {
			return t.Coeff, nil
		}
	}
	return 0, fmt.Errorf("inputTokens=%d 未命中任何上下文分档", inputTokens)
}

// Segment 一段时间段。
type Segment struct {
	Name  string  `json:"Name"`
	Start string  `json:"Start"` // "HH:MM"
	End   string  `json:"End"`   // "HH:MM"，"24:00" 表示日末
	Coeff float64 `json:"Coeff"`
}

// DateOverride 指定日期范围内的时段覆盖（忽略周期段）。
type DateOverride struct {
	Name     string    `json:"Name"`
	Start    string    `json:"Start"` // "YYYY-MM-DD"
	End      string    `json:"End"`
	Segments []Segment `json:"Segments"`
}

// TimeCoeffConfig 时段系数配置。
type TimeCoeffConfig struct {
	Timezone  string         `json:"Timezone"`
	Default   float64        `json:"Default"`
	Periodic  []Segment      `json:"Periodic"`
	Overrides []DateOverride `json:"Overrides"`
}

// parseTimeOfDay 解析 "HH:MM" 为距 0 点的分钟数（0..1439）。"24:00" 视为 1440（日末边界）。
func parseTimeOfDay(s string) (int, error) {
	if s == "24:00" {
		return 1440, nil
	}
	if s == "00:00" {
		return 0, nil
	}
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, fmt.Errorf("非法时段 %q: %w", s, err)
	}
	return t.Hour()*60 + t.Minute(), nil
}

// segHit 判断 nowMin（当日分钟）是否落在段 [start,end) 内。start>end 表示跨午夜。
func segHit(nowMin, start, end int) bool {
	if start == end {
		return false
	}
	if start < end {
		return nowMin >= start && nowMin < end
	}
	// 跨午夜，如 22:00-02:00。
	return nowMin >= start || nowMin < end
}

// bestSegCoeff 在一组段里找命中段；多个段重叠时取结束时间更晚的段（重叠兜底）。
func bestSegCoeff(nowMin int, segs []Segment) (float64, bool) {
	best, bestEnd, hit := 0.0, -1, false
	for _, sg := range segs {
		start, err := parseTimeOfDay(sg.Start)
		if err != nil {
			continue
		}
		end, err := parseTimeOfDay(sg.End)
		if err != nil {
			continue
		}
		if !segHit(nowMin, start, end) {
			continue
		}
		if !hit || end > bestEnd {
			hit, best, bestEnd = true, sg.Coeff, end
		}
	}
	return best, hit
}

// coeffFor 依据配置与某个时区下的本地时间，返回时段系数。
func (c TimeCoeffConfig) coeffFor(local time.Time) float64 {
	m := local.Hour()*60 + local.Minute()
	// 1) 日期覆盖
	for i := range c.Overrides {
		ov := &c.Overrides[i]
		if inDateRange(local, *ov) {
			if coeff, ok := bestSegCoeff(m, ov.Segments); ok {
				return coeff
			}
			// 该日在覆盖范围内但覆盖段未命中 → 跳出覆盖判断，回落周期段；周期段也未命中则落入 default。
			break
		}
	}
	// 2) 周期段
	if coeff, ok := bestSegCoeff(m, c.Periodic); ok {
		return coeff
	}
	// 3) 默认
	return c.Default
}

func inDateRange(local time.Time, ov DateOverride) bool {
	if len(ov.Start) != 10 || len(ov.End) != 10 {
		return false
	}
	// 用零填充的 YYYY-MM-DD 字符串直接比较日历日期，避免跨时区解析导致的日界错位。
	localDate := local.Format("2006-01-02")
	return ov.Start <= localDate && localDate <= ov.End
}

// CoeffFor 计算 now 对应的时段系数。先按 Timezone 转换到本地时区，
// 再优先匹配日期覆盖（落在覆盖日期内则忽略周期段），其次周期段，
// 最后返回 Default。时区非法返回 error。
func (c TimeCoeffConfig) CoeffFor(now time.Time) (float64, error) {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return 0, fmt.Errorf("加载时区 %q 失败: %w", c.Timezone, err)
	}
	return c.coeffFor(now.In(loc)), nil
}

// ValidateTimeConfig 校验时段配置：时区可加载、各时段起止时间合法且系数非负。
// 供对外/渠道内部模型创建与更新时前置校验使用（缺省配置由 CoeffFor 兜底）。
func ValidateTimeConfig(c TimeCoeffConfig) error {
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("加载时区 %q 失败: %w", c.Timezone, err)
	}
	if c.Default < 0 {
		return errors.New("default_coeff 不能为负")
	}
	for i := range c.Periodic {
		if err := validateSegment(c.Periodic[i]); err != nil {
			return fmt.Errorf("periodic_segments[%d]: %w", i, err)
		}
	}
	for i := range c.Overrides {
		ov := &c.Overrides[i]
		if len(ov.Start) != 10 || len(ov.End) != 10 {
			return fmt.Errorf("date_overrides[%d]: start/end 必须为 YYYY-MM-DD", i)
		}
		for j := range ov.Segments {
			if err := validateSegment(ov.Segments[j]); err != nil {
				return fmt.Errorf("date_overrides[%d].segments[%d]: %w", i, j, err)
			}
		}
	}
	return nil
}

func validateSegment(sg Segment) error {
	if sg.Coeff < 0 {
		return errors.New("coeff 不能为负")
	}
	if _, err := parseTimeOfDay(sg.Start); err != nil {
		return err
	}
	if _, err := parseTimeOfDay(sg.End); err != nil {
		return err
	}
	return nil
}

// ValidateTiers 校验上下文分档：每档 min>=0 且按 Min 单调非降（有序）。
func ValidateTiers(tiers []TierRule) error {
	prev := int64(-1)
	for i, t := range tiers {
		if t.Min < 0 {
			return fmt.Errorf("context_tiers[%d].min 不能为负", i)
		}
		if t.Coeff < 0 {
			return fmt.Errorf("context_tiers[%d].coeff 不能为负", i)
		}
		if t.Min < prev {
			return fmt.Errorf("context_tiers 未按 min 升序排列")
		}
		prev = t.Min
	}
	return nil
}
