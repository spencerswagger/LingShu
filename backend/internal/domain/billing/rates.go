// Package billing 提供大模型 API 调用的计费引擎（五段计费公式、上下文分档、
// 时段系数、记账与钱包扣减）。本包只依赖接口，不耦合具体领域实现。
package billing

import "fmt"

// Usage 一次调用的五段 token 用量（单位：个）。
type Usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Reasoning  int64 `json:"reasoning"`
}

// Rates 五段 token 的倍率表（倍率 = 人民币每百万 token 价格，无单位纯数字），键只能是五个 rateKey。
type Rates map[string]float64

// rateKeys 计费单价必须包含的五个键（售价/成本通用，与 external_models.sale_rates、
// channel_models.cost_rates 的键保持一致）。
var rateKeys = []string{"input", "output", "cache_read", "cache_write", "reasoning"}

// ValidateRates 校验单价表：键齐全且均非负。
func ValidateRates(r Rates) error {
	for _, k := range rateKeys {
		v, ok := r[k]
		if !ok {
			return fmt.Errorf("计费单价缺少键 %q", k)
		}
		if v < 0 {
			return fmt.Errorf("计费单价 %q 不能为负", k)
		}
	}
	return nil
}

// sumRates 计算五段 token×单价 之和。
func sumRates(u Usage, r Rates) float64 {
	return float64(u.Input)*r["input"] +
		float64(u.Output)*r["output"] +
		float64(u.CacheRead)*r["cache_read"] +
		float64(u.CacheWrite)*r["cache_write"] +
		float64(u.Reasoning)*r["reasoning"]
}
