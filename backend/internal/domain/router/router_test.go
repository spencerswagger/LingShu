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
			`{"input":1.0,"output":2.0,"cache_read":0.1,"cache_write":0.3,"reasoning":1.0}`,
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
		State: channel.StateNormal,
	}
}

func cm(chID int64, internal string) channel.ChannelModel {
	return channel.ChannelModel{
		ID: chID*100 + 1, ChannelID: chID, InternalModelID: internal,
		ExternalModelID: 10, State: channel.StateNormal,
		CostRates: billing.Rates{"input": 1, "output": 2, "cache_read": 0.1, "cache_write": 0.3, "reasoning": 1},
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

func TestRoute_NoTags_RoundRobinDistribution(t *testing.T) {
	const calls = 100
	svc, mock := newModelService(t, calls)
	defer func() { _ = mock.ExpectationsWereMet() }()

	// 同 priority（同层）两渠道在层内加权随机轮转，多次调用两渠道都应获选。
	mgr := newManagerWithKeys(t, []*channel.Channel{
		healthyCh(1, "a", nil, 100),
		healthyCh(2, "b", nil, 100),
	}, []channel.ChannelModel{cm(1, "int1"), cm(2, "int2")},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{cm(1, "int1"), cm(2, "int2")}}, mgr, nil)

	seen := map[int64]int{}
	for i := 0; i < calls; i++ {
		r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
		if err != nil {
			t.Fatalf("route #%d: %v", i, err)
		}
		if r.ChannelID != 1 && r.ChannelID != 2 {
			t.Fatalf("结果落在候选集之外：%d", r.ChannelID)
		}
		seen[r.ChannelID]++
	}
	if len(seen) != 2 {
		t.Fatalf("同层加权随机下期望两渠道都被命中，got %v", seen)
	}
}

// 会话命中（Attach 后）应覆盖加权轮询：即使权重指向渠道 2，也返回渠道 1。
func TestRoute_SessionHit_OverridesWeight(t *testing.T) {
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
	a.State = channel.StateDrain // 渠道排空
	b := healthyCh(2, "b", nil, 100)
	mgr := newManagerWithKeys(t, []*channel.Channel{a, b},
		[]channel.ChannelModel{cm(1, "int1"), cm(2, "int2")},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(21, 2, channel.StateNormal)})
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

	down := healthyCh(1, "a", nil, 100)
	down.State = channel.StateDisabled
	dis := cm(1, "int1")
	mgr := newManagerWithKeys(t, []*channel.Channel{down}, []channel.ChannelModel{dis},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal)})
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

func TestOrderByLayerAndWeight(t *testing.T) {
	cands := []candidate{
		{id: 1, priority: 100, weight: 1},
		{id: 2, priority: 100, weight: 3},
		{id: 3, priority: 50, weight: 1},
	}
	// 固定随机值 r=0：层 100 总权 4，落点 0 命中首个权重区间，
	// 渠道顺序应以被选中者起始。验证高层两渠道排在低层之前。
	got := orderByLayerAndWeight(cands, newStubRand(0))
	if got[len(got)-1].id != 3 {
		t.Fatalf("低优先级渠道应在最后，got %+v", got)
	}
	front := map[int64]bool{got[0].id: true, got[1].id: true}
	if !front[1] || !front[2] {
		t.Fatalf("高层两渠道应在前，got %+v", got)
	}
}

type stubRand struct{ n int }

func newStubRand(n int) *stubRand  { return &stubRand{n: n} }
func (s *stubRand) Intn(_ int) int { return s.n }

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

// B3：Route 对双密钥渠道按轮询打散，多次调用下两个密钥都获选。
func TestRoute_DualKeys_RoundRobinAcrossKeys(t *testing.T) {
	const calls = 100
	svc, mock := newModelService(t, calls)
	defer func() { _ = mock.ExpectationsWereMet() }()

	mgr := newManagerWithKeys(t,
		[]*channel.Channel{healthyCh(1, "a", nil, 100)},
		[]channel.ChannelModel{cm(1, "int1")},
		[]channel.ChannelKey{newKey(11, 1, channel.StateNormal), newKey(12, 1, channel.StateNormal)})
	e := NewEngine(svc, &fakeCMSource{cms: []channel.ChannelModel{cm(1, "int1")}}, mgr, nil)

	seen := map[int64]int{}
	for i := 0; i < calls; i++ {
		r, err := e.Route(context.Background(), RouteSpec{ExternalModel: "qw-max"})
		if err != nil {
			t.Fatalf("route #%d: %v", i, err)
		}
		if r.ChannelKeyID != 11 && r.ChannelKeyID != 12 {
			t.Fatalf("结果落在密钥候选之外：%d", r.ChannelKeyID)
		}
		seen[r.ChannelKeyID]++
	}
	if len(seen) != 2 {
		t.Fatalf("密钥轮询打散应命中两个密钥，got %v", seen)
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
