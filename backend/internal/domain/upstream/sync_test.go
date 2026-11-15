package upstream

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// testLogger 丢弃日志（-race 下 slog 输出会把测试结果淹掉）。
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// fakeSleep 只记录退避时长，不真的睡（否则一条用例要等 85 秒）。
type fakeSleep struct {
	mu    sync.Mutex
	waits []time.Duration
}

func (f *fakeSleep) sleep(ctx context.Context, d time.Duration) error {
	f.mu.Lock()
	f.waits = append(f.waits, d)
	f.mu.Unlock()
	return nil
}

func (f *fakeSleep) recorded() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.waits...)
}

// TestSyncer_SerialSteps 断言单轮内步骤严格串行且按注册顺序执行（不并发、不乱序）。
func TestSyncer_SerialSteps(t *testing.T) {
	s := NewSyncer(SyncerOptions{Logger: testLogger(), Sleep: (&fakeSleep{}).sleep})

	var active int32
	var mu sync.Mutex
	var order []string
	mk := func(name string) Step {
		return StepFunc{N: name, F: func(context.Context) error {
			if n := atomic.AddInt32(&active, 1); n != 1 {
				t.Errorf("步骤 %s 与其他步骤并发（active=%d）", name, n)
			}
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt32(&active, -1)
			return nil
		}}
	}
	for _, name := range []string{"models", "groups", "providers"} {
		if err := s.Register(mk(name)); err != nil {
			t.Fatalf("注册步骤失败: %v", err)
		}
	}
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("全部成功应返回 nil，got %v", err)
	}
	got := []string{"models", "groups", "providers"}
	if len(order) != len(got) {
		t.Fatalf("执行步骤数不符：got %v", order)
	}
	for i := range got {
		if order[i] != got[i] {
			t.Fatalf("步骤顺序不符：got %v want %v", order, got)
		}
	}
}

// TestSyncer_ConcurrentRunRejected 断言上一轮未结束时新的触发被拒绝（ErrBusy，不排队不并发）。
func TestSyncer_ConcurrentRunRejected(t *testing.T) {
	s := NewSyncer(SyncerOptions{Logger: testLogger(), Sleep: (&fakeSleep{}).sleep})
	entered := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	if err := s.Register(StepFunc{N: "models", F: func(context.Context) error {
		once.Do(func() { close(entered) })
		<-release
		return nil
	}}); err != nil {
		t.Fatalf("注册步骤失败: %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- s.RunOnce(context.Background()) }()
	<-entered

	// 第二次触发放进 goroutine + 超时：若串行守卫被删掉，这里会阻塞（等 release）而不是立即返回，
	// 用例必须快速变红而不是把 CI 挂死。
	second := make(chan error, 1)
	go func() { second <- s.RunOnce(context.Background()) }()
	var err error
	select {
	case err = <-second:
	case <-time.After(2 * time.Second):
		t.Fatal("并发触发被阻塞：串行守卫失效（应在上一轮未结束时立即返回 ErrBusy）")
	}
	if !IsSkip(err) {
		t.Fatalf("并发触发应返回跳过（ErrBusy），got %T %v", err, err)
	}
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("并发触发应是 ErrBusy，got %v", err)
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("第一轮应成功，got %v", err)
	}
	// 上一轮结束后必须能再次运行（占用标记已释放，不是一次性的）。
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("上一轮已结束，第二轮应能执行，got %v", err)
	}
}

// TestSyncer_RetryBackoffAndGiveUp 断言单步失败按 5s/20s/60s 重试 3 次后放弃，
// 且耗尽不影响同轮后续步骤（一个接口挂不拖死整轮）。
func TestSyncer_RetryBackoffAndGiveUp(t *testing.T) {
	sleep := &fakeSleep{}
	s := NewSyncer(SyncerOptions{Logger: testLogger(), Sleep: sleep.sleep})
	upstream := errors.New("upstream 503")
	var calls int32
	var secondRan int32
	if err := s.Register(StepFunc{N: "models", F: func(context.Context) error {
		atomic.AddInt32(&calls, 1)
		return upstream
	}}); err != nil {
		t.Fatalf("注册步骤失败: %v", err)
	}
	if err := s.Register(StepFunc{N: "ledger", F: func(context.Context) error {
		atomic.AddInt32(&secondRan, 1)
		return nil
	}}); err != nil {
		t.Fatalf("注册步骤失败: %v", err)
	}

	err := s.RunOnce(context.Background())
	if err == nil {
		t.Fatal("有步骤耗尽应返回错误")
	}
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Fatalf("应调用 4 次（首次 + 重试 3 次），got %d", got)
	}
	want := []time.Duration{5 * time.Second, 20 * time.Second, 60 * time.Second}
	waits := sleep.recorded()
	if len(waits) != len(want) {
		t.Fatalf("退避次数不符：got %v want %v", waits, want)
	}
	for i := range want {
		if waits[i] != want[i] {
			t.Fatalf("退避序列不符：got %v want %v", waits, want)
		}
	}
	if atomic.LoadInt32(&secondRan) != 1 {
		t.Fatal("前一步耗尽后，后续步骤仍应执行")
	}
	if !errors.Is(err, upstream) {
		t.Fatalf("错误链应保留原始失败原因，got %v", err)
	}
	if !strings.Contains(err.Error(), "models") {
		t.Fatalf("错误文案应点名步骤，got %v", err)
	}
}

// TestSyncer_GateSkipsWithoutCallingSteps 断言闸门跳过时一步都不跑、也不退避（降级开关路径）。
func TestSyncer_GateSkipsWithoutCallingSteps(t *testing.T) {
	sleep := &fakeSleep{}
	s := NewSyncer(SyncerOptions{
		Logger: testLogger(),
		Sleep:  sleep.sleep,
		Gate: func(context.Context) error {
			return &SkipError{Op: "上游同步", Reason: ConfigKeyDegrade + "=true（降级）"}
		},
	})
	var ran int32
	if err := s.Register(StepFunc{N: "models", F: func(context.Context) error {
		atomic.AddInt32(&ran, 1)
		return nil
	}}); err != nil {
		t.Fatalf("注册步骤失败: %v", err)
	}
	err := s.RunOnce(context.Background())
	if !IsSkip(err) {
		t.Fatalf("闸门跳过应返回跳过错误，got %T %v", err, err)
	}
	if !strings.Contains(err.Error(), ConfigKeyDegrade) {
		t.Fatalf("跳过原因应点名配置键，got %v", err)
	}
	if atomic.LoadInt32(&ran) != 0 {
		t.Fatal("跳过时不应执行任何步骤")
	}
	if len(sleep.recorded()) != 0 {
		t.Fatal("跳过时不应退避")
	}
}

// TestSyncer_StartNeverOverlaps 断言定时循环也不会并发（「跑完再等」，不堆积 tick）。
func TestSyncer_StartNeverOverlaps(t *testing.T) {
	s := NewSyncer(SyncerOptions{Logger: testLogger(), Sleep: (&fakeSleep{}).sleep})
	var active, runs int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := s.Register(StepFunc{N: "models", F: func(context.Context) error {
		if n := atomic.AddInt32(&active, 1); n != 1 {
			t.Errorf("定时轮次发生重叠（active=%d）", n)
		}
		time.Sleep(5 * time.Millisecond)
		atomic.AddInt32(&active, -1)
		if atomic.AddInt32(&runs, 1) >= 3 {
			cancel()
		}
		return nil
	}}); err != nil {
		t.Fatalf("注册步骤失败: %v", err)
	}

	done := make(chan struct{})
	go func() { s.Start(ctx, time.Millisecond); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Start 未在 ctx 取消后退出")
	}
	if got := atomic.LoadInt32(&runs); got < 3 {
		t.Fatalf("应至少跑 3 轮，got %d", got)
	}
}

// TestSyncer_Register 断言注册校验（空名/重名）与顺序。
func TestSyncer_Register(t *testing.T) {
	s := NewSyncer(SyncerOptions{Logger: testLogger(), Sleep: (&fakeSleep{}).sleep})
	if err := s.Register(StepFunc{N: ""}); err == nil {
		t.Fatal("空步骤名应报错")
	}
	if err := s.Register(nil); err == nil {
		t.Fatal("空步骤应报错")
	}
	if err := s.Register(StepFunc{N: "models", F: func(context.Context) error { return nil }}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	if err := s.Register(StepFunc{N: "models", F: func(context.Context) error { return nil }}); err == nil {
		t.Fatal("重名步骤应报错")
	}
	steps := s.Steps()
	if len(steps) != 1 || steps[0] != "models" {
		t.Fatalf("步骤清单不符：%v", steps)
	}
}
