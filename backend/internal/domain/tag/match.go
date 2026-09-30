package tag

// StrictContains 报告 tagKV 是否被 channelTags 严格包含：tagKV 中的每个键值对
// 都必须出现在 channelTags 中；channelTags 中多余的键不影响结果。
//
// 空 tagKV（非 nil）恒返回 true。由路由层负责语义：无标签（nil）走默认路由，
// 不调用本函数；有标签时才调用本函数做严格匹配，因此空 tagKV 返回 true 可接受，
// 不引入路由误判。
func StrictContains(channelTags, tagKV map[string]string) bool {
	for k, v := range tagKV {
		cv, ok := channelTags[k]
		if !ok || cv != v {
			return false
		}
	}
	return true
}
