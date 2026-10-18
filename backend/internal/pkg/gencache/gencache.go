// Package gencache 提供「有界 FIFO + 失效代际」的通用读缓存原语。
//
// 设计边界（与 plan Option A 一致）：本包只负责真正导致「读回填 / 写失效无版本保护」
// 这类缺陷的机制——有界存储、FIFO 淘汰、失效代际、按键/按组/整体失效；
// TTL、精确过期、防御性深拷贝、负缓存（nil 值）等 value 语义一律留在各缓存 wrapper。
//
// 失效代际（generation）契约：
//   - 任一次 Invalidate / InvalidateGroup / InvalidateAll 都会递增代际（即使目标键不存在），
//     因为并发的读路径可能正要回填同一键。
//   - 读路径应在回源（DB）前调用 Generation 捕获代际，回源后用 PutAtGen 携带该代际回填；
//     若期间发生过任一次失效，PutAtGen 返回 false 并丢弃本次回填，避免把失效前读到的
//     陈旧快照重新写回缓存（认证绕过 / 配置旧值无限滞留的根因）。
//
// 有界性与顺序一致性契约：
//   - max > 0 时条目数不超过 max，超限按首次插入顺序 FIFO 淘汰最早键；
//     max <= 0 表示不限制（与原三处缓存语义一致）。
//   - order 与 map 严格一致：每个存活键在 order 中恰好出现一次；失效、淘汰、
//     同键覆盖（含 group 变更）都会同步维护两边的对应关系，避免陈旧键只增不减导致内存缓漏。
//
// group 契约：
//   - group 是可选的二级失效维度（如 identity 用 userID 批量失效其名下令牌）。
//   - group 为空串表示不参与分组索引（零额外开销）；InvalidateGroup("") 不会命中任何键。
//   - 同一键重复写入不同 group 时，会先从旧 group 索引摘除，再登记到新 group。
//
// 并发契约：所有方法均并发安全；Get 返回的 V 是缓存内保存的值本身，
// 若 V 为指针/切片/map，wrapper 必须在边界做防御性拷贝后再交给调用方。
package gencache

import "sync"

// entry 是缓存内部条目：除值本身外记录其所属 group，便于同键覆盖时维护二级索引。
type entry[V any] struct {
	value V
	group string
}

// Cache 是带失效代际的并发安全有界读缓存。零值不可用，须经 New 创建。
type Cache[V any] struct {
	mu sync.RWMutex
	// m 为主存储：key -> (value, group)。
	m map[string]entry[V]
	// order 为 FIFO 顺序表，与 m 严格一致（每个存活键恰好出现一次）。
	order []string
	// groups 为 group -> set(key) 倒排索引；group 为空串的键不登记。
	groups map[string]map[string]struct{}
	// gen 为失效代际：任一失效操作递增，PutAtGen 据此拒绝陈旧回填。
	gen uint64
	// max 为条目上限；<=0 表示不限制。
	max int
}

// New 创建有界缓存，max 为条目上限（<=0 表示不限制）。
func New[V any](max int) *Cache[V] {
	return &Cache[V]{
		m:      make(map[string]entry[V]),
		groups: make(map[string]map[string]struct{}),
		max:    max,
	}
}

// Get 读取缓存。第二个返回值区分「命中但值为零值/nil」与「键不存在」：
// 命中负缓存（如 V 为指针类型时存入 nil）时返回 (nil, true)，调用方不得当作未命中。
func (c *Cache[V]) Get(key string) (V, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.m[key]
	if !ok {
		var zero V
		return zero, false
	}
	return e.value, true
}

// Generation 返回当前失效代际。读路径应在回源前捕获，回填时交给 PutAtGen 校验。
func (c *Cache[V]) Generation() uint64 {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.gen
}

// PutAtGen 在代际仍为 gen 时写入键值，返回是否写入成功。
//
// 契约：
//   - gen 与当前代际不一致（回源期间发生过失效）时拒绝写入并返回 false；
//   - 键已存在时覆盖值（不改变 FIFO 位置）；group 变化时同步迁移二级索引；
//   - 键不存在时追加到 FIFO 尾部，并在超限时淘汰最早键；
//   - group 为空串表示不登记分组索引。
func (c *Cache[V]) PutAtGen(key, group string, v V, gen uint64) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.gen != gen {
		return false
	}
	if old, ok := c.m[key]; ok {
		if old.group != group {
			c.removeFromGroupLocked(key, old.group)
			c.addToGroupLocked(key, group)
		}
		c.m[key] = entry[V]{value: v, group: group}
	} else {
		c.m[key] = entry[V]{value: v, group: group}
		c.order = append(c.order, key)
		c.addToGroupLocked(key, group)
	}
	c.evictLocked()
	return true
}

// Invalidate 失效单个键，并递增代际（即使该键不存在）。
// 用于拒绝「回源期间发生失效」后携带旧代际的陈旧回填。
func (c *Cache[V]) Invalidate(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	e, ok := c.m[key]
	if !ok {
		return
	}
	delete(c.m, key)
	c.removeFromGroupLocked(key, e.group)
	c.removeFromOrderLocked(key)
}

// InvalidateGroup 失效某 group 下的全部键，并递增代际（即使该 group 无键）。
// group 为空串时不会命中任何键（空 group 不参与索引）。
func (c *Cache[V]) InvalidateGroup(group string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	keys := c.groups[group]
	if len(keys) == 0 {
		delete(c.groups, group)
		return
	}
	for key := range keys {
		delete(c.m, key)
	}
	// 一次遍历压缩 order，保持与 map 严格一致（避免逐键 O(n) 扫描）。
	kept := c.order[:0]
	for _, key := range c.order {
		if _, drop := keys[key]; !drop {
			kept = append(kept, key)
		}
	}
	c.order = kept
	delete(c.groups, group)
}

// InvalidateAll 清空全部键与索引，并递增代际。
func (c *Cache[V]) InvalidateAll() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.gen++
	c.m = make(map[string]entry[V])
	c.order = nil
	c.groups = make(map[string]map[string]struct{})
}

// Count 返回当前条目数（测试/观测用）。
func (c *Cache[V]) Count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.m)
}

// evictLocked 在超过上限时按 FIFO 淘汰最早键（调用方须持有写锁）。
// order 与 map 严格一致，因此弹出的最早键必为有效条目。
func (c *Cache[V]) evictLocked() {
	if c.max <= 0 {
		return
	}
	for len(c.m) > c.max && len(c.order) > 0 {
		oldest := c.order[0]
		c.order = c.order[1:]
		e, ok := c.m[oldest]
		if !ok {
			continue
		}
		delete(c.m, oldest)
		c.removeFromGroupLocked(oldest, e.group)
	}
}

// removeFromOrderLocked 从 FIFO 顺序表中摘除指定键（调用方须持有写锁）。
// 失效为写路径低频调用，O(n) 原地压缩换取顺序表与映射的严格一致。
func (c *Cache[V]) removeFromOrderLocked(key string) {
	if len(c.order) == 0 {
		return
	}
	kept := c.order[:0]
	for _, k := range c.order {
		if k != key {
			kept = append(kept, k)
		}
	}
	c.order = kept
}

// addToGroupLocked 把键登记到 group 索引；空 group 不登记（调用方须持有写锁）。
func (c *Cache[V]) addToGroupLocked(key, group string) {
	if group == "" {
		return
	}
	set := c.groups[group]
	if set == nil {
		set = make(map[string]struct{})
		c.groups[group] = set
	}
	set[key] = struct{}{}
}

// removeFromGroupLocked 把键从 group 索引摘除，集合为空时删除该 group（调用方须持有写锁）。
func (c *Cache[V]) removeFromGroupLocked(key, group string) {
	if group == "" {
		return
	}
	set := c.groups[group]
	if set == nil {
		return
	}
	delete(set, key)
	if len(set) == 0 {
		delete(c.groups, group)
	}
}
