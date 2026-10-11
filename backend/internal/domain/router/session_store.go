package router

import (
	"context"
	"database/sql"
	"time"
)

// sessionCols 列出 sessions 表查询使用列，与 DDL 一致。
const sessionCols = `session_id, user_id, token_id, model, session_raw, channel_key_id, internal_model_id, created_at, last_active, expire_at, COALESCE(name, ''), COALESCE(closed, false), COALESCE(closed_at, NULL)`

// sessionUpsertSQL 创建/续期会话的 UPSERT：冲突（session_id）时刷新运行时列并把会话复位为活跃态
// （closed=false、closed_at=NULL）——Upsert 只由 Attach（新建/迁移/重新激活）与 Renew（仅未关闭会话）触发，
// 因此复位不会覆盖「关闭」：Renew 对已关闭会话直接返回，不会走到这里。
// user/model/name 等归属仅在新建行时写入；改名走 SetName。
const sessionUpsertSQL = `
INSERT INTO sessions(session_id, user_id, token_id, model, session_raw, channel_key_id, internal_model_id, name, created_at, last_active, expire_at, closed)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,false)
ON CONFLICT (session_id) DO UPDATE SET last_active=$10, expire_at=$11, channel_key_id=$6, internal_model_id=$7, closed=false, closed_at=NULL`

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

// SetClosed 更新会话关闭状态（关闭后不命中路由，记录保留；关闭时记录关闭时间用于保留期清扫）。
func (s *SessionStore) SetClosed(ctx context.Context, sessionID string, closed bool) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET closed=$2, closed_at=CASE WHEN $2 THEN COALESCE(closed_at, now()) ELSE NULL END WHERE session_id=$1`,
		sessionID, closed)
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

// LoadActive 查询启动恢复所需会话：仅未过期且未关闭的会话（粘性路由 + 并发计数）。
// 已关闭会话不再载入内存：会话列表已改读 sessions 表，关闭的会话既不参与路由也不需要留在内存。
// sessions 表无 channel_id 列，会话维度只有 channel_key_id（含 internal_model_id）。
func (s *SessionStore) LoadActive(ctx context.Context, now time.Time) ([]*Session, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+sessionCols+` FROM sessions WHERE expire_at > $1 AND NOT closed`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*Session, 0, 8)
	for rows.Next() {
		var sess Session
		var token sql.NullInt64
		var closedAt sql.NullTime
		if err := rows.Scan(&sess.SessionID, &sess.UserID, &token, &sess.Model, &sess.SessionRaw,
			&sess.ChannelKeyID, &sess.InternalModelID, &sess.CreatedAt, &sess.LastActive, &sess.ExpireAt,
			&sess.Name, &sess.Closed, &closedAt); err != nil {
			return nil, err
		}
		if token.Valid {
			sess.TokenID = token.Int64
		}
		if closedAt.Valid {
			v := closedAt.Time
			sess.ClosedAt = &v
		}
		out = append(out, &sess)
	}
	return out, rows.Err()
}
