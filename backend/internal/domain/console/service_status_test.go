package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
	return billing.Usage{Input: 10000, Output: 1000, CacheRead: 9000, Reasoning: 500}
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

// TestHandleServiceStatus 覆盖聚合端点：渠道明细、对外模型、按需候选展开与决策 14 的按需计算语义。
func TestHandleServiceStatus(t *testing.T) {
	mgr := channel.NewManager(nil, nil, nil, nil, nil, nil)
	h := NewAdmin(nil, nil, mgr, func(context.Context) (int64, bool) { return 1, true })
	fe := &fakeRouteEngine{}
	h.SetRouterEngine(fe)
	h.SetModelSource(fakeModelSource{})

	// 第一次请求：带 Expand=7，应按需计算候选。
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/service-status?Expand=7", nil)
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
		t.Fatalf("Candidates[\"7\"] 长度=%d, want 2", len(cand))
	}
	if cand[0].Rank != 1 || cand[1].Rank != 2 {
		t.Fatalf("Rank=%d/%d, want 1/2", cand[0].Rank, cand[1].Rank)
	}
	if data.CandidatesDataTime == nil {
		t.Fatalf("带 Expand 时 CandidatesDataTime 不应为 nil")
	}
	if fe.calls != 1 {
		t.Fatalf("RouteAll 调用次数=%d, want 1", fe.calls)
	}

	// 第二次请求：无 Expand，不应触发 RouteAll，候选为空且 CandidatesDataTime 为 nil。
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/admin/service-status", nil)
	rec2 := httptest.NewRecorder()
	h.HandleServiceStatus(rec2, req2)
	if fe.calls != 1 {
		t.Fatalf("无 Expand 时不应触发 RouteAll，calls=%d", fe.calls)
	}
	var body2 serviceStatusBody
	if err := json.Unmarshal(rec2.Body.Bytes(), &body2); err != nil {
		t.Fatalf("unmarshal response2: %v", err)
	}
	var data2 serviceStatusData
	if err := json.Unmarshal(body2.Data, &data2); err != nil {
		t.Fatalf("unmarshal Data2: %v", err)
	}
	if len(data2.Candidates) != 0 {
		t.Fatalf("无 Expand 时 Candidates 应为空 map，got=%+v", data2.Candidates)
	}
	if data2.CandidatesDataTime != nil {
		t.Fatalf("无 Expand 时 CandidatesDataTime 应为 nil，got=%v", data2.CandidatesDataTime)
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
