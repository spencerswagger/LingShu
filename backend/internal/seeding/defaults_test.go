// defaults_test.go 守住「种子默认值」与「代码默认值」不漂移：
// 订阅池前缀名单同时存在于 seeding（写库默认）与 magpie（读失败回退），两处必须一字不差，
// 否则「配置缺失时拦截什么」会悄悄变。
package seeding

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/team/llmgateway/internal/domain/magpie"
)

// TestSeedConfigs_SubscriptionPrefixesMatchesCodeDefault 断言两处默认名单一致。
func TestSeedConfigs_SubscriptionPrefixesMatchesCodeDefault(t *testing.T) {
	raw, ok := defaultSysConfigs[magpie.SubscriptionPrefixesKey]
	if !ok {
		t.Fatalf("种子缺少 %s：新库启动后前缀名单将无默认值", magpie.SubscriptionPrefixesKey)
	}
	var seeded []string
	if err := json.Unmarshal([]byte(raw), &seeded); err != nil {
		t.Fatalf("种子值不是合法 JSON 数组: %v", err)
	}
	if !reflect.DeepEqual(seeded, magpie.DefaultSubscriptionPrefixes) {
		t.Fatalf("种子名单与代码默认名单不一致\n seed: %v\n code: %v", seeded, magpie.DefaultSubscriptionPrefixes)
	}
}
