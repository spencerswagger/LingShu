// Package audit 提供管理操作审计日志落库（audit_logs 表）。
package audit

import (
	"context"
	"database/sql"
	"expvar"
)

// InsertFailures 审计写入失败累计计数（expvar，可在 /debug/vars 或指标导出中观测）。
// 审计是事后追责的唯一依据，其失效必须比业务失效更早被发现——写入失败不能只留在日志里。
var InsertFailures = expvar.NewInt("audit_insert_failures")

// Entry 一条审计记录。Detail 可选，绝不含口令/密钥明文。
type Entry struct {
	UserID     int64
	Username   string
	Action     string
	TargetType string
	TargetID   string
	Detail     any // 可 JSON 序列化结构；nil 则落 NULL
	RequestID  string
	IP         string
}

// Store 审计存储。
type Store struct {
	db *sql.DB
}

// NewStore 创建审计存储。
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Insert 写入一条审计记录。
func (s *Store) Insert(ctx context.Context, e Entry) error {
	var uid any
	if e.UserID != 0 {
		uid = e.UserID
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audit_logs(user_id, username, action, target_type, target_id, detail, request_id, ip)
		 VALUES($1, $2, $3, $4, $5, $6, $7, $8)`,
		uid, nullString(e.Username), e.Action, e.TargetType, e.TargetID, e.Detail, e.RequestID, e.IP)
	if err != nil {
		InsertFailures.Add(1)
	}
	return err
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}
