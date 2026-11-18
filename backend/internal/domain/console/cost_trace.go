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
// 该接口为只读能力，**恒可用**（无服务端开关门控）；参数非法一律 400。
func (h *Admin) HandleCostSessionTrace(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("id")
	if sid == "" {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "缺少会话 ID")
		return
	}
	limit, err := parseCostSessionLimit(r.URL.Query().Get("Limit"))
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	// 采样参数仍做校验（前端一直在传，非法值要继续 400），但预测已停用，值本身不再使用。
	if _, perr := parsePricingCurveParams(r.URL.Query(), h.now()); perr != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, perr.Error())
		return
	}
	// 「能力未启用」是预期内的拒绝，不落异常日志（否则 4xx 告警会把正常关开关算成故障）。

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
	// 基准取第一条**成功且有渠道内部模型**的记录：首条常是拒绝/余额不足账单（无 channel_key_id），
	// 拿它当基准会让整通会话误判成「配置缺失」，把主打能力整块废掉。
	// 一条合格记录都没有时，predictBreakEven 会回一个 Matched=false + Reason 的对象（不是 null）。
	// 预测打平点：**渠道体系下线后不再具备计算前提**（0024 删了 channels/channel_keys/channel_models，
	// 没有「同一内部模型在其它渠道的成本配置」这个对象）。这里回一个显式的不可用对象而不是 null，
	// 让前端能把结论区置灰并给出原因，而不是显示成「算出来是 0」。
	if len(points) > 0 {
		out.PredictedBreakEven = &costTraceBreakEven{
			Kind:      traceKindPredicted,
			Predicted: false,
			Reason:    "渠道体系已下线（迁移 0024）：没有多渠道成本配置可对比，打平点预测不再适用；成本请以账本口径为准",
		}
	}
	resp.OK(w, r, out)
}

// firstTraceAnchor 取轨迹里第一条「成功且解析到渠道内部模型」的记录。
//
// ⚠️ 渠道体系下线后它已无调用方（预测打平点整体停用），保留函数只为不打断既有用例；
// 后续清理可整块删除。
// 第二个返回值为 false 表示整段轨迹没有合格记录（全是失败/拒绝）。
// 注意：切换判定用的基准更宽松（只要求「成功且有渠道」），见 buildTraceSegments。
func firstTraceAnchor(points []costTracePoint) (costTracePoint, bool) {
	for i := range points {
		pt := points[i]
		if !pt.Failed && pt.ChannelID > 0 && pt.ChannelModelID > 0 && pt.InternalModelID != "" {
			return pt, true
		}
	}
	return costTracePoint{}, false
}

// truncateReason 在预测原因后追加「基于前 N/M 条」的限定，避免把截断轨迹的结论说成整通会话的事实。
func truncateReason(reason string, loaded int, total int64) string {
	suffix := fmt.Sprintf("（注意：本次只加载了前 %d 条（共 %d 条），结论仅基于已加载部分）", loaded, total)
	return reason + suffix
}

// loadTracePoints 取会话逐条账单并解析渠道/渠道内部模型。
func (h *Admin) loadTracePoints(ctx context.Context, sid string, limit int) ([]costTracePoint, error) {
	// 渠道维度已下线：channel_id / channel_name / channel_model_id 恒为 0 / 空串
	// （历史账单的渠道归属随表一起消失，没有留档），轨迹因此只剩「时间 × 模型 × 金额」。
	sqlText := fmt.Sprintf(`
SELECT b.call_time,
       b.status,
       0::bigint                       AS channel_id,
       ''::text                        AS channel_name,
       0::bigint                       AS channel_model_id,
       b.internal_model_id,
       %s AS input_tokens,
       %s AS cache_read_tokens,
       %s AS output_tokens,
       COALESCE(b.cost_credits, 0)     AS cost_credits,
       COALESCE(b.credits_consumed, 0) AS sale_credits
FROM billing_records b
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
	// 真实切换标记的基准 = 上一个「成功且有渠道」的点（不是上一个点）：
	// 同一次请求内候选失败（429/401）与拒绝账单（ChannelID=0）都不代表会话换了渠道，
	// 拿「上一个点」判会把请求内 failover 标成真实切换，还会算出 A→空→A 两次假切换。
	// 与 buildTraceSegments 的切换判定用同一基准，保证 Points 标记与 ActualSwitches 对得上。
	var anchorChannel int64
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
		// 真实切换标记：只在「成功且有渠道」的点上判定，基准是上一个这样的点（首点恒 false）。
		if !pt.Failed && pt.ChannelID > 0 {
			if anchorChannel > 0 && anchorChannel != pt.ChannelID {
				pt.ChannelChanged = true
			}
			anchorChannel = pt.ChannelID
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
//
// 切换点判定（与 Points[].ChannelChanged 同一基准）：
//   - 基准是**上一个成功且有渠道的点**，不是「上一个点」；
//   - 同一次请求内的候选失败重试（A 429 → B 成功）与无渠道的拒绝账单（ChannelID=0）
//     都不构成会话级渠道切换，既不产生切换点，也不参与「从哪个渠道切过来」；
//   - A(成功) → 空(拒绝失败) → A(成功) 恒为 0 次切换，不会出现空渠道名。
func buildTraceSegments(points []costTracePoint) ([]costTraceSegment, []costTraceSwitch) {
	segments := make([]costTraceSegment, 0, 4)
	switches := make([]costTraceSwitch, 0, 2)
	var anchorIdx = -1 // points 里上一个「成功且有渠道」的下标
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

		// 切换判定：只认成功且有渠道的点（失败/拒绝记录是「本轮换候选」，不是「会话换渠道」）。
		if pt.Failed || pt.ChannelID <= 0 {
			continue
		}
		if anchorIdx >= 0 {
			from := points[anchorIdx]
			if from.ChannelID != pt.ChannelID {
				switches = append(switches, costTraceSwitch{
					Kind:                traceKindActual,
					Seq:                 len(switches) + 1,
					TurnNo:              pt.TurnNo,
					AtCallTime:          pt.CallTime,
					FromChannelID:       from.ChannelID,
					FromChannelName:     from.ChannelName,
					FromChannelModelID:  from.ChannelModelID,
					FromInternalModelID: from.InternalModelID,
					ToChannelID:         pt.ChannelID,
					ToChannelName:       pt.ChannelName,
					ToChannelModelID:    pt.ChannelModelID,
					ToInternalModelID:   pt.InternalModelID,
				})
			}
		}
		anchorIdx = i
	}
	for i := range segments {
		segments[i].CostCredits = decimalx.Round5(segments[i].CostCredits)
		segments[i].SaleCredits = decimalx.Round5(segments[i].SaleCredits)
	}
	return segments, switches
}
