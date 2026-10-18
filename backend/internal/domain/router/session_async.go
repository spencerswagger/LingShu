package router

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/team/llmgateway/internal/pkg/asyncwrite"
)

// sessionOp 会话变更操作类型，随负载一并序列化，供崩溃重放时重建副作用。
type sessionOp string

const (
	// sessionOpUpsert 会话创建/续期（写入或刷新运行时列）。
	sessionOpUpsert sessionOp = "upsert"
	// sessionOpDelete 会话删除。
	sessionOpDelete sessionOp = "delete"
	// sessionOpSetClosed 会话关闭/重开。
	sessionOpSetClosed sessionOp = "set_closed"
)

// sessionJobPayload 是会话异步任务的持久负载：Exec 闭包不可序列化，
// 崩溃重启后由重放函数依据本结构 + 底层 store 重建副作用。
type sessionJobPayload struct {
	Op        sessionOp `json:"op"`
	SessionID string    `json:"session_id"`
	Closed    bool      `json:"closed,omitempty"`
	Session   *Session  `json:"session,omitempty"`
}

// AsyncPersister 是 sessionPersister 的异步适配器：把 Upsert/Delete/SetClosed 提交给
// asyncwrite.Writer 异步落库，LoadActive 保持同步直读（启动恢复须即时可用）。
//
// 保序与幂等（不允许隐性乱序/隐性丢写）：
//   - Key = session_id：同一会话行的全部变更被哈希到同一分片串行执行，
//     杜绝「SetClosed 先于 Upsert 落库」造成的乱序复活；
//   - IdempotencyKey 留空：会话续期（Renew→Upsert）是**合法且必须每次都落库**的重复写，
//     若用「sessionID|op」做进程内去重会把首次之后的续期全部吞掉（last_active/expire_at 永不刷新）。
//     因此不做进程内去重，改由底层 store 的 DB 级幂等（sessions 按 session_id UPSERT、
//     Delete/SetClosed 幂等）保证重放安全；同时避免 asyncwrite 去重集合随会话量无限增长；
//   - Payload 编码操作，崩溃重启后由 KindSession 重放函数重建副作用（DB 幂等，重放不产生副作用双写）；
//   - 失败任务保留 WAL 记录待重启重放（不静默丢失会话投影）。
type AsyncPersister struct {
	w     *asyncwrite.Writer
	inner sessionPersister
}

// 编译期断言：AsyncPersister 满足 sessionPersister。
var _ sessionPersister = (*AsyncPersister)(nil)

// NewAsyncPersister 创建会话异步持久化适配器，并注册 KindSession 的重放函数。
func NewAsyncPersister(w *asyncwrite.Writer, inner sessionPersister) *AsyncPersister {
	p := &AsyncPersister{w: w, inner: inner}
	if w != nil {
		w.SetReplayFunc(asyncwrite.KindSession, p.replay)
	}
	return p
}

// cloneSession 生成会话快照，避免异步执行闭包读取调用方后续会变更的会话指针（数据竞争）。
func cloneSession(s *Session) *Session {
	if s == nil {
		return nil
	}
	cp := *s
	if s.LastMsgFingerprints != nil {
		cp.LastMsgFingerprints = append([]string(nil), s.LastMsgFingerprints...)
	}
	if s.ClosedAt != nil {
		t := *s.ClosedAt
		cp.ClosedAt = &t
	}
	return &cp
}

// submit 统一提交入口：编码负载 + 生成执行闭包。writer 未装配时同步直调底层（安全兜底）。
func (p *AsyncPersister) submit(sessionID string, pl sessionJobPayload, exec func(ctx context.Context) error) error {
	if p.w == nil {
		return exec(context.Background())
	}
	payload, err := json.Marshal(pl)
	if err != nil {
		return err
	}
	return p.w.Submit(asyncwrite.Job{
		Kind:    asyncwrite.KindSession,
		Key:     sessionID,
		Payload: payload,
		Exec:    exec,
	})
}

// Upsert 提交会话创建/续期（异步）。
func (p *AsyncPersister) Upsert(ctx context.Context, sess *Session) error {
	if sess == nil || p.inner == nil {
		return nil
	}
	snapshot := cloneSession(sess)
	pl := sessionJobPayload{Op: sessionOpUpsert, SessionID: sess.SessionID, Session: snapshot}
	return p.submit(sess.SessionID, pl, func(c context.Context) error {
		return p.inner.Upsert(c, snapshot)
	})
}

// Delete 提交会话删除（异步）。
func (p *AsyncPersister) Delete(ctx context.Context, sessionID string) error {
	if p.inner == nil {
		return nil
	}
	pl := sessionJobPayload{Op: sessionOpDelete, SessionID: sessionID}
	return p.submit(sessionID, pl, func(c context.Context) error {
		return p.inner.Delete(c, sessionID)
	})
}

// DeleteMany 逐条提交会话删除：每条以自身 session_id 为 Key，保证与会话其余变更同分片保序。
func (p *AsyncPersister) DeleteMany(ctx context.Context, ids []string) error {
	if p.inner == nil {
		return nil
	}
	var firstErr error
	for _, id := range ids {
		if id == "" {
			continue
		}
		if err := p.Delete(ctx, id); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// SetClosed 提交会话关闭/重开（异步）。
func (p *AsyncPersister) SetClosed(ctx context.Context, sessionID string, closed bool) error {
	if p.inner == nil {
		return nil
	}
	pl := sessionJobPayload{Op: sessionOpSetClosed, SessionID: sessionID, Closed: closed}
	return p.submit(sessionID, pl, func(c context.Context) error {
		return p.inner.SetClosed(c, sessionID, closed)
	})
}

// LoadActive 启动恢复查询：保持同步直读（会话注册表须在清扫循环启动前完成重建）。
func (p *AsyncPersister) LoadActive(ctx context.Context, now time.Time) ([]*Session, error) {
	if p.inner == nil {
		return nil, nil
	}
	return p.inner.LoadActive(ctx, now)
}

// replay 崩溃恢复重放：解码负载并调用底层 store 重建副作用。
func (p *AsyncPersister) replay(ctx context.Context, key string, payload []byte) error {
	if p.inner == nil {
		return nil
	}
	var pl sessionJobPayload
	if err := json.Unmarshal(payload, &pl); err != nil {
		return fmt.Errorf("decode session payload: %w", err)
	}
	switch pl.Op {
	case sessionOpUpsert:
		if pl.Session == nil {
			return nil
		}
		return p.inner.Upsert(ctx, pl.Session)
	case sessionOpDelete:
		return p.inner.Delete(ctx, pl.SessionID)
	case sessionOpSetClosed:
		return p.inner.SetClosed(ctx, pl.SessionID, pl.Closed)
	default:
		return fmt.Errorf("unknown session op %q", pl.Op)
	}
}
