package identity

// credit_hold_pg_test.go 是预扣终结的**真库**证明（Owl 复审 P1）。
//
// 要证明的不是「代码调了两个函数」，而是三条资金不变量在真事务里成立：
//  1. SettleHold 的「终结 + 退钱 + 流水」要么全成、要么全不成；
//  2. 重复终结不会二次退款（过期扫描与退款抢同一行时只有一方成功）；
//  3. 已经 settled 的登记不会被过期扫描退回（否则等于白送一笔）。
//
// 运行方式：
//
//	export LLMGATEWAY_TEST_PG_DSN='postgres://user:pass@host:5432/postgres?sslmode=disable'
//	go test ./internal/domain/identity -run TestSettleHold -v
//
// 未设置该变量时 t.Skip（不阻塞普通 go test ./...）。

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/db"
)

// openIdentityTempDB 建临时库 + 跑全部迁移（含 0022 的 credit_holds），测试结束删库。
func openIdentityTempDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过预扣终结真库用例")
	}
	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("打开管理连接失败: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	name := fmt.Sprintf("ls_hold_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(context.Background(), `CREATE DATABASE "`+name+`"`); err != nil {
		t.Skipf("无法创建临时库（账号是否没有 CREATEDB 权限？）: %v", err)
	}
	t.Cleanup(func() {
		if a, derr := db.Open(dsn); derr == nil {
			_, _ = a.ExecContext(context.Background(), `DROP DATABASE IF EXISTS "`+name+`" WITH (FORCE)`)
			_ = a.Close()
		}
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("解析 DSN 失败: %v", err)
	}
	u.Path = "/" + name
	target, err := db.Open(u.String())
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })
	if _, err := db.Migrate(context.Background(), target); err != nil {
		t.Fatalf("应用迁移失败: %v", err)
	}
	return target
}

// seedHoldUser 造一个有钱包的用户（余额 seed）。
func seedHoldUser(t *testing.T, conn *sql.DB, userID int64, balance float64) {
	t.Helper()
	ctx := context.Background()
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO users(id, username, password_hash, role) VALUES ($1, $2, 'x', 'DEVELOPER')`,
		userID, fmt.Sprintf("u%d", userID)); err != nil {
		t.Fatalf("插入用户失败: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO credit_wallets(user_id, balance, version) VALUES ($1, $2, 1)`, userID, balance); err != nil {
		t.Fatalf("插入钱包失败: %v", err)
	}
}

func holdBalance(t *testing.T, conn *sql.DB, userID int64) float64 {
	t.Helper()
	var bal float64
	if err := conn.QueryRow(`SELECT balance FROM credit_wallets WHERE user_id = $1`, userID).Scan(&bal); err != nil {
		t.Fatalf("读余额失败: %v", err)
	}
	return bal
}

func holdStatus(t *testing.T, svc *CreditService, billingID string) Hold {
	t.Helper()
	h, err := svc.GetHold(context.Background(), billingID)
	if err != nil {
		t.Fatalf("读预扣登记失败: %v", err)
	}
	return h
}

// TestSettleHold_RefundIsAtomic 退款路径：终结 + 退钱 + 流水一个事务，余额回到预扣前。
func TestSettleHold_RefundIsAtomic(t *testing.T) {
	conn := openIdentityTempDB(t)
	seedHoldUser(t, conn, 7, 100)
	svc := NewCreditService(NewCreditStore(conn))

	hold, err := svc.PreConsumeHold(context.Background(), HoldRequest{
		BillingID: "req-atomic", UserID: 7, Amount: 30, SessionID: "s1", Remark: "预扣",
	})
	if err != nil {
		t.Fatalf("预扣失败: %v", err)
	}
	if got := holdBalance(t, conn, 7); got != 70 {
		t.Fatalf("预扣后余额应为 70，实际 %v", got)
	}

	owned, err := svc.SettleHold(context.Background(), SettleHoldRequest{
		BillingID: hold.BillingID, UserID: 7, Delta: -30, Status: HoldRefunded,
		SessionID: "s1", Remark: "请求失败退回预扣（test）",
	})
	if err != nil {
		t.Fatalf("SettleHold 失败: %v", err)
	}
	if !owned {
		t.Fatalf("首次终结应返回 owned=true")
	}
	if got := holdBalance(t, conn, 7); got != 100 {
		t.Fatalf("退款后余额应回到 100，实际 %v", got)
	}
	if st := holdStatus(t, svc, hold.BillingID).Status; st != HoldRefunded {
		t.Fatalf("登记状态应为 refunded，实际 %q", st)
	}
	// 流水必须落一条（报表按 remark 归集）。
	var flows int
	if err := conn.QueryRow(`SELECT count(*) FROM credit_flows WHERE user_id = 7 AND type = 'adjust'`).Scan(&flows); err != nil {
		t.Fatalf("读流水失败: %v", err)
	}
	if flows != 1 {
		t.Fatalf("应落 1 条 adjust 流水，实际 %d", flows)
	}
}

// TestSettleHold_NoDoubleRefund 重复终结（并发/重试）不会二次退钱 —— 这是双份预扣的直接防线。
func TestSettleHold_NoDoubleRefund(t *testing.T) {
	conn := openIdentityTempDB(t)
	seedHoldUser(t, conn, 7, 100)
	svc := NewCreditService(NewCreditStore(conn))

	hold, err := svc.PreConsumeHold(context.Background(), HoldRequest{
		BillingID: "req-dup", UserID: 7, Amount: 25,
	})
	if err != nil {
		t.Fatalf("预扣失败: %v", err)
	}

	for i := 0; i < 2; i++ {
		owned, err := svc.SettleHold(context.Background(), SettleHoldRequest{
			BillingID: hold.BillingID, UserID: 7, Delta: -25, Status: HoldRefunded,
			Remark: "请求失败退回预扣（dup）",
		})
		if err != nil {
			t.Fatalf("第 %d 次 SettleHold 失败: %v", i+1, err)
		}
		if want := i == 0; owned != want {
			t.Fatalf("第 %d 次 owned=%v，want %v", i+1, owned, want)
		}
	}

	if got := holdBalance(t, conn, 7); got != 100 {
		t.Fatalf("余额只应回到 100（不许退两次），实际 %v", got)
	}
	var flows int
	if err := conn.QueryRow(`SELECT count(*) FROM credit_flows WHERE user_id = 7`).Scan(&flows); err != nil {
		t.Fatalf("读流水失败: %v", err)
	}
	// 1 条 consume（预扣）+ 1 条 adjust（退款）。
	if flows != 2 {
		t.Fatalf("流水应为 2 条（预扣+退款），实际 %d —— 多出来就是重复退款", flows)
	}
}

// TestExpireHolds_SkipsSettledHold 已结算的登记过期扫描必须跳过。
//
// 这正是 Owl 指出的第二种病：账单已结算、hold 却停在 held → 过期把整笔预扣退回（白送）。
// 现在成功路径由 billing 结算事务顺带 settle，所以这里直接验「settled 之后扫不到」。
func TestExpireHolds_SkipsSettledHold(t *testing.T) {
	conn := openIdentityTempDB(t)
	seedHoldUser(t, conn, 7, 100)
	svc := NewCreditService(NewCreditStore(conn))

	hold, err := svc.PreConsumeHold(context.Background(), HoldRequest{
		BillingID: "req-settled", UserID: 7, Amount: 40, TTL: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("预扣失败: %v", err)
	}
	if owned, err := svc.SettleHold(context.Background(), SettleHoldRequest{
		BillingID: hold.BillingID, UserID: 7, Delta: 0, Status: HoldSettled,
	}); err != nil || !owned {
		t.Fatalf("结算终结失败: owned=%v err=%v", owned, err)
	}

	time.Sleep(5 * time.Millisecond)
	res, err := svc.ExpireHolds(context.Background(), time.Now(), 100)
	if err != nil {
		t.Fatalf("过期扫描失败: %v", err)
	}
	if res.Expired != 0 {
		t.Fatalf("已结算的登记不许过期退回，实际退了 %d 笔", res.Expired)
	}
	if got := holdBalance(t, conn, 7); got != 60 {
		t.Fatalf("余额应保持 60（预扣已作为消费结算），实际 %v", got)
	}
	if st := holdStatus(t, svc, hold.BillingID).Status; st != HoldSettled {
		t.Fatalf("登记状态应保持 settled，实际 %q", st)
	}
}

// TestSettleHold_ExpiryWinsRace 过期扫描先到时，随后的退款终结必须拿不到终结权（不动钱）。
func TestSettleHold_ExpiryWinsRace(t *testing.T) {
	conn := openIdentityTempDB(t)
	seedHoldUser(t, conn, 7, 50)
	svc := NewCreditService(NewCreditStore(conn))

	hold, err := svc.PreConsumeHold(context.Background(), HoldRequest{
		BillingID: "req-race", UserID: 7, Amount: 20, TTL: time.Millisecond,
	})
	if err != nil {
		t.Fatalf("预扣失败: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	res, err := svc.ExpireHolds(context.Background(), time.Now(), 100)
	if err != nil || res.Expired != 1 {
		t.Fatalf("过期扫描应退回 1 笔：res=%+v err=%v", res, err)
	}
	if got := holdBalance(t, conn, 7); got != 50 {
		t.Fatalf("过期退回后余额应为 50，实际 %v", got)
	}

	owned, err := svc.SettleHold(context.Background(), SettleHoldRequest{
		BillingID: hold.BillingID, UserID: 7, Delta: -20, Status: HoldRefunded,
		Remark: "请求失败退回预扣（race）",
	})
	if err != nil {
		t.Fatalf("SettleHold 不应报错（应静默不持权）: %v", err)
	}
	if owned {
		t.Fatalf("过期已终结，退款不该拿到终结权")
	}
	if got := holdBalance(t, conn, 7); got != 50 {
		t.Fatalf("不许二次退款，余额应仍为 50，实际 %v", got)
	}
}
