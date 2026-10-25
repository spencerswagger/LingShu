package billing

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestListRecords_FilterByKeyAndSession 断言 ListRecords 按渠道密钥 / 会话维度过滤：
// WHERE 使用 channel_key_id=$2、session_id=$3，参数顺序随索引平移。
func TestListRecords_FilterByKeyAndSession(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := NewSqlStore(db)
	keyID := int64(3)
	f := RecordFilter{ChannelKeyID: &keyID, SessionID: "sess-1", Page: 1, Size: 10}

	mock.ExpectQuery(`SELECT count\(\*\) FROM billing_records .*channel_key_id = \$2.*token_id = \$3.*session_id = \$4.*`).
		WithArgs(nil, int64(3), nil, "sess-1", "", "", "", nil, nil).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))
	mock.ExpectQuery(`SELECT .* FROM billing_records .*channel_key_id = \$2.*token_id = \$3.*session_id = \$4.*ORDER BY id DESC LIMIT \$10 OFFSET \$11`).
		WithArgs(nil, int64(3), nil, "sess-1", "", "", "", nil, nil, 10, 0).
		WillReturnRows(completedRow(t))

	list, total, err := store.ListRecords(context.Background(), f)
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if total != 1 {
		t.Fatalf("total=%d, want 1", total)
	}
	if len(list) != 1 || list[0].ChannelKeyID != 1 {
		t.Fatalf("list=%+v, want 1 row with channel_key_id=1", list)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestListRecords_FilterByModelMatchesAutoName 断言 Model 过滤同时匹配
// external_model_name 与 auto_model_name（Auto 账单按 Auto 名可搜）。
func TestListRecords_FilterByModelMatchesAutoName(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := NewSqlStore(db)
	f := RecordFilter{Model: "auto", Page: 1, Size: 10}

	mock.ExpectQuery(`SELECT count\(\*\) FROM billing_records .*external_model_name = \$5 OR auto_model_name = \$5.*`).
		WithArgs(nil, nil, nil, "", "auto", "", "", nil, nil).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))
	mock.ExpectQuery(`SELECT .* FROM billing_records .*external_model_name = \$5 OR auto_model_name = \$5.*LIMIT \$10 OFFSET \$11`).
		WithArgs(nil, nil, nil, "", "auto", "", "", nil, nil, 10, 0).
		WillReturnRows(completedRow(t))

	_, total, err := store.ListRecords(context.Background(), f)
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if total != 1 {
		t.Fatalf("total=%d, want 1", total)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestListRecords_NoFilter 断言零值过滤器仍可用：参数位全部占位且不报错。
func TestListRecords_NoFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := NewSqlStore(db)
	mock.ExpectQuery(`SELECT count\(\*\) FROM billing_records .*channel_key_id = \$2`).
		WithArgs(nil, nil, nil, "", "", "", "", nil, nil).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectQuery(`SELECT .* FROM billing_records .*LIMIT \$10 OFFSET \$11`).
		WithArgs(nil, nil, nil, "", "", "", "", nil, nil, 20, 0).
		WillReturnRows(sqlmock.NewRows(recordColsForTest))

	list, total, err := store.ListRecords(context.Background(), RecordFilter{})
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if total != 0 || len(list) != 0 {
		t.Fatalf("total=%d len=%d, want 0/0", total, len(list))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestGetByBillingID_ScanDims 断言 GetByBillingID 能扫描带 channel_key_id/session_id 的行。
func TestGetByBillingID_ScanDims(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := NewSqlStore(db)
	mock.ExpectQuery(regexp.MustCompile(`SELECT .* FROM billing_records WHERE billing_id=\$1`).String()).
		WithArgs(billingID).
		WillReturnRows(sqlmock.NewRows(recordColsForTest).AddRow(
			int64(5), billingID, int64(7), "sale", nil, "ext-model", nil, "int-model", int64(1), "sess-x", "会话A",
			time.Now(), `{}`, `{}`, []byte(`{"time":1,"context":1}`), int64(10000),
			nil, nil, nil, nil, nil, "completed", nil, nil, nil,
		))

	rec, err := store.GetByBillingID(context.Background(), billingID)
	if err != nil {
		t.Fatalf("GetByBillingID: %v", err)
	}
	if rec.ChannelKeyID != 1 || rec.SessionID != "sess-x" {
		t.Fatalf("扫到维度错误: channel_key_id=%d session_id=%q", rec.ChannelKeyID, rec.SessionID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestSummarize_BusyOnlyCountsCompletedWithDuration 断言忙时口径只统计「成功且带耗时」的行：
//   - SQL 里 Busy* 的 FILTER 必须带 status='completed'（用 ExpectQuery 正则锁定，丢掉条件即失败）；
//   - 返回值上 Requests 覆盖全部行（含失败），而 BusyRequests/BusyRPM 只按成功行计算，
//     证明「失败哪怕带耗时也不计入忙时」。
func TestSummarize_BusyOnlyCountsCompletedWithDuration(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := NewSqlStore(db)
	// 正则同时锁定：count(*) FILTER 与 sum(duration_ms) FILTER 都带 completed 条件。
	mock.ExpectQuery(regexp.QuoteMeta(`status = 'completed' AND duration_ms IS NOT NULL`)).
		WillReturnRows(sqlmock.NewRows([]string{
			"requests", "credits", "tokens", "duration", "input", "cache_read",
			"busyrequests", "busytokens", "busydur",
		}).AddRow(int64(10), float64(1.25), int64(900), int64(30000), int64(300), int64(100),
			int64(7), int64(700), int64(60000)))

	out, err := store.Summarize(context.Background(), RecordFilter{})
	if err != nil {
		t.Fatalf("Summarize: %v", err)
	}
	// 总量口径不受 status 影响（10 条全部计数）。
	if out.Requests != 10 || out.TokensTotal != 900 || out.DurationTotalMS != 30000 {
		t.Fatalf("totals=%d/%d/%d, want 10/900/30000", out.Requests, out.TokensTotal, out.DurationTotalMS)
	}
	// 忙时口径只算 7 条成功请求、60000ms（=1 分钟）→ 7 RPM、700 TPM。
	if out.BusyRequests != 7 || out.BusyTokens != 700 || out.BusyDurationMS != 60000 {
		t.Fatalf("busy=%d/%d/%d, want 7/700/60000", out.BusyRequests, out.BusyTokens, out.BusyDurationMS)
	}
	if out.BusyRPM != 7 || out.BusyTPM != 700 {
		t.Fatalf("busy_rpm=%v busy_tpm=%v, want 7/700", out.BusyRPM, out.BusyTPM)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestBusyRate_ZeroDenominator 断言忙时分母 ≤0 时速率为 0（不产生 Inf/NaN）。
func TestBusyRate_ZeroDenominator(t *testing.T) {
	if v := BusyRate(5, 0); v != 0 {
		t.Fatalf("BusyRate(5,0)=%v, want 0", v)
	}
	if v := BusyRate(5, -1); v != 0 {
		t.Fatalf("BusyRate(5,-1)=%v, want 0", v)
	}
	if v := BusyRate(5, 60000); v != 5 {
		t.Fatalf("BusyRate(5,60000)=%v, want 5", v)
	}
}
