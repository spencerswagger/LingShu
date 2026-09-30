package db

import (
	"database/sql"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// Open 打开 PostgreSQL 连接池（pgx stdlib 驱动，dsn 需为 PostgreSQL 连接串）。
func Open(dsn string) (*sql.DB, error) {
	d, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(20)
	d.SetMaxIdleConns(5)
	return d, nil
}
