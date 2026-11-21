// poller_legacy_test.go 锁 v3billing 旧名反查兜底（#171 并入 main 新结算链后的适配版）：
//  1. caller_key_name 是新名 → 直接命中（不触发 legacy 查询）；
//  2. caller_key_name 是改名窗口内的旧名快照 → key_name 查不到、按 legacy_key_name 兜底命中 + WARN；
//  3. 两步都查不到 → 单条失败（下轮重试），不影响整轮其它行。
//
// 变异点：删掉 legacy 兜底查询 → 用例 2 红；查询谓词漂移（模糊 LIKE）→ 用例 1/2 红。
package v3billing

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"regexp"
	"strings"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
)

// newSettlePoller 构造一个只跑 settleOne 的 Poller（sqlmock db + fakeRec 计费）。
func newSettlePoller(t *testing.T) (*Poller, sqlmock.Sqlmock, *fakeRec, *strings.Builder) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	buf := &strings.Builder{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	rec := &fakeRec{}
	p := NewPoller(db, nil, nil, log)
	p.bill = rec
	return p, mock, rec, buf
}

const tokenByNameSQL = `SELECT k.token_id, t.user_id FROM magpie_keys k JOIN tokens t ON t.id = k.token_id
		 WHERE k.key_name = $1 AND k.state = 'ACTIVE' LIMIT 1`

const tokenByLegacySQL = `SELECT k.token_id, t.user_id FROM magpie_keys k JOIN tokens t ON t.id = k.token_id
			 WHERE k.legacy_key_name = $1 AND k.state = 'ACTIVE' LIMIT 1`

const costRatesSQL = `SELECT COALESCE(cost_rates,'{}'::jsonb) FROM internal_models WHERE ref = $1`

func tokenRow() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"token_id", "user_id"}).AddRow(int64(7), int64(12))
}

// expectPriceMisses 装配候选查询全未命中（reqModel=model="m"）+ 成本查询 + 账单补写 UPDATE。
func expectPriceMisses(t *testing.T, mock sqlmock.Sqlmock) {
	t.Helper()
	mock.ExpectQuery(regexp.QuoteMeta(costRatesSQL)).WithArgs("p/m").
		WillReturnRows(sqlmock.NewRows([]string{"cost_rates"}).AddRow([]byte("{}")))
	mock.ExpectQuery(regexp.QuoteMeta(extByNameSQL)).WithArgs("m").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(extByBindingSQL)).WithArgs("group/m").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(extByBindingSQL)).WithArgs("auto/m").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(extByBindingSQL)).WithArgs("group/auto-m").WillReturnError(sql.ErrNoRows)
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE billing_records SET magpie_request_id = $1`)).WillReturnResult(sqlmock.NewResult(1, 1))
}

// TestSettleOne_NewNameDirectHit 新名直接命中，不触发 legacy 查询。
func TestSettleOne_NewNameDirectHit(t *testing.T) {
	p, mock, rec, buf := newSettlePoller(t)
	r := row{ledgerID: "lg-1", callerKey: "ls-u12-t7-alpha", reqModel: "m", provider: "p", model: "m"}

	mock.ExpectQuery(regexp.QuoteMeta(tokenByNameSQL)).WithArgs("ls-u12-t7-alpha").WillReturnRows(tokenRow())
	expectPriceMisses(t, mock)

	settled, unpriced, err := p.settleOne(context.Background(), r, nil)
	if err != nil {
		t.Fatalf("settleOne: %v", err)
	}
	if !settled || !unpriced {
		t.Fatalf("settled=%v unpriced=%v，想要 true/true（候选全未命中落 unpriced）", settled, unpriced)
	}
	if n := rec.callCount(); n != 1 {
		t.Fatalf("Record 应被调 1 次，got %d", n)
	}
	if req := rec.reqs[0]; req.Status != billing.StatusUnpriced {
		t.Fatalf("账单状态 = %q，想要 unpriced", req.Status)
	}
	if strings.Contains(buf.String(), "账本行按旧名结算") {
		t.Fatalf("新名命中不该触发旧名兜底 WARN，日志：%s", buf.String())
	}
}

// TestSettleOne_LegacyNameFallback 旧名快照：key_name 查不到 → legacy_key_name 兜底命中。
func TestSettleOne_LegacyNameFallback(t *testing.T) {
	p, mock, rec, buf := newSettlePoller(t)
	r := row{ledgerID: "lg-2", callerKey: "ls-u12-t7-old", reqModel: "m", provider: "p", model: "m"}

	mock.ExpectQuery(regexp.QuoteMeta(tokenByNameSQL)).WithArgs("ls-u12-t7-old").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(tokenByLegacySQL)).WithArgs("ls-u12-t7-old").WillReturnRows(tokenRow())
	expectPriceMisses(t, mock)

	settled, unpriced, err := p.settleOne(context.Background(), r, nil)
	if err != nil {
		t.Fatalf("settleOne: %v", err)
	}
	if !settled || !unpriced {
		t.Fatalf("settled=%v unpriced=%v，想要 true/true", settled, unpriced)
	}
	if n := rec.callCount(); n != 1 {
		t.Fatalf("Record 应被调 1 次，got %d", n)
	}
	if !strings.Contains(buf.String(), "账本行按旧名结算") {
		t.Fatalf("旧名命中必须打 WARN，日志：%s", buf.String())
	}
}

// TestSettleOne_UnknownNameFails 两步都查不到 → 单条失败，不触发计费。
func TestSettleOne_UnknownNameFails(t *testing.T) {
	p, mock, rec, _ := newSettlePoller(t)
	r := row{ledgerID: "lg-3", callerKey: "ls-ghost", reqModel: "m", provider: "p", model: "m"}

	mock.ExpectQuery(regexp.QuoteMeta(tokenByNameSQL)).WithArgs("ls-ghost").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(tokenByLegacySQL)).WithArgs("ls-ghost").WillReturnError(sql.ErrNoRows)

	_, _, err := p.settleOne(context.Background(), r, nil)
	if err == nil || !strings.Contains(err.Error(), "ls-ghost") {
		t.Fatalf("想要带 callerKey 的失败错误，got %v", err)
	}
	if n := rec.callCount(); n != 0 {
		t.Fatalf("失败行不得触发计费，Record 被调 %d 次", n)
	}
	if !errors.Is(err, err) { // 占位保持 errors import 语义清晰
		t.Fatalf("unreachable")
	}
}
