package tag

import "testing"

func TestStrictContains(t *testing.T) {
	channelTags := map[string]string{"region": "cn", "tier": "gold", "misc": "x"}

	t.Run("完全匹配通过", func(t *testing.T) {
		if !StrictContains(channelTags, map[string]string{"region": "cn", "tier": "gold"}) {
			t.Fatal("期望全部键值匹配通过")
		}
	})
	t.Run("缺 k 失败", func(t *testing.T) {
		if StrictContains(channelTags, map[string]string{"region": "cn", "tier": "gold", "zone": "a"}) {
			t.Fatal("期望缺失键失败")
		}
	})
	t.Run("v 不同失败", func(t *testing.T) {
		if StrictContains(channelTags, map[string]string{"region": "us"}) {
			t.Fatal("期望值不同失败")
		}
	})
	t.Run("空 tagKV 恒 true", func(t *testing.T) {
		// 空 tagKV 视同匹配：路由层只在有标签时调用本函数，无标签走默认路由，
		// 因此空 map 返回 true 可接受，不引入路由误判。
		if !StrictContains(channelTags, map[string]string{}) {
			t.Fatal("期望空 tagKV 返回 true")
		}
	})
	t.Run("nil channelTags 不匹配", func(t *testing.T) {
		if StrictContains(nil, map[string]string{"region": "cn"}) {
			t.Fatal("期望 channelTags 为 nil 时失败")
		}
	})
}
