package upstream

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"
)

// mutableConfigSource 是可变的 sys_configs 替身：用例改值后能观察「多久才生效」。
type mutableConfigSource struct {
	mu    sync.Mutex
	vals  map[string]string
	reads int
	err   error
}

func newMutableSource(vals map[string]string) *mutableConfigSource {
	return &mutableConfigSource{vals: vals}
}

func (f *mutableConfigSource) GetConfig(_ context.Context, key string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.err != nil {
		return nil, f.err
	}
	v, ok := f.vals[key]
	if !ok {
		return nil, errors.New("键不存在")
	}
	return []byte(v), nil
}

func (f *mutableConfigSource) set(key, val string) {
	f.mu.Lock()
	f.vals[key] = val
	f.mu.Unlock()
}

func (f *mutableConfigSource) readCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads
}

func degradeJSON(passthrough, ledger bool) string {
	b, _ := json.Marshal(map[string]bool{
		"passthrough_enabled": passthrough,
		"ledger_enabled":      ledger,
		"sync_enabled":        true,
	})
	return string(b)
}

// TestDegradeGate_TTLMakesSwitchTakeEffectWithoutRestart 是这层的存在理由：
// `ConfigProvider.Load` 的缓存没有 TTL，每请求直接 Load 会永远读到启动值 ——
// 运维关掉开关不重启就无效（真机复现：DB 改 false、不重启仍 401，重启才 503）。
func TestDegradeGate_TTLMakesSwitchTakeEffectWithoutRestart(t *testing.T) {
	src := newMutableSource(map[string]string{
		ConfigKeyBaseURL:  "http://127.0.0.1:3425",
		ConfigKeyTimeouts: `{"stream_first_byte_ms":30000,"stream_idle_ms":120000}`,
		ConfigKeyDegrade:  degradeJSON(true, true),
	})
	gate := NewDegradeGate(NewConfigProvider(src), time.Second)
	now := time.Unix(1700000000, 0)
	gate.now = func() time.Time { return now }

	if !gate.Passthrough(context.Background()) {
		t.Fatalf("初始应为打开")
	}

	// 运维关掉开关，不重启。
	src.set(ConfigKeyDegrade, degradeJSON(false, false))
	if !gate.Passthrough(context.Background()) {
		t.Fatalf("TTL 未到期前应沿用快照（不能每请求打 DB，但也不能永不更新）")
	}

	// TTL 到期后必须生效（不许要求重启）。
	now = now.Add(2 * time.Second)
	if gate.Passthrough(context.Background()) {
		t.Fatalf("TTL 到期后必须读到新值：开关冻结在启动值 = 运维改了也没用")
	}
	if gate.LedgerActive(context.Background()) {
		t.Fatalf("账本开关同样要跟着生效")
	}

	// 改回来也要生效（双向）。
	src.set(ConfigKeyDegrade, degradeJSON(true, true))
	now = now.Add(2 * time.Second)
	if !gate.Passthrough(context.Background()) {
		t.Fatalf("改回打开后必须生效（双向热更）")
	}
}

// TestDegradeGate_ReadsAtMostOncePerTTL 断言 TTL 内的重复调用不打 DB（否则配置表成热点）。
func TestDegradeGate_ReadsAtMostOncePerTTL(t *testing.T) {
	src := newMutableSource(map[string]string{
		ConfigKeyDegrade: degradeJSON(true, true),
	})
	gate := NewDegradeGate(NewConfigProvider(src), time.Minute)
	now := time.Unix(1700000000, 0)
	gate.now = func() time.Time { return now }

	gate.Passthrough(context.Background())
	before := src.readCount()
	if before == 0 {
		t.Fatalf("首次必须真读一次")
	}
	for i := 0; i < 100; i++ {
		gate.Passthrough(context.Background())
	}
	if got := src.readCount(); got != before {
		t.Fatalf("TTL 内重复调用不应再读 DB：exposed %d 次读（首次 %d）", got, before)
	}
	now = now.Add(2 * time.Minute)
	gate.Passthrough(context.Background())
	if got := src.readCount(); got == before {
		t.Fatalf("TTL 到期后应重新读一次")
	}
}

// TestDegradeGate_LedgerActiveRequiresBaseURL 账本可用 = 基址已配 && 开关开。
//
// 默认 `upstream.base_url` 是空串，这正是「对账写假 ok」的成因：只看开关会算「开着」。
func TestDegradeGate_LedgerActiveRequiresBaseURL(t *testing.T) {
	src := newMutableSource(map[string]string{
		ConfigKeyBaseURL: "",
		ConfigKeyDegrade: degradeJSON(true, true),
	})
	gate := NewDegradeGate(NewConfigProvider(src), 0) // TTL=0 → 每次真读
	if gate.LedgerActive(context.Background()) {
		t.Fatalf("基址未配时账本必须判为不可用（否则窗口写成 ok/0 对 0，漏账不可见）")
	}
	src.set(ConfigKeyBaseURL, "http://127.0.0.1:3425")
	if !gate.LedgerActive(context.Background()) {
		t.Fatalf("基址配上后应可用")
	}
	// 转发开关与基址无关：基址未配时 /v1 仍应放行。
	src.set(ConfigKeyBaseURL, "")
	if !gate.Passthrough(context.Background()) {
		t.Fatalf("转发门不许依赖 upstream.base_url（那是账本基址，未配置属正常态）")
	}
}

// TestDegradeGate_NilProviderUsesDefaults 未装配配置源时回落代码默认值，不 panic。
func TestDegradeGate_NilProviderUsesDefaults(t *testing.T) {
	gate := NewDegradeGate(nil, 0)
	if !gate.Passthrough(context.Background()) {
		t.Fatalf("默认应放行（默认配置 passthrough_enabled=true）")
	}
	if gate.LedgerActive(context.Background()) {
		t.Fatalf("默认基址为空 → 账本不可用")
	}
}
