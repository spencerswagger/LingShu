package subscription

import (
	"strings"
	"testing"
	"time"
)

func ptrInt(v int) *int    { return &v }
func ptrBool(v bool) *bool { return &v }

// L1 校验矩阵（标准 §3.1）。每条都要给出可定位到字段的 Reason=invalid_field。
func TestNormalizeAndValidate_Matrix(t *testing.T) {
	base := func() Input {
		return Input{Provider: "acme", Account: "acct-1", Price: 100, Currency: "CNY", Cycle: "month",
			PayDay: ptrInt(5), ShareCost: ptrBool(true), CycleStart: "2026-11-01"}
	}
	cases := []struct {
		name   string
		mutate func(in *Input)
		field  string
		wantOK bool
	}{
		{name: "合法最小集", mutate: func(*Input) {}, wantOK: true},
		{name: "省缺省值", mutate: func(in *Input) { *in = Input{Provider: "acme", Account: "a", Price: 1} }, wantOK: true},
		{name: "订阅方为空", mutate: func(in *Input) { in.Provider = "   " }, field: "Provider"},
		{name: "订阅方含换行", mutate: func(in *Input) { in.Provider = "a\nb" }, field: "Provider"},
		{name: "订阅方过长", mutate: func(in *Input) { in.Provider = strings.Repeat("x", 129) }, field: "Provider"},
		{name: "账号为空", mutate: func(in *Input) { in.Account = "" }, field: "Account"},
		{name: "账号过长", mutate: func(in *Input) { in.Account = strings.Repeat("x", 129) }, field: "Account"},
		{name: "价格为负", mutate: func(in *Input) { in.Price = -1 }, field: "Price"},
		{name: "价格超上限", mutate: func(in *Input) { in.Price = 1e9 + 1 }, field: "Price"},
		{name: "价格 6 位小数", mutate: func(in *Input) { in.Price = 1.123456 }, field: "Price"},
		{name: "0 元且参与分摊", mutate: func(in *Input) { in.Price = 0 }, field: "Price"},
		{name: "0 元停摊占位", mutate: func(in *Input) { in.Price = 0; in.ShareCost = ptrBool(false) }, wantOK: true},
		{name: "币种非法", mutate: func(in *Input) { in.Currency = "EUR" }, field: "Currency"},
		{name: "周期非法", mutate: func(in *Input) { in.Cycle = "week" }, field: "Cycle"},
		{name: "付款日 0", mutate: func(in *Input) { in.PayDay = ptrInt(0) }, field: "PayDay"},
		{name: "付款日 29", mutate: func(in *Input) { in.PayDay = ptrInt(29) }, field: "PayDay"},
		{name: "备注超 500 字", mutate: func(in *Input) { in.Note = strings.Repeat("字", 501) }, field: "Note"},
		{name: "账期起点格式错", mutate: func(in *Input) { in.CycleStart = "2026/11/01" }, field: "CycleStart"},
		{name: "账期起点过早", mutate: func(in *Input) { in.CycleStart = "1999-12-31" }, field: "CycleStart"},
		{name: "账期起点留空允许", mutate: func(in *Input) { in.CycleStart = "" }, wantOK: true},
		{name: "账期起点未来允许", mutate: func(in *Input) { in.CycleStart = "2030-01-01" }, wantOK: true},
		{name: "一次性周期允许", mutate: func(in *Input) { in.Cycle = "once" }, wantOK: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := base()
			tc.mutate(&in)
			got, verr := normalizeAndValidate(in)
			if tc.wantOK {
				if verr != nil {
					t.Fatalf("期望通过，得到错误：%+v", verr)
				}
				if got.PayDay < 1 || got.Cycle == "" || got.Currency == "" {
					t.Fatalf("缺省值未补齐：%+v", got)
				}
				return
			}
			if verr == nil {
				t.Fatalf("期望失败，实际通过：%+v", got)
			}
			if verr.Reason != ReasonInvalidField || verr.Status != 400 {
				t.Errorf("Reason/Status = %s/%d，期望 invalid_field/400", verr.Reason, verr.Status)
			}
			if tc.field != "" && verr.Field != tc.field {
				t.Errorf("Field = %q，期望 %q", verr.Field, tc.field)
			}
		})
	}
}

// 折算：积分 = 元 × 1e6 ÷ R；USD 先 × cny_rate（标准 §0 第 2 条，与 billing/engine.go 同源）。
func TestCreditsFor(t *testing.T) {
	cases := []struct {
		name     string
		price    float64
		currency string
		cnyRate  float64
		r        int64
		want     float64
		wantErr  bool
	}{
		{name: "100 元 R=10000", price: 100, currency: CurrencyCNY, r: 10000, want: 10000},
		{name: "USD 100 汇率 6.8", price: 100, currency: CurrencyUSD, cnyRate: 6.8, r: 10000, want: 68000},
		{name: "USD 缺汇率", price: 100, currency: CurrencyUSD, cnyRate: 0, r: 10000, wantErr: true},
		{name: "R 非法", price: 100, currency: CurrencyCNY, r: 0, wantErr: true},
		{name: "保持 5 位小数", price: 0.00001, currency: CurrencyCNY, r: 10000, want: 0.001},
		{name: "折算不足最小积分（round5 归零）", price: 0.00000001, currency: CurrencyCNY, r: 10000, want: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := creditsFor(tc.price, tc.currency, tc.cnyRate, tc.r)
			if tc.wantErr {
				if err == nil {
					t.Fatal("期望报错")
				}
				return
			}
			if err != nil {
				t.Fatalf("意外错误：%v", err)
			}
			if got != tc.want {
				t.Fatalf("credits = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// 折回原币只用于展示：10000 积分 / R=10000 = 100 元（2 位小数）。
func TestPriceFor(t *testing.T) {
	if got := priceFor(10000, CurrencyCNY, 0, 10000); got != 100 {
		t.Fatalf("priceFor = %v，期望 100", got)
	}
	if got := priceFor(68000, CurrencyUSD, 6.8, 10000); got != 100 {
		t.Fatalf("USD priceFor = %v，期望 100", got)
	}
}

// 派生状态优先级（标准 §2.3）：stopped → once → missing_cycle → pending → expired → active。
func TestStatusOf(t *testing.T) {
	today := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	start := func(y int, m time.Month, d int) *time.Time {
		v := time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
		return &v
	}
	cases := []struct {
		name      string
		shareCost bool
		cycle     string
		start     *time.Time
		want      string
	}{
		{"停摊优先于一切", false, CycleOnce, nil, StatusStopped},
		{"一次性", true, CycleOnce, start(2026, 11, 1), StatusOnce},
		{"缺账期起点", true, CycleMonth, nil, StatusMissingCycle},
		{"未生效", true, CycleMonth, start(2026, 12, 1), StatusPending},
		{"已过期（满 1 月）", true, CycleMonth, start(2026, 10, 16), StatusExpired},
		{"账期首日算生效", true, CycleMonth, start(2026, 11, 16), StatusActive},
		{"年付未过期", true, CycleYear, start(2026, 1, 1), StatusActive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := statusOf(tc.shareCost, tc.cycle, tc.start, today); got != tc.want {
				t.Fatalf("status = %s，期望 %s", got, tc.want)
			}
		})
	}
}

// 静态判定只覆盖 C1–C4：C5/C6 是运行时事实，不许在录入响应里承诺（标准 §5.3）。
func TestStaticVerdict(t *testing.T) {
	today := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	active := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	expired := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	pending := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)

	will, reasons := staticVerdict(true, 10000, CycleMonth, &active, today)
	if !will || len(reasons) != 0 {
		t.Fatalf("生效中的订阅应会摊：will=%v reasons=%v", will, reasons)
	}
	will, reasons = staticVerdict(false, 0, CycleOnce, nil, today)
	if will {
		t.Fatal("停摊不应会摊")
	}
	for _, want := range []string{ReasonShareOff, ReasonPriceZero, ReasonCycleOnce} {
		if !containsStr(reasons, want) {
			t.Errorf("reasons 缺少 %s：%v", want, reasons)
		}
	}
	if _, reasons = staticVerdict(true, 1, CycleMonth, nil, today); !containsStr(reasons, ReasonCycleMissing) {
		t.Errorf("缺账期起点应给 cycle_missing：%v", reasons)
	}
	if _, reasons = staticVerdict(true, 1, CycleMonth, &expired, today); !containsStr(reasons, ReasonCycleExpired) {
		t.Errorf("过期应给 cycle_expired：%v", reasons)
	}
	if will, reasons = staticVerdict(true, 1, CycleMonth, &pending, today); will || !containsStr(reasons, ReasonCyclePending) {
		t.Errorf("未生效：will=%v reasons=%v", will, reasons)
	}
}

func containsStr(list []string, v string) bool {
	for _, it := range list {
		if it == v {
			return true
		}
	}
	return false
}
