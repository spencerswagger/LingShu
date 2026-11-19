package v3billing

import "strings"

// 售价解析：把账本行的「模型名 / 组名」解析到一条对外模型（售价来源）。
//
// 为什么需要一条链而不是一次等值查询：账本行的 req_model 是**客户端请求里的模型名**，
// 它可能是
//   - 对外模型名本身（客户端直接点名单个模型）；
//   - magpie 组 ref（group/<id>，组路由请求时 req_model 就是组身份）；
//   - 组的人可读名（display_name，例如拓扑页上显示的名字）；
//   - auto 组的 agent 形态 auto/<model>（magpie 给 agent 的 ref 形态，与镜像/账本里的
//     group/auto-<slug> 不是同一个字符串）。
// 而灵枢的售价挂在 external_models 上，绑定真源是 bound_internal_ref（= 组 ref）。
//
// 解析顺序（纯结构规则，**禁止对任何具体名字特判** —— 随机的组名/前缀组合必须按同一规则工作）：
//  1. route_id > 0：这行账是网关按路由组转发的，先按**组 ref 匹配绑定**（绑定的售价才是
//     这行账的计费口径），再退回按名字匹配；
//  2. route_id == 0：session/reported 来源，客户端点名单个模型 → 先按名字匹配，再按组 ref 兜底；
//  3. 组快照（internal_models, kind='group'）只用于把「人可读名」映射到 ref —— 名字无法靠
//     前缀规则推导，必须查快照；快照缺失不报错，只是少这一档候选。
//
// 注意（已知边界）：route_id → 组 ref 的权威映射只在 magpie 的 routing trace 里（内存、仅保留
// 最近若干条），灵枢镜像不到。所以 route_id 在这里的作用是「声明这行账是组路由的」+ 审计留痕，
// 组 ref 由本行的 req_model/served_model 与组快照推导。少了这条链，组路由的账会永远结算不出去，
// 占满每轮批次头位把后面的正常账单堵死（真机事故）。

// groupNode 是 magpie 组快照（internal_models, kind='group'）的一行。
type groupNode struct {
	Ref         string // 形如 group/auto-flash
	DisplayName string // 形如 auto-flash（人可读名）
}

// priceCandidate 是一个售价候选：值 + 匹配列。
type priceCandidate struct {
	value string
	// byBinding=true 表示按 external_models.bound_internal_ref（组 ref）匹配；
	// false 表示按 external_name 匹配。
	byBinding bool
}

// groupRefForm 把可能是组名字符串归一成 magpie 的组 ref 形态 group/<id>。
// 返回空串表示「这个字符串不可能是一个组 ref 形态」。
func groupRefForm(name string) string {
	s := strings.TrimSpace(name)
	if s == "" {
		return ""
	}
	lower := strings.ToLower(s)
	switch {
	case strings.HasPrefix(lower, "group/"):
		// 已经是组 ref 形态：原样（只去空白），连写两层 group/ 只会永远匹配不到。
		rest := strings.TrimSpace(s[len("group/"):])
		if rest == "" {
			return ""
		}
		return "group/" + rest
	case strings.HasPrefix(lower, "auto/"):
		// magpie 给 agent 的 auto 组 ref 是 auto/<model>，而镜像/账本里同一组的 id 是 auto-<slug(model)>。
		rest := strings.TrimSpace(s[len("auto/"):])
		if rest == "" {
			return ""
		}
		return "group/auto-" + rest
	}
	return "group/" + s
}

// groupRefForms 返回一个名字可能对应的全部组 ref 形态（去重、保持顺序）。
// auto/<model> 的 slug 规则由 magpie 定义（小写字母/数字/点/横线），这里只做最保守的近似：
// 原样与「小写并把非 [a-z0-9.-] 换成 -」两种都给出，命中哪个算哪个（多一个候选不会算错账，
// 少一个候选会让这行账结算不出去）。
func groupRefForms(name string) []string {
	out := make([]string, 0, 2)
	seen := map[string]bool{}
	add := func(v string) {
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	add(groupRefForm(name))
	if rest, ok := cutPrefixFold(strings.TrimSpace(name), "auto/"); ok {
		add("group/auto-" + groupSlug(rest))
	}
	return out
}

// groupSlug 近似 magpie 的组 id slug：小写，[a-z0-9.-] 之外的字符替换为 '-'。
func groupSlug(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return b.String()
}

func cutPrefixFold(s, prefix string) (string, bool) {
	if len(s) < len(prefix) || !strings.EqualFold(s[:len(prefix)], prefix) {
		return "", false
	}
	return s[len(prefix):], true
}

// priceCandidates 按解析顺序返回候选（已去重：同一值在两种匹配列下各算一个候选）。
func priceCandidates(reqModel, servedModel string, groups []groupNode, routeID int64) []priceCandidate {
	var plain, binding []priceCandidate
	seen := map[priceCandidate]bool{}
	add := func(list []priceCandidate, c priceCandidate) []priceCandidate {
		c.value = strings.TrimSpace(c.value)
		if c.value == "" || seen[c] {
			return list
		}
		seen[c] = true
		return append(list, c)
	}

	for _, base := range []string{reqModel, servedModel} {
		plain = add(plain, priceCandidate{value: base})
		for _, ref := range groupRefForms(base) {
			binding = add(binding, priceCandidate{value: ref, byBinding: true})
		}
	}
	// 组快照映射：display_name ↔ ref 双向（人可读名无法靠前缀规则推导）。
	for _, g := range groups {
		ref := strings.TrimSpace(g.Ref)
		if ref == "" {
			continue
		}
		for _, name := range []string{reqModel, servedModel} {
			if !groupMatches(g, name) {
				continue
			}
			binding = add(binding, priceCandidate{value: ref, byBinding: true})
			// 历史数据里组 ref 也可能被直接当成对外模型名写进 external_name，两种都试。
			plain = add(plain, priceCandidate{value: ref})
		}
	}

	if routeID > 0 {
		return append(binding, plain...)
	}
	return append(plain, binding...)
}

// groupMatches 判断账本里的模型名是否指向这条组快照（三种写法都算命中）。
func groupMatches(g groupNode, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	ref := strings.TrimSpace(g.Ref)
	if ref == name || strings.TrimPrefix(ref, "group/") == name || groupRefForm(name) == ref {
		return true
	}
	return strings.TrimSpace(g.DisplayName) != "" && strings.EqualFold(strings.TrimSpace(g.DisplayName), name)
}
