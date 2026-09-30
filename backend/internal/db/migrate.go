package db

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrate 应用未执行的迁移文件（按文件名排序），每文件一个事务，失败回滚。
// 返回本次实际应用（新执行）的迁移数量。
func Migrate(ctx context.Context, d *sql.DB) (int, error) {
	// 1) 建变更追踪表
	if _, err := d.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS applied_migrations (
    name       TEXT PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
)`); err != nil {
		return 0, fmt.Errorf("create applied_migrations: %w", err)
	}

	// 2) 读取内嵌迁移文件，按字典序排序
	entries, err := fs.ReadDir(migrationsFS, "migrations")
	if err != nil {
		return 0, fmt.Errorf("read migrations dir: %w", err)
	}
	var files []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		files = append(files, e.Name())
	}
	sort.Strings(files)

	// 已应用集合
	applied := map[string]bool{}
	rows, err := d.QueryContext(ctx, `SELECT name FROM applied_migrations`)
	if err != nil {
		return 0, fmt.Errorf("query applied_migrations: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return 0, err
		}
		applied[name] = true
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}

	// 3) 逐文件应用：每文件一个事务
	appliedCount := 0
	for _, name := range files {
		if applied[name] {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return appliedCount, fmt.Errorf("read migration %s: %w", name, err)
		}
		tx, err := d.BeginTx(ctx, nil)
		if err != nil {
			return appliedCount, fmt.Errorf("begin tx for %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(body)); err != nil {
			_ = tx.Rollback()
			return appliedCount, fmt.Errorf("exec migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO applied_migrations(name) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback()
			return appliedCount, fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return appliedCount, fmt.Errorf("commit migration %s: %w", name, err)
		}
		appliedCount++
	}
	return appliedCount, nil
}
