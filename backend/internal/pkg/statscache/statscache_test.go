package statscache

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// 100 个并发请求同一个 key：只回源 1 次，其余全部拿到同一份结果。
func TestCache_ConcurrentSameKeySingleLoad(t *testing.T) {
	c := New[int](time.Minute)
	var loads atomic.Int64
	release := make(chan struct{})

	const n = 100
	var wg sync.WaitGroup
	results := make([]int, n)
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			v, _, err := c.Get(context.Background(), "user:1", func(context.Context) (int, error) {
				loads.Add(1)
				<-release // 卡住回源，保证 100 个请求全部落在同一个 inflight 上
				return 42, nil
			})
			if err != nil {
				t.Errorf("get: %v", err)
				return
			}
			results[i] = v
		}(i)
	}

	// 等所有 goroutine 进入 Get（最多 2s），再放行回源。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if int(loads.Load()) == 1 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	close(release)
	wg.Wait()

	if got := loads.Load(); got != 1 {
		t.Fatalf("回源次数 = %d，期望 1（并发同键必须只落一次 DB）", got)
	}
	for i, v := range results {
		if v != 42 {
			t.Fatalf("第 %d 个请求拿到 %d，期望 42", i, v)
		}
	}
	st := c.Stats()
	if st.Loads != 1 {
		t.Fatalf("Loads = %d，期望 1", st.Loads)
	}
	// n 个请求里恰好 1 个负责回源，其余要么合并到 inflight（Shared），
	// 要么在回源返回后才进 Get、直接命中（Hits）——两种都证明没有多打 DB。
	// 不直接断言 Shared≈n-1：release 关掉后新进 Get 的 goroutine 会走命中分支，这是调度决定的。
	if got := st.Hits + st.Shared; got != n-1 {
		t.Fatalf("未回源的请求数 = %d，期望 %d（Hits=%d Shared=%d）", got, n-1, st.Hits, st.Shared)
	}
}

// 跨 key 隔离：A 的缓存不能被 B 读到（反向用例）。
func TestCache_KeysAreIsolated(t *testing.T) {
	c := New[string](time.Minute)
	loadA := func(context.Context) (string, error) { return "A 的钱包数据", nil }
	loadB := func(context.Context) (string, error) { return "B 的钱包数据", nil }

	a1, _, err := c.Get(context.Background(), "summary:user:1", loadA)
	if err != nil {
		t.Fatalf("get A: %v", err)
	}
	b1, _, err := c.Get(context.Background(), "summary:user:2", loadB)
	if err != nil {
		t.Fatalf("get B: %v", err)
	}
	if a1 == b1 {
		t.Fatalf("A 与 B 读到同一份数据（%q）——缓存键没有按用户隔离", a1)
	}

	// B 的键在缓存里，A 再读不能读到 B 的值。
	a2, fromCache, err := c.Get(context.Background(), "summary:user:1", loadB)
	if err != nil {
		t.Fatalf("get A again: %v", err)
	}
	if !fromCache || a2 != a1 {
		t.Fatalf("A 第二次读到 %q（fromCache=%v），期望命中自己的 %q", a2, fromCache, a1)
	}
	// 反向：把 A 的值塞进 B 的键也读不到——键不同就是不同条目。
	if _, _, err := c.Get(context.Background(), "summary:user:2", loadA); err != nil {
		t.Fatalf("get B again: %v", err)
	}
	if v, _, _ := c.Get(context.Background(), "summary:user:2", loadA); v != b1 {
		t.Fatalf("B 的值被串成 %q，期望 %q", v, b1)
	}
}

// TTL 到期后能刷新（用可控时钟，不真等 5 分钟）。
func TestCache_TTLExpiryRefreshes(t *testing.T) {
	c := New[int](5 * time.Minute)
	base := time.Date(2026, 11, 10, 22, 0, 0, 0, time.UTC)
	var mu sync.Mutex
	now := base
	c.SetClock(func() time.Time { mu.Lock(); defer mu.Unlock(); return now })

	calls := 0
	load := func(context.Context) (int, error) { calls++; return calls, nil }

	if v, _, _ := c.Get(context.Background(), "k", load); v != 1 {
		t.Fatalf("首次 = %d，期望 1", v)
	}
	// 4 分 59 秒：仍命中，不回源。
	mu.Lock()
	now = base.Add(4*time.Minute + 59*time.Second)
	mu.Unlock()
	if v, fromCache, _ := c.Get(context.Background(), "k", load); v != 1 || !fromCache {
		t.Fatalf("TTL 内 = %d（fromCache=%v），期望命中 1", v, fromCache)
	}
	// 越过 5 分钟：过期，必须刷新。
	mu.Lock()
	now = base.Add(5*time.Minute + time.Second)
	mu.Unlock()
	if v, fromCache, _ := c.Get(context.Background(), "k", load); v != 2 || fromCache {
		t.Fatalf("TTL 到期后 = %d（fromCache=%v），期望刷新为 2", v, fromCache)
	}
	if calls != 2 {
		t.Fatalf("回源次数 = %d，期望 2", calls)
	}
}

// 回源失败不写缓存：下一次仍会重新回源，且错误原样返回。
func TestCache_ErrorNotCached(t *testing.T) {
	c := New[int](time.Minute)
	boom := errors.New("db down")
	if _, _, err := c.Get(context.Background(), "k", func(context.Context) (int, error) {
		return 0, boom
	}); !errors.Is(err, boom) {
		t.Fatalf("err = %v，期望 boom", err)
	}
	v, fromCache, err := c.Get(context.Background(), "k", func(context.Context) (int, error) { return 7, nil })
	if err != nil || fromCache || v != 7 {
		t.Fatalf("第二次 = (%d, %v, %v)，期望回源拿到 7", v, fromCache, err)
	}
}

// 空结果也要缓存：没有数据的账号不能每次都打穿到 DB。
func TestCache_EmptyValueCached(t *testing.T) {
	c := New[[]int](time.Minute)
	loads := 0
	load := func(context.Context) ([]int, error) { loads++; return nil, nil }
	for i := 0; i < 5; i++ {
		if _, _, err := c.Get(context.Background(), "k", load); err != nil {
			t.Fatalf("get: %v", err)
		}
	}
	if loads != 1 {
		t.Fatalf("回源次数 = %d，期望 1（空结果同样缓存）", loads)
	}
}

// Invalidate 之后下一次读必须回源。
func TestCache_Invalidate(t *testing.T) {
	c := New[int](time.Minute)
	n := 0
	load := func(context.Context) (int, error) { n++; return n, nil }
	if v, _, _ := c.Get(context.Background(), "k", load); v != 1 {
		t.Fatalf("首次 = %d", v)
	}
	c.Invalidate("k")
	if v, fromCache, _ := c.Get(context.Background(), "k", load); v != 2 || fromCache {
		t.Fatalf("失效后 = %d（fromCache=%v），期望 2", v, fromCache)
	}
	c.Invalidate("不存在的键") // 不能 panic
}
