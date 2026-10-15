package channelprotocol

import (
	"context"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func cpRow(id int64, name, baseURL, sourceID string, edited bool, status string) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows([]string{"id", "name", "base_url", "protocols", "auth", "request_headers",
		"models", "preset_source_id", "edited", "source_status", "created_at", "updated_at"}).
		AddRow(id, name, baseURL, `{}`, `{}`, `{}`, `{}`, sourceID, edited, status, now, now)
}

const testPackJSON = `{
  "opencode-zen": {
    "id": "opencode-zen",
    "name": "OpenCode Zen",
    "models": {},
    "gateway": {
      "baseUrl": "https://opencode.ai/zen/v1",
      "protocols": { "anthropic-messages": { "path": "/messages" } },
      "auth": { "header": "Authorization", "scheme": "bearer" },
      "requestHeaders": { "passthroughMode": "whitelist", "whitelist": ["x-opencode-session"] }
    }
  }
}`

func newTestService(t *testing.T, url string) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	svc := NewService(NewStore(db))
	svc.SetLogger(nil)
	if url != "" {
		svc.SetPackURL(url)
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(testPackJSON))
		}))
		t.Cleanup(srv.Close)
		svc.SetPackURL(srv.URL)
		svc.SetHTTPClient(srv.Client())
	}
	return svc, mock
}

// AC-12：预设地址未设置 → 不拉取、不报错、结果为空。
func TestSync_NoURL(t *testing.T) {
	svc, _ := newTestService(t, "")
	res, err := svc.Sync(context.Background(), false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Applied != 0 || len(res.Pending) != 0 {
		t.Fatalf("未配置地址应返回空结果, got %+v", res)
	}
	if svc.InitialSync(context.Background()) {
		t.Fatalf("未配置地址时 InitialSync 不应执行拉取")
	}
}

// AC-12：地址已配置且拉取成功 → 新增物化渠道协议（Insert）。
func TestSync_InsertsNewPreset(t *testing.T) {
	svc, mock := newTestService(t, "http://x")
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cpCols + ` FROM channel_protocols WHERE deleted_at IS NULL AND preset_source_id <> '' ORDER BY id ASC`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "base_url", "protocols", "auth", "request_headers",
			"models", "preset_source_id", "edited", "source_status", "created_at", "updated_at"}))
	mock.ExpectQuery(`INSERT INTO channel_protocols`).
		WillReturnRows(cpRow(1, "OpenCode Zen", "https://opencode.ai/zen/v1", "opencode-zen", false, SourceActive))

	res, err := svc.Sync(context.Background(), false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Applied != 1 {
		t.Fatalf("应新增 1 条, got %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// AC-13：未编辑条目 → 直接覆盖（Applied），编辑标记复位、来源 ACTIVE。
func TestSync_OverwritesUnedited(t *testing.T) {
	svc, mock := newTestService(t, "http://x")
	mock.ExpectQuery(`SELECT .* FROM channel_protocols WHERE deleted_at IS NULL AND preset_source_id <> '' ORDER BY id ASC`).
		WillReturnRows(cpRow(5, "Old Name", "https://old.example.com", "opencode-zen", false, SourceActive))
	mock.ExpectExec(`UPDATE channel_protocols SET`).
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := svc.Sync(context.Background(), false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Applied != 1 || len(res.Pending) != 0 {
		t.Fatalf("未编辑条目应直接覆盖, got %+v", res)
	}
}

// AC-14：已编辑条目 confirm=false → 进入 Pending（展示差异）不覆盖；confirm=true → 覆盖。
func TestSync_EditedRequiresConfirm(t *testing.T) {
	svc, mock := newTestService(t, "http://x")
	mock.ExpectQuery(`SELECT .* FROM channel_protocols WHERE deleted_at IS NULL AND preset_source_id <> '' ORDER BY id ASC`).
		WillReturnRows(cpRow(6, "Edited Name", "https://edited.example.com", "opencode-zen", true, SourceActive))

	res, err := svc.Sync(context.Background(), false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	if res.Applied != 0 || len(res.Pending) != 1 || !res.Pending[0].Edited {
		t.Fatalf("已编辑条目未确认时应进入 Pending, got %+v", res)
	}
	if res.Pending[0].Diff == "" || res.Pending[0].Diff == "无差异" {
		t.Fatalf("Pending 应包含差异文本, got %q", res.Pending[0].Diff)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("confirm=false 不应写库, expectations: %v", err)
	}

	// confirm=true：覆盖。
	svc2, mock2 := newTestService(t, "http://x")
	mock2.ExpectQuery(`SELECT .* FROM channel_protocols WHERE deleted_at IS NULL AND preset_source_id <> '' ORDER BY id ASC`).
		WillReturnRows(cpRow(6, "Edited Name", "https://edited.example.com", "opencode-zen", true, SourceActive))
	mock2.ExpectExec(`UPDATE channel_protocols SET`).WillReturnResult(sqlmock.NewResult(0, 1))
	res2, err := svc2.Sync(context.Background(), true)
	if err != nil {
		t.Fatalf("sync confirm: %v", err)
	}
	if res2.Applied != 1 || len(res2.Pending) != 0 {
		t.Fatalf("confirm=true 应覆盖, got %+v", res2)
	}
}

// AC-15：预设条目删除/改名 → 标记 STALE，协议不删除。
func TestSync_MarksStale(t *testing.T) {
	svc, mock := newTestService(t, "http://x")
	mock.ExpectQuery(`SELECT .* FROM channel_protocols WHERE deleted_at IS NULL AND preset_source_id <> '' ORDER BY id ASC`).
		WillReturnRows(cpRow(7, "Removed Provider", "https://removed.example.com", "removed-provider", false, SourceActive))
	// 新增当前 pack 内条目。
	mock.ExpectQuery(`INSERT INTO channel_protocols`).
		WillReturnRows(cpRow(8, "OpenCode Zen", "https://opencode.ai/zen/v1", "opencode-zen", false, SourceActive))
	// 来源已失效 → 标记 STALE。
	mock.ExpectExec(`UPDATE channel_protocols SET source_status=.*`).
		WithArgs(SourceStale, int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	res, err := svc.Sync(context.Background(), false)
	if err != nil {
		t.Fatalf("sync: %v", err)
	}
	stale := false
	for _, p := range res.Pending {
		if p.Stale {
			stale = true
		}
	}
	if !stale {
		t.Fatalf("来源失效条目应在 Pending 中标记 Stale, got %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
