package identity

import (
	"context"
	"database/sql"
	"regexp"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// TestCredit_Consume_ZeroBalanceGuard 表驱动覆盖扣减条件写的三类资金语义（错误码必须可区分且不变）：
//   - 钱包缺失 → 40401（钱包不存在）；
//   - 余额低于扣减额 → 40201（余额不足）；
//   - 余额为 0 且扣减为 0 → 仍被拒绝（零余额守卫，40201），不得放行。
func TestCredit_Consume_ZeroBalanceGuard(t *testing.T) {
	cases := []struct {
		name     string
		amount   float64
		setup    func(mock sqlmock.Sqlmock)
		wantCode int
	}{
		{
			name:   "钱包缺失且扣减为正→40401",
			amount: 10,
			setup: func(mock sqlmock.Sqlmock) {
				// 条件 UPDATE 未命中（钱包缺失）。
				mock.ExpectQuery(regexp.QuoteMeta(consumeSQL)).
					WithArgs(10.0, int64(7)).
					WillReturnRows(sqlmock.NewRows([]string{"balance"}))
				// 复核余额：钱包行不存在 → 40401。
				mock.ExpectQuery(regexp.QuoteMeta(getBalanceSQL)).
					WithArgs(int64(7)).
					WillReturnError(sql.ErrNoRows)
			},
			wantCode: resp.CodeNotFound,
		},
		{
			name:   "钱包缺失且扣减为 0→40401",
			amount: 0,
			setup: func(mock sqlmock.Sqlmock) {
				// 零金额扣减走零余额守卫：仅查余额。
				mock.ExpectQuery(regexp.QuoteMeta(getBalanceSQL)).
					WithArgs(int64(7)).
					WillReturnError(sql.ErrNoRows)
			},
			wantCode: resp.CodeNotFound,
		},
		{
			name:   "余额低于扣减额→40201",
			amount: 10,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(regexp.QuoteMeta(consumeSQL)).
					WithArgs(10.0, int64(7)).
					WillReturnRows(sqlmock.NewRows([]string{"balance"}))
				mock.ExpectQuery(regexp.QuoteMeta(getBalanceSQL)).
					WithArgs(int64(7)).
					WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(5.0))
			},
			wantCode: resp.CodeInsufficient,
		},
		{
			name:   "余额为 0 且扣减为 0→40201（零余额守卫拦截）",
			amount: 0,
			setup: func(mock sqlmock.Sqlmock) {
				mock.ExpectQuery(regexp.QuoteMeta(getBalanceSQL)).
					WithArgs(int64(7)).
					WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(0.0))
			},
			wantCode: resp.CodeInsufficient,
		},
		{
			name:     "扣减为负→40001（不触达数据库）",
			amount:   -1,
			setup:    func(mock sqlmock.Sqlmock) {},
			wantCode: resp.CodeBadRequest,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()

			mock.ExpectBegin()
			tc.setup(mock)
			mock.ExpectRollback()

			svc := NewCreditService(NewCreditStore(db))
			_, _, err = svc.Consume(context.Background(), 7, tc.amount, "bill-zero", "", "")
			var apiErr *APIError
			if !asAPIError(err, &apiErr) || apiErr.Code != tc.wantCode {
				t.Fatalf("want code %d, got %v", tc.wantCode, err)
			}
			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("mock expectations: %v", err)
			}
		})
	}
}

// TestCredit_Consume_ZeroAmount_PositiveBalanceAllowed 零金额扣减在余额 >0 时放行：
// 不写流水、不递增 version，返回前后余额相等（供网关把「估算为 0」纳入同一预扣语义）。
func TestCredit_Consume_ZeroAmount_PositiveBalanceAllowed(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(getBalanceSQL)).
		WithArgs(int64(7)).
		WillReturnRows(sqlmock.NewRows([]string{"balance"}).AddRow(100.0))
	mock.ExpectCommit()

	svc := NewCreditService(NewCreditStore(db))
	before, after, err := svc.Consume(context.Background(), 7, 0, "bill-zero", "", "")
	if err != nil {
		t.Fatalf("consume zero amount: %v", err)
	}
	if before != 100.0 || after != 100.0 {
		t.Fatalf("zero-amount consume must be a no-op, got before=%v after=%v", before, after)
	}
	// ExpectationsWereMet 同时断言「未写流水 / 未执行条件 UPDATE」。
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}
