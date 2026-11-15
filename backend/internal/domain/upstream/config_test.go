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

// TestConfigProvider_Defaults 断言三项缺省值：基址空（未接入）、超时 30s、降级关。
func TestConfigProvider_Defaults(t *testing.T) {
	p := NewConfigProvider(&fakeConfigSource{vals: map[string]string{}})
	cfg, problems := p.Load(context.Background())
	if cfg.BaseURL != "" {
		t.Fatalf("缺省基址应为空，got %q", cfg.BaseURL)
	}
	if cfg.Timeout != 30*time.Second {
		t.Fatalf("缺省超时应为 30s，got %s", cfg.Timeout)
	}
	if cfg.Degraded {
		t.Fatal("缺省降级开关应为 false")
	}
	if cfg.Enabled() {
		t.Fatal("基址为空时不应视为已接入")
	}
	// 键不存在（老库未种子）也要能跑：问题只告警，不阻断。
	if len(problems) != 3 {
		t.Fatalf("三项均读失败应报 3 条问题，got %v", problems)
	}
}

// TestConfigProvider_LoadsAll 断言三项被正确解析（基址去尾斜杠、超时按毫秒、降级按布尔）。
func TestConfigProvider_LoadsAll(t *testing.T) {
	src := &fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL:   `"http://gw.internal:3425/"`,
		ConfigKeyTimeoutMS: `15000`,
		ConfigKeyDegrade:   `true`,
	}}
	cfg, problems := p0(src)
	if len(problems) != 0 {
		t.Fatalf("三项干净读到不应有问题，got %v", problems)
	}
	if cfg.BaseURL != "http://gw.internal:3425" {
		t.Fatalf("基址应去掉尾部斜杠，got %q", cfg.BaseURL)
	}
	if cfg.Timeout != 15*time.Second {
		t.Fatalf("超时应为 15s，got %s", cfg.Timeout)
	}
	if !cfg.Degraded {
		t.Fatal("降级开关应为 true")
	}
	if !cfg.Enabled() {
		t.Fatal("基址已配置应视为已接入")
	}
}

// TestConfigProvider_TolerantFormats 断言控制台/手工写入的宽松格式（裸文本、数字字符串、布尔字符串）都能吃。
func TestConfigProvider_TolerantFormats(t *testing.T) {
	cfg, problems := p0(&fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL:   `http://gw.internal:3425`,
		ConfigKeyTimeoutMS: `"20000"`,
		ConfigKeyDegrade:   `"false"`,
	}})
	if len(problems) != 0 {
		t.Fatalf("宽松格式不应报问题，got %v", problems)
	}
	if cfg.BaseURL != "http://gw.internal:3425" || cfg.Timeout != 20*time.Second || cfg.Degraded {
		t.Fatalf("解析不符：%+v", cfg)
	}
}

// TestConfigProvider_ReadErrorKeepsLastGood 断言读失败沿用上期值（不清空基址、不停摆）。
func TestConfigProvider_ReadErrorKeepsLastGood(t *testing.T) {
	src := &fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL:   `"http://gw.internal:3425"`,
		ConfigKeyTimeoutMS: `15000`,
		ConfigKeyDegrade:   `false`,
	}}
	p := NewConfigProvider(src)
	first, problems := p.Load(context.Background())
	if len(problems) != 0 {
		t.Fatalf("首次应干净读到，got %v", problems)
	}

	p.Invalidate() // 配置被写入 → 下一轮重读
	src.errs = map[string]error{ConfigKeyBaseURL: errors.New("数据库连接被重置")}
	src.vals[ConfigKeyTimeoutMS] = `45000`

	cfg, problems := p.Load(context.Background())
	if len(problems) != 1 || !strings.Contains(problems[0], ConfigKeyBaseURL) {
		t.Fatalf("应只报基址读取失败，got %v", problems)
	}
	if cfg.BaseURL != first.BaseURL {
		t.Fatalf("读失败必须沿用上期基址 %q，got %q", first.BaseURL, cfg.BaseURL)
	}
	if cfg.Timeout != 45*time.Second {
		t.Fatalf("能读到的项应即时生效（45s），got %s", cfg.Timeout)
	}
}

// TestConfigProvider_InvalidValuesKeepLastGood 断言非法值不覆盖上期值（配置写错不许把链路打空）。
func TestConfigProvider_InvalidValuesKeepLastGood(t *testing.T) {
	src := &fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL:   `"http://gw.internal:3425"`,
		ConfigKeyTimeoutMS: `15000`,
		ConfigKeyDegrade:   `false`,
	}}
	p := NewConfigProvider(src)
	first, _ := p.Load(context.Background())

	p.Invalidate()
	src.vals[ConfigKeyTimeoutMS] = `600000000`                  // 超出上限
	src.vals[ConfigKeyBaseURL] = `"http://user:secret@gw:3425"` // 带凭据
	src.vals[ConfigKeyDegrade] = `"maybe"`                      // 非法布尔

	cfg, problems := p.Load(context.Background())
	if len(problems) != 3 {
		t.Fatalf("三项非法都应报问题，got %v", problems)
	}
	if cfg.BaseURL != first.BaseURL {
		t.Fatalf("带凭据的基址必须被拒绝且沿用上期值 %q，got %q", first.BaseURL, cfg.BaseURL)
	}
	if cfg.Timeout != first.Timeout {
		t.Fatalf("非法超时应沿用上期值 %s，got %s", first.Timeout, cfg.Timeout)
	}
	if cfg.Degraded != first.Degraded {
		t.Fatalf("非法开关应沿用上期值 %v，got %v", first.Degraded, cfg.Degraded)
	}
	if !strings.Contains(strings.Join(problems, "\n"), "凭据") {
		t.Fatalf("带凭据的基址问题文案应明说原因，got %v", problems)
	}
}

// TestConfigProvider_CachesUntilInvalidate 断言缓存行为（热路径不每次查库）与写后失效。
func TestConfigProvider_CachesUntilInvalidate(t *testing.T) {
	src := &fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL:   `"http://a:1"`,
		ConfigKeyTimeoutMS: `15000`,
		ConfigKeyDegrade:   `false`,
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

// TestConfigGate 断言闸门三态：未接入跳过、降级跳过、正常放行。
func TestConfigGate(t *testing.T) {
	ctx := context.Background()

	gate := ConfigGate(NewConfigProvider(&fakeConfigSource{vals: map[string]string{
		ConfigKeyTimeoutMS: `15000`,
		ConfigKeyDegrade:   `false`,
	}}), testLogger())
	err := gate(ctx)
	if !IsSkip(err) || !strings.Contains(err.Error(), ConfigKeyBaseURL) {
		t.Fatalf("基址未配置应跳过并点名键，got %T %v", err, err)
	}

	gate = ConfigGate(NewConfigProvider(&fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL:   `"http://gw:3425"`,
		ConfigKeyTimeoutMS: `15000`,
		ConfigKeyDegrade:   `true`,
	}}), testLogger())
	err = gate(ctx)
	if !IsSkip(err) || !strings.Contains(err.Error(), ConfigKeyDegrade) {
		t.Fatalf("降级开关应跳过并点名键，got %T %v", err, err)
	}

	gate = ConfigGate(NewConfigProvider(&fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL:   `"http://gw:3425"`,
		ConfigKeyTimeoutMS: `15000`,
		ConfigKeyDegrade:   `false`,
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
