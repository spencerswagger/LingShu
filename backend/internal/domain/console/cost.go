// Package console —— 「成本」只读接口（成本/售价对照、默认参数、成本曲线、单会话成本明细）。
//
// 全部为 SELECT 只读查询，不改动任何路由/计费行为。
// 口径说明：
//   - 成本 = billing_records.cost_credits，售价 = billing_records.credits_consumed；
//   - 三段 token（input/cache_read/output）取自 billing_records.tokens(JSONB)，
//     用正则 '^-?[0-9]+$' 守卫，非法/缺失一律按 0，绝不因脏数据抛错；
//   - 金额一律 decimalx.Round5。
package console

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/team/llmgateway/internal/pkg/decimalx"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// costTokenSumSQL 三段 token 之和的聚合表达式（别名固定为 b）：正则守卫非法值按 0。
const costTokenSumSQL = `COALESCE(SUM(CASE WHEN (b.tokens->>'%[1]s') ~ '^-?[0-9]+$' THEN (b.tokens->>'%[1]s')::bigint ELSE 0 END), 0)`

// costTokenRowSQL 三段 token 的单行表达式（别名固定为 b）：正则守卫非法值按 0。
const costTokenRowSQL = `CASE WHEN (b.tokens->>'%[1]s') ~ '^-?[0-9]+$' THEN (b.tokens->>'%[1]s')::bigint ELSE 0 END`

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
// 按「渠道密钥 + 内部模型 + 对外模型」分组统计成本/售价/请求数/三段 token 与派生比率；
// Total 为全表同口径聚合。
func (h *Admin) HandleCostStats(w http.ResponseWriter, r *http.Request) {
	rows := make([]costStatsRow, 0)
	if h.db == nil {
		resp.OK(w, r, map[string]any{"Rows": rows, "Total": costStatsTotal{}})
		return
	}
	sqlText := fmt.Sprintf(`
SELECT b.channel_key_id,
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
GROUP BY b.channel_key_id, c.name, b.internal_model_id, b.external_model_name
ORDER BY SUM(b.cost_credits) DESC NULLS LAST, b.channel_key_id, b.internal_model_id, b.external_model_name`,
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
			keyID  sql.NullInt64
			item   costStatsRow
			chName string
		)
		if err := rowSet.Scan(&keyID, &chName, &item.InternalModelID, &item.ExternalModelName,
			&item.CostCredits, &item.SaleCredits, &item.Requests,
			&item.InputTokens, &item.CacheReadTokens, &item.OutputTokens); err != nil {
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询成本统计失败")
			return
		}
		if keyID.Valid {
			item.ChannelID = keyID.Int64
		}
		item.ChannelName = chName
		if item.ChannelName == "" && keyID.Valid {
			// 渠道名缺失时回退渠道密钥 ID 字符串，避免前端展示空白。
			item.ChannelName = strconv.FormatInt(keyID.Int64, 10)
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
// 仅用 token 求和推导默认参数：每轮输入、缓存命中率、输入输出比。
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
FROM billing_records b`,
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
// cost_rates/sale_rates 单位「每 1e6 token 的积分」；cost_rates 缺失/为空的行跳过。
func (h *Admin) HandleCostCurves(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	perTurnInput := costCurveDefaultPerTurn
	if v, err := strconv.ParseFloat(q.Get("perTurnInput"), 64); err == nil && v > 0 {
		perTurnInput = v
	}
	hitRate := 0.0
	if v, err := strconv.ParseFloat(q.Get("hitRate"), 64); err == nil && v > 0 {
		hitRate = v
	}
	inOutRatio := 0.0
	if v, err := strconv.ParseFloat(q.Get("inOutRatio"), 64); err == nil && v > 0 {
		inOutRatio = v
	}
	turns := costCurveDefaultTurns
	if v, err := strconv.Atoi(q.Get("turns")); err == nil {
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
		if len(prices) == 0 {
			// cost_rates 缺失/为空：跳过该行。
			continue
		}
		if len(saleRaw) > 0 && string(saleRaw) != "null" {
			_ = json.Unmarshal(saleRaw, &sales)
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
			costK := (cacheRead*prices["cache_read"] + newInput*prices["input"] + output*prices["output"]) / 1e6
			saleK := (cacheRead*sales["cache_read"] + newInput*sales["input"] + output*sales["output"]) / 1e6
			item.Cost = append(item.Cost, decimalx.Round5(costK))
			item.Sale = append(item.Sale, decimalx.Round5(saleK))
		}
		out.Curves = append(out.Curves, item)
	}
	if err := rowSet.Err(); err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询成本曲线失败")
		return
	}
	resp.OK(w, r, out)
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
// 返回指定会话按 call_time 升序的逐轮成本明细与合计；缺 id 返回 400。
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
WHERE b.session_id = $1
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
