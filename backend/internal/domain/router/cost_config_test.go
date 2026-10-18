package router

import "testing"

func TestSessionCostConfigMissingFallsBackToDefault(t *testing.T) {
	p := NewSessionCostProvider(newFakeConfigSource(nil))
	got := p.Config()
	want := defaultSessionCostConfig()
	if got != want {
		t.Fatalf("缺失配置应回退缺省值: got=%+v want=%+v", got, want)
	}
	if got.Enabled {
		t.Fatalf("缺省安全态必须为 Enabled=false: %+v", got)
	}
}

func TestSessionCostConfigMalformedFallsBackToDefault(t *testing.T) {
	src := newFakeConfigSource(map[string][]byte{
		cfgSessionCost: []byte(`{not-json`),
	})
	got := NewSessionCostProvider(src).Config()
	want := defaultSessionCostConfig()
	if got != want {
		t.Fatalf("非法 JSON 应回退缺省值: got=%+v want=%+v", got, want)
	}
}

func TestSessionCostConfigNilSourceIsSafe(t *testing.T) {
	p := NewSessionCostProvider(nil)
	got := p.Config()
	if got != defaultSessionCostConfig() {
		t.Fatalf("nil 配置源应回退缺省值: %+v", got)
	}
	p.Invalidate()
}

func TestSessionCostConfigParsesAllFields(t *testing.T) {
	src := newFakeConfigSource(map[string][]byte{
		cfgSessionCost: []byte(`{
			"enabled": true,
			"remaining_a": 3.5,
			"remaining_b": 0.25,
			"remaining_min": 2,
			"remaining_max": 30,
			"min_remaining": 4,
			"cooldown_sec": 15
		}`),
	})
	got := NewSessionCostProvider(src).Config()
	want := SessionCostConfig{
		Enabled:      true,
		RemainingA:   3.5,
		RemainingB:   0.25,
		RemainingMin: 2,
		RemainingMax: 30,
		MinRemaining: 4,
		CooldownSec:  15,
	}
	if got != want {
		t.Fatalf("解析结果不符: got=%+v want=%+v", got, want)
	}
}

func TestSessionCostConfigFillsOmittedFieldsWithDefaults(t *testing.T) {
	src := newFakeConfigSource(map[string][]byte{
		cfgSessionCost: []byte(`{"enabled": true, "remaining_a": 7}`),
	})
	got := NewSessionCostProvider(src).Config()
	want := defaultSessionCostConfig()
	want.Enabled = true
	want.RemainingA = 7
	if got != want {
		t.Fatalf("缺省字段应补齐: got=%+v want=%+v", got, want)
	}
}

func TestSessionCostConfigInvalidateRefetches(t *testing.T) {
	src := newFakeConfigSource(map[string][]byte{
		cfgSessionCost: []byte(`{"enabled": false, "cooldown_sec": 5}`),
	})
	p := NewSessionCostProvider(src)

	if got := p.Config(); got.CooldownSec != 5 {
		t.Fatalf("首次读取失败: %+v", got)
	}
	if n := src.callCount(); n != 1 {
		t.Fatalf("首次读取应回源 1 次, got=%d", n)
	}

	src.mu.Lock()
	src.data[cfgSessionCost] = []byte(`{"enabled": true, "cooldown_sec": 9}`)
	src.mu.Unlock()

	if got := p.Config(); got.CooldownSec != 5 || got.Enabled {
		t.Fatalf("缓存未失效前应返回旧值: %+v", got)
	}
	if n := src.callCount(); n != 1 {
		t.Fatalf("缓存命中不应回源, got=%d", n)
	}

	p.Invalidate()
	if got := p.Config(); !got.Enabled || got.CooldownSec != 9 {
		t.Fatalf("Invalidate 后应重新读取: %+v", got)
	}
	if n := src.callCount(); n != 2 {
		t.Fatalf("Invalidate 后应回源 1 次, got=%d", n)
	}
}
