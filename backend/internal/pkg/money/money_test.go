package money

import (
	"math"
	"strings"
	"testing"
)

// TestValid 覆盖 Valid 的边界：NaN/±Inf、0、负数、上下界内外的超大值。
func TestValid(t *testing.T) {
	cases := []struct {
		name string
		v    float64
		want bool
	}{
		{"零", 0, true},
		{"正数", 123.456, true},
		{"负数", -123.456, true},
		{"正上界", MaxMagnitude, true},
		{"负下界", -MaxMagnitude, true},
		{"超上界", MaxMagnitude * 2, false},
		{"超下界", -MaxMagnitude * 2, false},
		{"最小正数", math.SmallestNonzeroFloat64, true},
		{"NaN", math.NaN(), false},
		{"正Inf", math.Inf(1), false},
		{"负Inf", math.Inf(-1), false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Valid(c.v); got != c.want {
				t.Fatalf("Valid(%v)=%v, want %v", c.v, got, c.want)
			}
		})
	}
}

// TestValidate 断言合法值返回 nil，非法值返回带字段名的错误。
func TestValidate(t *testing.T) {
	if err := Validate(1.5, "金额"); err != nil {
		t.Fatalf("合法值不应报错: %v", err)
	}
	if err := Validate(0, "金额"); err != nil {
		t.Fatalf("零不应报错: %v", err)
	}
	if err := Validate(-1.5, "金额"); err != nil {
		t.Fatalf("负数不应报错: %v", err)
	}
	if err := Validate(MaxMagnitude, "金额"); err != nil {
		t.Fatalf("上界不应报错: %v", err)
	}

	bad := []struct {
		name string
		v    float64
		what string
	}{
		{"NaN", math.NaN(), "计费积分"},
		{"正Inf", math.Inf(1), "计费积分"},
		{"负Inf", math.Inf(-1), "计费积分"},
		{"超上界", 1e15, "原始价值"},
		{"超下界", -1e15, "原始价值"},
	}
	for _, c := range bad {
		t.Run(c.name, func(t *testing.T) {
			err := Validate(c.v, c.what)
			if err == nil {
				t.Fatalf("Validate(%v) 应报错", c.v)
			}
			if !strings.Contains(err.Error(), c.what) {
				t.Fatalf("错误信息应包含字段名 %q: %v", c.what, err)
			}
		})
	}
}
