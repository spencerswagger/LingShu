package billing

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestUsageSplitTotal 断言三段互斥直接求和：
// 输入为纯输入、不含缓存读，Total = Input + CacheRead + Output。
func TestUsageSplitTotal(t *testing.T) {
	u := UsageSplit{Input: 100, Output: 50, CacheRead: 30}
	if got := u.Total(); got != 180 {
		t.Fatalf("Total()=%d, want 180（100+30+50）", got)
	}
}

// TestDashboard_Aggregates 断言 Dashboard 按固定顺序执行 9 条聚合查询，
// 并正确计算总量 / 成功率 / 吞吐量，同时按天补零。
func TestDashboard_Aggregates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := NewSqlStore(db)

	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 3, 23, 59, 59, 0, time.UTC)
	limit := 10

	// 1) 总计：input/output/cache_read（三段互斥求和）→ credits, calls, failed, sumdur, sumfirst, okdur, okfirst
	// 末列 busytokens = duration_ms 非空行的 token 合计（忙时口径的分子，与分母 sumdur 同源）。
	mock.ExpectQuery(regexp.QuoteMeta(`COALESCE(sum((first_token_ms IS NOT NULL)::int),0)`)).
		WillReturnRows(sqlmock.NewRows([]string{
			"input", "output", "cache_read",
			"credits", "calls", "failed", "sumdur", "sumfirst", "okdur", "okfirst", "busytokens",
		}).AddRow(int64(100), int64(50), int64(20),
			float64(12.5), int64(10), int64(1), int64(2000), int64(400), int64(10), int64(10), int64(170)))

	// 2) 按天（仅返回 1 天，另一天应补零）
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('day', call_time AT TIME ZONE 'Asia/Shanghai')::date`)).
		WillReturnRows(sqlmock.NewRows([]string{"d", "calls", "failed", "credits", "tokens", "duration"}).
			AddRow(from, int64(10), int64(1), float64(12.5), int64(150), int64(2000)))

	// 3) 按模型
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT external_model_name,`)).
		WillReturnRows(sqlmock.NewRows([]string{"model", "calls", "failed", "credits", "tokens"}).
			AddRow("gpt-4o", int64(10), int64(1), float64(12.5), int64(150)))

	// 4) 按渠道密钥
	mock.ExpectQuery(regexp.QuoteMeta(`JOIN channel_keys ck ON ck.id = br.channel_key_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"key", "channel", "calls", "failed", "credits", "tokens"}).
			AddRow("主密钥", "openai", int64(10), int64(1), float64(12.5), int64(150)))

	// 5) 按用户（userID=nil → 执行）
	mock.ExpectQuery(regexp.QuoteMeta(`JOIN users u ON u.id = br.user_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"username", "nickname", "calls", "failed", "credits", "tokens"}).
			AddRow("dev", "开发者", int64(10), int64(1), float64(12.5), int64(150)))

	// 6) 计费模式
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT pricing_mode,`)).
		WillReturnRows(sqlmock.NewRows([]string{"mode", "calls", "failed", "credits", "tokens"}).
			AddRow("sale", int64(10), int64(1), float64(12.5), int64(150)))

	// 7) 错误聚合
	mock.ExpectQuery(regexp.QuoteMeta(`'(未记录)'`)).
		WillReturnRows(sqlmock.NewRows([]string{"msg", "c"}).AddRow("超时", int64(1)))

	// 8) 延迟分桶
	mock.ExpectQuery(regexp.QuoteMeta(`'<100ms'`)).
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "n"}).AddRow("1-3s", int64(10)))

	// 9) 百分位
	mock.ExpectQuery(regexp.QuoteMeta(`percentile_disc(0.5)`)).
		WillReturnRows(sqlmock.NewRows([]string{"p50", "p90", "p95"}).AddRow(float64(200), float64(400), float64(500)))

	out, err := store.Dashboard(context.Background(), from, to, nil, limit)
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}
	if out.TotalCall != 10 || out.Failed != 1 {
		t.Fatalf("calls=%d failed=%d, want 10/1", out.TotalCall, out.Failed)
	}
	// 三段互斥：Total = input(100) + cache_read(20) + output(50) = 170。
	if out.Total.Total() != 170 {
		t.Fatalf("tokens=%d, want 170", out.Total.Total())
	}
	if out.Success != 0.9 {
		t.Fatalf("success=%v, want 0.9", out.Success)
	}
	if out.AvgMS != 200 {
		t.Fatalf("avg_ms=%v, want 200", out.AvgMS)
	}
	// 忙时口径：分母 = sumdur(2000ms) / 60000 = 0.03333 分钟 → 10 请求 = 300 RPM，170 token = 5100 TPM。
	if out.SumDurationMS != 2000 {
		t.Fatalf("sum_duration_ms=%d, want 2000", out.SumDurationMS)
	}
	if out.BusyRequests != 10 { // = okdur（有耗时的请求数），与 RPM/TPM 的分母同源
		t.Fatalf("busy_requests=%d, want 10", out.BusyRequests)
	}
	if out.BusyRPM != 300 || out.BusyTPM != 5100 {
		t.Fatalf("busy_rpm=%v busy_tpm=%v, want 300/5100", out.BusyRPM, out.BusyTPM)
	}
	if out.Days != 3 || len(out.Daily) != 3 {
		t.Fatalf("days=%d daily=%d, want 3/3（含补零）", out.Days, len(out.Daily))
	}
	if len(out.ByModel) != 1 || out.ByModel[0].Label != "gpt-4o" {
		t.Fatalf("by_model=%+v", out.ByModel)
	}
	if len(out.Latency) != 6 {
		t.Fatalf("latency buckets=%d, want 6（含零桶）", len(out.Latency))
	}
	if out.P50MS != 200 || out.P95MS != 500 {
		t.Fatalf("p50=%v p95=%v", out.P50MS, out.P95MS)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestDashboard_UserScope 断言传入 userID 时按用户过滤（$1 非空），且不执行按用户聚合。
func TestDashboard_UserScope(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := NewSqlStore(db)

	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 1, 23, 59, 59, 0, time.UTC)
	uid := int64(7)

	mock.ExpectQuery(regexp.QuoteMeta(`COALESCE(sum((first_token_ms IS NOT NULL)::int),0)`)).
		WithArgs(uid, from, to).
		WillReturnRows(sqlmock.NewRows([]string{
			"input", "output", "cache_read",
			"credits", "calls", "failed", "sumdur", "sumfirst", "okdur", "okfirst", "busytokens",
		}).AddRow(int64(0), int64(0), int64(0),
			float64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)))

	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('day', call_time AT TIME ZONE 'Asia/Shanghai')::date`)).
		WillReturnRows(sqlmock.NewRows([]string{"d", "calls", "failed", "credits", "tokens", "duration"}))

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT external_model_name,`)).
		WillReturnRows(sqlmock.NewRows([]string{"model", "calls", "failed", "credits", "tokens"}))
	mock.ExpectQuery(regexp.QuoteMeta(`JOIN channel_keys ck ON ck.id = br.channel_key_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"key", "channel", "calls", "failed", "credits", "tokens"}))
	// 无按用户查询（userID != nil）
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT pricing_mode,`)).
		WillReturnRows(sqlmock.NewRows([]string{"mode", "calls", "failed", "credits", "tokens"}))
	mock.ExpectQuery(regexp.QuoteMeta(`'(未记录)'`)).
		WillReturnRows(sqlmock.NewRows([]string{"msg", "c"}))
	mock.ExpectQuery(regexp.QuoteMeta(`'<100ms'`)).
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "n"}))
	mock.ExpectQuery(regexp.QuoteMeta(`percentile_disc(0.5)`)).
		WillReturnRows(sqlmock.NewRows([]string{"p50", "p90", "p95"}).AddRow(float64(0), float64(0), float64(0)))

	out, err := store.Dashboard(context.Background(), from, to, &uid, 10)
	if err != nil {
		t.Fatalf("Dashboard: %v", err)
	}
	if out.ByUser != nil {
		t.Fatalf("user scope 不应返回 by_user: %+v", out.ByUser)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestRangeTotals 断言环比上期只查总量。
func TestRangeTotals(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := NewSqlStore(db)
	from := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 8, 31, 23, 59, 59, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*),`)).
		WillReturnRows(sqlmock.NewRows([]string{"calls", "credits", "tokens"}).
			AddRow(int64(5), float64(3.25), int64(80)))

	calls, credits, tokens, err := store.RangeTotals(context.Background(), from, to, nil)
	if err != nil {
		t.Fatalf("RangeTotals: %v", err)
	}
	if calls != 5 || credits != 3.25 || tokens != 80 {
		t.Fatalf("got %d/%v/%d", calls, credits, tokens)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}
