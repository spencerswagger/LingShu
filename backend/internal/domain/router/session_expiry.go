package router

import (
	"context"
	"encoding/json"
	"sync"
	"time"
)

const (
	// cfgSessionExpiration 会话过期配置在 sys_configs 中的键。
	cfgSessionExpiration = "session.expiration"
	// defaultSessionExpirationTTLMinutes 兜底默认 TTL（分钟）。
	defaultSessionExpirationTTLMinutes = 5
)

// ContextTier 上下文档位：按上下文 token 数上界映射 TTL。
type ContextTier struct {
	MaxTokens  int `json:"max_tokens"`
	TTLMinutes int `json:"ttl_minutes"`
}

// TurnTier 对话档位：按对话轮数上界映射 TTL。
type TurnTier struct {
	MaxTurns   int `json:"max_turns"`
	TTLMinutes int `json:"ttl_minutes"`
}

// SessionExpirationConfig 会话过期配置。
type SessionExpirationConfig struct {
	DefaultTTLMinutes int           `json:"default_ttl_minutes"`
	ContextTiers      []ContextTier `json:"context_tiers"`
	TurnTiers         []TurnTier    `json:"turn_tiers"`
}

// configSource 配置读取源；返回 []byte 以匹配 *billing.SqlStore.GetConfig 签名。
type configSource interface {
	GetConfig(ctx context.Context, key string) ([]byte, error)
}

// SessionExpiryProvider 按需读取并缓存会话过期配置。
type SessionExpiryProvider struct {
	src    configSource
	mu     sync.RWMutex
	cached *SessionExpirationConfig
}

// NewSessionExpiryProvider 构造提供者。
func NewSessionExpiryProvider(src configSource) *SessionExpiryProvider {
	return &SessionExpiryProvider{src: src}
}

// Config 返回会话过期配置；解析失败或缺失时返回兜底值且不写缓存，便于自愈。
func (p *SessionExpiryProvider) Config() SessionExpirationConfig {
	p.mu.RLock()
	if p.cached != nil {
		c := *p.cached
		p.mu.RUnlock()
		return c
	}
	src := p.src
	p.mu.RUnlock()

	fallback := SessionExpirationConfig{DefaultTTLMinutes: defaultSessionExpirationTTLMinutes}
	if src == nil {
		return fallback
	}
	raw, err := src.GetConfig(context.Background(), cfgSessionExpiration)
	if err != nil {
		return fallback
	}
	var cfg SessionExpirationConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return fallback
	}

	p.mu.Lock()
	p.cached = &cfg
	p.mu.Unlock()
	return cfg
}

// TTL 计算会话过期时长：取 default 与命中的上下文/对话档位 TTL 的最大值。
func (p *SessionExpiryProvider) TTL(ctxTokens, turns int) time.Duration {
	cfg := p.Config()

	def := cfg.DefaultTTLMinutes
	if def <= 0 {
		def = defaultSessionExpirationTTLMinutes
	}
	best := def

	if ttl := pickContextTTL(cfg.ContextTiers, ctxTokens); ttl > best {
		best = ttl
	}
	if ttl := pickTurnTTL(cfg.TurnTiers, turns); ttl > best {
		best = ttl
	}
	return time.Duration(best) * time.Minute
}

// Invalidate 清空缓存。
func (p *SessionExpiryProvider) Invalidate() {
	p.mu.Lock()
	p.cached = nil
	p.mu.Unlock()
}

// pickContextTTL 选择命中上下文档位的 TTLMinutes：取第一个上界 ≥ 实际值的档；
// 超过最大上界时取最大档；无档位或命中档 TTL<=0 时返回 0。
func pickContextTTL(tiers []ContextTier, tokens int) int {
	if len(tiers) == 0 {
		return 0
	}
	bestIdx := -1
	for i := range tiers {
		if tiers[i].MaxTokens >= tokens {
			if bestIdx == -1 || tiers[i].MaxTokens < tiers[bestIdx].MaxTokens {
				bestIdx = i
			}
		}
	}
	if bestIdx == -1 {
		// 超过所有上界：取最大上界所在档。
		maxIdx := 0
		for i := range tiers {
			if tiers[i].MaxTokens > tiers[maxIdx].MaxTokens {
				maxIdx = i
			}
		}
		bestIdx = maxIdx
	}
	ttl := tiers[bestIdx].TTLMinutes
	if ttl <= 0 {
		return 0
	}
	return ttl
}

// pickTurnTTL 选择命中对话档位的 TTLMinutes，规则同 pickContextTTL。
func pickTurnTTL(tiers []TurnTier, turns int) int {
	if len(tiers) == 0 {
		return 0
	}
	bestIdx := -1
	for i := range tiers {
		if tiers[i].MaxTurns >= turns {
			if bestIdx == -1 || tiers[i].MaxTurns < tiers[bestIdx].MaxTurns {
				bestIdx = i
			}
		}
	}
	if bestIdx == -1 {
		maxIdx := 0
		for i := range tiers {
			if tiers[i].MaxTurns > tiers[maxIdx].MaxTurns {
				maxIdx = i
			}
		}
		bestIdx = maxIdx
	}
	ttl := tiers[bestIdx].TTLMinutes
	if ttl <= 0 {
		return 0
	}
	return ttl
}