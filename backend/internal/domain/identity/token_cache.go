// token_cache.go 令牌鉴权（TokenService.LookupByPlain）两级读缓存。
//
// 目标：网关每请求鉴权在缓存命中时 0 次 DB 查询；同时保证任何写路径后缓存「即时」失效
// （无 TTL 延迟），令牌自然过期按自身 ExpiresAt「精确」生效。
//
// 结构（两级索引，共用同一把 RWMutex 保护）：
//
//	一级 plainHash -> tokenCacheEntry：命中即为鉴权结果（令牌快照 + 用户状态快照 + 写入时间）。
//	二级 userID    -> set(plainHash)：按用户批量失效用的倒排索引（用户状态/资料变更时）。
//
// 失效矩阵（写路径 -> 失效动作；均为「同步、即时」失效，而非 TTL 到期）：
//
//	令牌域（TokenService）：
//	  Create      -> InvalidateByUserID(userID)        // 新令牌使该用户令牌视图变化，保守按用户全清
//	  BatchDelete -> InvalidateByHash(受影响令牌哈希)   // 删除前按 ID 反查哈希；反查失败则 InvalidateAll
//	  Rotate      -> InvalidateByHash(旧令牌哈希)       // 旧令牌被禁用
//	  Toggle      -> InvalidateByHash(令牌哈希)         // 状态翻转
//	用户域（Service，经 SetUserWriteHook 回调，与 pkg/session 用户状态缓存同点触发，避免双缓存漂移）：
//	  AdminCreateUser / UpdateUser / BatchDeleteUsers / AdminResetPassword
//	              -> InvalidateByUserID(userID)         // 用户状态/角色/删除变更
//
// 过期语义：
//  1. 精确过期：令牌自身 ExpiresAt 到点（now >= ExpiresAt）即视为未命中并回源 DB 复核——与 TTL 无关。
//  2. 安全 TTL（默认 60s，可配置）：仅作「最后防线」——条目存活超过 TTL 一律视为未命中，用于兜底
//     「理论上不该发生的失效遗漏」。TTL 不计入「即时失效」保证；即时性由上述失效钩子负责。
//
// 有界性：条目数上限默认 10 万，超限按插入顺序 FIFO 淘汰最早条目，避免无界增长。
// 存储/淘汰/代际/失效由 pkg/gencache 原语承载；本文件只保留 value 语义（TTL、精确过期、
// 防御性深拷贝）与鉴权快照结构。
// 开关：默认启用；SetTokenCacheEnabled(false) 后 LookupByPlain 退回纯 DB 直查（安全态）。
package identity

import (
	"strconv"
	"sync"
	"time"

	"github.com/team/llmgateway/internal/pkg/gencache"
)

const (
	// tokenCacheDefaultTTL 安全 TTL 默认值（最后防线，非即时失效机制）。
	tokenCacheDefaultTTL = 60 * time.Second
	// tokenCacheDefaultMaxEntries 缓存条目上限，超过后按 FIFO 淘汰最早插入的条目。
	tokenCacheDefaultMaxEntries = 100000
)

// tokenCacheEntry 是一条鉴权结果快照。写入后不可变（更新即整体替换指针），
// 因此命中路径可在 RLock 释放后安全读取。
type tokenCacheEntry struct {
	token      *Token
	userStatus string
	cachedAt   time.Time
}

// cloneToken 深拷贝 Token：除值字段外，额外为 TagID/ExpiresAt/LastUsedAt 三个指针字段
// 分配新地址，避免缓存与调用方共享同一指针，任一方写 *ptr 都会污染另一方（P2-5）。
// nil 入参返回 nil，nil 指针字段保持 nil。
func cloneToken(t *Token) *Token {
	if t == nil {
		return nil
	}
	c := *t
	if t.TagID != nil {
		v := *t.TagID
		c.TagID = &v
	}
	if t.ExpiresAt != nil {
		v := *t.ExpiresAt
		c.ExpiresAt = &v
	}
	if t.LastUsedAt != nil {
		v := *t.LastUsedAt
		c.LastUsedAt = &v
	}
	return &c
}

// tokenCache 令牌鉴权两级读缓存。零值不可用，须经 newTokenCache 创建。
//
// 存储、FIFO 淘汰、失效代际与 userID 二级倒排索引（group=userID 字符串）全部委托给
// pkg/gencache 原语；本类型只保留 value 语义（开关、安全 TTL、精确过期判定、防御性深拷贝）
// 以及「命中→过期判定→回源前取代际→putAtGen 回填」的编排。
type tokenCache struct {
	mu      sync.RWMutex
	enabled bool
	ttl     time.Duration
	// now 时钟注入点（默认 time.Now），便于测试精确过期边界。
	now func() time.Time
	// maxEntries 条目上限（默认 10 万）。保留为字段：既有测试会在首次访问前直写调整上限，
	// 底层原语据此创建（见 ensureEntriesLocked）。
	maxEntries int
	// entries 承载 plainHash -> tokenCacheEntry 的存储、FIFO 淘汰、失效代际与
	// userID 二级索引（以 userID 字符串作为 group）。首次访问时按 maxEntries 创建。
	// 代际语义：读路径回源前捕获、回填时校验，拒绝「回源期间发生失效」的陈旧快照
	// （防认证绕过窗口）。
	entries *gencache.Cache[*tokenCacheEntry]
}

// newTokenCache 创建缓存：默认启用、安全 TTL 60s、上限 10 万。
func newTokenCache() *tokenCache {
	return &tokenCache{
		enabled:    true,
		ttl:        tokenCacheDefaultTTL,
		now:        time.Now,
		maxEntries: tokenCacheDefaultMaxEntries,
	}
}

// ensureEntriesLocked 返回底层原语，首次访问时按当前 maxEntries 创建（调用方须持有 c.mu 写锁）。
// maxEntries 仅在测试中于首次访问前被直写调整，故创建时机决定其生效上限。
func (c *tokenCache) ensureEntriesLocked() *gencache.Cache[*tokenCacheEntry] {
	if c.entries == nil {
		c.entries = gencache.New[*tokenCacheEntry](c.maxEntries)
	}
	return c.entries
}

// setEnabled 开关缓存；关闭时清空全部条目（避免关闭期间残留旧值）。
//
// 锁序约定：统一为「先 c.mu，后原语锁」，二者不得嵌套持有。故此处先在 c.mu 内落定 enabled
// 并取得原语引用，释放 c.mu 后再调用 InvalidateAll——不在持有 c.mu 时进入原语锁。
// 语义等价：enabled 已在 c.mu 内落定，生产读路径（LookupByPlain）要么看到 enabled=false 直接回源，
// 要么其并发回填携带的旧代际被 putAtGen 拒绝。（无代际保护的 put 仅测试用，不在此路径。）
func (c *tokenCache) setEnabled(enabled bool) {
	c.mu.Lock()
	c.enabled = enabled
	var cache *gencache.Cache[*tokenCacheEntry]
	if !enabled {
		cache = c.ensureEntriesLocked()
	}
	c.mu.Unlock()
	if cache != nil {
		cache.InvalidateAll()
	}
}

// setTTL 设置安全 TTL（最后防线）。
func (c *tokenCache) setTTL(d time.Duration) {
	c.mu.Lock()
	c.ttl = d
	c.mu.Unlock()
}

// get 读取缓存：命中返回令牌快照副本（调用方可安全使用/持有）。
// 安全 TTL 超期或令牌自然过期时视为未命中，并顺手淘汰该条目。
func (c *tokenCache) get(hash string) (*Token, bool) {
	c.mu.RLock()
	if !c.enabled {
		c.mu.RUnlock()
		return nil, false
	}
	cache := c.entries
	ttl := c.ttl
	nowFn := c.now
	c.mu.RUnlock()
	if cache == nil {
		return nil, false
	}
	e, ok := cache.Get(hash)
	if !ok {
		return nil, false
	}
	now := nowFn()
	// 安全 TTL：仅最后防线，超期即回源（即时性由写路径失效钩子保证，非本 TTL）。
	if ttl > 0 && now.Sub(e.cachedAt) >= ttl {
		c.invalidateByHash(hash)
		return nil, false
	}
	// 精确过期：令牌自身的 ExpiresAt 到点即视为未命中（now >= ExpiresAt）。
	if e.token.ExpiresAt != nil && !now.Before(*e.token.ExpiresAt) {
		c.invalidateByHash(hash)
		return nil, false
	}
	// 防御性校验：仅成功鉴权（用户 ACTIVE）才会写缓存；若快照状态异常则视为未命中。
	if e.userStatus != StatusActive {
		c.invalidateByHash(hash)
		return nil, false
	}
	// 防御性深拷贝：指针字段（TagID/ExpiresAt/LastUsedAt）独立分配，调用方修改不影响缓存（P2-5）。
	return cloneToken(e.token), true
}

// put 写入鉴权结果快照（仅成功项）：以当前代际无条件写入（原语 Put），供测试/直接写入使用。
func (c *tokenCache) put(hash string, t *Token, userStatus string) {
	if t == nil {
		return
	}
	c.mu.Lock()
	cache := c.ensureEntriesLocked()
	enabled := c.enabled
	nowFn := c.now
	c.mu.Unlock()
	if !enabled {
		return
	}
	// 防御性深拷贝：入缓存的是副本，调用方后续修改其持有的 Token（含指针字段）不影响缓存（P2-5）。
	snap := cloneToken(t)
	cache.Put(hash, strconv.FormatInt(snap.UserID, 10),
		&tokenCacheEntry{token: snap, userStatus: userStatus, cachedAt: nowFn()})
}

// invalidateByHash 按令牌哈希精确失效单个条目。
func (c *tokenCache) invalidateByHash(hash string) {
	c.mu.Lock()
	cache := c.ensureEntriesLocked()
	c.mu.Unlock()
	cache.Invalidate(hash)
}

// invalidateByUserID 按用户失效其名下全部令牌条目（用户写路径）。
func (c *tokenCache) invalidateByUserID(userID int64) {
	c.mu.Lock()
	cache := c.ensureEntriesLocked()
	c.mu.Unlock()
	cache.InvalidateGroup(strconv.FormatInt(userID, 10))
}

// invalidateAll 清空全部条目（用于无法确定受影响键时的保守兜底）。
func (c *tokenCache) invalidateAll() {
	c.mu.Lock()
	cache := c.ensureEntriesLocked()
	c.mu.Unlock()
	cache.InvalidateAll()
}

// count 返回当前条目数（测试/观测用）。
func (c *tokenCache) count() int {
	c.mu.RLock()
	cache := c.entries
	c.mu.RUnlock()
	if cache == nil {
		return 0
	}
	return cache.Count()
}

// generation 返回当前缓存代际（失效时递增）。
// 读路径在回源 DB 前捕获代际，回填时校验未被 bump，避免「失效后回填陈旧快照」的认证绕过窗口。
func (c *tokenCache) generation() uint64 {
	c.mu.RLock()
	cache := c.entries
	c.mu.RUnlock()
	if cache == nil {
		return 0
	}
	return cache.Generation()
}

// putAtGen 写入鉴权结果快照，仅在 gen 仍为最新（期间未发生失效）时写入。
// 若期间发生任一次失效（invalidate*/setEnabled(false) 已 bump 代际），本次回填被丢弃——
// 否则会把失效前读到的陈旧（如 ACTIVE）快照回填进缓存，使撤销/禁用/删除在该条目存活期内失效。
func (c *tokenCache) putAtGen(hash string, t *Token, userStatus string, gen uint64) {
	if t == nil {
		return
	}
	c.mu.Lock()
	cache := c.ensureEntriesLocked()
	enabled := c.enabled
	nowFn := c.now
	c.mu.Unlock()
	if !enabled {
		return
	}
	// 防御性深拷贝：入缓存的是副本，调用方后续修改其持有的 Token（含指针字段）不影响缓存（P2-5）。
	snap := cloneToken(t)
	cache.PutAtGen(hash, strconv.FormatInt(snap.UserID, 10),
		&tokenCacheEntry{token: snap, userStatus: userStatus, cachedAt: nowFn()}, gen)
}
