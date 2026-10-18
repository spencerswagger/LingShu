package router

import "math"

// 会话成本决策层（Step 5）：纯函数，不产生任何 IO，供后续步骤接入路由热路径。
//
// 口径（单位：积分）：
//   - 价格 = 成本价 / 1e6 token（ChannelCost 各价字段为「每 1e6 token 的积分」）；
//   - mult = TimeCoeff × CtxCoeff（时段系数 × 上下文分档系数）；
//   - 每轮成本、重灌代价均按「样值用量代入三档成本单价 × mult / 1e6」保守计价：
//     缓存读按缓存读价、新输入按输入价、输出按输出价分别求和，不假设缓存命中率之外的折扣。
//
// 回本判据：把「重灌一次的开销」摊到「每轮省下的钱」上得 paybackTurns；
// 分母 ≤ 0 表示新渠道相对旧渠道每轮并不更便宜（或缓存读不更省），永远回不了本 → NaN，
// 调用方据此保持会话亲和、不切换。保守计价：省下的钱只按差异正项计入，避免高估回本速度。

// ChannelCost 单一渠道的计价视图（成本价，非售价）。
// Input/CacheRead/Output 为每 1e6 token 的积分；TimeCoeff/CtxCoeff 为时段与上下文分档系数。
type ChannelCost struct {
	Name      string
	Input     float64
	CacheRead float64
	Output    float64
	TimeCoeff float64
	CtxCoeff  float64
}

// SessionCostDecision 会话成本决策结果：是否切换、代价拆解、经验式剩余轮数与理由。
type SessionCostDecision struct {
	Switch           bool
	FromChannelKeyID int64
	ToChannelKeyID   int64
	TurnCostOld      float64
	TurnCostNew      float64
	RechargeCost     float64
	PaybackTurns     float64
	RemainingTurns   int
	Reason           string
}

// remainingTurns 经验式剩余轮数：k 为已进行的轮数（或某种负载指标），
// 结果 = clamp(RemainingA - RemainingB×k, RemainingMin, RemainingMax)。
// 夹取到 [RemainingMin, RemainingMax]，保证剩余轮数永远是有界正整数。
func remainingTurns(k int, cfg SessionCostConfig) int {
	v := cfg.RemainingA - cfg.RemainingB*float64(k)
	n := int(v)
	if n < cfg.RemainingMin {
		return cfg.RemainingMin
	}
	if n > cfg.RemainingMax {
		return cfg.RemainingMax
	}
	return n
}

// perTurnCost 单轮成本（积分）= (缓存读×缓存读价 + 新输入×输入价 + 输出×输出价) × mult / 1e6。
// mult = TimeCoeff × CtxCoeff。保守计价：不做任何未在单价中体现的折扣。
func perTurnCost(c ChannelCost, cacheReadTokens, newInputTokens, outputTokens int64) float64 {
	raw := float64(cacheReadTokens)*c.CacheRead +
		float64(newInputTokens)*c.Input +
		float64(outputTokens)*c.Output
	return raw * c.TimeCoeff * c.CtxCoeff / 1e6
}

// rechargeCost 切换渠道后重灌缓存的代价（积分）= 缓存读量 × (新输入价 − 新缓存读价) × mult / 1e6。
// 语义：原本可命中的缓存读，切到新渠道后需按新渠道「输入价」重新计入（其缓存尚未建立），
// 与新渠道「缓存读价」的差额即为每 token 的重灌溢价；乘新渠道 mult 后归一到积分。
func rechargeCost(cacheReadTokens int64, newCh ChannelCost) float64 {
	return float64(cacheReadTokens) * (newCh.Input - newCh.CacheRead) * newCh.TimeCoeff * newCh.CtxCoeff / 1e6
}

// paybackTurns 回本轮数：把重灌代价摊到每轮省下的成本上。
//
//	num = (新输入价 − 新缓存读价) × 缓存读量        （重灌一次的溢价）
//	den = (旧缓存读价 − 新缓存读价) × 缓存读量       （每轮缓存读省下的钱）
//	    + (旧输入价 − 新输入价) × 新输入量          （每轮新输入省下的钱）
//	    + (旧输出价 − 新输出价) × 输出量            （每轮输出省下的钱）
//
// den > 0 时返回 num/den；den ≤ 0 表示每轮并不更省 → 永远回不了本，返回 NaN。
func paybackTurns(cacheReadTokens int64, oldCh, newCh ChannelCost, newInputTokens, outputTokens int64) float64 {
	num := (newCh.Input - newCh.CacheRead) * float64(cacheReadTokens)
	den := (oldCh.CacheRead-newCh.CacheRead)*float64(cacheReadTokens) +
		(oldCh.Input-newCh.Input)*float64(newInputTokens) +
		(oldCh.Output-newCh.Output)*float64(outputTokens)
	if den > 0 {
		return num / den
	}
	return math.NaN()
}

// paybackLimit 回本轮数上限（与用量无关的经验阈值）= (新输入价 − 新缓存读价) / (旧缓存读价 − 新缓存读价)。
// 分母 ≤ 1e-12 表示旧渠道缓存读并不比新渠道贵 → 无处可省，返回 NaN（永不回本）。
func paybackLimit(oldCh, newCh ChannelCost) float64 {
	den := oldCh.CacheRead - newCh.CacheRead
	if den <= 1e-12 {
		return math.NaN()
	}
	return (newCh.Input - newCh.CacheRead) / den
}

// crossCacheRead 求「新渠道缓存读 vs 旧渠道缓存读」在给定用量下打平的缓存读量阈值：
// 令两渠道每轮成本相等解出缓存读量 =
//
//	(新输入量×(cj.Input−ci.Input) + 输出量×(cj.Output−ci.Output)) / (ci.CacheRead−cj.CacheRead)
//
// ci 为旧渠道、cj 为新渠道。分母 |ci.CacheRead−cj.CacheRead| ≤ 1e-12 时无解，返回 (0, false)。
func crossCacheRead(ci, cj ChannelCost, newInputTokens, outputTokens int64) (float64, bool) {
	den := ci.CacheRead - cj.CacheRead
	if math.Abs(den) <= 1e-12 {
		return 0, false
	}
	num := float64(newInputTokens)*(cj.Input-ci.Input) + float64(outputTokens)*(cj.Output-ci.Output)
	return num / den, true
}

// decideSessionCost 会话成本决策入口（纯函数）。
//
//   - cur == nil（起点：会话注册表 Lookup 未命中，尚无归属渠道）：直接切换/择优，
//     无重灌代价、无回本门控；Reason 含「起点」。
//   - cur != nil（中途：会话已有归属渠道）：先算回本 pb，仅当 pb 有效（非 NaN）
//     且经验式剩余轮数 > pb 才切换（else 保持亲和、不切换），Reason 说明门控结果。
//
// 返回代价拆解与理由，供决策轨迹排查。
func decideSessionCost(cfg SessionCostConfig, cur *ChannelCost, cand ChannelCost, cacheReadTokens, newInputTokens, outputTokens int64, k int) SessionCostDecision {
	rem := remainingTurns(k, cfg)
	if cur == nil {
		// 起点：无旧渠道可比较，直接采用候选；重灌代价/回本均为 0。
		return SessionCostDecision{
			Switch:         true,
			TurnCostNew:    perTurnCost(cand, cacheReadTokens, newInputTokens, outputTokens),
			RechargeCost:   0,
			PaybackTurns:   0,
			RemainingTurns: rem,
			Reason:         "起点（会话注册表未命中），直接择优采用候选渠道",
		}
	}

	pb := paybackTurns(cacheReadTokens, *cur, cand, newInputTokens, outputTokens)
	// 仅当回本有意义（pb 非 NaN）且剩余轮数严格大于回本轮数时才切换，
	// 否则保持会话亲和（切换无利可图或永远回不了本）。
	switchNow := !math.IsNaN(pb) && float64(rem) > pb

	reason := "中途：保持会话亲和（剩余轮数不足以回本）"
	if math.IsNaN(pb) {
		reason = "中途：保持会话亲和（分母≤0，永不回本）"
	} else if switchNow {
		reason = "中途：剩余轮数 > 回本轮数，切换更省"
	}

	return SessionCostDecision{
		Switch:         switchNow,
		TurnCostOld:    perTurnCost(*cur, cacheReadTokens, newInputTokens, outputTokens),
		TurnCostNew:    perTurnCost(cand, cacheReadTokens, newInputTokens, outputTokens),
		RechargeCost:   rechargeCost(cacheReadTokens, cand),
		PaybackTurns:   pb,
		RemainingTurns: rem,
		Reason:         reason,
	}
}

// shadowDecision 应用「影子模式」门控：开关关闭时不切换（渠道不变），
// 但仍保留完整的「假设」决策字段（TurnCostOld/TurnCostNew/RechargeCost/PaybackTurns/RemainingTurns）供记账；
// 开关开启时原样返回。
//
// 注意：门控刻意放在外层包装函数，而非 decideSessionCost 内部——后者保持「纯数学决策」语义
// （仅按回本判据给出是否切换），便于既有用例与 Step 10 接线复用。
func shadowDecision(cfg SessionCostConfig, d SessionCostDecision) SessionCostDecision {
	if !cfg.Enabled {
		d.Switch = false
		d.Reason = "影子模式(未启用，仅记录假设): " + d.Reason
	}
	return d
}
