// Package money 提供金额数值的合法性与可存储范围校验：拦截 NaN/Inf 与超限值，
// 避免将非法金额写入 DB NUMERIC(20,5) 列导致序列化失败或溢出。
package money

import (
	"fmt"
	"math"
)

// MaxMagnitude 金额可存储的最大绝对值（NUMERIC(20,5) 列约 1e15，预留安全余量）。
const MaxMagnitude = 1e14

// Valid 判断金额是否在可存储范围内且非 NaN/Inf。
func Valid(v float64) bool {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return false
	}
	return v >= -MaxMagnitude && v <= MaxMagnitude
}

// Validate 校验金额；非法时返回带字段名的错误信息。
func Validate(v float64, what string) error {
	if !Valid(v) {
		return fmt.Errorf("%s 超出可存储范围（±%.0f）: %v", what, MaxMagnitude, v)
	}
	return nil
}
