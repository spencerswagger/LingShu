package gateway

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/team/llmgateway/internal/pkg/asyncwrite"
)

// AsyncCallLogStore 是 CallLogStore 的异步适配器：Insert 提交 asyncwrite 异步落库，
// 把调用日志写入从网关响应热路径上剥离（丢失窗口上界 50ms，由 asyncwrite 背压策略决定）。
//
// 分类与幂等（不允许隐性重复）：
//   - Kind = KindCallLog；
//   - Key = billing_id（为空则回退 request_id）：同一账单的日志同分片保序；
//   - IdempotencyKey = billing_id：同一次运行内同一 billing_id 至多成功执行一次，
//     且 SQL 已 ON CONFLICT(billing_id) DO NOTHING，双重幂等；
//   - Payload = JSON(CallLog)：崩溃重启后由 KindCallLog 重放函数重建副作用。
//   - 失败任务保留 WAL 记录待重启重放（审计/日志不静默丢失）。
type AsyncCallLogStore struct {
	w     *asyncwrite.Writer
	inner CallLogStore
}

// 编译期断言：AsyncCallLogStore 满足 CallLogStore。
var _ CallLogStore = (*AsyncCallLogStore)(nil)

// NewAsyncCallLogStore 创建调用日志异步存储，并注册 KindCallLog 的重放函数。
func NewAsyncCallLogStore(w *asyncwrite.Writer, inner CallLogStore) *AsyncCallLogStore {
	s := &AsyncCallLogStore{w: w, inner: inner}
	if w != nil {
		w.SetReplayFunc(asyncwrite.KindCallLog, s.replay)
	}
	return s
}

// Insert 提交一条调用日志（异步）。writer 未装配时同步直调底层（安全兜底）。
func (s *AsyncCallLogStore) Insert(ctx context.Context, c *CallLog) error {
	if c == nil || s.inner == nil {
		return nil
	}
	snapshot := *c
	key := c.BillingID
	if key == "" {
		key = c.RequestID
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	if s.w == nil {
		return s.inner.Insert(ctx, &snapshot)
	}
	return s.w.Submit(asyncwrite.Job{
		Kind:           asyncwrite.KindCallLog,
		Key:            key,
		IdempotencyKey: c.BillingID,
		Payload:        payload,
		Exec: func(ectx context.Context) error {
			return s.inner.Insert(ectx, &snapshot)
		},
	})
}

// replay 崩溃恢复重放：解码负载并调用底层 store 重建副作用。
func (s *AsyncCallLogStore) replay(ctx context.Context, key string, payload []byte) error {
	if s.inner == nil {
		return nil
	}
	var c CallLog
	if err := json.Unmarshal(payload, &c); err != nil {
		return fmt.Errorf("decode call log payload: %w", err)
	}
	return s.inner.Insert(ctx, &c)
}
