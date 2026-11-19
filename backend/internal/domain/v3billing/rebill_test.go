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

// expectState 装配行内新鲜判定（标记 NULL / 新账单不存在 = 可补收）。
func (f *fixture) expectState() {
	f.mock.ExpectQuery("SELECT \\(SELECT error_message FROM billing_records").
		WillReturnRows(f.mock.NewRows([]string{"error_message", "exists"}).AddRow(nil, false))
}

// expectStateMarked 装配「原行已带标记」的新鲜判定。
func (f *fixture) expectStateMarked(mark string) {
	f.mock.ExpectQuery("SELECT \\(SELECT error_message FROM billing_records").
		WillReturnRows(f.mock.NewRows([]string{"error_message", "exists"}).AddRow(mark, true))
}

// expectStateNewExists 装配「新账单已存在」的新鲜判定（重放形态）。
func (f *fixture) expectStateNewExists(origID string, cost float64) {
	f.mock.ExpectQuery("SELECT \\(SELECT error_message FROM billing_records").
		WillReturnRows(f.mock.NewRows([]string{"error_message", "exists"}).AddRow(nil, true))
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
		// Record → 新账单；成本沿用原行。
		f.rec.bals = []float64{88.5}
		f.mock.ExpectExec("UPDATE billing_records SET cost_credits").
			WithArgs(cost, newID).WillReturnResult(sqlmock.NewResult(0, 1))
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

	t.Run("标记守卫归零必须报错", func(t *testing.T) {
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
		if err == nil || !strings.Contains(err.Error(), "拒绝重复补收") {
			t.Fatalf("守卫归零应报错，实际：%v", err)
		}
		if res.Rebilled != 0 {
			t.Fatalf("失败行不计入 Rebilled")
		}
	})
}

// priceRow 构造售价命中行。
func priceRow(extName string, rates map[string]float64) *sqlmock.Rows {
	raw, _ := json.Marshal(rates)
	return sqlmock.NewRows([]string{"external_name", "sale_rates", "time_config", "context_tiers"}).
		AddRow(extName, raw, []byte("null"), []byte("null"))
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
				WithArgs(cost, rebillNewBillingID(origID)).WillReturnResult(sqlmock.NewResult(0, 1))
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
			if got := rebillMatchesRow(tc.row, ref, []groupNode{g}); got != tc.want {
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
	t.Run("行级解析失败映射400+reason", func(t *testing.T) {
		f := newFixture(t)
		g := "group/" + rseg(t)
		f.expectGroups([2]string{g, g})
		f.mock.ExpectQuery("WHERE bound_internal_ref =").WithArgs(g).
			WillReturnRows(priceRow("ext", map[string]float64{"input": 1, "output": 1, "cache_read": 0.1}))
		f.expectScan(rebillRow{BillingID: "b1", UserID: 42, ExternalModel: g, Tokens: billing.Usage{Input: 5}, ReqModel: g})
		f.expectState()
		// 行级解析：整条候选链全落空。
		f.expectAllPriceMiss(g, "", nil, 0)
		h := NewRebillHandler(f.p)
		rec, body := doRebillRequest(t, h, `{"Ref":"`+g+`"}`)
		if rec.Code != 400 || !strings.Contains(body, ReasonRebillRowUnresolvable) {
			t.Fatalf("status=%d body=%s", rec.Code, body)
		}
	})
	t.Run("未装配返回503", func(t *testing.T) {
		h := NewRebillHandler(nil) // 装配了处理器但 poller 为空
		rec, body := doRebillRequest(t, h, `{"Ref":"g"}`)
		if rec.Code != 503 {
			t.Fatalf("status=%d body=%s", rec.Code, body)
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

// TestRebill_23505IsDuplicate 其他错误不吞：余额不足必须原样返回。
func TestRebill_23505IsDuplicate(t *testing.T) {
	f := newFixture(t)
	g := "group/" + rseg(t)
	f.expectGroups([2]string{g, g})
	f.expectPriceChain(g, g, nil, 1, g, "ext", map[string]float64{"input": 1, "output": 2, "cache_read": 0.1})
	f.expectScan(rebillRow{BillingID: "bill-poor", UserID: 42, ExternalModel: g, Tokens: billing.Usage{Input: 5}, ReqModel: g})
	f.expectState()
	f.expectPriceChain(g, "", nil, 0, g, "ext", map[string]float64{"input": 1, "output": 2, "cache_read": 0.1})
	f.rec.err = errors.New("余额不足（40201）")
	_, err := f.p.Rebill(context.Background(), g)
	if err == nil || !strings.Contains(err.Error(), "余额不足") {
		t.Fatalf("余额不足必须透传：%v", err)
	}
}
