package console

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/gateway"
	"github.com/team/llmgateway/internal/pkg/decimalx"
)

// Line 账单拆解的一行（如 "输入 1,234 × 1.0"）。
type Line struct {
	Label  string  `json:"Label"`
	Amount float64 `json:"Amount"`
}

// Step 账单拆解的一个步骤（Token × 单价 / 系数调整 / 积分换算）。
type Step struct {
	Title    string  `json:"Title"`
	Lines    []Line  `json:"Lines"`
	Subtotal float64 `json:"Subtotal"`
}

// RouteDiff 成本模式的额外路由差异展示（sale 模式或无差异时为 nil）。
type RouteDiff struct {
	Label string `json:"Label"`
	Value string `json:"Value"`
}

// BillingDetail 账单详情（admin/dev 契约共用；InternalModelID/ChannelName 仅 admin 返回；
// CallLog 为可选字段，查询不到调用日志时为 null——数组字段无则不带，单对象可 null）。
type BillingDetail struct {
	BillingID       string     `json:"BillingID"`
	CallTime        string     `json:"CallTime"`
	Model           string     `json:"Model"`
	PricingMode     string     `json:"PricingMode"`
	CreditsConsumed float64    `json:"CreditsConsumed"`
	Status          string     `json:"Status"`
	Steps           []Step     `json:"Steps"`
	RouteDiff       *RouteDiff `json:"RouteDiff"`
	// admin only（只读）
	InternalModelID string `json:"InternalModelID,omitempty"`
	ChannelName     string `json:"ChannelName,omitempty"`
	// CallLog 可选：call_logs 按 billing_id 1:1 关联的调用日志（无记录/未装配时 null）。
	CallLog *CallLogView `json:"CallLog,omitempty"`
}

// CallLogView 调用日志只读视图（会话调用列表项与 BillingDetail.CallLog 共用）。
// ReqMessages/Decision 为 JSONB 原始 value 反序列化结果（null 给 nil）；雪花 ID 以字符串序列化。
type CallLogView struct {
	BillingID    string `json:"BillingID"`
	RequestID    string `json:"RequestID"`
	SessionID    string `json:"SessionID"`
	Model        string `json:"Model"`
	Status       string `json:"Status"`
	RespKind     string `json:"RespKind"`
	ErrorMessage string `json:"ErrorMessage"`
	DurationMs   *int64 `json:"DurationMs,omitempty"`
	FirstTokenMs *int64 `json:"FirstTokenMs,omitempty"`
	CallTime     string `json:"CallTime"`
	ReqMessages  any    `json:"ReqMessages"` // JSONB 原始 value（可能 null）
	RespBody     string `json:"RespBody"`
	Decision     any    `json:"Decision"` // JSONB 原始 value（可能 null）
	PricingMode  string `json:"PricingMode"`
}

// toCallLogView 由存储层 CallLog 组装只读视图；ReqMessages/Decision 反序列化为 any，null 给 nil。
func toCallLogView(cl *gateway.CallLog) *CallLogView {
	if cl == nil {
		return nil
	}
	reqMsgs, decision := cl.ReqMessages, cl.Decision
	if raw, ok := cl.ReqMessages.(json.RawMessage); ok && len(raw) > 0 {
		var v any
		if err := json.Unmarshal(raw, &v); err == nil {
			reqMsgs = v
		}
	}
	if raw, ok := cl.Decision.(json.RawMessage); ok && len(raw) > 0 {
		var v any
		if err := json.Unmarshal(raw, &v); err == nil {
			decision = v
		}
	}
	return &CallLogView{
		BillingID:    cl.BillingID,
		RequestID:    cl.RequestID,
		SessionID:    cl.SessionID,
		Model:        cl.Model,
		Status:       cl.Status,
		RespKind:     cl.RespKind,
		ErrorMessage: cl.ErrorMessage,
		DurationMs:   cl.DurationMs,
		FirstTokenMs: cl.FirstTokenMs,
		CallTime:     cl.CreatedAt.Format(time.RFC3339),
		ReqMessages:  reqMsgs,
		RespBody:     cl.RespBody,
		Decision:     decision,
		PricingMode:  cl.PricingMode,
	}
}

// 五段 token 的展示名与取值。
var rateFields = []struct {
	Key   string
	Label string
	get   func(*billing.Usage) int64
}{
	{"input", "输入", func(u *billing.Usage) int64 { return u.Input }},
	{"output", "输出", func(u *billing.Usage) int64 { return u.Output }},
	{"cache_read", "缓存读", func(u *billing.Usage) int64 { return u.CacheRead }},
	{"cache_write", "缓存写", func(u *billing.Usage) int64 { return u.CacheWrite }},
	{"reasoning", "推理", func(u *billing.Usage) int64 { return u.Reasoning }},
}

// buildBreakdown 由 billing.Record 组装三步拆解。金额依次 Round5。
func buildBreakdown(rec *billing.Record) ([]Step, *RouteDiff) {
	// 步骤一：Token × 单价
	line1 := make([]Line, 0, 5)
	sub1 := 0.0
	for _, f := range rateFields {
		tok := f.get(&rec.Tokens)
		rate, ok := rec.Rates[f.Key]
		if !ok || tok <= 0 {
			continue
		}
		amt := decimalx.Round5(float64(tok) * rate)
		line1 = append(line1, Line{
			Label:  fmt.Sprintf("%s %s × %s", f.Label, formatInt(tok), formatFloat(rate)),
			Amount: amt,
		})
		sub1 += amt
	}
	sub1 = decimalx.Round5(sub1)

	// 步骤二：系数调整
	timeCoeff := rec.Coefficients.Time
	ctxCoeff := rec.Coefficients.Context
	line2 := []Line{
		{Label: "时段系数 ×" + formatFloat(timeCoeff), Amount: timeCoeff},
		{Label: "上下文分档 ×" + formatFloat(ctxCoeff), Amount: ctxCoeff},
	}
	sub2 := decimalx.Round5(sub1 * timeCoeff * ctxCoeff)

	// 步骤三：积分换算
	// R 值为非正（历史/异常数据）时不执行除法，避免产生 NaN/Inf 导致响应序列化失败。
	convert := 0.0
	rLabel := fmt.Sprintf("÷ %d (R)", rec.RValue)
	if rec.RValue > 0 {
		convert = sub2 / float64(rec.RValue)
	} else {
		rLabel = "R 值缺失，无法换算积分"
	}
	line3 := []Line{{Label: rLabel, Amount: decimalx.Round5(convert)}}

	steps := []Step{
		{Title: "Token × 单价", Lines: line1, Subtotal: sub1},
		{Title: "系数调整", Lines: line2, Subtotal: sub2},
		{Title: "积分换算", Lines: line3, Subtotal: decimalx.Round5(convert)},
	}

	// 成本模式的额外路由差异（rec.CostCredits 与销售积分不一致时展示）。
	var routeDiff *RouteDiff
	if rec.PricingMode == "cost" && rec.CostCredits > 0 && rec.CreditsConsumed > 0 {
		ratio := rec.CreditsConsumed / rec.CostCredits
		diff := rec.CreditsConsumed - rec.CostCredits
		if ratio != 1.0 {
			routeDiff = &RouteDiff{
				Label: "路由差异 ×" + formatFloat(decimalx.Round2(ratio)),
				Value: strconv.FormatFloat(decimalx.Round2(diff), 'f', 2, 64),
			}
		}
	}
	return steps, routeDiff
}

func formatInt(v int64) string     { return strconv.FormatInt(v, 10) }
func formatFloat(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }
