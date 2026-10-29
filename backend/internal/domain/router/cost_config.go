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
// 开关语义：Enabled 是「会话成本路由策略」总开关（起点择优 / 中途切换 / 闲忙时联动），
// MidSwitch 是「会话中途切换」子开关；关闭 = 不启用这些策略、渠道保持亲和。
// **产品默认（2026-10-30 定稿）：Enabled=true、MidSwitch=true、ShadowEnabled=false**，
// 与种子 routing.session_cost 一致；配置读不到/解析失败时回退故障安全态（三态全关）。
//
// 注意：成本比较口径（三档成本价 × 时段系数 × 上下文分档系数，替代原「分档系数恒 1」）
// 是 P0 既定新基线，**不由该开关回滚**；默认种子下模型级 context_tiers 为空、
// 全局分档为空，故默认部署排序不变。该配置仍不改变对外售价计费与响应语义（成本价 ≠ 售价）。
//
// 配置读取失败或解析失败时回退故障安全态（safeSessionCostConfig，三态全关），
// 避免配置异常被放大成路由行为异常；「配置里少写某字段」则用产品默认补齐。
type SessionCostConfig struct {
	Enabled bool `json:"enabled"`
	// RemainingA/RemainingB/RemainingMin/RemainingMax 是「预计剩余轮数」参数：
	// 剩余 = clamp(RemainingA − RemainingB×k, RemainingMin, RemainingMax)，k = 会话已完成轮数。
	// 默认 RemainingA=10、RemainingB=1 → 剩余按真实轮次递减（10,9,8…1），
	// 不再写死 10（写死会让第 29 轮的会话仍被判「还有 10 轮可回本」，从而每轮都切、每切丢一次 prefix cache）。
	RemainingA   float64 `json:"remaining_a"`
	RemainingB   float64 `json:"remaining_b"`
	RemainingMin int     `json:"remaining_min"`
	RemainingMax int     `json:"remaining_max"`
	MinRemaining int     `json:"min_remaining"`
	// CooldownSec 是同一会话两次切换之间的最小间隔（秒）：距上次切换不足该值则跳过。
	// 默认 300（5 分钟）——每次切换都会丢上游 prefix cache，必须有冷却，否则会抖动。
	CooldownSec int `json:"cooldown_sec"`
	// ShadowEnabled 是「影子观测」的独立开关（P0），默认 false：
	// 开启后，即便未启用切换（Enabled/MidSwitch 任一为 false），也在会话亲和命中且有更优候选时
	// 产出「若启用则会切换」的假设节省事件（Shadow=true、渠道不变），供账实校准。
	// 默认关闭，保证安全态下不产生任何额外异步写（WAL/事件流）与可观测副作用。
	ShadowEnabled bool `json:"shadow_enabled"`
	// MidSwitch 是「会话中途切换」这一子阶段的独立开关（P2），**产品默认 true**（2026-10-30 定稿）：
	// 显式置 false 即可在中途保持会话亲和、渠道不变（与 Enabled 独立回退）。
	// 与设计 D10「P0 影子只读 → P1 起点择优 → P2 中途切换，逐级开关、可独立回退」一致：
	// Enabled 为总开关，MidSwitch 为 P2 子开关（须 Enabled 与 MidSwitch 同时为 true 才启用中途切换）。
	MidSwitch bool `json:"mid_switch"`
	// MinImprove 是中途切换的滞回阈值（相对改善比例，0 ≤ x < 1）：仅当新渠道本轮成本相对当前渠道
	// 的改善**严格超过**该比例才切换，抑制成本接近时来回横跳。默认 0.05（改善 > 5% 才切）；0 = 只要更便宜就切。
	MinImprove float64 `json:"min_improve"`
}

// defaultSessionCostConfig 返回**产品默认**配置（2026-10-30 spencer 定稿，Owl 复审补滞回）：
// 起点择优（Enabled）与会话中途切换（MidSwitch）默认开启，影子观测（ShadowEnabled）默认关闭；
// 同时**默认就带最小滞回**，否则「默认开」不安全：
//   - RemainingB=1：剩余轮数按真实轮次递减（clamp(10−k, 1, 20)），不再恒为 10——
//     否则第 29 轮的会话仍被当成「还有 10 轮可回本」，只要目标渠道回本快就每轮都切，每切一次丢一次上游 prefix cache；
//   - CooldownSec=300：同会话两次切换至少间隔 5 分钟，避免来回横跳；
//   - MinImprove=0.05：新渠道本轮成本相对当前渠道的改善必须超过 5% 才切（抑制成本接近时抖动）。
//
// 与种子 routing.session_cost 的默认值逐字段一致。
//
// 注意区分 safeSessionCostConfig：本函数是「配置里没写这个字段时该取什么」，
// 而「配置根本读不到/解析不了/装配层没注入时该取什么」必须用故障安全态（关闭），
// 否则一次 DB 抖动或装配遗漏就会凭默认值改变路由行为。
func defaultSessionCostConfig() SessionCostConfig {
	return SessionCostConfig{
		Enabled:      true,
		MidSwitch:    true,
		RemainingA:   10,
		RemainingB:   1, // 按真实轮次递减（原 0 = 恒 10，会把长会话一直判成「还能回本」）
		RemainingMin: 1,
		RemainingMax: 20,
		MinRemaining: 1,
		CooldownSec:  300, // 5 分钟切换冷却
		MinImprove:   0.05,
	}
}

// safeSessionCostConfig 返回**故障安全态**：配置读取失败 / 解析失败 / 未装配配置源时使用。
// 三态开关一律关闭（渠道不变、无额外异步写），避免配置异常被放大成路由行为异常。
func safeSessionCostConfig() SessionCostConfig {
	cfg := defaultSessionCostConfig()
	cfg.Enabled = false
	cfg.MidSwitch = false
	cfg.ShadowEnabled = false
	return cfg
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

// Config 返回会话成本路由配置。
//   - 读到并且能解析 → 用配置值（缺字段用产品默认补：enabled/mid_switch=true）；
//   - 未装配配置源 / 读取失败 / 解析失败 → 回退故障安全态（三态开关全关，渠道不变）。
func (p *SessionCostProvider) Config() SessionCostConfig {
	if p == nil {
		return safeSessionCostConfig()
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
		return safeSessionCostConfig()
	}

	raw, err := src.GetConfig(context.Background(), cfgSessionCost)
	if err != nil {
		return safeSessionCostConfig()
	}
	cfg, ok := parseSessionCostConfig(raw)
	if !ok {
		return safeSessionCostConfig()
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
		MidSwitch    *bool    `json:"mid_switch"`
		MinImprove   *float64 `json:"min_improve"`
		ShadowEnable *bool    `json:"shadow_enabled"`
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
	if in.MidSwitch != nil {
		cfg.MidSwitch = *in.MidSwitch
	}
	if in.MinImprove != nil {
		cfg.MinImprove = *in.MinImprove
	}
	if in.ShadowEnable != nil {
		cfg.ShadowEnabled = *in.ShadowEnable
	}
	return cfg, true
}
