package router

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/model"
)

// fakeCMSource 满足 channelModelSource。
type fakeCMSource struct {
	cms []channel.ChannelModel
}

func (f *fakeCMSource) ListByExternal(_ context.Context, _ int64) ([]channel.ChannelModel, error) {
	return f.cms, nil
}

// extRow 构造 external_models 查询返回行。
func extRow(id int64, name string) *sqlmock.Rows {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return sqlmock.NewRows([]string{"id", "external_name", "description", "enabled", "sale_rates", "time_config", "context_tiers", "created_at", "updated_at"}).
		AddRow(id, name, "", true,
			`{"input":1.0,"output":2.0,"cache_read":0.1}`,
			nil, nil, now, now)
}

// newModelService 构造可用的 model.Service（expectTimes 次外部查询）。
func newModelService(t *testing.T, times int) (*model.Service, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	query := `SELECT id, external_name, description, enabled, sale_rates, time_config, context_tiers, created_at, updated_at FROM external_models WHERE external_name = $1 AND enabled = true AND deleted_at IS NULL LIMIT 1`
	for i := 0; i < times; i++ {
		mock.ExpectQuery(regexp.QuoteMeta(query)).
			WithArgs("qw-max").
			WillReturnRows(extRow(10, "qw-max"))
	}
	return model.NewService(model.NewStore(db)), mock
}

// newManager 构造渠道运行时 + 模型运行时。
func newManager(t *testing.T, channels []*channel.Channel, models []channel.ChannelModel) *channel.Manager {
	t.Helper()
	mgr := channel.NewManager(nil, nil, make([]byte, 16), nil, nil, nil)
	for i := range channels {
		mgr.Upsert(channels[i])
	}
	for i := range models {
		mgr.UpsertModel(&models[i])
	}
	return mgr
}

// newKey 构造一条可直接 UpsertKey 的密钥（路由只用 Machine 状态与 keyID，凭据留空即可）。
func newKey(id, chID int64, st channel.State) channel.ChannelKey {
	return channel.ChannelKey{
		ID: id, ChannelID: chID, Name: fmt.Sprintf("k%d", id),
		State: st,
	}
}

// newManagerWithKeys 构造渠道 + 模型 + 密钥运行时（B3 起候选以密钥为运行时实体）。
func newManagerWithKeys(t *testing.T, channels []*channel.Channel, models []channel.ChannelModel, keys []channel.ChannelKey) *channel.Manager {
	t.Helper()
	mgr := newManager(t, channels, models)
	for i := range keys {
		mgr.UpsertKey(keys[i])
	}
	return mgr
}

func healthyCh(id int64, name string, tags map[string]string, priority int) *channel.Channel {
	return &channel.Channel{
		ID: id, Name: name, Protocol: channel.ProtocolOpenAICompat,
		BaseURL: "https://x.example.com", Tags: tags, Priority: priority,
	}
}

func cm(chID int64, internal string) channel.ChannelModel {
	return channel.ChannelModel{
		ID: chID*100 + 1, ChannelID: chID, InternalModelID: internal,
		ExternalModelID: 10, State: channel.StateNormal,
		CostRates: billing.Rates{"input": 1, "output": 2, "cache_read": 0.1},
	}
}

// cmCost 构造带指定 input 单价的候选（其余段为 0）：样值下成本标量 = 10000 × inputRate，
// 随 inputRate 单调，便于构造「谁更便宜」的确定性场景。inputRate=0 时三段全 0 = 成本未知。
func cmCost(chID int64, internal string, inputRate float64) channel.ChannelModel {
	m := cm(chID, internal)
	m.CostRates = billing.Rates{"input": inputRate, "output": 0, "cache_read": 0}
	return m
}

// fakeCostConfig 满足 CostConfig（测试注入样值与全局时段）。
type fakeCostConfig struct {
	sample   billing.Usage
	globalTC *billing.TimeCoeffConfig
}

func (f *fakeCostConfig) SampleUsage() (billing.Usage, error) {
	if f.sample == (billing.Usage{}) {
		return defaultCostSample, nil // 未显式设置时用内置默认，便于只关心时段回落的用例
	}
	return f.sample, nil
}

// TimeConfig 返回全局时段配置；未设置时返回错误（模拟配置缺失），
// 使路由层回落时段系数 1（避免返回零值 TimeCoeffConfig 时 CoeffFor 得到 Default=0 而把成本全部清零）。
func (f *fakeCostConfig) TimeConfig() (billing.TimeCoeffConfig, error) {
	if f.globalTC == nil {
		return billing.TimeCoeffConfig{}, errors.New("未配置全局时段")
	}
	return *f.globalTC, nil
}

// attachTo 在会话注册表上直接建立一条绑定 (keyID, internal) 的存活会话（用于构造存活会话数场景）。
func attachTo(t *testing.T, r *SessionRegistry, id string, keyID int64, internal string) {
	t.Helper()
	ok, _ := r.Attach(&Session{
		SessionID: id, ChannelKeyID: keyID, InternalModelID: internal,
		ExpireAt: time.Now().Add(time.Hour),
	}, 10)
	if !ok {
		t.Fatalf("attach %s 应成功", id)
	}
}

func TestRoute_WithTags_SelectsMatching(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	mgr := newManagerWithKeys(t, []*channel.Channel{
		healthyCh(1, "a", nil, 100),
		healthyCh(2, "b", nil, 100),
	}, []channel.ChannelModel{cm(1, "cn-internal"), cm(2, "us-internal")},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})
	mgr.SetBoundTags(1, []channel.TagRef{{ID: 1, KV: map[string]string{"region": "cn"}}})
	mgr.SetBoundTags(2, []channel.TagRef{{ID: 2, KV: map[string]string{"region": "us", "tier": "gold"}}})
	cmSrc := &fakeCMSource{cms: []channel.ChannelModel{cm(1, "cn-internal"), cm(2, "us-internal")}}
	e := NewEngine(svc, cmSrc, mgr, nil)

	r, err := e.Route(context.Background(), RouteSpec{
		ExternalModel: "qw-max",
		TagKV:         map[string]string{"region": "cn"},
	})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 1 {
		t.Fatalf("期望选中匹配渠道 1，got %d", r.ChannelID)
	}
	if r.InternalModelID != "cn-internal" {
		t.Fatalf("internal model 未改写：%s", r.InternalModelID)
	}
}

func TestRoute_WithTags_NoMatch(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	mgr := newManagerWithKeys(t, []*channel.Channel{
		healthyCh(1, "a", nil, 100),
	}, []channel.ChannelModel{cm(1, "cn-internal")},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal)})
	mgr.SetBoundTags(1, []channel.TagRef{{ID: 1, KV: map[string]string{"region": "cn"}}})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{cm(1, "cn-internal")}}, mgr, nil)

	_, err := e.Route(context.Background(), RouteSpec{
		ExternalModel: "qw-max",
		TagKV:         map[string]string{"region": "us"},
	})
	re, ok := AsRouteError(err)
	if !ok || re.Kind != KindNoRoute {
		t.Fatalf("期望 KindNoRoute，got %v", err)
	}
}

// AC-1：同优先级内选成本最低者。
func TestRoute_SamePriority_LowestCostSelected(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	cheap := cmCost(1, "int1", 1) // 成本 1000
	dear := cmCost(2, "int2", 3)  // 成本 3000
	mgr := newManagerWithKeys(t, []*channel.Channel{
		healthyCh(1, "a", nil, 100),
		healthyCh(2, "b", nil, 100),
	}, []channel.ChannelModel{cheap, dear},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{cheap, dear}}, mgr, nil)

	r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 1 {
		t.Fatalf("同优先级内应选成本最低的渠道 1，got %d", r.ChannelID)
	}
}

// AC-2：priority 为硬分层——高优先级候选即便更贵也优先。
func TestRoute_PriorityLayerBeatsLowerCost(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	dearHighPrio := cmCost(1, "int1", 10) // 高优先级但更贵
	cheapLowPrio := cmCost(2, "int2", 1)  // 低优先级但更便宜
	mgr := newManagerWithKeys(t, []*channel.Channel{
		healthyCh(1, "a", nil, 100),
		healthyCh(2, "b", nil, 50),
	}, []channel.ChannelModel{dearHighPrio, cheapLowPrio},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{dearHighPrio, cheapLowPrio}}, mgr, nil)

	r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 1 {
		t.Fatalf("高优先级层应优先（即便更贵），期望渠道 1，got %d", r.ChannelID)
	}
}

// AC-3：时段系数参与成本比较并可翻转选择。
func TestRoute_TimeCoeffFlipsChoice(t *testing.T) {
	svc, mock := newModelService(t, 2)
	defer func() { _ = mock.ExpectationsWereMet() }()

	// 基准：渠道 1 更便宜（成本 2000 vs 10000）。
	m1 := cmCost(1, "int1", 2)
	m2 := cmCost(2, "int2", 10)
	// 渠道 2 配模型级时段系数 0.1：当前成本 = 10000 × 0.1 = 1000 < 2000 → 翻转选中渠道 2。
	tc := &billing.TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 0.1}
	m2.TimeConfig = tc

	mgr := newManagerWithKeys(t, []*channel.Channel{
		healthyCh(1, "a", nil, 100),
		healthyCh(2, "b", nil, 100),
	}, []channel.ChannelModel{m1, m2},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{m1, m2}}, mgr, nil)
	e.SetClock(func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) })

	r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 2 {
		t.Fatalf("时段系数应使渠道 2 更便宜并被选中，got %d", r.ChannelID)
	}
}

// AC-4：成本并列时，存活会话数更少者优先，且随会话分布自平衡。
func TestRoute_CostTie_BreaksByActiveSessions(t *testing.T) {
	svc, mock := newModelService(t, 2)
	defer func() { _ = mock.ExpectationsWereMet() }()

	// 两候选成本相同（同 inputRate）。
	m1 := cmCost(1, "int1", 1)
	m2 := cmCost(2, "int2", 1)
	mgr := newManagerWithKeys(t, []*channel.Channel{
		healthyCh(1, "a", nil, 100),
		healthyCh(2, "b", nil, 100),
	}, []channel.ChannelModel{m1, m2},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})
	sess := NewSessionRegistry(nil)
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{m1, m2}}, mgr, sess)

	// 渠道 1 已有 1 条会话 → 应优先渠道 2（存活 0）。
	attachTo(t, sess, "s1", 11, "int1")
	r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 2 {
		t.Fatalf("并列时应选存活会话更少的渠道 2，got %d", r.ChannelID)
	}

	// 渠道 2 也建一条会话 → 两者存活数相同 → 按稳定 id 次序回到渠道 1。
	attachTo(t, sess, "s2", 21, "int2")
	r2, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r2.ChannelID != 1 {
		t.Fatalf("存活数再次并列时按稳定 id 应选渠道 1，got %d", r2.ChannelID)
	}
}

// AC-5：同优先级、同成本、同存活会话数时按稳定 id 次序，且多次路由结果确定（无随机；权重已删除）。
func TestRoute_TieBreaksByStableIDDeterministic(t *testing.T) {
	const calls = 5
	svc, mock := newModelService(t, calls)
	defer func() { _ = mock.ExpectationsWereMet() }()

	ch1 := healthyCh(1, "a", nil, 100)
	ch2 := healthyCh(2, "b", nil, 100)
	m1 := cmCost(1, "int1", 1)
	m2 := cmCost(2, "int2", 1)
	mgr := newManagerWithKeys(t, []*channel.Channel{ch1, ch2},
		[]channel.ChannelModel{m1, m2},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{m1, m2}}, mgr, nil)

	for i := 0; i < calls; i++ {
		r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
		if err != nil {
			t.Fatalf("route #%d: %v", i, err)
		}
		if r.ChannelID != 1 {
			t.Fatalf("并列时应稳定命中最小的渠道 1，got %d（第 %d 次）", r.ChannelID, i)
		}
	}
}

// AC-6：缺失成本（空/全 0）视为最贵；全缺时按 priority/存活/id 兜底。
func TestRoute_MissingCostTreatedAsMostExpensive(t *testing.T) {
	t.Run("部分缺失", func(t *testing.T) {
		svc, mock := newModelService(t, 1)
		defer func() { _ = mock.ExpectationsWereMet() }()

		known := cmCost(1, "int1", 5) // 有有效成本
		unknown := cmCost(2, "int2", 0)
		unknown.CostRates = nil // 缺价
		mgr := newManagerWithKeys(t, []*channel.Channel{
			healthyCh(1, "a", nil, 100),
			healthyCh(2, "b", nil, 100),
		}, []channel.ChannelModel{known, unknown},
			[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})
		e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{known, unknown}}, mgr, nil)

		r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
		if err != nil {
			t.Fatalf("route: %v", err)
		}
		if r.ChannelID != 1 {
			t.Fatalf("有有效成本者应优先，期望渠道 1，got %d", r.ChannelID)
		}
	})

	t.Run("全部缺失按优先级兜底", func(t *testing.T) {
		svc, mock := newModelService(t, 1)
		defer func() { _ = mock.ExpectationsWereMet() }()

		a := cmCost(1, "int1", 0)
		a.CostRates = nil
		b := cmCost(2, "int2", 0)
		b.CostRates = nil
		mgr := newManagerWithKeys(t, []*channel.Channel{
			healthyCh(1, "a", nil, 50),
			healthyCh(2, "b", nil, 100), // 更高优先级
		}, []channel.ChannelModel{a, b},
			[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})
		e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{a, b}}, mgr, nil)

		r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
		if err != nil {
			t.Fatalf("全部缺价不应报错: %v", err)
		}
		if r.ChannelID != 2 {
			t.Fatalf("全部缺价时按 priority 兜底，期望渠道 2，got %d", r.ChannelID)
		}
	})
}

// AC-8：RouteAll 完整序列按成本升序（同优先级内），首位最便宜。
func TestRouteAll_OrderedByCostAscending(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	dear := cmCost(1, "int1", 3)
	cheap := cmCost(2, "int2", 1)
	mgr := newManagerWithKeys(t, []*channel.Channel{
		healthyCh(1, "a", nil, 100),
		healthyCh(2, "b", nil, 100),
	}, []channel.ChannelModel{dear, cheap},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{dear, cheap}}, mgr, nil)

	routes, err := e.RouteAll(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("routeall: %v", err)
	}
	if len(routes) != 2 || routes[0].ChannelID != 2 || routes[1].ChannelID != 1 {
		t.Fatalf("failover 序列应按成本升序（渠道 2 更便宜在前），got %+v", routes)
	}
}

// AC-9/TR-5.2：注入的全局样值确实参与成本比较——同一候选集、仅样值不同，首选渠道发生翻转。
// 该用例对「样值注入被忽略」有鉴别力：若始终回落内置默认，两次都会命中同一渠道（渠道 1）。
func TestRoute_SampleUsageChangesChoice(t *testing.T) {
	svc, mock := newModelService(t, 2)
	defer func() { _ = mock.ExpectationsWereMet() }()

	// 渠道 1 只按 output 计价；渠道 2 只按 input（样值 input=10000）计价。
	m1 := cm(1, "int1")
	m1.CostRates = billing.Rates{"input": 0, "output": 1, "cache_read": 0}
	m2 := cm(2, "int2")
	m2.CostRates = billing.Rates{"input": 1, "output": 0, "cache_read": 0}
	mgr := newManagerWithKeys(t, []*channel.Channel{
		healthyCh(1, "a", nil, 100),
		healthyCh(2, "b", nil, 100),
	}, []channel.ChannelModel{m1, m2},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{m1, m2}}, mgr, nil)

	// 样值 A：输出极小、输入极大 → 渠道 1（output 计价）成本 1，渠道 2（input 计价）成本 100000 → 首选渠道 1。
	e.SetCostConfig(&fakeCostConfig{sample: billing.Usage{Input: 100000, Output: 1}})
	rA, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("route A: %v", err)
	}
	if rA.ChannelID != 1 {
		t.Fatalf("样值 A 应首选渠道 1，got %d", rA.ChannelID)
	}

	// 样值 B：输出极大、输入极小 → 渠道 2 成本 1，渠道 1 成本 100000 → 翻转首选渠道 2。
	e.SetCostConfig(&fakeCostConfig{sample: billing.Usage{Input: 1, Output: 100000}})
	rB, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("route B: %v", err)
	}
	if rB.ChannelID != 2 {
		t.Fatalf("样值 B 应翻转首选渠道 2，got %d", rB.ChannelID)
	}
}

// AC-3/TR-1.1：模型级 time_config 为空时回落全局 billing.time_config 系数。
func TestRoute_GlobalTimeCoeffFallback(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	a := cmCost(1, "int1", 2) // 自带模型级时段系数 1 → 成本 2000
	a.TimeConfig = &billing.TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 1}
	b := cmCost(2, "int2", 10) // 无模型级时段 → 回落全局 0.1 → 成本 1000
	mgr := newManagerWithKeys(t, []*channel.Channel{
		healthyCh(1, "a", nil, 100),
		healthyCh(2, "b", nil, 100),
	}, []channel.ChannelModel{a, b},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{a, b}}, mgr, nil)
	e.SetClock(func() time.Time { return time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC) })
	e.SetCostConfig(&fakeCostConfig{globalTC: &billing.TimeCoeffConfig{Timezone: "Asia/Shanghai", Default: 0.1}})

	r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 2 {
		t.Fatalf("无模型级时段的候选应回落全局系数 0.1 而更便宜，期望渠道 2，got %d", r.ChannelID)
	}
}

// AC-7：会话命中（Attach 后）应覆盖成本排序：即便按成本会选其他候选，也返回命中渠道。
func TestRoute_SessionHit_OverridesCost(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	mgr := newManagerWithKeys(t, []*channel.Channel{
		healthyCh(1, "a", nil, 1),
		healthyCh(2, "b", nil, 100),
	}, []channel.ChannelModel{cm(1, "int1"), cm(2, "int2")},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{cm(1, "int1"), cm(2, "int2")}}, mgr, NewSessionRegistry(nil))

	key := "t1|qw-max|sess"
	if ok, created := e.Sessions().Attach(&Session{
		SessionID: key, ChannelKeyID: 11, InternalModelID: "int1",
		ExpireAt: time.Now().Add(time.Hour),
	}, 10); !ok || !created {
		t.Fatalf("attach failed ok=%v created=%v", ok, created)
	}

	r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max", SessionKey: key})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 1 {
		t.Fatalf("会话命中应返回渠道 1，got %d", r.ChannelID)
	}
}

// 同渠道多个内部模型绑定同一对外模型：每行都是独立候选（不按渠道归并取首行）。
func TestRoute_SameChannelMultiInternal_AllCandidate(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	flash := cm(1, "flash-internal")
	flash.ID = 101
	v4 := cm(1, "v4-internal")
	v4.ID = 102
	mgr := newManagerWithKeys(t, []*channel.Channel{healthyCh(1, "a", nil, 100)},
		[]channel.ChannelModel{flash, v4},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{flash, v4}}, mgr, NewSessionRegistry(nil))

	routes, err := e.RouteAll(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("routeAll: %v", err)
	}
	if len(routes) != 2 {
		t.Fatalf("两行内部模型都应候选，got %d 条: %+v", len(routes), routes)
	}
	seen := map[string]bool{}
	for _, r := range routes {
		seen[r.InternalModelID] = true
	}
	if !seen["flash-internal"] || !seen["v4-internal"] {
		t.Fatalf("期望两行内部模型都是候选，got %v", seen)
	}
}

// 同渠道某内部模型行禁用：只跳过该行，同渠道其他内部模型行仍候选（不会渠道整体 503）。
func TestRoute_SameChannel_DisabledInternal_SkipsOnlyThatRow(t *testing.T) {
	svc, mock := newModelService(t, 2)
	defer func() { _ = mock.ExpectationsWereMet() }()

	flash := cm(1, "flash-internal")
	flash.ID = 101
	v4 := cm(1, "v4-internal")
	v4.ID = 102
	mgr := newManagerWithKeys(t, []*channel.Channel{healthyCh(1, "a", nil, 100)},
		[]channel.ChannelModel{flash, v4},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{flash, v4}}, mgr, NewSessionRegistry(nil))

	if mr, ok := mgr.GetModelRuntime(1, 101); !ok {
		t.Fatal("model runtime 101 not found")
	} else {
		mr.Machine.ForceDisable(time.Now())
	}

	routes, err := e.RouteAll(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("routeAll: %v", err)
	}
	if len(routes) != 1 || routes[0].InternalModelID != "v4-internal" {
		t.Fatalf("禁用 flash 行后应只剩 v4 行候选，got %+v", routes)
	}
	r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max", SessionKey: ""})
	if err != nil {
		t.Fatalf("新会话应路由到同渠道另一内部模型，got %v", err)
	}
	if r.InternalModelID != "v4-internal" {
		t.Fatalf("期望落到 v4-internal，got %s", r.InternalModelID)
	}
}

// 会话命中的候选处于排空态仍放行（排空只拦新会话）。
func TestRoute_DrainedSession_StillRoutes(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	a := healthyCh(1, "a", nil, 100)
	b := healthyCh(2, "b", nil, 100)
	// 渠道 1 的密钥 11 置排空：会话命中该 (密钥,模型) 时仍放行（排空只拦新会话）。
	mgr := newManagerWithKeys(t, []*channel.Channel{a, b},
		[]channel.ChannelModel{cm(1, "int1"), cm(2, "int2")},
		[]channel.ChannelKey{newKey(11, 1, channel.StateDrain), newKey(21, 2, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{cm(1, "int1"), cm(2, "int2")}}, mgr, NewSessionRegistry(nil))

	key := "t|m|sess-drained"
	if ok, _ := e.Sessions().Attach(&Session{
		SessionID: key, ChannelKeyID: 11, InternalModelID: "int1",
		ExpireAt: time.Now().Add(time.Hour),
	}, 10); !ok {
		t.Fatal("attach failed")
	}

	r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max", SessionKey: key})
	if err != nil {
		t.Fatalf("drained session should still route, got %v", err)
	}
	if r.ChannelID != 1 {
		t.Fatalf("期望仍路由到排空渠道 1（老会话放行），got %d", r.ChannelID)
	}
}

// 密钥排空：会话命中（该密钥）仍放行续行（排空只拦新会话）。
func TestRoute_DrainedKey_SessionHit_StillRoutes(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	mgr := newManagerWithKeys(t, []*channel.Channel{healthyCh(1, "a", nil, 100)},
		[]channel.ChannelModel{cm(1, "int1")},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal)})
	if err := mgr.ManualSetKeyState(11, "drain"); err != nil {
		t.Fatalf("drain key: %v", err)
	}
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{cm(1, "int1")}}, mgr, NewSessionRegistry(nil))

	key := "t|m|sess-drained-key"
	if ok, _ := e.Sessions().Attach(&Session{
		SessionID: key, ChannelKeyID: 11, InternalModelID: "int1",
		ExpireAt: time.Now().Add(time.Hour),
	}, 10); !ok {
		t.Fatal("attach failed")
	}

	r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max", SessionKey: key})
	if err != nil {
		t.Fatalf("drained-key session should still route, got %v", err)
	}
	if r.ChannelID != 1 || r.ChannelKeyID != 11 {
		t.Fatalf("期望仍路由到排空密钥 (1,11)，got channel=%d key=%d", r.ChannelID, r.ChannelKeyID)
	}
}

// 密钥排空：新会话（未命中）不得路由到该密钥 → noAvailable（与渠道/模型排空一致）。
func TestRoute_DrainedKey_BlocksNewSession(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	mgr := newManagerWithKeys(t, []*channel.Channel{healthyCh(1, "a", nil, 100)},
		[]channel.ChannelModel{cm(1, "int1")},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal)})
	if err := mgr.ManualSetKeyState(11, "drain"); err != nil {
		t.Fatalf("drain key: %v", err)
	}
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{cm(1, "int1")}}, mgr, NewSessionRegistry(nil))

	_, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max", SessionKey: ""})
	var re *RouteError
	if !errors.As(err, &re) || re.Kind != KindNoAvailable {
		t.Fatalf("排空密钥不应接受新会话，期望 KindNoAvailable，got %v", err)
	}
}

func TestRoute_ModelNotFound(t *testing.T) {
	db, mock, _ := sqlmock.New()
	defer db.Close()
	svc := model.NewService(model.NewStore(db))
	mock.ExpectQuery(regexp.QuoteMeta(
		`SELECT id, external_name, description, enabled, sale_rates, time_config, context_tiers, created_at, updated_at FROM external_models WHERE external_name = $1 AND enabled = true AND deleted_at IS NULL LIMIT 1`)).
		WithArgs("ghost-model").
		WillReturnError(sql.ErrNoRows)

	e := NewEngine(svc, &fakeCMSource{}, nil, nil)
	_, err := e.Route(context.Background(), RouteSpec{ExternalModel: "ghost-model"})
	re, ok := AsRouteError(err)
	if !ok || re.Kind != KindModelNotFound {
		t.Fatalf("期望 KindModelNotFound，got %v", err)
	}
}

func TestRoute_AllDisabled(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	dis := cm(1, "int1")
	dis.State = channel.StateDisabled
	mgr := newManagerWithKeys(t, []*channel.Channel{healthyCh(1, "a", nil, 100)}, []channel.ChannelModel{dis},
		[]channel.ChannelKey{newKey(11, 1, channel.StateDisabled)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{dis}}, mgr, nil)

	_, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	re, ok := AsRouteError(err)
	if !ok || re.Kind != KindNoAvailable {
		t.Fatalf("期望 KindNoAvailable，got %v", err)
	}
}

func TestSessionRegistry_Expiry(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })

	s := &Session{SessionID: "k1", ChannelKeyID: 7, InternalModelID: "i",
		ExpireAt: now.Add(5 * time.Second)}
	ok, _ := r.Attach(s, 10)
	if !ok {
		t.Fatal("attach should succeed")
	}
	if _, found := r.Lookup("k1"); !found {
		t.Fatal("k1 should hit")
	}

	now = now.Add(6 * time.Second) // 过期
	if _, found := r.Lookup("k1"); found {
		t.Fatal("过期后不应命中")
	}
	if r.CountActive(7) != 0 {
		t.Fatalf("过期后密钥存活会话数应为 0，got %d", r.CountActive(7))
	}
}

// 已关闭会话保留期：关闭后过期且在保留期内 → Sweep 保留（列表可见）；
// 超过保留期 → Sweep 清理；未关闭过期 → 立即清理（与历史行为一致）。
func TestSessionRegistry_ClosedRetention(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })

	// 会话 a：未关闭，5 秒后过期（应被清扫）
	if ok, _ := r.Attach(&Session{SessionID: "a", ChannelKeyID: 1, InternalModelID: "i", ExpireAt: now.Add(5 * time.Second)}, 10); !ok {
		t.Fatal("attach a 应成功")
	}
	// 会话 b：已关闭，5 秒后过期（保留期内 → 不清扫）
	if ok, _ := r.Attach(&Session{SessionID: "b", ChannelKeyID: 1, InternalModelID: "i", ExpireAt: now.Add(5 * time.Second)}, 10); !ok {
		t.Fatal("attach b 应成功")
	}
	if !r.Close("b") {
		t.Fatal("close b 应成功")
	}
	// 会话 c：已关闭但在保留期内过期的会话，模拟保留期很长 → 不动（同 b）

	now = now.Add(6 * time.Second) // 全部过期（b/c 仍在保留期内）
	r.SweepNow()
	if _, ok := r.Lookup("a"); ok {
		t.Fatal("未关闭且过期会话 a 应被清理")
	}
	if _, ok := r.Lookup("b"); !ok {
		t.Fatal("已关闭且保留期内过期会话 b 应保留")
	}
	if r.CountActive(1) != 0 {
		t.Fatalf("已关闭会话不应占并发槽，got %d", r.CountActive(1))
	}

	// 超出保留期后再清扫：已关闭会话也被清理
	now = now.Add(31 * 24 * time.Hour)
	r.SweepNow()
	if _, ok := r.Lookup("b"); ok {
		t.Fatal("已关闭会话超过保留期后应被清理")
	}
}

func TestSessionRegistry_MaxConcurrent(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	for i := 0; i < 3; i++ {
		ok, _ := r.Attach(&Session{
			SessionID: "k" + string(rune('a'+i)), ChannelKeyID: 1, InternalModelID: "i",
			ExpireAt: now.Add(time.Hour),
		}, 3)
		if !ok {
			t.Fatalf("第 %d 个会话应成功", i+1)
		}
	}
	if ok, _ := r.Attach(&Session{
		SessionID: "koverflow", ChannelKeyID: 1, InternalModelID: "i",
		ExpireAt: now.Add(time.Hour),
	}, 3); ok {
		t.Fatal("超过 max_concurrent 后不应再加新会话")
	}
	if r.CountActive(1) != 3 {
		t.Fatalf("期望存活会话数 3，got %d", r.CountActive(1))
	}
}

// 按密钥计数：同一渠道下不同密钥互不影响，CountActive 为密钥（ChannelKeyID）维度。
func TestSessionRegistry_ByKey_IndependentCounting(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })
	r.SetKeyChannelOf(func(keyID int64) int64 {
		byKey := map[int64]int64{11: 1, 12: 1}
		return byKey[keyID]
	})
	attach := func(id string, keyID int64) bool {
		ok, _ := r.Attach(&Session{
			SessionID: id, ChannelKeyID: keyID, InternalModelID: "i",
			ExpireAt: now.Add(time.Hour),
		}, 2)
		return ok
	}
	if !attach("ka", 11) || !attach("kb", 11) {
		t.Fatal("密钥 11 前两个会话应成功")
	}
	if attach("kc", 11) {
		t.Fatal("密钥 11 达上限后不应再加新会话")
	}
	if !attach("kd", 12) {
		t.Fatal("密钥 12 计数独立：不因密钥 11 满载而拒绝")
	}
	if r.CountActive(11) != 2 || r.CountActive(12) != 1 {
		t.Fatalf("按密钥计数异常：key11=%d key12=%d", r.CountActive(11), r.CountActive(12))
	}
	if r.CountActiveByChannel(1) != 3 {
		t.Fatalf("渠道聚合计数应为 3，got %d", r.CountActiveByChannel(1))
	}
}

// 未注入 key→channel 映射时，按渠道聚合返回 0（依赖装配层注入，避免误导性统计）。
func TestSessionRegistry_CountActiveByChannel_NoResolverReturnsZero(t *testing.T) {
	now := time.Now()
	r := NewSessionRegistry(func() time.Time { return now })

	if ok, _ := r.Attach(&Session{
		SessionID: "s1", ChannelKeyID: 7, InternalModelID: "i",
		ExpireAt: now.Add(time.Hour),
	}, 3); !ok {
		t.Fatal("attach s1 应成功")
	}
	if got := r.CountActive(7); got != 1 {
		t.Fatalf("按密钥计数应为 1，got %d", got)
	}
	if got := r.CountActiveByChannel(7); got != 0 {
		t.Fatalf("未注入 resolver 时渠道聚合应返回 0，got %d", got)
	}
}

// orderCandidates 直接单测：priority 降序 → 成本升序 → 存活会话数升序 → 稳定 id 兜底。
func TestOrderCandidates_PriorityCostLoadThenID(t *testing.T) {
	rates := func(v float64) billing.Rates {
		return billing.Rates{"input": v, "output": 0, "cache_read": 0}
	}
	sess := NewSessionRegistry(nil)
	attachTo(t, sess, "sx", 33, "i3") // 渠道 3（keyID=33）已有 1 条存活会话
	e := &Engine{sess: sess, now: time.Now}

	cands := []candidate{
		{id: 1, keyID: 11, modelRow: 101, priority: 100, costRates: rates(3)},  // 高优先级但最贵
		{id: 2, keyID: 21, modelRow: 201, priority: 100, costRates: rates(1)},  // 高优先级便宜、无存活会话
		{id: 3, keyID: 33, modelRow: 301, priority: 100, costRates: rates(1)},  // 与 id=2 同价但存活会话 1
		{id: 4, keyID: 44, modelRow: 401, priority: 50, costRates: rates(0.1)}, // 低优先级层（即便最便宜也最后）
	}
	sample, globalTC := e.costContext()
	got := e.orderCandidates(cands, time.Now(), sample, globalTC)
	want := []int64{2, 3, 1, 4}
	for i := range want {
		if got[i].id != want[i] {
			t.Fatalf("排序不符：got %v（期望 id 顺序 %v）", candidateIDs(got), want)
		}
	}
}

func candidateIDs(cs []candidate) []int64 {
	out := make([]int64, len(cs))
	for i := range cs {
		out[i] = cs[i].id
	}
	return out
}

// B3：双密钥渠道 RouteAll 展开为两个 (渠道,密钥,模型) 候选，各携带不同 ChannelKeyID。
func TestRouteAll_DualKeys_ExpandsKeyCandidates(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	mgr := newManagerWithKeys(t,
		[]*channel.Channel{healthyCh(1, "a", nil, 100)},
		[]channel.ChannelModel{cm(1, "int1")},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(12, 1, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{cm(1, "int1")}}, mgr, nil)

	all, err := e.RouteAll(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("routeall: %v", err)
	}
	keyIDs := make([]int64, 0, len(all))
	for _, cr := range all {
		if cr.ChannelID != 1 {
			t.Fatalf("候选渠道越界：%d", cr.ChannelID)
		}
		if cr.ChannelKeyID == 0 {
			t.Fatalf("候选必须携带 ChannelKeyID")
		}
		keyIDs = append(keyIDs, cr.ChannelKeyID)
	}
	if len(keyIDs) != 2 || keyIDs[0] == keyIDs[1] {
		t.Fatalf("双密钥渠道应展开为两个不同 ChannelKeyID，got %v", keyIDs)
	}
}

// B3：某密钥 DISABLED 时不产出该密钥候选（RouteAll 与 Route 均不可见）。
func TestRoute_DisabledKey_ExcludedFromCandidates(t *testing.T) {
	svc, mock := newModelService(t, 2)
	defer func() { _ = mock.ExpectationsWereMet() }()

	mgr := newManagerWithKeys(t,
		[]*channel.Channel{healthyCh(1, "a", nil, 100)},
		[]channel.ChannelModel{cm(1, "int1")},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(12, 1, channel.StateDisabled)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{cm(1, "int1")}}, mgr, nil)

	all, err := e.RouteAll(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("routeall: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("期望仅 1 个候选（DISABLED 密钥排除），got %d", len(all))
	}
	if all[0].ChannelKeyID != 11 {
		t.Fatalf("DISABLED 密钥 12 不应出现，got key %d", all[0].ChannelKeyID)
	}

	r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelKeyID != 11 {
		t.Fatalf("路由应命中可用密钥 11，got %d", r.ChannelKeyID)
	}
}

// 同渠道双密钥：成本相同 → 按存活会话数分流；密钥 11 已有存活会话时新会话落到密钥 12。
func TestRoute_DualKeys_LoadBalancedBySessions(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	m := cmCost(1, "int1", 1)
	mgr := newManagerWithKeys(t,
		[]*channel.Channel{healthyCh(1, "a", nil, 100)},
		[]channel.ChannelModel{m},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(12, 1, channel.StateNormal)})
	sess := NewSessionRegistry(nil)
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{m}}, mgr, sess)

	attachTo(t, sess, "s1", 11, "int1")
	r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelKeyID != 12 {
		t.Fatalf("密钥 11 已有存活会话，应落到密钥 12，got %d", r.ChannelKeyID)
	}
}

// B3：会话命中候选置顶并携带渠道内密钥；D3 起命中判定为密钥级（ChannelKeyID+模型 精确匹配）。
func TestRoute_SessionHit_TopCandidateWithKeyID(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	mgr := newManagerWithKeys(t,
		[]*channel.Channel{healthyCh(1, "a", nil, 1), healthyCh(2, "b", nil, 100)},
		[]channel.ChannelModel{cm(1, "int1"), cm(2, "int2")},
		[]channel.ChannelKey{
			newKey(11, 1, channel.StateNormal), newKey(12, 1, channel.StateNormal),
			newKey(21, 2, channel.StateNormal),
		})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{cm(1, "int1"), cm(2, "int2")}}, mgr, NewSessionRegistry(nil))

	key := "t1|qw-max|sess"
	if ok, created := e.Sessions().Attach(&Session{
		SessionID: key, ChannelKeyID: 11, InternalModelID: "int1",
		ExpireAt: time.Now().Add(time.Hour),
	}, 10); !ok || !created {
		t.Fatalf("attach failed ok=%v created=%v", ok, created)
	}

	r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max", SessionKey: key})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelID != 1 {
		t.Fatalf("会话命中应返回渠道 1，got %d", r.ChannelID)
	}
	if r.ChannelKeyID != 11 {
		t.Fatalf("会话命中应落在归属密钥 11 上，got %d", r.ChannelKeyID)
	}
}

// D3：会话粘性为密钥级——会话绑定渠道 1 的次密钥 12 时，即使轮询顺序首密钥为 11，
// 也必须在候选集中找到 ChannelKeyID=12+同模型的候选并置顶选中（同渠道密钥不串线）。
func TestRoute_SessionHit_KeyLevelStickiness(t *testing.T) {
	svc, mock := newModelService(t, 2)
	defer func() { _ = mock.ExpectationsWereMet() }()

	mgr := newManagerWithKeys(t,
		[]*channel.Channel{healthyCh(1, "a", nil, 1)},
		[]channel.ChannelModel{cm(1, "int1")},
		[]channel.ChannelKey{
			newKey(11, 1, channel.StateNormal), newKey(12, 1, channel.StateNormal),
		})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{cm(1, "int1")}}, mgr, NewSessionRegistry(nil))

	key := "t1|qw-max|sess-k12"
	if ok, created := e.Sessions().Attach(&Session{
		SessionID: key, ChannelKeyID: 12, InternalModelID: "int1",
		ExpireAt: time.Now().Add(time.Hour),
	}, 10); !ok || !created {
		t.Fatalf("attach failed ok=%v created=%v", ok, created)
	}

	r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max", SessionKey: key})
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if r.ChannelKeyID != 12 {
		t.Fatalf("密钥级会话命中应返回归属密钥 12，got %d", r.ChannelKeyID)
	}

	// RouteAll 同样把归属密钥候选排首（网关 failover 优先续行归属密钥）。
	all, err := e.RouteAll(context.Background(), RouteSpec{ExternalModel: "qw-max", SessionKey: key})
	if err != nil {
		t.Fatalf("routeall: %v", err)
	}
	if len(all) == 0 || all[0].ChannelKeyID != 12 {
		t.Fatalf("RouteAll 首位应为归属密钥 12，got %+v", all)
	}
}

func TestRouteAll_TagAnyBoundMatch(t *testing.T) {
	svc, mock := newModelService(t, 1)
	defer func() { _ = mock.ExpectationsWereMet() }()

	ch1 := healthyCh(1, "a", nil, 100)
	ch2 := healthyCh(2, "b", nil, 100)
	mgr := newManagerWithKeys(t, []*channel.Channel{ch1, ch2},
		[]channel.ChannelModel{cm(1, "x"), cm(2, "y")},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})
	mgr.SetBoundTags(1, []channel.TagRef{{ID: 5, KV: map[string]string{"tier": "gold"}}})
	mgr.SetBoundTags(2, []channel.TagRef{{ID: 6, KV: map[string]string{"region": "cn"}}})

	cmSrc := &fakeCMSource{cms: []channel.ChannelModel{cm(1, "x"), cm(2, "y")}}
	e := NewEngine(svc, cmSrc, mgr, nil)

	routes, err := e.RouteAll(context.Background(), RouteSpec{
		ExternalModel: "qw-max",
		TagKV:         map[string]string{"region": "cn"},
	})
	if err != nil {
		t.Fatalf("route all: %v", err)
	}
	if len(routes) != 1 || routes[0].ChannelID != 2 {
		t.Fatalf("期望仅命中渠道 2，got %+v", routes)
	}
}
