package decimalx

import "math"

// Round5 保留 5 位小数（积分精度）
func Round5(f float64) float64 {
	return math.Round(f*1e5) / 1e5
}

// Round2 保留 2 位小数（余额展示）
func Round2(f float64) float64 {
	return math.Round(f*1e2) / 1e2
}