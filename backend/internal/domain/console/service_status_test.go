package console

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/model"
	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// fakeRouteEngine 记录 RouteAll 调用次数，返回固定两条候选，供按需计算断言使用。
type fakeRouteEngine struct {
	calls int
}

func (f *fakeRouteEngine) RouteAll(ctx context.Context, spec router.RouteSpec) ([]*router.ChannelRoute, error) {
	f.calls++
	return []*router.ChannelRoute{
		{ChannelID: 1, ChannelKeyID: 1, InternalModelID: "m1", ExternalModelID: 7, Cost: 1.0, CostKnown: true},
		{ChannelID: 2, ChannelKeyID: 2, InternalModelID: "m2", ExternalModelID: 7, Cost: 2.0, CostKnown: true},
	}, nil
}

func (f *fakeRouteEngine) CostSample() billing.Usage {
	return billing.Usage{Input: 10000, Output: 1000, CacheRead: 9000}
}

// ActiveKeySessions 固定返回 3，供服务状态端点密钥存活会话数回填断言使用。
func (f *fakeRouteEngine) ActiveKeySessions(keyID int64) int { return 3 }

// fakeModelSource 返回固定一条对外模型。
type fakeModelSource struct{}

func (fakeModelSource) ListModels(ctx context.Context, enabled *bool) ([]model.ExternalModel, error) {
	return []model.ExternalModel{{ID: 7, ExternalName: "gpt-x", Enabled: true}}, nil
}

// serviceStatusData 是聚合端点响应的 Data 部分（仅取本测试断言所需字段）。
type serviceStatusData struct {
	DataTime           time.Time                  `json:"DataTime"`
	Channels           []channel.ChannelDetail    `json:"Channels"`
	ExternalModels     []externalModelItem        `json:"ExternalModels"`
	Candidates         map[string][]candidateItem `json:"Candidates"`
	CandidatesDataTime *time.Time                 `json:"CandidatesDataTime"`
}

// serviceStatusBody 是统一响应外层（Code/RequestID/Data）。
type serviceStatusBody struct {
	Code      int             `json:"Code"`
	RequestID string          `json:"RequestID"`
	Data      json.RawMessage `json:"Data"`
}

// TestHandleServiceStatus 覆盖聚合端点：渠道明细、对外模型、全量候选计算（决策 14 调整为全量）。
// 候选（成本/序位）对全部「已启用」对外模型逐一计算，不再依赖 Expand 查询参数；已禁用模型跳过。
func TestHandleServiceStatus(t *testing.T) {
	mgr := channel.NewManager(nil, nil, nil, nil, nil, nil)
	h := NewAdmin(nil, nil, mgr, func(context.Context) (int64, bool) { return 1, true })
	fe := &fakeRouteEngine{}
	h.SetRouterEngine(fe)
	h.SetModelSource(fakeModelSource{})

	// 第一次请求：无任何查询参数，应直接对启用的对外模型（ID=7）计算候选。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/service-status", nil)
	req = resp.WithRequestID(req, "req-service-status")
	rec := httptest.NewRecorder()
	h.HandleServiceStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body serviceStatusBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.Code != 0 {
		t.Fatalf("Code=%d, want 0; body=%s", body.Code, rec.Body.String())
	}
	if body.RequestID == "" {
		t.Fatalf("RequestID 不应为空: %s", rec.Body.String())
	}
	var data serviceStatusData
	if err := json.Unmarshal(body.Data, &data); err != nil {
		t.Fatalf("unmarshal Data: %v", err)
	}
	if data.DataTime.IsZero() {
		t.Fatalf("DataTime 不应为零值")
	}
	if len(data.ExternalModels) != 1 || data.ExternalModels[0].ID != 7 {
		t.Fatalf("ExternalModels=%+v, want 1 项且 ID=7", data.ExternalModels)
	}
	cand := data.Candidates["7"]
	if len(cand) != 2 {
		t.Fatalf("Candidates[\"7\"] 长度=%d, want 2（无需 Expand 即应计算）", len(cand))
	}
	if cand[0].Rank != 1 || cand[1].Rank != 2 {
		t.Fatalf("Rank=%d/%d, want 1/2", cand[0].Rank, cand[1].Rank)
	}
	if data.CandidatesDataTime == nil {
		t.Fatalf("已注入引擎时应计算候选，CandidatesDataTime 不应为 nil")
	}
	if fe.calls != 1 {
		t.Fatalf("RouteAll 调用次数=%d, want 1", fe.calls)
	}

	// 第二次请求：仍应全量计算（不再受 Expand 参数控制），候选与首次一致。
	rec2 := httptest.NewRecorder()
	h.HandleServiceStatus(rec2, httptest.NewRequest(http.MethodGet, "/api/v1/admin/service-status", nil))
	if fe.calls != 2 {
		t.Fatalf("第二次请求也应全量计算，calls=%d, want 2", fe.calls)
	}
	var body2 serviceStatusBody
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatalf("unmarshal response2: %v", err)
	}
	var data2 serviceStatusData
	if err := json.Unmarshal(body2.Data, &data2); err != nil {
		t.Fatalf("unmarshal Data2: %v", err)
	}
	if len(data2.Candidates["7"]) != 2 {
		t.Fatalf("第二次请求 Candidates[\"7\"] 长度=%d, want 2", len(data2.Candidates["7"]))
	}
	if data2.CandidatesDataTime == nil {
		t.Fatalf("第二次请求 CandidatesDataTime 不应为 nil")
	}

	// 已禁用的对外模型（ID=8）无法路由，应跳过而不产生候选与错误调用。
	h2 := NewAdmin(nil, nil, channel.NewManager(nil, nil, nil, nil, nil, nil), func(context.Context) (int64, bool) { return 1, true })
	fe2 := &fakeRouteEngine{}
	h2.SetRouterEngine(fe2)
	h2.SetModelSource(fakeModelListSource{items: []model.ExternalModel{
		{ID: 7, ExternalName: "gpt-x", Enabled: true},
		{ID: 8, ExternalName: "gpt-off", Enabled: false},
	}})
	data3 := doStatus(t, h2)
	if len(data3.Candidates["7"]) != 2 {
		t.Fatalf("启用模型 ID=7 应有候选，got=%+v", data3.Candidates["7"])
	}
	if _, ok := data3.Candidates["8"]; ok {
		t.Fatalf("已禁用模型 ID=8 不应计算候选，got=%+v", data3.Candidates["8"])
	}
	if fe2.calls != 1 {
		t.Fatalf("仅启用模型应触发 RouteAll，calls=%d, want 1", fe2.calls)
	}
}

// TestHandleServiceStatus_ActiveSessions 验证密钥存活会话数由路由引擎实时回填（非快照）；
// 未装配路由引擎时保持 0，且与 MaxSessions（并发上限）语义区分。
func TestHandleServiceStatus_ActiveSessions(t *testing.T) {
	mgr := channel.NewManager(nil, nil, nil, nil, nil, nil)
	ch := channel.Channel{
		ID: 1, Name: "c1", Protocol: channel.ProtocolOpenAICompat,
		RateLimit: channel.RateLimitConfig{RPM: 100, TPM: 1000, BurstMultiplier: 1.0, MaxConcurrent: 8},
	}
	mgr.Upsert(&ch)
	mgr.UpsertKey(channel.ChannelKey{ID: 11, ChannelID: 1, Name: "k1", State: channel.StateNormal})

	// 已装配路由引擎：ActiveSessions 取 fake 返回值 3。
	h := NewAdmin(nil, nil, mgr, func(context.Context) (int64, bool) { return 1, true })
	h.SetRouterEngine(&fakeRouteEngine{})
	rec := httptest.NewRecorder()
	h.HandleServiceStatus(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/service-status", nil))

	var body serviceStatusBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	var data serviceStatusData
	if err := json.Unmarshal(body.Data, &data); err != nil {
		t.Fatalf("unmarshal Data: %v", err)
	}
	if len(data.Channels) != 1 || len(data.Channels[0].Keys) != 1 {
		t.Fatalf("want 1 channel with 1 key, got %+v", data.Channels)
	}
	k := data.Channels[0].Keys[0]
	if k.View.KeyID != 11 {
		t.Fatalf("key id=%d, want 11", k.View.KeyID)
	}
	if k.ActiveSessions != 3 {
		t.Fatalf("ActiveSessions=%d, want 3（fake 返回值）", k.ActiveSessions)
	}
	if k.MaxSessions != 8 {
		t.Fatalf("MaxSessions=%d, want 8（上限，与 ActiveSessions 区分）", k.MaxSessions)
	}

	// 未装配路由引擎：ActiveSessions 保持 0。
	h2 := NewAdmin(nil, nil, mgr, func(context.Context) (int64, bool) { return 1, true })
	rec2 := httptest.NewRecorder()
	h2.HandleServiceStatus(rec2, httptest.NewRequest(http.MethodGet, "/api/v1/admin/service-status", nil))
	var body2 serviceStatusBody
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatalf("unmarshal response2: %v", err)
	}
	var data2 serviceStatusData
	if err := json.Unmarshal(body2.Data, &data2); err != nil {
		t.Fatalf("unmarshal Data2: %v", err)
	}
	if len(data2.Channels) != 1 || len(data2.Channels[0].Keys) != 1 {
		t.Fatalf("want 1 channel with 1 key（无路由引擎）, got %+v", data2.Channels)
	}
	if got := data2.Channels[0].Keys[0].ActiveSessions; got != 0 {
		t.Fatalf("未装配路由引擎时 ActiveSessions=%d, want 0", got)
	}
}

// fakeModelListSource 返回可配置的对外模型清单，供「已删除对外模型残留过滤」断言使用。
type fakeModelListSource struct{ items []model.ExternalModel }

func (s fakeModelListSource) ListModels(ctx context.Context, enabled *bool) ([]model.ExternalModel, error) {
	return s.items, nil
}

// doStatus 发起一次服务状态请求并解出 Data，供断言使用。
func doStatus(t *testing.T, h *Admin) serviceStatusData {
	t.Helper()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/service-status", nil)
	req = resp.WithRequestID(req, "req-status")
	h.HandleServiceStatus(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body serviceStatusBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	var data serviceStatusData
	if err := json.Unmarshal(body.Data, &data); err != nil {
		t.Fatalf("unmarshal Data: %v", err)
	}
	return data
}

// TestHandleServiceStatus_FiltersDeletedExternalModels 覆盖「已删除对外模型不再展示」的一致性收敛：
// 渠道运行时快照会残留「外部模型已被软删」的内部模型行，聚合端点须按未删清单白名单剔除，
// 并据裁剪后的结果重算渠道派生状态；未注入模型源时不得过滤。
func TestHandleServiceStatus_FiltersDeletedExternalModels(t *testing.T) {
	// 渠道含两个内部模型：m-live 绑定存在的对外模型 7；m-dead 绑定已删的对外模型 99（残留且为 DISABLED）。
	newMgr := func() *channel.Manager {
		mgr := channel.NewManager(nil, nil, nil, nil, nil, nil)
		mgr.Upsert(&channel.Channel{ID: 1, Name: "c1", Protocol: channel.ProtocolOpenAICompat})
		mgr.UpsertModel(&channel.ChannelModel{ID: 1, ChannelID: 1, InternalModelID: "m-live", ExternalModelID: 7, State: channel.StateNormal})
		mgr.UpsertModel(&channel.ChannelModel{ID: 2, ChannelID: 1, InternalModelID: "m-dead", ExternalModelID: 99, State: channel.StateDisabled})
		return mgr
	}
	newAdmin := func(mgr *channel.Manager) *Admin {
		return NewAdmin(nil, nil, mgr, func(context.Context) (int64, bool) { return 1, true })
	}

	// 已注入模型源且清单仅含 7：剔除残留行，且渠道派生状态据裁剪结果重算（残留 DISABLED 行被删 → NORMAL）。
	h := newAdmin(newMgr())
	h.SetModelSource(fakeModelListSource{items: []model.ExternalModel{{ID: 7, ExternalName: "gpt-x", Enabled: true}}})
	data := doStatus(t, h)
	if len(data.Channels) != 1 {
		t.Fatalf("want 1 channel, got %d", len(data.Channels))
	}
	models := data.Channels[0].Models
	if len(models) != 1 || models[0].View.InternalModelID != "m-live" {
		t.Fatalf("应只保留绑定存在对外模型的内部模型，got=%+v", models)
	}
	if got := string(data.Channels[0].State); got != string(channel.StateNormal) {
		t.Fatalf("State=%s, want NORMAL（残留 DISABLED 行被剔除后据裁剪结果重算）", got)
	}

	// 已注入模型源但清单为空（全部对外模型均已删除）：残留行应全部清空。
	h2 := newAdmin(newMgr())
	h2.SetModelSource(fakeModelListSource{items: []model.ExternalModel{}})
	if got := doStatus(t, h2).Channels[0].Models; len(got) != 0 {
		t.Fatalf("清单为空时应清空全部残留模型，got=%+v", got)
	}

	// 未注入模型源：无法判定存在性，不做过滤（保留全部内部模型）。
	h3 := newAdmin(newMgr())
	if got := doStatus(t, h3).Channels[0].Models; len(got) != 2 {
		t.Fatalf("未注入模型源时不应过滤，got=%d", len(got))
	}
}

// fakeRouteEngineNoRoute 对 failName 指定的对外模型返回路由业务错误（无可用候选），
// 其余模型返回固定单条候选；用于验证「无候选属预期状态：跳过且不刷 ERROR」。
type fakeRouteEngineNoRoute struct {
	failName string
	calls    int
}

func (f *fakeRouteEngineNoRoute) RouteAll(ctx context.Context, spec router.RouteSpec) ([]*router.ChannelRoute, error) {
	f.calls++
	if spec.ExternalModel == f.failName {
		return nil, &router.RouteError{
			Kind:       router.KindNoAvailable,
			HTTPStatus: http.StatusServiceUnavailable,
			Code:       resp.CodeNoRoute,
			Message:    "无可用渠道，请稍后重试",
		}
	}
	return []*router.ChannelRoute{{ChannelID: 1, ChannelKeyID: 1, InternalModelID: "m1", ExternalModelID: 7, Cost: 1.0, CostKnown: true}}, nil
}

func (f *fakeRouteEngineNoRoute) CostSample() billing.Usage { return billing.Usage{} }

func (f *fakeRouteEngineNoRoute) ActiveKeySessions(keyID int64) int { return 0 }

// TestHandleServiceStatus_NoAvailableCandidateSkipped 验证启用但无可用候选的对外模型：
// RouteAll 返回路由业务错误时应跳过（不产生候选人条目、不影响其它模型），且请求仍正常返回 200。
func TestHandleServiceStatus_NoAvailableCandidateSkipped(t *testing.T) {
	h := NewAdmin(nil, nil, channel.NewManager(nil, nil, nil, nil, nil, nil), func(context.Context) (int64, bool) { return 1, true })
	fe := &fakeRouteEngineNoRoute{failName: "gpt-no-route"}
	h.SetRouterEngine(fe)
	h.SetModelSource(fakeModelListSource{items: []model.ExternalModel{
		{ID: 7, ExternalName: "gpt-x", Enabled: true},
		{ID: 9, ExternalName: "gpt-no-route", Enabled: true},
	}})

	data := doStatus(t, h)
	if len(data.Candidates["7"]) != 1 {
		t.Fatalf("有候选模型 ID=7 应产生 1 条候选，got=%+v", data.Candidates["7"])
	}
	if _, ok := data.Candidates["9"]; ok {
		t.Fatalf("无可用候选的模型 ID=9 不应产生候选人条目，got=%+v", data.Candidates["9"])
	}
	if fe.calls != 2 {
		t.Fatalf("两个启用模型均应被计算一次，calls=%d, want 2", fe.calls)
	}
}

// fakeRouteEngineCtxErr 对任意请求返回固定的非路由业务错误，用于验证 ctx 取消/超时的降级路径。
type fakeRouteEngineCtxErr struct {
	err   error
	calls int
}

func (f *fakeRouteEngineCtxErr) RouteAll(ctx context.Context, spec router.RouteSpec) ([]*router.ChannelRoute, error) {
	f.calls++
	return nil, f.err
}

func (f *fakeRouteEngineCtxErr) CostSample() billing.Usage         { return billing.Usage{} }
func (f *fakeRouteEngineCtxErr) ActiveKeySessions(keyID int64) int { return 0 }

// levelCaptureHandler 捕获 slog 记录并按级别计数，供降级断言使用。
type levelCaptureHandler struct {
	mu     sync.Mutex
	errors int
	warns  int
}

func (h *levelCaptureHandler) Enabled(ctx context.Context, l slog.Level) bool { return true }

func (h *levelCaptureHandler) Handle(ctx context.Context, r slog.Record) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case r.Level >= slog.LevelError:
		h.errors++
	case r.Level >= slog.LevelWarn:
		h.warns++
	}
	return nil
}

func (h *levelCaptureHandler) WithAttrs(attrs []slog.Attr) slog.Handler { return h }
func (h *levelCaptureHandler) WithGroup(name string) slog.Handler       { return h }

func (h *levelCaptureHandler) errorCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.errors
}

func (h *levelCaptureHandler) warnCount() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.warns
}

// TestHandleServiceStatus_ContextCanceledDowngraded 验证：请求 ctx 被取消/超时导致上游探测返回
// 非路由业务错误时，按客户端中断处理——降级为 Warn 而非 Error（全量循环下客户端中断不刷 ERROR 噪音），
// 且不影响响应（仍 200，仅跳过该模型候选）。
func TestHandleServiceStatus_ContextCanceledDowngraded(t *testing.T) {
	prev := slog.Default()
	defer slog.SetDefault(prev)
	capture := &levelCaptureHandler{}
	slog.SetDefault(slog.New(capture))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	h := NewAdmin(nil, nil, channel.NewManager(nil, nil, nil, nil, nil, nil), func(context.Context) (int64, bool) { return 1, true })
	fe := &fakeRouteEngineCtxErr{err: ctx.Err()}
	h.SetRouterEngine(fe)
	h.SetModelSource(fakeModelListSource{items: []model.ExternalModel{{ID: 7, ExternalName: "gpt-x", Enabled: true}}})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/service-status", nil).WithContext(ctx)
	req = resp.WithRequestID(req, "req-ctx-canceled")
	h.HandleServiceStatus(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200（ctx 取消仅影响日志级别，不影响响应）; body=%s", rec.Code, rec.Body.String())
	}
	var body serviceStatusBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	var data serviceStatusData
	if err := json.Unmarshal(body.Data, &data); err != nil {
		t.Fatalf("unmarshal Data: %v", err)
	}
	if _, ok := data.Candidates["7"]; ok {
		t.Fatalf("ctx 取消应跳过该模型候选，got=%+v", data.Candidates["7"])
	}
	if capture.errorCount() != 0 {
		t.Fatalf("ctx 取消不应产生 ERROR 日志，got=%d", capture.errorCount())
	}
	if capture.warnCount() == 0 {
		t.Fatalf("ctx 取消应降级为 WARN 日志，got=0")
	}
}
