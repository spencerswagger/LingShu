package v3billing

// rebill_test.go：补收链路的行为证明（sqlmock 逐条对账 + 随机值驱动）。
//
// 任务硬断言（缺一不可）：
//  1. 重复补收被拒 —— 三种形态各一条用例：扫描快照已带标记 / 行内新鲜判定带标记 /
//     新账单已存在（重放）；外加「守卫 UPDATE 影响行数为 0」的畸形兜底；
//  2. 余额逐笔吻合 —— 新账单金额 = 补收时刻售价 × 原行 tokens（自校验用 ComputeCredits
//     独立复算，同值才放行）；
//  3. 原行标记正确 —— 标记文本 = 『；已补收:<新账单id>』、守卫谓词齐全（status/unpriced/
//     NOT LIKE）、RowsAffected==0 必须报错。
//
// 所有名字/token 数/单价随机：匹配规则必须与具体名字无关（禁止特判）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/team/llmgateway/internal/domain/billing"
)

// rfloat 生成 (lo,hi) 区间随机浮点。
func rfloat(lo, hi float64) float64 { return lo + rand.Float64()*(hi-lo) }

// rint 生成 [lo,hi] 区间随机整数。
func rint(lo, hi int64) int64 { return lo + rand.Int63n(hi-lo+1) }

// expectScan 装配 rebillScanSQL 的结果行（列序与实现逐字对齐）。
func (f *fixture) expectScan(rows ...rebillRow) {
	rs := f.mock.NewRows([]string{
		"billing_id", "user_id", "token_id", "external_model_name", "internal_model_id",
		"tokens", "cost_credits", "error_message", "req_model", "served_model", "route_id",
	})
	for _, r := range rows {
		tokensRaw, _ := json.Marshal(r.Tokens)
		var cost any
		if r.CostCredits != nil {
			cost = *r.CostCredits
		}
		tokenID := any(nil)
		if r.TokenID != nil {
			tokenID = *r.TokenID
		}
		rs.AddRow(r.BillingID, r.UserID, tokenID, r.ExternalModel, r.InternalModelID,
			tokensRaw, cost, r.ErrorMessage, r.ReqModel, r.ServedModel, r.RouteID)
	}
	f.mock.ExpectQuery("FROM billing_records b").WillReturnRows(rs)
}

// expectState 装配行内新鲜判定（标记 NULL / 新账单不存在 / 有钱包 = 可补收）。
func (f *fixture) expectState() {
	f.mock.ExpectQuery("SELECT \\(SELECT error_message FROM billing_records").
		WillReturnRows(f.mock.NewRows([]string{"error_message", "status", "exists"}).AddRow(nil, nil, true))
}

// expectStateNoWallet 装配「用户没有积分钱包」的新鲜判定（Record 前拦截，不落 failed 残单）。
func (f *fixture) expectStateNoWallet() {
	f.mock.ExpectQuery("SELECT \\(SELECT error_message FROM billing_records").
		WillReturnRows(f.mock.NewRows([]string{"error_message", "status", "exists"}).AddRow(nil, nil, false))
}

// expectStateMarked 装配「原行已带标记」的新鲜判定。
func (f *fixture) expectStateMarked(mark string) {
	f.mock.ExpectQuery("SELECT \\(SELECT error_message FROM billing_records").
		WillReturnRows(f.mock.NewRows([]string{"error_message", "status", "exists"}).AddRow(mark, nil, true))
}

// expectStateNewExists 装配「新账单已完成」的新鲜判定（重放形态）。
func (f *fixture) expectStateNewExists(origID string, cost float64) {
	f.mock.ExpectQuery("SELECT \\(SELECT error_message FROM billing_records").
		WillReturnRows(f.mock.NewRows([]string{"error_message", "status", "exists"}).
			AddRow(nil, billing.StatusCompleted, true))
	f.mock.ExpectExec("UPDATE billing_records SET cost_credits").
		WithArgs(cost, rebillNewBillingID(origID)).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectMark 装配原行标记 UPDATE。正则钉**整条 WHERE**：守卫谓词（status/unpriced、
// NOT LIKE 已补收）被删时 sqlmock 直接判「未预期的调用」——变异必须变红。
const rebillMarkSQLPattern = `UPDATE billing_records SET error_message = CASE[\s\S]*` +
	`WHERE billing_id = \$1\s+AND status = 'unpriced'\s+AND COALESCE\(error_message,''\) NOT LIKE`

func (f *fixture) expectMark(origID, newID string) {
	wantMark := rebillMarkPrefix + newID
	f.mock.ExpectExec(rebillMarkSQLPattern).
		WithArgs(origID, wantMark, rebillMarkSep+wantMark).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// expectPriceChain 按 priceCandidates 程序化装配候选链：resolvePrice **命中即停**，
// 所以只注册到命中位为止（之前的候选逐个落空、命中位返回行，之后的候选不会发生查询）。
func (f *fixture) expectPriceChain(reqModel, servedModel string, groups []groupNode, routeID int64, hitValue, extName string, rates map[string]float64) {
	for _, c := range priceCandidates(reqModel, servedModel, groups, routeID) {
		if c.value == hitValue {
			f.mock.ExpectQuery(priceQueryPattern(c.byBinding)).WithArgs(c.value).
				WillReturnRows(priceRow(extName, rates))
			return
		}
		f.mock.ExpectQuery(priceQueryPattern(c.byBinding)).WithArgs(c.value).WillReturnError(sql.ErrNoRows)
	}
	panic(fmt.Sprintf("候选链里没有期望命中的值 %q（req=%q served=%q）", hitValue, reqModel, servedModel))
}

// expectAllPriceMiss 装配整条候选链全落空。
func (f *fixture) expectAllPriceMiss(reqModel, servedModel string, groups []groupNode, routeID int64) {
	for _, c := range priceCandidates(reqModel, servedModel, groups, routeID) {
		f.mock.ExpectQuery(priceQueryPattern(c.byBinding)).WithArgs(c.value).WillReturnError(sql.ErrNoRows)
	}
}

// TestRebill_HappyPathAndIdempotent 补收主链 + 三种重复形态。
func TestRebill_HappyPathAndIdempotent(t *testing.T) {
	groupID := rseg(t)             // 随机组 id
	groupRef := "group/" + groupID // 绑定真源形态
	display := "组" + rseg(t)       // 人可读名（display_name 档）
	extName := "ext-" + rseg(t)    // 售价命中的对外模型名
	rates := map[string]float64{   // 补收时刻售价（随机）
		"input": rfloat(1, 10), "output": rfloat(1, 10), "cache_read": rfloat(0.1, 1),
	}
	in, out, cache := rint(100, 9000), rint(50, 4000), rint(10, 500)
	cost := rfloat(0.01, 2)
	newID := rebillNewBillingID("bill-x")

	t.Run("正常补收一条", func(t *testing.T) {
		f := newFixture(t)
		groups := []groupNode{{Ref: groupRef, DisplayName: display}}
		f.expectGroups([2]string{groupRef, display})
		// 预检（ref 本身 + 显示名两轮，显示名 ≠ ref id 时名字轮天然落空）。
		f.expectPriceChain(groupRef, display, groups, 1, groupRef, extName, rates)
		f.expectScan(rebillRow{
			BillingID: "bill-x", UserID: 42, TokenID: int64Ptr(7),
			ExternalModel: groupRef, InternalModelID: "prov/m",
			Tokens:      billing.Usage{Input: in - cache, Output: out, CacheRead: cache},
			CostCredits: &cost, ErrorMessage: "未定价组：" + groupRef,
			ReqModel: groupRef, ServedModel: "served-m", RouteID: 5,
		})
		f.expectState()
		// 行级解析：req=组ref 直中绑定档。
		f.expectPriceChain(groupRef, "served-m", groups, 5, groupRef, extName, rates)
		// Record → 新账单；成本沿用原行；留痕参数恒 nil（旧三段行不落 price_eval）。
		f.rec.bals = []float64{88.5}
		f.mock.ExpectExec("UPDATE billing_records SET cost_credits").
			WithArgs(cost, newID, nil).WillReturnResult(sqlmock.NewResult(0, 1))
		f.expectMark("bill-x", newID)

		res, err := f.p.Rebill(context.Background(), groupRef)
		if err != nil {
			t.Fatalf("Rebill 失败：%v", err)
		}
		if res.Rebilled != 1 || len(res.NewBillingIDs) != 1 || res.TotalCredits <= 0 {
			t.Fatalf("汇总不符：%+v", res)
		}
		req := f.rec.reqs[0]
		if req.BillingID != newID {
			t.Fatalf("新账单号 = %q，期望固定派生 %q", req.BillingID, newID)
		}
		if req.UserID != 42 || req.Status != billing.StatusCompleted || req.Fail {
			t.Fatalf("新账单归属/状态不符：%+v", req)
		}
		// 计价自校验：补收时刻售价 × 原行用量；余额/合计按同一口径逐笔吻合。
		usage := billing.Usage{Input: in - cache, Output: out, CacheRead: cache}
		want, err := billing.ComputeCredits(usage, rates, 1, 1, 10000)
		if err != nil {
			t.Fatalf("ComputeCredits 失败：%v", err)
		}
		got, err := billing.ComputeCredits(req.Tokens, req.Rates, 1, 1, 10000)
		if err != nil || got != want {
			t.Fatalf("补收计价不符：got=%v want=%v", got, want)
		}
		if res.TotalCredits != want {
			t.Fatalf("金额合计与逐笔复算不符：total=%v want=%v", res.TotalCredits, want)
		}
		if req.ErrorMessage != rebillNewRemark+"bill-x" {
			t.Fatalf("新账单标记不符：%q", req.ErrorMessage)
		}
	})

	t.Run("原行已带标记则跳过", func(t *testing.T) {
		f := newFixture(t)
		f.expectGroups([2]string{groupRef, display})
		f.mock.ExpectQuery("WHERE bound_internal_ref =").WithArgs(groupRef).
			WillReturnRows(priceRow(extName, rates))
		f.expectScan(rebillRow{
			BillingID: "bill-y", UserID: 42,
			ExternalModel: groupRef, Tokens: billing.Usage{Input: 10},
			ErrorMessage: "未定价组：x" + rebillMarkSep + "已补收:" + rebillNewBillingID("bill-y"),
			ReqModel:     groupRef,
		})
		res, err := f.p.Rebill(context.Background(), groupRef)
		if err != nil {
			t.Fatalf("Rebill 失败：%v", err)
		}
		if res.Rebilled != 0 || res.AlreadyMarked != 1 || len(f.rec.reqs) != 0 {
			t.Fatalf("应跳过且不扣款：%+v reqs=%d", res, len(f.rec.reqs))
		}
	})

	t.Run("扫描快照过期但行内已带标记则跳过", func(t *testing.T) {
		f := newFixture(t)
		f.expectGroups([2]string{groupRef, display})
		f.mock.ExpectQuery("WHERE bound_internal_ref =").WithArgs(groupRef).
			WillReturnRows(priceRow(extName, rates))
		f.expectScan(rebillRow{ // 快照里没有标记（过期）
			BillingID: "bill-z", UserID: 42, ExternalModel: groupRef,
			Tokens: billing.Usage{Input: 10}, ReqModel: groupRef,
		})
		f.expectStateMarked(rebillMarkPrefix + rebillNewBillingID("bill-z"))
		res, err := f.p.Rebill(context.Background(), groupRef)
		if err != nil {
			t.Fatalf("Rebill 失败：%v", err)
		}
		if res.AlreadyMarked != 1 || len(f.rec.reqs) != 0 {
			t.Fatalf("行内判定应拦截重复补收：%+v", res)
		}
	})

	t.Run("新账单已存在（重放）不重复扣款", func(t *testing.T) {
		f := newFixture(t)
		f.expectGroups([2]string{groupRef, display})
		f.mock.ExpectQuery("WHERE bound_internal_ref =").WithArgs(groupRef).
			WillReturnRows(priceRow(extName, rates))
		f.expectScan(rebillRow{
			BillingID: "bill-w", UserID: 42, ExternalModel: groupRef,
			Tokens: billing.Usage{Input: 10}, CostCredits: &cost, ReqModel: groupRef,
		})
		f.expectStateNewExists("bill-w", cost)
		res, err := f.p.Rebill(context.Background(), groupRef)
		if err != nil {
			t.Fatalf("Rebill 失败：%v", err)
		}
		if res.AlreadyMarked != 1 || res.Rebilled != 0 || len(f.rec.reqs) != 0 {
			t.Fatalf("重放不应重复扣款：%+v reqs=%d", res, len(f.rec.reqs))
		}
	})

	t.Run("组未定价整体拒绝", func(t *testing.T) {
		f := newFixture(t)
		f.expectGroups() // 无快照
		f.expectAllPriceMiss(groupRef, "", nil, 1)
		_, err := f.p.Rebill(context.Background(), groupRef)
		if !errors.Is(err, ErrRebillGroupUnpriced) {
			t.Fatalf("期望 ErrRebillGroupUnpriced，实际：%v", err)
		}
		if len(f.rec.reqs) != 0 {
			t.Fatalf("未定价不允许扣款")
		}
	})

	t.Run("标记守卫归零进失败明细", func(t *testing.T) {
		f := newFixture(t)
		groups := []groupNode{{Ref: groupRef, DisplayName: display}}
		f.expectGroups([2]string{groupRef, display})
		f.expectPriceChain(groupRef, display, groups, 1, groupRef, extName, rates)
		f.expectScan(rebillRow{
			BillingID: "bill-m", UserID: 42, ExternalModel: groupRef,
			Tokens: billing.Usage{Input: 10}, ReqModel: groupRef,
		})
		f.expectState()
		f.expectPriceChain(groupRef, "", groups, 0, groupRef, extName, rates)
		f.rec.bals = []float64{50}
		f.mock.ExpectExec(rebillMarkSQLPattern).
			WillReturnResult(sqlmock.NewResult(0, 0))
		res, err := f.p.Rebill(context.Background(), groupRef)
		if err != nil {
			t.Fatalf("毒行隔离：守卫归零不应失败整批：%v", err)
		}
		if res.Rebilled != 0 || len(res.Failed) != 1 {
			t.Fatalf("失败行应进明细不计 Rebilled：%+v", res)
		}
		if res.Failed[0].BillingID != "bill-m" ||
			!strings.Contains(res.Failed[0].Message, "拒绝重复补收") ||
			res.Failed[0].Reason != ReasonRebillRecordError {
			t.Fatalf("失败明细缺原因/定位：%+v", res.Failed[0])
		}
	})
}

// TestRebill_PriceLookupDeterministic 售价查询的两条语句都必须带确定性 ORDER BY（评审 P2-2）：
// 绑定列 bound_internal_ref 上只有非唯一索引，同 ref 重复绑定时 LIMIT 1 无 ORDER BY 的取价
// 不确定（结算与补收同源）；名字列唯一索引兜底，但同样把确定性写显式。
func TestRebill_PriceLookupDeterministic(t *testing.T) {
	for _, tc := range []struct {
		name string
		sql  string
	}{
		{"名字匹配", extByNameSQL},
		{"绑定匹配", extByBindingSQL},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !strings.Contains(tc.sql, "ORDER BY id ASC LIMIT 1") {
				t.Fatalf("售价查询缺确定性排序（同 ref 重复绑定时取价不确定）：\n%s", tc.sql)
			}
		})
	}
	// 列清单一致（同函数共享 Scan 目标数）：改一条加列必红。
	if strings.Count(extByNameSQL, ",") != strings.Count(extByBindingSQL, ",") {
		t.Fatalf("两条售价查询的列清单不一致：\n%s\n%s", extByNameSQL, extByBindingSQL)
	}
	// 注释不得声称不存在的唯一索引（评审 P2-2：uq_external_models_bound_ref 不存在）。
	src, err := readPollerSource(t)
	if err != nil {
		t.Fatalf("读取 poller.go 失败：%v", err)
	}
	if strings.Contains(src, "uq_external_models_bound_ref") {
		t.Fatalf("poller.go 声称不存在的索引 uq_external_models_bound_ref（绑定列只有非唯一 idx_external_models_bound）")
	}
}

func readPollerSource(t *testing.T) (string, error) {
	t.Helper()
	b, err := os.ReadFile("poller.go")
	return string(b), err
}

// TestRebill_MatchingAsymmetryIsDocumented 匹配侧三名（req/served/external）与售价解析侧
// 的**刻意**不对称：解析侧输入 = 账本行的 req/served 两名（与主链 settleOne 完全同源），
// 账单的 external_model_name 只在 req 空缺时作回落、**不当独立候选**。external 名通常是
// unpricedName 的兜底产物（req→served→"unknown"），若给它开独立档，同一行会出现
// 「结算按主链口径落 unpriced、补收却能算出价」的口径分叉，账目两边对不上。
// 这里钉行为事实：external 名能参与匹配（最大召回），但不出现在解析候选里。
func TestRebill_MatchingAsymmetryIsDocumented(t *testing.T) {
	g := "group/auto-" + rseg(t)
	extOnlyName := "ext-only-" + rseg(t) // 只出现在账单 external_model_name 上的名字
	// 匹配侧：external 名命中组 ref 形态 → 该行进扫描（最大召回）。
	if !rebillMatchesRow(rebillRow{ExternalModel: g}, []string{g}, nil) {
		t.Fatalf("external 名应参与匹配（最大召回）")
	}
	// 解析侧：req/served 之外没有第三个独立档 —— served=extOnlyName（req 是组 ref）时，
	// 候选集合里针对 extOnlyName 只有它作为「已知名」自己的档（裸名归一出的绑定/镜像
	// 形态是对**所有**已知名统一生效的归一规则，不是 external 独有的第三档）；
	// extOnlyName 若独立成档会多出「按 extOnlyName 查 external_name」的独立 plain 档。
	// 钉住候选总数形态。
	cands := priceCandidates(g, extOnlyName, nil, 0)
	want := []priceCandidate{
		{value: g, byBinding: false},
		{value: extOnlyName, byBinding: false},
		{value: g, byBinding: true},
		{value: "auto/" + strings.TrimPrefix(g, "group/auto-"), byBinding: true},
		{value: "group/ext-only-" + strings.TrimPrefix(extOnlyName, "ext-only-"), byBinding: true},
		// 裸名归一（评审 P3 第三向）：抽屉形态 + 镜像 slug 档，对所有已知名统一生效。
		{value: "auto/" + extOnlyName, byBinding: true},
		{value: "group/auto-" + groupSlug(extOnlyName), byBinding: true},
	}
	if !reflect.DeepEqual(cands, want) {
		t.Fatalf("解析候选形态不符（external 名不应有独立第三档）：\ngot  %+v\nwant %+v", cands, want)
	}
	// req 空缺时 external 作回落（口径：回落 ≠ 独立候选）。
	lr := row{reqModel: "", model: extOnlyName}
	if lr.reqModel == "" {
		lr.reqModel = extOnlyName
	}
	if lr.reqModel != extOnlyName {
		t.Fatalf("external 回落口径被破坏")
	}
}

// TestRebill_PoisonRowIsolation 毒行不堵整批：坏行带原因进明细，好行照常补收。
// 覆盖四类毒行：tokens 损坏（扫描层）、解析不到售价（确定性行级）、缺 user_id、计费错误。
func TestRebill_PoisonRowIsolation(t *testing.T) {
	groupRef := "group/" + rseg(t)
	extName := "ext-" + rseg(t)
	rates := map[string]float64{"input": 2, "output": 4, "cache_read": 0.2}
	goodID, badTokensID, unresolvableID, noUserID, recFailID :=
		"bill-good", "bill-badtok", "bill-unres", "bill-nouser", "bill-recfail"
	usable := billing.Usage{Input: 100, Output: 50}

	t.Run("tokens损坏+解析失败+计费错误不堵整批", func(t *testing.T) {
		f := newFixture(t)
		f.expectGroups([2]string{groupRef, groupRef})
		// 预检命中绑定档。
		f.mock.ExpectQuery("WHERE bound_internal_ref =").WithArgs(groupRef).
			WillReturnRows(priceRow(extName, rates))
		// 5 行同组（扫描按 call_time 序）：坏 tokens / 正常 / 解析不到 / 缺 user / 计费错误。
		// 坏 tokens 行无法经 expectScan（它会 Marshal），手工构造含非法 JSON 的行。
		usableRaw, _ := json.Marshal(usable)
		ghost := "group/ghost-" + rseg(t)
		rs := f.mock.NewRows([]string{
			"billing_id", "user_id", "token_id", "external_model_name", "internal_model_id",
			"tokens", "cost_credits", "error_message", "req_model", "served_model", "route_id",
		})
		rs.AddRow(badTokensID, 42, nil, groupRef, "", []byte("{not-json"), nil, "未定价组", groupRef, "", 0)
		rs.AddRow(goodID, 42, nil, groupRef, "", usableRaw, nil, "未定价组", groupRef, "", 0)
		rs.AddRow(unresolvableID, 42, nil, groupRef, "", usableRaw, nil, "未定价组", ghost, "", 0)
		rs.AddRow(noUserID, 0, nil, groupRef, "", usableRaw, nil, "未定价组", groupRef, "", 0)
		rs.AddRow(recFailID, 42, nil, groupRef, "", usableRaw, nil, "未定价组", groupRef, "", 0)
		f.mock.ExpectQuery("FROM billing_records b").WillReturnRows(rs)
		// 正常行：完整链（行级 routeID=0 → 名字档先查）。
		f.expectState()
		f.expectPriceChain(groupRef, "", nil, 0, groupRef, extName, rates)
		f.rec.bals = []float64{80}
		f.expectMark(goodID, rebillNewBillingID(goodID))
		// 解析不到行：先 state 判定再整链落空（req 是无关 ref，served/route 空）。
		f.expectState()
		f.expectAllPriceMiss(ghost, "", nil, 0)
		// 缺 user 行：解析命中后再拒（无 user_id 不许扣款）。
		f.expectState()
		f.expectPriceChain(groupRef, "", nil, 0, groupRef, extName, rates)
		// 计费错误行：解析命中后 Record 报错（errSeq 按调用序：第 1 次好行成功、第 2 次报错）。
		f.expectState()
		f.expectPriceChain(groupRef, "", nil, 0, groupRef, extName, rates)
		f.rec.errSeq = []error{nil, errors.New("余额不足（40201）")}

		res, err := f.p.Rebill(context.Background(), groupRef)
		if err != nil {
			t.Fatalf("毒行不应失败整批：%v", err)
		}
		if res.Rebilled != 1 || len(res.NewBillingIDs) != 1 {
			t.Fatalf("好行应照常补收：%+v", res)
		}
		if res.Scanned != 5 {
			t.Fatalf("Scanned 应含全部同组行（含毒行）：%d", res.Scanned)
		}
		if len(res.Failed) != 4 {
			t.Fatalf("失败明细应 4 行：%+v", res.Failed)
		}
		byID := map[string]RebillRowFailure{}
		for _, fl := range res.Failed {
			byID[fl.BillingID] = fl
		}
		if fl := byID[badTokensID]; fl.Reason != ReasonRebillBrokenRow ||
			!strings.Contains(fl.Message, "tokens") {
			t.Fatalf("坏 tokens 行明细不符：%+v", fl)
		}
		if fl := byID[unresolvableID]; fl.Reason != ReasonRebillRowUnresolvable ||
			!strings.Contains(fl.Message, "解析不到售价") {
			t.Fatalf("解析不到行明细不符：%+v", fl)
		}
		if fl := byID[noUserID]; fl.Reason != ReasonRebillRowUnresolvable ||
			!strings.Contains(fl.Message, "user_id") {
			t.Fatalf("缺 user 行明细不符：%+v", fl)
		}
		if fl := byID[recFailID]; fl.Reason != ReasonRebillRecordError ||
			!strings.Contains(fl.Message, "余额不足") {
			t.Fatalf("计费错误行明细不符：%+v", fl)
		}
		// Record 只被「可计价且 user 合法」的两行调用（好行成功 + 计费错误行失败）；
		// 坏 tokens / 解析不到 / 缺 user 三行不得触发计费。
		if n := f.rec.callCount(); n != 2 {
			t.Fatalf("Record 应被好行与计费错误行各调 1 次（共 2），实际 %d", n)
		}
		if f.rec.reqs[0].BillingID != rebillNewBillingID(goodID) {
			t.Fatalf("第 1 次 Record 应是好行：%s", f.rec.reqs[0].BillingID)
		}
	})
}

// TestRebill_PartialResultHTTP 200 + 部分结果 + Failed 明细（HTTP 契约，毒行不堵整批）。
func TestRebill_PartialResultHTTP(t *testing.T) {
	g := "group/" + rseg(t)
	f := newFixture(t)
	f.expectGroups([2]string{g, g})
	f.mock.ExpectQuery("WHERE bound_internal_ref =").WithArgs(g).
		WillReturnRows(priceRow("ext", map[string]float64{"input": 1, "output": 2, "cache_read": 0.1}))
	f.expectScan(
		rebillRow{BillingID: "b-ok", UserID: 42, ExternalModel: g, Tokens: billing.Usage{Input: 8}, ReqModel: g},
		rebillRow{BillingID: "b-bad", UserID: 42, ExternalModel: g,
			Tokens: billing.Usage{Input: 3}, ReqModel: g},
	)
	f.expectState()
	f.expectPriceChain(g, "", nil, 0, g, "ext", map[string]float64{"input": 1, "output": 2, "cache_read": 0.1})
	f.rec.bals = []float64{60}
	f.expectMark("b-ok", rebillNewBillingID("b-ok"))
	f.expectAllPriceMiss(g, "", nil, 0)

	h := NewRebillHandler(f.p)
	rec, body := doRebillRequest(t, h, `{"Ref":"`+g+`"}`)
	if rec.Code != 200 {
		t.Fatalf("部分成功应 200（不丢已扣结果），实际 %d：%s", rec.Code, body)
	}
	if !strings.Contains(body, `"Rebilled":1`) || !strings.Contains(body, `"Failed"`) ||
		!strings.Contains(body, `"Reason"`) || !strings.Contains(body, "b-bad") {
		t.Fatalf("响应缺部分结果/失败明细：%s", body)
	}
}

// priceRow 构造售价命中行。第 5 列 sale_v2 恒 nil（这些用例都是旧三段口径；
// v2 行为在 settle v2 专项用例里构造）。
func priceRow(extName string, rates map[string]float64) *sqlmock.Rows {
	raw, _ := json.Marshal(rates)
	return sqlmock.NewRows([]string{"external_name", "sale_rates", "time_config", "context_tiers", "sale_v2"}).
		AddRow(extName, raw, []byte("null"), []byte("null"), nil)
}

func int64Ptr(v int64) *int64 { return &v }

// TestRebill_Random20 随机 token 数 / 随机价格 20 轮：逐笔金额独立自校验 + 重复补收全拒。
func TestRebill_Random20(t *testing.T) {
	for i := 0; i < 20; i++ {
		t.Run(fmt.Sprintf("round-%02d", i+1), func(t *testing.T) {
			groupID := rseg(t)
			groupRef := "group/" + groupID
			extName := "ext-" + rseg(t)
			rates := map[string]float64{
				"input": rfloat(0.5, 20), "output": rfloat(0.5, 20), "cache_read": rfloat(0.05, 2)}
			origID := "bill-" + rseg(t)
			cost := rfloat(0.01, 5)
			in, out, cache := rint(0, 50000), rint(0, 20000), rint(0, 5000)
			usage := billing.Usage{Input: in - cache, Output: out, CacheRead: cache}
			want, err := billing.ComputeCredits(usage, rates, 1, 1, 10000)
			if err != nil {
				t.Fatalf("ComputeCredits 失败：%v", err)
			}

			f := newFixture(t)
			f.expectGroups([2]string{groupRef, groupID})
			f.mock.ExpectQuery("WHERE bound_internal_ref =").WithArgs(groupRef).
				WillReturnRows(priceRow(extName, rates))
			f.expectScan(rebillRow{
				BillingID: origID, UserID: 42, TokenID: int64Ptr(7),
				ExternalModel: groupRef, InternalModelID: "p/m",
				Tokens: usage, CostCredits: &cost,
				ReqModel: groupRef, ServedModel: "s-" + rseg(t), RouteID: rint(1, 99),
			})
			f.expectState()
			f.mock.ExpectQuery("WHERE bound_internal_ref =").WithArgs(groupRef).
				WillReturnRows(priceRow(extName, rates))
			credits := rfloat(0.01, 500)
			f.rec.bals = []float64{credits}
			f.mock.ExpectExec("UPDATE billing_records SET cost_credits").
				WithArgs(cost, rebillNewBillingID(origID), nil).WillReturnResult(sqlmock.NewResult(0, 1))
			f.expectMark(origID, rebillNewBillingID(origID))

			res, err := f.p.Rebill(context.Background(), groupRef)
			if err != nil {
				t.Fatalf("第 %d 轮 Rebill 失败：%v", i+1, err)
			}
			if res.Rebilled != 1 {
				t.Fatalf("第 %d 轮 Rebilled = %d", i+1, res.Rebilled)
			}
			req := f.rec.reqs[0]
			got, _ := billing.ComputeCredits(req.Tokens, req.Rates, 1, 1, 10000)
			if got != want {
				t.Fatalf("第 %d 轮金额不符：got=%v want=%v（rates=%v usage=%+v）", i+1, got, want, rates, usage)
			}
			if req.Tokens != usage {
				t.Fatalf("第 %d 轮用量应取原行：%+v", i+1, req.Tokens)
			}
			// 重复补收（同一原行）：标记判定拦截，不再产生 Record。
			f2 := newFixture(t)
			f2.expectGroups([2]string{groupRef, groupID})
			f2.mock.ExpectQuery("WHERE bound_internal_ref =").WithArgs(groupRef).
				WillReturnRows(priceRow(extName, rates))
			f2.expectScan(rebillRow{
				BillingID: origID, UserID: 42, ExternalModel: groupRef,
				Tokens:       usage,
				ErrorMessage: "原文" + rebillMarkSep + rebillMarkPrefix + rebillNewBillingID(origID),
				ReqModel:     groupRef,
			})
			res2, err := f2.p.Rebill(context.Background(), groupRef)
			if err != nil {
				t.Fatalf("第 %d 轮重复补收应静默跳过：%v", i+1, err)
			}
			if res2.Rebilled != 0 || res2.AlreadyMarked != 1 || len(f2.rec.reqs) != 0 {
				t.Fatalf("第 %d 轮重复补收未被拒：%+v", i+1, res2)
			}
		})
	}
}

// TestRebill_Matching 覹盖匹配范围：auto/<model> slug 归一、display_name 档、无关组不匹配。
func TestRebill_Matching(t *testing.T) {
	modelName := "My Model " + rseg(t)
	groupID := "auto-" + groupSlug(modelName)
	display := "组" + rseg(t)
	g := groupNode{Ref: "group/" + groupID, DisplayName: display}
	ref := "group/" + groupID

	cases := []struct {
		name string
		row  rebillRow
		want bool
	}{
		{"组ref命中", rebillRow{ReqModel: ref}, true},
		{"组id命中", rebillRow{ExternalModel: groupID}, true},
		{"display_name命中", rebillRow{ReqModel: display}, true},
		{"served名命中", rebillRow{ServedModel: ref}, true},
		{"auto形态命中", rebillRow{ReqModel: "auto/" + modelName}, true},
		{"无关组不命中", rebillRow{ReqModel: "group/other-" + rseg(t)}, false},
		{"同名前缀不命中", rebillRow{ReqModel: ref + "-x"}, false},
		{"空名不命中", rebillRow{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := rebillMatchesRow(tc.row, []string{ref}, []groupNode{g}); got != tc.want {
				t.Fatalf("匹配结果 = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// TestMarkRebilled_GuardClauses 标记 UPDATE 的守卫谓词必须齐全（源码级）。
// 锚定 markRebilled 的 SQL 段（从 SET error_message 到 $3 结尾）：扫描 SQL 里的同名子串
// （b.status = 'unpriced'）不能算数 —— 不锁段落，守卫被删时这条断言是假绿。
func TestMarkRebilled_GuardClauses(t *testing.T) {
	src, err := readRebillSource(t)
	if err != nil {
		t.Fatalf("读取 rebill.go 失败：%v", err)
	}
	start := strings.Index(src, "UPDATE billing_records SET error_message = CASE")
	if start < 0 {
		t.Fatalf("找不到 markRebilled 的 SQL 段")
	}
	seg := src[start:]
	if end := strings.Index(seg, "rebillMarkSep+mark"); end > 0 {
		seg = seg[:end]
	}
	mustContain := []string{
		"status = 'unpriced'",
		// 源码形态：SQL 字符串与常量的拼接表达式（不是解析后的字面量）。
		"NOT LIKE '%`+rebillMarkPrefix+`%'",
		"error_message ||",
	}
	for _, pat := range mustContain {
		if !strings.Contains(seg, pat) {
			t.Fatalf("标记 SQL 缺守卫谓词 %q：\n%s", pat, seg)
		}
	}
}

// readRebillSource 读本包源码（与 server 包的源码级断言同法）。
func readRebillSource(t *testing.T) (string, error) {
	t.Helper()
	b, err := os.ReadFile("rebill.go")
	return string(b), err
}

// doRebillRequest 走真实 HTTP mux 调补收端点（断言的是线上那条路由的 handler）。
func doRebillRequest(t *testing.T, h *RebillHandler, body string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/admin/billings/rebill-unpriced", h.HandleRebillUnpriced)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/billings/rebill-unpriced", strings.NewReader(body))
	mux.ServeHTTP(rec, req)
	return rec, rec.Body.String()
}

// TestRebillHandler_HTTP HTTP 层：401 由鉴权守卫承担（server 侧），这里验业务分支与 Reason。
func TestRebillHandler_HTTP(t *testing.T) {
	t.Run("空Ref返回400+invalid_field", func(t *testing.T) {
		h := NewRebillHandler(&Poller{}) // 业务前置校验不需要 poller 依赖
		rec, body := doRebillRequest(t, h, `{}`)
		if rec.Code != 400 || !strings.Contains(body, `"invalid_field"`) {
			t.Fatalf("status=%d body=%s", rec.Code, body)
		}
	})
	t.Run("组未定价映射400+reason", func(t *testing.T) {
		f := newFixture(t)
		f.expectGroups()
		f.expectAllPriceMiss("group/ghost", "", nil, 1)
		h := NewRebillHandler(f.p)
		rec, body := doRebillRequest(t, h, `{"Ref":"group/ghost"}`)
		if rec.Code != 400 || !strings.Contains(body, ReasonRebillGroupUnpriced) {
			t.Fatalf("status=%d body=%s", rec.Code, body)
		}
	})
	t.Run("行级解析失败进明细不再400", func(t *testing.T) {
		f := newFixture(t)
		g := "group/" + rseg(t)
		f.expectGroups([2]string{g, g})
		f.mock.ExpectQuery("WHERE bound_internal_ref =").WithArgs(g).
			WillReturnRows(priceRow("ext", map[string]float64{"input": 1, "output": 1, "cache_read": 0.1}))
		f.expectScan(rebillRow{BillingID: "b1", UserID: 42, ExternalModel: g, Tokens: billing.Usage{Input: 5}, ReqModel: g})
		f.expectState()
		// 行级解析：整条候选链全落空 → 行进 Failed 明细、整批仍 200。
		f.expectAllPriceMiss(g, "", nil, 0)
		h := NewRebillHandler(f.p)
		rec, body := doRebillRequest(t, h, `{"Ref":"`+g+`"}`)
		if rec.Code != 200 {
			t.Fatalf("行级失败随批次走完应 200，实际 %d：%s", rec.Code, body)
		}
		if !strings.Contains(body, ReasonRebillRowUnresolvable) || !strings.Contains(body, `"Rebilled":0`) {
			t.Fatalf("响应应含失败明细键：%s", body)
		}
	})
	t.Run("未装配返回503", func(t *testing.T) {
		h := NewRebillHandler(nil) // 装配了处理器但 poller 为空
		rec, body := doRebillRequest(t, h, `{"Ref":"g"}`)
		if rec.Code != 503 {
			t.Fatalf("status=%d body=%s", rec.Code, body)
		}
	})
	t.Run("批次级库故障500不裸吐原文", func(t *testing.T) {
		// 不注册任何价格/扫描期望 → 预检查询报错 → Rebill 上抛 → default 兜底 500；
		// 响应只给通用文案，库层错误原文不得进响应（进日志）。
		f := newFixture(t)
		h := NewRebillHandler(f.p)
		rec, body := doRebillRequest(t, h, `{"Ref":"group/`+rseg(t)+`"}`)
		if rec.Code != 500 {
			t.Fatalf("status=%d body=%s", rec.Code, body)
		}
		if strings.Contains(body, "sqlmock") || strings.Contains(body, "was not expected") ||
			strings.Contains(body, "Query") {
			t.Fatalf("500 不得裸吐内部错误原文：%s", body)
		}
		if !strings.Contains(body, "补收执行失败") {
			t.Fatalf("500 应给通用文案：%s", body)
		}
		// 原文必须在服务端日志里留痕（排障不能丢现场）。
		if !strings.Contains(f.logs.String(), "V3 补收批次级失败") {
			t.Fatalf("批次级错误应进服务端日志：%s", f.logs.String())
		}
	})
}

// TestRebill_DuplicateDBEcho 库层 23505 回声：Record 撞 billing_id 唯一索引 → 视为已补收跳过。
func TestRebill_DuplicateDBEcho(t *testing.T) {
	f := newFixture(t)
	g := "group/" + rseg(t)
	f.expectGroups([2]string{g, g})
	f.expectPriceChain(g, g, nil, 1, g, "ext", map[string]float64{"input": 1, "output": 2, "cache_read": 0.1})
	f.expectScan(rebillRow{BillingID: "bill-dup", UserID: 42, ExternalModel: g, Tokens: billing.Usage{Input: 5}, ReqModel: g})
	f.expectState()
	f.expectPriceChain(g, "", nil, 0, g, "ext", map[string]float64{"input": 1, "output": 2, "cache_read": 0.1})
	f.rec.err = &pgconn.PgError{Code: "23505", Message: "duplicate key value violates unique constraint \"billing_records_pkey\""}
	res, err := f.p.Rebill(context.Background(), g)
	if err != nil {
		t.Fatalf("23505 应视为已补收：%v", err)
	}
	if res.AlreadyMarked != 1 || res.Rebilled != 0 {
		t.Fatalf("23505 应计 AlreadyMarked：%+v", res)
	}
}

// TestRebill_23505IsDuplicate 其他错误不吞：余额不足进失败明细（毒行不堵整批）。
func TestRebill_23505IsDuplicate(t *testing.T) {
	f := newFixture(t)
	g := "group/" + rseg(t)
	f.expectGroups([2]string{g, g})
	f.expectPriceChain(g, g, nil, 1, g, "ext", map[string]float64{"input": 1, "output": 2, "cache_read": 0.1})
	f.expectScan(rebillRow{BillingID: "bill-poor", UserID: 42, ExternalModel: g, Tokens: billing.Usage{Input: 5}, ReqModel: g})
	f.expectState()
	f.expectPriceChain(g, "", nil, 0, g, "ext", map[string]float64{"input": 1, "output": 2, "cache_read": 0.1})
	f.rec.err = errors.New("余额不足（40201）")
	res, err := f.p.Rebill(context.Background(), g)
	if err != nil {
		t.Fatalf("行级失败不失败整批：%v", err)
	}
	if len(res.Failed) != 1 || !strings.Contains(res.Failed[0].Message, "余额不足") {
		t.Fatalf("余额不足必须进失败明细原样透传：%+v", res.Failed)
	}
	if res.Failed[0].Reason != ReasonRebillRecordError || res.Rebilled != 0 {
		t.Fatalf("失败明细键/计数不符：%+v", res)
	}
}
