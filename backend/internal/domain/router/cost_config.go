package router

import (
	"context"
	"encoding/json"
	"sync"
)

// cfgSessionCost 会话成本路由配置在 sys_configs 中的键。
const cfgSessionCost = "routing.session_cost"

// SessionCostConfig 会话成本路由配置（routing.session_cost）。
//
// 安全态语义：Enabled=false 为默认安全态，门控的是「会话成本路由策略」
// （起点择优 / 中途切换 / 闲忙时联动）；关闭 = 不启用这些策略。
//
// 注意：成本比较口径（三档成本价 × 时段系数 × 上下文分档系数，替代原「分档系数恒 1」）
// 是 P0 既定新基线，**不由该开关回滚**；默认种子下模型级 context_tiers 为空、
// 全局分档为空，故默认部署排序不变。该配置仍不改变对外售价计费与响应语义（成本价 ≠ 售价）。
//
// 只有在运维显式把 enabled 置为 true 后才允许后续步骤接入会话成本路由策略。
// 配置缺失、读取失败或解析失败时同样回退到该安全态，避免配置异常放大为路由异常。
type SessionCostConfig struct {
	Enabled      bool    `json:"enabled"`
	RemainingA   float64 `json:"remaining_a"`
	RemainingB   float64 `json:"remaining_b"`
	RemainingMin int     `json:"remaining_min"`
	RemainingMax int     `json:"remaining_max"`
	MinRemaining int     `json:"min_remaining"`
	CooldownSec  int     `json:"cooldown_sec"`
}

// defaultSessionCostConfig 返回缺省配置：关闭（安全态）并带上默认阈值。
func defaultSessionCostConfig() SessionCostConfig {
	return SessionCostConfig{
		Enabled:      false,
		RemainingA:   12,
		RemainingB:   1,
		RemainingMin: 1,
		RemainingMax: 20,
		MinRemaining: 1,
		CooldownSec:  0,
	}
}

// SessionCostProvider 按需读取并缓存会话成本路由配置。
//
// 缓存/失效机制与包内既有配置提供者及 billing.defaultConfigProvider 对齐：
// 内存缓存 + 显式 Invalidate；读取失败/解析失败不写缓存，便于瞬时故障自愈。
// gen 为失效代际：Invalidate 递增，回源期间若发生失效则丢弃本次陈旧回填。
type SessionCostProvider struct {
	src configSource

	mu     sync.RWMutex
	cached *SessionCostConfig
	gen    uint64
}

// NewSessionCostProvider 构造会话成本路由配置提供者。
func NewSessionCostProvider(src configSource) *SessionCostProvider {
	return &SessionCostProvider{src: src}
}

// Config 返回会话成本路由配置：缺失、读取失败或解析失败时回退缺省配置（Enabled=false 安全态）。
func (p *SessionCostProvider) Config() SessionCostConfig {
	if p == nil {
		return defaultSessionCostConfig()
	}

	p.mu.RLock()
	cached := p.cached
	gen := p.gen
	src := p.src
	p.mu.RUnlock()

	if cached != nil {
		return *cached
	}
	if src == nil {
		return defaultSessionCostConfig()
	}

	raw, err := src.GetConfig(context.Background(), cfgSessionCost)
	if err != nil {
		return defaultSessionCostConfig()
	}
	cfg, ok := parseSessionCostConfig(raw)
	if !ok {
		return defaultSessionCostConfig()
	}

	p.mu.Lock()
	// 仅当回源期间未发生失效（代际未变）才回填，避免旧值滞留。
	if p.gen == gen {
		p.cached = &cfg
	}
	p.mu.Unlock()
	return cfg
}

// Invalidate 清空缓存（供装配层在 sys_configs 写入后调用）。
func (p *SessionCostProvider) Invalidate() {
	p.mu.Lock()
	p.gen++
	p.cached = nil
	p.mu.Unlock()
}

// parseSessionCostConfig 解析 routing.session_cost：字段使用 snake_case，
// 未提供的字段用缺省值补齐；JSON 非法时返回 ok=false。
func parseSessionCostConfig(raw []byte) (SessionCostConfig, bool) {
	var in struct {
		Enabled      *bool    `json:"enabled"`
		RemainingA   *float64 `json:"remaining_a"`
		RemainingB   *float64 `json:"remaining_b"`
		RemainingMin *int     `json:"remaining_min"`
		RemainingMax *int     `json:"remaining_max"`
		MinRemaining *int     `json:"min_remaining"`
		CooldownSec  *int     `json:"cooldown_sec"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return SessionCostConfig{}, false
	}

	cfg := defaultSessionCostConfig()
	if in.Enabled != nil {
		cfg.Enabled = *in.Enabled
	}
	if in.RemainingA != nil {
		cfg.RemainingA = *in.RemainingA
	}
	if in.RemainingB != nil {
		cfg.RemainingB = *in.RemainingB
	}
	if in.RemainingMin != nil {
		cfg.RemainingMin = *in.RemainingMin
	}
	if in.RemainingMax != nil {
		cfg.RemainingMax = *in.RemainingMax
	}
	if in.MinRemaining != nil {
		cfg.MinRemaining = *in.MinRemaining
	}
	if in.CooldownSec != nil {
		cfg.CooldownSec = *in.CooldownSec
	}
	return cfg, true
}
