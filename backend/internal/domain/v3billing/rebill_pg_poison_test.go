package v3billing

// rebill_pg_poison_test.go：毒行隔离与重复绑定 ref 的**真库**证明（评审 P2-1/P2-2）。
//
// 运行方式与 rebill_pg_test.go 相同（LLMGATEWAY_TEST_PG_DSN 未设置时 t.Skip）。
//
// 两条硬断言：
//  1. 毒行不堵整批 —— tokens 形状损坏（合法 JSONB、错形状）与「无钱包用户」两类毒行
//     只进响应 Failed 明细，好行照常补收、余额只扣好行的钱、原行不被改动；
//  2. 同 ref 重复绑定（非唯一索引下库里允许）时取价确定性 —— 两次解析同一 ref 结果
//     逐字段一致（ORDER BY id ASC 钉住）；探针只报数量、不清理数据（唯一化属部署窗口
//     动作，等拍板）。

import (
	"context"
	"database/sql"
	"fmt"
	"math/rand"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/domain/billing"
)

// seedUnpricedRaw 落一张 unpriced 原账单（tokens 用**原始 JSON 文本**，可注入错形状毒行）。
func seedUnpricedRaw(t *testing.T, conn *sql.DB, billingID string, userID, tokenID int64, tokensRaw, reqModel string) {
	t.Helper()
	if _, err := conn.Exec(`
		INSERT INTO billing_records(id, billing_id, user_id, pricing_mode, token_id, external_model_name,
			internal_model_id, call_time, tokens, rates, coefficients, r_value, raw_total, credits_consumed,
			cost_credits, balance_before, balance_after, status, error_message, cost_source)
		VALUES ($1,$2,$3,'sale',$4,$5,'prov/served',now(),$6::jsonb,'{}','{}',10000,0,0,0,0,0,'unpriced',$7,'ledger')`,
		rand.Int63n(1<<40)+1, billingID, userID, tokenID, reqModel, tokensRaw, "未定价组："+reqModel); err != nil {
		t.Fatalf("插入 unpriced 账单 %s 失败: %v", billingID, err)
	}
}

// TestRebill_PG_PoisonRowIsolation 真库毒行隔离：坏行进明细、好行照常补收、余额只扣好行。
func TestRebill_PG_PoisonRowIsolation(t *testing.T) {
	conn := openRebillTempDB(t)
	env := seedRebillEnv(t, conn)
	ctx := context.Background()

	// 毒行 2 的前置：存在用户但没有钱包（计费 40401 钱包不存在）。
	poorUser := env.userID + 1
	if _, err := conn.Exec(`INSERT INTO users(id, username, password_hash, role)
		VALUES ($1,$2,'x','DEVELOPER')`, poorUser, "rebill-poor-user"); err != nil {
		t.Fatalf("插入无钱包用户失败: %v", err)
	}

	usage := billing.Usage{Input: 600, Output: 60}
	goodID := "orig-good"
	env.seedUnpriced(t, goodID, usage, 0.1, env.groupRef) // 好行（有成本可沿用）
	// 毒行 1：tokens 合法 JSONB、错形状（字符串）—— json.Unmarshal 进 Usage 必失败。
	seedUnpricedRaw(t, conn, "orig-badtok", env.userID, env.tokenID, `"corrupted"`, env.groupRef)
	// 毒行 2：user_id 指向无钱包用户。
	seedUnpricedRaw(t, conn, "orig-poor", poorUser, env.tokenID, string(mustJSON(t, usage)), env.groupRef)

	rates := map[string]float64{"input": 3, "output": 6, "cache_read": 0.5}
	wantGood, err := billing.ComputeCredits(usage, rates, 1, 1, 10000)
	if err != nil {
		t.Fatalf("复算失败: %v", err)
	}

	before := env.wallet(t)
	res, err := env.poller.Rebill(ctx, env.groupRef)
	if err != nil {
		t.Fatalf("毒行不应失败整批: %v", err)
	}
	if res.Rebilled != 1 || res.Scanned != 3 || len(res.Failed) != 2 {
		t.Fatalf("毒行隔离汇总不符：%+v", res)
	}
	// 余额只扣好行的钱（毒行零扣款）。
	after := env.wallet(t)
	if diff := (before - after) - wantGood; diff > 1e-5 || diff < -1e-5 {
		t.Fatalf("余额扣减不符：实扣=%v 期望=%v（毒行不得扣款）", before-after, wantGood)
	}
	byID := map[string]string{}
	for _, fl := range res.Failed {
		byID[fl.BillingID] = fl.Reason + "|" + fl.Message
	}
	if fl := byID["orig-badtok"]; !strings.Contains(fl, ReasonRebillBrokenRow) || !strings.Contains(fl, "tokens") {
		t.Fatalf("坏 tokens 行明细不符：%q", fl)
	}
	if fl := byID["orig-poor"]; !strings.Contains(fl, ReasonRebillNoWallet) || !strings.Contains(fl, "钱包") {
		t.Fatalf("无钱包行明细不符：%q", fl)
	}
	// 原行不被改动（毒行保持 unpriced 原样、好行带标记）。
	for _, orig := range []string{"orig-badtok", "orig-poor"} {
		b, _ := env.getBill(t, orig)
		if b.Status != billing.StatusUnpriced || strings.Contains(b.ErrorMessage, rebillMarkPrefix) {
			t.Fatalf("毒行原行被改动：%+v", b)
		}
	}
	if b, _ := env.getBill(t, goodID); !strings.Contains(b.ErrorMessage, rebillMarkPrefix) {
		t.Fatalf("好行未标记：%+v", b)
	}
	// 重放：毒行仍在明细（数据没修就不消失），好行转 AlreadyMarked。
	res2, err := env.poller.Rebill(ctx, env.groupRef)
	if err != nil {
		t.Fatalf("重放失败: %v", err)
	}
	if res2.Rebilled != 0 || res2.AlreadyMarked != 1 || len(res2.Failed) != 2 {
		t.Fatalf("重放汇总不符：%+v", res2)
	}
}

// TestRebill_PG_DuplicateBindingRefProbe 同 ref 重复绑定探针（评审 P2-2）：
// 造第二行同 ref 绑定（非唯一索引下库里允许）→ 证明取价确定性；只报数量，不清理。
func TestRebill_PG_DuplicateBindingRefProbe(t *testing.T) {
	conn := openRebillTempDB(t)
	env := seedRebillEnv(t, conn)
	ctx := context.Background()

	// 前置：该 ref 恰 1 个绑定。
	var cnt int
	if err := conn.QueryRow(`SELECT count(*) FROM external_models
		WHERE bound_internal_ref = $1 AND deleted_at IS NULL`, env.groupRef).Scan(&cnt); err != nil {
		t.Fatalf("探针前置查询失败: %v", err)
	}
	if cnt != 1 {
		t.Fatalf("探针前置不符：该 ref 应恰 1 个绑定，实际 %d", cnt)
	}
	// 造重复：同 ref 第二行（售价不同，加大「取错行」的区分度）。
	if _, err := conn.Exec(`INSERT INTO external_models(id, external_name, sale_rates, bound_internal_ref, enabled)
		VALUES ($1,$2,$3,$4,true)`, rand.Int63n(1<<40)+1,
		fmt.Sprintf("ext-dup-%d", rand.Int63n(1<<40)),
		mustJSON(t, map[string]float64{"input": 99, "output": 99, "cache_read": 99}),
		env.groupRef); err != nil {
		t.Fatalf("制造重复绑定失败: %v", err)
	}
	if err := conn.QueryRow(`SELECT count(*) FROM external_models
		WHERE bound_internal_ref = $1 AND deleted_at IS NULL`, env.groupRef).Scan(&cnt); err != nil {
		t.Fatalf("重复计数失败: %v", err)
	}
	if cnt != 2 {
		t.Fatalf("重复绑定未生效：%d", cnt)
	}
	// 取价确定性：生产 SQL（extByBindingSQL）解析两次，命中行逐字段一致。
	p1, _, err := env.poller.resolvePrice(ctx, row{reqModel: env.groupRef, routeID: 1}, nil)
	if err != nil || p1 == nil {
		t.Fatalf("首次解析失败: %v", err)
	}
	p2, _, err := env.poller.resolvePrice(ctx, row{reqModel: env.groupRef, routeID: 1}, nil)
	if err != nil || p2 == nil {
		t.Fatalf("二次解析失败: %v", err)
	}
	if p1.externalName != p2.externalName {
		t.Fatalf("同 ref 重复绑定时取价不确定：%s vs %s", p1.externalName, p2.externalName)
	}
	if len(p1.saleRates) != len(p2.saleRates) {
		t.Fatalf("同 ref 重复绑定时售价不确定：%v vs %v", p1.saleRates, p2.saleRates)
	}
	for k, v := range p1.saleRates {
		if p2.saleRates[k] != v {
			t.Fatalf("同 ref 重复绑定时售价不确定（键 %s）：%v vs %v", k, v, p2.saleRates[k])
		}
	}
	t.Logf("重复 ref 探针：绑定列同 ref 行数=%d（探明不清理；唯一化涉及存量去重与部署窗口，等拍板）", cnt)
}
