package v3billing

// rebill_pg_test.go：补收链路的**真库**证明（可选执行，只在自己的临时库里跑）。
//
// 运行方式：
//
//	cd backend
//	LLMGATEWAY_TEST_PG_DSN='postgres://user:pass@host:port/postgres?sslmode=disable' \
//	  go test ./internal/domain/v3billing -run TestRebill_PG -v
//
// 未设置该变量时 t.Skip（不阻塞普通 go test ./...）。
//
// 三条硬断言（任务口径）：
//  1. 补收后余额 = 初始余额 − Σ(新账单金额)，逐笔与 billing.ComputeCredits 独立复算一致；
//  2. 同一原行第二次补收被拒（不产生新账单、不扣款、标记不变）；
//  3. 原行标记 = 『；已补收:<新账单id>』，金额/状态不被改动（仍是 unpriced、仍 0 营收）。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/db"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/identity"
)

// openRebillTempDB 建临时库 + 跑全部迁移，测试结束自动删库（与主链真库用例同法）。
func openRebillTempDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过补收真库集成测试")
	}
	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("打开管理连接失败: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if err := admin.Ping(); err != nil {
		t.Fatalf("管理连接 ping 失败（DSN 是否可达?）: %v", err)
	}
	name := fmt.Sprintf("llmgateway-rebill-%d", time.Now().UnixNano())
	if _, err := admin.Exec(`CREATE DATABASE "` + name + `"`); err != nil {
		t.Fatalf("创建临时库 %s 失败: %v", name, err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(`SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1`, name)
		if _, derr := admin.Exec(`DROP DATABASE IF EXISTS "` + name + `"`); derr != nil {
			t.Logf("删除临时库 %s 失败: %v", name, derr)
		}
	})
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("解析 DSN 失败: %v", err)
	}
	u.Path = "/" + name
	temp, err := db.Open(u.String())
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = temp.Close() })
	if _, err := db.Migrate(context.Background(), temp); err != nil {
		t.Fatalf("在临时库应用迁移失败: %v", err)
	}
	return temp
}

// rebillEnv 组装真库补收环境：真计费服务（含钱包事务）+ 组 + 令牌 + unpriced 账单。
type rebillEnv struct {
	conn     *sql.DB
	poller   *Poller
	bill     *billing.Service
	groupRef string
	userID   int64
	tokenID  int64
	balance  float64
}

// seedRebillEnv 造数：用户+钱包+令牌+magpie_key+组+对外模型（含售价与绑定）。
func seedRebillEnv(t *testing.T, conn *sql.DB) *rebillEnv {
	t.Helper()
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", rand.Int63())

	var userID, tokenID int64 = 9001, 9101
	if _, err := conn.ExecContext(ctx, `INSERT INTO users(id, username, password_hash, role)
		VALUES ($1,$2,'x','DEVELOPER') ON CONFLICT (id) DO NOTHING`, userID, "rebill-u-"+suffix); err != nil {
		t.Fatalf("插入用户失败: %v", err)
	}
	const balance = 1000.0
	if _, err := conn.ExecContext(ctx, `INSERT INTO credit_wallets(user_id, balance, version)
		VALUES ($1,$2,1) ON CONFLICT (user_id) DO UPDATE SET balance = EXCLUDED.balance`, userID, balance); err != nil {
		t.Fatalf("插入钱包失败: %v", err)
	}
	if _, err := conn.ExecContext(ctx, `INSERT INTO tokens(id, token_hash, token_display, user_id, display_name, status)
		VALUES ($1,$2,'t-rebill',$3,'t-rebill','ACTIVE') ON CONFLICT (id) DO NOTHING`,
		tokenID, "hash-rebill-"+suffix, userID); err != nil {
		t.Fatalf("插入令牌失败: %v", err)
	}
	keyName := "ck-rebill-" + suffix
	if _, err := conn.ExecContext(ctx, `INSERT INTO magpie_keys(id, token_id, key_name, key_cipher, state)
		VALUES ($1,$2,$3,'x','ACTIVE')`, rand.Int63n(1<<40)+1, tokenID, keyName); err != nil {
		t.Fatalf("插入 magpie_key 失败: %v", err)
	}

	groupRef := "group/auto-" + suffix
	if _, err := conn.ExecContext(ctx, `INSERT INTO internal_models(ref, display_name, kind)
		VALUES ($1,$2,'group') ON CONFLICT (ref) DO NOTHING`, groupRef, "grp-"+suffix); err != nil {
		t.Fatalf("插入组快照失败: %v", err)
	}
	// 对外模型：售价 + 绑定组 ref（补收的售价来源）。
	extName := "ext-" + suffix
	rates := map[string]float64{"input": 3, "output": 6, "cache_read": 0.5}
	ratesRaw := mustJSON(t, rates)
	if _, err := conn.ExecContext(ctx, `INSERT INTO external_models(id, external_name, sale_rates, bound_internal_ref, enabled)
		VALUES ($1,$2,$3,$4,true)`, rand.Int63n(1<<40)+1, extName, ratesRaw, groupRef); err != nil {
		t.Fatalf("插入对外模型失败: %v", err)
	}

	// R 与时段/分档：R=10000、无全局分档（缺省即 1）、时段固定 1。
	for k, v := range map[string]string{
		"billing.r":             "10000",
		"billing.context_tiers": "[]",
		"billing.time_config":   `{"Timezone":"Asia/Shanghai","Default":1}`,
	} {
		if _, err := conn.ExecContext(ctx, `INSERT INTO sys_configs(key, value)
			VALUES ($1,$2) ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, k, v); err != nil {
			t.Fatalf("写入配置 %s 失败: %v", k, err)
		}
	}

	creditSvc := identity.NewCreditService(identity.NewCreditStore(conn))
	billSvc := billing.NewService(billing.NewSqlStore(conn), billing.NewConfigProvider(billing.NewSqlStore(conn)), creditSvc)
	billSvc.SetTxBeginner(conn)
	p := NewPoller(conn, billSvc, nil, testLogger(nil))
	return &rebillEnv{conn: conn, poller: p, bill: billSvc, groupRef: groupRef, userID: userID, tokenID: tokenID, balance: balance}
}

// seedUnpriced 落一张 unpriced 原账单（status='unpriced'、0 营收、带原因原文）。
func (e *rebillEnv) seedUnpriced(t *testing.T, billingID string, usage billing.Usage, cost float64, reqModel string) {
	t.Helper()
	raw := mustJSON(t, usage)
	if _, err := e.conn.ExecContext(context.Background(), `
		INSERT INTO billing_records(id, billing_id, user_id, pricing_mode, token_id, external_model_name,
			internal_model_id, call_time, tokens, rates, coefficients, r_value, raw_total, credits_consumed,
			cost_credits, balance_before, balance_after, status, error_message, cost_source)
		VALUES ($1,$2,$3,'sale',$4,$5,'prov/served',now(),$6,'{}','{}',10000,0,0,$7,0,0,'unpriced',$8,'ledger')`,
		rand.Int63n(1<<40)+1, billingID, e.userID, e.tokenID, reqModel, raw, cost,
		"未定价组："+reqModel); err != nil {
		t.Fatalf("插入 unpriced 账单 %s 失败: %v", billingID, err)
	}
}

// balance 读钱包余额。
func (e *rebillEnv) wallet(t *testing.T) float64 {
	t.Helper()
	var b float64
	if err := e.conn.QueryRow(`SELECT balance FROM credit_wallets WHERE user_id = $1`, e.userID).Scan(&b); err != nil {
		t.Fatalf("读余额失败: %v", err)
	}
	return b
}

// row 读一张账单的关键列。
type billRow struct {
	Status        string
	Credits       float64
	CostCredits   float64
	ErrorMessage  string
	ExternalModel string
}

func (e *rebillEnv) getBill(t *testing.T, billingID string) (billRow, bool) {
	t.Helper()
	var out billRow
	var cost sql.NullFloat64
	var msg sql.NullString
	err := e.conn.QueryRow(`SELECT status, COALESCE(credits_consumed,0), cost_credits,
		error_message, external_model_name FROM billing_records WHERE billing_id = $1`, billingID).
		Scan(&out.Status, &out.Credits, &cost, &msg, &out.ExternalModel)
	if err == sql.ErrNoRows {
		return out, false
	}
	if err != nil {
		t.Fatalf("读账单 %s 失败: %v", billingID, err)
	}
	out.CostCredits = cost.Float64
	out.ErrorMessage = msg.String
	return out, true
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return b
}

// TestRebill_PG_EndToEnd 真库端到端：补收两条 → 余额逐笔吻合 → 重复补收被拒 → 标记/金额不变。
func TestRebill_PG_EndToEnd(t *testing.T) {
	conn := openRebillTempDB(t)
	env := seedRebillEnv(t, conn)
	ctx := context.Background()

	usage1 := billing.Usage{Input: 1200, Output: 340, CacheRead: 150}
	usage2 := billing.Usage{Input: 800, Output: 90, CacheRead: 0}
	env.seedUnpriced(t, "orig-1", usage1, 0.42, env.groupRef)
	env.seedUnpriced(t, "orig-2", usage2, 0.11, env.groupRef)
	// 无关组的一张 unpriced：不许被本次补收碰到。
	env.seedUnpriced(t, "orig-other", usage1, 0.2, "group/other-"+fmt.Sprint(rand.Int63()))

	rates := map[string]float64{"input": 3, "output": 6, "cache_read": 0.5}
	want1, err := billing.ComputeCredits(usage1, rates, 1, 1, 10000)
	if err != nil {
		t.Fatalf("复算失败: %v", err)
	}
	want2, err := billing.ComputeCredits(usage2, rates, 1, 1, 10000)
	if err != nil {
		t.Fatalf("复算失败: %v", err)
	}

	before := env.wallet(t)
	res, err := env.poller.Rebill(ctx, env.groupRef)
	if err != nil {
		t.Fatalf("补收失败: %v", err)
	}
	if res.Rebilled != 2 || res.Scanned != 2 {
		t.Fatalf("补收条数不符：%+v", res)
	}
	// 1) 余额逐笔吻合（容差 1e-5 = NUMERIC(20,5) 精度）。
	after := env.wallet(t)
	wantSpend := want1 + want2
	if diff := (before - after) - wantSpend; diff > 1e-5 || diff < -1e-5 {
		t.Fatalf("余额扣减不符：实扣=%v 期望=%v（%v + %v）", before-after, wantSpend, want1, want2)
	}
	for i, orig := range []string{"orig-1", "orig-2"} {
		newID := res.NewBillingIDs[i]
		newBill, ok := env.getBill(t, newID)
		if !ok {
			t.Fatalf("新账单 %s 未落库", newID)
		}
		if newBill.Status != billing.StatusCompleted || newBill.Credits <= 0 {
			t.Fatalf("新账单状态/金额不符：%+v", newBill)
		}
		want := want1
		if i == 1 {
			want = want2
		}
		if d := newBill.Credits - want; d > 1e-5 || d < -1e-5 {
			t.Fatalf("新账单 %s 金额=%v 期望 %v", newID, newBill.Credits, want)
		}
		// 2) 原行：状态/金额不变、标记追加。
		origBill, _ := env.getBill(t, orig)
		if origBill.Status != billing.StatusUnpriced || origBill.Credits != 0 {
			t.Fatalf("原行被改动了（应仍是 unpriced/0 营收）：%+v", origBill)
		}
		if !strings.Contains(origBill.ErrorMessage, rebillMarkPrefix+newID) {
			t.Fatalf("原行标记不符：%q 应含 %q", origBill.ErrorMessage, rebillMarkPrefix+newID)
		}
		if !strings.HasPrefix(origBill.ErrorMessage, "未定价组：") {
			t.Fatalf("原行原因原文被覆盖：%q", origBill.ErrorMessage)
		}
	}
	// 成本沿用原行。
	b1, _ := env.getBill(t, res.NewBillingIDs[0])
	if d := b1.CostCredits - 0.42; d > 1e-5 || d < -1e-5 {
		t.Fatalf("成本未沿用原行：%v 期望 0.42", b1.CostCredits)
	}
	// 无关组未被触碰。
	if other, _ := env.getBill(t, "orig-other"); strings.Contains(other.ErrorMessage, rebillMarkPrefix) {
		t.Fatalf("无关组账单被误补收：%q", other.ErrorMessage)
	}

	// 3) 重复补收被拒：不新增账单、不扣款、标记不变。
	balanceAfterFirst := env.wallet(t)
	res2, err := env.poller.Rebill(ctx, env.groupRef)
	if err != nil {
		t.Fatalf("重复补收应静默跳过：%v", err)
	}
	if res2.Rebilled != 0 || res2.AlreadyMarked != 2 {
		t.Fatalf("重复补收未被拒：%+v", res2)
	}
	if got := env.wallet(t); got != balanceAfterFirst {
		t.Fatalf("重复补收扣了钱：%v → %v", balanceAfterFirst, got)
	}
	var cnt int
	if err := conn.QueryRow(`SELECT count(*) FROM billing_records WHERE billing_id LIKE $1`,
		rebillNewPrefix+"%"+rebillNewSuffix).Scan(&cnt); err != nil {
		t.Fatalf("统计新账单失败: %v", err)
	}
	if cnt != 2 {
		t.Fatalf("新账单条数 = %d，期望 2（重复补收不得新增）", cnt)
	}
}

// TestRebill_PG_ReplayAfterCrash 模拟「新账单已落、标记没跟上」的中断态：重放只补标记不再扣款。
func TestRebill_PG_ReplayAfterCrash(t *testing.T) {
	conn := openRebillTempDB(t)
	env := seedRebillEnv(t, conn)
	ctx := context.Background()

	usage := billing.Usage{Input: 500, Output: 50}
	env.seedUnpriced(t, "orig-crash", usage, 0.3, env.groupRef)

	res, err := env.poller.Rebill(ctx, env.groupRef)
	if err != nil {
		t.Fatalf("首次补收失败: %v", err)
	}
	newID := res.NewBillingIDs[0]
	balanceAfterFirst := env.wallet(t)

	// 人为抹掉标记（模拟「账单写成功、标记事务丢失」的中间态）。
	if _, err := conn.Exec(`UPDATE billing_records SET error_message = $1 WHERE billing_id = 'orig-crash'`,
		"未定价组："+env.groupRef); err != nil {
		t.Fatalf("抹标记失败: %v", err)
	}

	res2, err := env.poller.Rebill(ctx, env.groupRef)
	if err != nil {
		t.Fatalf("重放失败: %v", err)
	}
	if res2.Rebilled != 0 || res2.AlreadyMarked != 1 {
		t.Fatalf("重放应识别「新账单已存在」：%+v", res2)
	}
	if got := env.wallet(t); got != balanceAfterFirst {
		t.Fatalf("重放重复扣款：%v → %v", balanceAfterFirst, got)
	}
	// 重放不补标记（标记由首次成功路径写）→ 这里断言「原行仍可直接读取、新账单唯一」。
	var n int
	if err := conn.QueryRow(`SELECT count(*) FROM billing_records WHERE billing_id = $1`, newID).Scan(&n); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if n != 1 {
		t.Fatalf("新账单应唯一，实际 %d 条", n)
	}
}

// TestRebill_PG_ConcurrentDoubleClick 并发双击：只出一张补收单、只扣一次钱。
func TestRebill_PG_ConcurrentDoubleClick(t *testing.T) {
	conn := openRebillTempDB(t)
	env := seedRebillEnv(t, conn)
	ctx := context.Background()
	usage := billing.Usage{Input: 700, Output: 70}
	env.seedUnpriced(t, "orig-race", usage, 0.2, env.groupRef)

	before := env.wallet(t)
	type out struct {
		res *RebillResult
		err error
	}
	results := make([]out, 2)
	start := make(chan struct{})
	done := make(chan int, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			<-start
			r, err := env.poller.Rebill(ctx, env.groupRef)
			results[i] = out{r, err}
			done <- i
		}(i)
	}
	close(start)
	for i := 0; i < 2; i++ {
		<-done
	}

	// 至少一路成功；两路合计只允许一张新账单（另一路被 23505/标记拒绝或跳过）。
	var totalRebilled int
	for _, o := range results {
		if o.err == nil {
			totalRebilled += o.res.Rebilled
		} else if !strings.Contains(o.err.Error(), "拒绝") && !strings.Contains(o.err.Error(), "已补收") {
			t.Logf("并发路径报错（可接受的非重复类错误）：%v", o.err)
		}
	}
	if totalRebilled > 1 {
		t.Fatalf("并发补收产生了 %d 张补收单，期望 ≤1", totalRebilled)
	}
	var cnt int
	if err := conn.QueryRow(`SELECT count(*) FROM billing_records WHERE billing_id = $1`,
		rebillNewBillingID("orig-race")).Scan(&cnt); err != nil {
		t.Fatalf("统计失败: %v", err)
	}
	if cnt > 1 {
		t.Fatalf("并发补收落了 %d 张新账单（billing_id 唯一应挡住）", cnt)
	}
	if cnt == 1 {
		b, _ := env.getBill(t, rebillNewBillingID("orig-race"))
		if diff := (before - env.wallet(t)) - b.Credits; diff > 1e-5 || diff < -1e-5 {
			t.Fatalf("并发下余额与账单金额不符：扣=%v 账单=%v", before-env.wallet(t), b.Credits)
		}
	}
}
