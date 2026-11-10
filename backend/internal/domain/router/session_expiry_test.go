package router

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeConfigSource 内存配置源：按 key 返回预设 JSON 字节，并统计调用次数。
type fakeConfigSource struct {
	mu    sync.Mutex
	data  map[string][]byte
	calls int
}

func newFakeConfigSource(raw map[string][]byte) *fakeConfigSource {
	return &fakeConfigSource{data: raw}
}

func (f *fakeConfigSource) GetConfig(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	v, ok := f.data[key]
	if !ok {
		return nil, errors.New("key not found")
	}
	return v, nil
}

func (f *fakeConfigSource) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func TestSessionExpiryContextTierOnly(t *testing.T) {
	src := newFakeConfigSource(map[string][]byte{
		cfgSessionExpiration: []byte(`{
			"default_ttl_minutes": 5,
			"context_tiers": [
				{"max_tokens": 1000, "ttl_minutes": 10},
				{"max_tokens": 5000, "ttl_minutes": 30}
			],
			"turn_tiers": []
		}`),
	})
	p := NewSessionExpiryProvider(src)
	if got := p.TTL(800, 3); got != 10*time.Minute {
		t.Fatalf("TTL(800,3) = %v, want 10m", got)
	}
	if got := p.TTL(4000, 3); got != 30*time.Minute {
		t.Fatalf("TTL(4000,3) = %v, want 30m", got)
	}
}

func TestSessionExpiryTurnTierOnly(t *testing.T) {
	src := newFakeConfigSource(map[string][]byte{
		cfgSessionExpiration: []byte(`{
			"default_ttl_minutes": 5,
			"context_tiers": [],
			"turn_tiers": [
				{"max_turns": 2, "ttl_minutes": 15},
				{"max_turns": 10, "ttl_minutes": 45}
			]
		}`),
	})
	p := NewSessionExpiryProvider(src)
	if got := p.TTL(0, 1); got != 15*time.Minute {
		t.Fatalf("TTL(0,1) = %v, want 15m", got)
	}
	if got := p.TTL(0, 8); got != 45*time.Minute {
		t.Fatalf("TTL(0,8) = %v, want 45m", got)
	}
}

func TestSessionExpiryBothTiersTakeMax(t *testing.T) {
	src := newFakeConfigSource(map[string][]byte{
		cfgSessionExpiration: []byte(`{
			"default_ttl_minutes": 5,
			"context_tiers": [{"max_tokens": 1000, "ttl_minutes": 20}],
			"turn_tiers": [{"max_turns": 10, "ttl_minutes": 60}]
		}`),
	})
	p := NewSessionExpiryProvider(src)
	if got := p.TTL(500, 5); got != 60*time.Minute {
		t.Fatalf("TTL(500,5) = %v, want 60m", got)
	}
}

func TestSessionExpiryFallbackEmptyOrMissing(t *testing.T) {
	// 空配置对象。
	pEmpty := NewSessionExpiryProvider(newFakeConfigSource(map[string][]byte{
		cfgSessionExpiration: []byte(`{}`),
	}))
	if got := pEmpty.TTL(100, 1); got != 5*time.Minute {
		t.Fatalf("empty config TTL = %v, want 5m", got)
	}

	// key 缺失。
	pMissing := NewSessionExpiryProvider(newFakeConfigSource(map[string][]byte{}))
	if got := pMissing.TTL(100, 1); got != 5*time.Minute {
		t.Fatalf("missing key TTL = %v, want 5m", got)
	}

	// src 为 nil。
	pNil := NewSessionExpiryProvider(nil)
	if got := pNil.TTL(100, 1); got != 5*time.Minute {
		t.Fatalf("nil src TTL = %v, want 5m", got)
	}
}

func TestSessionExpiryFallbackInvalidJSON(t *testing.T) {
	src := newFakeConfigSource(map[string][]byte{
		cfgSessionExpiration: []byte(`{not valid json`),
	})
	p := NewSessionExpiryProvider(src)
	if got := p.TTL(100, 1); got != 5*time.Minute {
		t.Fatalf("invalid json TTL = %v, want 5m", got)
	}
}

func TestSessionExpiryContextExceedsMaxTier(t *testing.T) {
	src := newFakeConfigSource(map[string][]byte{
		cfgSessionExpiration: []byte(`{
			"default_ttl_minutes": 5,
			"context_tiers": [
				{"max_tokens": 1000, "ttl_minutes": 10},
				{"max_tokens": 5000, "ttl_minutes": 30}
			],
			"turn_tiers": []
		}`),
	})
	p := NewSessionExpiryProvider(src)
	if got := p.TTL(999999, 0); got != 30*time.Minute {
		t.Fatalf("TTL(999999,0) = %v, want 30m", got)
	}
}

func TestSessionExpiryTierBoundaryContext(t *testing.T) {
	src := newFakeConfigSource(map[string][]byte{
		cfgSessionExpiration: []byte(`{
			"default_ttl_minutes": 5,
			"context_tiers": [
				{"max_tokens": 1000, "ttl_minutes": 10},
				{"max_tokens": 5000, "ttl_minutes": 30}
			],
			"turn_tiers": []
		}`),
	})
	p := NewSessionExpiryProvider(src)
	// 恰好命中上界 1000 的档。
	if got := p.TTL(1000, 0); got != 10*time.Minute {
		t.Fatalf("TTL(1000,0) = %v, want 10m", got)
	}
}

func TestSessionExpiryTierBoundaryTurn(t *testing.T) {
	src := newFakeConfigSource(map[string][]byte{
		cfgSessionExpiration: []byte(`{
			"default_ttl_minutes": 5,
			"context_tiers": [],
			"turn_tiers": [
				{"max_turns": 2, "ttl_minutes": 15},
				{"max_turns": 10, "ttl_minutes": 45}
			]
		}`),
	})
	p := NewSessionExpiryProvider(src)
	// 恰好命中上界 10 的对话档。
	if got := p.TTL(0, 10); got != 45*time.Minute {
		t.Fatalf("TTL(0,10) = %v, want 45m", got)
	}
}

func TestSessionExpiryDefaultOverridesTiers(t *testing.T) {
	src := newFakeConfigSource(map[string][]byte{
		cfgSessionExpiration: []byte(`{
			"default_ttl_minutes": 120,
			"context_tiers": [{"max_tokens": 1000, "ttl_minutes": 10}],
			"turn_tiers": [{"max_turns": 2, "ttl_minutes": 15}]
		}`),
	})
	p := NewSessionExpiryProvider(src)
	if got := p.TTL(500, 1); got != 120*time.Minute {
		t.Fatalf("TTL(500,1) = %v, want 120m", got)
	}

	// Invalidate 后重新读取，src 应被再次调用。
	before := src.callCount()
	p.Invalidate()
	if got := p.TTL(500, 1); got != 120*time.Minute {
		t.Fatalf("after invalidate TTL = %v, want 120m", got)
	}
	if src.callCount() <= before {
		t.Fatalf("expected src re-read after Invalidate, calls before=%d after=%d", before, src.callCount())
	}
}