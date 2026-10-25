package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
)

// pricingTestAt 高峰段（18:00-22:00，系数 1.5）的时间点。
func pricingTestAt(t *testing.T) time.Time {
	t.Helper()
	return time.Date(2026, 10, 25, 19, 30, 0, 0, cnLocation())
}

// pricingTestTimeConfig 与 seeding 默认同形状的时段配置（低谷 0.5 / 普通 1.0 / 高峰 1.5）。
func pricingTestTimeConfig() billing.TimeCoeffConfig {
	return billing.TimeCoeffConfig{
		Timezone: "Asia/Shanghai",
		Default:  1.0,
		Periodic: []billing.Segment{
			{Name: "低谷", Start: "00:00", End: "08:00", Coeff: 0.5},
			{Name: "普通", Start: "08:00", End: "18:00", Coeff: 1.0},
			{Name: "高峰", Start: "18:00", End: "22:00", Coeff: 1.5},
			{Name: "低谷", Start: "22:00", End: "24:00", Coeff: 0.5},
		},
	}
}

func int64p(v int64) *int64 { return &v }

// TestBuildPricingLine_TierBreaksAndOverTier 断言：跳档拐点位置、超档区段、含/不含系数两套金额。
// 公式手算（R=10000，时段系数 1.5，三段费率 input=1 / cache_read=0.1 / output=2）：
//
//	tokens=1000，hit=0 → input=1000, output=100, Σ=1000×1+100×2=1200
//	  含系数（ctx=1.0）= 1200×1.5×1.0÷10000 = 0.18
//	  不含系数        = 1200÷10000           = 0.12
func TestBuildPricingLine_TierBreaksAndOverTier(t *testing.T) {
	cfg := pricingModelConfig{
		Source:           "external_model",
		Name:             "gpt-x",
		ExternalModelID:  11,
		Rates:            billing.Rates{"input": 1, "cache_read": 0.1, "output": 2},
		TimeConfig:       pricingTestTimeConfig(),
		TimeConfigSource: "global",
		Tiers: []billing.TierRule{
			{Min: 0, Max: int64p(8000), Coeff: 1.0},
			{Min: 8001, Max: int64p(32000), Coeff: 1.2},
			{Min: 32001, Max: int64p(128000), Coeff: 1.5},
		},
		TierSource: "model",
		Available:  true,
	}
	p := pricingCurveParams{Step: 1000, MaxTokens: 130000, HitRate: 0, InOutRatio: 10, At: pricingTestAt(t)}

	line, err := buildPricingLine(cfg, p, 10000)
	if err != nil {
		t.Fatalf("buildPricingLine: %v", err)
	}
	if line.TimeCoeff != 1.5 {
		t.Fatalf("时段系数=%v，want 1.5（19:30 落在高峰段）", line.TimeCoeff)
	}
	if len(line.Points) != 130 {
		t.Fatalf("点数=%d，want 130", len(line.Points))
	}
	first := line.Points[0]
	if first.Tokens != 1000 || first.InputTokens != 1000 || first.CacheReadTokens != 0 || first.OutputTokens != 100 {
		t.Fatalf("首个点 token 拆分错: %+v", first)
	}
	if first.CtxCoeff != 1.0 || first.Credits != 0.18 || first.CreditsNoCoeff != 0.12 {
		t.Fatalf("首点金额错: %+v", first)
	}

	// 跳档拐点：两处（8001 从 1.0→1.2、32001 从 1.2→1.5）。
	wantBreaks := []pricingTierBreak{
		{Tokens: 8001, FromCoeff: 1.0, ToCoeff: 1.2},
		{Tokens: 32001, FromCoeff: 1.2, ToCoeff: 1.5},
	}
	if len(line.TierBreaks) != len(wantBreaks) {
		t.Fatalf("拐点数=%d，want %d：%+v", len(line.TierBreaks), len(wantBreaks), line.TierBreaks)
	}
	for i, want := range wantBreaks {
		if line.TierBreaks[i] != want {
			t.Fatalf("第 %d 个拐点=%+v，want %+v", i, line.TierBreaks[i], want)
		}
	}

	// 超过最后一档上界（128000）→ 129000/130000 两个点是超档区段。
	if len(line.OverTier) != 1 || line.OverTier[0].FromTokens != 129000 || line.OverTier[0].ToTokens != 130000 {
		t.Fatalf("超档区段错: %+v", line.OverTier)
	}
	last := line.Points[len(line.Points)-1]
	if !last.OverTier || last.Credits != 0 || last.CtxCoeff != 0 {
		t.Fatalf("超档点应标记且不给假金额: %+v", last)
	}

	// 含系数/不含系数两条序列：比值恒等于 timeCoeff × ctxCoeff。
	for _, pt := range line.Points {
		if pt.OverTier {
			continue
		}
		want := decimal5(pt.CreditsNoCoeff * line.TimeCoeff * pt.CtxCoeff)
		if pt.Credits != want {
			t.Fatalf("token=%d 含系数金额=%v，want %v（NoCoeff=%v × time %v × ctx %v）",
				pt.Tokens, pt.Credits, want, pt.CreditsNoCoeff, line.TimeCoeff, pt.CtxCoeff)
		}
	}
}

// decimal5 与 decimalx.Round5 同口径（测试内联，避免为测试扩大依赖面）。
func decimal5(v float64) float64 {
	return float64(int64(v*1e5+0.5)) / 1e5
}

// TestBuildPricingLine_TierGapOverTier 断言档与档之间留缝时，缝内 token 被判为超档。
func TestBuildPricingLine_TierGapOverTier(t *testing.T) {
	cfg := pricingModelConfig{
		Source:     "external_model",
		Rates:      billing.Rates{"input": 1, "cache_read": 0.1, "output": 2},
		TimeConfig: billing.TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1},
		Tiers: []billing.TierRule{
			{Min: 0, Max: int64p(8000), Coeff: 1.0},
			{Min: 10000, Max: int64p(20000), Coeff: 1.5},
		},
		TierSource: "model",
		Available:  true,
	}
	p := pricingCurveParams{Step: 1000, MaxTokens: 11000, HitRate: 0, InOutRatio: 0,
		At: time.Date(2026, 10, 25, 12, 0, 0, 0, cnLocation())}
	line, err := buildPricingLine(cfg, p, 10000)
	if err != nil {
		t.Fatalf("buildPricingLine: %v", err)
	}
	// 档 [0,8000] 与 [10000,20000] 之间有缝：采样点里只有 9000 落在缝内。
	if len(line.OverTier) != 1 {
		t.Fatalf("应有 1 段超档（9000 落在 8001-9999 的缝里），got %+v", line.OverTier)
	}
	if line.OverTier[0].FromTokens != 9000 || line.OverTier[0].ToTokens != 9000 {
		t.Fatalf("超档区段错: %+v", line.OverTier[0])
	}
}

// TestBuildPricingLine_NoTiers 断言未配分档：系数恒 1、无拐点、无超档。
func TestBuildPricingLine_NoTiers(t *testing.T) {
	cfg := pricingModelConfig{
		Source:     "channel_model",
		Rates:      billing.Rates{"input": 2, "cache_read": 0.2, "output": 4},
		TimeConfig: billing.TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1},
		Tiers:      []billing.TierRule{},
		TierSource: "none",
		Available:  true,
	}
	p := pricingCurveParams{Step: 1000, MaxTokens: 5000, HitRate: 0.5, InOutRatio: 4,
		At: time.Date(2026, 10, 25, 12, 0, 0, 0, cnLocation())}
	line, err := buildPricingLine(cfg, p, 10000)
	if err != nil {
		t.Fatalf("buildPricingLine: %v", err)
	}
	if len(line.TierBreaks) != 0 || len(line.OverTier) != 0 {
		t.Fatalf("无分档时不应有拐点/超档: %+v %+v", line.TierBreaks, line.OverTier)
	}
	for _, pt := range line.Points {
		if pt.CtxCoeff != 1 || pt.OverTier || pt.Credits == 0 {
			t.Fatalf("无分档时系数应为 1 且不超档: %+v", pt)
		}
		// 缓存读按 HitRate 拆分：tokens=1000 → cache_read=500、input=500。
		if pt.CacheReadTokens != pt.Tokens/2 {
			t.Fatalf("缓存读拆分错: %+v", pt)
		}
	}
	if line.Tiers == nil {
		t.Fatalf("Tiers 必须是空数组不是 nil（JSON 不能出 null）")
	}
}

// TestBuildPricingLine_Unavailable 断言缺配置时返回 Available=false + 空数组，不 panic、不返 nil。
func TestBuildPricingLine_Unavailable(t *testing.T) {
	cfg := pricingModelConfig{Source: "channel_model", Available: false, Reason: "渠道内部模型未配置成本费率"}
	line, err := buildPricingLine(cfg, pricingCurveParams{Step: 1000, MaxTokens: 10000}, 10000)
	if err != nil {
		t.Fatalf("buildPricingLine: %v", err)
	}
	if line.Available || line.Reason == "" {
		t.Fatalf("不可用线应带原因: %+v", line)
	}
	if line.Points == nil || line.TierBreaks == nil || line.OverTier == nil || line.Tiers == nil {
		t.Fatalf("不可用线的集合字段也必须是空数组: %+v", line)
	}
}

// TestBuildPricingLine_PointCap 断言点数上限：超大 MaxTokens 自动截断到 2000 点，
// 响应里的 MaxTokens 与点一致（由调用方先算好，这里验证 builder 的兜底）。
func TestBuildPricingLine_PointCap(t *testing.T) {
	cfg := pricingModelConfig{
		Source:     "external_model",
		Rates:      billing.Rates{"input": 1, "output": 1, "cache_read": 1},
		TimeConfig: billing.TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1},
		Available:  true,
	}
	line, err := buildPricingLine(cfg, pricingCurveParams{Step: 10, MaxTokens: 1_000_000}, 10000)
	if err != nil {
		t.Fatalf("buildPricingLine: %v", err)
	}
	if len(line.Points) != pricingCurveMaxPoints {
		t.Fatalf("点数=%d，want %d", len(line.Points), pricingCurveMaxPoints)
	}
}

// TestParsePricingCurveParams 断言参数缺省/收敛/报错。
func TestParsePricingCurveParams(t *testing.T) {
	now := pricingTestAt(t)
	p, err := parsePricingCurveParams(url.Values{}, now)
	if err != nil {
		t.Fatalf("缺省解析失败: %v", err)
	}
	if p.Step != 1000 || p.MaxTokens != 0 || p.HitRate != 0.9 || p.InOutRatio != 10 || !p.At.Equal(now) {
		t.Fatalf("缺省值错: %+v", p)
	}

	p, err = parsePricingCurveParams(url.Values{
		"Step": {"0"}, "MaxTokens": {"99999999"}, "HitRate": {"5"}, "InOutRatio": {"-1"},
	}, now)
	if err != nil {
		t.Fatalf("越界应收敛: %v", err)
	}
	if p.Step != 1000 || p.MaxTokens != pricingCurveMaxMaxToken || p.HitRate != 1 || p.InOutRatio != 0 {
		t.Fatalf("收敛错: %+v", p)
	}

	if _, err := parsePricingCurveParams(url.Values{"Step": {"abc"}}, now); err == nil {
		t.Fatalf("非数字 Step 应报错")
	}
	if _, err := parsePricingCurveParams(url.Values{"HitRate": {"NaN"}}, now); err == nil {
		t.Fatalf("NaN HitRate 应报错")
	}
	// At 支持 HH:MM（按 Asia/Shanghai 当天）与 RFC3339。
	p, err = parsePricingCurveParams(url.Values{"At": {"19:30"}}, now)
	if err != nil {
		t.Fatalf("HH:MM 解析失败: %v", err)
	}
	if p.At.Hour() != 19 || p.At.Minute() != 30 || p.At.Day() != now.Day() {
		t.Fatalf("HH:MM 解析错: %v", p.At)
	}
	p, err = parsePricingCurveParams(url.Values{"At": {"2026-10-25T02:00:00Z"}}, now)
	if err != nil {
		t.Fatalf("RFC3339 解析失败: %v", err)
	}
	if p.At.In(cnLocation()).Hour() != 10 {
		t.Fatalf("UTC 02:00 应换算为北京 10:00，got %v", p.At.In(cnLocation()))
	}
	if _, err := parsePricingCurveParams(url.Values{"At": {"昨天"}}, now); err == nil {
		t.Fatalf("非法 At 应报错")
	}
}

// TestTimeOptions 断言时段下拉：去重 + 无周期段时给「全天」兜底。
func TestTimeOptions(t *testing.T) {
	opts, def := timeOptions(pricingTestTimeConfig())
	if def != 1.0 || len(opts) != 4 {
		t.Fatalf("时段选项错: def=%v opts=%+v", def, opts)
	}
	if opts[0].Name != "低谷" || opts[0].Coeff != 0.5 {
		t.Fatalf("首项错: %+v", opts[0])
	}
	empty, def := timeOptions(billing.TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1})
	if len(empty) != 1 || empty[0].Name != "全天" || empty[0].Coeff != 1 || def != 1 {
		t.Fatalf("无周期段应兜底「全天」: %+v def=%v", empty, def)
	}
}

// TestMergeTiersAndTimeConfig 断言模型级/全局的回落与来源标记。
func TestMergeTiersAndTimeConfig(t *testing.T) {
	globalTiers := []billing.TierRule{{Min: 0, Max: int64p(1000), Coeff: 1}}
	modelTiers := []byte(`[{"Min":0,"Max":null,"Coeff":2}]`)

	if tiers, src := mergeTiers(modelTiers, globalTiers); src != "model" || len(tiers) != 1 || tiers[0].Coeff != 2 {
		t.Fatalf("模型级分档应优先: %+v %s", tiers, src)
	}
	if tiers, src := mergeTiers(nil, globalTiers); src != "global" || len(tiers) != 1 {
		t.Fatalf("NULL 应回落全局: %+v %s", tiers, src)
	}
	if tiers, src := mergeTiers([]byte(`[]`), nil); src != "none" || tiers == nil || len(tiers) != 0 {
		t.Fatalf("全空应为 none + 空数组: %+v %s", tiers, src)
	}

	g := pricingTestTimeConfig()
	if cfg, src := mergeTimeConfig(nil, g); src != "global" || cfg.Periodic[0].Coeff != 0.5 {
		t.Fatalf("NULL 时段配置应回落全局: %+v %s", cfg, src)
	}
	if cfg, src := mergeTimeConfig([]byte(`{}`), g); src != "global" {
		t.Fatalf("空对象应回落全局: %+v %s", cfg, src)
	}
	modelTime := []byte(`{"Timezone":"Asia/Shanghai","Default":2.0,"Periodic":[{"Name":"整点","Start":"00:00","End":"24:00","Coeff":2.0}]}`)
	if cfg, src := mergeTimeConfig(modelTime, g); src != "model" || cfg.Default != 2.0 || cfg.Periodic[0].Coeff != 2.0 {
		t.Fatalf("模型级时段应优先: %+v %s", cfg, src)
	}
}

// TestParseSnapshotNames 断言快照名解析：JSON 数组 → 字典序；空/非法 → 空切片（不是 nil）。
func TestParseSnapshotNames(t *testing.T) {
	got := parseSnapshotNames(`["gpt-4o","gpt-4o-legacy","gpt-4o"]`)
	if len(got) != 2 || got[0] != "gpt-4o" || got[1] != "gpt-4o-legacy" {
		t.Fatalf("快照名解析错: %v", got)
	}
	for _, raw := range []string{"", "null", "not-json"} {
		if out := parseSnapshotNames(raw); out == nil || len(out) != 0 {
			t.Fatalf("raw=%q 应返回空切片，got %v", raw, out)
		}
	}
}

// costStatsBody 成本统计响应结构。
type costStatsBody struct {
	Code int `json:"Code"`
	Data struct {
		Rows  []costStatsRow `json:"Rows"`
		Total costStatsTotal `json:"Total"`
	} `json:"Data"`
}

// TestHandleCostStats_EntityRows 断言实体聚合行：稳定键、解析出的 ID、曾用名快照、金额与合计逐位相等。
func TestHandleCostStats_EntityRows(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, nil, nil, nil)
	admin.now = func() time.Time { return time.Date(2026, 10, 25, 15, 30, 0, 0, cnLocation()) }

	mock.ExpectQuery(regexp.QuoteMeta(`to_json(array_agg(DISTINCT b.external_model_name))::text AS snapshot_names`)).
		WillReturnRows(sqlmock.NewRows([]string{
			"entity_key", "channel_id", "channel_name", "channel_model_id", "external_model_id",
			"internal_model_id", "external_model_name", "cost_credits", "sale_credits", "requests",
			"input_tokens", "cache_read_tokens", "output_tokens", "snapshot_names",
		}).
			// 解析成功的实体（含 1 个曾用名）：金额 0.00001
			AddRow("cm:900", int64(5), "渠道A", int64(900), int64(31), "gpt-4o-int", "gpt-4o",
				float64(0.00001), float64(0.00002), int64(3), int64(100), int64(20), int64(30),
				`["gpt-4o","gpt-4o-legacy"]`).
			// 解析不到的降级桶（同一文本不同内部模型不会并到一起）
			AddRow("name:ghost-model#ghost-int", nil, "", int64(0), int64(0), "ghost-int", "ghost-model",
				float64(0.00002), float64(0.00003), int64(1), int64(10), int64(0), int64(5),
				`["ghost-model"]`))

	rec := httptest.NewRecorder()
	admin.HandleCostStats(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/stats", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码=%d body=%s", rec.Code, rec.Body.String())
	}
	var body costStatsBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data.Rows) != 2 {
		t.Fatalf("行数=%d，want 2", len(body.Data.Rows))
	}

	resolved := body.Data.Rows[0]
	if resolved.EntityKey != "cm:900" || resolved.ChannelModelID != 900 || resolved.ExternalModelID != 31 {
		t.Fatalf("实体键/解析 ID 错: %+v", resolved)
	}
	if resolved.ChannelID != 5 || resolved.ChannelName != "渠道A" || resolved.InternalModelID != "gpt-4o-int" {
		t.Fatalf("维度字段错: %+v", resolved)
	}
	if len(resolved.SnapshotNames) != 2 || resolved.SnapshotNames[0] != "gpt-4o" || resolved.SnapshotNames[1] != "gpt-4o-legacy" {
		t.Fatalf("曾用名快照错: %+v", resolved.SnapshotNames)
	}

	fallback := body.Data.Rows[1]
	if fallback.EntityKey != "name:ghost-model#ghost-int" {
		t.Fatalf("降级桶键错: %q", fallback.EntityKey)
	}
	if fallback.ChannelName != "(无渠道)" || fallback.ChannelModelID != 0 || fallback.ExternalModelID != 0 {
		t.Fatalf("降级桶维度错: %+v", fallback)
	}
	if fallback.ExternalModelName != "ghost-model" {
		t.Fatalf("降级桶展示名应取原始文本: %q", fallback.ExternalModelName)
	}

	// 金额逐位相等：合计 = 各行的精确和（这里刻意用 5 位小数的最小单位验证不丢精度）。
	if body.Data.Total.SaleCredits != 0.00005 {
		t.Fatalf("售价合计=%v，want 0.00005", body.Data.Total.SaleCredits)
	}
	if body.Data.Total.CostCredits != 0.00003 {
		t.Fatalf("成本合计=%v，want 0.00003", body.Data.Total.CostCredits)
	}
	if body.Data.Total.Requests != 4 {
		t.Fatalf("请求数=%d，want 4", body.Data.Total.Requests)
	}
	if body.Data.Total.InputTokens != 110 || body.Data.Total.CacheReadTokens != 20 || body.Data.Total.OutputTokens != 35 {
		t.Fatalf("token 合计错: %+v", body.Data.Total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未满足的期望: %v", err)
	}
}

// TestCostStatsQuery_ProbeExclusion 守护探测行剔除口径：SQL 必须同时带上
// 「cost 模式 + 系统探测身份 + 无 call_log」三个条件，外加 status='completed'。
func TestCostStatsQuery_ProbeExclusion(t *testing.T) {
	sql := costStatsQuerySQL
	for _, want := range []string{
		"b.pricing_mode = 'cost'",
		"u.is_system",
		"t.display_name = 'system-probe'",
		"NOT EXISTS (SELECT 1 FROM call_logs cl WHERE cl.billing_id = b.billing_id)",
		"b.status = 'completed'",
		"LEFT JOIN channel_keys k ON k.id = b.channel_key_id",
		"DISTINCT ON (cm.channel_id, cm.internal_model_id)",
	} {
		if !contains(sql, want) {
			t.Fatalf("成本统计 SQL 缺少关键条件：%s", want)
		}
	}
	// 降级桶与实体键表达式必须在。
	if !contains(sql, "'cm:' || r.cm_id::text") || !contains(sql, "'name:' || b.external_model_name") {
		t.Fatalf("实体键表达式缺失：%s", sql)
	}
}

func contains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && regexp.MustCompile(regexp.QuoteMeta(sub)).MatchString(s))
}

// TestHandleCostPricingCurve_NilDB 断言未装配 DB 时返回 200 + 两条不可用线（不报错、不返 null）。
func TestHandleCostPricingCurve_NilDB(t *testing.T) {
	h := NewAdmin(nil, nil, nil, nil)
	h.now = func() time.Time { return pricingTestAt(t) }
	rec := httptest.NewRecorder()
	h.HandleCostPricingCurve(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/pricing-curve", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code int                  `json:"Code"`
		Data pricingCurveResponse `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Data.Sale.Available || body.Data.Cost.Available {
		t.Fatalf("无 DB 时两条线都应不可用: %+v", body.Data)
	}
	if body.Data.Sale.Points == nil || body.Data.Cost.Points == nil || body.Data.TimeOptions == nil {
		t.Fatalf("集合字段必须是空数组不是 null: %+v", body.Data)
	}
	raw := rec.Body.String()
	if contains(raw, `"Sale":null`) || contains(raw, `"Cost":null`) {
		t.Fatalf("Sale/Cost 不能是 null: %s", raw)
	}
}

// TestHandleCostPricingCurve_BadParam 断言非法参数返回 400。
func TestHandleCostPricingCurve_BadParam(t *testing.T) {
	h := NewAdmin(nil, nil, nil, nil)
	for _, q := range []string{"?Step=abc", "?MaxTokens=x", "?HitRate=NaN", "?At=昨天", "?ChannelModelID=abc"} {
		rec := httptest.NewRecorder()
		h.HandleCostPricingCurve(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/cost/pricing-curve"+q, nil))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query=%s status=%d，want 400 body=%s", q, rec.Code, rec.Body.String())
		}
	}
}

// TestHandleCostPricingCurve_WithChannelModel 断言完整链路：全局时段/分档 → 渠道内部模型 → 反查对外模型，
// 两条线都出点，拐点来自分档，时段系数按 At 命中高峰。
func TestHandleCostPricingCurve_WithChannelModel(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// 1) 全局 time_config
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key = $1`)).
		WithArgs("billing.time_config").
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`{"Timezone":"Asia/Shanghai","Default":1.0,"Periodic":[
			{"Name":"低谷","Start":"00:00","End":"08:00","Coeff":0.5},
			{"Name":"高峰","Start":"18:00","End":"22:00","Coeff":1.5}]}`)))
	// 2) 全局 context_tiers
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key = $1`)).
		WithArgs("billing.context_tiers").
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte(`[{"Min":0,"Max":8000,"Coeff":1.0},{"Min":8001,"Max":null,"Coeff":1.5}]`)))
	// 3) 成本线模型
	mock.ExpectQuery(regexp.QuoteMeta(`FROM channel_models cm`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name", "internal_model_id", "cost_rates", "time_config", "context_tiers"}).
			AddRow(int64(900), int64(5), "渠道A", "gpt-4o-int",
				[]byte(`{"input":1,"output":4,"cache_read":0.1}`), nil, nil))
	// 4) 反查对外模型（售价线）
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT cm.external_model_id, em.external_name`)).
		WillReturnRows(sqlmock.NewRows([]string{"external_model_id", "external_name", "sale_rates", "time_config", "context_tiers"}).
			AddRow(int64(31), "gpt-4o", []byte(`{"input":1.5,"output":6,"cache_read":0.15}`), nil, nil))

	h := NewAdmin(db, nil, nil, nil)
	h.now = func() time.Time { return pricingTestAt(t) }
	rec := httptest.NewRecorder()
	h.HandleCostPricingCurve(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/cost/pricing-curve?ChannelModelID=900&Step=1000&MaxTokens=40000&HitRate=0&InOutRatio=10&At=19:30", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code int                  `json:"Code"`
		Data pricingCurveResponse `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	d := body.Data
	if !d.Sale.Available || !d.Cost.Available {
		t.Fatalf("两条线都应可用: sale=%+v cost=%+v", d.Sale, d.Cost)
	}
	if d.R != costDefaultR {
		t.Fatalf("R=%d，want %d（未注入 rProvider 时回退默认）", d.R, costDefaultR)
	}
	if d.Sale.TimeCoeff != 1.5 || d.Cost.TimeCoeff != 1.5 {
		t.Fatalf("19:30 应命中高峰 1.5：sale=%v cost=%v", d.Sale.TimeCoeff, d.Cost.TimeCoeff)
	}
	if len(d.Sale.Points) != 40 || d.PointCount != 40 || d.MaxTokens != 40000 {
		t.Fatalf("采样参数错: points=%d count=%d max=%d", len(d.Sale.Points), d.PointCount, d.MaxTokens)
	}
	if len(d.Sale.TierBreaks) != 1 || d.Sale.TierBreaks[0].Tokens != 8001 ||
		d.Sale.TierBreaks[0].FromCoeff != 1.0 || d.Sale.TierBreaks[0].ToCoeff != 1.5 {
		t.Fatalf("拐点错: %+v", d.Sale.TierBreaks)
	}
	if len(d.Sale.OverTier) != 0 {
		t.Fatalf("最后一档无上限时不应有超档: %+v", d.Sale.OverTier)
	}
	if d.Sale.ExternalModelID != 31 || d.Sale.Name != "gpt-4o" {
		t.Fatalf("售价线维度错: %+v", d.Sale)
	}
	if d.Cost.ChannelModelID != 900 || d.Cost.ChannelID != 5 || d.Cost.ChannelName != "渠道A" {
		t.Fatalf("成本线维度错: %+v", d.Cost)
	}
	// 售价必须高于成本（同一 token 下），且含系数线 > 不含系数线（系数 1.5×1.0 或 1.5×1.5）。
	for i := range d.Sale.Points {
		if d.Sale.Points[i].Credits <= d.Cost.Points[i].Credits {
			t.Fatalf("第 %d 点售价应高于成本: sale=%v cost=%v", i,
				d.Sale.Points[i].Credits, d.Cost.Points[i].Credits)
		}
		if d.Sale.Points[i].Credits <= d.Sale.Points[i].CreditsNoCoeff {
			t.Fatalf("第 %d 点含系数金额应大于不含系数金额: %+v", i, d.Sale.Points[i])
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未满足的期望: %v", err)
	}
}
