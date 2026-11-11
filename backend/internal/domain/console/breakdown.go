package console

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/gateway"
	"github.com/team/llmgateway/internal/pkg/decimalx"
	"github.com/team/llmgateway/internal/pkg/errbody"
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
	AutoModelName   string     `json:"AutoModelName,omitempty"` // 请求的 Auto 名（非 Auto 请求为空）
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
	BillingID     string `json:"BillingID"`
	RequestID     string `json:"RequestID"`
	SessionID     string `json:"SessionID"`
	Model         string `json:"Model"`
	AutoModelName string `json:"AutoModelName,omitempty"` // 请求的 Auto 名（非 Auto 请求为空）
	Status        string `json:"Status"`
	RespKind      string `json:"RespKind"`
	ErrorMessage  string `json:"ErrorMessage"`
	// UpstreamStatus 上游真实 HTTP 状态码（仅上游非 2xx 的失败记录有值，nil=无上游状态码）：
	// 让「上游 400」与「网关 502」在账单详情里可区分，不必从 ErrorMessage 的中文里推断。
	UpstreamStatus *int   `json:"UpstreamStatus,omitempty"`
	DurationMs     *int64 `json:"DurationMs,omitempty"`
	FirstTokenMs   *int64 `json:"FirstTokenMs,omitempty"`
	CallTime       string `json:"CallTime"`
	ReqMessages    any    `json:"ReqMessages"` // JSONB 原始 value（可能 null）
	// ReqParams 入站请求参数快照（JSONB 反序列化结果：model/temperature/tools 摘要/端点/上游协议与
	// 上游 URL/token_id 等；可能 null）。定位「这次到底发了什么参数」用，不含 messages（见 ReqMessages）。
	ReqParams any    `json:"ReqParams"`
	RespBody  string `json:"RespBody"`
	// RespBodyTruncated 仅**列表里的失败行**（resp_kind=error）可能为 true：受响应体积预算限制，
	// 该行的 RespBody 只是预览，完整原文走该账单的详情接口（那里不做预览截断）。
	// 成功行恒为 false：会话重放页直接用它的 RespBody 渲染回答正文，截了就是半截回答。
	RespBodyTruncated bool `json:"RespBodyTruncated,omitempty"`
	Decision          any  `json:"Decision"` // JSONB 原始 value（可能 null）
	// UpstreamUsage 上游原始 usage（JSONB 反序列化结果：{"Protocol":...,"Usage":{...}}，可能 null）：
	// 诊断渠道 usage 口径用（billing_records.tokens 是归一化后的三段，看不出上游原文）。
	UpstreamUsage any    `json:"UpstreamUsage"`
	PricingMode   string `json:"PricingMode"`
}

// callListBodyPreviewBytes 会话调用**列表**里**失败行** resp_body 的预览上限。
// 失败行的 resp_body 是上游错误报文，最长可达 32KB，一个会话上百次失败就是数 MB JSON；
// 列表只承载「这次为什么失败」的线索，要看完整原文去账单详情（详情接口不截断）。
const callListBodyPreviewBytes = 4 << 10

// previewCallLogBody 只对**失败行**（resp_kind=error）的 resp_body 做预览截断。
//
// 为什么必须按行判：成功行的 resp_body 是真实的模型回答正文，前端会话重放页
// （views/admin/SessionDetail.vue）**直接用它渲染** assistant 内容、不再另取详情，
// 无差别截断会让超过 4KB 的回答在重放里显示成半截（实测 21,600 字节 → 4,094 字节）。
// 成功行原样返回、RespBodyTruncated 恒为 false。
//
// 返回是否发生了截断，供视图层置 RespBodyTruncated。
func previewCallLogBody(respKind, body string) (string, bool) {
	if respKind != "error" {
		return body, false
	}
	if len(body) <= callListBodyPreviewBytes {
		return body, false
	}
	note := fmt.Sprintf("\n…（已截断：列表只展示前 %d 字节，完整原文 %d 字节见该账单详情接口）",
		callListBodyPreviewBytes, len(body))
	out, _ := errbody.TruncateUTF8(body, callListBodyPreviewBytes, note)
	return out, true
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
	var reqParams any
	if len(cl.ReqParams) > 0 {
		if err := json.Unmarshal(cl.ReqParams, &reqParams); err != nil {
			reqParams = nil
		}
	}
	var upstreamUsage any
	if len(cl.UpstreamUsage) > 0 {
		if err := json.Unmarshal(cl.UpstreamUsage, &upstreamUsage); err != nil {
			upstreamUsage = nil
		}
	}
	return &CallLogView{
		BillingID:      cl.BillingID,
		RequestID:      cl.RequestID,
		SessionID:      cl.SessionID,
		Model:          cl.Model,
		AutoModelName:  cl.AutoModelName,
		Status:         cl.Status,
		RespKind:       cl.RespKind,
		ErrorMessage:   cl.ErrorMessage,
		UpstreamStatus: cl.UpstreamStatus,
		DurationMs:     cl.DurationMs,
		FirstTokenMs:   cl.FirstTokenMs,
		CallTime:       cl.CreatedAt.Format(time.RFC3339),
		ReqMessages:    reqMsgs,
		ReqParams:      reqParams,
		RespBody:       cl.RespBody,
		Decision:       decision,
		UpstreamUsage:  upstreamUsage,
		PricingMode:    cl.PricingMode,
	}
}

// 三段 token 的展示名与取值（口径见 billing.Usage：输入为纯输入、输出含推理，三段互斥）。
var rateFields = []struct {
	Key   string
	Label string
	get   func(*billing.Usage) int64
}{
	{"input", "输入", func(u *billing.Usage) int64 { return u.Input }},
	{"output", "输出", func(u *billing.Usage) int64 { return u.Output }},
	{"cache_read", "缓存读", func(u *billing.Usage) int64 { return u.CacheRead }},
}

// buildBreakdown 由 billing.Record 组装三步拆解。金额依次 Round5。
func buildBreakdown(rec *billing.Record) ([]Step, *RouteDiff) {
	// 步骤一：Token × 单价
	line1 := make([]Line, 0, 3)
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

// ===== 开发者端可见性 =====
//
// 脱敏实现已迁到 `dev_view.go`（白名单正向构造：每个 dev 结构体逐字段列举，
// Decision/ReqParams 用键白名单过滤）。本文件只保留「开发者端账单拆解」口径。
// buildBreakdownDev 开发者端账单拆解：sale 模式与 admin 同构（单价即售价）；
// cost 模式不展示明细——成本金额属内部信息，展示即可反推平台采购价。
func buildBreakdownDev(rec *billing.Record) ([]Step, *RouteDiff) {
	if rec.PricingMode != "" && rec.PricingMode != "sale" {
		return []Step{}, nil
	}
	steps, _ := buildBreakdown(rec) // 成本模式的路由差异（RouteDiff）恒不返回
	return steps, nil
}
