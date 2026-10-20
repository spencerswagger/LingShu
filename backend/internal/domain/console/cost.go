// Package console —— 「成本」只读接口（成本/售价对照、默认参数、成本曲线、单会话成本明细）。
//
// 全部为 SELECT 只读查询，不改动任何路由/计费行为。
// 口径说明：
//   - 成本 = billing_records.cost_credits，售价 = billing_records.credits_consumed；
//   - 三段 token（input/cache_read/output）取自 billing_records.tokens(JSONB)，
//     用正则 '^-?[0-9]{1,18}$' 守卫（同时限制长度防 ::bigint 溢出），非法/超范围/缺失一律按 0；
//   - 金额一律 decimalx.Round5；成本/售价即积分（已含时段/分档系数，见 billing.ComputeCredits）。
//   - 消耗口径统一过滤 status='completed'（与 console/summary.go 一致）。
package console

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"github.com/team/llmgateway/internal/pkg/decimalx"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// costRProvider 提供积分换算系数 R（billing.ConfigProvider 满足；窄接口避免过度耦合）。
// nil=未装配时成本曲线回退默认 10000。
type costRProvider interface {
	R() (int64, error)
}

// costDefaultR 积分换算系数默认值（未装配配置源时的兜底，与 sys_configs billing.r 默认一致）。
const costDefaultR = 10000

// costTokenSumSQL 三段 token 之和的聚合表达式（别名固定为 b）：正则守卫非法值/超长值按 0。
// 长度上限 18 位（含符号最多 19 字符）确保 ::bigint 不溢出（int64 上限约 9.2e18）。
const costTokenSumSQL = `COALESCE(SUM(CASE WHEN (b.tokens->>'%[1]s') ~ '^-?[0-9]{1,18}$' THEN (b.tokens->>'%[1]s')::bigint ELSE 0 END), 0)`

// costTokenRowSQL 三段 token 的单行表达式（别名固定为 b）：正则守卫非法值/超长值按 0。
const costTokenRowSQL = `CASE WHEN (b.tokens->>'%[1]s') ~ '^-?[0-9]{1,18}$' THEN (b.tokens->>'%[1]s')::bigint ELSE 0 END`

// costStatsRow 「成本/售价对照」分组行。
type costStatsRow struct {
	ChannelID         int64   `json:"ChannelID"`
	ChannelName       string  `json:"ChannelName"`
	InternalModelID   string  `json:"InternalModelID"`
	ExternalModelName string  `json:"ExternalModelName"`
	CostCredits       float64 `json:"CostCredits"`
	SaleCredits       float64 `json:"SaleCredits"`
	Requests          int64   `json:"Requests"`
	InputTokens       int64   `json:"InputTokens"`
	CacheReadTokens   int64   `json:"CacheReadTokens"`
	OutputTokens      int64   `json:"OutputTokens"`
	CacheHitRate      float64 `json:"CacheHitRate"`
	InputOutputRatio  float64 `json:"InputOutputRatio"`
}

// costStatsTotal 全表同口径聚合（不含渠道/模型维度字段）。
type costStatsTotal struct {
	CostCredits      float64 `json:"CostCredits"`
	SaleCredits      float64 `json:"SaleCredits"`
	Requests         int64   `json:"Requests"`
	InputTokens      int64   `json:"InputTokens"`
	CacheReadTokens  int64   `json:"CacheReadTokens"`
	OutputTokens     int64   `json:"OutputTokens"`
	CacheHitRate     float64 `json:"CacheHitRate"`
	InputOutputRatio float64 `json:"InputOutputRatio"`
}

// HandleCostStats GET /api/v1/admin/cost/stats
// 按「渠道 + 内部模型 + 对外模型」分组统计成本/售价/请求数/三段 token 与派生比率；
// 仅统计 status='completed' 的消耗（与 console/summary.go 一致）；Total 为全表同口径聚合。
func (h *Admin) HandleCostStats(w http.ResponseWriter, r *http.Request) {
	rows := make([]costStatsRow, 0)
	if h.db == nil {
		resp.OK(w, r, map[string]any{"Rows": rows, "Total": costStatsTotal{}})
		return
	}
	sqlText := fmt.Sprintf(`
SELECT k.channel_id,
       COALESCE(c.name, '') AS channel_name,
       b.internal_model_id,
       b.external_model_name,
       COALESCE(SUM(b.cost_credits), 0)      AS cost_credits,
       COALESCE(SUM(b.credits_consumed), 0)  AS sale_credits,
       COUNT(*)                              AS requests,
       %s AS input_tokens,
       %s AS cache_read_tokens,
       %s AS output_tokens
FROM billing_records b
LEFT JOIN channel_keys k ON k.id = b.channel_key_id
LEFT JOIN channels c ON c.id = k.channel_id
WHERE b.status = 'completed'
GROUP BY k.channel_id, c.name, b.internal_model_id, b.external_model_name
ORDER BY SUM(b.cost_credits) DESC NULLS LAST, k.channel_id, b.internal_model_id, b.external_model_name`,
		fmt.Sprintf(costTokenSumSQL, "input"),
		fmt.Sprintf(costTokenSumSQL, "cache_read"),
		fmt.Sprintf(costTokenSumSQL, "output"))

	rowSet, err := h.db.QueryContext(r.Context(), sqlText)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询成本统计失败")
		return
	}
	defer rowSet.Close()

	var total costStatsTotal
	for rowSet.Next() {
		var (
			chID   sql.NullInt64
			item   costStatsRow
			chName string
		)
		if err := rowSet.Scan(&chID, &chName, &item.InternalModelID, &item.ExternalModelName,
			&item.CostCredits, &item.SaleCredits, &item.Requests,
			&item.InputTokens, &item.CacheReadTokens, &item.OutputTokens); err != nil {
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询成本统计失败")
			return
		}
		if chID.Valid {
			item.ChannelID = chID.Int64
		}
		switch {
		case chName != "":
			item.ChannelName = chName
		case chID.Valid:
			// 渠道名缺失时回退渠道 ID 字符串，避免前端展示空白。
			item.ChannelName = strconv.FormatInt(chID.Int64, 10)
		default:
			// channel_key_id 为 NULL（未进入渠道的拒绝类账单）→ 无渠道归属。
			item.ChannelName = "(无渠道)"
		}
		item.CostCredits = decimalx.Round5(item.CostCredits)
		item.SaleCredits = decimalx.Round5(item.SaleCredits)
		item.CacheHitRate = decimalx.Round5(cacheHitRate(item.CacheReadTokens, item.InputTokens))
		item.InputOutputRatio = decimalx.Round5(inputOutputRatio(item.InputTokens, item.OutputTokens))

		total.CostCredits += item.CostCredits
		total.SaleCredits += item.SaleCredits
		total.Requests += item.Requests
		total.InputTokens += item.InputTokens
		total.CacheReadTokens += item.CacheReadTokens
		total.OutputTokens += item.OutputTokens
		rows = append(rows, item)
	}
	if err := rowSet.Err(); err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询成本统计失败")
		return
	}
	total.CostCredits = decimalx.Round5(total.CostCredits)
	total.SaleCredits = decimalx.Round5(total.SaleCredits)
	total.CacheHitRate = decimalx.Round5(cacheHitRate(total.CacheReadTokens, total.InputTokens))
	total.InputOutputRatio = decimalx.Round5(inputOutputRatio(total.InputTokens, total.OutputTokens))

	resp.OK(w, r, map[string]any{"Rows": rows, "Total": total})
}

// costDefaultsResponse 默认参数响应（仅 token 口径，不含金额）。
type costDefaultsResponse struct {
	PerTurnInputTokens float64 `json:"PerTurnInputTokens"`
	CacheHitRate       float64 `json:"CacheHitRate"`
	InputOutputRatio   float64 `json:"InputOutputRatio"`
}

// HandleCostDefaults GET /api/v1/admin/cost/defaults
// 仅用 token 求和推导默认参数：每轮输入、缓存命中率、输入输出比；仅统计 status='completed'。
func (h *Admin) HandleCostDefaults(w http.ResponseWriter, r *http.Request) {
	out := costDefaultsResponse{}
	if h.db == nil {
		resp.OK(w, r, out)
		return
	}
	sqlText := fmt.Sprintf(`
SELECT
    %s AS input_tokens,
    %s AS cache_read_tokens,
    %s AS output_tokens,
    COUNT(*) AS requests
FROM billing_records b
WHERE b.status = 'completed'`,
		fmt.Sprintf(costTokenSumSQL, "input"),
		fmt.Sprintf(costTokenSumSQL, "cache_read"),
		fmt.Sprintf(costTokenSumSQL, "output"))

	var input, cacheRead, output, requests int64
	if err := h.db.QueryRowContext(r.Context(), sqlText).Scan(&input, &cacheRead, &output, &requests); err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询默认参数失败")
		return
	}
	if requests > 0 {
		out.PerTurnInputTokens = decimalx.Round5(float64(input) / float64(requests))
	}
	out.CacheHitRate = decimalx.Round5(cacheHitRate(cacheRead, input))
	out.InputOutputRatio = decimalx.Round5(inputOutputRatio(input, output))
	resp.OK(w, r, out)
}

// 成本曲线默认值与上限钳制。
const (
	costCurveDefaultTurns   = 30
	costCurveMaxTurns       = 200
	costCurveDefaultPerTurn = 10000.0
	costCurveMaxPerTurn     = 1e9 // 每轮新增输入 token 上限
	costCurveMaxInOutRatio  = 1e6 // 输入输出比上限
)

// costCurveItem 单渠道成本曲线（Cost/Sale 为逐轮序列，长度 = Turns）。
type costCurveItem struct {
	ChannelID         int64     `json:"ChannelID"`
	ChannelName       string    `json:"ChannelName"`
	InternalModelID   string    `json:"InternalModelID"`
	ExternalModelName string    `json:"ExternalModelName"`
	Cost              []float64 `json:"Cost"`
	Sale              []float64 `json:"Sale"`
}

// costCurvesResponse 成本曲线响应。
type costCurvesResponse struct {
	Turns  int             `json:"Turns"`
	Curves []costCurveItem `json:"Curves"`
}

// HandleCostCurves GET /api/v1/admin/cost/curves
// 依据每轮输入/缓存命中率/输入输出比模拟各渠道多轮累计成本与售价曲线。
// rate 单位「人民币每百万 token 价」（纯数字）；每轮值 = Σ(token×rate) ÷ R，与账单积分同量纲
// （R = billing.r，默认 10000）。cost_rates 缺失/全 0 的行跳过；sale_rates 缺失/全 0 则不画售价曲线。
func (h *Admin) HandleCostCurves(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()

	perTurnInput := costCurveDefaultPerTurn
	if raw := q.Get("perTurnInput"); raw != "" {
		v, err := parseCostFiniteFloat(raw)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "perTurnInput 参数非法")
			return
		}
		if v > 0 {
			perTurnInput = math.Min(v, costCurveMaxPerTurn)
		}
	}

	hitRate := 0.0
	if raw := q.Get("hitRate"); raw != "" {
		v, err := parseCostFiniteFloat(raw)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "hitRate 参数非法")
			return
		}
		hitRate = math.Min(math.Max(v, 0), 1)
	}

	inOutRatio := 0.0
	if raw := q.Get("inOutRatio"); raw != "" {
		v, err := parseCostFiniteFloat(raw)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "inOutRatio 参数非法")
			return
		}
		if v > 0 {
			inOutRatio = math.Min(v, costCurveMaxInOutRatio)
		}
	}

	turns := costCurveDefaultTurns
	if raw := q.Get("turns"); raw != "" {
		v, err := strconv.Atoi(raw)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "turns 参数非法")
			return
		}
		if v > 0 {
			turns = v
		}
	}
	if turns <= 0 {
		turns = costCurveDefaultTurns
	}
	if turns > costCurveMaxTurns {
		turns = costCurveMaxTurns
	}

	out := costCurvesResponse{Turns: turns, Curves: make([]costCurveItem, 0)}
	if h.db == nil {
		resp.OK(w, r, out)
		return
	}

	rValue := h.resolveR()

	rowSet, err := h.db.QueryContext(r.Context(), `
SELECT cm.channel_id,
       COALESCE(c.name, '') AS channel_name,
       cm.internal_model_id,
       COALESCE(em.external_name, '') AS external_name,
       cm.cost_rates,
       em.sale_rates
FROM channel_models cm
LEFT JOIN channels c ON c.id = cm.channel_id
JOIN external_models em ON em.id = cm.external_model_id
WHERE cm.deleted_at IS NULL
ORDER BY cm.channel_id, cm.internal_model_id`)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询成本曲线失败")
		return
	}
	defer rowSet.Close()

	for rowSet.Next() {
		var (
			item    costCurveItem
			costRaw []byte
			saleRaw []byte
			prices  map[string]float64
			sales   map[string]float64
		)
		if err := rowSet.Scan(&item.ChannelID, &item.ChannelName, &item.InternalModelID, &item.ExternalModelName,
			&costRaw, &saleRaw); err != nil {
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询成本曲线失败")
			return
		}
		if len(costRaw) > 0 && string(costRaw) != "null" {
			_ = json.Unmarshal(costRaw, &prices)
		}
		// cost_rates 缺失/三段全 0 视为「成本未知」，跳过该行（对齐 router/engine.go 的 hasValidCost 口径）。
		if !hasNonZeroRates(prices) {
			continue
		}
		saleValid := false
		if len(saleRaw) > 0 && string(saleRaw) != "null" {
			_ = json.Unmarshal(saleRaw, &sales)
		}
		// sale_rates 缺失/三段全 0 → 不画售价曲线（避免误导的全 0 线）。
		if hasNonZeroRates(sales) {
			saleValid = true
		}

		item.Cost = make([]float64, 0, turns)
		item.Sale = make([]float64, 0, turns)
		output := 0.0
		if inOutRatio > 0 {
			output = perTurnInput / inOutRatio
		}
		for k := 1; k <= turns; k++ {
			newInput := perTurnInput
			cacheRead := hitRate * perTurnInput * float64(k-1)
			costK := (cacheRead*prices["cache_read"] + newInput*prices["input"] + output*prices["output"]) / float64(rValue)
			item.Cost = append(item.Cost, decimalx.Round5(costK))
			if saleValid {
				saleK := (cacheRead*sales["cache_read"] + newInput*sales["input"] + output*sales["output"]) / float64(rValue)
				item.Sale = append(item.Sale, decimalx.Round5(saleK))
			}
		}
		out.Curves = append(out.Curves, item)
	}
	if err := rowSet.Err(); err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询成本曲线失败")
		return
	}
	resp.OK(w, r, out)
}

// resolveR 返回成本曲线使用的积分换算系数 R：优先读注入的配置源，未装配/读取失败/非法时回退默认 10000。
func (h *Admin) resolveR() int64 {
	if h.rProvider != nil {
		if v, err := h.rProvider.R(); err == nil && v > 0 {
			return v
		}
	}
	return costDefaultR
}

// parseCostFiniteFloat 解析成本曲线数值入参：非数字或 NaN/±Inf 一律返回错误（调用方转 400），
// 避免 Inf/NaN 流入响应序列化触发 500。
func parseCostFiniteFloat(raw string) (float64, error) {
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, fmt.Errorf("非有限数值")
	}
	return v, nil
}

// hasNonZeroRates 判断费率表三段是否至少一段非 0（缺失/全 0 视为无效）。
func hasNonZeroRates(rates map[string]float64) bool {
	for _, k := range []string{"input", "output", "cache_read"} {
		if rates[k] != 0 {
			return true
		}
	}
	return false
}

// costSessionPoint 单会话逐轮成本点。
type costSessionPoint struct {
	Turn            int     `json:"Turn"`
	CallTime        string  `json:"CallTime"`
	ChannelName     string  `json:"ChannelName"`
	InternalModelID string  `json:"InternalModelID"`
	InputTokens     int64   `json:"InputTokens"`
	CacheReadTokens int64   `json:"CacheReadTokens"`
	OutputTokens    int64   `json:"OutputTokens"`
	CostCredits     float64 `json:"CostCredits"`
	SaleCredits     float64 `json:"SaleCredits"`
}

// costSessionTotals 单会话成本/售价合计。
type costSessionTotals struct {
	CostCredits float64 `json:"CostCredits"`
	SaleCredits float64 `json:"SaleCredits"`
}

// HandleCostSession GET /api/v1/admin/cost/session/{id}
// 返回指定会话按 call_time 升序的逐轮成本明细与合计（仅 status='completed'）；缺 id 返回 400。
func (h *Admin) HandleCostSession(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	if sid == "" {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "缺少会话 ID")
		return
	}
	points := make([]costSessionPoint, 0)
	totals := costSessionTotals{}
	if h.db == nil {
		resp.OK(w, r, map[string]any{"SessionID": sid, "Points": points, "Totals": totals})
		return
	}
	sqlText := fmt.Sprintf(`
SELECT b.call_time,
       COALESCE(c.name, '') AS channel_name,
       b.channel_key_id,
       b.internal_model_id,
       %s AS input_tokens,
       %s AS cache_read_tokens,
       %s AS output_tokens,
       COALESCE(b.cost_credits, 0)     AS cost_credits,
       COALESCE(b.credits_consumed, 0) AS sale_credits
FROM billing_records b
LEFT JOIN channel_keys k ON k.id = b.channel_key_id
LEFT JOIN channels c ON c.id = k.channel_id
WHERE b.session_id = $1 AND b.status = 'completed'
ORDER BY b.call_time ASC, b.id ASC`,
		fmt.Sprintf(costTokenRowSQL, "input"),
		fmt.Sprintf(costTokenRowSQL, "cache_read"),
		fmt.Sprintf(costTokenRowSQL, "output"))

	rowSet, err := h.db.QueryContext(r.Context(), sqlText, sid)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询会话成本失败")
		return
	}
	defer rowSet.Close()

	turn := 0
	for rowSet.Next() {
		var (
			callTime time.Time
			chName   string
			keyID    sql.NullInt64
			pt       costSessionPoint
		)
		if err := rowSet.Scan(&callTime, &chName, &keyID, &pt.InternalModelID,
			&pt.InputTokens, &pt.CacheReadTokens, &pt.OutputTokens,
			&pt.CostCredits, &pt.SaleCredits); err != nil {
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询会话成本失败")
			return
		}
		turn++
		pt.Turn = turn
		pt.CallTime = callTime.Format(time.RFC3339)
		pt.ChannelName = chName
		if pt.ChannelName == "" && keyID.Valid {
			// 渠道名缺失时回退渠道密钥 ID 字符串。
			pt.ChannelName = strconv.FormatInt(keyID.Int64, 10)
		}
		pt.CostCredits = decimalx.Round5(pt.CostCredits)
		pt.SaleCredits = decimalx.Round5(pt.SaleCredits)
		totals.CostCredits += pt.CostCredits
		totals.SaleCredits += pt.SaleCredits
		points = append(points, pt)
	}
	if err := rowSet.Err(); err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询会话成本失败")
		return
	}
	totals.CostCredits = decimalx.Round5(totals.CostCredits)
	totals.SaleCredits = decimalx.Round5(totals.SaleCredits)

	resp.OK(w, r, map[string]any{"SessionID": sid, "Points": points, "Totals": totals})
}

// cacheHitRate 缓存命中率 = cache_read ÷ (input + cache_read)；分母为 0 返回 0。
func cacheHitRate(cacheRead, input int64) float64 {
	denom := input + cacheRead
	if denom == 0 {
		return 0
	}
	return float64(cacheRead) / float64(denom)
}

// inputOutputRatio 输入输出比 = input ÷ output；output 为 0 返回 0。
func inputOutputRatio(input, output int64) float64 {
	if output == 0 {
		return 0
	}
	return float64(input) / float64(output)
}
