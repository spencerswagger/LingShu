package gencache

import (
	"fmt"
	"sync"
	"testing"
)

// assertConsistentLocked 校验 order 与 map 严格一致：每个存活键恰好出现一次。
// 仅测试（同包）可直接读取内部字段。
func assertConsistent(t *testing.T, c *Cache[int]) {
	t.Helper()
	if len(c.order) != len(c.m) {
		t.Fatalf("order 与 map 不一致：len(order)=%d len(map)=%d", len(c.order), len(c.m))
	}
	seen := make(map[string]bool, len(c.order))
	for _, k := range c.order {
		if seen[k] {
			t.Fatalf("order 中键重复：%q", k)
		}
		seen[k] = true
		if _, ok := c.m[k]; !ok {
			t.Fatalf("order 中存在 map 已删除的陈旧键：%q", k)
		}
	}
}

// TestCache_BoundedFIFO 锁定有界 FIFO：超过上限淘汰最早插入的键，且失效后顺序表同步收敛。
func TestCache_BoundedFIFO(t *testing.T) {
	c := New[int](3)
	for _, k := range []string{"a", "b", "c"} {
		if !c.PutAtGen(k, "", 1, c.Generation()) {
			t.Fatalf("put %q 应成功", k)
		}
	}
	if c.Count() != 3 {
		t.Fatalf("count=%d, want 3", c.Count())
	}
	assertConsistent(t, c)

	// 第 4 个键触发 FIFO：最早的 a 被淘汰。
	if !c.PutAtGen("d", "", 4, c.Generation()) {
		t.Fatal("put d 应成功")
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("最早的键 a 应被 FIFO 淘汰")
	}
	for _, k := range []string{"b", "c", "d"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("%q 应仍存活", k)
		}
	}
	if c.Count() != 3 {
		t.Fatalf("count=%d, want 3", c.Count())
	}
	assertConsistent(t, c)

	// 失效 b 后顺序表同步摘除；assertConsistent 校验 order 与 map 严格一致：
	// 若失效未从顺序表摘除 b，len(order) 会大于 len(map) 而立即失败。
	c.Invalidate("b")
	if _, ok := c.Get("b"); ok {
		t.Fatal("b 应已失效")
	}
	assertConsistent(t, c)

	// 再插入两个键重新触达上限：淘汰的应是当前最早键 c（而非已失效的陈旧键 b）。
	if !c.PutAtGen("e", "", 5, c.Generation()) {
		t.Fatal("put e 应成功")
	}
	if !c.PutAtGen("f", "", 6, c.Generation()) {
		t.Fatal("put f 应成功")
	}
	if _, ok := c.Get("c"); ok {
		t.Fatal("c 应为当前最早键并被淘汰")
	}
	for _, k := range []string{"d", "e", "f"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("%q 应仍存活", k)
		}
	}
	if c.Count() != 3 {
		t.Fatalf("count=%d, want 3", c.Count())
	}
	assertConsistent(t, c)
}

// TestCache_RepeatedInvalidateRefillKeepsOrderBounded 锁定「失效须同步从 FIFO 顺序表摘除」：
// 反复「写入 → 失效」同一键不得让 order 只增不减（内存缓漏）。失效后 Count() 必须为 0，
// 且 assertConsistent 校验 order 与 map 严格一致——若 Invalidate 未摘除 order 中的键，
// 循环后 len(order)=1000、len(map)=0 会立即失败。
func TestCache_RepeatedInvalidateRefillKeepsOrderBounded(t *testing.T) {
	c := New[int](8)
	for i := 0; i < 1000; i++ {
		c.Put("same", "", i)
		c.Invalidate("same")
	}
	if got := c.Count(); got != 0 {
		t.Fatalf("反复失效后应为空，got %d", got)
	}
	assertConsistent(t, c)
}

// TestCache_EvictionClearsGroupIndex 锁定 FIFO 淘汰须同步清理 group 倒排索引：
// 被淘汰键若残留在 groups 集合中，会导致 InvalidateGroup 命中幽灵键、并造成索引侧内存缓漏。
func TestCache_EvictionClearsGroupIndex(t *testing.T) {
	c := New[int](2)
	g := c.Generation()
	for _, k := range []string{"a", "b"} {
		if !c.PutAtGen(k, "grp", 1, g) {
			t.Fatalf("put %q 应成功", k)
		}
	}
	// 第 3 个键触发 FIFO：最早的 a 被淘汰。
	if !c.PutAtGen("c", "grp", 3, g) {
		t.Fatal("put c 应成功")
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("最早的键 a 应被 FIFO 淘汰")
	}
	if _, ghost := c.groups["grp"]["a"]; ghost {
		t.Fatal("被淘汰键 a 不得残留在 group 索引中（否则按组失效会命中幽灵键）")
	}
	for _, k := range []string{"b", "c"} {
		if _, ok := c.groups["grp"][k]; !ok {
			t.Fatalf("存活键 %q 应仍登记在其 group 中", k)
		}
	}
	assertConsistent(t, c)
}

// TestCache_OverwriteKeepsFIFOPosition 锁定 putLocked 契约：同键覆盖只换值、不改变 FIFO 位置。
// a 与 b 依次插入，覆盖 a 后 a 仍在队首（order[0] 不变），再插入 c 触发淘汰时被淘汰的仍是
// 位置未变的 a；若实现把覆盖键重新追加到队尾，则 order[0] 会变成 b、淘汰 b——本用例据 order[0]
// 与淘汰结果双重锁定，确保「覆盖不刷新 FIFO 位置」。
func TestCache_OverwriteKeepsFIFOPosition(t *testing.T) {
	c := New[int](2)
	g := c.Generation()
	if !c.PutAtGen("a", "", 1, g) || !c.PutAtGen("b", "", 2, g) {
		t.Fatal("初始写入应成功")
	}
	if !c.PutAtGen("a", "", 10, g) {
		t.Fatal("覆盖 a 应成功")
	}
	if len(c.order) == 0 || c.order[0] != "a" {
		t.Fatalf("同键覆盖不得改变 FIFO 位置：order[0]=%v, want a", c.order)
	}
	if !c.PutAtGen("c", "", 3, g) {
		t.Fatal("插入 c 应成功")
	}
	if _, ok := c.Get("a"); ok {
		t.Fatal("覆盖未改变 a 位置，a 应作为最早键被淘汰")
	}
	if v, ok := c.Get("b"); !ok || v != 2 {
		t.Fatalf("b 应存活且保留原值 2，got (%d,%v)", v, ok)
	}
	if _, ok := c.Get("c"); !ok {
		t.Fatal("c 应存活")
	}
	assertConsistent(t, c)
}

// TestCache_PutAtGenStaleRejected 锁定代际校验：陈旧代际拒绝回填，新鲜代际正常写入。
func TestCache_PutAtGenStaleRejected(t *testing.T) {
	c := New[int](0)
	gen := c.Generation()
	if !c.PutAtGen("k", "", 1, gen) {
		t.Fatal("当前代际回填应成功")
	}

	stale := gen // 失效前的代际
	c.Invalidate("k")
	if c.PutAtGen("k", "", 2, stale) {
		t.Fatal("失效后携带陈旧代际的回填必须被拒绝")
	}
	if _, ok := c.Get("k"); ok {
		t.Fatal("陈旧回填不得写入缓存")
	}

	// 无失效发生时，携带当前代际应正常写入。
	if !c.PutAtGen("k", "", 3, c.Generation()) {
		t.Fatal("当前代际回填应成功")
	}
	if v, ok := c.Get("k"); !ok || v != 3 {
		t.Fatalf("got (%d,%v), want (3,true)", v, ok)
	}
}

// TestCache_PutWritesAtCurrentGeneration 锁定 Put 契约：以当前代际无条件写入（供测试/直写），
// 无需先捕获代际；写入后失效仍会递增代际，使旧代际回填被拒。
func TestCache_PutWritesAtCurrentGeneration(t *testing.T) {
	c := New[int](0)
	c.Put("k", "g", 7)
	if v, ok := c.Get("k"); !ok || v != 7 {
		t.Fatalf("Put 后应命中 (7,true)，got (%d,%v)", v, ok)
	}
	// Put 采用当前代际：失效后再用失效前的代际回填仍须被拒绝。
	stale := c.Generation()
	c.Invalidate("k")
	if c.PutAtGen("k", "g", 8, stale) {
		t.Fatal("Put 之后失效，携带陈旧代际的回填必须被拒绝")
	}
	// Put 本身不受旧代际约束（无条件写入）。
	c.Put("k", "g", 9)
	if v, ok := c.Get("k"); !ok || v != 9 {
		t.Fatalf("Put 应无条件写入当前代际，got (%d,%v)", v, ok)
	}
	assertConsistent(t, c)
}

// TestCache_InvalidateBumpsGenerationEvenIfMissing 锁定「失效即使键不存在也递增代际」：
// 并发读路径可能正要回填该键，因此不能因键缺失而跳过代际 bump。
func TestCache_InvalidateBumpsGenerationEvenIfMissing(t *testing.T) {
	c := New[int](0)
	gen := c.Generation()
	c.Invalidate("never-existed")
	if c.Generation() == gen {
		t.Fatal("Invalidate 必须递增代际（即使键不存在）")
	}
	if c.PutAtGen("never-existed", "", 1, gen) {
		t.Fatal("失效前的代际回填必须被拒绝")
	}
}

// TestCache_InvalidateGroup 锁定按组失效与 group 迁移：
//   - 只清除目标 group 的键，其它 group 与无 group 键保留；
//   - 同键换 group 后，旧 group 失效不得误删该键。
func TestCache_InvalidateGroup(t *testing.T) {
	c := New[int](0)
	g := c.Generation()
	c.PutAtGen("u1-a", "u1", 1, g)
	c.PutAtGen("u1-b", "u1", 2, g)
	c.PutAtGen("u2-a", "u2", 3, g)
	c.PutAtGen("plain", "", 4, g)

	g = c.Generation()
	c.InvalidateGroup("u1")
	for _, k := range []string{"u1-a", "u1-b"} {
		if _, ok := c.Get(k); ok {
			t.Fatalf("%q 应随 group u1 失效", k)
		}
	}
	for _, k := range []string{"u2-a", "plain"} {
		if _, ok := c.Get(k); !ok {
			t.Fatalf("%q 不应被 group u1 失效影响", k)
		}
	}
	assertConsistent(t, c)

	// 同键从 u2 迁移到 u3：u2 失效不得误删已迁移的键。
	g = c.Generation()
	c.PutAtGen("u2-a", "u3", 30, g)
	c.InvalidateGroup("u2")
	if _, ok := c.Get("u2-a"); !ok {
		t.Fatal("同键已迁移到 u3，按 u2 失效不得误删")
	}
	c.InvalidateGroup("u3")
	if _, ok := c.Get("u2-a"); ok {
		t.Fatal("按 u3 失效后该键应被删除")
	}
	assertConsistent(t, c)
}

// TestCache_InvalidateAll 锁定整体失效：清空条目与索引并拒绝旧代际回填。
func TestCache_InvalidateAll(t *testing.T) {
	c := New[int](0)
	g := c.Generation()
	c.PutAtGen("a", "u1", 1, g)
	c.PutAtGen("b", "", 2, g)

	c.InvalidateAll()
	if c.Count() != 0 {
		t.Fatalf("InvalidateAll 后应为空，got %d", c.Count())
	}
	assertConsistent(t, c)
	if c.PutAtGen("a", "u1", 1, g) {
		t.Fatal("InvalidateAll 前捕获的代际回填必须被拒绝")
	}
	if !c.PutAtGen("a", "u1", 1, c.Generation()) {
		t.Fatal("InvalidateAll 后的新鲜代际回填应成功")
	}
}

// TestCache_ValuePresenceDistinguishesZeroAndNil 锁定 (V, bool) 语义：
// 命中「零值」与命中「nil 指针」都必须返回 ok=true，不能与「键不存在」混淆。
func TestCache_ValuePresenceDistinguishesZeroAndNil(t *testing.T) {
	zero := New[int](0)
	zero.PutAtGen("zero", "", 0, zero.Generation())
	if v, ok := zero.Get("zero"); !ok || v != 0 {
		t.Fatalf("零值命中应为 (0,true)，got (%d,%v)", v, ok)
	}
	if _, ok := zero.Get("missing"); ok {
		t.Fatal("不存在的键应返回 ok=false")
	}

	ptr := New[*int](0)
	ptr.PutAtGen("nil", "", nil, ptr.Generation())
	if v, ok := ptr.Get("nil"); !ok || v != nil {
		t.Fatalf("nil 指针命中应为 (nil,true)，got (%v,%v)", v, ok)
	}
	if _, ok := ptr.Get("missing"); ok {
		t.Fatal("不存在的键应返回 ok=false")
	}
}

// TestCache_Concurrent 并发读写/失效/计数，配合 -race 验证无数据竞争且始终有界。
func TestCache_Concurrent(t *testing.T) {
	const (
		workers = 8
		ops     = 500
		max     = 64
	)
	c := New[int](max)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < ops; i++ {
				key := fmt.Sprintf("k-%d-%d", w, i%16)
				switch i % 4 {
				case 0:
					c.PutAtGen(key, fmt.Sprintf("g-%d", w%3), i, c.Generation())
				case 1:
					c.Get(key)
				case 2:
					c.Invalidate(key)
				case 3:
					c.InvalidateGroup(fmt.Sprintf("g-%d", i%3))
				}
				if got := c.Count(); got > max {
					t.Errorf("缓存超过上限：%d > %d", got, max)
					return
				}
			}
		}(w)
	}
	wg.Wait()

	if c.Count() > max {
		t.Fatalf("并发后超过上限：%d > %d", c.Count(), max)
	}
	// 并发停止后校验顺序表与映射仍严格一致（并发失效/淘汰/覆盖不得破坏对应关系）。
	assertConsistent(t, c)

	// 无失效并发：不存在任何 Invalidate*，代际恒定，故每次回填携带的代际都最新，PutAtGen 必须全部成功。
	noInv := New[int](max)
	var wgNoInv sync.WaitGroup
	for w := 0; w < workers; w++ {
		wgNoInv.Add(1)
		go func(w int) {
			defer wgNoInv.Done()
			for i := 0; i < ops; i++ {
				key := fmt.Sprintf("n-%d-%d", w, i%16)
				if !noInv.PutAtGen(key, "", i, noInv.Generation()) {
					t.Errorf("无失效并发下 PutAtGen 不应失败")
					return
				}
			}
		}(w)
	}
	wgNoInv.Wait()
	assertConsistent(t, noInv)

	// 有失效并发：PutAtGen 可能因回源期间代际被 bump 而返回 false（预期），但必须不 panic，
	// 且并发结束后顺序表与映射仍严格一致。
	withInv := New[int](max)
	var wgInv sync.WaitGroup
	wgInv.Add(1)
	go func() {
		defer wgInv.Done()
		for i := 0; i < ops; i++ {
			withInv.Invalidate(fmt.Sprintf("n-%d", i%8))
			withInv.InvalidateGroup(fmt.Sprintf("g-%d", i%3))
		}
	}()
	for w := 0; w < workers; w++ {
		wgInv.Add(1)
		go func(w int) {
			defer wgInv.Done()
			for i := 0; i < ops; i++ {
				key := fmt.Sprintf("n-%d-%d", w, i%16)
				withInv.PutAtGen(key, fmt.Sprintf("g-%d", w%3), i, withInv.Generation())
				withInv.Get(key)
			}
		}(w)
	}
	wgInv.Wait()
	assertConsistent(t, withInv)
}
