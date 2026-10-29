// cost_trace.go 会话渠道轨迹（只读）：从 billing_records 按 session_id 还原「每次调用落在哪个渠道/哪个模型/花了多少」。
//
// 口径（全部只读，不改计费、不改路由，也不把引擎决策落库）：
//   - 数据源只有 billing_records：session_id / channel_key_id / internal_model_id / cost_credits 等；
//   - 渠道 = channel_keys JOIN channels（channel_key_id 可空：拒绝类账单没有渠道维度）；
//   - 渠道内部模型 = 按 (channel_id, internal_model_id) 解析 channel_models（与成本统计同一解析式）；
//   - 金额直接取账单落库值（cost_credits / credits_consumed），不重算。
//
// 两类「切换点」在结构上就分开，且各带 Kind 枚举，避免前端或用户混淆：
//   - 真实切换点（ActualSwitches，Kind="actual"）：会话里渠道真的变过，取相邻记录 channel_id 变化处；
//   - 预测打平点（PredictedBreakEven，Kind="predicted"）：会话**从未切过**渠道时才给，
//     按同一内部模型在其它渠道的成本曲线与实际渠道曲线的交叉点推算（明确是「预测」，不是事实）。
package console

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/decimalx"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// 切换点类型枚举：响应里同时出现在两个数组/对象各自的 Kind 字段，前端据此着色与文案。
const (
	traceKindActual    = "actual"    // 真实切换点（账单事实）
	traceKindPredicted = "predicted" // 预测打平点（按成本曲线推算，非事实）
)

// costTracePoint 轨迹上的一个点（= 一条 billing_records，按 call_time 升序）。
//
// Turn / TurnNo 的分工与 /cost/session/{id} 完全一致：
//   - Turn 只给成功调用编号（失败轮为 0），老前端按 Turn 读不会错位；
//   - TurnNo 是时间线序号（含失败轮），x 轴用这个。
type costTracePoint struct {
	Turn                  int     `json:"Turn"`
	TurnNo                int     `json:"TurnNo"`
	Status                string  `json:"Status"`
	Failed                bool    `json:"Failed"`
	CallTime              string  `json:"CallTime"`
	ChannelID             int64   `json:"ChannelID"`
	ChannelName           string  `json:"ChannelName"`
	ChannelModelID        int64   `json:"ChannelModelID"`
	InternalModelID       string  `json:"InternalModelID"`
	InputTokens           int64   `json:"InputTokens"`
	CacheReadTokens       int64   `json:"CacheReadTokens"`
	OutputTokens          int64   `json:"OutputTokens"`
	CostCredits           float64 `json:"CostCredits"`
	SaleCredits           float64 `json:"SaleCredits"`
	CumulativeCostCredits float64 `json:"CumulativeCostCredits"` // 到本点为止的累计成本积分（含失败轮，失败轮为 0）
	// ChannelChanged 真实切换点标记：本点渠道 ≠ 上一点渠道（第一点恒 false）。
	ChannelChanged bool `json:"ChannelChanged"`
}

// costTraceSegment 一段连续同渠道同模型的轨迹（相邻点渠道或模型变了就开新段）。
type costTraceSegment struct {
	Seq             int     `json:"Seq"`
	ChannelID       int64   `json:"ChannelID"`
	ChannelName     string  `json:"ChannelName"`
	ChannelModelID  int64   `json:"ChannelModelID"`
	InternalModelID string  `json:"InternalModelID"`
	FromCallTime    string  `json:"FromCallTime"`
	ToCallTime      string  `json:"ToCallTime"`
	Points          int     `json:"Points"`
	SuccessPoints   int     `json:"SuccessPoints"`
	CostCredits     float64 `json:"CostCredits"`
	SaleCredits     float64 `json:"SaleCredits"`
	InputTokens     int64   `json:"InputTokens"`
	CacheReadTokens int64   `json:"CacheReadTokens"`
	OutputTokens    int64   `json:"OutputTokens"`
}

// costTraceSwitch 真实切换点（Kind 恒为 "actual"）。
type costTraceSwitch struct {
	Kind                string `json:"Kind"`
	Seq                 int    `json:"Seq"`        // 第几次切换（1..N）
	TurnNo              int    `json:"TurnNo"`     // 切换后第一轮的时间线序号
	AtCallTime          string `json:"AtCallTime"` // 切换后第一条记录的时刻
	FromChannelID       int64  `json:"FromChannelID"`
	FromChannelName     string `json:"FromChannelName"`
	FromChannelModelID  int64  `json:"FromChannelModelID"`
	FromInternalModelID string `json:"FromInternalModelID"`
	ToChannelID         int64  `json:"ToChannelID"`
	ToChannelName       string `json:"ToChannelName"`
	ToChannelModelID    int64  `json:"ToChannelModelID"`
	ToInternalModelID   string `json:"ToInternalModelID"`
}

// costTraceBreakEven 预测打平点（Kind 恒为 "predicted"，Predicted 恒 true）。
//
// 语义：会话实际一直用 ChannelModelID 这一条渠道；在 Tokens 这个上下文长度上，
// AlternativeChannelModelID 这条渠道的单次成本**开始低于**实际渠道（打平后反超）。
// Matched=false 表示采样范围内没出现反超，Tokens 为 0。
type costTraceBreakEven struct {
	Kind                      string `json:"Kind"`
	Predicted                 bool   `json:"Predicted"`
	ChannelModelID            int64  `json:"ChannelModelID"`
	ChannelID                 int64  `json:"ChannelID"`
	ChannelName               string `json:"ChannelName"`
	InternalModelID           string `json:"InternalModelID"`
	AlternativeChannelModelID int64  `json:"AlternativeChannelModelID"`
	AlternativeChannelID      int64  `json:"AlternativeChannelID"`
	AlternativeChannelName    string `json:"AlternativeChannelName"`
	Tokens                    int64  `json:"Tokens"` // 打平点上下文长度（输入 + 缓存读）
	Matched                   bool   `json:"Matched"`
	Step                      int64  `json:"Step"`
	MaxTokens                 int64  `json:"MaxTokens"`
	Reason                    string `json:"Reason"`
}

// costTraceResponse 会话渠道轨迹响应。
//
// PredictedBreakEven 为 null 的两种情形：① 会话里已经真实切换过渠道（那就给 ActualSwitches，不再预测）；
// ② 算不出打平点（成本费率缺失 / 采样范围内无交叉）。具体原因见其 Reason 字段。
type costTraceResponse struct {
	SessionID           string              `json:"SessionID"`
	Points              []costTracePoint    `json:"Points"`
	Segments            []costTraceSegment  `json:"Segments"`
	ActualSwitches      []costTraceSwitch   `json:"ActualSwitches"`
	PredictedBreakEven  *costTraceBreakEven `json:"PredictedBreakEven"`
	Total               int64               `json:"Total"`
	Limit               int                 `json:"Limit"`
	Truncated           bool                `json:"Truncated"`
	SwitchKindActual    string              `json:"SwitchKindActual"`
	SwitchKindPredicted string              `json:"SwitchKindPredicted"`
}

// HandleCostSessionTrace GET /api/v1/admin/cost/session/{id}/trace
//
// 返回会话按时间排序的渠道轨迹：逐点渠道/模型/积分 → 连续段 → 真实切换点；
// 从未切换过渠道时额外给一个**预测打平点**（Kind="predicted"，与真实切换点字段与枚举都不同）。
//
// 参数：Limit（可选，语义与 /cost/session/{id} 完全一致：缺省不截断、显式上限 2000）；
// Step / MaxTokens / HitRate / InOutRatio / At（可选，只影响预测打平点的采样，与定价曲线同源）。
//
// 受功能开关 console.cost_multichannel_trace 控制：关闭时返回 403（默认关闭 = 现状不变）。
func (h *Admin) HandleCostSessionTrace(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	if sid == "" {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "缺少会话 ID")
		return
	}
	if !h.costViewsEnabled(r.Context()) {
		resp.Err(w, r, http.StatusForbidden, resp.CodeForbidden, costViewsSwitchDisabledMsg)
		return
	}
	limit, err := parseCostSessionLimit(r.URL.Query().Get("Limit"))
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	params, err := parsePricingCurveParams(r.URL.Query(), h.now())
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}

	out := costTraceResponse{
		SessionID:           sid,
		Points:              make([]costTracePoint, 0),
		Segments:            make([]costTraceSegment, 0),
		ActualSwitches:      make([]costTraceSwitch, 0),
		Limit:               limit,
		SwitchKindActual:    traceKindActual,
		SwitchKindPredicted: traceKindPredicted,
	}
	if h.db == nil {
		resp.OK(w, r, out)
		return
	}

	// 总量：用于判断是否被 Limit 截断（走会话索引，代价很小）。
	var total int64
	if err := h.db.QueryRowContext(r.Context(),
		`SELECT count(*) FROM billing_records WHERE session_id = $1`, sid).Scan(&total); err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询会话渠道轨迹失败")
		return
	}
	out.Total = total

	points, err := h.loadTracePoints(r.Context(), sid, limit)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询会话渠道轨迹失败")
		return
	}
	out.Points = points
	out.Segments, out.ActualSwitches = buildTraceSegments(points)
	out.Truncated = total > int64(len(points))

	// 预测打平点：只在「从未真实切换过渠道」且轨迹非空时给。
	if len(out.ActualSwitches) == 0 && len(points) > 0 {
		out.PredictedBreakEven = h.predictBreakEven(r.Context(), points[0], params, h.resolveR())
	}
	resp.OK(w, r, out)
}

// loadTracePoints 取会话逐条账单并解析渠道/渠道内部模型。
func (h *Admin) loadTracePoints(ctx context.Context, sid string, limit int) ([]costTracePoint, error) {
	sqlText := fmt.Sprintf(`
WITH res AS (`+costEntityResolveBody+`)
SELECT b.call_time,
       b.status,
       COALESCE(k.channel_id, 0)       AS channel_id,
       COALESCE(c.name, '')            AS channel_name,
       COALESCE(r.cm_id, 0)            AS channel_model_id,
       b.internal_model_id,
       %s AS input_tokens,
       %s AS cache_read_tokens,
       %s AS output_tokens,
       COALESCE(b.cost_credits, 0)     AS cost_credits,
       COALESCE(b.credits_consumed, 0) AS sale_credits
FROM billing_records b
LEFT JOIN channel_keys k ON k.id = b.channel_key_id
LEFT JOIN channels c ON c.id = k.channel_id
LEFT JOIN res r ON r.channel_id = k.channel_id AND r.internal_model_id = b.internal_model_id
WHERE b.session_id = $1
ORDER BY b.call_time ASC, b.id ASC
LIMIT $2`,
		fmt.Sprintf(costTokenRowSQL, "input"),
		fmt.Sprintf(costTokenRowSQL, "cache_read"),
		fmt.Sprintf(costTokenRowSQL, "output"))

	// limit<=0 → 传 NULL，Postgres 的 `LIMIT NULL` 等价于不限制（与 /cost/session/{id} 同口径）。
	var limitArg any
	if limit > 0 {
		limitArg = limit
	}
	rows, err := h.db.QueryContext(ctx, sqlText, sid, limitArg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]costTracePoint, 0, 16)
	successTurn, turnNo := 0, 0
	cumulative := 0.0
	for rows.Next() {
		var (
			callTime time.Time
			pt       costTracePoint
		)
		if err := rows.Scan(&callTime, &pt.Status, &pt.ChannelID, &pt.ChannelName, &pt.ChannelModelID,
			&pt.InternalModelID, &pt.InputTokens, &pt.CacheReadTokens, &pt.OutputTokens,
			&pt.CostCredits, &pt.SaleCredits); err != nil {
			return nil, err
		}
		turnNo++
		pt.TurnNo = turnNo
		pt.CallTime = callTime.Format(time.RFC3339)
		if pt.Status == billing.StatusCompleted {
			successTurn++
			pt.Turn = successTurn
			pt.CostCredits = decimalx.Round5(pt.CostCredits)
			pt.SaleCredits = decimalx.Round5(pt.SaleCredits)
		} else {
			// 失败轮：如实标注，金额一律 0（失败不产生成本积分），Turn 不占成功序号。
			pt.Failed = true
			pt.Turn = 0
			pt.CostCredits = 0
			pt.SaleCredits = 0
		}
		cumulative += pt.CostCredits
		pt.CumulativeCostCredits = decimalx.Round5(cumulative)
		// 真实切换点标记：与本点之前的**上一个点**比渠道（不是与段首比）。
		if len(out) > 0 && out[len(out)-1].ChannelID != pt.ChannelID {
			pt.ChannelChanged = true
		}
		out = append(out, pt)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// buildTraceSegments 把逐点轨迹压成连续段，并抽真实切换点。
//
// 分段键 = (渠道 ID, 渠道内部模型 ID, 内部模型 ID)：同渠道换模型也算新段（但**不是**渠道切换）。
// 切换点判定只看 channel_id 变化（任务口径：相邻记录 channel 变化处），模型变化写进段与切换点的字段里。
func buildTraceSegments(points []costTracePoint) ([]costTraceSegment, []costTraceSwitch) {
	segments := make([]costTraceSegment, 0, 4)
	switches := make([]costTraceSwitch, 0, 2)
	for i := range points {
		pt := points[i]
		sameAsPrev := len(segments) > 0 &&
			segments[len(segments)-1].ChannelID == pt.ChannelID &&
			segments[len(segments)-1].ChannelModelID == pt.ChannelModelID &&
			segments[len(segments)-1].InternalModelID == pt.InternalModelID
		if !sameAsPrev {
			seg := costTraceSegment{
				Seq:             len(segments) + 1,
				ChannelID:       pt.ChannelID,
				ChannelName:     pt.ChannelName,
				ChannelModelID:  pt.ChannelModelID,
				InternalModelID: pt.InternalModelID,
				FromCallTime:    pt.CallTime,
				ToCallTime:      pt.CallTime,
			}
			segments = append(segments, seg)
			// 渠道真的变了 → 真实切换点（同渠道换模型不算）。
			if len(segments) > 1 {
				prev := segments[len(segments)-2]
				if prev.ChannelID != pt.ChannelID {
					switches = append(switches, costTraceSwitch{
						Kind:                traceKindActual,
						Seq:                 len(switches) + 1,
						TurnNo:              pt.TurnNo,
						AtCallTime:          pt.CallTime,
						FromChannelID:       prev.ChannelID,
						FromChannelName:     prev.ChannelName,
						FromChannelModelID:  prev.ChannelModelID,
						FromInternalModelID: prev.InternalModelID,
						ToChannelID:         pt.ChannelID,
						ToChannelName:       pt.ChannelName,
						ToChannelModelID:    pt.ChannelModelID,
						ToInternalModelID:   pt.InternalModelID,
					})
				}
			}
		}
		seg := &segments[len(segments)-1]
		seg.ToCallTime = pt.CallTime
		seg.Points++
		if !pt.Failed {
			seg.SuccessPoints++
		}
		seg.CostCredits += pt.CostCredits
		seg.SaleCredits += pt.SaleCredits
		seg.InputTokens += pt.InputTokens
		seg.CacheReadTokens += pt.CacheReadTokens
		seg.OutputTokens += pt.OutputTokens
	}
	for i := range segments {
		segments[i].CostCredits = decimalx.Round5(segments[i].CostCredits)
		segments[i].SaleCredits = decimalx.Round5(segments[i].SaleCredits)
	}
	return segments, switches
}

// predictBreakEven 计算预测打平点：同一内部模型的其它渠道里，谁能在最小的上下文长度上反超会话实际渠道。
//
// 曲线与定价曲线同一套公式与采样参数（buildPricingLine），不另立口径。
// 单点成本相等的处理：要求严格小于才算「反超」，相等继续往后找（避免把打平前的重合段当切换点）。
func (h *Admin) predictBreakEven(ctx context.Context, first costTracePoint,
	p pricingCurveParams, r int64) *costTraceBreakEven {
	out := &costTraceBreakEven{
		Kind:            traceKindPredicted,
		Predicted:       true,
		ChannelModelID:  first.ChannelModelID,
		ChannelID:       first.ChannelID,
		ChannelName:     first.ChannelName,
		InternalModelID: first.InternalModelID,
		Step:            p.Step,
	}
	if first.ChannelModelID <= 0 || first.InternalModelID == "" {
		out.Reason = "账单未解析到渠道内部模型，无法预测打平点"
		return out
	}
	if h.db == nil {
		out.Reason = "未装配数据库，无法预测打平点"
		return out
	}

	globalTime := billing.TimeCoeffConfig{Timezone: cnLocation().String(), Default: 1}
	if cfg, err := h.loadGlobalTimeConfig(ctx); err == nil {
		globalTime = cfg
	}
	globalTiers := h.loadGlobalContextTiers(ctx)

	candidates, err := h.loadSiblingChannelModels(ctx, first.InternalModelID, globalTime, globalTiers)
	if err != nil {
		out.Reason = "查询同模型其它渠道配置失败，无法预测打平点"
		return out
	}
	var current *pricingModelConfig
	for i := range candidates {
		if candidates[i].ChannelModelID == first.ChannelModelID {
			current = &candidates[i]
			break
		}
	}
	if current == nil {
		out.Reason = "未找到会话实际渠道的内部模型配置（可能已删除），无法预测打平点"
		return out
	}
	if len(candidates) < 2 {
		out.Reason = "该内部模型只有一条渠道配置，无对比对象"
		return out
	}

	curLine, err := buildPricingLine(*current, p, r)
	if err != nil || !curLine.Available {
		out.Reason = "会话实际渠道的成本费率缺失，无法预测打平点"
		return out
	}

	var best *costTraceBreakEven
	for i := range candidates {
		alt := &candidates[i]
		if alt.ChannelModelID == current.ChannelModelID {
			continue
		}
		altLine, aerr := buildPricingLine(*alt, p, r)
		if aerr != nil || !altLine.Available {
			continue
		}
		tokens, matched := firstCrossTokens(curLine.Points, altLine.Points)
		if !matched {
			continue
		}
		if best == nil || tokens < best.Tokens {
			best = &costTraceBreakEven{
				Kind:                      traceKindPredicted,
				Predicted:                 true,
				ChannelModelID:            current.ChannelModelID,
				ChannelID:                 current.ChannelID,
				ChannelName:               current.ChannelName,
				InternalModelID:           current.InternalModelID,
				AlternativeChannelModelID: alt.ChannelModelID,
				AlternativeChannelID:      alt.ChannelID,
				AlternativeChannelName:    alt.ChannelName,
				Tokens:                    tokens,
				Matched:                   true,
				Step:                      p.Step,
				Reason:                    "会话从未切换渠道；该上下文长度起，替代渠道的单次成本开始低于实际渠道（按当前费率与分档推算）",
			}
		}
	}
	if best == nil {
		out.Reason = "采样范围内（Step/MaxTokens）没有出现替代渠道更便宜的点，可放大 MaxTokens 或缩小 Step 再试"
		return out
	}
	best.MaxTokens = p.MaxTokens
	return best
}

// firstCrossTokens 在两条同轴成本曲线上找「替代线第一次严格低于实际线」的上下文长度（线性插值）。
// 返回 (tokens, true)；没有任何反超点返回 (0, false)。任一点命中超档（OverTier）一律跳过。
func firstCrossTokens(current, alternative []pricingPoint) (int64, bool) {
	n := len(current)
	if len(alternative) < n {
		n = len(alternative)
	}
	prevDiff := 0.0
	hasPrev := false
	for i := 0; i < n; i++ {
		if current[i].OverTier || alternative[i].OverTier {
			hasPrev = false // 超档段不成曲线，打断插值
			continue
		}
		diff := alternative[i].Credits - current[i].Credits // <0 表示替代更便宜
		if diff < 0 {
			if !hasPrev {
				// 起点就已经更便宜：打平点即曲线左端（再往左不在采样范围内）。
				return current[i].Tokens, true
			}
			// 在 [i-1, i] 之间线性插值出 diff=0 的横轴位置。
			span := prevDiff - diff
			if span <= 0 {
				return current[i].Tokens, true
			}
			frac := prevDiff / span
			if frac < 0 {
				frac = 0
			}
			if frac > 1 {
				frac = 1
			}
			from := float64(current[i-1].Tokens)
			to := float64(current[i].Tokens)
			return int64(roundHalfUp(from + frac*(to-from))), true
		}
		prevDiff = diff
		hasPrev = true
	}
	return 0, false
}

// loadSiblingChannelModels 取同一内部模型在所有未删除渠道下的成本配置（预测打平点的候选线）。
// 与 loadChannelModelConfig 的判定口径一致：费率全 0/缺失 → Available=false（不参与预测）。
func (h *Admin) loadSiblingChannelModels(ctx context.Context, internalModelID string,
	globalTime billing.TimeCoeffConfig, globalTiers []billing.TierRule) ([]pricingModelConfig, error) {
	rows, err := h.db.QueryContext(ctx, `
SELECT cm.id, cm.channel_id, COALESCE(c.name, ''), cm.internal_model_id, cm.cost_rates, cm.time_config, cm.context_tiers
FROM channel_models cm
LEFT JOIN channels c ON c.id = cm.channel_id
WHERE cm.deleted_at IS NULL AND cm.internal_model_id = $1
ORDER BY cm.id`, internalModelID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]pricingModelConfig, 0, 4)
	for rows.Next() {
		var (
			cfg               pricingModelConfig
			ratesRaw          []byte
			timeRaw, tiersRaw []byte
			channelName       string
		)
		if err := rows.Scan(&cfg.ChannelModelID, &cfg.ChannelID, &channelName, &cfg.InternalModelID,
			&ratesRaw, &timeRaw, &tiersRaw); err != nil {
			return nil, err
		}
		cfg.Source = "channel_model"
		cfg.ChannelName = channelName
		cfg.Name = cfg.InternalModelID
		cfg.Rates = parseRates(ratesRaw)
		cfg.TimeConfig, cfg.TimeConfigSource = mergeTimeConfig(timeRaw, globalTime)
		cfg.Tiers, cfg.TierSource = mergeTiers(tiersRaw, globalTiers)
		cfg.Available = hasNonZeroRates(cfg.Rates)
		if !cfg.Available {
			cfg.Reason = "渠道内部模型未配置成本费率"
		}
		out = append(out, cfg)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
