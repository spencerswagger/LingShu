// poller_legacy_test.go 锁 v3billing 旧名反查兜底：
//  1. caller_key_name 是新名 → 直接命中（不触发 legacy 查询）；
//  2. caller_key_name 是改名窗口内的旧名快照 → key_name 查不到、按 legacy_key_name 兜底命中 + WARN；
//  3. 两步都查不到 → 单条失败（下轮重试），不影响整轮其它行。
//
// 变异点：删掉 legacy 兜底查询 → 用例 2 红；查询谓词漂移（模糊 LIKE）→ 用例 1/2 红。
package v3billing

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
)

// settleOnlyPoller 构造一个只跑 settleOne 的 Poller（sqlmock db + nil bill 前置拦截）。
// bill 为 nil 时 settleOne 会在 Record 处 panic，因此 legacy 反查用例在 token 查询层断言；
// 「命中后继续结算」的行为由真库用例覆盖（门禁口径）。
func newSettlePoller(t *testing.T) (*Poller, sqlmock.Sqlmock, *strings.Builder) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	buf := &strings.Builder{}
	log := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return &Poller{db: db, log: log, interval: time.Hour, batch: 200}, mock, buf
}

const tokenByNameSQL = `SELECT k.token_id, t.user_id FROM magpie_keys k JOIN tokens t ON t.id = k.token_id
		 WHERE k.key_name = $1 AND k.state = 'ACTIVE' LIMIT 1`

const tokenByLegacySQL = `SELECT k.token_id, t.user_id FROM magpie_keys k JOIN tokens t ON t.id = k.token_id
			 WHERE k.legacy_key_name = $1 AND k.state = 'ACTIVE' LIMIT 1`

func tokenRow() *sqlmock.Rows {
	return sqlmock.NewRows([]string{"token_id", "user_id"}).AddRow(int64(7), int64(12))
}

// TestSettleOne_NewNameDirectHit 新名直接命中，不触发 legacy 查询（一次 token 查询即返回）。
func TestSettleOne_NewNameDirectHit(t *testing.T) {
	p, mock, _ := newSettlePoller(t)
	r := row{ledgerID: "lg-1", callerKey: "ls-u12-t7-alpha", reqModel: "m", provider: "p", model: "m"}

	mock.ExpectQuery(regexp.QuoteMeta(tokenByNameSQL)).WithArgs("ls-u12-t7-alpha").WillReturnRows(tokenRow())
	// 命中后走售价查询（external_models）→ 用不存在错误短路（本用例只验 token 反查路径）。
	mock.ExpectQuery(`SELECT sale_rates, COALESCE\(time_config,'null'::jsonb\), COALESCE\(context_tiers,'null'::jsonb\), COALESCE\(magpie_model,''\)`).
		WithArgs("m").WillReturnError(fmt.Errorf("short-circuit"))

	_, err := p.settleOne(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "short-circuit") {
		t.Fatalf("应推进到售价查询后短路：err=%v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("新名命中不应触发 legacy 查询：%v", err)
	}
}

// TestSettleOne_LegacyNameFallback 旧名快照：key_name 查不到 → legacy_key_name 兜底命中 + WARN。
func TestSettleOne_LegacyNameFallback(t *testing.T) {
	p, mock, buf := newSettlePoller(t)
	r := row{ledgerID: "lg-2", callerKey: "ls-token-7", reqModel: "m", provider: "p", model: "m"}

	mock.ExpectQuery(regexp.QuoteMeta(tokenByNameSQL)).WithArgs("ls-token-7").WillReturnError(fmt.Errorf("no rows"))
	mock.ExpectQuery(regexp.QuoteMeta(tokenByLegacySQL)).WithArgs("ls-token-7").WillReturnRows(tokenRow())
	mock.ExpectQuery(`SELECT sale_rates, COALESCE\(time_config,'null'::jsonb\), COALESCE\(context_tiers,'null'::jsonb\), COALESCE\(magpie_model,''\)`).
		WithArgs("m").WillReturnError(fmt.Errorf("short-circuit"))

	_, err := p.settleOne(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "short-circuit") {
		t.Fatalf("兜底命中后应继续推进到售价查询：err=%v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("应发出 legacy 兜底查询：%v", err)
	}
	if !strings.Contains(buf.String(), "账本行按旧名结算") {
		t.Fatalf("旧名命中必须打 WARN（可观测）：%s", buf.String())
	}
}

// TestSettleOne_UnknownNameFails 两步都查不到 → 明确错误（含原名），不 panic、不静默。
func TestSettleOne_UnknownNameFails(t *testing.T) {
	p, mock, _ := newSettlePoller(t)
	r := row{ledgerID: "lg-3", callerKey: "ls-ghost", reqModel: "m", provider: "p", model: "m"}

	mock.ExpectQuery(regexp.QuoteMeta(tokenByNameSQL)).WithArgs("ls-ghost").WillReturnError(fmt.Errorf("no rows"))
	mock.ExpectQuery(regexp.QuoteMeta(tokenByLegacySQL)).WithArgs("ls-ghost").WillReturnError(fmt.Errorf("no rows"))

	_, err := p.settleOne(context.Background(), r)
	if err == nil || !strings.Contains(err.Error(), "ls-ghost") {
		t.Fatalf("查不到应报错并带上 key 名：err=%v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
}

// 编译期断言：billing.Usage / billing.Rates 引用仍在（防包漂移导致用例空转）。
var _ = billing.Usage{}

// TestOnce_WarnSkipsUnknownKey 整轮口径：两段皆空（key_name 与 legacy_key_name 都查不到）
// → 该行只 Warn 跳过、整轮不炸、下轮重试（任务书：日志样例须含一次该形态）。
func TestOnce_WarnSkipsUnknownKey(t *testing.T) {
	p, mock, buf := newSettlePoller(t)

	// 一轮：一行未知 key 的账本行。
	mock.ExpectQuery(`SELECT e.ledger_id, e.occurred_at`).
		WithArgs(200).
		WillReturnRows(sqlmock.NewRows([]string{"ledger_id", "occurred_at", "provider", "model",
			"req_model", "caller_key_name", "tokens_in", "tokens_out", "cache_read", "cache_write", "cost_credits"}).
			AddRow("lg-warn", time.Now(), "prov", "m", "m", "ls-ghost", 10, 20, 0, 0, 0.1))
	mock.ExpectQuery(regexp.QuoteMeta(tokenByNameSQL)).WithArgs("ls-ghost").WillReturnError(fmt.Errorf("no rows"))
	mock.ExpectQuery(regexp.QuoteMeta(tokenByLegacySQL)).WithArgs("ls-ghost").WillReturnError(fmt.Errorf("no rows"))

	n, err := p.Once(context.Background())
	if err != nil {
		t.Fatalf("单行不可归属不应炸整轮：%v", err)
	}
	if n != 0 {
		t.Fatalf("不可归属行不应计 settled=%d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
	// 日志样例断言：WARN 行 + 原因含 key 名（运营可见）。
	if !strings.Contains(buf.String(), "单条结算失败（下轮重试）") || !strings.Contains(buf.String(), "ls-ghost") {
		t.Fatalf("Warn 日志应含跳过原因与 key 名：%s", buf.String())
	}
}
