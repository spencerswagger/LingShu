package identity

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// 以下 SQL 与 credit_hold.go 逐字对应（sqlmock 的默认匹配器会折叠空白）。
const (
	insertHoldSQL = `INSERT INTO credit_holds(id, billing_id, user_id, token_id, amount, status, expires_at)
		 VALUES ($1, $2, $3, $4, $5, 'held', $6)`

	closeHoldSQL = `UPDATE credit_holds
		    SET status = $2,
		        ledger_entry_id = COALESCE(NULLIF($3, ''), ledger_entry_id),
		        closed_at = now()
		  WHERE billing_id = $1 AND status = 'held'`

	expireSelectSQL = `SELECT billing_id, user_id, amount
		   FROM credit_holds
		  WHERE status = 'held' AND expires_at <= $1
		  ORDER BY expires_at
		  LIMIT $2
		  FOR UPDATE SKIP LOCKED`

	expireClaimSQL = `UPDATE credit_holds SET status = 'expired', closed_at = now()
			  WHERE billing_id = $1 AND status = 'held'`

	addBalanceSQL = `UPDATE credit_wallets SET balance = balance + $1, version = version + 1, updated_at = now()
	      WHERE user_id = $2`
)

// TestHold_PreConsumeHold_Atomic：登记与扣款同事务，提交后才算成功。
func TestHold_PreConsumeHold_Atomic(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(insertHoldSQL)).
		WithArgs(sqlmock.AnyArg(), "req-1", int64(7), int64(42), 3.5, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// 扣款：before=13.5，扣 3.5 → after=10。
	mock.ExpectQuery(regexp.QuoteMeta(consumeSQL)).
		WithArgs(3.5, int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(10.0))
	mock.ExpectQuery(regexp.QuoteMeta(insertFlowSQL)).
		WithArgs(sqlmock.AnyArg(), int64(7), FlowTypeConsume, -3.5, "req-1", "sess-1", "预扣费").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), time.Now()))
	mock.ExpectCommit()

	svc := NewCreditService(NewCreditStore(db))
	tid := int64(42)
	hold, err := svc.PreConsumeHold(context.Background(), HoldRequest{
		BillingID: "req-1", UserID: 7, TokenID: &tid, Amount: 3.5,
		SessionID: "sess-1", Remark: "预扣费",
	})
	if err != nil {
		t.Fatalf("预扣登记：%v", err)
	}
	if hold.Status != HoldHeld || hold.Amount != 3.5 {
		t.Fatalf("登记形状不符：%+v", hold)
	}
	if !hold.ExpiresAt.After(time.Now().Add(DefaultHoldTTL - time.Minute)) {
		t.Fatalf("到期时间应约为 now+15min，got %v", hold.ExpiresAt)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock 期望未满足：%v", err)
	}
}

// TestHold_PreConsumeHold_Insufficient_RollsBack：余额不足整体回滚 —— 不留 hold（否则过期扫描会退出一笔没扣过的钱）。
func TestHold_PreConsumeHold_Insufficient_RollsBack(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(insertHoldSQL)).
		WithArgs(sqlmock.AnyArg(), "req-2", int64(7), int64(0), 9.9, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	// 条件扣减没命中 → 再查余额区分「钱包缺失 / 余额不足」。
	mock.ExpectQuery(regexp.QuoteMeta(consumeSQL)).
		WithArgs(9.9, int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}))
	mock.ExpectQuery(regexp.QuoteMeta(getBalanceSQL)).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(1.0))
	mock.ExpectRollback()

	svc := NewCreditService(NewCreditStore(db))
	zero := int64(0)
	_, err = svc.PreConsumeHold(context.Background(), HoldRequest{
		BillingID: "req-2", UserID: 7, TokenID: &zero, Amount: 9.9,
	})
	if err == nil {
		t.Fatal("余额不足应返回错误")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != 402 {
		t.Fatalf("应为 402（积分不足），got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock 期望未满足（提交不应发生）：%v", err)
	}
}

// TestHold_CloseHold_Idempotent：只有 held 能被终结；第二次终结拿到 false 而不是错误。
func TestHold_CloseHold_Idempotent(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectExec(regexp.QuoteMeta(closeHoldSQL)).
		WithArgs("req-3", HoldSettled, "").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(closeHoldSQL)).
		WithArgs("req-3", HoldSettled, "").
		WillReturnResult(sqlmock.NewResult(0, 0))

	svc := NewCreditService(NewCreditStore(db))
	closed, err := svc.CloseHold(context.Background(), "req-3", HoldSettled, "")
	if err != nil || !closed {
		t.Fatalf("首次终结应成功：closed=%v err=%v", closed, err)
	}
	closed, err = svc.CloseHold(context.Background(), "req-3", HoldSettled, "")
	if err != nil {
		t.Fatalf("重复终结不该报错：%v", err)
	}
	if closed {
		t.Fatal("重复终结应返回 false（已被终结过）")
	}
}

// TestHold_CloseHold_RejectsBadStatus：只允许 settled / refunded。
func TestHold_CloseHold_RejectsBadStatus(t *testing.T) {
	svc := NewCreditService(NewCreditStore(nil))
	if _, err := svc.CloseHold(context.Background(), "req-4", HoldHeld, ""); err == nil {
		t.Fatal("held 不是合法终结状态，应报错")
	}
}

// TestHold_ExpireHolds_RefundsAndCloses：一条过期 → 终结为 expired + 余额退回 + adjust 流水。
func TestHold_ExpireHolds_RefundsAndCloses(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(expireSelectSQL)).
		WithArgs(now, 200).
		WillReturnRows(sqlmock.NewRows([]string{"billing_id", "user_id", "amount"}).AddRow("req-5", int64(7), 2.5))
	mock.ExpectExec(regexp.QuoteMeta(expireClaimSQL)).
		WithArgs("req-5").
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(addBalanceSQL)).
		WithArgs(2.5, int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(insertFlowSQL)).
		WithArgs(sqlmock.AnyArg(), int64(7), FlowTypeAdjust, 2.5, nil, nil, RetryRemarkExpired).
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), time.Now()))
	mock.ExpectCommit()

	svc := NewCreditService(NewCreditStore(db))
	res, err := svc.ExpireHolds(context.Background(), now, 0)
	if err != nil {
		t.Fatalf("过期扫描：%v", err)
	}
	if res.Expired != 1 || res.Amount != 2.5 {
		t.Fatalf("退回合计不符：%+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock 期望未满足：%v", err)
	}
}

// TestHold_ExpireHolds_SkipsAlreadyClosed：条件 UPDATE 命中 0 行 = 已被结算/退款，绝不重复退钱。
func TestHold_ExpireHolds_SkipsAlreadyClosed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	now := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(expireSelectSQL)).
		WithArgs(now, 200).
		WillReturnRows(sqlmock.NewRows([]string{"billing_id", "user_id", "amount"}).AddRow("req-6", int64(7), 4.0))
	mock.ExpectExec(regexp.QuoteMeta(expireClaimSQL)).
		WithArgs("req-6").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	svc := NewCreditService(NewCreditStore(db))
	res, err := svc.ExpireHolds(context.Background(), now, 0)
	if err != nil {
		t.Fatalf("过期扫描：%v", err)
	}
	if res.Expired != 0 || res.Amount != 0 {
		t.Fatalf("已终结的预扣不许重复退：%+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock 期望未满足（不该有退余额/流水）：%v", err)
	}
}
