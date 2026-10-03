// Package session 提供进程内会话注册表：缓存每个用户的 token_version/status/must_change，
// 供鉴权中间件 O(1) 校验，避免逐请求查库（单实例部署前提）。
package session

import (
	"context"
	"errors"
	"sync"
)

// ErrRevoked 表示会话已失效（版本不匹配 / 账号禁用 / 用户不存在）。
var ErrRevoked = errors.New("session revoked")

// Entry 是用户会话状态的内存视图。
type Entry struct {
	Version    int
	Status     string
	MustChange bool
}

// Loader 在缓存未命中时从持久层加载。
type Loader func(ctx context.Context, userID int64) (Entry, error)

// Registry 内存会话注册表。
type Registry struct {
	mu   sync.RWMutex
	m    map[int64]Entry
	load Loader
}

// NewRegistry 创建注册表。load 为 nil 时未命中直接返回 ErrRevoked。
func NewRegistry(load Loader) *Registry {
	return &Registry{m: map[int64]Entry{}, load: load}
}

// Check 校验用户会话：版本一致、账号 ACTIVE；返回 must_change 标记。
func (r *Registry) Check(ctx context.Context, userID int64, ver int) (bool, error) {
	e, err := r.get(ctx, userID)
	if err != nil {
		return false, ErrRevoked
	}
	if e.Status != "ACTIVE" || e.Version != ver {
		return false, ErrRevoked
	}
	return e.MustChange, nil
}

// Set 更新某用户的缓存视图（登录/登出/改密/重置/禁用后调用，保持一致性）。
func (r *Registry) Set(userID int64, e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.m[userID] = e
}

// Delete 移除缓存条目（用户删除时）。
func (r *Registry) Delete(userID int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.m, userID)
}

func (r *Registry) get(ctx context.Context, userID int64) (Entry, error) {
	r.mu.RLock()
	e, ok := r.m[userID]
	r.mu.RUnlock()
	if ok {
		return e, nil
	}
	if r.load == nil {
		return Entry{}, ErrRevoked
	}
	loaded, err := r.load(ctx, userID)
	if err != nil {
		return Entry{}, err
	}
	r.mu.Lock()
	r.m[userID] = loaded
	r.mu.Unlock()
	return loaded, nil
}