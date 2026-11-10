package seeding

// seed_test.go 锁住「种子初始配置」与「代码里的产品默认」同源，避免两处漂移
// （漂移的后果：新库用种子、老库用默认值补齐，同一个页面在两台机器上显示不同的开关）。

import (
	"encoding/json"
	"testing"

	"github.com/team/llmgateway/internal/domain/router"
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
