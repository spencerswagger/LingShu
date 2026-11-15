package upstream

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fakeConfigSource 是 sys_configs 的假读源：可按下标注入值与读取错误。
type fakeConfigSource struct {
	vals map[string]string
	errs map[string]error
	keys []string // 记录读取顺序/次数
}

func (f *fakeConfigSource) GetConfig(ctx context.Context, key string) ([]byte, error) {
	f.keys = append(f.keys, key)
	if err, ok := f.errs[key]; ok {
		return nil, err
	}
	v, ok := f.vals[key]
	if !ok {
		return nil, fmt.Errorf("sys_configs 无键 %s", key)
	}
	return []byte(v), nil
}

// TestConfigProvider_Defaults 断言三键缺省：基址空（未接入）、两段超时 30s/120s、三个开关全开。
func TestConfigProvider_Defaults(t *testing.T) {
	p := NewConfigProvider(&fakeConfigSource{vals: map[string]string{}})
	cfg, problems := p.Load(context.Background())
	if cfg.BaseURL != "" {
		t.Fatalf("缺省基址应为空，got %q", cfg.BaseURL)
	}
	if cfg.Timeouts.StreamFirstByte != DefaultStreamFirstByte || cfg.Timeouts.StreamIdle != DefaultStreamIdle {
		t.Fatalf("缺省超时应为 %s/%s，got %+v",
			DefaultStreamFirstByte, DefaultStreamIdle, cfg.Timeouts)
	}
	if !cfg.Degrade.PassthroughEnabled || !cfg.Degrade.LedgerEnabled || !cfg.Degrade.SyncEnabled {
		t.Fatalf("缺省三段开关应全开（true = 该段可用），got %+v", cfg.Degrade)
	}
	if cfg.Enabled() {
		t.Fatal("基址为空时不应视为已接入")
	}
	// 键不存在（老库未种子）也要能跑：问题只告警，不阻断。
	if len(problems) != 3 {
		t.Fatalf("三项均读失败应报 3 条问题，got %v", problems)
	}
}

// TestConfigProvider_LoadsAll 断言三键被正确解析（基址去尾斜杠、超时逐字段、开关逐字段）。
func TestConfigProvider_LoadsAll(t *testing.T) {
	src := &fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL:  `"http://gw.internal:3425/"`,
		ConfigKeyTimeouts: `{"stream_first_byte_ms":15000,"stream_idle_ms":45000}`,
		ConfigKeyDegrade:  `{"passthrough_enabled":true,"ledger_enabled":false,"sync_enabled":true}`,
	}}
	cfg, problems := p0(src)
	if len(problems) != 0 {
		t.Fatalf("三项干净读到不应有问题，got %v", problems)
	}
	if cfg.BaseURL != "http://gw.internal:3425" {
		t.Fatalf("基址应去掉尾部斜杠，got %q", cfg.BaseURL)
	}
	if cfg.Timeouts.StreamFirstByte != 15*time.Second || cfg.Timeouts.StreamIdle != 45*time.Second {
		t.Fatalf("两段超时解析不符：%+v", cfg.Timeouts)
	}
	if !cfg.Degrade.PassthroughEnabled || cfg.Degrade.LedgerEnabled || !cfg.Degrade.SyncEnabled {
		t.Fatalf("三开关解析不符：%+v", cfg.Degrade)
	}
	if !cfg.Enabled() || !cfg.SyncActive() || cfg.LedgerActive() != false {
		t.Fatalf("派生判定不符：Enabled=%v Sync=%v Ledger=%v",
			cfg.Enabled(), cfg.SyncActive(), cfg.LedgerActive())
	}
}

// TestConfigProvider_TolerantFormats 断言控制台/手工写入的宽松格式（裸文本、数字字符串、布尔字符串）都能吃。
func TestConfigProvider_TolerantFormats(t *testing.T) {
	cfg, problems := p0(&fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL:  `http://gw.internal:3425`,
		ConfigKeyTimeouts: `{"stream_first_byte_ms":"20000","stream_idle_ms":"90000"}`,
		ConfigKeyDegrade:  `{"passthrough_enabled":"false","ledger_enabled":"true","sync_enabled":"true"}`,
	}})
	if len(problems) != 0 {
		t.Fatalf("宽松格式不应报问题，got %v", problems)
	}
	if cfg.BaseURL != "http://gw.internal:3425" || cfg.Timeouts.StreamFirstByte != 20*time.Second ||
		cfg.Timeouts.StreamIdle != 90*time.Second || cfg.Degrade.PassthroughEnabled {
		t.Fatalf("解析不符：%+v", cfg)
	}
}

// TestConfigProvider_LegacyTimeoutKeyIgnored 断言单值 `upstream.timeout_ms` 已不再被读取。
//
// 这条是「删键」的守卫：老库里那个键还在（种子 ON CONFLICT DO NOTHING 不会删它），
// 只要代码还在读它，就会出现「控制台改了没效果」——两种配置来源打架，比没有配置更难查。
func TestConfigProvider_LegacyTimeoutKeyIgnored(t *testing.T) {
	src := &fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL:      `"http://gw:3425"`,
		"upstream.timeout_ms": `9000`,
		ConfigKeyTimeouts:     `{"stream_first_byte_ms":20000,"stream_idle_ms":60000}`,
		ConfigKeyDegrade:      `{"passthrough_enabled":true,"ledger_enabled":true,"sync_enabled":true}`,
	}}
	cfg, problems := p0(src)
	if len(problems) != 0 {
		t.Fatalf("不读旧键不应报问题：%v", problems)
	}
	if cfg.Timeouts.StreamFirstByte != 20*time.Second {
		t.Fatalf("超时应只来自 upstream.timeouts，got %s（旧键被读了？）", cfg.Timeouts.StreamFirstByte)
	}
	for _, k := range src.keys {
		if k == "upstream.timeout_ms" {
			t.Fatalf("配置读取不该再碰 upstream.timeout_ms（F1 已删该键）")
		}
	}
}

// TestConfigProvider_ReadErrorKeepsLastGood 断言读失败沿用上期值（不清空基址、不停摆）。
func TestConfigProvider_ReadErrorKeepsLastGood(t *testing.T) {
	src := &fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL:  `"http://gw.internal:3425"`,
		ConfigKeyTimeouts: `{"stream_first_byte_ms":15000,"stream_idle_ms":60000}`,
		ConfigKeyDegrade:  `{"passthrough_enabled":true,"ledger_enabled":true,"sync_enabled":true}`,
	}}
	p := NewConfigProvider(src)
	first, problems := p.Load(context.Background())
	if len(problems) != 0 {
		t.Fatalf("首次应干净读到，got %v", problems)
	}

	p.Invalidate() // 配置被写入 → 下一轮重读
	src.errs = map[string]error{ConfigKeyBaseURL: errors.New("数据库连接被重置")}
	src.vals[ConfigKeyTimeouts] = `{"stream_first_byte_ms":45000,"stream_idle_ms":60000}`

	cfg, problems := p.Load(context.Background())
	if len(problems) != 1 || !strings.Contains(problems[0], ConfigKeyBaseURL) {
		t.Fatalf("应只报基址读取失败，got %v", problems)
	}
	if cfg.BaseURL != first.BaseURL {
		t.Fatalf("读失败必须沿用上期基址 %q，got %q", first.BaseURL, cfg.BaseURL)
	}
	if cfg.Timeouts.StreamFirstByte != 45*time.Second {
		t.Fatalf("能读到的项应即时生效（45s），got %s", cfg.Timeouts.StreamFirstByte)
	}
}

// TestConfigProvider_InvalidValuesKeepLastGood 断言非法值不覆盖上期值（配置写错不许把链路打空）。
func TestConfigProvider_InvalidValuesKeepLastGood(t *testing.T) {
	src := &fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL:  `"http://gw.internal:3425"`,
		ConfigKeyTimeouts: `{"stream_first_byte_ms":15000,"stream_idle_ms":60000}`,
		ConfigKeyDegrade:  `{"passthrough_enabled":true,"ledger_enabled":true,"sync_enabled":true}`,
	}}
	p := NewConfigProvider(src)
	first, _ := p.Load(context.Background())

	p.Invalidate()
	src.vals[ConfigKeyBaseURL] = `"http://user:secret@gw:3425"`                                              // 带凭据
	src.vals[ConfigKeyTimeouts] = `{"stream_first_byte_ms":600000000,"stream_idle_ms":60000}`                // 首包超上限
	src.vals[ConfigKeyDegrade] = `{"passthrough_enabled":"maybe","ledger_enabled":true,"sync_enabled":true}` // 非法布尔

	cfg, problems := p.Load(context.Background())
	if len(problems) != 3 {
		t.Fatalf("三项非法都应报问题，got %v", problems)
	}
	if cfg.BaseURL != first.BaseURL {
		t.Fatalf("带凭据的基址必须被拒绝且沿用上期值 %q，got %q", first.BaseURL, cfg.BaseURL)
	}
	if cfg.Timeouts.StreamFirstByte != first.Timeouts.StreamFirstByte {
		t.Fatalf("非法首包超时应沿用上期值 %s，got %s",
			first.Timeouts.StreamFirstByte, cfg.Timeouts.StreamFirstByte)
	}
	if !cfg.Degrade.PassthroughEnabled {
		t.Fatalf("非法开关项应沿用上期值 true，got %+v", cfg.Degrade)
	}
	if !strings.Contains(strings.Join(problems, "\n"), "凭据") {
		t.Fatalf("带凭据的基址问题文案应明说原因，got %v", problems)
	}
}

// TestConfigProvider_UnknownKeysReported 断言拼错的键会报问题（静默走默认值最难查）。
func TestConfigProvider_UnknownKeysReported(t *testing.T) {
	_, problems := p0(&fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL:  `"http://gw:3425"`,
		ConfigKeyTimeouts: `{"stream_first_byte":20000,"stream_idle_ms":60000}`, // 少写 _ms
		ConfigKeyDegrade:  `{"passthrough":false,"ledger_enabled":true,"sync_enabled":true}`,
	}})
	if len(problems) != 2 {
		t.Fatalf("两个未知键应各报一条，got %v", problems)
	}
	joined := strings.Join(problems, "\n")
	if !strings.Contains(joined, "stream_first_byte") || !strings.Contains(joined, "passthrough") {
		t.Fatalf("问题文案应点名未知键，got %v", problems)
	}
}

// TestConfigProvider_PartialDegradeKeepsOtherSwitches 断言逐开关回落（坏一项不连累其余）。
func TestConfigProvider_PartialDegradeKeepsOtherSwitches(t *testing.T) {
	src := &fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL:  `"http://gw:3425"`,
		ConfigKeyTimeouts: `{"stream_first_byte_ms":20000,"stream_idle_ms":60000}`,
		ConfigKeyDegrade:  `{"passthrough_enabled":true,"ledger_enabled":true,"sync_enabled":true}`,
	}}
	p := NewConfigProvider(src)
	first, _ := p.Load(context.Background())

	p.Invalidate()
	src.vals[ConfigKeyDegrade] = `{"passthrough_enabled":"bad","ledger_enabled":false,"sync_enabled":false}`
	cfg, problems := p.Load(context.Background())
	if len(problems) != 1 {
		t.Fatalf("只有一个坏开关，应报一条，got %v", problems)
	}
	if !cfg.Degrade.PassthroughEnabled {
		t.Fatalf("坏的 passthrough_enabled 应沿用上期 %v", first.Degrade.PassthroughEnabled)
	}
	if cfg.Degrade.LedgerEnabled || cfg.Degrade.SyncEnabled {
		t.Fatalf("同块其它开关应按新值生效，got %+v", cfg.Degrade)
	}
}

// TestConfigProvider_CachesUntilInvalidate 断言缓存行为（热路径不每次查库）与写后失效。
func TestConfigProvider_CachesUntilInvalidate(t *testing.T) {
	src := &fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL:  `"http://a:1"`,
		ConfigKeyTimeouts: `{"stream_first_byte_ms":20000,"stream_idle_ms":60000}`,
		ConfigKeyDegrade:  `{"passthrough_enabled":true,"ledger_enabled":true,"sync_enabled":true}`,
	}}
	p := NewConfigProvider(src)
	if _, problems := p.Load(context.Background()); len(problems) != 0 {
		t.Fatalf("首次读到问题：%v", problems)
	}
	reads := len(src.keys)

	src.vals[ConfigKeyBaseURL] = `"http://b:2"`
	cfg, problems := p.Load(context.Background())
	if len(problems) != 0 {
		t.Fatalf("缓存命中不应报问题：%v", problems)
	}
	if cfg.BaseURL != "http://a:1" {
		t.Fatalf("未失效前应命中缓存，got %q", cfg.BaseURL)
	}
	if len(src.keys) != reads {
		t.Fatalf("缓存命中不应再查库，多读了 %d 次", len(src.keys)-reads)
	}

	p.Invalidate()
	cfg, _ = p.Load(context.Background())
	if cfg.BaseURL != "http://b:2" {
		t.Fatalf("失效后应读到新值，got %q", cfg.BaseURL)
	}
}

// TestConfigKeys_CarryNoSecrets 守住红线：sys_configs 键里永不出现密钥类配置。
func TestConfigKeys_CarryNoSecrets(t *testing.T) {
	keys := ConfigKeys()
	if len(keys) != 3 {
		t.Fatalf("本期只允许三项运行参数，got %v", keys)
	}
	want := map[string]bool{ConfigKeyBaseURL: true, ConfigKeyTimeouts: true, ConfigKeyDegrade: true}
	for _, k := range keys {
		if !want[k] {
			t.Fatalf("出现未批准的配置键 %q（F1 只允许 base_url/timeouts/degrade）", k)
		}
	}
	forbidden := []string{"key", "secret", "token", "password", "passwd", "auth", "credential", "admin"}
	for _, k := range keys {
		lower := strings.ToLower(k)
		for _, bad := range forbidden {
			if strings.Contains(lower, bad) {
				t.Fatalf("配置键 %q 命中密钥类词 %q：密钥不得进 sys_configs", k, bad)
			}
		}
	}
}

// TestConfigGate 断言闸门三态：未接入跳过、同步开关关闭跳过、正常放行。
func TestConfigGate(t *testing.T) {
	ctx := context.Background()

	gate := ConfigGate(NewConfigProvider(&fakeConfigSource{vals: map[string]string{
		ConfigKeyTimeouts: `{"stream_first_byte_ms":20000,"stream_idle_ms":60000}`,
		ConfigKeyDegrade:  `{"passthrough_enabled":true,"ledger_enabled":true,"sync_enabled":true}`,
	}}), testLogger())
	err := gate(ctx)
	if !IsSkip(err) || !strings.Contains(err.Error(), ConfigKeyBaseURL) {
		t.Fatalf("基址未配置应跳过并点名键，got %T %v", err, err)
	}

	gate = ConfigGate(NewConfigProvider(&fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL:  `"http://gw:3425"`,
		ConfigKeyTimeouts: `{"stream_first_byte_ms":20000,"stream_idle_ms":60000}`,
		ConfigKeyDegrade:  `{"passthrough_enabled":true,"ledger_enabled":true,"sync_enabled":false}`,
	}}), testLogger())
	err = gate(ctx)
	if !IsSkip(err) || !strings.Contains(err.Error(), ConfigKeyDegrade) {
		t.Fatalf("同步开关关闭应跳过并点名键，got %T %v", err, err)
	}

	gate = ConfigGate(NewConfigProvider(&fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL:  `"http://gw:3425"`,
		ConfigKeyTimeouts: `{"stream_first_byte_ms":20000,"stream_idle_ms":60000}`,
		ConfigKeyDegrade:  `{"passthrough_enabled":true,"ledger_enabled":true,"sync_enabled":true}`,
	}}), testLogger())
	if err := gate(ctx); err != nil {
		t.Fatalf("配置齐全应放行，got %v", err)
	}
}

// TestConfigGate_NilProvider 断言未装配配置源时安全跳过（不 nil panic，也不打空地址）。
func TestConfigGate_NilProvider(t *testing.T) {
	err := ConfigGate(NewConfigProvider(nil), testLogger())(context.Background())
	if !IsSkip(err) {
		t.Fatalf("未装配配置源应跳过，got %T %v", err, err)
	}
}

// p0 是「加载并发问题」helper：只取配置不关心问题。
func p0(src ConfigSource) (RuntimeConfig, []string) {
	return NewConfigProvider(src).Load(context.Background())
}
