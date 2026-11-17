package upstream

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// saveCountsSQL 与 window_store.go 的 upsert 逐字对应（sqlmock 折叠空白）。
const saveCountsSQL = `INSERT INTO reconcile_windows AS w
		        (window_start, window_end, local_count, local_tokens,
		         upstream_count, upstream_tokens, status, alarm_streak, detail, checked_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9::jsonb, $10)
		 ON CONFLICT (window_start) DO UPDATE
		    SET window_end      = EXCLUDED.window_end,
		        local_count     = EXCLUDED.local_count,
		        local_tokens    = EXCLUDED.local_tokens,
		        upstream_count  = EXCLUDED.upstream_count,
		        upstream_tokens = EXCLUDED.upstream_tokens,
		        status          = EXCLUDED.status,
		        alarm_streak    = EXCLUDED.alarm_streak,
		        detail          = w.detail || EXCLUDED.detail,
		        checked_at      = EXCLUDED.checked_at
		  WHERE w.status NOT IN ('blocked','error')`

// TestSaveCounts_UpsertCreatesMissingWindow 是 P0 的回归锚点：
// 对账必须能在**同步轮次没建行**的窗口上写计数（以前 UPDATE-only，写不进去还静默）。
func TestSaveCounts_UpsertCreatesMissingWindow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	start := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta(saveCountsSQL)).
		WithArgs(start, start.Add(WindowSize), int64(2), int64(120), int64(2), int64(130),
			string(WindowOK), int16(0), sqlmock.AnyArg(), start.Add(time.Minute)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	store := NewSQLWindowStore(db)
	err = store.SaveCounts(context.Background(), WindowCounts{
		Start: start, End: start.Add(WindowSize),
		LocalCount: 2, LocalTokens: 120, UpstreamCount: 2, UpstreamTokens: 130,
		Status: WindowOK, CheckedAt: start.Add(time.Minute),
		Detail: map[string]any{"deviation_rate": 0.0},
	})
	if err != nil {
		t.Fatalf("没有窗口行时也必须能写进去：%v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock 期望未满足：%v", err)
	}
}

// TestSaveCounts_BlockedConflictRefused：行存在且被同步轮次持有（blocked/error）→ 0 行 → 哨兵错误。
func TestSaveCounts_BlockedConflictRefused(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	start := time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)
	mock.ExpectExec(regexp.QuoteMeta(saveCountsSQL)).
		WillReturnResult(sqlmock.NewResult(0, 0))

	store := NewSQLWindowStore(db)
	err = store.SaveCounts(context.Background(), WindowCounts{
		Start: start, End: start.Add(WindowSize), Status: WindowOK,
		LocalCount: 1, UpstreamCount: 1, CheckedAt: start.Add(time.Minute),
	})
	if !errors.Is(err, ErrWindowNotReconcilable) {
		t.Fatalf("blocked/error 窗口应拒绝对账覆盖，got %v", err)
	}
}

// TestSaveCounts_RealSQL_CreatesRowForReconcileOnlyWindow 用真迁移库验同一件事（无 DSN 时跳过）：
// 同步没跑过的时间段，对账照样把四列写进去，且 blocked 行仍受保护。
func TestSaveCounts_RealSQL_CreatesRowForReconcileOnlyWindow(t *testing.T) {
	conn := openMigratedTempDB(t)
	ctx := context.Background()
	store := NewSQLWindowStore(conn)

	start := time.Date(2026, 11, 16, 11, 0, 0, 0, time.UTC)
	if err := store.SaveCounts(ctx, WindowCounts{
		Start: start, End: start.Add(WindowSize),
		LocalCount: 5, LocalTokens: 500, UpstreamCount: 4, UpstreamTokens: 480,
		Status: WindowSkipped, CheckedAt: start.Add(time.Minute),
		Detail: map[string]any{"reason": "ledger_disabled"},
	}); err != nil {
		t.Fatalf("对账建行失败：%v", err)
	}
	var lc, oc, lt, ut int64
	var status string
	if err := conn.QueryRowContext(ctx,
		`SELECT local_count, upstream_count, local_tokens, upstream_tokens, status
		   FROM reconcile_windows WHERE window_start = $1`, start).
		Scan(&lc, &oc, &lt, &ut, &status); err != nil {
		t.Fatalf("读回计数失败：%v", err)
	}
	if lc != 5 || oc != 4 || lt != 500 || ut != 480 || status != string(WindowSkipped) {
		t.Fatalf("计数列不符：%d/%d/%d/%d status=%s", lc, oc, lt, ut, status)
	}

	// blocked 行仍不许覆盖。
	blockedStart := start.Add(WindowSize)
	if err := store.Save(ctx, WindowRecord{
		Start: blockedStart, End: blockedStart.Add(WindowSize), Status: WindowBlocked,
		Reason: "单轮上限未拉完", ScrollOffset: 4000, RecordedAt: blockedStart,
	}); err != nil {
		t.Fatalf("写 blocked 窗口失败：%v", err)
	}
	if err := store.SaveCounts(ctx, WindowCounts{
		Start: blockedStart, End: blockedStart.Add(WindowSize), Status: WindowOK,
		LocalCount: 1, UpstreamCount: 1, CheckedAt: blockedStart,
	}); !errors.Is(err, ErrWindowNotReconcilable) {
		t.Fatalf("blocked 窗口应拒绝覆盖，got %v", err)
	}
}

// TestLastSuccessEnd_IgnoresReconcileOnlyRows 锁定「水位线只认同步轮次写过的行」：
// 对账会自己建行（status=ok/skipped），若把它们算进水位线，同步就会跳过从未拉过的区间。
func TestLastSuccessEnd_IgnoresReconcileOnlyRows(t *testing.T) {
	conn := openMigratedTempDB(t)
	ctx := context.Background()
	store := NewSQLWindowStore(conn)

	syncStart := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, WindowRecord{
		Start: syncStart, End: syncStart.Add(WindowSize), Status: WindowOK,
		Pages: 1, Fetched: 3, Inserted: 3, RecordedAt: syncStart.Add(WindowSize),
	}); err != nil {
		t.Fatalf("写同步窗口失败：%v", err)
	}
	// 对账在不存在的窗口上建行（同步没跑过这一刻）。
	reconStart := syncStart.Add(WindowSize)
	if err := store.SaveCounts(ctx, WindowCounts{
		Start: reconStart, End: reconStart.Add(WindowSize),
		LocalCount: 0, LocalTokens: 0, UpstreamCount: 0, UpstreamTokens: 0,
		Status: WindowOK, CheckedAt: reconStart.Add(time.Minute),
	}); err != nil {
		t.Fatalf("对账建行失败：%v", err)
	}

	end, ok, err := store.LastSuccessEnd(ctx)
	if err != nil || !ok {
		t.Fatalf("应有水位线：ok=%v err=%v", ok, err)
	}
	if !end.Equal(syncStart.Add(WindowSize)) {
		t.Fatalf("水位线必须停在同步轮次写过的窗口（%s），got %s",
			syncStart.Add(WindowSize), end)
	}
}
