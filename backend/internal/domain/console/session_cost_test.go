package console

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/model"
	"github.com/team/llmgateway/internal/domain/router"
)

// fakeExtModelSource 满足 console.externalModelSource（供候选渠道枚举解析对外模型 ID）。
type fakeExtModelSource struct {
	list []model.ExternalModel
}

func (f fakeExtModelSource) ListModels(_ context.Context, _ *bool) ([]model.ExternalModel, error) {
	return f.list, nil
}

// sessionCostTestManager 构造含单渠道 + 单内部模型（绑定对外模型 10）的渠道运行时。
func sessionCostTestManager(t *testing.T) *channel.Manager {
	t.Helper()
	mgr := channel.NewManager(nil, nil, make([]byte, 16), nil, nil, nil)
	mgr.Upsert(&channel.Channel{
		ID: 1, Name: "a", Protocol: channel.ProtocolOpenAICompat,
		BaseURL: "https://x.example.com", Priority: 100,
	})
	mgr.UpsertModel(&channel.ChannelModel{
		ID: 101, ChannelID: 1, InternalModelID: "int1", ExternalModelID: 10,
		State:     channel.StateNormal,
		CostRates: billing.Rates{"input": 0.5, "output": 1.0, "cache_read": 0.05},
	})
	return mgr
}

// TestSessionCost_ConfigEndpointShapeAndSources 断言 /session-cost/config 返回各候选渠道三档成本价、
// 时段/分档倍率、当前时刻倍率、cost_sample 默认样值、k 系数与门控阈值，且所有值带来源标签。
func TestSessionCost_ConfigEndpointShapeAndSources(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	admin := NewAdmin(db, billing.NewSqlStore(db), sessionCostTestManager(t), nil)
	admin.SetModelSource(fakeExtModelSource{list: []model.ExternalModel{
		{ID: 10, ExternalName: "qw-max", Enabled: true},
	}})
	admin.now = func() time.Time { return time.Date(2026, 1, 2, 20, 0, 0, 0, time.UTC) } // 高峰

	// 读取顺序与实现一致：time_config → context_tiers → cost_sample → session_cost。
	expectConfigRow(mock, "billing.time_config",
		[]byte(`{"Timezone":"UTC","Default":1,"Periodic":[{"Name":"peak","Start":"18:00","End":"22:00","Coeff":2}]}`))
	expectConfigRow(mock, "billing.context_tiers", []byte(`[{"Min":8000,"Coeff":2},{"Min":0,"Coeff":1}]`))
	expectConfigRow(mock, "routing.cost_sample", []byte(`{"input":6000,"cache_read":4000,"output":200}`))
	expectConfigRow(mock, "routing.session_cost",
		[]byte(`{"enabled":true,"remaining_a":20,"remaining_b":0,"remaining_min":1,"remaining_max":20,"min_remaining":1,"cooldown_sec":0,"min_improve":0.1}`))

	rec := httptest.NewRecorder()
	admin.HandleSessionCostConfig(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/session-cost/config?Model=qw-max", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	var body struct {
		Code int             `json:"Code"`
		Data json.RawMessage `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body.Code != 0 {
		t.Fatalf("Code=%d, want 0; body=%s", body.Code, rec.Body.String())
	}

	var data struct {
		Now                time.Time `json:"Now"`
		NowTimeCoeff       float64   `json:"NowTimeCoeff"`
		NowTimeCoeffSource string    `json:"NowTimeCoeffSource"`
		TimeConfigSource   string    `json:"TimeConfigSource"`
		ContextTiersSource string    `json:"ContextTiersSource"`
		CostSample         struct {
			Input, CacheRead, Output int64
		} `json:"CostSample"`
		CostSampleSource  string `json:"CostSampleSource"`
		SessionCostSource string `json:"SessionCostSource"`
		SessionCost       struct {
			Enabled    bool    `json:"enabled"`
			RemainingA float64 `json:"remaining_a"`
			MidSwitch  bool    `json:"mid_switch"`
		} `json:"SessionCost"`
		Gates struct {
			RemainingA float64 `json:"RemainingA"`
			MinImprove float64 `json:"MinImprove"`
			Source     string  `json:"Source"`
		} `json:"Gates"`
		CandidatesSource string `json:"CandidatesSource"`
		Candidates       []struct {
			ChannelID       string `json:"ChannelID"`
			InternalModelID string `json:"InternalModelID"`
			Cost            struct {
				Input, CacheRead, Output float64
				Source                   string
			} `json:"Cost"`
			TimeCoeff       float64 `json:"TimeCoeff"`
			TimeCoeffSource string  `json:"TimeCoeffSource"`
			CtxCoeff        float64 `json:"CtxCoeff"`
			CtxCoeffSource  string  `json:"CtxCoeffSource"`
		} `json:"Candidates"`
	}
	if err := json.Unmarshal(body.Data, &data); err != nil {
		t.Fatalf("unmarshal Data: %v", err)
	}

	// 来源标签必须齐全。
	sources := map[string]string{
		"NowTimeCoeffSource": data.NowTimeCoeffSource,
		"TimeConfigSource":   data.TimeConfigSource,
		"ContextTiersSource": data.ContextTiersSource,
		"CostSampleSource":   data.CostSampleSource,
		"SessionCostSource":  data.SessionCostSource,
		"CandidatesSource":   data.CandidatesSource,
	}
	for name, v := range sources {
		if v == "" {
			t.Fatalf("%s 来源标签缺失（应为非空）", name)
		}
	}
	if data.Gates.Source == "" {
		t.Fatal("Gates.Source 来源标签缺失")
	}

	// 当前时刻倍率（高峰 18:00-22:00 系数 2）。
	if data.NowTimeCoeff != 2 {
		t.Fatalf("NowTimeCoeff=%v, want 2（高峰）", data.NowTimeCoeff)
	}
	// cost_sample 默认样值。
	if data.CostSample.Input != 6000 || data.CostSample.CacheRead != 4000 || data.CostSample.Output != 200 {
		t.Fatalf("CostSample=%+v, want 6000/4000/200", data.CostSample)
	}
	// k 系数与门控阈值。
	if data.SessionCost.RemainingA != 20 || data.Gates.RemainingA != 20 || data.Gates.MinImprove != 0.1 {
		t.Fatalf("k 系数/门控阈值不符: SessionCost=%+v Gates=%+v", data.SessionCost, data.Gates)
	}

	// 候选渠道三档成本价 + 时段/分档倍率（高峰系数 2；Input+CacheRead=10000 落高档系数 2）。
	if len(data.Candidates) != 1 {
		t.Fatalf("Candidates 数量=%d, want 1", len(data.Candidates))
	}
	c := data.Candidates[0]
	if c.InternalModelID != "int1" {
		t.Fatalf("candidate InternalModelID=%q, want int1", c.InternalModelID)
	}
	if c.Cost.Input != 0.5 || c.Cost.CacheRead != 0.05 || c.Cost.Output != 1.0 {
		t.Fatalf("candidate 三档成本价=%+v, want 0.5/0.05/1.0", c.Cost)
	}
	if c.Cost.Source == "" || c.TimeCoeffSource == "" || c.CtxCoeffSource == "" {
		t.Fatalf("candidate 来源标签缺失: %+v", c)
	}
	if c.TimeCoeff != 2 || c.CtxCoeff != 2 {
		t.Fatalf("candidate 倍率=%v/%v, want 2/2", c.TimeCoeff, c.CtxCoeff)
	}
}

// TestSessionCost_EventsAndDistributionFromStore 断言写入事件存储后可被 /events 读到（含来源标签、
// NaN 回本序列化为 null），且 /distribution 按会话聚合临界缓存读分位与直方。
func TestSessionCost_EventsAndDistributionFromStore(t *testing.T) {
	store := router.NewSessionCostEventStore(0)
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	admin := NewAdmin(db, billing.NewSqlStore(db), nil, nil)
	admin.SetSessionCostEventStore(store)

	store.Append(router.SessionCostEvent{
		SessionID: "s1", Turn: 1, CrossCacheRead: 13480, CrossCacheReadKnown: true,
		Decision: router.SessionCostDecision{Switch: true, FromChannelKeyID: 21, ToChannelKeyID: 11,
			TurnCostOld: 0.00305, TurnCostNew: 0.00205, RechargeCost: 0.00294, PaybackTurns: 18.04,
			RemainingTurns: 20, Reason: "中途：剩余轮数 > 回本轮数，切换更省"},
	})
	store.Append(router.SessionCostEvent{
		SessionID: "s1", Turn: 2, CrossCacheRead: 20000, CrossCacheReadKnown: true,
		Decision: router.SessionCostDecision{Switch: true, PaybackTurns: 12.0, RemainingTurns: 19},
	})
	// 起点择优（无旧渠道）：crossCacheRead 未知 + 回本 NaN（应序列化为 null），不参与分布。
	store.Append(router.SessionCostEvent{
		SessionID: "s2", Turn: 1, CrossCacheReadKnown: false,
		Decision: router.SessionCostDecision{Switch: true, PaybackTurns: math.NaN(), RemainingTurns: 20, Reason: "起点"},
	})

	// /events：全量。
	rec := httptest.NewRecorder()
	admin.HandleSessionCostEvents(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/session-cost/events", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("events status=%d, body=%s", rec.Code, rec.Body.String())
	}
	var ev struct {
		Code int `json:"Code"`
		Data struct {
			List []struct {
				SessionID            string   `json:"SessionID"`
				Turn                 int      `json:"Turn"`
				PaybackTurns         *float64 `json:"PaybackTurns"`
				CrossCacheRead       float64  `json:"CrossCacheRead"`
				CrossCacheReadKnown  bool     `json:"CrossCacheReadKnown"`
				CrossCacheReadSource string   `json:"CrossCacheReadSource"`
			} `json:"List"`
			Total  int    `json:"Total"`
			Source string `json:"Source"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ev); err != nil {
		t.Fatalf("unmarshal events: %v", err)
	}
	if ev.Code != 0 || ev.Data.Total != 3 || len(ev.Data.List) != 3 {
		t.Fatalf("events Code=%d Total=%d len=%d, want 0/3/3", ev.Code, ev.Data.Total, len(ev.Data.List))
	}
	if ev.Data.Source == "" {
		t.Fatal("events Source 来源标签缺失")
	}
	if ev.Data.List[0].SessionID != "s1" || ev.Data.List[0].CrossCacheReadSource == "" {
		t.Fatalf("events 首条不符: %+v", ev.Data.List[0])
	}
	if ev.Data.List[2].PaybackTurns != nil {
		t.Fatalf("NaN 回本轮数应序列化为 null，got %v", *ev.Data.List[2].PaybackTurns)
	}

	// /events?SessionID=s1：过滤。
	recF := httptest.NewRecorder()
	admin.HandleSessionCostEvents(recF, httptest.NewRequest(http.MethodGet, "/api/v1/admin/session-cost/events?SessionID=s1", nil))
	var evF struct {
		Data struct {
			Total int `json:"Total"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(recF.Body.Bytes(), &evF); err != nil {
		t.Fatalf("unmarshal filtered events: %v", err)
	}
	if evF.Data.Total != 2 {
		t.Fatalf("按 s1 过滤 Total=%d, want 2", evF.Data.Total)
	}

	// /distribution：仅 s1 有已知临界缓存读（2 条：13480 / 20000）。
	recD := httptest.NewRecorder()
	admin.HandleSessionCostDistribution(recD, httptest.NewRequest(http.MethodGet, "/api/v1/admin/session-cost/distribution", nil))
	if recD.Code != http.StatusOK {
		t.Fatalf("distribution status=%d, body=%s", recD.Code, recD.Body.String())
	}
	var dist struct {
		Code int `json:"Code"`
		Data struct {
			Sessions []struct {
				SessionID       string  `json:"SessionID"`
				Count           int     `json:"Count"`
				P10             float64 `json:"P10"`
				Median          float64 `json:"Median"`
				P90             float64 `json:"P90"`
				QuantilesSource string  `json:"QuantilesSource"`
				Histogram       []struct {
					UpperBound float64 `json:"UpperBound"`
					Count      int     `json:"Count"`
				} `json:"Histogram"`
			} `json:"Sessions"`
			TotalSessions int    `json:"TotalSessions"`
			Source        string `json:"Source"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(recD.Body.Bytes(), &dist); err != nil {
		t.Fatalf("unmarshal distribution: %v", err)
	}
	if dist.Data.TotalSessions != 1 || len(dist.Data.Sessions) != 1 {
		t.Fatalf("distribution Sessions=%d TotalSessions=%d, want 1/1", len(dist.Data.Sessions), dist.Data.TotalSessions)
	}
	if dist.Data.Source == "" {
		t.Fatal("distribution Source 来源标签缺失")
	}
	s := dist.Data.Sessions[0]
	if s.SessionID != "s1" || s.Count != 2 {
		t.Fatalf("distribution s1 Count=%d, want 2", s.Count)
	}
	if s.QuantilesSource == "" {
		t.Fatal("distribution QuantilesSource 来源标签缺失")
	}
	// 分位（线性插值）：[13480,20000] → P10=14132, 中位=16740, P90=19348。
	if math.Abs(s.P10-14132) > 1 || math.Abs(s.Median-16740) > 1 || math.Abs(s.P90-19348) > 1 {
		t.Fatalf("分位不符: P10=%v Median=%v P90=%v", s.P10, s.Median, s.P90)
	}
	if len(s.Histogram) == 0 {
		t.Fatal("distribution Histogram 为空")
	}
}

// TestSessionCost_EventStoreConcurrentAccess 断言事件存储读写并发安全（-race），且有界（环形不无界增长）。
func TestSessionCost_EventStoreConcurrentAccess(t *testing.T) {
	const capacity = 64
	store := router.NewSessionCostEventStore(capacity)

	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	admin := NewAdmin(db, billing.NewSqlStore(db), nil, nil)
	admin.SetSessionCostEventStore(store)

	const writers, per = 8, 100
	var wg sync.WaitGroup
	for w := 0; w < writers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 0; i < per; i++ {
				store.Append(router.SessionCostEvent{
					SessionID: fmt.Sprintf("s%d", w), Turn: i,
					CrossCacheRead: float64(i), CrossCacheReadKnown: true,
				})
			}
		}(w)
	}

	// 读端与写端并发（-race 校验）：并发调用 /events 与 /distribution。
	stop := make(chan struct{})
	var readerWg sync.WaitGroup
	readerWg.Add(1)
	go func() {
		defer readerWg.Done()
		for {
			select {
			case <-stop:
				return
			default:
				admin.HandleSessionCostEvents(httptest.NewRecorder(),
					httptest.NewRequest(http.MethodGet, "/api/v1/admin/session-cost/events", nil))
				admin.HandleSessionCostDistribution(httptest.NewRecorder(),
					httptest.NewRequest(http.MethodGet, "/api/v1/admin/session-cost/distribution", nil))
			}
		}
	}()

	wg.Wait()
	close(stop)
	readerWg.Wait()

	if got := store.Len(); got != capacity {
		t.Fatalf("有界存储 Len=%d, want %d（写满后覆盖最旧）", got, capacity)
	}
	if snap := store.Snapshot(); len(snap) != capacity {
		t.Fatalf("Snapshot len=%d, want %d", len(snap), capacity)
	}
}
