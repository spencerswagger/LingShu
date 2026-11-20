// Package mult 是「multipliers 规则引擎」：models.dev v2 目录对齐（四项定案 2026-11-20）
// 中唯一的价格条件因子求值器。零 IO 纯函数，成本层（internal_models.cost_v2）与
// 售价层（external_models.sale_v2）共用同一套语义，Gate 单测主战场。
//
// 语义六条（口径正本 = modelsdev-v2-billing-plan 设计报告 §2.2，逐条钉死）：
//  1. 条件键只认七个：utc_hours / utc_days / date / region / service_tier / context / holiday
//     （v2 词表的中文业务子集）；未知键的规则**整条跳过**（与 v2 "Rules with unknown
//     condition keys are skipped" 一致，保证前向兼容）。
//  2. any/all/not 递归；叶子=一键一操作符（严格一个）；all=与、any=或、not=非，可嵌套；
//     空 when 恒真（禁止，校验拒绝）。
//  3. 时间基准一律 UTC：utc_hours "22:00"=UTC 22 点；date 按 UTC 日。
//  4. factor 组合：所有命中规则的 factor **连乘**；数字乘全部计费段，对象形式只乘点名的
//     模态段（text/audio/...）。
//  5. 求值顺序由调用方钉死（先归一各段单位价 → 再乘命中因子 → Σ(token×段价) ÷ R），
//     本包只负责「给定规则与上下文，返回命中因子」。
//  6. 失败方向：解析失败/越界值 → 该规则按「未命中」处理并记告警，不中断计费；
//     上下文缺值（service_tier 未带、region 未知、节假日表缺国别）→ 含该条件的规则不命中。
//
// 挂载层级已定案：按模型区分，成本挂 internal_models、售价挂 external_models，
// 不开放用户组层。service_tier 键只在成本层的规则里生效（售卖层规则不使用该键，
// 由调用方约束：售卖层求值上下文永远不带 service_tier 值）。
package mult

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

// 条件键（唯一合法集合）。
const (
	KeyUTCHours    = "utc_hours"
	KeyUTCDays     = "utc_days"
	KeyDate        = "date"
	KeyRegion      = "region"
	KeyServiceTier = "service_tier"
	KeyContext     = "context"
	KeyHoliday     = "holiday"
)

// 组合键（非条件键）。
const (
	keyAll = "all"
	keyAny = "any"
	keyNot = "not"
)

// 操作符（叶子严格一个）。
const (
	OpEquals = "equals"
	OpIn     = "in"
	OpGT     = "gt"
	OpGTE    = "gte"
	OpLT     = "lt"
	OpLTE    = "lte"
)

// conditionKeys 合法条件键集合；strOnly 标记只接受 equals/in 的键
// （region/service_tier/holiday 是离散值，大小比较无业务语义）。
var conditionKeys = map[string]bool{
	KeyUTCHours: true, KeyUTCDays: true, KeyDate: true,
	KeyRegion: true, KeyServiceTier: true, KeyContext: true, KeyHoliday: true,
}

var strOnlyKeys = map[string]bool{
	KeyRegion: true, KeyServiceTier: true, KeyHoliday: true,
}

var validOps = map[string]bool{
	OpEquals: true, OpIn: true, OpGT: true, OpGTE: true, OpLT: true, OpLTE: true,
}

// utcDayNames 星期三字码（UTC 日界），index=time.Weekday()。
var utcDayNames = []string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

var utcDayIndex = func() map[string]int {
	m := make(map[string]int, 7)
	for i, n := range utcDayNames {
		m[n] = i
	}
	return m
}()

// ===== 规则（解析后的内存形态）=====

// Cond 单条规则的条件树。
type Cond struct {
	kind condKind
	// 叶子：一键一操作符。
	key string
	op  string
	// 归一后的取值（按键的类型各自解释）：
	strVals  []string // date/region/service_tier/holiday（equals 取 1 个，in 取多个）
	numVals  []int64  // utc_hours（距 0 点分钟）/utc_days（weekday 序）/context（token 数）
	children []*Cond  // all/any/not 的子条件
}

type condKind int

const (
	condLeaf condKind = iota
	condAll
	condAny
	condNot
)

// Factor 因子：数字形式乘全部计费段；对象形式只乘点名的模态段。
type Factor struct {
	number      float64
	perModality map[string]float64
}

// IsNumber 报告是否数字形式（对象形式返回 false）。
func (f Factor) IsNumber() bool { return f.perModality == nil }

// Number 返回数字形式的因子值（对象形式返回 0）。
func (f Factor) Number() float64 { return f.number }

// PerModality 返回对象形式的模态→因子映射（数字形式返回 nil）。
func (f Factor) PerModality() map[string]float64 { return f.perModality }

// Rule 一条 multiplier 规则。ID 为空时求值留痕用 "#序号" 兜底。
type Rule struct {
	ID     string
	Cond   *Cond
	Factor Factor
}

// Rules 规则集合（保持目录里的顺序，求值按序扫描、因子连乘与序无关）。
type Rules []Rule

// Warning 一条被跳过的规则及其原因（调用方记告警日志，不中断计费）。
type Warning struct {
	Rule   string
	Reason string
}

func (w Warning) String() string { return w.Rule + ": " + w.Reason }

// ParseRules 解析 multipliers 数组。非法的规则**逐条丢弃**并返回 Warning
// （失败方向：解析失败按「未命中」处理，绝不让一条坏规则打挂计费）。
// 整个 raw 非法（不是数组）返回 (nil, nil) —— 视为无规则，等价于恒 1 因子。
func ParseRules(raw []byte) (Rules, []Warning) {
	var raws []json.RawMessage
	if err := json.Unmarshal(raw, &raws); err != nil {
		return nil, nil
	}
	var (
		rules Rules
		warns []Warning
	)
	for i, rr := range raws {
		id := fmt.Sprintf("#%d", i)
		rule, reason := parseRule(rr)
		if rule != nil && strings.TrimSpace(rule.ID) != "" {
			id = strings.TrimSpace(rule.ID)
		}
		if reason != "" {
			warns = append(warns, Warning{Rule: id, Reason: reason})
			continue
		}
		if rule.ID == "" {
			rule.ID = id
		}
		rules = append(rules, *rule)
	}
	return rules, warns
}

// rawRule 与目录 JSONB 同形：{"id":..., "when":{...}, "factor":...}。
type rawRule struct {
	ID     string          `json:"id"`
	When   json.RawMessage `json:"when"`
	Factor json.RawMessage `json:"factor"`
}

// MustParseForTest 是测试脚手架：解析必须零告警，否则 panic
// （让测试夹具里的非法规则立刻暴露，而不是静默按未命中处理）。
func MustParseForTest(raw string) Rules {
	rules, warns := ParseRules([]byte(raw))
	if len(warns) != 0 {
		panic("mult.MustParseForTest: 非法规则 " + warns[0].String())
	}
	return rules
}

func parseRule(rr json.RawMessage) (*Rule, string) {
	var r rawRule
	if err := json.Unmarshal(rr, &r); err != nil {
		return nil, "规则不是合法 JSON 对象: " + err.Error()
	}
	if len(r.When) == 0 {
		return nil, "缺 when（空 when 恒真已禁止，请写显式条件）"
	}
	cond, reason := parseCond(r.When)
	if reason != "" {
		return nil, reason
	}
	factor, reason := parseFactor(r.Factor)
	if reason != "" {
		return nil, reason
	}
	return &Rule{ID: strings.TrimSpace(r.ID), Cond: cond, Factor: factor}, ""
}

// parseCond 解析条件树节点。when 对象只允许**恰好一个键**：
// 叶子=一键一操作符，组合用 all/any/not 显式表达（隐式多键与「一键一操作符」冲突，拒绝）。
func parseCond(raw json.RawMessage) (*Cond, string) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, "when 不是合法 JSON 对象: " + err.Error()
	}
	if len(m) == 0 {
		return nil, "空 when 恒真（禁止）：请写显式条件，或用 factor 直接改价"
	}
	if len(m) > 1 {
		return nil, "when 只允许一个键（叶子=一键一操作符；组合请用 all/any/not 显式表达）"
	}
	for k, v := range m {
		switch k {
		case keyAll, keyAny:
			var arr []json.RawMessage
			if err := json.Unmarshal(v, &arr); err != nil || len(arr) == 0 {
				return nil, k + " 必须是非空数组"
			}
			c := &Cond{kind: condAll, key: k}
			if k == keyAny {
				c.kind = condAny
			}
			for _, item := range arr {
				child, reason := parseCond(item)
				if reason != "" {
					return nil, reason
				}
				// 递归展开为扁平子节点列表（求值按 kind 递归本结构即可）。
				c.children = append(c.children, child)
			}
			return c, ""
		case keyNot:
			child, reason := parseCond(v)
			if reason != "" {
				return nil, "not: " + reason
			}
			return &Cond{kind: condNot, children: []*Cond{child}}, ""
		default:
			if !conditionKeys[k] {
				return nil, fmt.Sprintf("未知条件键 %q（整条规则跳过，前向兼容）", k)
			}
			return parseLeaf(k, v)
		}
	}
	return nil, "unreachable"
}

// parseLeaf 解析叶子：值必须是 {操作符: 值} 且恰好一个操作符。
func parseLeaf(key string, raw json.RawMessage) (*Cond, string) {
	var ops map[string]json.RawMessage
	if err := json.Unmarshal(raw, &ops); err != nil {
		return nil, key + " 的条件值不是对象（应为 {操作符: 值}）: " + err.Error()
	}
	if len(ops) == 0 {
		return nil, key + " 缺操作符（空 when 恒真已禁止）"
	}
	if len(ops) > 1 {
		return nil, key + " 只允许一个操作符（一键一操作符）"
	}
	for op, valRaw := range ops {
		if !validOps[op] {
			return nil, fmt.Sprintf("%s 的操作符 %q 不合法（只认 equals/in/gt/gte/lt/lte）", key, op)
		}
		if strOnlyKeys[key] && op != OpEquals && op != OpIn {
			return nil, fmt.Sprintf("%s 是离散值键，只接受 equals/in（得到 %q）", key, op)
		}
		c := &Cond{kind: condLeaf, key: key, op: op}
		switch key {
		case KeyUTCHours:
			vals, reason := parseVals(valRaw, op, parseUTCHour)
			if reason != "" {
				return nil, "utc_hours: " + reason
			}
			c.numVals = vals
		case KeyUTCDays:
			vals, reason := parseVals(valRaw, op, parseUTCDay)
			if reason != "" {
				return nil, "utc_days: " + reason
			}
			c.numVals = vals
		case KeyContext:
			if op == OpIn {
				return nil, "context 是数值键，不接受 in"
			}
			vals, reason := parseVals(valRaw, op, parseContextTokens)
			if reason != "" {
				return nil, "context: " + reason
			}
			c.numVals = vals
		default:
			// date/region/service_tier/holiday：字符串键。
			strs, reason := parseStrVals(valRaw, op, key)
			if reason != "" {
				return nil, key + ": " + reason
			}
			c.strVals = strs
		}
		return c, ""
	}
	return nil, "unreachable"
}

// parseVals 解析数值型条件值：非 in 为单值，in 为数组；全部经 norm 校验归一。
func parseVals(raw json.RawMessage, op string, norm func(json.RawMessage) (int64, string)) ([]int64, string) {
	if op == OpIn {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil || len(arr) == 0 {
			return nil, "in 必须是非空数组"
		}
		out := make([]int64, 0, len(arr))
		for _, item := range arr {
			v, reason := norm(item)
			if reason != "" {
				return nil, reason
			}
			out = append(out, v)
		}
		return out, ""
	}
	v, reason := norm(raw)
	if reason != "" {
		return nil, reason
	}
	return []int64{v}, ""
}

func parseUTCHour(raw json.RawMessage) (int64, string) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, "值必须是 \"HH:MM\" 字符串（UTC）"
	}
	var h, m int
	if n, _ := fmt.Sscanf(s, "%d:%d", &h, &m); n != 2 {
		return 0, fmt.Sprintf("%q 不是 HH:MM 形态", s)
	}
	if len(s) < 4 || !strings.Contains(s, ":") || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, fmt.Sprintf("%q 越界（UTC 小时 00:00–23:59）", s)
	}
	return int64(h*60 + m), ""
}

func parseUTCDay(raw json.RawMessage) (int64, string) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return 0, "值必须是星期三字码（sun/mon/…/sat）"
	}
	idx, ok := utcDayIndex[strings.ToLower(s)]
	if !ok {
		return 0, fmt.Sprintf("%q 不是合法星期（sun/mon/tue/wed/thu/fri/sat）", s)
	}
	return int64(idx), ""
}

func parseContextTokens(raw json.RawMessage) (int64, string) {
	var n json.Number
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, "值必须是整数 token 数"
	}
	i, err := n.Int64()
	if err != nil || i < 0 {
		return 0, fmt.Sprintf("%s 不是非负整数", n.String())
	}
	return i, ""
}

// parseStrVals 解析字符串型条件值（date/region/service_tier/holiday）。
func parseStrVals(raw json.RawMessage, op string, key string) ([]string, string) {
	single := func(r json.RawMessage) (string, string) {
		var s string
		if err := json.Unmarshal(r, &s); err != nil {
			return "", "值必须是字符串"
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return "", key + " 值不能为空"
		}
		switch key {
		case KeyDate:
			if _, err := time.Parse("2006-01-02", s); err != nil {
				return "", fmt.Sprintf("%q 不是 YYYY-MM-DD 形态", s)
			}
		case KeyHoliday:
			up := strings.ToUpper(s)
			if up != "CN" && up != "US" && up != "CA" {
				return "", fmt.Sprintf("%q 不在节假日枚举（CN/US/CA）", s)
			}
			s = up
		}
		return s, ""
	}
	if op == OpIn {
		var arr []json.RawMessage
		if err := json.Unmarshal(raw, &arr); err != nil || len(arr) == 0 {
			return nil, "in 必须是非空数组"
		}
		out := make([]string, 0, len(arr))
		for _, item := range arr {
			s, reason := single(item)
			if reason != "" {
				return nil, reason
			}
			out = append(out, s)
		}
		return out, ""
	}
	s, reason := single(raw)
	if reason != "" {
		return nil, reason
	}
	return []string{s}, ""
}

// parseFactor 解析因子：数字形式（>0，乘全部段）或对象形式（模态→因子，各 >0）。
func parseFactor(raw json.RawMessage) (Factor, string) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return Factor{}, "缺 factor"
	}
	var num json.Number
	if err := json.Unmarshal(raw, &num); err == nil {
		f, ferr := num.Float64()
		if ferr != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
			return Factor{}, fmt.Sprintf("factor 数字必须为正（得到 %s）", trimmed)
		}
		return Factor{number: f}, ""
	}
	var m map[string]json.Number
	if err := json.Unmarshal(raw, &m); err != nil || len(m) == 0 {
		return Factor{}, "factor 必须是正数字或 {模态: 正数字} 对象"
	}
	per := make(map[string]float64, len(m))
	for k, v := range m {
		f, ferr := v.Float64()
		if ferr != nil || math.IsNaN(f) || math.IsInf(f, 0) || f <= 0 {
			return Factor{}, fmt.Sprintf("factor[%s] 必须为正", k)
		}
		per[strings.TrimSpace(k)] = f
	}
	return Factor{perModality: per}, ""
}

// ===== 求值 =====

// Ctx 求值上下文。字段缺值语义（§2.3）：
//   - At：必填，任意时区（内部一律归一 UTC）。
//   - Context：必填（input+cacheRead，与 ContextTierCoeff 同源）。
//   - ServiceTier：空 = 未带 → 含 service_tier 条件的规则不命中（售卖层永远传空：
//     service_tier 差价已定案只在成本层）。
//   - Region：空 = 未知 → 含 region 条件的规则不命中。
//   - Holidays：nil/缺国别 → 含 holiday 条件的规则不命中。
type Ctx struct {
	At          time.Time
	Context     int64
	ServiceTier string
	Region      string
	Holidays    HolidayTable
}

// HolidayTable 静态节假日表（已定案只做静态映射，不做自动地理库）：
// 国别 → 日期集合（"YYYY-MM-DD"，UTC 日）。DefaultHolidays 是内置缺省表。
type HolidayTable map[string]map[string]bool

// Hit 一条命中规则的留痕：数字形式 Global=true 且 Factor 为整体因子；
// 对象形式 Global=false 且 PerModality 为模态→因子。
type Hit struct {
	Rule        string             `json:"rule"`
	Global      bool               `json:"global"`
	Factor      float64            `json:"factor"`
	PerModality map[string]float64 `json:"per_modality,omitempty"`
}

// Result 求值结果：命中明细 + 全局/分模态连乘。
type Result struct {
	Hits   []Hit
	Global float64 // 数字形式因子连乘（无命中 = 1）
	perMod map[string]float64
}

// FactorFor 返回某模态的最终因子 = 全局连乘 × 该模态对象因子连乘。
// 无命中时恒 1（乘法单位元，不改变计费）。
func (r Result) FactorFor(modality string) float64 {
	f := r.Global
	if v, ok := r.perMod[modality]; ok {
		f *= v
	}
	return f
}

// Eval 求值：按规则顺序扫描，命中的因子连乘。纯函数、永不报错
// （坏规则在 ParseRules 阶段已丢弃；求值期的意外一律按「未命中」处理）。
func Eval(rules Rules, ctx Ctx) Result {
	res := Result{Global: 1, perMod: map[string]float64{}}
	at := ctx.At.UTC()
	evalCtx := evalContext{
		minutes:     int64(at.Hour()*60 + at.Minute()),
		day:         int64(at.Weekday()),
		date:        at.Format("2006-01-02"),
		context:     ctx.Context,
		serviceTier: strings.TrimSpace(ctx.ServiceTier),
		region:      strings.TrimSpace(ctx.Region),
		holidays:    ctx.Holidays,
	}
	for _, rule := range rules {
		if !match(rule.Cond, evalCtx) {
			continue
		}
		if rule.Factor.IsNumber() {
			res.Global *= rule.Factor.Number()
			res.Hits = append(res.Hits, Hit{Rule: rule.ID, Global: true, Factor: rule.Factor.Number()})
			continue
		}
		// 对象形式：只乘点名的模态段（未知模态键只留痕、不影响我们计价的 text 段）。
		per := rule.Factor.PerModality()
		for mod, f := range per {
			if cur, ok := res.perMod[mod]; ok {
				res.perMod[mod] = cur * f
			} else {
				res.perMod[mod] = f
			}
		}
		res.Hits = append(res.Hits, Hit{Rule: rule.ID, Global: false, PerModality: per})
	}
	return res
}

// evalContext 归一后的求值输入（全部已 UTC）。
type evalContext struct {
	minutes     int64
	day         int64
	date        string
	context     int64
	serviceTier string
	region      string
	holidays    HolidayTable
}

func match(c *Cond, e evalContext) bool {
	if c == nil {
		return false // 防御：无条件的规则在解析期已被拒，这里永不真
	}
	switch c.kind {
	case condAll:
		for _, child := range c.children {
			if !match(child, e) {
				return false
			}
		}
		return true
	case condAny:
		for _, child := range c.children {
			if match(child, e) {
				return true
			}
		}
		return false
	case condNot:
		return len(c.children) == 1 && !match(c.children[0], e)
	default:
		return matchLeaf(c, e)
	}
}

func matchLeaf(c *Cond, e evalContext) bool {
	switch c.key {
	case KeyUTCHours:
		return cmpInt(c.op, e.minutes, c.numVals)
	case KeyUTCDays:
		return cmpInt(c.op, e.day, c.numVals)
	case KeyDate:
		return cmpStr(c.op, e.date, c.strVals)
	case KeyContext:
		return cmpInt(c.op, e.context, c.numVals)
	case KeyRegion:
		if e.region == "" {
			return false // 未知地区：不命中
		}
		return cmpStr(c.op, e.region, c.strVals)
	case KeyServiceTier:
		if e.serviceTier == "" {
			return false // 请求未带：不命中
		}
		return cmpStr(c.op, e.serviceTier, c.strVals)
	case KeyHoliday:
		if e.holidays == nil {
			return false
		}
		for _, country := range c.strVals {
			if e.holidays[country] != nil && e.holidays[country][e.date] {
				return true
			}
		}
		return false
	}
	return false
}

// cmpInt 数值键的六操作符（in 取多值）。
func cmpInt(op string, actual int64, vals []int64) bool {
	switch op {
	case OpEquals:
		return len(vals) == 1 && actual == vals[0]
	case OpIn:
		for _, v := range vals {
			if actual == v {
				return true
			}
		}
		return false
	case OpGT:
		return len(vals) == 1 && actual > vals[0]
	case OpGTE:
		return len(vals) == 1 && actual >= vals[0]
	case OpLT:
		return len(vals) == 1 && actual < vals[0]
	case OpLTE:
		return len(vals) == 1 && actual <= vals[0]
	}
	return false
}

// cmpStr 字符串键（date 已归一 YYYY-MM-DD，字典序即时间序）。
func cmpStr(op string, actual string, vals []string) bool {
	switch op {
	case OpEquals:
		return len(vals) == 1 && actual == vals[0]
	case OpIn:
		for _, v := range vals {
			if actual == v {
				return true
			}
		}
		return false
	case OpGT:
		return len(vals) == 1 && actual > vals[0]
	case OpGTE:
		return len(vals) == 1 && actual >= vals[0]
	case OpLT:
		return len(vals) == 1 && actual < vals[0]
	case OpLTE:
		return len(vals) == 1 && actual <= vals[0]
	}
	return false
}

// SortedModalities 返回对象形式因子点名的模态键（留痕展示用，稳定排序）。
func SortedModalities(m map[string]float64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
