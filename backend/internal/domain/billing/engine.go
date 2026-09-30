package billing

import (
	"errors"

	"github.com/team/llmgateway/internal/pkg/decimalx"
)

// ComputeCredits 计算一次调用消耗的积分：
// 五段 token 按倍率计费求和 × 时段系数 × 上下文分档系数 ÷ R，结果保留 5 位小数。
// 倍率(rate) = 人民币每百万 token 价格（无单位纯数字），照抄官网价录入；积分价值 V = R ÷ 1,000,000（每积分对应人民币元）由 R 派生，不单独存储。
// R = token 缩小分母（billing.r，默认 10000），即积分 = 按倍率求和后 ÷ R 的小数积分。
// rates 会经 ValidateRates 校验；r 必须大于 0。
func ComputeCredits(u Usage, rates Rates, timeCoeff, ctxCoeff float64, r int64) (float64, error) {
	if r <= 0 {
		return 0, errors.New("R(积分换算系数) 必须大于 0")
	}
	if err := ValidateRates(rates); err != nil {
		return 0, err
	}
	total := sumRates(u, rates)
	credits := total * timeCoeff * ctxCoeff / float64(r)
	return decimalx.Round5(credits), nil
}

// RawTotal 计算未除 R、仅乘系数的原始价值（保留 5 位小数），供记账展示/对账用。
func RawTotal(u Usage, rates Rates, timeCoeff, ctxCoeff float64) (float64, error) {
	if err := ValidateRates(rates); err != nil {
		return 0, err
	}
	return decimalx.Round5(sumRates(u, rates) * timeCoeff * ctxCoeff), nil
}
