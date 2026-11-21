package billing

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"time"

	"github.com/team/llmgateway/internal/domain/billing/mult"
	"github.com/team/llmgateway/internal/pkg/decimalx"
)

// ===== models.dev v2 cost 形状（cost_v2 / sale_v2 两列共用）=====
//
// 口径正本 = modelsdev-v2-billing-plan 设计报告（2026-11-20 spencer 四项定案）。
// 本文件是 v2 目录形状的**唯一解析与求值入口**（M1：读价收敛，写入口不切）。
//
// 存储形状（与 models.dev v2 cost 树同构，灵枢计价子集）：
//
//	{
//	  "text": {
//	    "input":  {"price": 1.25, "cache": {"read": 0.10, "write": 1.875}},
//	    "output": {"price": 10.0, "reasoning": 0}
//	  },
//	  "web_search": {"price": 25.0},
//	  "multipliers": [
//	    {"id": "night", "when": {"utc_hours": {"gte": "22:00"}}, "factor": 0.5}
//	  ]
//	}
//
// 单位 = 人民币/百万 token（与 sale_rates/cost_rates 同单位）：目录 USD 价必须先
// ×billing.cny_rate 折算再落列（历史红线：models.dev 美元数误当人民币出账虚低 6.8 倍）。
//
// 解析口径（钉死）：
//   - text.input.price / text.output.price / text.input.cache.read **必填**（缺任一 = 形状不完整，
//     调用方回落旧三段并告警——cache_read 是既有计费段，静默按 0 = 少收事故）；
//   - text.input.cache.write 可选，**缺省按 0 计**（四项定案①空价回落：宁少不多收，
//     运营逐模型填价后该段才产生金额）；
//   - text.output.reasoning 可选，仅解析留位（灵枢 usage 口径推理含在 output 段，0011 定调；
//     上游单列推理价的拆段属 usage 侧后续工作，M1 不单独计价）；
//   - web_search 照存不启用（四项定案③：预留给未来 MCP/工具调用计费）；
//   - image/audio/video/overrides 等未知节点一律容忍忽略（元数据属 catalog 暂存区职责）；
//   - multipliers 非法规则**逐条丢弃**并带 Warning 返回（mult 失败方向：按未命中处理，
//     绝不让一条坏规则打挂计费）。

// V2Cost 解析后的 v2 目录形状（灵枢计价子集）。
type V2Cost struct {
	Input       float64  // text.input.price
	Output      float64  // text.output.price（含推理，与 v2 output 同口径）
	CacheRead   float64  // text.input.cache.read
	CacheWrite  float64  // text.input.cache.write（缺省 0，空价回落）
	Reasoning   float64  // text.output.reasoning（可选留位，M1 不计价）
	WebSearch   *float64 // web_search.price 照存不启用
	Multipliers mult.Rules
	Warnings    []mult.Warning // 解析期被丢弃的规则及原因（调用方告警留痕）
}

// ParseV2Cost 解析 cost_v2 / sale_v2 JSONB。返回错误 = 形状不完整/非法，
// 调用方应**回落旧三段并记告警**（失败方向：解析失败不中断计费）。
func ParseV2Cost(raw []byte) (*V2Cost, error) {
	var tree struct {
		Text struct {
			Input struct {
				Price *float64         `json:"price"`
				Cache *json.RawMessage `json:"cache"`
			} `json:"input"`
			Output struct {
				Price    *float64         `json:"price"`
				Reasoned *json.RawMessage `json:"reasoning"`
			} `json:"output"`
		} `json:"text"`
		WebSearch *struct {
			Price *float64 `json:"price"`
		} `json:"web_search"`
		Multipliers json.RawMessage `json:"multipliers"`
	}
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil, fmt.Errorf("v2 cost 形状不是合法 JSON：%w", err)
	}
	// 必填三件套：text.input.price / text.output.price / text.input.cache.read。
	// 指针判存在：显式 0 是合法价（免费模型），缺失才是形状错误
	// （float64 分不清「缺」与「显式 0」，cache_read 是既有计费段，静默按 0 = 少收事故）。
	if tree.Text.Input.Price == nil || tree.Text.Output.Price == nil {
		return nil, errors.New("v2 缺 text.input.price / text.output.price（必填；免费请显式写 0）")
	}
	c := &V2Cost{Input: *tree.Text.Input.Price, Output: *tree.Text.Output.Price}
	if c.Input < 0 || c.Output < 0 {
		return nil, fmt.Errorf("v2 价不能为负：input=%v output=%v", c.Input, c.Output)
	}
	if tree.Text.Input.Cache == nil {
		return nil, errors.New("v2 缺 text.input.cache 节点（cache_read 必填；无缓存价请显式写 {\"read\":0}）")
	}
	{
		var cache struct {
			Read  *float64 `json:"read"`
			Write *float64 `json:"write"`
		}
		if err := json.Unmarshal(*tree.Text.Input.Cache, &cache); err != nil {
			return nil, fmt.Errorf("v2 cache 节点形状不合法：%w", err)
		}
		if cache.Read == nil {
			return nil, errors.New("v2 cache.read 必填（cache_read 是既有计费段，缺省=静默少收事故；无缓存价请显式写 0）")
		}
		if *cache.Read < 0 || (cache.Write != nil && *cache.Write < 0) {
			return nil, errors.New("v2 缓存价不能为负")
		}
		c.CacheRead = *cache.Read
		if cache.Write != nil {
			c.CacheWrite = *cache.Write
		}
	}
	if tree.Text.Output.Reasoned != nil {
		var r float64
		if err := json.Unmarshal(*tree.Text.Output.Reasoned, &r); err == nil && r > 0 {
			c.Reasoning = r
		}
	}
	if tree.WebSearch != nil && tree.WebSearch.Price != nil {
		p := *tree.WebSearch.Price
		c.WebSearch = &p
	}
	// 键缺省（raw 为空）= 合法「无规则」，不进 ParseRules——否则空输入也会触发
	// 「非数组」告警，每个干净形状都背一条噪音（真值非数组仍由 ParseRules 告警）。
	if len(tree.Multipliers) > 0 {
		c.Multipliers, c.Warnings = mult.ParseRules(tree.Multipliers)
	}
	return c, nil
}

// ===== 求值输入（RecordReq.V2 / RecordReq.CostV2）=====

// V2Prices 四段价（人民币/百万 token）。CacheWrite 缺省 0 = 空价回落口径。
type V2Prices struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
}

// V2Pricing 一次 v2 计价的输入与留痕输出。
//   - Sale 侧（RecordReq.V2）：ServiceTier 恒空（四项定案②：售卖层不做 tier 差价，
//     magpie 不感知该字段）；Region 现网无来源、恒空。
//   - Cost 侧（RecordReq.CostV2）：ServiceTier 预留账本归因位（当前 v3 账本无该字段，
//     传空=相关规则不命中），Region 同上。
//   - Eval 是**求值输出**：requestCredits / 成本分支求值后填充，供 price_eval 留痕。
type V2Pricing struct {
	Prices      V2Prices
	Rules       mult.Rules
	EvalAt      time.Time         // 求值时刻（v3=账本 occurred_at；预扣估算=now），一律按 UTC 判定
	ServiceTier string            // 成本层条件因子入参；空=未带（相关规则不命中）
	Region      string            // 空=未知（相关规则不命中）
	Holidays    mult.HolidayTable // nil=未配节假日表（相关规则不命中）
	Eval        mult.Result
	evaluated   bool // 求值收口置位：区分「已求值」与「幂等命中未求值」（留痕只在求值后写）
}

// Evaluated 报告本输入是否已经过求值收口（requestCreditsV2 / Record 成本分支）。
// 幂等命中（Record 直接返回既有行）时为 false——price_eval 留痕只在真求值后写，
// 否则会用空求值结果覆盖首次结算写入的留痕。
func (v *V2Pricing) Evaluated() bool { return v.evaluated }

// SetEvaluatedForTest 测试脚手架：在包外模拟「求值收口已发生」的下游形态
// （生产代码只在 requestCreditsV2 / Record 成本分支置位，别处不得调用）。
func (v *V2Pricing) SetEvaluatedForTest() { v.evaluated = true }

// PricingFromV2Cost 由解析结果构造求值输入（evalAt 必填）。
func PricingFromV2Cost(c *V2Cost, evalAt time.Time) *V2Pricing {
	return &V2Pricing{
		Prices: V2Prices{
			Input:      c.Input,
			Output:     c.Output,
			CacheRead:  c.CacheRead,
			CacheWrite: c.CacheWrite, // 空价回落：缺省 0，该段不产生金额
		},
		Rules:  c.Multipliers,
		EvalAt: evalAt,
	}
}

// ValidateV2Prices 校验四段价非负有限。
func ValidateV2Prices(p V2Prices) error {
	for name, v := range map[string]float64{
		"input": p.Input, "output": p.Output, "cache_read": p.CacheRead, "cache_write": p.CacheWrite,
	} {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("v2 价 %q 非有限数", name)
		}
		if v < 0 {
			return fmt.Errorf("v2 价 %q 不能为负", name)
		}
	}
	return nil
}

// evalContextTokens 求值上下文的 context 键取值（与 ContextTierCoeff 同源：input+cache_read）。
func evalContextTokens(u Usage) int64 { return u.Input + u.CacheRead }

// EvaluateV2 求值一组规则（求值时刻一律 UTC）。纯函数，供售价/成本两个挂载点同用。
func EvaluateV2(v2 *V2Pricing, u Usage) mult.Result {
	return mult.Eval(v2.Rules, mult.Ctx{
		At:          v2.EvalAt,
		Context:     evalContextTokens(u),
		ServiceTier: v2.ServiceTier,
		Region:      v2.Region,
		Holidays:    v2.Holidays,
	})
}

// ComputeCreditsV2 v2 计价公式：四段 token×段价之和 × 命中因子连乘 ÷ R，Round5。
// 与 ComputeCredits 的关系：v2 路径不叠加 time_config/context_tiers
// （其时间与上下文条件由 multipliers 吸收），legacy 路径公式逐字节不变。
// token 分桶互斥（四项定案①）：同一 token 只落 input/cache_write/cache_read 一个桶，
// 直接求和即可，无重复累计。
func ComputeCreditsV2(u Usage, p V2Prices, factor float64, r int64) (float64, error) {
	if r <= 0 {
		return 0, errors.New("R(积分换算系数) 必须大于 0")
	}
	if err := ValidateV2Prices(p); err != nil {
		return 0, err
	}
	total := float64(u.Input)*p.Input +
		float64(u.Output)*p.Output +
		float64(u.CacheRead)*p.CacheRead +
		float64(u.CacheWrite)*p.CacheWrite
	return decimalx.Round5(total * factor / float64(r)), nil
}

// RawTotalV2 计算未除 R 的原始价值（对账展示），与 ComputeCreditsV2 同口径。
func RawTotalV2(u Usage, p V2Prices, factor float64) (float64, error) {
	if err := ValidateV2Prices(p); err != nil {
		return 0, err
	}
	total := float64(u.Input)*p.Input +
		float64(u.Output)*p.Output +
		float64(u.CacheRead)*p.CacheRead +
		float64(u.CacheWrite)*p.CacheWrite
	return decimalx.Round5(total * factor), nil
}

// LegacyView 把 v2 四价折成旧三段视图（rates 列的存储形状，console 渲染兼容）：
// input/output/cache_read 三键，不含 cache_write 与因子（完整口径看 price_eval 留痕）。
func (p V2Prices) LegacyView() Rates {
	return Rates{"input": p.Input, "output": p.Output, "cache_read": p.CacheRead}
}

// V2PriceEval price_eval 列的留痕形状（v2 计价行才有；旧行 NULL）。
// 对账可复算：四价 × usage × 因子连乘 ÷ R == credits_consumed。
// 成本层同刻独立求值（四项定案②④：multipliers 成本挂 internal_models、售价挂
// external_models，两层规则可不同），只在存在时随留痕一并落库。
type V2PriceEval struct {
	Kind    string     `json:"kind"` // 恒 "v2"（向后兼容位：将来其它留痕形状不混读）
	At      time.Time  `json:"at"`   // 求值时刻（UTC）
	Prices  V2Prices   `json:"prices"`
	Context int64      `json:"context"` // input+cache_read
	Factors []mult.Hit `json:"factors"`
	Factor  float64    `json:"factor"` // text 模态最终连乘（无命中 = 1）
	R       int64      `json:"r"`
	// 成本层（可空）：cost_v2 命中时一并留痕，毛利可复算。
	CostPrices  *V2Prices  `json:"cost_prices,omitempty"`
	CostFactors []mult.Hit `json:"cost_factors,omitempty"`
	CostFactor  *float64   `json:"cost_factor,omitempty"`
}

// PriceEvalTrace 由求值结果构造留痕（settleOne 落 price_eval 列用）。
// cost 为 nil 或未经求值（幂等命中）时不写成本层。
func PriceEvalTrace(sale *V2Pricing, cost *V2Pricing, u Usage, r int64) V2PriceEval {
	e := V2PriceEval{
		Kind:    "v2",
		At:      sale.EvalAt.UTC(),
		Prices:  sale.Prices,
		Context: evalContextTokens(u),
		Factors: sale.Eval.Hits,
		Factor:  sale.Eval.FactorFor("text"),
		R:       r,
	}
	if cost != nil && cost.Evaluated() {
		cp := cost.Prices
		cf := cost.Eval.FactorFor("text")
		e.CostPrices = &cp
		e.CostFactors = cost.Eval.Hits
		e.CostFactor = &cf
	}
	return e
}
