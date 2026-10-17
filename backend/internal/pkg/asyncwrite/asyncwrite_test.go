package asyncwrite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// waitCond 轮询等待条件成立，超时则判失败。
func waitCond(t *testing.T, d time.Duration, msg string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("等待超时: %s", msg)
}

// waitDepthZero 等待 Depth 归零。
func waitDepthZero(t *testing.T, w *Writer, d time.Duration) {
	t.Helper()
	waitCond(t, d, "Depth 未归零", func() bool { return w.Stats().Depth == 0 })
}

// waitWG 等待 WaitGroup 完成，带超时。
func waitWG(t *testing.T, wg *sync.WaitGroup, d time.Duration) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("WaitGroup 等待超时")
	}
}

func TestSubmitNonBlockingAndDepth(t *testing.T) {
	ctx := context.Background()
	w := New(Config{
		WALPath:    filepath.Join(t.TempDir(), "wal.log"),
		AsyncKinds: map[Kind]bool{KindSession: true},
	})
	w.Start(ctx)
	defer func() { _ = w.Shutdown(ctx) }()

	const n = 20
	gate := make(chan struct{})
	var started int64
	for i := 0; i < n; i++ {
		i := i
		err := w.Submit(Job{
			Kind: KindSession,
			Key:  fmt.Sprintf("s-%d", i),
			Exec: func(ctx context.Context) error {
				atomic.AddInt64(&started, 1)
				<-gate
				return nil
			},
		})
		if err != nil {
			t.Fatalf("提交失败: %v", err)
		}
	}

	if got := w.Stats().Depth; got <= 0 || got > n {
		t.Fatalf("Depth=%d 不在 (0,%d] 区间", got, n)
	}

	close(gate)
	waitDepthZero(t, w, 3*time.Second)
	if got := w.Stats().Depth; got != 0 {
		t.Fatalf("最终 Depth=%d，期望 0", got)
	}
}

func TestSameKeyFIFO(t *testing.T) {
	ctx := context.Background()
	w := New(Config{
		WALPath:    filepath.Join(t.TempDir(), "wal.log"),
		AsyncKinds: map[Kind]bool{KindSession: true},
	})
	w.Start(ctx)
	defer func() { _ = w.Shutdown(ctx) }()

	const n = 50
	var mu sync.Mutex
	var order []int
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		err := w.Submit(Job{
			Kind: KindSession,
			Key:  "same-key",
			Exec: func(ctx context.Context) error {
				mu.Lock()
				order = append(order, i)
				mu.Unlock()
				wg.Done()
				return nil
			},
		})
		if err != nil {
			t.Fatalf("提交失败: %v", err)
		}
	}
	waitWG(t, &wg, 3*time.Second)

	mu.Lock()
	defer mu.Unlock()
	if len(order) != n {
		t.Fatalf("执行数量=%d，期望 %d", len(order), n)
	}
	for i := 0; i < n; i++ {
		if order[i] != i {
			t.Fatalf("同 Key 未保序，order=%v", order)
		}
	}
}

func TestDifferentKeyConcurrent(t *testing.T) {
	ctx := context.Background()
	w := New(Config{
		WALPath:    filepath.Join(t.TempDir(), "wal.log"),
		AsyncKinds: map[Kind]bool{KindCallLog: true},
		QueueSize:  8192,
	})
	w.Start(ctx)
	defer func() { _ = w.Shutdown(ctx) }()

	const n = 300
	var count int64
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		i := i
		err := w.Submit(Job{
			Kind: KindCallLog,
			Key:  fmt.Sprintf("k-%d", i),
			Exec: func(ctx context.Context) error {
				atomic.AddInt64(&count, 1)
				wg.Done()
				return nil
			},
		})
		if err != nil {
			t.Fatalf("提交失败: %v", err)
		}
	}
	waitWG(t, &wg, 5*time.Second)

	if got := atomic.LoadInt64(&count); got != n {
		t.Fatalf("执行总数=%d，期望 %d", got, n)
	}
}

func TestReplayAfterCrash(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	walPath := filepath.Join(dir, "wal.log")

	const n = 10
	// 第一个 writer：不 Start、不 Shutdown，模拟进程崩溃，任务仅落在 WAL。
	w1 := New(Config{
		WALPath:    walPath,
		AsyncKinds: map[Kind]bool{KindCallLog: true},
		QueueSize:  1024,
	})
	for i := 0; i < n; i++ {
		i := i
		err := w1.Submit(Job{
			Kind:           KindCallLog,
			Key:            fmt.Sprintf("k-%d", i),
			IdempotencyKey: fmt.Sprintf("idem-%d", i),
			Payload:        []byte(fmt.Sprintf("payload-%d", i)),
			Exec:           func(ctx context.Context) error { return nil },
		})
		if err != nil {
			t.Fatalf("提交失败: %v", err)
		}
	}
	// 断言崩溃前 WAL 中确有记录。
	recs, err := readWALRecords(walPath)
	if err != nil {
		t.Fatalf("读取 WAL 失败: %v", err)
	}
	if len(recs) != n {
		t.Fatalf("崩溃前 WAL 记录数=%d，期望 %d", len(recs), n)
	}

	// 第二个 writer：同一 WALPath，启动后应完成重放。
	w2 := New(Config{
		WALPath:    walPath,
		AsyncKinds: map[Kind]bool{KindCallLog: true},
		QueueSize:  1024,
	})
	var replayed int64
	w2.SetReplayFunc(KindCallLog, func(ctx context.Context, key string, payload []byte) error {
		atomic.AddInt64(&replayed, 1)
		return nil
	})
	w2.Start(ctx)

	waitCond(t, 3*time.Second, "重放未完成", func() bool {
		return atomic.LoadInt64(&replayed) == n
	})
	if got := atomic.LoadInt64(&replayed); got != n {
		t.Fatalf("重放次数=%d，期望 %d", got, n)
	}
	if err := w2.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown 失败: %v", err)
	}
	// 重放完成后 WAL 应被清空。
	recs2, err := readWALRecords(walPath)
	if err != nil {
		t.Fatalf("读取 WAL 失败: %v", err)
	}
	if len(recs2) != 0 {
		t.Fatalf("重放后 WAL 仍剩 %d 条记录", len(recs2))
	}
}

func TestIdempotent(t *testing.T) {
	ctx := context.Background()
	w := New(Config{
		WALPath:    filepath.Join(t.TempDir(), "wal.log"),
		AsyncKinds: map[Kind]bool{KindCallLog: true},
	})
	w.Start(ctx)
	defer func() { _ = w.Shutdown(ctx) }()

	var count int64
	for i := 0; i < 20; i++ {
		i := i
		err := w.Submit(Job{
			Kind:           KindCallLog,
			Key:            fmt.Sprintf("k-%d", i),
			IdempotencyKey: "same-idempotency-key",
			Exec: func(ctx context.Context) error {
				atomic.AddInt64(&count, 1)
				return nil
			},
		})
		if err != nil {
			t.Fatalf("提交失败: %v", err)
		}
	}

	waitCond(t, 3*time.Second, "未观察到首次执行", func() bool {
		return atomic.LoadInt64(&count) >= 1
	})
	waitDepthZero(t, w, 3*time.Second)
	if got := atomic.LoadInt64(&count); got != 1 {
		t.Fatalf("幂等去重失败，执行次数=%d，期望 1", got)
	}
}

func TestBatchFSync(t *testing.T) {
	ctx := context.Background()
	var fsyncs int64

	walPath := filepath.Join(t.TempDir(), "wal.log")
	w := New(Config{
		WALPath:    walPath,
		AsyncKinds: map[Kind]bool{KindCallLog: true},
		MaxBatch:   100,
		QueueSize:  8192,
	})
	// 实例级测试钩子（取代旧包级变量，避免并行测试竞态），须在 Start 前设置。
	w.fsyncCounter = func() { atomic.AddInt64(&fsyncs, 1) }
	w.Start(ctx)

	const n = 500
	var done int64
	for i := 0; i < n; i++ {
		i := i
		err := w.Submit(Job{
			Kind: KindCallLog,
			Key:  fmt.Sprintf("k-%d", i),
			Exec: func(ctx context.Context) error {
				atomic.AddInt64(&done, 1)
				return nil
			},
		})
		if err != nil {
			t.Fatalf("提交失败: %v", err)
		}
	}

	waitCond(t, 5*time.Second, "任务未全部执行", func() bool {
		return atomic.LoadInt64(&done) == n
	})
	waitCond(t, 3*time.Second, "未发生 fsync", func() bool {
		return !w.Stats().LastFSync.IsZero()
	})
	if err := w.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown 失败: %v", err)
	}

	if w.Stats().LastFSync.IsZero() {
		t.Fatal("LastFSync 仍为零值")
	}
	c := atomic.LoadInt64(&fsyncs)
	if c == 0 || c >= n {
		t.Fatalf("fsync 次数=%d，应远小于提交数 %d", c, n)
	}
}

func TestShutdownFlush(t *testing.T) {
	ctx := context.Background()
	walPath := filepath.Join(t.TempDir(), "wal.log")
	w := New(Config{
		WALPath:    walPath,
		AsyncKinds: map[Kind]bool{KindSession: true},
	})
	w.Start(ctx)

	const n = 100
	var done int64
	for i := 0; i < n; i++ {
		i := i
		err := w.Submit(Job{
			Kind: KindSession,
			Key:  fmt.Sprintf("s-%d", i),
			Exec: func(ctx context.Context) error {
				atomic.AddInt64(&done, 1)
				return nil
			},
		})
		if err != nil {
			t.Fatalf("提交失败: %v", err)
		}
	}

	if err := w.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown 失败: %v", err)
	}
	if got := w.Stats().Depth; got != 0 {
		t.Fatalf("Shutdown 后 Depth=%d，期望 0", got)
	}
	if got := atomic.LoadInt64(&done); got != n {
		t.Fatalf("Shutdown 后完成数=%d，期望 %d", got, n)
	}
	recs, err := readWALRecords(walPath)
	if err != nil {
		t.Fatalf("读取 WAL 失败: %v", err)
	}
	if len(recs) != 0 {
		t.Fatalf("Shutdown 后 WAL 仍有 %d 条待处理记录", len(recs))
	}

	// Shutdown 之后提交必须报错。
	if err := w.Submit(Job{Kind: KindSession, Key: "x", Exec: func(ctx context.Context) error { return nil }}); err == nil {
		t.Fatal("Shutdown 后 Submit 应返回错误")
	}
}

func TestAsyncKindSwitch(t *testing.T) {
	ctx := context.Background()
	sentinel := errors.New("同步执行失败")

	// 类别未开启异步（map 中存在其他键）-> 同步执行并返回错误。
	w := New(Config{AsyncKinds: map[Kind]bool{KindCallLog: true}})
	if err := w.Submit(Job{Kind: KindSession, Key: "k", Exec: func(ctx context.Context) error { return sentinel }}); !errors.Is(err, sentinel) {
		t.Fatalf("同步模式应返回 Exec 错误，实际 err=%v", err)
	}

	// AsyncKinds 为 nil -> 全部视为同步。
	w2 := New(Config{})
	if err := w2.Submit(Job{Kind: KindSession, Key: "k", Exec: func(ctx context.Context) error { return sentinel }}); !errors.Is(err, sentinel) {
		t.Fatalf("nil AsyncKinds 应同步返回 Exec 错误，实际 err=%v", err)
	}

	// 同步执行不受 Shutdown 影响：未启用异步则无 WAL、无协程。
	if err := w.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown 失败: %v", err)
	}
}

func TestMemoryOnlyNoSpool(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	// WALPath 为空 -> 纯内存模式。
	w := New(Config{AsyncKinds: map[Kind]bool{KindTokenLastUsed: true}})
	if w.walFile != nil {
		t.Fatal("内存模式不应打开 WAL 文件")
	}
	w.Start(ctx)

	var done int64
	for i := 0; i < 50; i++ {
		i := i
		err := w.Submit(Job{
			Kind: KindTokenLastUsed,
			Key:  fmt.Sprintf("t-%d", i),
			Exec: func(ctx context.Context) error {
				atomic.AddInt64(&done, 1)
				return nil
			},
		})
		if err != nil {
			t.Fatalf("提交失败: %v", err)
		}
	}
	waitCond(t, 3*time.Second, "任务未全部执行", func() bool {
		return atomic.LoadInt64(&done) == 50
	})
	if err := w.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown 失败: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("读取目录失败: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("内存模式不应创建任何文件，实际=%v", entries)
	}
}

func TestConcurrentRace(t *testing.T) {
	ctx := context.Background()
	w := New(Config{
		WALPath:    filepath.Join(t.TempDir(), "wal.log"),
		AsyncKinds: map[Kind]bool{KindCallLog: true},
		QueueSize:  16384,
	})
	w.Start(ctx)

	const workers = 8
	const per = 100
	var done int64
	var gwg sync.WaitGroup
	gwg.Add(workers)
	for k := 0; k < workers; k++ {
		k := k
		go func() {
			defer gwg.Done()
			for i := 0; i < per; i++ {
				key := fmt.Sprintf("k-%d", (i+k)%5)
				exec := func(ctx context.Context) error {
					atomic.AddInt64(&done, 1)
					return nil
				}
				if err := w.Submit(Job{Kind: KindCallLog, Key: key, Exec: exec}); err != nil {
					return
				}
				_ = w.Stats()
			}
		}()
	}
	waitWG(t, &gwg, 10*time.Second)

	waitCond(t, 10*time.Second, "执行数不足", func() bool {
		return atomic.LoadInt64(&done) == workers*per
	})
	if err := w.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown 失败: %v", err)
	}
	if got := atomic.LoadInt64(&done); got != workers*per {
		t.Fatalf("执行总数=%d，期望 %d", got, workers*per)
	}
}

// TestWALRetainedOnFailure 锁定「绝不静默丢弃」契约：
// 执行持续失败（重试耗尽）时，WAL 记录必须保留，Shutdown 后记录仍在文件中；
// 重启并注册可成功的重放函数后，记录被重放（不丢）。
func TestWALRetainedOnFailure(t *testing.T) {
	dir := t.TempDir()
	walPath := filepath.Join(dir, "asyncwrite.wal")

	base := context.Background()
	w := New(Config{
		WALPath:    walPath,
		AsyncKinds: map[Kind]bool{KindCallLog: true},
	})
	w.Start(base)

	failExec := func(ctx context.Context) error { return errors.New("上游持续失败") }
	if err := w.Submit(Job{
		Kind: KindCallLog, Key: "k1", IdempotencyKey: "idem-keep",
		Payload: []byte("payload-keep"), Exec: failExec,
	}); err != nil {
		t.Fatalf("Submit 失败: %v", err)
	}
	waitCond(t, 5*time.Second, "重试耗尽未发生", func() bool {
		return w.Stats().Retried >= 1
	})
	if err := w.Shutdown(base); err != nil {
		t.Fatalf("Shutdown 失败: %v", err)
	}

	// 失败记录必须仍在 WAL 中（未被删除）。
	data, err := os.ReadFile(walPath)
	if err != nil {
		t.Fatalf("读取 WAL 失败: %v", err)
	}
	if !bytes.Contains(data, []byte("idem-keep")) {
		t.Fatalf("失败任务的 WAL 记录不应被丢弃，实际文件=%q", string(data))
	}

	// 重启后注册可成功的重放函数，记录应被重放。
	var replayed int64
	w2 := New(Config{
		WALPath:    walPath,
		AsyncKinds: map[Kind]bool{KindCallLog: true},
	})
	w2.SetReplayFunc(KindCallLog, func(ctx context.Context, key string, payload []byte) error {
		atomic.AddInt64(&replayed, 1)
		return nil
	})
	w2.Start(base)
	waitCond(t, 5*time.Second, "重启后未重放保留记录", func() bool {
		return atomic.LoadInt64(&replayed) == 1
	})
	if err := w2.Shutdown(base); err != nil {
		t.Fatalf("w2 Shutdown 失败: %v", err)
	}
}

// TestSubmitBackpressureKeepsWAL 锁定 P1-1：队列背压溢出时 Submit 必须返回非 nil 错误，
// 且**不得删除**已持久化的 WAL 记录（留待重启重放），避免已落盘的审计/会话/事件/调用日志静默丢失。
func TestSubmitBackpressureKeepsWAL(t *testing.T) {
	walPath := filepath.Join(t.TempDir(), "wal.log")
	w := New(Config{
		WALPath:    walPath,
		AsyncKinds: map[Kind]bool{KindCallLog: true},
		Shards:     1,
		QueueSize:  1, // 每分片容量 1
	})
	// 故意不 Start：工作协程不消费队列，第二次提交必然触发背压超时。
	if err := w.Submit(Job{
		Kind: KindCallLog, Key: "k", IdempotencyKey: "idem-1", Payload: []byte("p1"),
		Exec: func(context.Context) error { return nil },
	}); err != nil {
		t.Fatalf("首个提交失败: %v", err)
	}
	err := w.Submit(Job{
		Kind: KindCallLog, Key: "k", IdempotencyKey: "idem-2", Payload: []byte("p2"),
		Exec: func(context.Context) error { return nil },
	})
	if !errors.Is(err, ErrBackpressure) {
		t.Fatalf("背压溢出应返回 ErrBackpressure，got %v", err)
	}
	if got := w.Stats().Dropped; got != 1 {
		t.Fatalf("Dropped=%d，期望 1", got)
	}
	// 关键断言：被背压拒绝的任务其 WAL 记录必须保留。
	data, rerr := os.ReadFile(walPath)
	if rerr != nil {
		t.Fatalf("读取 WAL 失败: %v", rerr)
	}
	if !bytes.Contains(data, []byte("idem-2")) {
		t.Fatalf("背压溢出不得删除 WAL 记录，实际文件=%q", string(data))
	}
	if err := w.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown 失败: %v", err)
	}
}

// TestSubmitWALWriteErrorNotQueued 锁定 P1-2：WAL 写入失败时必须返回错误且不入队、不执行 Exec，
// 绝不把「未持久化」的任务伪称为已持久化。
func TestSubmitWALWriteErrorNotQueued(t *testing.T) {
	walPath := filepath.Join(t.TempDir(), "wal.log")
	w := New(Config{
		WALPath:    walPath,
		AsyncKinds: map[Kind]bool{KindCallLog: true},
	})
	// 用只读句柄替换 WAL 文件，强制 appendWAL 写失败（Linux 下写只读 fd 返回 EBADF）。
	ro, oerr := os.Open(walPath)
	if oerr != nil {
		t.Fatalf("只读打开 WAL 失败: %v", oerr)
	}
	defer ro.Close()
	w.walMu.Lock()
	if w.walFile != nil {
		_ = w.walFile.Close()
	}
	w.walFile = ro
	w.walMu.Unlock()

	var ran int64
	err := w.Submit(Job{
		Kind: KindCallLog, Key: "k",
		Exec: func(context.Context) error { atomic.AddInt64(&ran, 1); return nil },
	})
	if err == nil {
		t.Fatal("WAL 写失败时 Submit 必须返回错误")
	}
	if got := w.Stats().Depth; got != 0 {
		t.Fatalf("WAL 写失败不应入队，Depth=%d", got)
	}
	if atomic.LoadInt64(&ran) != 0 {
		t.Fatal("WAL 写失败不应执行 Exec")
	}
}

// TestIdemSetBounded 锁定 P1-3：幂等去重集合有界（FIFO 淘汰），不发生无界增长。
func TestIdemSetBounded(t *testing.T) {
	w := New(Config{AsyncKinds: map[Kind]bool{KindCallLog: true}})
	w.idemMax = 8
	for i := 0; i < 100; i++ {
		if !w.claim(KindCallLog, fmt.Sprintf("idem-%d", i)) {
			t.Fatalf("首次占用不应返回 false: %d", i)
		}
	}
	w.idemMu.Lock()
	got := len(w.done)
	w.idemMu.Unlock()
	if got > 8 {
		t.Fatalf("去重集合应有界（<=8），got %d", got)
	}
	// 最早键已被 FIFO 淘汰：再次占用应成功（证明淘汰生效而非仅截断计数）。
	if !w.claim(KindCallLog, "idem-0") {
		t.Fatal("被 FIFO 淘汰的键应可再次占用")
	}
}

// TestIdemKeyNamespacedByKind 锁定 P1-3：去重键带 Kind 命名空间，跨类别同名字符串互不误判。
func TestIdemKeyNamespacedByKind(t *testing.T) {
	w := New(Config{AsyncKinds: map[Kind]bool{KindCallLog: true, KindSession: true}})
	if !w.claim(KindCallLog, "shared") {
		t.Fatal("KindCallLog 首次占用应成功")
	}
	if !w.claim(KindSession, "shared") {
		t.Fatal("不同 Kind 下同名 idem 不应被误判为重复")
	}
	if w.claim(KindCallLog, "shared") {
		t.Fatal("同 Kind 同 idem 应判为重复")
	}
}
