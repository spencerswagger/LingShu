package channel

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/resp"
)

func cmTestCols() []string {
	return []string{"id", "channel_id", "internal_model_id", "external_model_id", "cost_rates",
		"time_config", "context_tiers", "state", "rate_limit", "health_probe", "reliability",
		"created_at", "updated_at"}
}

func cmRow(m *ChannelModel, costJSON string) *sqlmock.Rows {
	return sqlmock.NewRows(cmTestCols()).AddRow(m.ID, m.ChannelID, m.InternalModelID, m.ExternalModelID,
		costJSON, nil, nil, string(m.State), `{}`, `{}`, `{}`, m.CreatedAt, m.UpdatedAt)
}

func mockCMStore(t *testing.T) (*channelModelStore, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewChannelModelStore(db), mock
}

func TestChannelModelStore_GetByChannelAndInternal(t *testing.T) {
	s, mock := mockCMStore(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT `+cmCols+` FROM channel_models WHERE channel_id=$1 AND internal_model_id=$2 AND deleted_at IS NULL ORDER BY id ASC LIMIT 1`)).
		WithArgs(int64(1), "qwen-max").
		WillReturnRows(cmRow(&ChannelModel{ID: 1, ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2, CreatedAt: now, UpdatedAt: now}, `{"input":1}`))

	m, err := s.GetByChannelAndInternal(context.Background(), 1, "qwen-max")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if m.InternalModelID != "qwen-max" || m.ExternalModelID != 2 {
		t.Fatalf("unexpected: %+v", m)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestChannelModelStore_ListByExternal(t *testing.T) {
	s, mock := mockCMStore(t)
	now := time.Now()
	q := `SELECT ` + cmCols + ` FROM channel_models WHERE external_model_id = $1 AND state != 'DISABLED' AND deleted_at IS NULL ORDER BY channel_id ASC, id ASC`
	mock.ExpectQuery(regexp.QuoteMeta(q)).
		WithArgs(int64(2)).
		WillReturnRows(cmRow(&ChannelModel{ID: 1, ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2, CreatedAt: now, UpdatedAt: now}, `{}`).
			AddRow(2, 3, "qwen-plus", 2, `{}`, nil, nil, "NORMAL", `{}`, `{}`, `{}`, now, now))

	list, err := s.ListByExternal(context.Background(), 2)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("expected 2, got %d", len(list))
	}
}

func TestChannelModelStore_Insert_Conflict(t *testing.T) {
	s, mock := mockCMStore(t)
	rates := billing.Rates{"input": 0.8, "output": 1.6, "cache_read": 0.05, "cache_write": 0.2, "reasoning": 0.8}
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, cost_rates, time_config, context_tiers, state, rate_limit, health_probe, reliability) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+cmCols)).
		WithArgs(sqlmock.AnyArg(), int64(1), "qwen-max", int64(2), sqlmock.AnyArg(), nil, nil, "NORMAL", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(&pgconn.PgError{Code: "23505"})

	_, err := s.Insert(context.Background(), &ChannelModel{
		ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2,
		CostRates: rates,
	})
	if err != ErrChannelModelExists {
		t.Fatalf("expected ErrChannelModelExists, got %v", err)
	}
}

// ---- PullModels ----

func TestService_PullModels_HTTPServer(t *testing.T) {
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path want /models, got %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-plain" {
			t.Errorf("auth want Bearer sk-plain, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen-max","object":"model","owned_by":"aliyun"},{"id":"qwen-plus","object":"model","owned_by":"aliyun"}]}`))
	}))
	defer mock.Close()

	mgr := NewManager(nil, nil, make([]byte, 16), nil, nil, nil)
	rt := &RuntimeChannel{Channel: Channel{
		ID: 1, Name: "p", Protocol: ProtocolOpenAICompat, BaseURL: mock.URL,
	}}
	// 密钥 1 已 DISABLED（应被跳过）→ 凭据取自第一个可用密钥（密钥 2，NORMAL）。
	rt.Keys = []*KeyRuntime{
		{Key: ChannelKey{ID: 1, ChannelID: 1, Name: "k1", State: StateDisabled}, CredentialPlain: "sk-disabled"},
		{Key: ChannelKey{ID: 2, ChannelID: 1, Name: "k2", State: StateNormal}, CredentialPlain: "sk-plain"},
	}
	mgr.mu.Lock()
	mgr.rt[1] = rt
	mgr.mu.Unlock()

	svc := NewService(nil)
	svc.SetManager(mgr)

	list, err := svc.PullModels(context.Background(), 1, mock.Client())
	if err != nil {
		t.Fatalf("pull models: %v", err)
	}
	if len(list) != 2 || list[0].ID != "qwen-max" {
		t.Fatalf("unexpected list: %+v", list)
	}
}

func TestService_PullModels_NoAvailableKey(t *testing.T) {
	mgr := NewManager(nil, nil, make([]byte, 16), nil, nil, nil)
	rt := &RuntimeChannel{Channel: Channel{ID: 1, Name: "p", Protocol: ProtocolOpenAICompat, BaseURL: "http://x"}}
	mgr.mu.Lock()
	mgr.rt[1] = rt
	mgr.mu.Unlock()

	svc := NewService(nil)
	svc.SetManager(mgr)

	_, err := svc.PullModels(context.Background(), 1, nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeInternalError {
		t.Fatalf("期望无可用密钥 50001，got %v", err)
	}
	if apiErr.Message != "暂无可用渠道密钥" {
		t.Fatalf("期望文案「暂无可用渠道密钥」，got %q", apiErr.Message)
	}
}
