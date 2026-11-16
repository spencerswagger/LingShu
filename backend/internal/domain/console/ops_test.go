package console

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
)

// TestHandlePnL_AggregatesRevenueAndCost：收入/成本/毛利/毛利率 + 成本来源拆分 + 未计成本计数。
func TestHandlePnL_AggregatesRevenueAndCost(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	from := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	to := from.Add(24 * time.Hour)

	// 两行分组：p1（收入 100、成本 30，其中账本 20 / 摊销 10，无未计成本）
	//             p2（收入 50、成本 0、未计成本 2 条）。
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(upstream_provider,'(unknown)') AS k")).
		WithArgs(from, to).
		WillReturnRows(sqlmock.NewRows([]string{
			"k", "count", "tokens", "revenue", "cost", "ledger_cost", "amortized_cost", "uncosted",
		}).
			AddRow("p1", int64(3), int64(300), 100.0, 30.0, 20.0, 10.0, int64(0)).
			AddRow("p2", int64(2), int64(200), 50.0, 0.0, 0.0, 0.0, int64(2)))

	admin := NewAdmin(db, billing.NewSqlStore(db), testUserIDFrom)
	admin.now = func() time.Time { return to }

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/ops/pnl?From="+from.Format(time.RFC3339)+"&To="+to.Format(time.RFC3339), nil)
	rec := httptest.NewRecorder()
	admin.HandlePnL(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，got %d body=%s", rec.Code, rec.Body.String())
	}

	var payload struct {
		Data pnlResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析响应失败：%v body=%s", err, rec.Body.String())
	}
	got := payload.Data
	if got.Revenue != 150 || got.Cost != 30 || got.Margin != 120 {
		t.Fatalf("合计不符：%+v", got)
	}
	if got.MarginPct == nil || *got.MarginPct != 0.8 {
		t.Fatalf("毛利率应为 0.8，got %+v", got.MarginPct)
	}
	if got.Uncosted != 2 {
		t.Fatalf("未计成本请求数应为 2（毛利此刻偏高，必须暴露），got %d", got.Uncosted)
	}
	if len(got.Groups) != 2 {
		t.Fatalf("应返回 2 组，got %+v", got.Groups)
	}
	g := got.Groups[0]
	if g.Key != "p1" || g.LedgerCost != 20 || g.AmortizedCost != 10 || g.Margin != 70 {
		t.Fatalf("分组口径不符：%+v", g)
	}
	if g.MarginPct == nil || *g.MarginPct != 0.7 {
		t.Fatalf("分组毛利率应为 0.7，got %+v", g.MarginPct)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock 期望未满足：%v", err)
	}
}

// TestHandlePnL_ZeroRevenueMarginNil：收入为 0 时毛利率返回 null（不是 0）。
func TestHandlePnL_ZeroRevenueMarginNil(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	from := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	to := from.Add(time.Hour)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT COALESCE(upstream_provider,'(unknown)') AS k")).
		WillReturnRows(sqlmock.NewRows([]string{
			"k", "count", "tokens", "revenue", "cost", "ledger_cost", "amortized_cost", "uncosted",
		}).AddRow("p1", int64(1), int64(10), 0.0, 5.0, 5.0, 0.0, int64(0)))

	admin := NewAdmin(db, billing.NewSqlStore(db), testUserIDFrom)
	admin.now = func() time.Time { return to }
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/ops/pnl?From="+from.Format(time.RFC3339)+"&To="+to.Format(time.RFC3339), nil)
	rec := httptest.NewRecorder()
	admin.HandlePnL(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，got %d", rec.Code)
	}
	var payload struct {
		Data pnlResponse `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if payload.Data.MarginPct != nil {
		t.Fatalf("收入为 0 时毛利率必须是 null，got %v", *payload.Data.MarginPct)
	}
	if payload.Data.Margin != -5 {
		t.Fatalf("毛利应为 -5，got %v", payload.Data.Margin)
	}
}

// TestHandlePnL_BadParams：分组非法 / 区间非法都 400。
func TestHandlePnL_BadParams(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), testUserIDFrom)
	admin.now = func() time.Time { return time.Date(2026, 11, 16, 0, 0, 0, 0, time.UTC) }

	cases := []string{
		"/api/v1/admin/ops/pnl?GroupBy=channnel",
		"/api/v1/admin/ops/pnl?From=not-a-time",
		"/api/v1/admin/ops/pnl?From=2026-11-16T00:00:00Z&To=2026-11-15T00:00:00Z",
		"/api/v1/admin/ops/pnl?From=2024-01-01T00:00:00Z&To=2026-11-16T00:00:00Z",
	}
	for _, target := range cases {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rec := httptest.NewRecorder()
		admin.HandlePnL(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，got %d body=%s", target, rec.Code, rec.Body.String())
		}
	}
}

// TestHandleReconcileWindows_NullCountsStayNull：计数列 null 必须原样透出（= 没算过，不是 0 笔）。
func TestHandleReconcileWindows_NullCountsStayNull(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	start := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("SELECT window_start, window_end, status, alarm_streak,")).
		WithArgs(start.Add(-time.Hour), start, 500).
		WillReturnRows(sqlmock.NewRows([]string{
			"window_start", "window_end", "status", "alarm_streak",
			"local_count", "upstream_count", "local_tokens", "upstream_tokens",
			"deviation_rate", "scroll_offset", "checked_at",
		}).
			AddRow(start, start.Add(time.Minute), "blocked", int16(0), nil, nil, nil, nil, nil, int64(4000), start).
			AddRow(start.Add(time.Minute), start.Add(2*time.Minute), "alarm", int16(2),
				int64(10), int64(9), int64(1000), int64(950), 0.05, int64(0), start))

	admin := NewAdmin(db, billing.NewSqlStore(db), testUserIDFrom)
	admin.now = func() time.Time { return start }
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/ops/reconcile-windows?From="+start.Add(-time.Hour).Format(time.RFC3339)+
			"&To="+start.Format(time.RFC3339), nil)
	rec := httptest.NewRecorder()
	admin.HandleReconcileWindows(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，got %d body=%s", rec.Code, rec.Body.String())
	}
	var payload struct {
		Data struct {
			Items []reconcileWindow `json:"Items"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if len(payload.Data.Items) != 2 {
		t.Fatalf("应返回 2 行，got %d", len(payload.Data.Items))
	}
	if payload.Data.Items[0].LocalCount != nil {
		t.Fatalf("blocked 窗口计数必须为 null，got %v", *payload.Data.Items[0].LocalCount)
	}
	if payload.Data.Items[1].Status != "alarm" || payload.Data.Items[1].DeviationRate == nil ||
		*payload.Data.Items[1].DeviationRate != 0.05 {
		t.Fatalf("偏差率应透出：%+v", payload.Data.Items[1])
	}
}
