package billing

import (
	"strings"
	"testing"
	"time"
)

func fullRates() Rates {
	return Rates{"input": 1.0, "output": 2.0, "cache_read": 0.1, "cache_write": 0.3, "reasoning": 1.0}
}

func TestComputeCredits(t *testing.T) {
	tests := []struct {
		name      string
		u         Usage
		rates     Rates
		timeCoeff float64
		ctxCoeff  float64
		r         int64
		want      float64
		wantErr   bool
	}{
		{
			// 产品文档示例：五段求和 = 1234×1 + 567×2 = 2368；÷10000 = 0.2368（文档 §2.1.2 给出 0.2368）
			name: "文档示例", u: Usage{Input: 1234, Output: 567}, rates: fullRates(),
			timeCoeff: 1.0, ctxCoeff: 1.0, r: 10000, want: 0.2368,
		},
		{
			name: "文档示例-时段系数0.5", u: Usage{Input: 1234, Output: 567}, rates: fullRates(),
			timeCoeff: 0.5, ctxCoeff: 1.0, r: 10000, want: 0.1184, // 0.2368 × 0.5
		},
		{
			// 缓存命中段参与计费：1000×1 + 100×0.1 = 1010；÷1000 = 1.01
			name: "cache_read参与", u: Usage{Input: 1000, CacheRead: 100}, rates: fullRates(),
			timeCoeff: 1.0, ctxCoeff: 1.0, r: 1000, want: 1.01,
		},
		{
			name: "cache_write-缓存回写", u: Usage{CacheWrite: 5000}, rates: fullRates(),
			timeCoeff: 1.2, ctxCoeff: 1.5, r: 10000, want: 0.27, // 5000×0.3×1.2×1.5/10000=0.27
		},
		{
			name: "r非正数报错", u: Usage{Input: 10}, rates: fullRates(),
			timeCoeff: 1.0, ctxCoeff: 1.0, r: 0, wantErr: true,
		},
		{
			name: "缺键报错", u: Usage{Input: 10}, rates: Rates{"input": 1.0},
			timeCoeff: 1.0, ctxCoeff: 1.0, r: 10000, wantErr: true,
		},
		{
			name: "负单价格报错", u: Usage{Input: 10}, rates: Rates{"input": -1.0, "output": 1, "cache_read": 1, "cache_write": 1, "reasoning": 1},
			timeCoeff: 1.0, ctxCoeff: 1.0, r: 10000, wantErr: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ComputeCredits(tc.u, tc.rates, tc.timeCoeff, tc.ctxCoeff, tc.r)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ComputeCredits: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func defaultTierRules() []TierRule {
	eightK, thirty2K, one28K := int64(8000), int64(32000), int64(128000)
	return []TierRule{
		{Min: 0, Max: &eightK, Coeff: 1.0},
		{Min: eightK, Max: &thirty2K, Coeff: 1.2},
		{Min: thirty2K, Max: &one28K, Coeff: 1.5},
		{Min: one28K, Max: nil, Coeff: 2.0},
	}
}

func TestContextTierCoeff(t *testing.T) {
	tiers := defaultTierRules()
	tests := []struct {
		name string
		in   int64
		ts   []TierRule
		want float64
		err  bool
	}{
		{name: "8K归S", in: 8000, ts: tiers, want: 1.0},
		{name: "8001归M", in: 8001, ts: tiers, want: 1.2},
		{name: "32K归M", in: 32000, ts: tiers, want: 1.2},
		{name: "128001归XL", in: 128001, ts: tiers, want: 2.0},
		{name: "15K归M", in: 15000, ts: tiers, want: 1.2},
		{name: "负输入报错", in: -1, ts: tiers, err: true},
		{name: "空档位不启用系数1", in: 100, ts: nil, want: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ContextTierCoeff(tc.in, tc.ts)
			if tc.err {
				if err == nil {
					t.Fatalf("期望报错，got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ContextTierCoeff: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func shLoc(t *testing.T) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	return loc
}

func TestTimeCoeff_CoeffFor(t *testing.T) {
	seg := func(name, start, end string, coeff float64) Segment {
		return Segment{Name: name, Start: start, End: end, Coeff: coeff}
	}

	// 与 seed 相同的低谷/普通/高峰配置。
	periodicCfg := TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1.0, Periodic: []Segment{
		seg("低谷", "00:00", "08:00", 0.5),
		seg("普通", "08:00", "18:00", 1.0),
		seg("高峰", "18:00", "22:00", 1.5),
	}}

	tests := []struct {
		name    string
		cfg     TimeCoeffConfig
		now     time.Time
		want    float64
		wantErr bool
	}{
		{
			name: "低谷03:00", cfg: periodicCfg,
			now: time.Date(2026, 1, 2, 3, 0, 0, 0, shLoc(t)), want: 0.5,
		},
		{
			name: "普通12:00", cfg: periodicCfg,
			now: time.Date(2026, 1, 2, 12, 0, 0, 0, shLoc(t)), want: 1.0,
		},
		{
			name: "高峰20:00", cfg: periodicCfg,
			now: time.Date(2026, 1, 2, 20, 0, 0, 0, shLoc(t)), want: 1.5,
		},
		{
			name: "未命中回落默认",
			cfg:  TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1.0, Periodic: []Segment{seg("白天", "08:00", "10:00", 0.9)}},
			now:  time.Date(2026, 1, 2, 15, 0, 0, 0, shLoc(t)), want: 1.0,
		},
		{
			name: "跨午夜段23:00命中22-02",
			cfg:  TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1.0, Periodic: []Segment{seg("夜", "22:00", "02:00", 0.8)}},
			now:  time.Date(2026, 1, 2, 23, 0, 0, 0, shLoc(t)), want: 0.8,
		},
		{
			name: "跨午夜段01:00命中22-02",
			cfg:  TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1.0, Periodic: []Segment{seg("夜", "22:00", "02:00", 0.8)}},
			now:  time.Date(2026, 1, 3, 1, 0, 0, 0, shLoc(t)), want: 0.8,
		},
		{
			name: "重叠段取结束更晚者",
			cfg:  TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1.0, Periodic: []Segment{seg("长段", "00:00", "10:00", 0.5), seg("短段", "00:00", "06:00", 0.9)}},
			now:  time.Date(2026, 1, 2, 3, 0, 0, 0, shLoc(t)), want: 0.5,
		},
		{
			name: "日期覆盖全天0.8",
			cfg: TimeCoeffConfig{
				Timezone: "Asia/Shanghai", Default: 1.0,
				Periodic: []Segment{seg("普通", "00:00", "24:00", 1.0)},
				Overrides: []DateOverride{{
					Name: "国庆", Start: "2026-10-01", End: "2026-10-01",
					Segments: []Segment{seg("全天", "00:00", "24:00", 0.8)},
				}},
			},
			now: time.Date(2026, 10, 1, 12, 0, 0, 0, shLoc(t)), want: 0.8,
		},
		{
			name: "日期覆盖未命中回落周期",
			cfg: TimeCoeffConfig{
				Timezone: "Asia/Shanghai", Default: 1.0,
				Periodic: []Segment{seg("普通", "00:00", "24:00", 1.0)},
				Overrides: []DateOverride{{
					Name: "国庆", Start: "2026-10-01", End: "2026-10-02",
					Segments: []Segment{seg("高峰", "18:00", "22:00", 1.5)},
				}},
			},
			now: time.Date(2026, 10, 2, 9, 0, 0, 0, shLoc(t)), want: 1.0,
		},
		{
			name: "非法时区报错", cfg: TimeCoeffConfig{Timezone: "Not/AZone"},
			now: time.Now(), wantErr: true,
		},
		// ---- Weekdays（星期差异化） ----
		// 2026-01-03 是周六、2026-01-05 是周一（Asia/Shanghai）。
		{
			name: "周末高峰1.5-周六命中",
			cfg: TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1.0, Periodic: []Segment{
				{Name: "周末高峰", Start: "18:00", End: "22:00", Coeff: 1.5, Weekdays: []time.Weekday{time.Saturday}},
				{Name: "工作日高峰", Start: "18:00", End: "22:00", Coeff: 2.0, Weekdays: []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}},
			}},
			now: time.Date(2026, 1, 3, 20, 0, 0, 0, shLoc(t)), want: 1.5,
		},
		{
			name: "工作日高峰2.0-周一命中",
			cfg: TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1.0, Periodic: []Segment{
				{Name: "周末高峰", Start: "18:00", End: "22:00", Coeff: 1.5, Weekdays: []time.Weekday{time.Saturday}},
				{Name: "工作日高峰", Start: "18:00", End: "22:00", Coeff: 2.0, Weekdays: []time.Weekday{time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday}},
			}},
			now: time.Date(2026, 1, 5, 20, 0, 0, 0, shLoc(t)), want: 2.0,
		},
		{
			name: "周末外时段回落默认-周日",
			cfg: TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1.0, Periodic: []Segment{
				{Name: "周末全天0.8", Start: "00:00", End: "24:00", Coeff: 0.8, Weekdays: []time.Weekday{time.Saturday, time.Sunday}},
			}},
			now: time.Date(2026, 1, 5, 12, 0, 0, 0, shLoc(t)), want: 1.0,
		},
		{
			name: "跨午夜段按当前周日判定不命中周一段",
			cfg: TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1.0, Periodic: []Segment{
				{Name: "周六夜", Start: "22:00", End: "02:00", Coeff: 0.8, Weekdays: []time.Weekday{time.Saturday}},
			}},
			now: time.Date(2026, 1, 5, 1, 0, 0, 0, shLoc(t)), want: 1.0, // 周一 01:00 属周一，不命中周六夜段
		},
		{
			name: "跨午夜段周六01:00命中",
			cfg: TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1.0, Periodic: []Segment{
				{Name: "周五夜", Start: "22:00", End: "02:00", Coeff: 0.8, Weekdays: []time.Weekday{time.Saturday}},
			}},
			now: time.Date(2026, 1, 3, 1, 0, 0, 0, shLoc(t)), want: 0.8, // 周六 01:00 属周六，命中
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.cfg.CoeffFor(tc.now)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("期望报错，got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("CoeffFor: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestValidateRates(t *testing.T) {
	if err := ValidateRates(fullRates()); err != nil {
		t.Fatalf("完整单价应通过: %v", err)
	}
	if err := ValidateRates(Rates{"input": 1}); err == nil || !strings.Contains(err.Error(), "output") {
		t.Fatalf("缺键应报错，got %v", err)
	}
}

func TestValidateTimeConfig_Weekdays(t *testing.T) {
	good := TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1.0, Periodic: []Segment{
		{Name: "工作日高峰", Start: "18:00", End: "22:00", Coeff: 1.5, Weekdays: []time.Weekday{time.Monday, time.Saturday}},
	}}
	if err := ValidateTimeConfig(good); err != nil {
		t.Fatalf("合法 Weekdays 应通过: %v", err)
	}
	bad := TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1.0, Periodic: []Segment{
		{Name: "非法星期", Start: "18:00", End: "22:00", Coeff: 1.5, Weekdays: []time.Weekday{7}},
	}}
	if err := ValidateTimeConfig(bad); err == nil || !strings.Contains(err.Error(), "weekday") {
		t.Fatalf("非法 Weekdays 应报错，got %v", err)
	}
	// 日期覆盖段同样受 Weekdays 校验约束。
	badOverride := TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1.0,
		Overrides: []DateOverride{{
			Name: "国庆", Start: "2026-10-01", End: "2026-10-01",
			Segments: []Segment{{Name: "全天", Start: "00:00", End: "24:00", Coeff: 0.8, Weekdays: []time.Weekday{-1}}},
		}},
	}
	if err := ValidateTimeConfig(badOverride); err == nil {
		t.Fatal("覆盖段非法 Weekdays 应报错")
	}
}
