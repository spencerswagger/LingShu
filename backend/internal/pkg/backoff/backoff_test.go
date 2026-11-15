package backoff

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// recordSleep 是测试用的假等待：只记录时长，不真的睡（否则一条用例要等 85 秒）。
type recordSleep struct {
	waits []time.Duration
	// onWait 可选：模拟等待期间 ctx 被取消等异常。
	onWait func(ctx context.Context, d time.Duration) error
	calls  int
}

func (s *recordSleep) sleep(ctx context.Context, d time.Duration) error {
	s.calls++
	s.waits = append(s.waits, d)
	if s.onWait != nil {
		return s.onWait(ctx, d)
	}
	return nil
}

func wantWaits(t *testing.T, got []time.Duration, want ...time.Duration) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("退避次数不符：got %v want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("退避序列不符：got %v want %v", got, want)
		}
	}
}

// TestDefaultPolicy_Sequence 守住「5s/20s/60s + 重试 3 次」这个唯一口径不被改动。
func TestDefaultPolicy_Sequence(t *testing.T) {
	p := Default()
	if got := p.DelaysText(); got != "5s/20s/60s" {
		t.Fatalf("默认退避序列应为 5s/20s/60s，got %s", got)
	}
	if p.MaxRetries != 3 {
		t.Fatalf("默认重试次数应为 3，got %d", p.MaxRetries)
	}
	if got := p.Attempts(); got != 4 {
		t.Fatalf("重试 3 次 → 最多调用 4 次，got %d", got)
	}
	for i, want := range []time.Duration{5 * time.Second, 20 * time.Second, 60 * time.Second} {
		d, ok := p.DelayFor(i + 1)
		if !ok || d != want {
			t.Fatalf("第 %d 次重试应等 %s，got %s ok=%v", i+1, want, d, ok)
		}
	}
	if _, ok := p.DelayFor(4); ok {
		t.Fatal("第 4 次重试不应有退避项（重试上限 3）")
	}
}

// TestDo_RetrySequenceExactly 注入连续失败：调用 4 次、退避恰为 5s/20s/60s，然后放弃。
func TestDo_RetrySequenceExactly(t *testing.T) {
	calls := 0
	sleep := &recordSleep{}
	upstream := errors.New("upstream 429")

	err := Do(context.Background(), "拉取模型", Default(), sleep.sleep, func(ctx context.Context, attempt int) error {
		calls++
		return upstream
	})

	if calls != 4 {
		t.Fatalf("应调用 4 次（首次 + 重试 3 次），got %d", calls)
	}
	wantWaits(t, sleep.waits, 5*time.Second, 20*time.Second, 60*time.Second)

	var ex *ExhaustedError
	if !errors.As(err, &ex) {
		t.Fatalf("重试耗尽应返回 *ExhaustedError，got %T %v", err, err)
	}
	if ex.Op != "拉取模型" || ex.Attempts != 4 || ex.Retries != 3 {
		t.Fatalf("诊断信息不全：op=%q attempts=%d retries=%d", ex.Op, ex.Attempts, ex.Retries)
	}
	// 不吞错：原始错误必须能穿透。
	if !errors.Is(err, upstream) {
		t.Fatalf("错误链里应能取到原始错误，got %v", err)
	}
	// 文案要能诊断：操作名 + 次数 + 退避序列 + 原始错误。
	for _, want := range []string{"拉取模型", "4 次", "5s/20s/60s", "upstream 429"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误文案缺少 %q：%s", want, err.Error())
		}
	}
}

// TestDo_StopsAfterThirdRetry 断言「第 3 次重试之后不再调用、不再退避」——这是上限，不是建议值。
func TestDo_StopsAfterThirdRetry(t *testing.T) {
	calls := 0
	sleep := &recordSleep{}
	_ = Do(context.Background(), "拉取账本", Default(), sleep.sleep, func(ctx context.Context, attempt int) error {
		calls++
		if calls > 4 {
			t.Fatalf("超过上限仍在调用：第 %d 次", calls)
		}
		return errors.New("boom")
	})
	if calls != 4 {
		t.Fatalf("上限应为 4 次调用，got %d", calls)
	}
	if sleep.calls != 3 {
		t.Fatalf("应只退避 3 次，got %d", sleep.calls)
	}
}

// TestDo_SucceedsOnRetry 断言重试成功后不再继续（也不返回错误）。
func TestDo_SucceedsOnRetry(t *testing.T) {
	calls := 0
	sleep := &recordSleep{}
	err := Do(context.Background(), "拉取分组", Default(), sleep.sleep, func(ctx context.Context, attempt int) error {
		calls++
		if calls < 3 {
			return errors.New("暂时失败")
		}
		if attempt != 3 {
			t.Fatalf("attempt 序号应为 3，got %d", attempt)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("第 3 次成功应返回 nil，got %v", err)
	}
	wantWaits(t, sleep.waits, 5*time.Second, 20*time.Second)
}

// TestDo_CancelDuringWait 断言等待期间 ctx 取消：立刻返回，不再重试，且错误链可判 canceled。
func TestDo_CancelDuringWait(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	sleep := &recordSleep{}
	sleep.onWait = func(context.Context, time.Duration) error {
		cancel()
		return context.Canceled
	}
	last := errors.New("上游超时")
	err := Do(ctx, "拉取模型", Default(), sleep.sleep, func(context.Context, int) error {
		calls++
		return last
	})
	if calls != 1 {
		t.Fatalf("取消后不应再调用上游，got %d 次", calls)
	}
	var ce *CanceledError
	if !errors.As(err, &ce) {
		t.Fatalf("应返回 *CanceledError，got %T %v", err, err)
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, last) {
		t.Fatalf("错误链应同时保留 canceled 与原始错误，got %v", err)
	}
}

// TestDo_ZeroPolicyNoRetry 断言零值策略 = 只试一次（不静默放大成 4 次），错误原样返回。
func TestDo_ZeroPolicyNoRetry(t *testing.T) {
	calls := 0
	sleep := &recordSleep{}
	boom := errors.New("boom")
	err := Do(context.Background(), "单次探测", Policy{}, sleep.sleep, func(context.Context, int) error {
		calls++
		return boom
	})
	if calls != 1 {
		t.Fatalf("零值策略应只调用 1 次，got %d", calls)
	}
	if len(sleep.waits) != 0 {
		t.Fatalf("零值策略不应退避，got %v", sleep.waits)
	}
	if !errors.Is(err, boom) {
		t.Fatalf("单次路径应原样返回错误，got %v", err)
	}
	var ex *ExhaustedError
	if errors.As(err, &ex) {
		t.Fatal("单次路径不应包成「重试耗尽」")
	}
}

// TestPolicy_Normalized 断言非法策略被收敛（重试次数不得超过退避项数，非正值等待被丢弃）。
func TestPolicy_Normalized(t *testing.T) {
	p := Policy{Delays: []time.Duration{5 * time.Second, 0, 20 * time.Second}, MaxRetries: 9}.normalized()
	if p.MaxRetries != 2 {
		t.Fatalf("重试次数应被收敛到可用退避项数 2，got %d", p.MaxRetries)
	}
	if len(p.Delays) != 2 {
		t.Fatalf("非正值等待应被丢弃，got %v", p.Delays)
	}
	if got := (Policy{Delays: []time.Duration{5 * time.Second}, MaxRetries: -1}).normalized().MaxRetries; got != 0 {
		t.Fatalf("负重试次数应收敛为 0，got %d", got)
	}
}
