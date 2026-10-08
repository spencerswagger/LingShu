package identity

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// consumeSQL 与 credit.go 中 ConsumeCredit 保持一致（匹配时空白会被折叠）。
const consumeSQL = `UPDATE credit_wallets SET balance = balance - $1, version = version + 1, updated_at = now()
		 WHERE user_id = $2 AND balance >= $1
		 RETURNING balance`

const getBalanceSQL = `SELECT balance FROM credit_wallets WHERE user_id = $1`

const insertFlowSQL = `INSERT INTO credit_flows(id, user_id, type, amount, ref_billing_id, session_id, remark)
		 VALUES($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, created_at`

func TestCredit_Consume_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	// 扣减前的 balance=13.5，扣 3.5 → after=10。
	mock.ExpectQuery(regexp.QuoteMeta(consumeSQL)).
		WithArgs(3.5, int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(10.0))
	// consume 流水：amount 为负；携带会话。
	mock.ExpectQuery(regexp.QuoteMeta(insertFlowSQL)).
		WithArgs(sqlmock.AnyArg(), int64(7), FlowTypeConsume, -3.5, "bill-1", "sess-1", "测试消耗").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(1), time.Now()))
	mock.ExpectCommit()

	svc := NewCreditService(NewCreditStore(db))
	before, after, err := svc.Consume(context.Background(), 7, 3.5, "bill-1", "sess-1", "测试消耗")
	if err != nil {
		t.Fatalf("consume: %v", err)
	}
	if after != 10.0 {
		t.Fatalf("after=%v, want 10.0", after)
	}
	if before != 13.5 {
		t.Fatalf("before=%v, want 13.5 (=after+amount)", before)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestCredit_Consume_Insufficient(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	amount := 10.0
	mock.ExpectBegin()
	// UPDATE 影响 0 行（余额不足，未命中 WHERE）。
	mock.ExpectQuery(regexp.QuoteMeta(consumeSQL)).
		WithArgs(amount, int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}))
	// 复核余额：钱包存在但只有 5，不足。
	mock.ExpectQuery(regexp.QuoteMeta(getBalanceSQL)).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(5.0))
	mock.ExpectRollback()

	svc := NewCreditService(NewCreditStore(db))
	_, _, err = svc.Consume(context.Background(), 7, amount, "bill-2", "", "")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != resp.CodeInsufficient {
		t.Fatalf("expected 40201, got %v", err)
	}
	if !strings.Contains(apiErr.Message, "10") {
		t.Fatalf("message should mention needed amount, got %q", apiErr.Message)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestCredit_Consume_WalletMissing(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(consumeSQL)).
		WithArgs(10.0, int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}))
	// 钱包行不存在。
	mock.ExpectQuery(regexp.QuoteMeta(getBalanceSQL)).
		WithArgs(int64(7)).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectRollback()

	svc := NewCreditService(NewCreditStore(db))
	_, _, err = svc.Consume(context.Background(), 7, 10.0, "bill-3", "", "")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != resp.CodeNotFound {
		t.Fatalf("expected 40401, got %v", err)
	}
}

func TestCredit_Recharge_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	ensureSQL := `INSERT INTO credit_wallets(user_id, balance) VALUES ($1, 0)
		 ON CONFLICT (user_id) DO NOTHING`
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(ensureSQL)).
		WithArgs(int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))

	changeSQL := `UPDATE credit_wallets SET balance = balance + $1, version = version + 1, updated_at = now()
		 WHERE user_id = $2`
	mock.ExpectExec(regexp.QuoteMeta(changeSQL)).
		WithArgs(100.0, int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))

	// recharge 流水：ref_billing_id、session_id 为 nil/空；operatorID=1 写入备注前缀。
	mock.ExpectQuery(regexp.QuoteMeta(insertFlowSQL)).
		WithArgs(sqlmock.AnyArg(), int64(7), FlowTypeRecharge, 100.0, nil, nil, "操作员#1 初始充值").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(2), time.Now()))
	mock.ExpectCommit()

	svc := NewCreditService(NewCreditStore(db))
	if err := svc.Recharge(context.Background(), 7, 1, 100.0, "初始充值"); err != nil {
		t.Fatalf("recharge: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestCredit_Consume_UsesAtomicGuard(t *testing.T) {
	// 并发安全的证明在于这条单语句 UPDATE 的 WHERE balance >= $1 原子性：
	// 即使余额字段“陈旧”，也绝不会扣成负数。sqlmock 不适合测真实并发，
	// 这里改为断言原子语义（balance 扣减与余额门槛在同一个 UPDATE 内）。
	if !strings.Contains(consumeSQL, "balance = balance - $1") {
		t.Fatal("consume must decrement balance atomically")
	}
	if !strings.Contains(consumeSQL, "balance >= $1") {
		t.Fatal("consume must guard balance >= amount in the same statement")
	}
	if strings.Contains(consumeSQL, ";") || strings.Contains(consumeSQL, "BEGIN") {
		t.Fatal("consume must be a single atomic statement, no explicit transaction")
	}
}

func TestCredit_Recharge_InvalidAmount(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	svc := NewCreditService(NewCreditStore(db))
	err = svc.Recharge(context.Background(), 7, 1, -5.0, "")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != resp.CodeBadRequest {
		t.Fatalf("expected 40001, got %v", err)
	}
}

// TestCredit_Adjust_Negative_Success 调减余额（负数调整）成功路径：回归「ChangeBalance 用 $3 传正数门槛」，
// 修复 PostgreSQL “operator is not unique: - unknown” (42725) —— SQL 中不得对未定型参数做一元负号。
func TestCredit_Adjust_Negative_Success(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	ensureSQL := `INSERT INTO credit_wallets(user_id, balance) VALUES ($1, 0)
		 ON CONFLICT (user_id) DO NOTHING`
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(ensureSQL)).
		WithArgs(int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))

	changeSQL := `UPDATE credit_wallets SET balance = balance + $1, version = version + 1, updated_at = now()
		 WHERE user_id = $2 AND balance >= $3`
	mock.ExpectExec(regexp.QuoteMeta(changeSQL)).
		WithArgs(-50.0, int64(7), 50.0).WillReturnResult(sqlmock.NewResult(0, 1))

	mock.ExpectQuery(regexp.QuoteMeta(insertFlowSQL)).
		WithArgs(sqlmock.AnyArg(), int64(7), FlowTypeAdjust, -50.0, nil, nil, "操作员#1 测试调减").
		WillReturnRows(sqlmock.NewRows([]string{"id", "created_at"}).AddRow(int64(3), time.Now()))
	mock.ExpectCommit()

	svc := NewCreditService(NewCreditStore(db))
	if err := svc.Adjust(context.Background(), 7, 1, -50.0, "测试调减"); err != nil {
		t.Fatalf("adjust: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestCredit_Adjust_Negative_Insufficient 调减时余额不足 → 40201（不写流水）。
func TestCredit_Adjust_Negative_Insufficient(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	ensureSQL := `INSERT INTO credit_wallets(user_id, balance) VALUES ($1, 0)
		 ON CONFLICT (user_id) DO NOTHING`
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(ensureSQL)).
		WithArgs(int64(7)).WillReturnResult(sqlmock.NewResult(0, 1))

	changeSQL := `UPDATE credit_wallets SET balance = balance + $1, version = version + 1, updated_at = now()
		 WHERE user_id = $2 AND balance >= $3`
	mock.ExpectExec(regexp.QuoteMeta(changeSQL)).
		WithArgs(-100.0, int64(7), 100.0).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	svc := NewCreditService(NewCreditStore(db))
	err = svc.Adjust(context.Background(), 7, 1, -100.0, "")
	var apiErr *APIError
	if !asAPIError(err, &apiErr) || apiErr.Code != resp.CodeInsufficient {
		t.Fatalf("expected 40201, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}
