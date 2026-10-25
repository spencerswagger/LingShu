package billing

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestStartOfCNDay 断言日界按北京时间取，而不是按请求时间自带时区。
// 2026-10-25T18:00Z = 北京时间 2026-10-26 02:00，归属日应为 10-26。
func TestStartOfCNDay(t *testing.T) {
	utc := time.Date(2026, 10, 25, 18, 0, 0, 0, time.UTC)
	got := startOfCNDay(utc)
	if want := time.Date(2026, 10, 26, 0, 0, 0, 0, cnLocation()); !got.Equal(want) {
		t.Fatalf("startOfCNDay=%v, want %v（北京日界）", got, want)
	}
}

// TestCompareLabel 断言同期对比标签：前四天用相对词，更早回落 MM-DD。
func TestCompareLabel(t *testing.T) {
	day := time.Date(2026, 10, 21, 0, 0, 0, 0, time.UTC)
	cases := map[int]string{0: "今天", 1: "昨天", 2: "前天", 3: "大前天", 4: "10-21", 9: "10-21"}
	for offset, want := range cases {
		if got := compareLabel(offset, day); got != want {
			t.Fatalf("compareLabel(%d)=%q, want %q", offset, got, want)
		}
	}
}

// TestNormalizeModelSort 断言排序键白名单：非法值回落到 tokens（不报错，老调用方无感）。
func TestNormalizeModelSort(t *testing.T) {
	cases := map[string]string{
		"":        ModelSortTokens,
		"tokens":  ModelSortTokens,
		"tpm":     ModelSortTPM,
		"failed":  ModelSortFailed,
		"latency": ModelSortLatency,
		"bogus":   ModelSortTokens,
		"TOKENS":  ModelSortTokens, // 大小写敏感：非白名单一律回落
	}
	for in, want := range cases {
		if got := NormalizeModelSort(in); got != want {
			t.Fatalf("NormalizeModelSort(%q)=%q, want %q", in, got, want)
		}
	}
}

// TestDashboardHourly_Buckets 断言小时桶：北京时间墙钟、HourOfDay 对齐、缺桶不补零。
func TestDashboardHourly_Buckets(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := NewSqlStore(db)

	from := time.Date(2026, 10, 25, 1, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 25, 5, 0, 0, 0, time.UTC)

	// 只有 14:00 与 16:00 两个桶有数据（15:00 缺口必须保持缺失）。
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('hour', call_time AT TIME ZONE 'Asia/Shanghai')`)).
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "calls", "failed", "credits", "tokens", "duration"}).
			AddRow(time.Date(2026, 10, 25, 14, 0, 0, 0, time.UTC), int64(10), int64(2), float64(3.5), int64(150), int64(2000)).
			AddRow(time.Date(2026, 10, 25, 16, 0, 0, 0, time.UTC), int64(4), int64(0), float64(1.25), int64(60), int64(800)))

	out, err := store.DashboardHourly(context.Background(), from, to, nil)
	if err != nil {
		t.Fatalf("DashboardHourly: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("buckets=%d, want 2（缺桶不补零）", len(out))
	}
	if out[0].Hour != "2026-10-25T14:00" || out[0].Day != "2026-10-25" || out[0].HourOfDay != 14 {
		t.Fatalf("首个桶字段错: %+v", out[0])
	}
	if out[1].HourOfDay != 16 {
		t.Fatalf("第二桶 HourOfDay=%d, want 16", out[1].HourOfDay)
	}
	if out[0].SuccessRate != 0.8 {
		t.Fatalf("首桶成功率=%v, want 0.8", out[0].SuccessRate)
	}
	// 15:00 必须缺席（缺失 = 断线，不是 0）。
	for _, h := range out {
		if h.HourOfDay == 15 {
			t.Fatalf("15:00 桶不应存在")
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未满足的期望: %v", err)
	}
}

// TestDashboardHourly_EmptyWindow 断言空窗口返回空切片（不是 nil，序列化后不是 null）。
func TestDashboardHourly_EmptyWindow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := NewSqlStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('hour', call_time AT TIME ZONE 'Asia/Shanghai')`)).
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "calls", "failed", "credits", "tokens", "duration"}))

	out, err := store.DashboardHourly(context.Background(), time.Now().Add(-time.Hour), time.Now(), nil)
	if err != nil {
		t.Fatalf("DashboardHourly: %v", err)
	}
	if out == nil {
		t.Fatalf("空窗口应返回空切片而不是 nil")
	}
	if len(out) != 0 {
		t.Fatalf("空窗口桶数=%d, want 0", len(out))
	}
}

// TestDashboardCompare_FourSeriesAndGaps 断言四日同期对比：
// 序列顺序（今天→大前天）、Offset/Label、同小时对齐、缺失小时不出行。
func TestDashboardCompare_FourSeriesAndGaps(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := NewSqlStore(db)

	// 「今天」= 北京时间 2026-10-25。
	to := time.Date(2026, 10, 25, 15, 30, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta(`EXTRACT(hour FROM call_time AT TIME ZONE 'Asia/Shanghai')::int`)).
		WillReturnRows(sqlmock.NewRows([]string{"day", "hour", "calls", "failed", "credits", "tokens", "duration"}).
			// 今天：09、10 点（11 点缺）
			AddRow(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), 9, int64(2), int64(0), float64(1), int64(20), int64(100)).
			AddRow(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), 10, int64(3), int64(1), float64(2), int64(30), int64(200)).
			// 昨天：只有 09 点
			AddRow(time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC), 9, int64(5), int64(0), float64(4), int64(50), int64(300)).
			// 大前天（10-22）：13 点
			AddRow(time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC), 13, int64(1), int64(0), float64(1), int64(5), int64(10)))

	out, err := store.DashboardCompare(context.Background(), to, nil, 4)
	if err != nil {
		t.Fatalf("DashboardCompare: %v", err)
	}
	if len(out) != 4 {
		t.Fatalf("series=%d, want 4", len(out))
	}
	wantDays := []string{"2026-10-25", "2026-10-24", "2026-10-23", "2026-10-22"}
	wantLabels := []string{"今天", "昨天", "前天", "大前天"}
	for i := range out {
		if out[i].Day != wantDays[i] || out[i].Label != wantLabels[i] || out[i].Offset != i {
			t.Fatalf("第 %d 条序列元信息错: %+v", i, out[i])
		}
		if out[i].Points == nil {
			t.Fatalf("第 %d 条序列 Points 不应为 nil", i)
		}
	}
	// 今天：09、10 两点，按 HourOfDay 升序，且 11 点缺失。
	if len(out[0].Points) != 2 {
		t.Fatalf("今天点数=%d, want 2（缺桶不出行）", len(out[0].Points))
	}
	if out[0].Points[0].HourOfDay != 9 || out[0].Points[1].HourOfDay != 10 {
		t.Fatalf("今天点位顺序错: %+v", out[0].Points)
	}
	if out[0].Points[1].Hour != "2026-10-25T10:00" || out[0].Points[1].Day != "2026-10-25" {
		t.Fatalf("今天 10 点字段错: %+v", out[0].Points[1])
	}
	// 昨天只有 1 点；前天没有数据但序列仍然存在（空 Points）。
	if len(out[1].Points) != 1 {
		t.Fatalf("昨天点数=%d, want 1", len(out[1].Points))
	}
	if len(out[2].Points) != 0 {
		t.Fatalf("前天点数=%d, want 0（无数据不等于补零）", len(out[2].Points))
	}
	if len(out[3].Points) != 1 || out[3].Points[0].HourOfDay != 13 {
		t.Fatalf("大前天点位错: %+v", out[3].Points)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未满足的期望: %v", err)
	}
}

// TestDashboardCompare_Disabled 断言 days<=0 时不做查询、直接返回空切片（老调用方不传即无额外开销）。
func TestDashboardCompare_Disabled(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := NewSqlStore(db)

	out, err := store.DashboardCompare(context.Background(), time.Now(), nil, 0)
	if err != nil {
		t.Fatalf("DashboardCompare: %v", err)
	}
	if out == nil || len(out) != 0 {
		t.Fatalf("days=0 应返回空切片，got %v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("不应发起查询: %v", err)
	}
}

// TestDashboardModels_SortZeroUsageAndTruncate 断言模型广场：
// 零用量模型也出行（Active=false）、失败率/TPM 计算、目录总数与截断标记。
func TestDashboardModels_SortZeroUsageAndTruncate(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := NewSqlStore(db)

	from := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 10, 25, 23, 59, 59, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta(`LEFT JOIN agg a ON a.model = em.external_name`)).
		WillReturnRows(sqlmock.NewRows([]string{"model", "enabled", "calls", "failed", "credits", "tokens",
			"busy_calls", "busy_tokens", "busy_ms", "avg_ms", "avg_first"}).
			// 活跃模型：10 次请求 1 次失败；忙时分母 60000ms → TPM = 300 / 1min = 300
			AddRow("gpt-4o", true, int64(10), int64(1), float64(12.5), int64(1000),
				int64(9), int64(300), int64(60000), float64(2000), float64(400)).
			// 空闲模型：零用量
			AddRow("claude-3", true, int64(0), int64(0), float64(0), int64(0),
				int64(0), int64(0), int64(0), float64(0), float64(0)))

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT (SELECT count(*) FROM external_models WHERE deleted_at IS NULL)`)).
		WillReturnRows(sqlmock.NewRows([]string{"total", "active"}).AddRow(int64(5), int64(1)))

	out, err := store.DashboardModels(context.Background(), from, to, nil, ModelSortTPM, 200)
	if err != nil {
		t.Fatalf("DashboardModels: %v", err)
	}
	if out.Sort != ModelSortTPM || out.Limit != MaxModelLimit {
		t.Fatalf("Limit 应被收敛到 %d，got %+v", MaxModelLimit, out)
	}
	if len(out.List) != 2 {
		t.Fatalf("行数=%d, want 2", len(out.List))
	}
	active := out.List[0]
	if !active.Active || active.TPM != 300 {
		t.Fatalf("活跃模型指标错: %+v", active)
	}
	if active.FailRate != 0.1 {
		t.Fatalf("失败率=%v, want 0.1", active.FailRate)
	}
	if active.Label != "gpt-4o" {
		t.Fatalf("Label=%q, want gpt-4o", active.Label)
	}
	idle := out.List[1]
	if idle.Active || idle.Calls != 0 || idle.TPM != 0 || idle.FailRate != 0 {
		t.Fatalf("空闲模型应零值: %+v", idle)
	}
	if out.Total != 5 || out.Active != 1 || !out.Truncated {
		t.Fatalf("目录统计错: Total=%d Active=%d Truncated=%v", out.Total, out.Active, out.Truncated)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未满足的期望: %v", err)
	}
}

// TestDashboardModels_DefaultSortAndLimit 断言缺省排序键 tokens、缺省上限 50。
func TestDashboardModels_DefaultSortAndLimit(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := NewSqlStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`LEFT JOIN agg a ON a.model = em.external_name`)).
		WillReturnRows(sqlmock.NewRows([]string{"model", "enabled", "calls", "failed", "credits", "tokens",
			"busy_calls", "busy_tokens", "busy_ms", "avg_ms", "avg_first"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT (SELECT count(*) FROM external_models WHERE deleted_at IS NULL)`)).
		WillReturnRows(sqlmock.NewRows([]string{"total", "active"}).AddRow(int64(0), int64(0)))

	out, err := store.DashboardModels(context.Background(), time.Now(), time.Now(), nil, "", 0)
	if err != nil {
		t.Fatalf("DashboardModels: %v", err)
	}
	if out.Sort != ModelSortTokens || out.Limit != 50 {
		t.Fatalf("缺省值错: sort=%q limit=%d", out.Sort, out.Limit)
	}
	if out.List == nil {
		t.Fatalf("List 不应为 nil")
	}
	if out.Truncated {
		t.Fatalf("空目录不应标记截断")
	}
}
