package router

import (
	"context"
	"database/sql"
	"time"
)

// sessionCols 列出 sessions 表查询使用列，与 DDL 一致。
const sessionCols = `session_id, user_id, token_id, model, session_raw, channel_key_id, internal_model_id, created_at, last_active, expire_at, COALESCE(name, ''), COALESCE(closed, false)`

// sessionUpsertSQL 创建/续期会话的 UPSERT：冲突（session_id）时只刷新运行时列，不覆盖 user/model/name 等归属。
// name/closed 仅在新建行时写入（首次自动命名）；改名走 SetName、关闭走 SetClosed，网关续期不会覆盖。
const sessionUpsertSQL = `
INSERT INTO sessions(session_id, user_id, token_id, model, session_raw, channel_key_id, internal_model_id, name, created_at, last_active, expire_at, closed)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,false)
ON CONFLICT (session_id) DO UPDATE SET last_active=$10, expire_at=$11, channel_key_id=$6, internal_model_id=$7`

// SessionStore 提供 sessions 表数据访问（内存为主、DB 为持久化投影，最终一致）。
type SessionStore struct {
	db *sql.DB
}

// NewSessionStore 创建会话存储，db 为 pgx stdlib 连接池。
func NewSessionStore(db *sql.DB) *SessionStore {
	return &SessionStore{db: db}
}

// nullableID 可空外键（token_id）：0 → NULL。
func nullableID(v int64) any {
	if v == 0 {
		return nil
	}
	return v
}

// Upsert 创建或更新会话行；冲突键为 session_id。channel_key_id 取 ChannelKeyID（唯一维度）。
func (s *SessionStore) Upsert(ctx context.Context, sess *Session) error {
	if sess == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, sessionUpsertSQL,
		sess.SessionID, sess.UserID, nullableID(sess.TokenID), sess.Model, sess.SessionRaw,
		sess.ChannelKeyID, sess.InternalModelID, sess.Name, sess.CreatedAt, sess.LastActive, sess.ExpireAt)
	return err
}

// SetName 更新会话可读名称（手动改名，网关自动续期不会覆盖）。
func (s *SessionStore) SetName(ctx context.Context, sessionID, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE sessions SET name=$2 WHERE session_id=$1`, sessionID, name)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// SetClosed 更新会话关闭状态（关闭后不命中路由，记录保留）。
func (s *SessionStore) SetClosed(ctx context.Context, sessionID string, closed bool) error {
	_, err := s.db.ExecContext(ctx, `UPDATE sessions SET closed=$2 WHERE session_id=$1`, sessionID, closed)
	return err
}

// Delete 删除单个会话行。
func (s *SessionStore) Delete(ctx context.Context, sessionID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE session_id=$1`, sessionID)
	return err
}

// DeleteMany 批量删除会话行（ANY 数组参数）；空列表为无操作。
func (s *SessionStore) DeleteMany(ctx context.Context, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE session_id = ANY($1)`, ids)
	return err
}

// LoadActive 查询未过期会话（expire_at > now），用于启动恢复。
// sessions 表无 channel_id 列，会话维度只有 channel_key_id（含 internal_model_id）。
func (s *SessionStore) LoadActive(ctx context.Context, now time.Time) ([]*Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE expire_at > $1`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*Session, 0, 8)
	for rows.Next() {
		var sess Session
		var token sql.NullInt64
		if err := rows.Scan(&sess.SessionID, &sess.UserID, &token, &sess.Model, &sess.SessionRaw,
			&sess.ChannelKeyID, &sess.InternalModelID, &sess.CreatedAt, &sess.LastActive, &sess.ExpireAt, &sess.Name, &sess.Closed); err != nil {
			return nil, err
		}
		if token.Valid {
			sess.TokenID = token.Int64
		}
		out = append(out, &sess)
	}
	return out, rows.Err()
}
