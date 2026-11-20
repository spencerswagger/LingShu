package v3billing

// 结算匹配测试：表驱动 + 随机值驱动。
//
// 三条硬断言（任务口径，缺一不可）：
//  1. route_id > 0 的账本行优先按**组 ref**（external_models.bound_internal_ref）找到售价；
//  2. route_id 命中不了组时，回退链逐级生效（绑定候选 → req 名 → served 名）；
//  3. 全不中的行落 unpriced 账单（营收 0、ErrorMessage 带原因），**不阻塞**同一批次后面的行，
//     且 unpriced **不许**触发「余额≤0 禁用令牌」（Fail 通道 BalanceAfter 恒 0，不是没钱）。
//
// 所有名字/单价/token 数都用随机值：解析规则必须对任意名字成立，禁止按名字特判。

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/domain/billing"
)

// fakeRec 记录 Record 调用并返回可控结果（余额按调用序取自 bals，用来驱动禁用令牌分支）。
type fakeRec struct {
	mu     sync.Mutex
	reqs   []billing.RecordReq
	bals   []float64 // 第 n 次调用返回 bals[min(n, len-1)]；空 = 恒 100
	err    error     // 恒定错误（每次调用都返回）
	errSeq []error   // 按调用序的错误（第 n 次取 errSeq[n-1]，越界回落 err）；非 nil 时优先
}

func (f *fakeRec) Record(_ context.Context, req billing.RecordReq) (*billing.Record, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := len(f.reqs)
	f.reqs = append(f.reqs, req)
	var e error
	if f.errSeq != nil {
		if n < len(f.errSeq) {
			e = f.errSeq[n]
		}
	} else {
		e = f.err
	}
	if e != nil {
		return nil, e
	}
	bal := 100.0
	if len(f.bals) > 0 {
		i := len(f.reqs) - 1
		if i >= len(f.bals) {
			i = len(f.bals) - 1
		}
		bal = f.bals[i]
	}
	// CreditsConsumed 按真实公式回填（rates 缺失时为 0）：补收汇总的金额合计依赖它。
	credits, _ := billing.ComputeCredits(req.Tokens, req.Rates, 1, 1, 10000)
	return &billing.Record{BillingID: req.BillingID, BalanceAfter: bal, CreditsConsumed: credits}, nil
}

// callCount 返回 Record 被调用次数（毒行隔离断言：坏行不得触发计费）。
func (f *fakeRec) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.reqs)
}

func testLogger(buf *strings.Builder) *slog.Logger {
	if buf == nil {
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return slog.New(slog.NewTextHandler(buf, nil))
}

// rseg 生成随机短名：组 id / 对外模型名 / callerKey 全部随机，规则必须与具体名字无关。
func rseg(t *testing.T) string {
	t.Helper()
	const chars = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 8)
	for i := range b {
		b[i] = chars[rand.Intn(len(chars))]
	}
	return string(b)
}

// fixture 一轮 Once 的 DB 期望装配 + 断言上下文。
type fixture struct {
	t    *testing.T
	mock sqlmock.Sqlmock
	rec  *fakeRec
	p    *Poller
	logs strings.Builder
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock 失败：%v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f := &fixture{t: t, mock: mock, rec: &fakeRec{}, logs: strings.Builder{}}
	f.p = NewPoller(db, nil, nil, testLogger(&f.logs))
	f.p.bill = f.rec
	return f
}

// expectOnceRows 装配「本轮要结算的账本行」。
func (f *fixture) expectOnceRows(rows ...[]any) {
	cols := []string{"ledger_id", "occurred_at", "provider", "model", "req_model", "caller_key_name",
		"route_id", "tokens_in", "tokens_out", "cache_read", "cache_write", "cost_credits"}
	rs := f.mock.NewRows(cols)
	for _, r := range rows {
		vals := make([]driver.Value, len(r))
		for i, v := range r {
			vals[i] = v
		}
		rs.AddRow(vals...)
	}
	f.mock.ExpectQuery("FROM ledger_entries e").WillReturnRows(rs)
}

// expectGroups 装配组快照（不给 = 无组快照）。
func (f *fixture) expectGroups(pairs ...[2]string) {
	rs := f.mock.NewRows([]string{"ref", "display_name"})
	for _, p := range pairs {
		rs.AddRow(p[0], p[1])
	}
	f.mock.ExpectQuery("FROM internal_models WHERE kind = 'group'").WillReturnRows(rs)
}

func (f *fixture) expectToken(callerKey string) {
	rs := f.mock.NewRows([]string{"token_id", "user_id"}).AddRow(int64(7), int64(42))
	f.mock.ExpectQuery("FROM magpie_keys k JOIN tokens t").WithArgs(callerKey).WillReturnRows(rs)
}

func (f *fixture) expectCostMiss(provider, model string) {
	f.mock.ExpectQuery("FROM internal_models WHERE ref =").
		WithArgs(provider + "/" + model).
		WillReturnRows(f.mock.NewRows([]string{"cost_rates"}).AddRow([]byte(`{}`)))
}

// expectPriceHit 装配一次命中（byBinding 决定走绑定列还是名字列）。
func (f *fixture) expectPriceHit(byBinding bool, wantValue, extName string, rates map[string]float64) {
	raw, err := json.Marshal(rates)
	if err != nil {
		f.t.Fatalf("rates 序列化失败：%v", err)
	}
	f.mock.ExpectQuery(priceQueryPattern(byBinding)).WithArgs(wantValue).
		WillReturnRows(f.mock.NewRows([]string{"external_name", "sale_rates", "time_config", "context_tiers"}).
			AddRow(extName, raw, []byte("null"), []byte("null")))
}

func (f *fixture) expectPriceMiss(byBinding bool, wantValue string) {
	f.mock.ExpectQuery(priceQueryPattern(byBinding)).WithArgs(wantValue).WillReturnError(sql.ErrNoRows)
}

func priceQueryPattern(byBinding bool) string {
	if byBinding {
		return "WHERE bound_internal_ref ="
	}
	return "WHERE external_name ="
}

func (f *fixture) expectSettled(ledgerID string, cost float64) {
	f.mock.ExpectExec("UPDATE billing_records SET magpie_request_id").
		WithArgs(ledgerID, cost, "v3-"+ledgerID).
		WillReturnResult(sqlmock.NewResult(0, 1))
}

// bindingValues 取候选里的绑定档值（测试断言输出用）。
func bindingValues(cands []priceCandidate) []string {
	var out []string
	for _, c := range cands {
		if c.byBinding {
			out = append(out, c.value)
		}
	}
	return out
}

func TestSettleRouteGroupPricing(t *testing.T) {
	// 共用随机量：期望与夹具用同一组随机值（本次运行内确定）。
	autoModel := "GPT " + rseg(t) // auto 组的模型名（含空格与大写 → 与 slug 形态不同）
	plainExt := "ext-" + rseg(t)  // 命中/回退的对外模型名
	caller := "ck-" + rseg(t)
	provider := "prov-" + rseg(t)
	served := "served-" + rseg(t)
	groupRates := map[string]float64{
		"input": rand.Float64()*9 + 1, "output": rand.Float64()*9 + 1, "cache_read": rand.Float64() + 0.1}
	fallbackRates := map[string]float64{
		"input": rand.Float64()*9 + 1, "output": rand.Float64()*9 + 1, "cache_read": rand.Float64() + 0.1}

	cases := []struct {
		name string
		run  func(t *testing.T)
	}{
		{
			// route_id>0 + req_model 是 auto/<model>：第一跳就是组 ref 绑定查询。
			// magpie 镜像里 auto 组的 id 是 auto-<slug(model)>（小写、非字母数字转 -），
			// 所以「auto/GPT x」必须先原样归一（group/auto-GPT x，落空）再 slug 化
			// （group/auto-gpt-x，命中）—— 绑定真源存的是 slug 形态。
			name: "route_id命中按组ref计价",
			run: func(t *testing.T) {
				f := newFixture(t)
				autoRef := "group/auto-" + groupSlug(autoModel)
				lg := "lg-" + rseg(t)
				in, out, cache := int64(1000), int64(500), int64(200)
				f.expectOnceRows([]any{lg, time.Now(), provider, served, "auto/" + autoModel, caller, int64(321),
					in, out, cache, int64(100), 1.25})
				f.expectGroups([2]string{autoRef, autoModel})
				f.expectToken(caller)
				f.expectCostMiss(provider, served)
				// 绑定候选逐个落空，slug 形态命中（命中即停：只注册命中位之前的落空）。
				cands := priceCandidates("auto/"+autoModel, served, nil, 321)
				drawerForm := "auto/" + autoModel
				idxDrawer, idxSlug := -1, -1
				for i, c := range cands {
					if !c.byBinding {
						continue
					}
					if c.value == autoRef {
						idxSlug = i
						break
					}
					if c.value == drawerForm {
						idxDrawer = i
					}
					f.expectPriceMiss(true, c.value)
				}
				if idxDrawer < 0 || idxSlug < 0 || idxDrawer > idxSlug {
					t.Fatalf("auto/<model> 应归一出抽屉形态 %s 与 slug 组 ref %s，实际候选 %v", drawerForm, autoRef, bindingValues(cands))
				}
				f.expectPriceHit(true, autoRef, plainExt, groupRates)
				f.expectSettled(lg, 1.25)

				if _, err := f.p.Once(context.Background()); err != nil {
					t.Fatalf("Once 失败：%v", err)
				}
				if len(f.rec.reqs) != 1 {
					t.Fatalf("Record 调用次数 = %d，期望 1", len(f.rec.reqs))
				}
				req := f.rec.reqs[0]
				if !reflect.DeepEqual(map[string]float64(req.Rates), groupRates) {
					t.Fatalf("计价没有走组售价：rates=%v 期望 %v", req.Rates, groupRates)
				}
				if req.ExternalModel != plainExt {
					t.Fatalf("账单对外模型 = %q，期望绑定命中的 %q", req.ExternalModel, plainExt)
				}
				if req.Status != billing.StatusCompleted || req.Fail {
					t.Fatalf("status=%q fail=%v，期望 completed/正常路径", req.Status, req.Fail)
				}
				// 计价数值自校验：组售价 × token = 期望金额（R=10000、系数 1）。
				usage := billing.Usage{Input: in - cache, Output: out, CacheRead: cache}
				want, err := billing.ComputeCredits(usage, groupRates, 1, 1, 10000)
				if err != nil {
					t.Fatalf("ComputeCredits 失败：%v", err)
				}
				got, err := billing.ComputeCredits(usage, req.Rates, 1, 1, 10000)
				if err != nil || got != want {
					t.Fatalf("组计价金额不符：got=%v want=%v", got, want)
				}
			},
		},
		{
			// PR #169 阻断项回归：售价抽屉保存的绑定形态是网关的 auto/<组名>（Gate 真机
			// A/B 实锤：绑旧形态全绿、绑抽屉形态即断）。候选链必须把抽屉形态本身作为
			// 绑定候选（且排最前），镜像 slug 形态与抽屉形态互相归一。
			name: "抽屉auto形态绑定命中",
			run: func(t *testing.T) {
				f := newFixture(t)
				autoModel := "Flash " + rseg(t) // 含空格与大写：形态之间字符串不相等才有效
				served2 := "served-" + rseg(t)
				groupRates := map[string]float64{
					"input": rand.Float64()*9 + 1, "output": rand.Float64()*9 + 1, "cache_read": rand.Float64() + 0.1}
				lg := "lg-" + rseg(t)
				f.expectOnceRows([]any{lg, time.Now(), provider, served2, "auto/" + autoModel, caller, int64(77),
					int64(800), int64(300), int64(120), int64(30), 0.75})
				f.expectGroups() // 快照被同步侧过滤（auto 组），必须靠形态归一命中，不许依赖快照
				f.expectToken(caller)
				f.expectCostMiss(provider, served2)
				// 命中即停：抽屉形态排最前，注册它之前落的候选（镜像原样形态）。
				for _, c := range priceCandidates("auto/"+autoModel, served2, nil, 77) {
					if c.byBinding && c.value == "auto/"+autoModel {
						break
					}
					if c.byBinding {
						f.expectPriceMiss(true, c.value)
					}
				}
				f.expectPriceHit(true, "auto/"+autoModel, plainExt, groupRates)
				f.expectSettled(lg, 0.75)

				if _, err := f.p.Once(context.Background()); err != nil {
					t.Fatalf("Once 失败：%v", err)
				}
				if len(f.rec.reqs) != 1 {
					t.Fatalf("Record 调用次数 = %d，期望 1", len(f.rec.reqs))
				}
				req := f.rec.reqs[0]
				if req.Fail || req.Status != billing.StatusCompleted {
					t.Fatalf("抽屉形态应命中组售价：status=%q fail=%v", req.Status, req.Fail)
				}
				if !reflect.DeepEqual(map[string]float64(req.Rates), groupRates) {
					t.Fatalf("没走抽屉形态绑定的组售价：rates=%v", req.Rates)
				}
				// 双向归一自校验（补收链 rebillMatchesRow 同源）。注意方向性：
				// slug 有损（"Flash x" → "flash-x"），从 slug 串**还原不出**原样的抽屉形态，
				// 只断言无损方向；slug 形态两边互推用 slug 安全的模型名另测。
				mirror := "group/auto-" + autoModel // 原样镜像形态（无损）
				if !rebillMatchesRow(rebillRow{ReqModel: mirror}, []string{"auto/" + autoModel}, nil) {
					t.Fatalf("镜像形态 %s 应能匹配抽屉绑定 auto/%s", mirror, autoModel)
				}
				if !rebillMatchesRow(rebillRow{ReqModel: "auto/" + autoModel}, []string{mirror}, nil) {
					t.Fatalf("抽屉形态行应能匹配镜像绑定 %s", mirror)
				}
				// 裸名方向（评审 P3 第三向）：账本行 req_model 是裸组名时，同样能匹配
				// 抽屉绑定与镜像 slug 形态（无组快照桥时不丢账）。
				if !rebillMatchesRow(rebillRow{ReqModel: autoModel}, []string{"auto/" + autoModel}, nil) {
					t.Fatalf("裸名行 %s 应能匹配抽屉绑定 auto/%s", autoModel, autoModel)
				}
				slugModel := groupSlug(autoModel)
				slugMirror := "group/auto-" + slugModel
				if !rebillMatchesRow(rebillRow{ReqModel: slugModel}, []string{slugMirror}, nil) {
					t.Fatalf("裸 slug 名行 %s 应能匹配镜像绑定 %s", slugModel, slugMirror)
				}
				if !rebillMatchesRow(rebillRow{ReqModel: slugMirror}, []string{slugMirror}, nil) ||
					!rebillMatchesRow(rebillRow{ReqModel: "auto/" + slugModel}, []string{slugMirror}, nil) {
					t.Fatalf("slug 形态自反匹配失败：%s", slugMirror)
				}
			},
		},
		{
			// route_id>0 但绑定查不到：回退链逐级生效 —— 绑定候选全空 → req 名落空 → served 名命中。
			name: "route_id不中回退链逐级生效",
			run: func(t *testing.T) {
				f := newFixture(t)
				reqModel := "group/" + rseg(t) // 查不到绑定的组 ref 形态
				lg := "lg-" + rseg(t)
				f.expectOnceRows([]any{lg, time.Now(), provider, served, reqModel, caller, int64(88),
					int64(100), int64(50), int64(0), int64(0), 0})
				f.expectGroups() // 无组快照：少一档候选，不报错
				f.expectToken(caller)
				f.expectCostMiss(provider, served)
				for _, c := range priceCandidates(reqModel, served, nil, 88) {
					if c.byBinding {
						f.expectPriceMiss(true, c.value)
					}
				}
				f.expectPriceMiss(false, reqModel) // 名字候选第一级：req 落空
				f.expectPriceHit(false, served, plainExt, fallbackRates)
				f.expectSettled(lg, 0)

				if _, err := f.p.Once(context.Background()); err != nil {
					t.Fatalf("Once 失败：%v", err)
				}
				req := f.rec.reqs[0]
				if !reflect.DeepEqual(map[string]float64(req.Rates), fallbackRates) {
					t.Fatalf("回退计价不符：rates=%v 期望 %v", req.Rates, fallbackRates)
				}
				if req.ExternalModel != plainExt || req.Status != billing.StatusCompleted {
					t.Fatalf("回退结果不符：ext=%q status=%q", req.ExternalModel, req.Status)
				}
			},
		},
		{
			// 全不中：第一行落 unpriced（营收 0、原因可见、不重试），第二行照常结算 —— 不堵批次。
			name: "全不中落unpriced且不阻塞后续行",
			run: func(t *testing.T) {
				f := newFixture(t)
				ghost := "ghost-" + rseg(t)
				lg1, lg2 := "lg-"+rseg(t), "lg-"+rseg(t)
				f.expectOnceRows(
					[]any{lg1, time.Now(), provider, served, ghost, caller, int64(9),
						int64(10), int64(5), int64(0), int64(0), 0.5},
					[]any{lg2, time.Now(), provider, served, plainExt, caller, int64(0),
						int64(20), int64(10), int64(0), int64(0), 0.25},
				)
				f.expectGroups()
				// 行 1：令牌 + 成本 + 全部候选落空
				f.expectToken(caller)
				f.expectCostMiss(provider, served)
				for _, c := range priceCandidates(ghost, served, nil, 9) {
					f.expectPriceMiss(c.byBinding, c.value)
				}
				f.expectSettled(lg1, 0.5) // unpriced 行也要补幂等键 + 成本兜底
				// 行 2：正常结算
				f.expectToken(caller)
				f.expectCostMiss(provider, served)
				f.expectPriceHit(false, plainExt, plainExt, fallbackRates)
				f.expectSettled(lg2, 0.25)

				f.rec.bals = []float64{0, 50} // 行 1「余额」0（Fail 通道的固有形态），行 2 余额 50
				n, err := f.p.Once(context.Background())
				if err != nil {
					t.Fatalf("Once 失败：%v", err)
				}
				if n != 2 {
					t.Fatalf("入账条数 = %d，期望 2（unpriced 也算入账）", n)
				}
				if len(f.rec.reqs) != 2 {
					t.Fatalf("Record 调用次数 = %d，期望 2", len(f.rec.reqs))
				}
				first, second := f.rec.reqs[0], f.rec.reqs[1]
				if !first.Fail || first.Status != billing.StatusUnpriced {
					t.Fatalf("第一行应为 unpriced：fail=%v status=%q", first.Fail, first.Status)
				}
				if !strings.Contains(first.ErrorMessage, "未定价组：") || !strings.Contains(first.ErrorMessage, ghost) {
					t.Fatalf("unpriced 原因不可见：%q", first.ErrorMessage)
				}
				if first.BillingID != "v3-"+lg1 {
					t.Fatalf("unpriced 账单幂等键不符：%q", first.BillingID)
				}
				if second.Status != billing.StatusCompleted || second.Fail {
					t.Fatalf("第二行被第一行堵住：status=%q fail=%v", second.Status, second.Fail)
				}
				// sqlmock 顺序期望里没有「UPDATE tokens」：若实现把 unpriced 的余额 0 当没钱，
				// 会在这里收到未预期调用而失败。
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, tc.run)
	}
}

// TestSettleUnpricedNeverDisablesToken 专项守卫：unpriced 行绝不发禁用令牌的 UPDATE。
// fake 余额恒 0（Fail 通道的固有形态），若实现按「BalanceAfter<=0」一刀切，sqlmock 会报
// 未预期的 UPDATE tokens 调用而变红。
func TestSettleUnpricedNeverDisablesToken(t *testing.T) {
	f := newFixture(t)
	ghost := "ghost-" + rseg(t)
	lg := "lg-" + rseg(t)
	f.expectOnceRows([]any{lg, time.Now(), "p", "m", ghost, "ck", int64(1), 1, 1, 0, 0, 0})
	f.expectGroups()
	f.expectToken("ck")
	f.expectCostMiss("p", "m")
	for _, c := range priceCandidates(ghost, "m", nil, 1) {
		f.expectPriceMiss(c.byBinding, c.value)
	}
	f.expectSettled(lg, 0)
	f.rec.bals = []float64{0}
	if _, err := f.p.Once(context.Background()); err != nil {
		t.Fatalf("Once 失败：%v", err)
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("出现了未预期的 DB 调用（unpriced 不得禁用令牌）：%v", err)
	}
	if strings.Contains(f.logs.String(), "禁用令牌失败") {
		t.Fatalf("unpriced 行触发了禁用令牌（日志）：%s", f.logs.String())
	}
}

// TestSettleCompletedLowBalanceDisablesToken 对照组：正常计费 + 余额 0 → 禁用令牌仍然生效。
func TestSettleCompletedLowBalanceDisablesToken(t *testing.T) {
	f := newFixture(t)
	ext := "ext-" + rseg(t)
	lg := "lg-" + rseg(t)
	f.expectOnceRows([]any{lg, time.Now(), "p", "m", ext, "ck", int64(0), 1, 1, 0, 0, 0})
	f.expectGroups()
	f.expectToken("ck")
	f.expectCostMiss("p", "m")
	f.expectPriceHit(false, ext, ext, map[string]float64{"input": 1, "output": 1, "cache_read": 1})
	f.expectSettled(lg, 0)
	f.mock.ExpectExec("UPDATE tokens SET status = 'DISABLED'").
		WithArgs(int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))
	f.rec.bals = []float64{0}
	if _, err := f.p.Once(context.Background()); err != nil {
		t.Fatalf("Once 失败：%v", err)
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("余额 0 的正常计费行应禁用令牌：%v", err)
	}
	if strings.Contains(f.logs.String(), "禁用令牌失败") {
		t.Fatalf("禁用成功的行不该报失败告警：%s", f.logs.String())
	}
}

// TestSettleGroupSnapshotFailureDoesNotBlock 组快照读失败：不报错，直接模型仍按名字结算。
func TestSettleGroupSnapshotFailureDoesNotBlock(t *testing.T) {
	f := newFixture(t)
	ext := "ext-" + rseg(t)
	lg := "lg-" + rseg(t)
	f.expectOnceRows([]any{lg, time.Now(), "p", "m", ext, "ck", int64(0), 1, 1, 0, 0, 0})
	f.mock.ExpectQuery("FROM internal_models WHERE kind = 'group'").WillReturnError(errors.New("db down"))
	f.expectToken("ck")
	f.expectCostMiss("p", "m")
	f.expectPriceHit(false, ext, ext, map[string]float64{"input": 1, "output": 1, "cache_read": 1})
	f.expectSettled(lg, 0)
	if _, err := f.p.Once(context.Background()); err != nil {
		t.Fatalf("组快照失败不应让整轮失败：%v", err)
	}
}
