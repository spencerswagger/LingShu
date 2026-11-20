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
//     这行账的计费口径；绑定真源可能是售价抽屉保存的 auto/<组名> 或镜像的 group/auto-<slug>，
//     两种形态互相归一，见 groupRefForms），再退回按名字匹配；
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
//
// 四种形态必须互相归一（Gate 真机实锤，PR #169：前端售价抽屉保存的绑定是网关形态
// auto/<组名>，而镜像/账本里同一组的 id 是 auto-<slug>；评审 P3 补第三向：网关 auto 组
// 账本行 req_model 是**裸组名**——裸名也要反推抽屉形态与 slug 档，否则无组快照桥时
// 裸名行结算不出、补收入口 400）：
//   - 网关/抽屉形态：auto/<组名>（绑定真源可能就是它本身）；
//   - 镜像形态原样：group/auto-<组名原样>；
//   - 镜像形态 slug：group/auto-<slug(组名)>（小写、非 [a-z0-9.-] 转 -）；
//   - 裸名直接档：group/<组名>（不带头的历史写法）。
//
// slug 规则由 magpie 定义，这里只做最保守的近似，四种都给出：多一个候选不会算错账
// （查询的是运营配置的绑定/名字，命中哪条都是那组自己的售价），少一个候选会让这行账
// 结算不出去（回到 unpriced 堆积，真机事故）。
func groupRefForms(name string) []string {
	s := strings.TrimSpace(name)
	out := make([]string, 0, 4)
	seen := map[string]bool{}
	add := func(v string) {
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	add(groupRefForm(s))
	if rest, ok := cutPrefixFold(s, "auto/"); ok {
		rest = strings.TrimSpace(rest)
		if rest != "" {
			// 网关/抽屉形态本身（绑定真源可能原样存 auto/<组名>）。
			add("auto/" + rest)
			// 镜像 id 的 slug 形态。
			add("group/auto-" + groupSlug(rest))
		}
	} else if id, ok := cutPrefixFold(s, "group/auto-"); ok && id != "" {
		// 镜像形态反推网关形态：group/auto-<id> ⇄ auto/<id>（同一组的两种写法）。
		add("auto/" + id)
		// 历史原样落库的镜像形态（含非 slug 字符）也补 slug 档；真 slug 的 id 会去重掉。
		add("group/auto-" + groupSlug(id))
	} else if strings.HasPrefix(strings.ToLower(s), "group/") {
		// 已是组 ref 形态但非 auto 组（group/<名>）：只按原样，不反推 auto/ ——
		// 「组名叫 X」与「auto 组名叫 X」是两个不同对象，反推会把不相关的账并进来。
	} else if s != "" {
		// 裸名（网关 auto 组账本行 req_model 的形态，评审 P3）：反推抽屉绑定形态与
		// 镜像 slug 档——无组快照桥时裸名行也能按绑定/镜像形态命中售价。
		add("auto/" + s)
		add("group/auto-" + groupSlug(s))
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
