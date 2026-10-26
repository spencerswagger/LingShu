package console

import (
	"encoding/json"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/gateway"
	"github.com/team/llmgateway/internal/pkg/decimalx"
)

// 本文件是**开发者端 DTO 的唯一出处**（P1 安全项：白名单正向构造）。
//
// 为什么不用「拷一份再删几个键」：删键是黑名单思路，将来 `CallLogView` / `CallLog.Decision`
// 新增字段时开发者端会**默认暴露**（PR #63 验收 P3）。这里的做法是：
//   - 每一层都有独立的 dev 结构体，字段逐个列举（新字段不会自动出现在 dev 输出里）；
//   - `Decision` / `ReqParams` 这类自由形状 JSONB 用**键白名单**过滤，
//     白名单外的键（包括将来新加的）一律不返回。
//
// 三处（列表项 / 详情 / 调用日志）共用同一份「敏感键」定义（devSensitiveKeys）与
// 同一套键白名单（devDecisionKeys / devAttemptKeys / devReqParamsKeys），避免三份口径漂移。

// devDecisionKeys 开发者端可见的决策轨迹**顶层键**白名单（其余一律不返回）。
var devDecisionKeys = []string{
	"result",           // 结论（success/failed/上游失败原因等）
	"attempts",         // 候选轨迹（逐项再按 devAttemptKeys 过滤）
	"est_input_tokens", // 预扣用的输入估算
	"time_coeff",       // 时段系数（价格构成，非成本金额）
	"ctx_coeff",        // 上下文分档系数
	"pre_consumed",     // 预扣积分（用户自己的钱）
}

// devAttemptKeys 开发者端可见的**候选轨迹项**键白名单。
var devAttemptKeys = []string{
	"order", "channel_id", "channel_name", "reason", "skipped",
	"converted", "inbound_protocol", "upstream_protocol",
	"passthrough_first", "passthrough_fallback",
	"lossy", "lossy_notes",
	"delivered", "first_byte_received", "elapsed_ms",
}

// devReqParamsKeys 开发者端可见的**请求参数快照**键白名单（不含 upstream_url：含部署名/供应商地址）。
var devReqParamsKeys = []string{
	"endpoint", "inbound_protocol", "upstream_protocol", "converted",
	"token_id", "model", "auto_model_name", "stream", "message_count",
	"tools", "params", "summarized", "dropped", "over_budget",
}

// devSensitiveKeys 三处共用的「敏感键」定义：任何出现在开发者端输出里都算泄漏。
// 单测按它做负向断言（`dev_redact_test.go`），新增敏感维度只需改这一处。
var devSensitiveKeys = []string{
	// 渠道密钥维度
	"channel_key_id", "channel_key_name", "ChannelKeyID", "KeyName",
	// 内部模型 ID / 映射
	"internal_model_id", "InternalModelID", "upstream_url",
	// 成本
	"cost", "cost_known", "Rates", "RValue", "route_time_coeff", "RouteDiff",
	// 他人维度
	"UserID", "Username", "UserNickname", "TokenName",
}

// DevCallLogView 开发者端调用日志视图（白名单字段，逐个列举）。
// JSON 键与共享的 CallLogView 保持一致，前端无需区分两套形状。
type DevCallLogView struct {
	BillingID     string `json:"BillingID"`
	RequestID     string `json:"RequestID"`
	SessionID     string `json:"SessionID"`
	Model         string `json:"Model"`
	AutoModelName string `json:"AutoModelName,omitempty"`
	Status        string `json:"Status"`
	RespKind      string `json:"RespKind"`
	ErrorMessage  string `json:"ErrorMessage"`
	DurationMs    *int64 `json:"DurationMs,omitempty"`
	FirstTokenMs  *int64 `json:"FirstTokenMs,omitempty"`
	CallTime      string `json:"CallTime"`
	ReqMessages   any    `json:"ReqMessages"`
	RespBody      string `json:"RespBody"`
	// Decision / ReqParams 为**白名单过滤后**的副本（未白名单的键不会出现）。
	Decision      any    `json:"Decision"`
	ReqParams     any    `json:"ReqParams"`
	UpstreamUsage any    `json:"UpstreamUsage"`
	PricingMode   string `json:"PricingMode"`
}

// devBillingDetail 开发者端账单详情（白名单字段，逐个列举；不含内部模型 ID / 渠道名 / 成本差异）。
type devBillingDetail struct {
	BillingID       string          `json:"BillingID"`
	CallTime        string          `json:"CallTime"`
	Model           string          `json:"Model"`
	AutoModelName   string          `json:"AutoModelName,omitempty"`
	PricingMode     string          `json:"PricingMode"`
	CreditsConsumed float64         `json:"CreditsConsumed"`
	Status          string          `json:"Status"`
	Steps           []Step          `json:"Steps"`
	RouteDiff       *RouteDiff      `json:"RouteDiff"`
	CallLog         *DevCallLogView `json:"CallLog,omitempty"`
}

// toDevCallLogView 由存储层 CallLog 组装开发者端视图（白名单正向构造）。
func toDevCallLogView(cl *gateway.CallLog) *DevCallLogView {
	if cl == nil {
		return nil
	}
	reqMsgs := decodeJSONValue(cl.ReqMessages)
	return &DevCallLogView{
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
		RespBody:      cl.RespBody,
		Decision:      filterDecisionForDev(decodeJSONValue(cl.Decision)),
		ReqParams:     filterReqParamsForDev(decodeJSONValue(cl.ReqParams)),
		UpstreamUsage: decodeUpstreamUsage(cl.UpstreamUsage),
		PricingMode:   cl.PricingMode,
	}
}

// decodeJSONValue 把 JSONB 原始值还原为 any：
// 存储层读出来是 []byte（CallLog.ReqParams）或 json.RawMessage（CallLog.Decision/ReqMessages），
// 也兼容调用方直接塞已解析的 any（测试/内存实现）。
func decodeJSONValue(v any) any {
	var raw []byte
	switch b := v.(type) {
	case json.RawMessage:
		raw = b
	case []byte:
		raw = b
	default:
		return v
	}
	if len(raw) == 0 {
		return nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// decodeUpstreamUsage 解析上游原始 usage（JSONB 字节）。
func decodeUpstreamUsage(raw []byte) any {
	if len(raw) == 0 {
		return nil
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil
	}
	return out
}

// filterDecisionForDev 决策轨迹白名单过滤：顶层按 devDecisionKeys，attempts 逐项按 devAttemptKeys。
// 形状不认识（非对象）时返回 nil——**宁可不下发，也不整块透传**。
func filterDecisionForDev(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]any, len(devDecisionKeys))
	for _, k := range devDecisionKeys {
		val, ok := m[k]
		if !ok {
			continue
		}
		if k == "attempts" {
			out[k] = filterAttemptsForDev(val)
			continue
		}
		out[k] = val
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// filterAttemptsForDev 候选轨迹逐项过滤（非对象项丢弃，避免夹带自由形状）。
func filterAttemptsForDev(v any) []any {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]any, 0, len(arr))
	for _, item := range arr {
		im, ok := item.(map[string]any)
		if !ok {
			continue
		}
		cp := make(map[string]any, len(devAttemptKeys))
		for _, k := range devAttemptKeys {
			if val, ok := im[k]; ok {
				cp[k] = val
			}
		}
		out = append(out, cp)
	}
	return out
}

// filterReqParamsForDev 请求参数快照白名单过滤（去掉 upstream_url 等内部信息）。
func filterReqParamsForDev(v any) any {
	m, ok := v.(map[string]any)
	if !ok {
		return nil
	}
	out := make(map[string]any, len(devReqParamsKeys))
	for _, k := range devReqParamsKeys {
		if val, ok := m[k]; ok {
			out[k] = val
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// devBillingListItem 开发者端账单列表项（白名单字段；与管理端 billingListItem 相比
// 不含内部模型 ID / 渠道密钥维度 / 费率与系数 / 他人维度）。
type devBillingListItem struct {
	BillingID       string            `json:"BillingID"`
	ExternalModel   string            `json:"ExternalModel"`
	AutoModelName   string            `json:"AutoModelName,omitempty"`
	PricingMode     string            `json:"PricingMode"`
	CreditsConsumed float64           `json:"CreditsConsumed"`
	Status          string            `json:"Status"`
	CallTime        time.Time         `json:"CallTime"`
	SessionID       string            `json:"SessionID,omitempty"`
	ChannelName     string            `json:"ChannelName,omitempty"` // 渠道显示名（非密钥；白名单内）
	SessionName     string            `json:"SessionName,omitempty"`
	Tokens          billing.WireUsage `json:"Tokens"`
	ErrorMessage    string            `json:"ErrorMessage,omitempty"`
	DurationMs      *int64            `json:"DurationMs,omitempty"`
	FirstTokenMs    *int64            `json:"FirstTokenMs,omitempty"`
	RequestID       string            `json:"RequestID,omitempty"`
	Attempts        int               `json:"Attempts,omitempty"`
}

// toDevBillingListItem 由账单行组装开发者端列表项（字段白名单，新增字段必须显式加进来）。
func toDevBillingListItem(rec *billing.Record) devBillingListItem {
	return devBillingListItem{
		BillingID:       rec.BillingID,
		ExternalModel:   rec.ExternalModel,
		AutoModelName:   rec.AutoModelName,
		PricingMode:     rec.PricingMode,
		CreditsConsumed: decimalx.Round5(rec.CreditsConsumed),
		Status:          rec.Status,
		CallTime:        rec.CallTime,
		SessionID:       rec.SessionID,
		SessionName:     rec.SessionName,
		Tokens:          billing.WireUsage(rec.Tokens),
		ErrorMessage:    rec.ErrorMessage,
		DurationMs:      rec.DurationMs,
		FirstTokenMs:    rec.FirstTokenMs,
		RequestID:       rec.RequestID,
		Attempts:        rec.Attempts,
	}
}
