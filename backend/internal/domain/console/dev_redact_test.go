package console

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/gateway"
)

// assertNoSensitive 断言输出 JSON 里不含任何敏感键（三处共用 devSensitiveKeys 单一来源）。
func assertNoSensitive(t *testing.T, out, surface string) {
	t.Helper()
	for _, banned := range devSensitiveKeys {
		if strings.Contains(out, `"`+banned+`"`) {
			t.Fatalf("%s 泄漏敏感键 %q: %s", surface, banned, out)
		}
	}
}

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
	assertNoSensitive(t, out, "dev 列表项")
	for _, leak := range []string{"gpt-4-azure-prod", "CacheWrite", "0.3"} {
		if strings.Contains(out, leak) {
			t.Fatalf("dev 列表项泄漏 %q: %s", leak, out)
		}
	}
	// 该给的仍要给：账单号、对外模型、积分、用量、request_id、尝试次数。
	for _, want := range []string{`"BillingID":"bill-1"`, `"ExternalModel":"gpt-4"`, `"CreditsConsumed":1.5`, `"RequestID":"req-1"`, `"Attempts":2`} {
		if !strings.Contains(out, want) {
			t.Fatalf("dev 列表项缺少 %q: %s", want, out)
		}
	}
}

// TestToDevCallLogView_WhitelistOnly 断言开发者端调用日志是**白名单正向构造**：
//   - 敏感键（密钥/内部模型/成本/上游 URL）不出现；
//   - 决策轨迹与请求快照里**将来新增的未知键**同样不出现（这是「删键式黑名单」做不到的，
//     也是 PR #63 验收 P3 的核心）；
//   - 白名单内的键（含嵌套 params 里的采样参数）完整保留，JSON 键名与共享视图一致。
func TestToDevCallLogView_WhitelistOnly(t *testing.T) {
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
			"future_secret":     "should-never-leak", // 模拟将来新增的敏感字段
			"attempts": []any{
				map[string]any{
					"order":              1,
					"channel_name":       "azure-1",
					"channel_key_id":     "88",
					"channel_key_name":   "secret-key",
					"internal_model_id":  "gpt-4-azure-prod",
					"cost":               0.77,
					"route_time_coeff":   1.2,
					"future_attempt_key": "should-never-leak",
					"reason":             "上游返回 400",
				},
			},
		},
		ReqParams: []byte(`{"endpoint":"/v1/chat/completions","model":"gpt-4",
			"upstream_url":"https://r.openai.azure.com/openai/deployments/gpt4o/chat/completions",
			"future_param_key":"should-never-leak",
			"params":{"temperature":0.3,"max_tokens":256}}`),
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
	assertNoSensitive(t, out, "dev 调用日志")
	for _, banned := range []string{"secret-key", "gpt-4-azure-prod", "r.openai.azure.com",
		"future_secret", "future_attempt_key", "future_param_key", "should-never-leak"} {
		if strings.Contains(out, banned) {
			t.Fatalf("dev 调用日志泄漏 %q: %s", banned, out)
		}
	}
	// 白名单内仍保留：状态、候选理由、渠道名、嵌套的采样参数、自有积分。
	for _, want := range []string{`"Status":"failed"`, "上游返回 400", `"channel_name":"azure-1"`,
		`"temperature":0.3`, `"max_tokens":256`, `"pre_consumed":0.5`, `"endpoint":"/v1/chat/completions"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("dev 调用日志缺少 %q: %s", want, out)
		}
	}
	// 原对象不得被就地修改（同一 CallLog 可能同时被 admin 视图使用）。
	if _, ok := cl.Decision.(map[string]any)["internal_model_id"]; !ok {
		t.Fatal("source decision must stay untouched (filtering works on a copy)")
	}
}

// TestDevViewWhitelists_SingleSource 断言三处白名单口径不漂移：
//   - 三份键白名单都**不收录**任何敏感键（敏感键定义是单一来源 devSensitiveKeys）；
//   - 每份内部无重复；
//   - 关键可诊断键仍在白名单里（避免以后误删导致前端丢字段），且 upstream_url 永不收录。
func TestDevViewWhitelists_SingleSource(t *testing.T) {
	sensitive := make(map[string]bool, len(devSensitiveKeys))
	for _, k := range devSensitiveKeys {
		sensitive[k] = true
	}
	check := func(name string, keys []string) {
		seen := make(map[string]bool, len(keys))
		for _, k := range keys {
			if sensitive[k] {
				t.Fatalf("%s 白名单收录了敏感键 %q", name, k)
			}
			if seen[k] {
				t.Fatalf("%s 内部重复键 %q", name, k)
			}
			seen[k] = true
		}
	}
	check("devDecisionKeys", devDecisionKeys)
	check("devAttemptKeys", devAttemptKeys)
	check("devReqParamsKeys", devReqParamsKeys)

	// 关键项必须在白名单里（避免以后误删导致前端丢字段）。
	for _, need := range []string{"result", "attempts", "pre_consumed"} {
		if !containsStr(devDecisionKeys, need) {
			t.Fatalf("devDecisionKeys 缺少 %q", need)
		}
	}
	for _, need := range []string{"endpoint", "upstream_protocol", "params", "tools", "dropped"} {
		if !containsStr(devReqParamsKeys, need) {
			t.Fatalf("devReqParamsKeys 缺少 %q", need)
		}
	}
	if containsStr(devReqParamsKeys, "upstream_url") {
		t.Fatal("devReqParamsKeys 不得包含 upstream_url（含部署名/供应商地址）")
	}
	// 敏感键清单本身要覆盖三类红线（密钥 / 内部模型 / 成本），否则负向断言会形同虚设。
	for _, need := range []string{"channel_key_id", "internal_model_id", "cost", "Rates", "upstream_url"} {
		if !containsStr(devSensitiveKeys, need) {
			t.Fatalf("devSensitiveKeys 缺少红线键 %q", need)
		}
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

// containsStr 判断切片里是否存在目标串（测试小工具）。
func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}
