package mult

import (
	"strings"
	"testing"
	"time"
)

func rules(t *testing.T, raw string) (Rules, []Warning) {
	t.Helper()
	r, w := ParseRules([]byte(raw))
	return r, w
}

func mustRules(t *testing.T, raw string) Rules {
	t.Helper()
	r, w := rules(t, raw)
	if len(w) != 0 {
		t.Fatalf("期望零告警，得到 %v", w)
	}
	return r
}

// utcTime 固定一个 UTC 时刻（2026-11-20 周五 22:30 UTC）。
func utcTime() time.Time { return time.Date(2026, 11, 20, 22, 30, 0, 0, time.UTC) }

func TestParse_UnknownKeySkipsWholeRule(t *testing.T) {
	// 未知条件键：整条规则跳过（前向兼容），其余规则照常。
	rs, warns := rules(t, `[
		{"id":"bad","when":{"platform":{"equals":"cn"}},"factor":9},
		{"id":"good","when":{"utc_days":{"equals":"fri"}},"factor":2}
	]`)
	if len(rs) != 1 || rs[0].ID != "good" {
		t.Fatalf("未知键规则应被跳过，剩 1 条 good：len=%d %+v", len(rs), rs)
	}
	if len(warns) != 1 || !strings.Contains(warns[0].Reason, "未知条件键") {
		t.Fatalf("应有一条未知键告警，得到 %v", warns)
	}
	if got := Eval(rs, Ctx{At: utcTime()}).Global; got != 2 {
		t.Fatalf("good 规则应命中 factor=2，得到 %v", got)
	}
}

func TestParse_EmptyWhenRejected(t *testing.T) {
	if rs, warns := rules(t, `[{"id":"x","when":{},"factor":1.5}]`); len(rs) != 0 || len(warns) != 1 {
		t.Fatalf("空 when 必须拒绝：rs=%v warns=%v", rs, warns)
	}
	if _, warns := rules(t, `[{"id":"x","factor":1.5}]`); len(warns) != 1 {
		t.Fatalf("缺 when 同样拒绝：warns=%v", warns)
	}
}

func TestParse_NonArrayShapeWarns(t *testing.T) {
	// multipliers 非数组形状整段忽略——与逐条丢弃同可见性口径，必须告警。
	for _, raw := range []string{`{"a":1}`, `"x"`, `1.5`} {
		rs, warns := ParseRules([]byte(raw))
		if len(rs) != 0 || len(warns) != 1 || !strings.Contains(warns[0].Reason, "数组") {
			t.Fatalf("非数组形状 %q 应整段告警：rs=%d warns=%+v", raw, len(rs), warns)
		}
	}
	// JSON null = 合法「无规则」，不告警。
	if rs, warns := ParseRules([]byte(`null`)); len(rs) != 0 || len(warns) != 0 {
		t.Fatalf("null 应按无规则处理：rs=%d warns=%+v", len(rs), warns)
	}
}

func TestParse_LeafOneKeyOneOperator(t *testing.T) {
	// when 多键 → 拒绝（组合必须显式 all/any）。
	if _, warns := rules(t, `[{"when":{"utc_days":{"equals":"fri"},"region":{"equals":"CN"}},"factor":1}]`); len(warns) != 1 {
		t.Fatalf("多键 when 必须拒绝")
	}
	// 单键多操作符 → 拒绝。
	if _, warns := rules(t, `[{"when":{"utc_hours":{"gte":"22:00","lt":"02:00"}},"factor":1}]`); len(warns) != 1 {
		t.Fatalf("多操作符必须拒绝")
	}
	// 非法操作符 → 拒绝。
	if _, warns := rules(t, `[{"when":{"utc_hours":{"between":["22:00","02:00"]}},"factor":1}]`); len(warns) != 1 {
		t.Fatalf("非法操作符必须拒绝")
	}
	// 离散值键不接受大小比较 → 拒绝。
	if _, warns := rules(t, `[{"when":{"region":{"gt":"CN"}},"factor":1}]`); len(warns) != 1 {
		t.Fatalf("region 只接受 equals/in")
	}
}

func TestParse_BadValuesRejected(t *testing.T) {
	bad := []string{
		`[{"when":{"utc_hours":{"equals":"25:00"}},"factor":1}]`,
		`[{"when":{"utc_hours":{"equals":"9:5"}},"factor":1}]`,
		`[{"when":{"utc_hours":{"equals":"12:30:45"}},"factor":1}]`,
		`[{"when":{"utc_days":{"equals":"funday"}},"factor":1}]`,
		`[{"when":{"date":{"equals":"2026-13-01"}},"factor":1}]`,
		`[{"when":{"context":{"gt":-1}},"factor":1}]`,
		`[{"when":{"holiday":{"equals":"FR"}},"factor":1}]`,
		`[{"when":{"context":{"in":[1000,2000]}},"factor":1}]`,
		`[{"when":{"utc_days":{"equals":"fri"}},"factor":0}]`,
		`[{"when":{"utc_days":{"equals":"fri"}},"factor":-1.5}]`,
		`[{"when":{"utc_days":{"equals":"fri"}},"factor":{}}]`,
		`[{"when":{"utc_days":{"equals":"fri"}},"factor":{"text":-1}}]`,
		`[{"when":{"utc_days":{"equals":"fri"}}}]`,
		`[{"when":{"all":[]},"factor":1}]`,
		`[{"when":{"not":{"utc_days":{"equals":"fri"},"region":{"equals":"CN"}}},"factor":1}]`,
	}
	for i, raw := range bad {
		if rs, warns := rules(t, raw); len(rs) != 0 || len(warns) != 1 {
			t.Fatalf("第 %d 条应整条拒绝：rs=%v warns=%v", i, rs, warns)
		}
	}
}

func TestParse_NotArrayMeansNoRules(t *testing.T) {
	// 整体不是数组（如手滑存了对象）→ 视为无规则，等价恒 1 因子；与逐条丢弃同
	// 可见性口径，必须带一条形状告警（评审 P3：静默忽略 = 规则漂移不可见）。
	if rs, warns := rules(t, `{"multipliers":[]}`); rs != nil || len(warns) != 1 {
		t.Fatalf("非数组应返回 (nil,1 条形状告警)，得到 %v %v", rs, warns)
	}
	if rs, _ := rules(t, `[]`); len(rs) != 0 {
		t.Fatalf("空数组 = 无规则")
	}
}

func TestEval_AllAnyNotNesting(t *testing.T) {
	rs := mustRules(t, `[
		{"id":"combo","when":{"all":[
			{"utc_hours":{"gte":"22:00"}},
			{"any":[
				{"utc_days":{"in":["fri","sat"]}},
				{"not":{"holiday":{"equals":"CN"}}}
			]}
		]},"factor":1.5}
	]`)
	// 2026-11-20 周五 22:30 UTC：gte 22:00 ✓，any(fri ✓) → 命中。
	if got := Eval(rs, Ctx{At: utcTime(), Holidays: HolidayTable{"CN": {"2026-11-20": true}}}).Global; got != 1.5 {
		t.Fatalf("all+any 嵌套应命中 1.5，得到 %v", got)
	}
	// utc_hours 不满足 → all 失败。
	at := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if got := Eval(rs, Ctx{At: at}).Global; got != 1 {
		t.Fatalf("时段外不应命中，得到 %v", got)
	}
	// fri 不满足且 holiday 命中 → any 两支都假 → 不命中。
	at = time.Date(2026, 11, 23, 23, 0, 0, 0, time.UTC) // 周一
	if got := Eval(rs, Ctx{At: at, Holidays: HolidayTable{"CN": {"2026-11-23": true}}}).Global; got != 1 {
		t.Fatalf("not+holiday 应为假，得到 %v", got)
	}
	// fri 不满足但 holiday 表缺国别 → not 分支为真（holiday 条件不命中→not 真）→ 命中。
	if got := Eval(rs, Ctx{At: at, Holidays: nil}).Global; got != 1.5 {
		t.Fatalf("not 对未配国别应取非（命中），得到 %v", got)
	}
}

func TestEval_UTCBoundary(t *testing.T) {
	rs := mustRules(t, `[{"id":"night","when":{"utc_hours":{"gte":"22:00"}},"factor":0.5}]`)
	cases := []struct {
		at   time.Time
		want float64
	}{
		{time.Date(2026, 11, 20, 21, 59, 0, 0, time.UTC), 1},
		{time.Date(2026, 11, 20, 22, 0, 0, 0, time.UTC), 0.5},
		{time.Date(2026, 11, 20, 23, 59, 0, 0, time.UTC), 0.5},
		{time.Date(2026, 11, 21, 0, 0, 0, 0, time.UTC), 1},
	}
	for i, c := range cases {
		if got := Eval(rs, Ctx{At: c.at}).Global; got != c.want {
			t.Fatalf("UTC 边界 %d：得 %v 期望 %v", i, got, c.want)
		}
	}
	// 时间基准一律 UTC：北京时间 06:30（= UTC 22:30）必须命中。
	bj := time.Date(2026, 11, 21, 6, 30, 0, 0, time.FixedZone("CST", 8*3600))
	if got := Eval(rs, Ctx{At: bj}).Global; got != 0.5 {
		t.Fatalf("本地时区入参必须按 UTC 判定：得到 %v", got)
	}
}

func TestEval_CrossMidnightWindow(t *testing.T) {
	// 跨午夜窗口 22:00–02:00 用 any 两个半窗表达（报告 §2.2 语义钉死这条组合方式）。
	rs := mustRules(t, `[{"id":"night","when":{"any":[
		{"utc_hours":{"gte":"22:00"}},{"utc_hours":{"lt":"02:00"}}
	]},"factor":0.5}]`)
	cases := []struct {
		at   time.Time
		want float64
	}{
		{time.Date(2026, 11, 20, 23, 30, 0, 0, time.UTC), 0.5},
		{time.Date(2026, 11, 21, 1, 59, 0, 0, time.UTC), 0.5},
		{time.Date(2026, 11, 21, 2, 0, 0, 0, time.UTC), 1},
		{time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC), 1},
	}
	for i, c := range cases {
		if got := Eval(rs, Ctx{At: c.at}).Global; got != c.want {
			t.Fatalf("跨午夜 %d：得 %v 期望 %v", i, got, c.want)
		}
	}
}

func TestEval_FactorObjectAndConsecutiveProduct(t *testing.T) {
	rs := mustRules(t, `[
		{"id":"off","when":{"utc_hours":{"gte":"22:00"}},"factor":0.5},
		{"id":"txt","when":{"utc_days":{"equals":"fri"}},"factor":{"text":1.5}},
		{"id":"img","when":{"utc_days":{"equals":"fri"}},"factor":{"image":2}}
	]`)
	res := Eval(rs, Ctx{At: utcTime()})
	if res.Global != 0.5 {
		t.Fatalf("数字因子连乘错误：%v", res.Global)
	}
	if got := res.FactorFor("text"); got != 0.75 {
		t.Fatalf("text = 0.5×1.5，得到 %v", got)
	}
	if got := res.FactorFor("image"); got != 1.0 {
		t.Fatalf("image = 0.5×2，得到 %v", got)
	}
	if got := res.FactorFor("audio"); got != 0.5 {
		t.Fatalf("未点名模态只受全局因子：得到 %v", got)
	}
	if len(res.Hits) != 3 {
		t.Fatalf("命中留痕应 3 条：%+v", res.Hits)
	}
	if res.Hits[1].Global || res.Hits[1].PerModality["text"] != 1.5 {
		t.Fatalf("对象形式留痕错误：%+v", res.Hits[1])
	}
}

func TestEval_ServiceTierOnlyWhenProvided(t *testing.T) {
	rs := mustRules(t, `[{"id":"flex","when":{"service_tier":{"equals":"flex"}},"factor":0.5}]`)
	// 售卖层口径：上下文永不带 service_tier → 不命中。
	if got := Eval(rs, Ctx{At: utcTime()}).Global; got != 1 {
		t.Fatalf("未带 service_tier 不得命中，得到 %v", got)
	}
	// 成本层口径：按账本归因取值 → 命中。
	if got := Eval(rs, Ctx{At: utcTime(), ServiceTier: "flex"}).Global; got != 0.5 {
		t.Fatalf("flex 应命中 0.5，得到 %v", got)
	}
	if got := Eval(rs, Ctx{At: utcTime(), ServiceTier: "priority"}).Global; got != 1 {
		t.Fatalf("priority 不等于 flex 不得命中，得到 %v", got)
	}
}

func TestEval_RegionAndContext(t *testing.T) {
	rs := mustRules(t, `[
		{"id":"cn","when":{"region":{"in":["CN","HK"]}},"factor":0.9},
		{"id":"big","when":{"context":{"gt":128000}},"factor":1.2}
	]`)
	// region 未知 → 不命中；context 命中。
	res := Eval(rs, Ctx{At: utcTime(), Context: 200000})
	if res.Global != 1.2 {
		t.Fatalf("region 未知应不命中，context 应命中：%v", res.Global)
	}
	res = Eval(rs, Ctx{At: utcTime(), Region: "CN", Context: 1000})
	if res.Global != 0.9 {
		t.Fatalf("CN 应命中 0.9：%v", res.Global)
	}
	res = Eval(rs, Ctx{At: utcTime(), Region: "CN", Context: 128001})
	if res.Global != 0.9*1.2 {
		t.Fatalf("两规则应连乘 %v：%v", 0.9*1.2, res.Global)
	}
	res = Eval(rs, Ctx{At: utcTime(), Region: "US", Context: 128001})
	if res.Global != 1.2 {
		t.Fatalf("US 不在 [CN,HK]，只 context 命中：%v", res.Global)
	}
}

func TestEval_HolidayTable(t *testing.T) {
	rs := mustRules(t, `[{"id":"us-thx","when":{"holiday":{"equals":"US"}},"factor":0.8}]`)
	table := HolidayTable{"US": {"2026-11-26": true, "2026-12-25": true}}
	// 感恩节命中。
	at := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	if got := Eval(rs, Ctx{At: at, Holidays: table}).Global; got != 0.8 {
		t.Fatalf("US 节假日应命中：%v", got)
	}
	// 普通日不命中。
	at = time.Date(2026, 11, 27, 10, 0, 0, 0, time.UTC)
	if got := Eval(rs, Ctx{At: at, Holidays: table}).Global; got != 1 {
		t.Fatalf("非节假日不得命中：%v", got)
	}
	// 同日是 CN 表的节日但规则点名 US → 只看 US 表。
	cnTable := HolidayTable{"CN": {"2026-11-26": true}}
	if got := Eval(rs, Ctx{At: at, Holidays: cnTable}).Global; got != 1 {
		t.Fatalf("CN 表不应影响 US 规则：%v", got)
	}
}

func TestEval_DateOps(t *testing.T) {
	rs := mustRules(t, `[{"id":"promo","when":{"date":{"gte":"2026-12-01"}},"factor":0.7}]`)
	if got := Eval(rs, Ctx{At: time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)}).Global; got != 0.7 {
		t.Fatalf("边界日应命中（含端点）：%v", got)
	}
	if got := Eval(rs, Ctx{At: time.Date(2026, 11, 30, 23, 59, 59, 0, time.UTC)}).Global; got != 1 {
		t.Fatalf("前一天不得命中：%v", got)
	}
	// date 按 UTC 日：北京时间 12-01 07:00 = UTC 11-30 23:00 → 不命中。
	bj := time.Date(2026, 12, 1, 7, 0, 0, 0, time.FixedZone("CST", 8*3600))
	if got := Eval(rs, Ctx{At: bj}).Global; got != 1 {
		t.Fatalf("date 必须按 UTC 日判定：%v", got)
	}
}

func TestEval_UTCDaysWeekend(t *testing.T) {
	rs := mustRules(t, `[{"id":"wknd","when":{"utc_days":{"in":["sat","sun"]}},"factor":1.1}]`)
	sat := time.Date(2026, 11, 21, 3, 0, 0, 0, time.UTC) // 周六
	if got := Eval(rs, Ctx{At: sat}).Global; got != 1.1 {
		t.Fatalf("周六应命中：%v", got)
	}
	fri := time.Date(2026, 11, 20, 23, 0, 0, 0, time.UTC)
	if got := Eval(rs, Ctx{At: fri}).Global; got != 1 {
		t.Fatalf("周五不得命中：%v", got)
	}
}

func TestEval_NoRulesFactorOne(t *testing.T) {
	if got := Eval(nil, Ctx{At: utcTime()}).Global; got != 1 {
		t.Fatalf("无规则恒 1：%v", got)
	}
	res := Eval(nil, Ctx{At: utcTime()})
	if got := res.FactorFor("text"); got != 1 {
		t.Fatalf("无规则 FactorFor 恒 1：%v", got)
	}
}

func TestParse_RoundTripShape(t *testing.T) {
	// 与目录 JSONB 同形的端到端样例（含 id 留痕、对象 factor）。
	// 跨午夜窗口必须用 any 两个半窗表达（all 是与，22:00∧<06:00 恒假）。
	raw := `[{"id":"night-utc","when":{"any":[{"utc_hours":{"gte":"22:00"}},{"utc_hours":{"lt":"06:00"}}]},"factor":{"text":0.8}},{"id":"cn-hol","when":{"holiday":{"in":["CN"]}},"factor":0.95}]`
	rs, warns := rules(t, raw)
	if len(warns) != 0 || len(rs) != 2 {
		t.Fatalf("样例应全部合法：warns=%v rs=%d", warns, len(rs))
	}
	res := Eval(rs, Ctx{At: utcTime()})
	if got := res.FactorFor("text"); got != 0.8 {
		t.Fatalf("22:30 落在夜窗，应命中 text 0.8：%v", got)
	}
	if len(res.Hits) != 1 || res.Hits[0].Rule != "night-utc" {
		t.Fatalf("留痕 rule id 应透传：%+v", res.Hits)
	}
}
