package main

import (
	"context"
	"database/sql"
	"errors"
	"sync"

	"github.com/team/llmgateway/internal/domain/gateway"
	"github.com/team/llmgateway/internal/domain/identity"
)

// pricingModeCache 缓存「用户 ID → 计费模式」解析结果（网关每请求按 user_id 解析，用户计费模式低频变更）。
//   - 数据来源：users.pricing_mode（经 identity.Store.GetByID 读取）；
//   - 键 = userID；值为归一化后的计费模式（用户未设置时归一为 sale）；
//   - 失效点（写路径显式失效，无 TTL）：identity 用户写路径
//     （Service.AdminCreateUser / UpdateUser / BatchDeleteUsers）经 SetUserWriteHook 回调本缓存 Invalidate(userID)，
//     用户计费模式变更后下一次解析立即读到新值；
//   - 未命中用户（sql.ErrNoRows）不写缓存，避免新建用户后被负缓存误导；
//   - 由配置开关控制：关闭时装配层不创建缓存（resolver 传 nil），退回直查 DB。
type pricingModeCache struct {
	mu sync.RWMutex
	m  map[int64]string
}

// newPricingModeCache 创建计费模式缓存。
func newPricingModeCache() *pricingModeCache {
	return &pricingModeCache{m: make(map[int64]string)}
}

func (c *pricingModeCache) get(userID int64) (string, bool) {
	c.mu.RLock()
	mode, ok := c.m[userID]
	c.mu.RUnlock()
	return mode, ok
}

func (c *pricingModeCache) put(userID int64, mode string) {
	c.mu.Lock()
	c.m[userID] = mode
	c.mu.Unlock()
}

// Invalidate 失效单个用户 ID 的计费模式缓存（用户写路径经 SetUserWriteHook 回调）。
func (c *pricingModeCache) Invalidate(userID int64) {
	c.mu.Lock()
	delete(c.m, userID)
	c.mu.Unlock()
}

// newPricingModeResolver 由用户 ID 解析计费模式，默认 sale。
// cache 非 nil 时启用内存缓存（命中零 DB 查询；用户写路径经 identity.SetUserWriteHook 即时失效）；
// cache 为 nil 时退回直查（读缓存开关关闭的安全态）。
func newPricingModeResolver(store *identity.Store, cache *pricingModeCache) gateway.PricingModeResolver {
	return func(ctx context.Context, userID int64) (string, error) {
		if cache != nil {
			if mode, ok := cache.get(userID); ok {
				return mode, nil
			}
		}
		u, err := store.GetByID(ctx, userID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return identity.PricingModeSale, nil
			}
			return "", err
		}
		mode := u.PricingMode
		if mode == "" {
			mode = identity.PricingModeSale
		}
		if cache != nil {
			cache.put(userID, mode)
		}
		return mode, nil
	}
}

// gatewayConfigSource 提供 sys_configs 单键读取（*billing.SqlStore 满足）。
type gatewayConfigSource interface {
	GetConfig(ctx context.Context, key string) ([]byte, error)
}

// configValueCache 缓存某个 sys_configs 键的原始 JSON 值（网关每请求读取，console 低频写入）。
//   - 数据来源：sys_configs 指定键（经 billing.SqlStore.GetConfig）；
//   - 键 = 固定的单个 config key；值为该键的原始 JSON 字节；
//   - 失效点（写路径显式失效，无 TTL）：console 写该配置经 billing.UpsertConfig 触发
//     SetConfigWriteHook，装配层在钩子中调用 Invalidate()（配置变更即时生效）；
//   - 读取失败不写缓存（瞬时故障可自愈）。
type configValueCache struct {
	src    gatewayConfigSource
	key    string
	mu     sync.RWMutex
	value  []byte
	cached bool
}

// newConfigValueCache 创建单键配置值缓存。
func newConfigValueCache(src gatewayConfigSource, key string) *configValueCache {
	return &configValueCache{src: src, key: key}
}

// Get 读取配置值：命中缓存零 DB 查询；未命中回源并写缓存（失败不写缓存）。
func (c *configValueCache) Get(ctx context.Context) ([]byte, error) {
	c.mu.RLock()
	if c.cached {
		v := c.value
		c.mu.RUnlock()
		return v, nil
	}
	c.mu.RUnlock()
	raw, err := c.src.GetConfig(ctx, c.key)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.value = raw
	c.cached = true
	c.mu.Unlock()
	return raw, nil
}

// Invalidate 清空缓存（sys_configs 写入后由 SetConfigWriteHook 调用）。
func (c *configValueCache) Invalidate() {
	c.mu.Lock()
	c.value = nil
	c.cached = false
	c.mu.Unlock()
}
