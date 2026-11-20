package v3billing

// dryrun_test.go：新结算链路部署前的**只读**试算（kill-switch 决策依据，2026-11-20 派工）。
//
// 目的：真机切换 V3_SETTLE_ENABLED 之前，对存量未结算账本行跑一遍**与主链完全同源**的
// 归属 + 定价解析（loadGroupNodes / priceCandidates / resolvePrice，一字不差），分三桶
// 报告行数/占比/金额，回答「新链路开了以后这些账会怎么结、有多少结不出去」。
//
//   - 桶1 route_id 直接命中：route_id>0 且绑定候选（bound_internal_ref）命中售价；
//   - 桶2 回退链命中：其余全部定价形态（名字候选 / route_id=0 的绑定候选 / 组快照桥）；
//   - 桶3 unpriced 兜底：候选链全落空（开了新链路也结不出去，落 unpriced 账单等定价）；
//   - 桶0 无归属（单列）：caller_key_name 解析不到 ACTIVE 令牌——哪条链路都结不了，
//     需要先修数据（这种行在旧链路 = 永久重试占用批次，在新链路 = 每轮 warn + 留行）。
//
// 金额口径（估算，非账单值）：
//   - 营收估算 = Σ billing.ComputeCredits(四段三键 usage, saleRates, timeCoeff, ctxCoeff, R)，
//     R/时段/分档照抄主链 requestCredits 的读法（sys_configs 三键 + 模型级 time/tiers 覆盖）；
//   - 成本 = Σ 账本行 cost_credits（magpie 侧真实值，非估算）。
//
// 纪律：
//   - **只读**：本文件不得出现任何 INSERT/UPDATE/DELETE/DDL（源码级守卫用例钉死）；
//   - 目标库必须显式给 `V3_DRYRUN_DSN`（专用变量，防止误连测试库拿假数据做决策）；
//     不给就 SKIP——空跑没有意义，也不许拿它冒充「已验证」；
//   - 跑法（SSH 隧道后）：`V3_DRYRUN_DSN='postgres://…' go test ./internal/domain/v3billing
//     -run TestV3SettleDryRun -v -count=1`；结果同时落 notes/（人工粘贴）。
//
// 变异防线：把 dryrunClassify 里 resolvePrice 换成恒 nil（模拟「解析永远失败」）→
// 桶1/桶2 归零、桶3 = 全量，用例红。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
)

// dryBucket 一行账本行的分桶结果。
type dryBucket string

const (
	dryNoToken   dryBucket = "no-token"   // callerKey 解析不到令牌（桶0，单列）
	dryRouteHit  dryBucket = "route-hit"  // 桶1：route_id>0 且绑定候选命中
	dryFallback  dryBucket = "fallback"   // 桶2：其余定价形态命中
	dryUnpriced  dryBucket = "unpriced"   // 桶3：候选链全落空
	dryQueryFail dryBucket = "query-fail" // 解析过程库故障（不应出现，出现即环境问题）
)

// dryReport 分桶汇总。
type dryReport struct {
	Buckets map[dryBucket]*dryStats
	Total   int
}

type dryStats struct {
	Rows        int64
	TokensIn    int64 // 账本口径（含缓存读+写）
	TokensOut   int64
	CacheRead   int64
	CacheWrite  int64
	CostCredits float64 // 账本成本（magpie 真实值）
	EstRevenue  float64 // 营收估算（ComputeCredits 口径）
}

// dryrunClassify 对一行账本数据做归属+定价分桶（纯读路径，复用主链实现）。
// r/price 预载参数只为可测性拆分；生产路径上两者都从 DB 读。
func dryrunClassify(ctx context.Context, p *Poller, r row, groups []groupNode, params *dryParams) (dryBucket, float64) {
	if r.callerKey == "" {
		return dryNoToken, 0
	}
	var tokenID, userID int64
	err := p.db.QueryRowContext(ctx, `
		SELECT k.token_id, t.user_id FROM magpie_keys k JOIN tokens t ON t.id = k.token_id
		 WHERE k.key_name = $1 AND k.state = 'ACTIVE' LIMIT 1`, r.callerKey).Scan(&tokenID, &userID)
	if err != nil {
		return dryNoToken, 0
	}
	price, _, err := p.resolvePrice(ctx, r, groups)
	if err != nil {
		return dryQueryFail, 0
	}
	if price == nil {
		return dryUnpriced, 0
	}
	bucket := dryFallback
	if r.routeID > 0 && price.byBinding {
		bucket = dryRouteHit
	}
	if params == nil {
		return bucket, 0
	}
	return bucket, params.estimateRevenue(r, price)
}

// dryParams 计费参数（R / 全局时段 / 全局分档），照抄 requestCredits 的读法。
type dryParams struct {
	R             int64
	GlobalTime    billing.TimeCoeffConfig
	GlobalTiers   []billing.TierRule
	GlobalTimeSet bool // 全局时段键是否存在（不存在 → 系数 1，等价主链空配置）
}

// estimateRevenue 复刻主链 requestCredits 的金额计算（模型级 time/tiers 覆盖全局）。
func (dp *dryParams) estimateRevenue(r row, price *priceInfo) float64 {
	usage := billing.Usage{Input: r.tokensIn - r.cacheRead, Output: r.tokensOut, CacheRead: r.cacheRead}
	if usage.Input < 0 {
		usage.Input = 0
	}
	timeCfg := dp.GlobalTime
	if len(price.timeRaw) > 0 && string(price.timeRaw) != "null" {
		var tc billing.TimeCoeffConfig
		if json.Unmarshal(price.timeRaw, &tc) == nil {
			timeCfg = tc
		}
	}
	tiers := dp.GlobalTiers
	if len(price.tiersRaw) > 0 && string(price.tiersRaw) != "null" {
		var ts []billing.TierRule
		if json.Unmarshal(price.tiersRaw, &ts) == nil && len(ts) > 0 {
			tiers = ts
		}
	}
	timeCoeff, err := timeCfg.CoeffFor(time.Now())
	if err != nil {
		timeCoeff = 1
	}
	ctxCoeff, err := billing.ContextTierCoeff(usage.Input+usage.CacheRead, tiers)
	if err != nil {
		ctxCoeff = 1
	}
	credits, err := billing.ComputeCredits(usage, billing.Rates(price.saleRates), timeCoeff, ctxCoeff, dp.R)
	if err != nil {
		return 0
	}
	return credits
}

// dryLoadParams 读 sys_configs 三键（billing.r / billing.time_config / billing.context_tiers），
// 解析口径照抄 billing 包（R 兼容数字与字符串 JSON；空时段/分档 → 系数 1）。
func dryLoadParams(db *sql.DB) (*dryParams, error) {
	dp := &dryParams{R: 10000}
	get := func(key string) (string, error) {
		var v sql.NullString
		err := db.QueryRow(`SELECT value FROM sys_configs WHERE key = $1`, key).Scan(&v)
		if err == sql.ErrNoRows {
			return "", nil
		}
		return v.String, err
	}
	rawR, err := get("billing.r")
	if err != nil {
		return nil, err
	}
	if rawR != "" {
		var asInt int64
		if json.Unmarshal([]byte(rawR), &asInt) == nil && asInt > 0 {
			dp.R = asInt
		} else {
			var asStr string
			if json.Unmarshal([]byte(rawR), &asStr) == nil {
				var r int64
				if _, err := fmt.Sscanf(asStr, "%d", &r); err == nil && r > 0 {
					dp.R = r
				}
			}
		}
	}
	rawTime, err := get("billing.time_config")
	if err != nil {
		return nil, err
	}
	if rawTime != "" {
		var tc billing.TimeCoeffConfig
		if json.Unmarshal([]byte(rawTime), &tc) == nil {
			dp.GlobalTime = tc
			dp.GlobalTimeSet = true
		}
	}
	rawTiers, err := get("billing.context_tiers")
	if err != nil {
		return nil, err
	}
	if rawTiers != "" {
		var ts []billing.TierRule
		if json.Unmarshal([]byte(rawTiers), &ts) == nil {
			dp.GlobalTiers = ts
		}
	}
	return dp, nil
}

// TestV3SettleDryRun_SourceGuard 源码级守卫：本文件必须只读。
// 写动作（哪怕「只是看看」的 UPDATE）会污染生产库——dry-run 的全部价值就在零写。
func TestV3SettleDryRun_SourceGuard(t *testing.T) {
	body, err := os.ReadFile("dryrun_test.go")
	if err != nil {
		t.Fatalf("读取自身失败：%v", err)
	}
	s := string(body)
	for _, word := range []string{"INSERT", "UPDATE", "DELETE", "ALTER", "DROP"} {
		if containsSQLKeyword(s, word) {
			t.Fatalf("dry-run 文件出现写/DDL 关键字 %s（守卫串自身除外）", word)
		}
	}
}

// containsSQLKeyword 判定源码里是否出现「作为 SQL 语句」的关键字（注释/守卫串里讨论关键字
// 不算）。口径：行首到该词之间没有 `//` 或 `"` 的行才算代码行。
func containsSQLKeyword(src, word string) bool {
	for _, line := range splitLines(src) {
		for i := 0; i+len(word) <= len(line); i++ {
			if line[i:i+len(word)] == word {
				prefix := line[:i]
				if !containsAny(prefix, "//", "\"") {
					return true
				}
			}
		}
	}
	return false
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}

func containsAny(s, a, b string) bool {
	for i := 0; i < len(s); i++ {
		if i+len(a) <= len(s) && s[i:i+len(a)] == a {
			return true
		}
		if i+len(b) <= len(s) && s[i:i+len(b)] == b {
			return true
		}
	}
	return false
}

// TestV3SettleDryRun 真库只读试算（DSN 门控）。
// 本地验证只读性可用 lingshu_test（空表 → 全 0 行报告）；真机数据等 SSH 隧道后跑。
func TestV3SettleDryRun(t *testing.T) {
	dsn := os.Getenv("V3_DRYRUN_DSN")
	if dsn == "" {
		t.Skip("未设置 V3_DRYRUN_DSN（只读试算需要显式指定目标库；空跑没有意义）")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("连接失败：%v", err)
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		t.Fatalf("目标库不可达：%v", err)
	}

	ctx := context.Background()
	params, err := dryLoadParams(db)
	if err != nil {
		t.Fatalf("读计费参数失败：%v", err)
	}
	p := NewPoller(db, nil, nil, nil)
	groups, err := p.loadGroupNodes(ctx)
	if err != nil {
		t.Fatalf("读组快照失败：%v", err)
	}

	// route_id 列容错（0029 才加列）：旧链库（如 rc4 未迁移）没有这列——
	// 不存在时按 route_id=0 试算（桶1 恒空属**口径限制**，不是解析失败；
	// 桶1 的意义在迁移后新流量上，存量行本来就没有路由信息）。
	var hasRouteID bool
	if err := db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		 WHERE table_name = 'ledger_entries' AND column_name = 'route_id')`).Scan(&hasRouteID); err != nil {
		t.Fatalf("探测 route_id 列失败：%v", err)
	}
	routeCol := ", e.route_id"
	if !hasRouteID {
		routeCol = ", 0::bigint AS route_id"
	}
	t.Logf("route_id 列：%v", hasRouteID)

	// 未结算行（与 Once 同谓词，但不设 LIMIT：试算要看全量积压）。
	rows, err := db.QueryContext(ctx, `
		SELECT e.ledger_id, e.occurred_at, e.provider, e.model, e.req_model, e.caller_key_name`+routeCol+`,
		       e.tokens_in, e.tokens_out, e.cache_read, e.cache_write, e.cost_credits
		  FROM ledger_entries e
		 WHERE NOT EXISTS (SELECT 1 FROM billing_records b WHERE b.magpie_request_id = e.ledger_id)
		 ORDER BY e.occurred_at ASC`)
	if err != nil {
		t.Fatalf("读账本失败：%v", err)
	}
	defer rows.Close()

	rep := &dryReport{Buckets: map[dryBucket]*dryStats{
		dryNoToken: {}, dryRouteHit: {}, dryFallback: {}, dryUnpriced: {}, dryQueryFail: {},
	}}
	unpricedNames := map[string]int64{}
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ledgerID, &r.occurredAt, &r.provider, &r.model, &r.reqModel, &r.callerKey,
			&r.routeID, &r.tokensIn, &r.tokensOut, &r.cacheRead, &r.cacheWrite, &r.costCredits); err != nil {
			t.Fatalf("扫描失败：%v", err)
		}
		rep.Total++
		bucket, estRev := dryrunClassify(ctx, p, r, groups, params)
		st := rep.Buckets[bucket]
		st.Rows++
		st.TokensIn += r.tokensIn
		st.TokensOut += r.tokensOut
		st.CacheRead += r.cacheRead
		st.CacheWrite += r.cacheWrite
		st.CostCredits += r.costCredits
		st.EstRevenue += estRev
		if bucket == dryUnpriced {
			unpricedNames[unpricedName(r)]++
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("遍历失败：%v", err)
	}

	// —— 报告（t.Logf 进测试输出，人工粘贴进 notes/）——
	t.Logf("== V3 结算 dry-run（只读试算）==")
	t.Logf("计费参数：R=%d 全局时段=%v 全局分档规则数=%d", params.R, params.GlobalTimeSet, len(params.GlobalTiers))
	t.Logf("未结算账本行总计：%d", rep.Total)
	pct := func(n int64) float64 {
		if rep.Total == 0 {
			return 0
		}
		return float64(n) * 100 / float64(rep.Total)
	}
	order := []dryBucket{dryNoToken, dryRouteHit, dryFallback, dryUnpriced, dryQueryFail}
	names := map[dryBucket]string{
		dryNoToken: "桶0 无归属（先修数据）", dryRouteHit: "桶1 route_id 直接命中",
		dryFallback: "桶2 回退链命中", dryUnpriced: "桶3 unpriced 兜底（定价后可补收）",
		dryQueryFail: "解析故障（环境问题，应为 0）",
	}
	for _, b := range order {
		st := rep.Buckets[b]
		t.Logf("%-28s 行数=%d (%.1f%%)  tokens(in/out/cr/cw)=%d/%d/%d/%d  账本成本=%.5f  营收估算=%.5f",
			names[b], st.Rows, pct(st.Rows), st.TokensIn, st.TokensOut, st.CacheRead, st.CacheWrite,
			st.CostCredits, st.EstRevenue)
	}
	if len(unpricedNames) > 0 {
		type kv struct {
			name string
			n    int64
		}
		var list []kv
		for n, c := range unpricedNames {
			list = append(list, kv{n, c})
		}
		sort.Slice(list, func(i, j int) bool { return list[i].n > list[j].n })
		t.Logf("桶3 未定价名字 TOP10（定价动作清单）：")
		for i, e := range list {
			if i >= 10 {
				break
			}
			t.Logf("  %4d  %s", e.n, e.name)
		}
	}
	if rep.Buckets[dryQueryFail].Rows > 0 {
		t.Errorf("存在解析故障行（环境问题）：dry-run 结果不可用作决策依据，先查库")
	}
}
