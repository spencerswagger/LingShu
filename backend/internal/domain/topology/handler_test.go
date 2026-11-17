package topology

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func newTestHandler(st *fakeStore, src *fakeSource) *Handler {
	sy := NewSyncer(SyncerOptions{Source: src, Store: st, Logger: testLogger()})
	return NewHandler(st, sy)
}

// T1：字段名必须与 View 的 InternalModelView / TopologySync 逐字对齐（PascalCase）。
func TestHandler_HandleGet_Shape(t *testing.T) {
	seen := time.Date(2026, 11, 16, 9, 30, 0, 0, time.UTC)
	lastOK := time.Date(2026, 11, 16, 9, 0, 0, 0, time.UTC)
	st := &fakeStore{
		nodes: []Node{{
			Ref: "acme/gpt-x", ProviderRef: "acme", Kind: KindDirect, DisplayName: "gpt-x",
			ContextLen: 128000, Caps: json.RawMessage(`{"tools":true}`), ListPrice: json.RawMessage(`{"input":1}`),
			LastSeenAt: seen, Stale: false,
		}},
		links:    []Link{{InternalRef: "acme/gpt-x", ExternalModelID: 300, PosX: 240, PosY: 190}},
		counts:   Counts{Stale: 1, UnboundExternal: 2},
		lastSeen: seen, hasSeen: true,
		state: SyncState{LastRunAt: seen, LastSuccessAt: lastOK, State: StateOK, Created: 2, Stale: 1},
	}
	h := newTestHandler(st, &fakeSource{})

	rec := httptest.NewRecorder()
	h.HandleGet(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/topology", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d，body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code int
		Data struct {
			Items []struct {
				Ref         string
				ProviderRef string
				Kind        string
				ContextLen  int64
				LastSeenAt  string
				Stale       bool
				Caps        map[string]any
			}
			Links []struct {
				InternalRef     string
				ExternalModelID string
				PosX            float64
			}
			StaleCount      int
			UnboundExternal int
			SyncedAt        string
			Sync            struct {
				LastSuccessAt string
				LastRunAt     string
				State         string
				Message       string
				Created       int
				Stale         int
			}
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	d := body.Data
	if len(d.Items) != 1 || d.Items[0].Ref != "acme/gpt-x" || d.Items[0].ContextLen != 128000 {
		t.Fatalf("Items = %+v", d.Items)
	}
	if d.Items[0].Caps["tools"] != true {
		t.Errorf("Caps 应原样下发：%+v", d.Items[0].Caps)
	}
	if len(d.Links) != 1 || d.Links[0].ExternalModelID != "300" {
		t.Fatalf("Links = %+v（雪花必须以字符串下发）", d.Links)
	}
	if d.StaleCount != 1 || d.UnboundExternal != 2 {
		t.Errorf("计数 = %d/%d", d.StaleCount, d.UnboundExternal)
	}
	if d.SyncedAt != "2026-11-16T09:30:00Z" {
		t.Errorf("SyncedAt = %q", d.SyncedAt)
	}
	if d.Sync.State != StateOK || d.Sync.Created != 2 || d.Sync.LastSuccessAt != "2026-11-16T09:00:00Z" {
		t.Errorf("Sync = %+v", d.Sync)
	}
}

// 从未同步：State=never（不是 failed），所有时间为空串（不许出现 0001-01-01）。
func TestHandler_HandleGet_NeverSynced(t *testing.T) {
	st := &fakeStore{nodes: []Node{}, links: []Link{}}
	h := newTestHandler(st, &fakeSource{})

	rec := httptest.NewRecorder()
	h.HandleGet(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/topology", nil))
	var body struct {
		Data struct {
			Items    []any
			Links    []any
			SyncedAt string
			Sync     struct{ State, LastSuccessAt, LastRunAt string }
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if body.Data.Sync.State != StateNever {
		t.Fatalf("State = %q，期望 never", body.Data.Sync.State)
	}
	if body.Data.SyncedAt != "" || body.Data.Sync.LastSuccessAt != "" || body.Data.Sync.LastRunAt != "" {
		t.Fatalf("空时间必须下发空串：%+v", body.Data)
	}
}

// T3 成功：200 + Fetched/Created/Stale + State=ok。
func TestHandler_HandleSync_OK(t *testing.T) {
	st := &fakeStore{replaceFn: func(nodes []Node, runAt time.Time) (int, int, error) {
		return len(nodes), 2, nil
	}}
	src := &fakeSource{nodes: []Node{{Ref: "a"}, {Ref: "b"}, {Ref: "c"}}}
	h := newTestHandler(st, src)

	rec := httptest.NewRecorder()
	h.HandleSync(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/topology/sync", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d，body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			Fetched, Created, Stale int
			State, Message          string
		}
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if body.Data.Fetched != 3 || body.Data.Created != 3 || body.Data.Stale != 2 || body.Data.State != StateOK {
		t.Fatalf("结果 = %+v", body.Data)
	}
}

// T3 失败（已跑过）：200 + State=failed + 原因 —— 页面据此保留旧快照并显示原因（Spec P02 验收 3）。
func TestHandler_HandleSync_FailureStillOKWithMessage(t *testing.T) {
	st := &fakeStore{}
	h := newTestHandler(st, &fakeSource{err: errors.New("网关 502")})

	rec := httptest.NewRecorder()
	h.HandleSync(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/topology/sync", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d，期望 200（业务失败也要给出可展示原因），body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct{ State, Message string }
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析响应失败：%v", err)
	}
	if body.Data.State != StateFailed || body.Data.Message != "网关 502" {
		t.Fatalf("结果 = %+v", body.Data)
	}
}

// T3 未配源：503 + 明确原因（这是运维要动的事，不能报成「同步完成 0 条」）。
func TestHandler_HandleSync_NotConfigured(t *testing.T) {
	st := &fakeStore{}
	h := newTestHandler(st, &fakeSource{err: ErrSourceNotConfigured})

	rec := httptest.NewRecorder()
	h.HandleSync(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/topology/sync", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d，期望 503", rec.Code)
	}
	got := rec.Body.String()
	if !strings.Contains(got, "topology_source_not_configured") || !strings.Contains(got, "未配置") {
		t.Fatalf("响应缺少原因：%s", got)
	}
}
