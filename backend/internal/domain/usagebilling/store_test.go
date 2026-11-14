package usagebilling

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

var (
	localGroupsSQL   = regexp.MustCompile(`SELECT magpie_key_name, internal_model_id, estimated,`)
	callerKeysSQL    = regexp.MustCompile(`SELECT key_name FROM magpie_keys`)
	prevStreakSQL    = regexp.MustCompile(`SELECT alarm_streak FROM reconcile_windows`)
	saveWindowSQL    = regexp.MustCompile(`INSERT INTO reconcile_windows`)
	advisoryLockSQL  = regexp.MustCompile(`SELECT pg_try_advisory_lock`)
	advisoryUnlockSQ = regexp.MustCompile(`SELECT pg_advisory_unlock`)
)

func newMockStore(t *testing.T) (*SQLReconcileStore, sqlmock.Sqlmock, *sql.DB) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	return NewSQLReconcileStore(db), mock, db
}

// 本地聚合必须只取 completed + magpie_key_name 非空，并把 estimated 标记带回。
func TestSQLReconcileStore_LocalGroups(t *testing.T) {
	store, mock, db := newMockStore(t)
	defer db.Close()

	mock.ExpectQuery(localGroupsSQL.String()).WithArgs(wStart, wEnd).
		WillReturnRows(sqlmock.NewRows([]string{"key", "model", "estimated", "reqs", "in", "cache", "out"}).
			AddRow("k1", "m1", false, int64(3), int64(30), int64(3), int64(9)).
			AddRow("k1", "m1", true, int64(1), int64(7), int64(0), int64(2)))

	gs, err := store.LocalGroups(context.Background(), wStart, wEnd)
	if err != nil {
		t.Fatalf("LocalGroups: %v", err)
	}
	if len(gs) != 2 {
		t.Fatalf("分组数=%d", len(gs))
	}
	if gs[0].Requests != 3 || gs[0].Input != 30 || gs[0].Output != 9 || gs[0].Estimated {
		t.Fatalf("主口径分组=%+v", gs[0])
	}
	if !gs[1].Estimated {
		t.Fatalf("第二条应为 estimated: %+v", gs[1])
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
}

func TestSQLReconcileStore_CallerKeys(t *testing.T) {
	store, mock, db := newMockStore(t)
	defer db.Close()
	mock.ExpectQuery(callerKeysSQL.String()).
		WillReturnRows(sqlmock.NewRows([]string{"key_name"}).AddRow("k1").AddRow("k2"))
	keys, err := store.CallerKeys(context.Background())
	if err != nil {
		t.Fatalf("CallerKeys: %v", err)
	}
	if len(keys) != 2 || keys[1] != "k2" {
		t.Fatalf("keys=%v", keys)
	}
}

// 没有历史窗口时 PreviousStreak 返回 0（首窗不因缺表报错）。
func TestSQLReconcileStore_PreviousStreakNoRows(t *testing.T) {
	store, mock, db := newMockStore(t)
	defer db.Close()
	mock.ExpectQuery(prevStreakSQL.String()).WillReturnRows(sqlmock.NewRows([]string{"alarm_streak"}))
	streak, err := store.PreviousStreak(context.Background())
	if err != nil || streak != 0 {
		t.Fatalf("streak=%d err=%v", streak, err)
	}
	mock2 := mock
	mock2.ExpectQuery(prevStreakSQL.String()).
		WillReturnRows(sqlmock.NewRows([]string{"alarm_streak"}).AddRow(2))
	if streak, err = store.PreviousStreak(context.Background()); err != nil || streak != 2 {
		t.Fatalf("streak=%d err=%v", streak, err)
	}
}

// SaveWindow 用 ON CONFLICT 幂等写（同一窗口重复跑只更新，不新增行）。
func TestSQLReconcileStore_SaveWindowUpserts(t *testing.T) {
	store, mock, db := newMockStore(t)
	defer db.Close()

	mock.ExpectExec(saveWindowSQL.String()).WithArgs(
		sqlmock.AnyArg(), // id
		wStart, wEnd,
		int64(5), int64(6),
		sqlmock.AnyArg(), // tokens_ls
		sqlmock.AnyArg(), // tokens_magpie
		sqlmock.AnyArg(), // dev_pct
		"alarm", 3, sqlmock.AnyArg(),
	).WillReturnResult(sqlmock.NewResult(1, 1))

	dev := 3.5
	w := Window{WindowStart: wStart, WindowEnd: wEnd, ReqCountLS: 5, ReqCountMag: 6,
		DevPct: &dev, Status: "alarm", AlarmStreak: 3, Detail: map[string]any{"alarm_groups": []any{}}}
	if err := store.SaveWindow(context.Background(), w); err != nil {
		t.Fatalf("SaveWindow: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
}

// advisory lock：拿不到锁返回 ok=false（多副本只让一个实例跑）。
// 取锁/解锁必须在同一连接（sqlmock 层只验证语句形态；真 PG 行为见 reconcile_lock_pg_test.go）。
func TestSQLReconcileStore_AdvisoryLock(t *testing.T) {
	store, mock, db := newMockStore(t)
	defer db.Close()

	mock.ExpectQuery(advisoryLockSQL.String()).WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(true))
	// 解锁改成走 QueryRow（要读回 unlocked 布尔值，确认真的解掉了）。
	mock.ExpectQuery(advisoryUnlockSQ.String()).WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"unlocked"}).AddRow(true))

	release, ok := store.TryAdvisoryLock(context.Background())
	if !ok {
		t.Fatalf("应拿到锁")
	}
	release()

	mock.ExpectQuery(advisoryLockSQL.String()).WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"locked"}).AddRow(false))
	if _, ok := store.TryAdvisoryLock(context.Background()); ok {
		t.Fatalf("锁被占用时应返回 false")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
}

// 窗口区间与状态可读（日志用）。
func TestWindow_String(t *testing.T) {
	dev := 1.25
	w := Window{WindowStart: wStart, WindowEnd: wEnd, Status: "alarm", AlarmStreak: 2,
		ReqCountLS: 3, ReqCountMag: 4, DevPct: &dev}
	s := w.String()
	for _, want := range []string{"status=alarm", "streak=2", "req=3/4", "dev=1.25%"} {
		if !containsStr(s, want) {
			t.Fatalf("String()=%q 缺 %q", s, want)
		}
	}
	_ = time.Now
}

func containsStr(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
