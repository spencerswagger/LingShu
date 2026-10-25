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

	mock.ExpectQuery(regexp.QuoteMeta(`LEFT JOIN agg a ON a.ext_id = em.id`)).
		WillReturnRows(sqlmock.NewRows(modelRowCols()).
			// 活跃模型：10 次请求 1 次失败；忙时分母 60000ms → TPM = 300 / 1min = 300；
			// 耗时和 20000ms / 10 次 → AvgMS 2000；首字和 4000 / 10 → 400。
			AddRow("gpt-4o", true, int64(10), int64(1), float64(12.5), int64(1000),
				int64(9), int64(300), int64(60000), int64(20000), int64(10), int64(4000), int64(10)).
			// 空闲模型：零用量
			AddRow("claude-3", true, int64(0), int64(0), float64(0), int64(0),
				int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)))

	mock.ExpectQuery(regexp.QuoteMeta(`FROM agg a`)).
		WillReturnRows(sqlmock.NewRows(modelTotalsCols()).
			AddRow(int64(5), int64(1),
				int64(0), int64(0), float64(0), float64(0),
				int64(0), float64(0), int64(0), int64(0), int64(0), int64(0), int64(0)))

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

	mock.ExpectQuery(regexp.QuoteMeta(`LEFT JOIN agg a ON a.ext_id = em.id`)).
		WillReturnRows(sqlmock.NewRows(modelRowCols()))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM agg a`)).
		WillReturnRows(sqlmock.NewRows(modelTotalsCols()).
			AddRow(int64(0), int64(0),
				int64(0), int64(0), float64(0), float64(0),
				int64(0), float64(0), int64(0), int64(0), int64(0), int64(0), int64(0)))

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

// modelRowCols 模型广场列表查询的列（顺序与实现一致）。
func modelRowCols() []string {
	return []string{"model", "enabled", "calls", "failed", "credits", "tokens",
		"busy_calls", "busy_tokens", "busy_ms", "dur_sum", "dur_n", "first_sum", "first_n"}
}

// modelTotalsCols 模型广场总量查询的列（目录总数 / 活跃数 / 未匹配汇总）。
func modelTotalsCols() []string {
	return []string{"total", "active", "um_calls", "um_failed", "um_credits", "um_tokens",
		"um_busy_calls", "um_busy_tokens", "um_busy_ms", "um_dur_sum", "um_dur_n", "um_first_sum", "um_first_n"}
}

// TestDashboardModels_UnmatchedRowAppended 断言解析不到的用量不会被静默丢掉：
// 它作为显式一行（Unmatched=true）追加在列表末尾，且不参与 Total/Active。
func TestDashboardModels_UnmatchedRowAppended(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	store := NewSqlStore(db)

	mock.ExpectQuery(regexp.QuoteMeta(`LEFT JOIN agg a ON a.ext_id = em.id`)).
		WillReturnRows(sqlmock.NewRows(modelRowCols()).
			AddRow("gpt-4o", true, int64(10), int64(1), float64(12.5), int64(1000),
				int64(9), int64(300), int64(60000), int64(20000), int64(10), int64(4000), int64(10)))
	mock.ExpectQuery(regexp.QuoteMeta(`FROM agg a`)).
		WillReturnRows(sqlmock.NewRows(modelTotalsCols()).
			AddRow(int64(3), int64(1),
				int64(7), int64(2), float64(0.7), float64(70), // 未匹配：7 次请求 / 2 失败 / 70 token
				int64(5), float64(50), int64(10000), int64(9000), int64(5), int64(1000), int64(5)))

	out, err := store.DashboardModels(context.Background(), time.Now(), time.Now(), nil, ModelSortTokens, 50)
	if err != nil {
		t.Fatalf("DashboardModels: %v", err)
	}
	if len(out.List) != 2 {
		t.Fatalf("行数=%d，want 2（1 个目录行 + 1 个未匹配行）", len(out.List))
	}
	um := out.List[1]
	if !um.Unmatched || um.Model != DashModelUnmatchedKey || um.Label != "未匹配模型" {
		t.Fatalf("未匹配行标记错: %+v", um)
	}
	if um.Calls != 7 || um.Failed != 2 || um.Tokens != 70 || !um.Active {
		t.Fatalf("未匹配行用量错: %+v", um)
	}
	if um.FailRate != 2.0/7.0 {
		t.Fatalf("未匹配行失败率=%v", um.FailRate)
	}
	if um.TPM != 300 { // 50 token / (10000ms/60000) = 300
		t.Fatalf("未匹配行 TPM=%v，want 300", um.TPM)
	}
	if um.AvgMS != 1800 { // 9000/5
		t.Fatalf("未匹配行 AvgMS=%v，want 1800", um.AvgMS)
	}
	if um.AvgFirstMS != 200 { // 1000/5
		t.Fatalf("未匹配行 AvgFirstMS=%v，want 200", um.AvgFirstMS)
	}
	// 未匹配行不计入目录口径的 Total/Active。
	if out.Total != 3 || out.Active != 1 {
		t.Fatalf("目录口径错: Total=%d Active=%d", out.Total, out.Active)
	}
}

// TestFillModelStat_MergedGroups 断言合并分组（改名/别名归并）后的派生指标仍精确：
// 用「和 ÷ 次数」而不是 avg-of-avg。
func TestFillModelStat_MergedGroups(t *testing.T) {
	it := DashModelStat{Calls: 4, Failed: 1, BusyMS: 120000}
	fillModelStat(&it, 600, 8000, 4, 1200, 4)
	if it.FailRate != 0.25 {
		t.Fatalf("失败率=%v want 0.25", it.FailRate)
	}
	if it.TPM != 300 { // 600 token / 2 分钟
		t.Fatalf("TPM=%v want 300", it.TPM)
	}
	if it.AvgMS != 2000 || it.AvgFirstMS != 300 {
		t.Fatalf("均值=%v/%v want 2000/300", it.AvgMS, it.AvgFirstMS)
	}
	// 分母为 0 时保持 0（不产生 Inf/NaN）。
	zero := DashModelStat{}
	fillModelStat(&zero, 0, 0, 0, 0, 0)
	if zero.TPM != 0 || zero.AvgMS != 0 || zero.AvgFirstMS != 0 || zero.FailRate != 0 {
		t.Fatalf("零样本应全 0: %+v", zero)
	}
}
