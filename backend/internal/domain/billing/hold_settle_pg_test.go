package billing

// hold_settle_pg_test.go 是「预扣终结与账单结算同事务」的**真库**证明（Owl 复审 #84 P1）。
//
// 要证明的两件事：
//  1. 结算成功 → 账单 completed 与 hold=settled 同时生效（不会出现「已结算但 hold 未 settled」→ 过期整笔退回）；
//  2. 终结失败 → 整个结算事务回滚：账单不落、余额不动、hold 仍 held（可被退款/过期扫描接手）。
//
// 运行方式：
//
//	export LLMGATEWAY_TEST_PG_DSN='postgres://user:pass@host:5432/postgres?sslmode=disable'
//	go test ./internal/domain/billing -run TestRecord_HoldSettle -v

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/pkg/dbx"
)

// emptyConfig 复用包内 defaultConfigProvider + staticSource：R=10000、无全局时段/分档。
func emptyConfig() ConfigProvider {
	return &defaultConfigProvider{src: staticSource{
		cfgR:          []byte(`10000`),
		cfgTimeConfig: []byte(`{}`),
		cfgTiers:      []byte(`[]`),
	}}
}

// failingCloser 模拟「终结 hold 时数据库出错」。
type failingCloser struct{ err error }

func (f failingCloser) CloseHoldTx(context.Context, dbx.Execer, string, string, string) (bool, error) {
	return false, f.err
}

func newHoldSettleService(t *testing.T, conn *sql.DB) (*Service, *identity.CreditService) {
	t.Helper()
	credits := identity.NewCreditService(identity.NewCreditStore(conn))
	svc := NewService(NewSqlStore(conn), emptyConfig(), credits)
	svc.SetTxBeginner(conn)
	return svc, credits
}

func holdBalance(t *testing.T, conn *sql.DB, userID int64) float64 {
	t.Helper()
	var bal float64
	if err := conn.QueryRow(`SELECT balance FROM credit_wallets WHERE user_id = $1`, userID).Scan(&bal); err != nil {
		t.Fatalf("读余额失败: %v", err)
	}
	return bal
}

// TestRecord_HoldSettle_SameTransaction 结算成功 → 账单与 hold 终结一起生效。
func TestRecord_HoldSettle_SameTransaction(t *testing.T) {
	conn := openBillingTempDB(t)
	seedBillingUser(t, conn)
	if _, err := conn.Exec(`INSERT INTO credit_wallets(user_id, balance, version) VALUES (7, 100, 1)`); err != nil {
		t.Fatalf("插入钱包失败: %v", err)
	}
	svc, credits := newHoldSettleService(t, conn)

	hold, err := credits.PreConsumeHold(context.Background(), identity.HoldRequest{
		BillingID: "req-hold-ok", UserID: 7, Amount: 10, Remark: "预扣",
	})
	if err != nil {
		t.Fatalf("预扣失败: %v", err)
	}
	if got := holdBalance(t, conn, 7); got != 90 {
		t.Fatalf("预扣后余额应为 90，实际 %v", got)
	}

	rec, err := svc.Record(context.Background(), RecordReq{
		BillingID:     hold.BillingID,
		UserID:        7,
		PricingMode:   "sale",
		ExternalModel: "gpt-4o",
		Tokens:        Usage{Input: 1000, Output: 100},
		Rates:         Rates{"input": 0.001, "output": 0.002, "cache_read": 0},
		PreConsumed:   10,
		Hold: &HoldCloseReq{
			BillingID: hold.BillingID,
			Status:    identity.HoldSettled,
			Closer:    credits,
		},
	})
	if err != nil {
		t.Fatalf("结算失败: %v", err)
	}
	if rec == nil || rec.Status != StatusCompleted {
		t.Fatalf("账单应为 completed，实际 %+v", rec)
	}
	// hold 必须在同一事务里已经是 settled —— 之后过期扫描就不可能再退这笔。
	h, err := credits.GetHold(context.Background(), hold.BillingID)
	if err != nil {
		t.Fatalf("读预扣登记失败: %v", err)
	}
	if h.Status != identity.HoldSettled {
		t.Fatalf("结算成功后 hold 应为 settled，实际 %q（会被过期扫描整笔退回）", h.Status)
	}
}

// TestRecord_HoldSettle_RollsBackOnCloseFailure 终结失败 → 账单不落、余额不动、hold 仍 held。
//
// 这条是关键：只要「账单落了但 hold 没 settled」，15 分钟后过期扫描就会把预扣整笔退回 = 白送。
// 回滚后由调用方走退款路径（facadebill.refundAndClose），两条路都是原子的。
func TestRecord_HoldSettle_RollsBackOnCloseFailure(t *testing.T) {
	conn := openBillingTempDB(t)
	seedBillingUser(t, conn)
	if _, err := conn.Exec(`INSERT INTO credit_wallets(user_id, balance, version) VALUES (7, 100, 1)`); err != nil {
		t.Fatalf("插入钱包失败: %v", err)
	}
	svc, credits := newHoldSettleService(t, conn)

	hold, err := credits.PreConsumeHold(context.Background(), identity.HoldRequest{
		BillingID: "req-hold-fail", UserID: 7, Amount: 10, Remark: "预扣",
	})
	if err != nil {
		t.Fatalf("预扣失败: %v", err)
	}

	if _, err := svc.Record(context.Background(), RecordReq{
		BillingID:     hold.BillingID,
		UserID:        7,
		PricingMode:   "sale",
		ExternalModel: "gpt-4o",
		Tokens:        Usage{Input: 1000, Output: 100},
		Rates:         Rates{"input": 0.001, "output": 0.002, "cache_read": 0},
		PreConsumed:   10,
		Hold: &HoldCloseReq{
			BillingID: hold.BillingID,
			Status:    identity.HoldSettled,
			Closer:    failingCloser{err: errors.New("close hold boom")},
		},
	}); err == nil {
		t.Fatalf("终结失败时 Record 必须报错（不许假装成功）")
	}

	var cnt int
	if err := conn.QueryRow(`SELECT count(*) FROM billing_records WHERE billing_id = $1`, hold.BillingID).Scan(&cnt); err != nil {
		t.Fatalf("读账单失败: %v", err)
	}
	if cnt != 0 {
		t.Fatalf("终结失败应整体回滚，账单不该落库（实际 %d 行）", cnt)
	}
	if got := holdBalance(t, conn, 7); got != 90 {
		t.Fatalf("余额应保持预扣后的 90（差额没结算），实际 %v", got)
	}
	h, err := credits.GetHold(context.Background(), hold.BillingID)
	if err != nil {
		t.Fatalf("读预扣登记失败: %v", err)
	}
	if h.Status != identity.HoldHeld {
		t.Fatalf("终结失败后 hold 应仍为 held（留给退款/过期兜底），实际 %q", h.Status)
	}
}
