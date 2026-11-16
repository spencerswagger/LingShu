package router

import (
	"context"
	"database/sql"
	"time"
)

// sessionCols 列出 sessions 表查询使用列，与 DDL 一致。
//
// ⚠️ 0024 已删 sessions.channel_key_id：查询/写入都不再碰该列，
// Session.ChannelKeyID 恒为 0（会话粘性的渠道维度随渠道体系一起下线）。
const sessionCols = `session_id, user_id, token_id, model, session_raw, internal_model_id, created_at, last_active, expire_at, COALESCE(name, ''), COALESCE(closed, false), COALESCE(closed_at, NULL)`

// sessionUpsertSQL 创建/续期会话的 UPSERT：冲突（session_id）时刷新运行时列并把会话复位为活跃态
// （closed=false、closed_at=NULL）——Upsert 只由 Attach（新建/迁移/重新激活）与 Renew（仅未关闭会话）触发，
// 因此复位不会覆盖「关闭」：Renew 对已关闭会话直接返回，不会走到这里。
// user/model/name 等归属仅在新建行时写入；改名走 SetName。
const sessionUpsertSQL = `
INSERT INTO sessions(session_id, user_id, token_id, model, session_raw, internal_model_id, name, created_at, last_active, expire_at, closed)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,false)
ON CONFLICT (session_id) DO UPDATE SET last_active=$9, expire_at=$10, internal_model_id=$6, closed=false, closed_at=NULL`

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

// Upsert 创建或更新会话行；冲突键为 session_id。
//
// ⚠️ 0024 删了 sessions.channel_key_id：会话不再记渠道维度（ChannelKeyID 不落库）。
func (s *SessionStore) Upsert(ctx context.Context, sess *Session) error {
	if sess == nil {
		return nil
	}
	_, err := s.db.ExecContext(ctx, sessionUpsertSQL,
		sess.SessionID, sess.UserID, nullableID(sess.TokenID), sess.Model, sess.SessionRaw,
		sess.InternalModelID, sess.Name, sess.CreatedAt, sess.LastActive, sess.ExpireAt)
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
// sessions 表已无渠道维度列（0024）：只回填 internal_model_id 等运行时列。
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
			&sess.InternalModelID, &sess.CreatedAt, &sess.LastActive, &sess.ExpireAt,
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
