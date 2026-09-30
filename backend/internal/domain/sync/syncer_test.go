package sync

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// fakeSourceRaw 启动一个 httptest 价格源，返回给定的 JSON 原始字符串（models.dev 嵌套结构）。
func fakeSourceRaw(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// pricesToPayload 把 []ExternalPrice 构造为 models.dev 嵌套结构 JSON（input/output 落在 cost 字段）。
// 供测试把内存 prices 直接转为源返回体，保持对既有断言无侵入。
func pricesToPayload(prices []ExternalPrice) string {
	providers := make(map[string]interface{})
	for _, p := range prices {
		in, out := p.Pricing.Prompt, p.Pricing.Completion
		prov := p.Provider
		if prov == "" {
			prov = "mock"
		}
		pm, ok := providers[prov].(map[string]interface{})
		if !ok {
			pm = map[string]interface{}{"models": map[string]interface{}{}}
			providers[prov] = pm
		}
		pm["models"].(map[string]interface{})[p.ID] = map[string]interface{}{
			"id": p.ID,
			"cost": map[string]interface{}{
				"input":  in,
				"output": out,
			},
		}
	}
	raw, _ := json.Marshal(providers)
	return string(raw)
}

// newTestSyncer 基于 sqlmock 构建 Syncer 与关联文件，返回 (syncer, mock)。
func newTestSyncer(t *testing.T, sourceURL string) (*Syncer, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	store := NewStore(db)
	syncer := NewSyncer(SyncerConfig{
		Store:     store,
		SourceURL: sourceURL,
		Interval:  time.Hour,
		Now:       func() time.Time { return time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC) },
		Client:    http.DefaultClient,
	})
	return syncer, mock
}

func TestSyncOnce_FirstSync_NoAlert(t *testing.T) {
	prices := []ExternalPrice{
		{ID: "gpt-4o", Provider: "openai", Pricing: Pricing{Prompt: 2.5, Completion: 10.0}},
		{ID: "gpt-4o-mini", Provider: "openai", Pricing: Pricing{Prompt: 0.15, Completion: 0.6}},
	}
	srv := fakeSourceRaw(t, pricesToPayload(prices))
	syncer, mock := newTestSyncer(t, srv.URL)

	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	mock.ExpectQuery(`SELECT id, external_model_id`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "external_model_id", "local_model_name",
			"alert_on_change", "last_synced_at", "prompt_price", "completion_price", "created_at"}).
			AddRow(1, "openai/gpt-4o", "gpt-4o", true, nil, nil, nil, created))
	// 首同步建立基线（预期一次 UPDATE，无告警 INSERT）。
	mock.ExpectExec(`UPDATE watchlist_items SET prompt_price`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	summary, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	if want := "产生 0 条告警"; !contains(summary, want) {
		t.Fatalf("summary %q 应包含 %q", summary, want)
	}
}

func TestSyncOnce_PriceChange_GeneratesAlert(t *testing.T) {
	prices := []ExternalPrice{
		{ID: "gpt-4o", Provider: "openai", Pricing: Pricing{Prompt: 3.0, Completion: 11.0}},
	}
	srv := fakeSourceRaw(t, pricesToPayload(prices))
	syncer, mock := newTestSyncer(t, srv.URL)

	lastSynced := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	baselinePrompt, baselineCompletion := 2.5, 10.0

	mock.ExpectQuery(`SELECT id, external_model_id`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "external_model_id", "local_model_name",
			"alert_on_change", "last_synced_at", "prompt_price", "completion_price", "created_at"}).
			AddRow(1, "openai/gpt-4o", "gpt-4o", true, lastSynced, baselinePrompt, baselineCompletion, created))
	// 变化 → 插入 pending 告警。
	mock.ExpectQuery(`INSERT INTO price_change_alerts`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "admin_id", "watchlist_item_id",
			"external_model_id", "local_model_name", "changes", "status", "detected_at", "resolved_at"}).
			AddRow(1, nil, 1, "openai/gpt-4o", "gpt-4o",
				[]byte(`{"input":{"old":2.5,"new":3.0,"change_pct":20},"output":{"old":10,"new":11,"change_pct":10}}`),
				"pending", lastSynced.Add(24*time.Hour), nil))
	// 更新基线到新价。
	mock.ExpectExec(`UPDATE watchlist_items SET prompt_price`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	summary, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	if want := "产生 1 条告警"; !contains(summary, want) {
		t.Fatalf("summary %q 应包含 %q", summary, want)
	}
}

func TestSyncOnce_WatchlistMissingInSource_NoAlert(t *testing.T) {
	prices := []ExternalPrice{{ID: "gpt-4o-mini", Provider: "openai", Pricing: Pricing{Prompt: 0.15, Completion: 0.6}}}
	srv := fakeSourceRaw(t, pricesToPayload(prices))
	syncer, mock := newTestSyncer(t, srv.URL)

	lastSynced := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	created := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	// watchlist 项在价格源中不存在 → 跳过，不应产生任何 DB 副作用。
	mock.ExpectQuery(`SELECT id, external_model_id`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "external_model_id", "local_model_name",
			"alert_on_change", "last_synced_at", "prompt_price", "completion_price", "created_at"}).
			AddRow(1, "misc/not-in-source", "other", true, lastSynced, 1.0, 2.0, created))

	summary, err := syncer.SyncOnce(context.Background())
	if err != nil {
		t.Fatalf("SyncOnce: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
	if want := "产生 0 条告警"; !contains(summary, want) {
		t.Fatalf("summary %q 应包含 %q", summary, want)
	}
}

func TestResolveAlert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock new: %v", err)
	}
	store := NewStore(db)
	mock.ExpectExec(`UPDATE price_change_alerts SET status`).
		WithArgs("resolved", int64(3), int64(9)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := store.ResolveAlert(context.Background(), 9, 3); err != nil {
		t.Fatalf("ResolveAlert: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
