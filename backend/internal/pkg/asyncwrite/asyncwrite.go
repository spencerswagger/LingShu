// Package asyncwrite 提供统一的写后异步持久化层（write-behind）。
//
// 设计目标：
//   - 有界内存队列 + 分片工作协程，按 Key 哈希固定路由，保证同 Key FIFO、异 Key 并发；
//   - 可选 WAL 预写日志（DurableSpool），批量组提交 fsync，崩溃后可重放；
//   - 幂等去重：非空 IdempotencyKey 在一次运行内至多成功执行一次；
//   - 安全模式：未开启异步的类别同步执行，绝不静默丢弃审计/会话数据。
//
// 丢失窗口上界 50ms：Submit 遇到队列满时最多短暂阻塞 50ms（等待下游消化），
// 仍满才计 Dropped、打 Error 日志并返回 ErrBackpressure（保留 WAL 记录待重启重放）；
// 同步型类别则直接同步执行，不进入队列。
package asyncwrite

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/team/llmgateway/internal/pkg/logger"
)

// 默认参数。
const (
	defaultShards        = 4                     // 默认分片数
	defaultFSyncInterval = 10 * time.Millisecond // 默认组提交间隔
	defaultMaxBatch      = 100                   // 默认触发一次组提交的累积记录数
	defaultQueueSize     = 4096                  // 默认内存队列总容量
	backpressureWait     = 50 * time.Millisecond // 队列满时的短暂背压等待
	maxExecAttempts      = 3                     // 单个任务最大执行尝试次数
	retryBackoff         = 2 * time.Millisecond  // 重试间隔
	// defaultIdemMaxEntries 幂等去重集合的有界上限（FIFO 淘汰）。
	// 去重仅为「进程内至多一次」的性能优化，正确性由 DB 侧唯一约束（如账单 billing_id、
	// 事件主键的 ON CONFLICT）兜底，故允许有界淘汰导致极少数重复执行，但不允许无界增长。
	defaultIdemMaxEntries = 100000
	// statsLogInterval 运行期统计（Dropped/Retried）周期日志间隔。
	// 仅在计数发生变化时输出，开销可忽略；使「绝不静默丢弃」契约在生产中可观测（P2-6）。
	statsLogInterval = 30 * time.Second
)

// ErrBackpressure 表示队列背压超时：任务已写入 WAL（若启用）但未能入队。
// 调用方可据此感知「未入队但记录已持久化」，由下次重启重放兜底，绝不静默丢弃。
var ErrBackpressure = errors.New("asyncwrite: 队列背压超时，任务未能入队")

// ErrWALUnavailable 表示配置了 WALPath 但 WAL 不可用（打开失败或运行期重开失败）。
// 此时拒绝异步提交而非静默降级为纯内存模式，避免「持久化」类别（会话/渠道事件/调用日志）
// 在无 WAL 兜底的情况下静默丢失（违背「绝不静默丢弃」契约）。装配层应在启动期据此致命化。
var ErrWALUnavailable = errors.New("asyncwrite: WAL 不可用，拒绝写入（配置了 WALPath 但打开失败）")

// Config 是 Writer 的构造配置；零值字段由 New 填充默认值。
type Config struct {
	// WALPath 预写日志路径；为空表示纯内存模式（不落盘、不支持重放）。
	WALPath string
	// Shards 分片（工作协程）数，默认 4。
	Shards int
	// FSyncInterval 组提交定时 fsync 间隔，默认 10ms。
	FSyncInterval time.Duration
	// MaxBatch 累积多少条记录后立即触发一次组提交，默认 100。
	MaxBatch int
	// QueueSize 内存队列总容量（按分片均分），默认 4096。
	QueueSize int
	// AsyncKinds 标记哪些类别走异步；map 为 nil 或键缺失即视为同步（安全模式）。
	AsyncKinds map[Kind]bool
}

// Stats 是运行期统计快照。
type Stats struct {
	// Depth 在途任务数（已入队但尚未执行完成）。
	Depth int
	// Dropped 因背压超时被丢弃的任务数。
	Dropped int
	// Retried 执行失败后的重试次数。
	Retried int
	// LastFSync 最近一次 fsync 完成时间；零值表示尚未 fsync。
	LastFSync time.Time
}

// replayFn 是崩溃恢复时的重放函数（不含 Exec，靠 Payload 重建副作用）。
type replayFn func(ctx context.Context, key string, payload []byte) error

// envelope 是入队单元，额外携带 WAL 序号以便任务完成后删除对应记录。
type envelope struct {
	job Job
	seq uint64
}

// shard 是单个分片：一个固定工作协程 + 一条有界队列。
type shard struct {
	ch chan envelope
}

// loadedRec 是从 WAL 恢复出的记录及其在内存中的序号。
type loadedRec struct {
	seq uint64
	rec []byte
}

// Writer 是写后异步持久化器。
type Writer struct {
	cfg    Config
	shards []*shard

	started  atomic.Bool
	closed   atomic.Bool
	stopOnce sync.Once
	stopCh   chan struct{}
	wg       sync.WaitGroup

	// sendMu 保护"入队"与"关停时关闭队列"之间的竞态，避免向已关闭 channel 发送。
	sendMu sync.RWMutex

	// WAL 状态（walMu 保护）。
	walMu         sync.Mutex
	walFile       *os.File
	walErr        error
	pending       map[uint64][]byte
	pendingOrder  []uint64
	nextSeq       uint64
	appendedSince int
	walDirty      bool
	recovered     []loadedRec

	// 重放函数注册表。
	replayMu sync.RWMutex
	replay   map[Kind]replayFn

	// 幂等去重集合（idemMu 保护）。键带 Kind 命名空间（避免跨类别同名 IdempotencyKey 互相误判），
	// 且默认有界（idemMax，FIFO 淘汰），防止长跑进程去重集合无界增长。
	idemMu    sync.Mutex
	done      map[string]struct{}
	doneOrder []string
	idemMax   int

	// fsyncCounter 是测试钩子（每次真正 fsync 时回调一次）；作为实例字段而非包级变量，
	// 避免并行测试共享可变状态产生数据竞争。须在 Start 之前设置。
	fsyncCounter func()

	commitCh chan struct{}

	depth    atomic.Int64
	dropped  atomic.Int64
	retried  atomic.Int64
	lastSync atomic.Int64
}

// New 创建 Writer 并填默认值：Shards=4、FSyncInterval=10ms、MaxBatch=100、QueueSize=4096。
//
// 语义要点：
//   - 丢失窗口上界 50ms（由 backpressureWait 决定）；
//   - 背压默认短暂阻塞 50ms，仍满才计 Dropped 并打日志，绝不静默丢弃审计/会话；
//   - WALPath 为空时进入纯内存模式，不落盘、不支持重放，适合 KindTokenLastUsed。
//
// 若 WALPath 指向已存在的文件（上次崩溃遗留），New 会加载其记录，待 Start 时重放。
func New(cfg Config) *Writer {
	if cfg.Shards <= 0 {
		cfg.Shards = defaultShards
	}
	if cfg.FSyncInterval <= 0 {
		cfg.FSyncInterval = defaultFSyncInterval
	}
	if cfg.MaxBatch <= 0 {
		cfg.MaxBatch = defaultMaxBatch
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}

	per := cfg.QueueSize / cfg.Shards
	if per < 1 {
		per = 1
	}

	w := &Writer{
		cfg:      cfg,
		stopCh:   make(chan struct{}),
		commitCh: make(chan struct{}, 1),
		pending:  make(map[uint64][]byte),
		replay:   make(map[Kind]replayFn),
		done:     make(map[string]struct{}),
		idemMax:  defaultIdemMaxEntries,
	}
	w.shards = make([]*shard, cfg.Shards)
	for i := range w.shards {
		w.shards[i] = &shard{ch: make(chan envelope, per)}
	}

	if cfg.WALPath != "" {
		if dir := filepath.Dir(cfg.WALPath); dir != "" && dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				logger.Fallback().Error("asyncwrite: 创建 WAL 目录失败", "err", err, "path", cfg.WALPath)
				w.walErr = fmt.Errorf("asyncwrite: 创建 WAL 目录失败: %w", err)
			}
		}
		f, err := os.OpenFile(cfg.WALPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
		if err != nil {
			logger.Fallback().Error("asyncwrite: 打开 WAL 失败", "err", err, "path", cfg.WALPath)
			// 记录失败：配置了 WAL 却打不开时，绝不允许静默降级为内存模式（见 Err/appendWAL）。
			if w.walErr == nil {
				w.walErr = fmt.Errorf("asyncwrite: 打开 WAL 失败: %w", err)
			}
		} else {
			w.walFile = f
			w.loadRecovered(cfg.WALPath)
		}
	}
	return w
}

// Err 返回构造期 WAL 打开/目录创建错误（nil 表示 WAL 正常或未配置）。
// 装配层应据此致命化启动：配置了 WALPath 却不可用时不得静默降级为内存模式，
// 否则「持久化」类别（会话/渠道事件/调用日志）将失去 WAL 兜底，违背「绝不静默丢弃」契约。
func (w *Writer) Err() error { return w.walErr }

// loadRecovered 读取既有 WAL，登记为待处理记录并保存原始字节供 Start 时重放。
func (w *Writer) loadRecovered(path string) {
	recs, err := readWALRecords(path)
	if err != nil {
		logger.Fallback().Error("asyncwrite: 读取 WAL 失败", "err", err, "path", path)
		return
	}
	for _, rec := range recs {
		seq := w.nextSeq
		w.nextSeq++
		w.pending[seq] = rec
		w.pendingOrder = append(w.pendingOrder, seq)
		w.recovered = append(w.recovered, loadedRec{seq: seq, rec: rec})
	}
}

// SetReplayFunc 注册某类别的重放函数；需在 Start 之前调用。
// 崩溃恢复时对 WAL 中同类别记录调用 fn(ctx, key, payload) 重放副作用。
func (w *Writer) SetReplayFunc(kind Kind, fn func(ctx context.Context, key string, payload []byte) error) {
	w.replayMu.Lock()
	defer w.replayMu.Unlock()
	if fn == nil {
		delete(w.replay, kind)
		return
	}
	w.replay[kind] = fn
}

// Submit 提交一个任务。
//
//   - 若 AsyncKinds[job.Kind] 为 false（或 map 为 nil），同步执行 Exec 并返回其错误（安全模式）；
//   - 否则写入 WAL（若已启用）并入队：WAL 写入失败则记录日志并返回错误，不入队（不伪称已持久化）；
//     队列满则短暂背压 50ms，仍满时计 Dropped、记 Error 日志并返回 ErrBackpressure——
//     已持久化的 WAL 记录**保留**（不删除），留待重启重放，绝不静默丢弃。
//   - Shutdown 之后调用返回错误。
func (w *Writer) Submit(job Job) error {
	if !w.cfg.AsyncKinds[job.Kind] {
		if job.Exec == nil {
			return nil
		}
		return job.Exec(context.Background())
	}

	if job.Exec == nil {
		return nil
	}

	w.sendMu.RLock()
	defer w.sendMu.RUnlock()
	if w.closed.Load() {
		return errors.New("asyncwrite: writer 已关闭，拒绝新的提交")
	}

	// 先落 WAL 再入队。WAL 写失败意味着「未持久化」，必须把错误如实返回给调用方，
	// 绝不入队、不宣称已持久化（否则会在队列丢失时造成隐性数据丢失）。
	seq, err := w.appendWAL(job)
	if err != nil {
		return err
	}

	w.depth.Add(1)
	env := envelope{job: job, seq: seq}
	if !w.enqueueLocked(env) {
		w.depth.Add(-1)
		w.dropped.Add(1)
		// 背压溢出：保留 WAL 记录（不 dropWAL）以便重启重放，并返回非 nil 错误，避免静默丢弃。
		logger.Fallback().Error("asyncwrite: 队列背压超时，任务未入队（WAL 记录保留待重放）",
			"kind", job.Kind, "key", job.Key, "idem", job.IdempotencyKey)
		return ErrBackpressure
	}
	return nil
}

// enqueueLocked 在持有 sendMu 读锁的前提下把任务投递到固定分片。
// 先在满时短暂背压 50ms，仍失败返回 false。
func (w *Writer) enqueueLocked(env envelope) bool {
	s := w.shards[shardIndex(env.job.Key, len(w.shards))]

	select {
	case s.ch <- env:
		return true
	default:
	}

	t := time.NewTimer(backpressureWait)
	defer t.Stop()
	select {
	case s.ch <- env:
		return true
	case <-t.C:
		return false
	case <-w.stopCh:
		return false
	}
}

// Start 启动重放与工作协程；可安全重复调用。
func (w *Writer) Start(ctx context.Context) {
	if w.started.Swap(true) {
		return
	}
	w.replayRecovered(ctx)
	for i := range w.shards {
		w.wg.Add(1)
		go w.worker(ctx, w.shards[i])
	}
	w.wg.Add(1)
	go w.commitLoop(ctx)
}

// Shutdown 停止接收、排空队列、最终 fsync 并关闭文件描述符。
// 返回后 Depth 应为 0，且 WAL 不含待处理记录；此后 Submit 会返回错误。
func (w *Writer) Shutdown(ctx context.Context) error {
	w.sendMu.Lock()
	if w.closed.Load() {
		w.sendMu.Unlock()
		return nil
	}
	w.closed.Store(true)
	for _, s := range w.shards {
		close(s.ch)
	}
	w.stopOnce.Do(func() { close(w.stopCh) })
	w.sendMu.Unlock()

	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()

	select {
	case <-done:
		return w.finalize()
	case <-ctx.Done():
		w.finalize()
		return ctx.Err()
	}
}

// Stats 返回运行期统计快照。
func (w *Writer) Stats() Stats {
	s := Stats{
		Depth:   int(w.depth.Load()),
		Dropped: int(w.dropped.Load()),
		Retried: int(w.retried.Load()),
	}
	if ns := w.lastSync.Load(); ns > 0 {
		s.LastFSync = time.Unix(0, ns)
	}
	return s
}

// worker 是分片工作协程：串行消费本分片队列，保证同 Key FIFO。
func (w *Writer) worker(ctx context.Context, s *shard) {
	defer w.wg.Done()
	for env := range s.ch {
		w.process(ctx, env)
	}
}

// process 执行单个任务：幂等去重 -> 有限重试执行 -> 仅成功后删除 WAL 记录。
// 关键契约（不允许隐性丢失）：执行失败（重试耗尽）时**保留** WAL 记录，绝不 dropWAL——
// 记录留待下次启动重放，与「绝不静默丢弃审计/会话」一致；仅成功或重复幂等键才删除记录。
// 幂等语义：非空 IdempotencyKey 在本次运行内至多成功执行一次（done 集合按 Kind 命名空间 +
// 有界 FIFO 淘汰；淘汰仅意味着极少数重复执行，DB 侧 ON CONFLICT 等唯一约束仍保证最终幂等）。
func (w *Writer) process(ctx context.Context, env envelope) {
	defer w.depth.Add(-1)

	job := env.job
	if !w.claim(job.Kind, job.IdempotencyKey) {
		w.dropWAL(env.seq)
		return
	}

	var err error
	for attempt := 0; attempt < maxExecAttempts; attempt++ {
		err = job.Exec(ctx)
		if err == nil {
			break
		}
		w.retried.Add(1)
		logger.Fallback().Error("asyncwrite: 任务执行失败",
			"kind", job.Kind, "key", job.Key, "attempt", attempt+1, "err", err)
		if attempt+1 < maxExecAttempts {
			time.Sleep(retryBackoff)
		}
	}
	if err != nil {
		// 重试耗尽：保留 WAL 记录（不删除），留待重启重放；同时保留幂等占用，
		// 避免本进程内对同一幂等键重复投递再次执行。
		logger.Fallback().Error("asyncwrite: 任务重试耗尽，保留 WAL 待重启重放",
			"kind", job.Kind, "key", job.Key, "err", err)
		return
	}
	w.dropWAL(env.seq)
}

// claim 对非空 IdempotencyKey 做一次性占用，返回 false 表示已被占用（应跳过）。
// 去重键带 Kind 命名空间，避免不同类别复用同一字符串 idem 时互相误判；
// 集合有界（idemMax），超限按插入顺序 FIFO 淘汰最早条目（E6 修复：防无界增长）。
func (w *Writer) claim(kind Kind, idem string) bool {
	if idem == "" {
		return true
	}
	key := idemKey(kind, idem)
	w.idemMu.Lock()
	defer w.idemMu.Unlock()
	if _, ok := w.done[key]; ok {
		return false
	}
	w.done[key] = struct{}{}
	w.doneOrder = append(w.doneOrder, key)
	w.evictIdemLocked()
	return true
}

// evictIdemLocked 当去重集合超过上限时按插入顺序淘汰最早条目（调用方须持有 idemMu）。
func (w *Writer) evictIdemLocked() {
	if w.idemMax <= 0 {
		return
	}
	for len(w.done) > w.idemMax && len(w.doneOrder) > 0 {
		oldest := w.doneOrder[0]
		w.doneOrder = w.doneOrder[1:]
		delete(w.done, oldest)
	}
}

// idemKey 生成去重键：Kind 命名空间 + 原始幂等键，避免跨类别同名冲突。
func idemKey(kind Kind, idem string) string {
	return strconv.Itoa(int(kind)) + ":" + idem
}

// replayRecovered 对 New 时加载的 WAL 记录做重放：命中重放函数且幂等未完成才执行。
func (w *Writer) replayRecovered(ctx context.Context) {
	for _, lr := range w.recovered {
		kind, key, idem, payload, err := parseRecord(lr.rec)
		if err != nil {
			logger.Fallback().Error("asyncwrite: WAL 记录解析失败", "err", err)
			continue
		}
		if !w.claim(kind, idem) {
			w.dropWAL(lr.seq)
			continue
		}
		w.replayMu.RLock()
		fn := w.replay[kind]
		w.replayMu.RUnlock()
		if fn == nil {
			logger.Fallback().Warn("asyncwrite: 无重放函数，保留 WAL 记录",
				"kind", kind, "key", key)
			continue
		}
		if err := fn(ctx, key, payload); err != nil {
			logger.Fallback().Error("asyncwrite: WAL 重放失败",
				"kind", kind, "key", key, "err", err)
			continue
		}
		w.dropWAL(lr.seq)
	}
}

// commitLoop 周期性触发组提交，并在累积记录达到 MaxBatch 时立即提交。
// 同时按 statsLogInterval 周期输出运行统计（仅在 Dropped/Retried 变化时），
// 让「绝不静默丢弃」在生产中可观测（P2-6）。
func (w *Writer) commitLoop(ctx context.Context) {
	defer w.wg.Done()
	t := time.NewTicker(w.cfg.FSyncInterval)
	defer t.Stop()
	statsT := time.NewTicker(statsLogInterval)
	defer statsT.Stop()
	var lastDropped, lastRetried int64
	for {
		select {
		case <-t.C:
			w.commit(false)
		case <-w.commitCh:
			w.commit(false)
		case <-statsT.C:
			dropped, retried := w.dropped.Load(), w.retried.Load()
			if dropped != lastDropped || retried != lastRetried {
				logger.Fallback().Warn("asyncwrite: 运行统计",
					"dropped", dropped, "retried", retried, "depth", w.depth.Load())
				lastDropped, lastRetried = dropped, retried
			}
		case <-w.stopCh:
			return
		case <-ctx.Done():
			return
		}
	}
}

// finalize 做最终提交、fsync 并关闭文件描述符。
func (w *Writer) finalize() error {
	w.commit(true)
	w.walMu.Lock()
	if w.walFile != nil {
		if err := w.walFile.Sync(); err != nil {
			logger.Fallback().Error("asyncwrite: 最终 fsync 失败", "err", err)
		}
		if err := w.walFile.Close(); err != nil {
			logger.Fallback().Error("asyncwrite: 关闭 WAL 失败", "err", err)
		}
		w.walFile = nil
	}
	w.walMu.Unlock()
	return nil
}

// appendWAL 追加一条 WAL 记录并返回其内存序号（显式内存模式返回 0）。
// 若配置了 WALPath 却不可用（打开/重开失败），返回 ErrWALUnavailable——绝不由调用方静默降级为内存模式。
func (w *Writer) appendWAL(job Job) (uint64, error) {
	w.walMu.Lock()
	defer w.walMu.Unlock()
	if w.walFile == nil {
		if w.cfg.WALPath != "" {
			// 配置了 WAL 却不可用：拒绝写入，避免无兜底的静默丢弃。
			return 0, ErrWALUnavailable
		}
		return 0, nil
	}
	rec := encodeRecord(job.Kind, job.Key, job.IdempotencyKey, job.Payload)
	if _, err := w.walFile.Write(rec); err != nil {
		logger.Fallback().Error("asyncwrite: 写 WAL 失败", "err", err, "kind", job.Kind, "key", job.Key)
		return 0, err
	}
	seq := w.nextSeq
	w.nextSeq++
	w.pending[seq] = rec
	w.pendingOrder = append(w.pendingOrder, seq)
	w.appendedSince++
	if w.appendedSince >= w.cfg.MaxBatch {
		select {
		case w.commitCh <- struct{}{}:
		default:
		}
	}
	return seq, nil
}

// dropWAL 标记序号对应记录已完成（不再需要持久化）。
func (w *Writer) dropWAL(seq uint64) {
	w.walMu.Lock()
	defer w.walMu.Unlock()
	if w.walFile == nil {
		return
	}
	if _, ok := w.pending[seq]; ok {
		delete(w.pending, seq)
		w.walDirty = true
	}
}

// commit 执行一次组提交：fsync 当前 WAL，并在有已完成记录或 force 时，
// 用 tmp + os.Rename 重写 WAL，丢弃已完成记录。
func (w *Writer) commit(force bool) {
	w.walMu.Lock()
	defer w.walMu.Unlock()
	if w.walFile == nil {
		return
	}

	if err := w.walFile.Sync(); err != nil {
		logger.Fallback().Error("asyncwrite: WAL fsync 失败", "err", err)
	}
	w.recordFSync()
	w.appendedSince = 0

	if !w.walDirty && !force {
		return
	}

	var buf []byte
	kept := make([]uint64, 0, len(w.pending))
	for _, seq := range w.pendingOrder {
		rec, ok := w.pending[seq]
		if !ok {
			continue
		}
		buf = append(buf, rec...)
		kept = append(kept, seq)
	}

	tmp := w.cfg.WALPath + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		logger.Fallback().Error("asyncwrite: 创建 WAL 临时文件失败", "err", err)
		return
	}
	if _, err := f.Write(buf); err != nil {
		logger.Fallback().Error("asyncwrite: 写入 WAL 临时文件失败", "err", err)
		_ = f.Close()
		return
	}
	if err := f.Sync(); err != nil {
		logger.Fallback().Error("asyncwrite: WAL 临时文件 fsync 失败", "err", err)
	}
	w.recordFSync()
	_ = f.Close()

	if err := os.Rename(tmp, w.cfg.WALPath); err != nil {
		logger.Fallback().Error("asyncwrite: WAL 重命名失败", "err", err)
		return
	}
	nf, err := os.OpenFile(w.cfg.WALPath, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		logger.Fallback().Error("asyncwrite: 重开 WAL 失败", "err", err)
		_ = w.walFile.Close()
		w.walFile = nil
		return
	}
	_ = w.walFile.Close()
	w.walFile = nf
	w.pendingOrder = kept
	w.walDirty = false
}

// recordFSync 记录 fsync 时间并回调测试钩子（实例钩子，避免包级共享状态竞态）。
func (w *Writer) recordFSync() {
	w.lastSync.Store(time.Now().UnixNano())
	if w.fsyncCounter != nil {
		w.fsyncCounter()
	}
}

// shardIndex 用 FNV-1a 把 Key 映射到固定分片，保证同 Key 同分片。
func shardIndex(key string, n int) int {
	if n <= 1 {
		return 0
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return int(h.Sum32() % uint32(n))
}

// 记录格式：[Len uint32][Kind uint32][KeyLen uint32][Key][IdemLen uint32][Idem][PayloadLen uint32][Payload]
// 其中 Len 为整条记录（含自身 4 字节）的总长度。

func encodeRecord(kind Kind, key, idem string, payload []byte) []byte {
	body := 4 + 4 + len(key) + 4 + len(idem) + 4 + len(payload)
	total := 4 + body
	b := make([]byte, total)
	binary.BigEndian.PutUint32(b[0:], uint32(total))
	off := 4
	binary.BigEndian.PutUint32(b[off:], uint32(kind))
	off += 4
	binary.BigEndian.PutUint32(b[off:], uint32(len(key)))
	off += 4
	off += copy(b[off:], key)
	binary.BigEndian.PutUint32(b[off:], uint32(len(idem)))
	off += 4
	off += copy(b[off:], idem)
	binary.BigEndian.PutUint32(b[off:], uint32(len(payload)))
	off += 4
	copy(b[off:], payload)
	return b
}

func parseRecord(rec []byte) (Kind, string, string, []byte, error) {
	if len(rec) < 4 {
		return 0, "", "", nil, errors.New("asyncwrite: 记录过短")
	}
	total := int(binary.BigEndian.Uint32(rec[0:]))
	if total != len(rec) {
		return 0, "", "", nil, errors.New("asyncwrite: 记录长度不一致")
	}
	off := 4
	readU32 := func() (uint32, error) {
		if off+4 > len(rec) {
			return 0, errors.New("asyncwrite: 记录字段越界")
		}
		v := binary.BigEndian.Uint32(rec[off:])
		off += 4
		return v, nil
	}
	readStr := func() (string, error) {
		n, err := readU32()
		if err != nil {
			return "", err
		}
		if off+int(n) > len(rec) {
			return "", errors.New("asyncwrite: 字符串越界")
		}
		s := string(rec[off : off+int(n)])
		off += int(n)
		return s, nil
	}

	kind, err := readU32()
	if err != nil {
		return 0, "", "", nil, err
	}
	key, err := readStr()
	if err != nil {
		return 0, "", "", nil, err
	}
	idem, err := readStr()
	if err != nil {
		return 0, "", "", nil, err
	}
	plen, err := readU32()
	if err != nil {
		return 0, "", "", nil, err
	}
	if off+int(plen) > len(rec) {
		return 0, "", "", nil, errors.New("asyncwrite: 负载越界")
	}
	payload := make([]byte, plen)
	copy(payload, rec[off:off+int(plen)])
	return Kind(kind), key, idem, payload, nil
}

// readWALRecords 读取整条 WAL，遇到最后一条半写记录（崩溃中断）即停止。
func readWALRecords(path string) ([][]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out [][]byte
	off := 0
	for off < len(data) {
		if len(data)-off < 4 {
			break
		}
		total := int(binary.BigEndian.Uint32(data[off:]))
		if total < 4 || off+total > len(data) {
			break
		}
		out = append(out, data[off:off+total])
		off += total
	}
	return out, nil
}
