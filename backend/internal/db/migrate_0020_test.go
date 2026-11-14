// migrate_0020_test.go 验证 0020_reconcile_blocked_status.sql：
//  1. 全链应用后 reconcile_windows.status 接受 `blocked`（契约 §8.2 的不可对账状态），
//     仍拒绝未知状态；
//  2. 重复执行幂等（DROP CONSTRAINT IF EXISTS + ADD CONSTRAINT）。
package db

import (
	"path/filepath"
	"testing"
)

func TestMigrate_0020_ReconcileBlockedStatus(t *testing.T) {
	tempDB := openTempDB(t)
	migDir := filepath.Join(filepath.Dir(mustCallerFile(t)), "migrations")
	for _, n := range migrationNames(t, migDir) {
		runMigration(t, tempDB, migDir, n)
	}

	nextID := int64(1000)
	insertWindow := func(status string) error {
		nextID++
		_, err := tempDB.Exec(
			`INSERT INTO reconcile_windows(id, window_start, window_end, status)
			 VALUES ($1::bigint, now() - make_interval(secs => $1::int), now(), $2::text)`,
			nextID, status)
		return err
	}

	if err := insertWindow("blocked"); err != nil {
		t.Fatalf("status=blocked 应被接受（0020 未落地？）: %v", err)
	}
	for _, ok := range []string{"ok", "alarm", "error", "skipped"} {
		if err := insertWindow(ok); err != nil {
			t.Fatalf("status=%s 应仍被接受: %v", ok, err)
		}
	}
	if err := insertWindow("bogus"); err == nil {
		t.Fatal("未知状态必须仍被 CHECK 拒绝")
	}

	// 幂等：重放 0020 不报错，且约束仍然生效。
	name := migrationFile(t, migDir, "0020")
	runMigration(t, tempDB, migDir, name)
	if err := insertWindow("blocked"); err != nil {
		t.Fatalf("重放后 status=blocked 仍应被接受: %v", err)
	}
	if err := insertWindow("bogus2"); err == nil {
		t.Fatal("重放后未知状态仍必须被拒绝")
	}
}
