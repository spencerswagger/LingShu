package console

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/gateway"
)

// TestToDevBillingListItem_MasksInternals 断言开发者端列表项是脱敏白名单：
// 不含内部模型 ID、渠道密钥维度、成本/费率字段（P1 安全项），但保留 request_id/尝试次数与用量。
func TestToDevBillingListItem_MasksInternals(t *testing.T) {
	rec := &billing.Record{
		BillingID:       "bill-1",
		ExternalModel:   "gpt-4",
		InternalModelID: "gpt-4-azure-prod", // 内部模型 ID（映射关系）——开发者端不得可见
		ChannelKeyID:    99,                 // 渠道密钥维度——不得可见
		PricingMode:     "sale",
		CreditsConsumed: 1.5,
		Status:          "completed",
		Tokens:          billing.Usage{Input: 10, Output: 5, CacheRead: 2},
		Rates:           billing.Rates{"input": 1, "output": 2, "cache_read": 0.1, "cache_write": 0.3},
		RValue:          10000,
		RequestID:       "req-1",
		Attempts:        2,
	}
	item := toDevBillingListItem(rec)
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(raw)
	for _, banned := range []string{"InternalModelID", "ChannelKeyID", "Rates", "RValue", "gpt-4-azure-prod", "CacheWrite"} {
		if strings.Contains(out, banned) {
			t.Fatalf("dev list item leaks %q: %s", banned, out)
		}
	}
	// 该给的仍要给：账单号、对外模型、积分、用量、request_id、尝试次数。
	for _, want := range []string{`"BillingID":"bill-1"`, `"ExternalModel":"gpt-4"`, `"CreditsConsumed":1.5`, `"RequestID":"req-1"`, `"Attempts":2`} {
		if !strings.Contains(out, want) {
			t.Fatalf("dev list item missing %q: %s", want, out)
		}
	}
}

// TestToDevCallLogView_MasksDecisionAndReqParams 断言开发者端调用日志脱敏：
// 决策轨迹里的渠道密钥/内部模型 ID/成本键被抹掉（含 attempts 内每一项），
// 请求快照里的上游 URL 被抹掉；其余诊断信息（状态、错误、用量、参数）保留。
func TestToDevCallLogView_MasksDecisionAndReqParams(t *testing.T) {
	cl := &gateway.CallLog{
		BillingID: "bill-1",
		RequestID: "req-1",
		Status:    "failed",
		Decision: map[string]any{
			"result":            "failed",
			"pre_consumed":      0.5,
			"channel_key_id":    "88",
			"channel_key_name":  "secret-key",
			"internal_model_id": "gpt-4-azure-prod",
			"cost":              0.77,
			"cost_known":        true,
			"attempts": []any{
				map[string]any{
					"order":             1,
					"channel_name":      "azure-1",
					"channel_key_id":    "88",
					"channel_key_name":  "secret-key",
					"internal_model_id": "gpt-4-azure-prod",
					"cost":              0.77,
					"reason":            "上游返回 400",
				},
			},
		},
		ReqParams: []byte(`{"endpoint":"/v1/chat/completions","model":"gpt-4","upstream_url":"https://r.openai.azure.com/openai/deployments/gpt4o/chat/completions","temperature":0.3}`),
	}
	v := toDevCallLogView(cl)
	if v == nil {
		t.Fatal("nil view")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	out := string(raw)
	for _, banned := range []string{"channel_key_id", "channel_key_name", "secret-key", "internal_model_id",
		"gpt-4-azure-prod", `"cost"`, "cost_known", "upstream_url", "r.openai.azure.com"} {
		if strings.Contains(out, banned) {
			t.Fatalf("dev call log leaks %q: %s", banned, out)
		}
	}
	// 仍保留：状态、候选理由、请求参数、自有积分（pre_consumed）。
	for _, want := range []string{`"Status":"failed"`, "上游返回 400", `"temperature":0.3`, `"pre_consumed":0.5`, `"channel_name":"azure-1"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("dev call log missing %q: %s", want, out)
		}
	}
	// 原对象不得被就地修改（两个投影共用一个 CallLog 时互不污染）。
	if _, ok := cl.Decision.(map[string]any)["internal_model_id"]; !ok {
		t.Fatal("source decision must stay untouched (redaction works on a copy)")
	}
}

// TestBuildBreakdownDev_HidesCostMode 断言开发者端拆解不展示成本模式明细（成本金额属内部信息），
// 且任何情况下都不返回成本模式的路由差异。
func TestBuildBreakdownDev_HidesCostMode(t *testing.T) {
	cost := &billing.Record{
		PricingMode:     "cost",
		Tokens:          billing.Usage{Input: 1000, Output: 500},
		Rates:           billing.Rates{"input": 0.8, "output": 1.6},
		RValue:          10000,
		CreditsConsumed: 2,
		CostCredits:     1,
	}
	cost.Coefficients.Time = 1
	cost.Coefficients.Context = 1
	steps, diff := buildBreakdownDev(cost)
	if len(steps) != 0 {
		t.Fatalf("cost mode steps=%v, want empty", steps)
	}
	if diff != nil {
		t.Fatalf("cost mode route diff=%+v, want nil", diff)
	}

	sale := &billing.Record{
		PricingMode: "sale",
		Tokens:      billing.Usage{Input: 1000, Output: 500},
		Rates:       billing.Rates{"input": 1, "output": 2},
		RValue:      10000,
	}
	sale.Coefficients.Time = 1
	sale.Coefficients.Context = 1
	steps2, diff2 := buildBreakdownDev(sale)
	if len(steps2) == 0 || diff2 != nil {
		t.Fatalf("sale mode steps=%d diff=%+v, want non-empty steps and nil diff", len(steps2), diff2)
	}
}
