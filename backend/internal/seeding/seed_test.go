package seeding

// seed_test.go 锁住「种子初始配置」与「代码里的产品默认」同源，避免两处漂移
// （漂移的后果：新库用种子、老库用默认值补齐，同一个页面在两台机器上显示不同的开关）。

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/domain/upstream"
)

// TestSeedSessionCostMatchesProductDefault 断言 routing.session_cost 的种子与
// router.DefaultSessionCostConfig() 逐字段一致，且三态开关为 true/true/false。
func TestSeedSessionCostMatchesProductDefault(t *testing.T) {
	raw, ok := defaultSysConfigs["routing.session_cost"]
	if !ok {
		t.Fatalf("种子缺少 routing.session_cost")
	}
	var seeded router.SessionCostConfig
	if err := json.Unmarshal([]byte(raw), &seeded); err != nil {
		t.Fatalf("种子不是合法 JSON: %v (%s)", err, raw)
	}
	want := router.DefaultSessionCostConfig()
	if seeded != want {
		t.Fatalf("种子与产品默认不一致:\n seed=%+v\n want=%+v", seeded, want)
	}
	if !want.Enabled || !want.MidSwitch || want.ShadowEnabled {
		t.Fatalf("三态开关产品默认应为 true/true/false，got %+v", want)
	}
}

// TestSeedSessionCostIsParsedAsProductDefault 断言种子经运行时同一解析后仍等于产品默认
// （防止种子里字段名拼错导致「落库了但运行时读成零值」）。
func TestSeedSessionCostIsParsedAsProductDefault(t *testing.T) {
	raw := defaultSysConfigs["routing.session_cost"]
	got, ok := router.ParseSessionCostConfig([]byte(raw))
	if !ok {
		t.Fatalf("种子解析失败")
	}
	if got != router.DefaultSessionCostConfig() {
		t.Fatalf("种子解析结果与产品默认不一致:\n got=%+v\n want=%+v", got, router.DefaultSessionCostConfig())
	}
}

// seedConfigSource 用种子里写的值充当 sys_configs 读源：让断言走**运行时同一解析路径**，
// 避免出现「种子落库了、但字段名/格式写错、运行时读成零值」这种假通过。
type seedConfigSource struct{}

func (seedConfigSource) GetConfig(_ context.Context, key string) ([]byte, error) {
	v, ok := defaultSysConfigs[key]
	if !ok {
		return nil, fmt.Errorf("种子缺少 %s", key)
	}
	return []byte(v), nil
}

// TestSeedUpstreamConfigMatchesCodeDefault 断言上游三项运行参数（基址/超时/降级）的种子与
// upstream.DefaultRuntimeConfig 一致，且解析出来就是该值。
func TestSeedUpstreamConfigMatchesCodeDefault(t *testing.T) {
	for _, key := range upstream.ConfigKeys() {
		if _, ok := defaultSysConfigs[key]; !ok {
			t.Fatalf("种子缺少 %s：新库启动后该项将无默认值", key)
		}
	}
	cfg, problems := upstream.NewConfigProvider(seedConfigSource{}).Load(context.Background())
	if len(problems) != 0 {
		t.Fatalf("种子值应能被运行时解析，问题：%v", problems)
	}
	want := upstream.DefaultRuntimeConfig()
	if cfg != want {
		t.Fatalf("种子与代码默认不一致:\n seed=%+v\n want=%+v", cfg, want)
	}
	if cfg.Enabled() {
		t.Fatal("种子基址应为空（未接入）：新库不应默认向上游发流量")
	}
	if !cfg.Degrade.PassthroughEnabled || !cfg.Degrade.LedgerEnabled || !cfg.Degrade.SyncEnabled {
		t.Fatalf("种子三段开关应为全开（true = 该段可用），got %+v", cfg.Degrade)
	}
}
