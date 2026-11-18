package topology

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

// Syncer 是拓扑同步器：手动触发（T3）与后台定时轮共用同一份实现。
//
// 三条不变量（改这里之前先读）：
//  1. **失败不清空快照**：取不到数据只写失败状态，不碰 internal_models；
//  2. **空快照不算成功**：0 个节点视为失败（见 ErrEmptySnapshot），否则存量节点会被集体标灰；
//  3. **同一时刻只跑一轮**：TryLock 抢不到直接返回 ErrSyncInFlight，不排队。
type Syncer struct {
	src   Source
	store Store
	now   func() time.Time
	log   *slog.Logger
	mu    sync.Mutex
}

// SyncerOptions 是 NewSyncer 的入参。
type SyncerOptions struct {
	Source Source
	Store  Store
	Now    func() time.Time
	Logger *slog.Logger
}

// NewSyncer 创建同步器。
func NewSyncer(opts SyncerOptions) *Syncer {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Syncer{src: opts.Source, store: opts.Store, now: now, log: logger}
}

// RunOnce 跑一轮同步。
//
// 返回的 error 与 Result 的分工：error 表示「这轮没同步成功」（调用方据此决定怎么展示），
// Result 里始终带上可读的 State/Message，两者都不为空才算把失败讲清楚。
func (s *Syncer) RunOnce(ctx context.Context) (Result, error) {
	if s == nil || s.store == nil {
		return Result{}, errors.New("拓扑同步器未装配存储")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if !s.mu.TryLock() {
		return Result{State: StateFailed, Message: ErrSyncInFlight.Error()}, ErrSyncInFlight
	}
	defer s.mu.Unlock()

	runAt := s.now()
	nodes, err := s.src.Fetch(ctx)
	if err == nil && len(nodes) == 0 {
		err = ErrEmptySnapshot
	}
	if err != nil {
		res := s.fail(ctx, runAt, err)
		return res, err
	}

	created, stale, rerr := s.store.Replace(ctx, nodes, runAt)
	if rerr != nil {
		res := s.fail(ctx, runAt, rerr)
		return res, rerr
	}

	st := SyncState{
		LastRunAt:     runAt,
		LastSuccessAt: runAt,
		State:         StateOK,
		Created:       created,
		Stale:         stale,
	}
	// 失败原因自上次成功起就无意义了：成功即清零 Message。
	if err := s.store.SaveState(ctx, st); err != nil {
		s.log.Error("写入拓扑同步状态失败（本轮数据已落库）", "err", err)
	}
	return Result{Fetched: len(nodes), Created: created, Stale: stale, State: StateOK}, nil
}

// fail 记录失败状态并返回可展示结果。LastSuccessAt 必须沿用上一次成功的值
// （页面的「最后成功」在失败时不能变），取不到就留零值。
func (s *Syncer) fail(ctx context.Context, runAt time.Time, cause error) Result {
	prev, err := s.store.LoadState(ctx)
	if err != nil {
		s.log.Warn("读取上期同步状态失败（按无历史处理）", "err", err)
		prev = SyncState{}
	}
	st := SyncState{
		LastRunAt:     runAt,
		LastSuccessAt: prev.LastSuccessAt,
		State:         StateFailed,
		Message:       cause.Error(),
		Created:       0,
		Stale:         0,
	}
	if err := s.store.SaveState(ctx, st); err != nil {
		s.log.Error("写入拓扑同步失败状态失败", "err", err)
	}
	s.log.Warn("拓扑同步失败（保留上期快照）", "err", cause)
	return Result{State: StateFailed, Message: cause.Error()}
}
