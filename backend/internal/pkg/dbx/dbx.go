// Package dbx 提供数据库执行载体（*sql.DB / *sql.Tx）的公共接口，
// 让同一段 SQL 逻辑既能在非事务下执行，也能在调用方事务内执行，
// 避免为「事务版」复制一份几乎相同的代码。
package dbx

import (
	"context"
	"database/sql"
)

// Execer 抽象数据库执行器：*sql.DB 与 *sql.Tx 均满足该接口。
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// TxBeginner 抽象事务开启者，*sql.DB 满足。
type TxBeginner interface {
	BeginTx(ctx context.Context, opts *sql.TxOptions) (*sql.Tx, error)
}
