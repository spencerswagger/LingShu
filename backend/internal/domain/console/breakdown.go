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
	DurationMs    *int64 `json:"DurationMs,omitempty"`
	FirstTokenMs  *int64 `json:"FirstTokenMs,omitempty"`
	CallTime      string `json:"CallTime"`
	ReqMessages   any    `json:"ReqMessages"` // JSONB 原始 value（可能 null）
	// ReqParams 入站请求参数快照（JSONB 反序列化结果：model/temperature/tools 摘要/端点/上游协议与
	// 上游 URL/token_id 等；可能 null）。定位「这次到底发了什么参数」用，不含 messages（见 ReqMessages）。
	ReqParams any    `json:"ReqParams"`
	RespBody  string `json:"RespBody"`
	Decision  any    `json:"Decision"` // JSONB 原始 value（可能 null）
	// UpstreamUsage 上游原始 usage（JSONB 反序列化结果：{"Protocol":...,"Usage":{...}}，可能 null）：
	// 诊断渠道 usage 口径用（billing_records.tokens 是归一化后的三段，看不出上游原文）。
	UpstreamUsage any    `json:"UpstreamUsage"`
	PricingMode   string `json:"PricingMode"`
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
		BillingID:     cl.BillingID,
		RequestID:     cl.RequestID,
		SessionID:     cl.SessionID,
		Model:         cl.Model,
		AutoModelName: cl.AutoModelName,
		Status:        cl.Status,
		RespKind:      cl.RespKind,
		ErrorMessage:  cl.ErrorMessage,
		DurationMs:    cl.DurationMs,
		FirstTokenMs:  cl.FirstTokenMs,
		CallTime:      cl.CreatedAt.Format(time.RFC3339),
		ReqMessages:   reqMsgs,
		ReqParams:     reqParams,
		RespBody:      cl.RespBody,
		Decision:      decision,
		UpstreamUsage: upstreamUsage,
		PricingMode:   cl.PricingMode,
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

// ===== 开发者端脱敏（P1 安全项）=====
//
// 脱敏位置的选择：放在**视图/DTO 层**（console 包组装 CallLogView / billingListItem 的那一层），
// 不放序列化层（不写自定义 MarshalJSON）、也不放存储层：
//   - 存储层脱敏会让管理端也看不到原值（管理端需要排查渠道密钥维度的问题）；
//   - 序列化层（MarshalJSON）脱敏会污染同一份结构的所有使用方，且难以被单测直接断言；
//   - DTO 层「同一份数据、两个投影」最直白：admin 用 toCallLogView，dev 用 toDevCallLogView，
//     谁能看到什么在类型上一目了然，也便于加断言防回归。
//
// 开发者端必须看不到：渠道密钥（id/名）、内部模型 ID 与映射、成本金额（含成本模式路由差异）。
// 可以看：自己的账单金额（积分）、自己请求的消息与参数、上游协议与用量（便于自助排查）。

// devMaskedKeys 开发者端必须抹掉的决策轨迹键（渠道密钥维度 / 内部模型维度 / 成本维度）。
var devMaskedKeys = []string{
	"channel_key_id",    // 渠道密钥维度
	"channel_key_name",  // 渠道密钥名
	"internal_model_id", // 内部模型 ID（上游模型名 = 映射关系）
	"cost",              // 成本单价/金额
	"cost_known",        // 成本是否已知（等同泄漏成本可得性）
}

// toDevCallLogView 开发者端调用日志视图：在共享视图基础上脱敏。
// 返回新对象（不修改入参），避免同一份 CallLogView 被两个投影共享后互相污染。
func toDevCallLogView(cl *gateway.CallLog) *CallLogView {
	v := toCallLogView(cl)
	if v == nil {
		return nil
	}
	v.Decision = redactDecision(v.Decision)
	v.ReqParams = redactReqParams(v.ReqParams)
	return v
}

// redactDecision 抹掉决策轨迹里的密钥/内部模型/成本键（含 attempts 数组内每一项）。
// 未知结构原样返回：脱敏只针对已知形状，绝不因为形状变化而泄漏。
func redactDecision(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		out[k] = val
	}
	for _, k := range devMaskedKeys {
		delete(out, k)
	}
	if arr, ok := out["attempts"].([]any); ok {
		cleaned := make([]any, 0, len(arr))
		for _, item := range arr {
			im, ok := item.(map[string]any)
			if !ok {
				cleaned = append(cleaned, item)
				continue
			}
			cp := make(map[string]any, len(im))
			for k, val := range im {
				cp[k] = val
			}
			for _, k := range devMaskedKeys {
				delete(cp, k)
			}
			cleaned = append(cleaned, cp)
		}
		out["attempts"] = cleaned
	}
	return out
}

// redactReqParams 抹掉开发者端不该看到的请求快照字段：目前是 upstream_url
// （含部署名 → 等价于泄漏「内部模型 ↔ 上游部署」的映射；同时避免暴露供应商内网地址）。
func redactReqParams(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return v
	}
	out := make(map[string]any, len(m))
	for k, val := range m {
		out[k] = val
	}
	delete(out, "upstream_url")
	return out
}

// buildBreakdownDev 开发者端账单拆解：sale 模式与 admin 同构（单价即售价）；
// cost 模式不展示明细——成本金额属内部信息，展示即可反推平台采购价。
func buildBreakdownDev(rec *billing.Record) ([]Step, *RouteDiff) {
	if rec.PricingMode != "" && rec.PricingMode != "sale" {
		return []Step{}, nil
	}
	steps, _ := buildBreakdown(rec) // 成本模式的路由差异（RouteDiff）恒不返回
	return steps, nil
}
