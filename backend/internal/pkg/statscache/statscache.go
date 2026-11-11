// Package statscache 提供进程内的 TTL 缓存 + 并发回源合并（singleflight）：
// 同一个 key 上的并发请求只会触发一次回源，其余请求等这一次的结果。
//
// 为什么不用 Redis / 不用第三方组件（2026-11-07 已拍板）：
// 看板数据是「可容忍几十秒滞后」的读放大热点，进程内缓存即可消掉绝大部分回源；
// 引入中间件会把「排查一次慢查询」变成「排查一次网络 + 一个中间件」，收益不成比例。
//
// 不变量：
//   - 回源失败不写缓存（错误不会被缓存放大）；
//   - 空结果照样缓存（否则「本来就没数据」的账号每次请求都会打穿到 DB）；
//   - key 由调用方负责包含所有隔离维度（如用户 ID），本包不做任何键拼装。
package statscache

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultTTL 看板默认缓存时长：5 分钟（与前端 /me 缓存口径一致）。
const DefaultTTL = 5 * time.Minute

// entry 一条缓存值及其过期时刻。
type entry[T any] struct {
	val T
	exp time.Time
}

// call 一次进行中的回源；done 关闭后 val/err 可读。
type call[T any] struct {
	done chan struct{}
	val  T
	err  error
}

// Stats 命中统计（随看板响应头输出，压测与线上观测同一口径）。
type Stats struct {
	Hits   int64 // 直接命中未过期缓存
	Shared int64 // 未命中但与别的请求合并，没有回源
	Misses int64 // 未命中且本请求负责回源
	Loads  int64 // 实际回源次数（= 打 DB 次数）
	Keys   int   // 当前缓存键数
}

// Cache 进程内 TTL 缓存。零值不可用，用 New 构造。
type Cache[T any] struct {
	ttl time.Duration
	now func() time.Time

	mu       sync.Mutex
	entries  map[string]entry[T]
	inflight map[string]*call[T]

	hits   atomic.Int64
	shared atomic.Int64
	misses atomic.Int64
	loads  atomic.Int64
}

// New 构造一个 TTL 为 ttl 的缓存；ttl<=0 时取 DefaultTTL。
func New[T any](ttl time.Duration) *Cache[T] {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Cache[T]{
		ttl:      ttl,
		now:      time.Now,
		entries:  make(map[string]entry[T]),
		inflight: make(map[string]*call[T]),
	}
}

// SetClock 替换时钟（单测用：验证 TTL 到期后刷新，不必真的等 5 分钟）。
func (c *Cache[T]) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

// Get 取 key 对应的值：命中直接返回（fromCache=true）；未命中则调用 load 回源，
// 同一 key 的并发调用只会有一个真正执行 load。
func (c *Cache[T]) Get(ctx context.Context, key string, load func(context.Context) (T, error)) (val T, fromCache bool, err error) {
	c.mu.Lock()
	now := c.now()
	if e, ok := c.entries[key]; ok {
		if now.Before(e.exp) {
			c.mu.Unlock()
			c.hits.Add(1)
			return e.val, true, nil
		}
		// 过期即删，避免长期不访问的键永久占内存。
		delete(c.entries, key)
	}
	if cl, ok := c.inflight[key]; ok {
		c.mu.Unlock()
		c.shared.Add(1)
		select {
		case <-cl.done:
			return cl.val, true, cl.err
		case <-ctx.Done():
			var zero T
			return zero, false, ctx.Err()
		}
	}
	cl := &call[T]{done: make(chan struct{})}
	c.inflight[key] = cl
	c.mu.Unlock()

	c.misses.Add(1)
	c.loads.Add(1)
	v, err := load(ctx)

	c.mu.Lock()
	delete(c.inflight, key)
	if err == nil {
		c.entries[key] = entry[T]{val: v, exp: c.now().Add(c.ttl)}
	}
	c.mu.Unlock()

	cl.val, cl.err = v, err
	close(cl.done)
	return v, false, err
}

// Invalidate 删除单个键（写路径主动失效用，下一次读回源）。
func (c *Cache[T]) Invalidate(key string) {
	c.mu.Lock()
	delete(c.entries, key)
	c.mu.Unlock()
}

// Purge 清空所有缓存（配置变更/手工清理用），不影响统计计数。
func (c *Cache[T]) Purge() {
	c.mu.Lock()
	c.entries = make(map[string]entry[T])
	c.mu.Unlock()
}

// Stats 返回当前命中统计快照。
func (c *Cache[T]) Stats() Stats {
	c.mu.Lock()
	keys := len(c.entries)
	c.mu.Unlock()
	return Stats{
		Hits:   c.hits.Load(),
		Shared: c.shared.Load(),
		Misses: c.misses.Load(),
		Loads:  c.loads.Load(),
		Keys:   keys,
	}
}

// ResetStats 归零计数（单测/压测分段统计用）。
func (c *Cache[T]) ResetStats() {
	c.hits.Store(0)
	c.shared.Store(0)
	c.misses.Store(0)
	c.loads.Store(0)
}
