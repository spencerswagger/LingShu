package channelprotocol

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func cpRow(id int64, name, baseURL, sourceID string, edited bool, status string) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows([]string{"id", "name", "base_url", "protocols", "request_headers",
		"models", "preset_source_id", "edited", "source_status", "created_at", "updated_at"}).
		AddRow(id, name, baseURL, `{}`, `{}`, `{}`, sourceID, edited, status, now, now)
}

const testPackJSON = `{
  "opencode-zen": {
    "id": "opencode-zen",
    "name": "OpenCode Zen",
    "models": {},
    "gateway": {
      "baseUrl": "https://opencode.ai/zen/v1",
      "protocols": { "anthropic-messages": { "path": "/messages" } },
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
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "base_url", "protocols", "request_headers",
			"models", "preset_source_id", "edited", "source_status", "created_at", "updated_at"}))
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO channel_protocols`).
		WillReturnRows(cpRow(1, "OpenCode Zen", "https://opencode.ai/zen/v1", "opencode-zen", false, SourceActive))
	mock.ExpectCommit()

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
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE channel_protocols SET`).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

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
	mock.ExpectBegin()
	mock.ExpectCommit()

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
	mock2.ExpectBegin()
	mock2.ExpectExec(`UPDATE channel_protocols SET`).WillReturnResult(sqlmock.NewResult(0, 1))
	mock2.ExpectCommit()
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
	mock.ExpectBegin()
	// 新增当前 pack 内条目。
	mock.ExpectQuery(`INSERT INTO channel_protocols`).
		WillReturnRows(cpRow(8, "OpenCode Zen", "https://opencode.ai/zen/v1", "opencode-zen", false, SourceActive))
	// 来源已失效 → 标记 STALE。
	mock.ExpectExec(`UPDATE channel_protocols SET source_status=.*`).
		WithArgs(SourceStale, int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

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

// 事务性：写入中途失败 → 整体回滚并返回错误（不再出现「同步半完成」）。
func TestSync_RollsBackOnFailure(t *testing.T) {
	svc, mock := newTestService(t, "http://x")
	mock.ExpectQuery(`SELECT .* FROM channel_protocols WHERE deleted_at IS NULL AND preset_source_id <> '' ORDER BY id ASC`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "base_url", "protocols", "request_headers",
			"models", "preset_source_id", "edited", "source_status", "created_at", "updated_at"}))
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO channel_protocols`).WillReturnError(errors.New("insert boom"))
	mock.ExpectRollback()

	if _, err := svc.Sync(context.Background(), false); err == nil {
		t.Fatalf("insert 失败应返回错误")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// InitialSync 幂等：已同步过（sys_configs 标记存在）→ 跳过，不再拉取/写库。
func TestInitialSync_SkipsWhenAlreadySynced(t *testing.T) {
	svc, mock := newTestService(t, "http://x")
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key=$1`)).
		WithArgs(initialSyncedConfigKey).
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow([]byte("true")))

	if svc.InitialSync(context.Background()) {
		t.Fatalf("已同步时应跳过 InitialSync")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// InitialSync 首次：执行同步并在成功后落「已同步」标记。
func TestInitialSync_MarksWhenFirstRun(t *testing.T) {
	svc, mock := newTestService(t, "http://x")
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT value FROM sys_configs WHERE key=$1`)).
		WithArgs(initialSyncedConfigKey).
		WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`SELECT .* FROM channel_protocols WHERE deleted_at IS NULL AND preset_source_id <> '' ORDER BY id ASC`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "base_url", "protocols", "request_headers",
			"models", "preset_source_id", "edited", "source_status", "created_at", "updated_at"}))
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO channel_protocols`).
		WillReturnRows(cpRow(1, "OpenCode Zen", "https://opencode.ai/zen/v1", "opencode-zen", false, SourceActive))
	mock.ExpectCommit()
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO sys_configs(key, value) VALUES ($1, 'true'::jsonb) ON CONFLICT (key) DO NOTHING`)).
		WithArgs(initialSyncedConfigKey).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if !svc.InitialSync(context.Background()) {
		t.Fatalf("首次启动应执行同步")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ---- 变更刷新钩子（SetChannelRefresher） ----

func TestCreate_InvokesRefresher(t *testing.T) {
	svc, mock := newTestService(t, "")
	var got int64
	svc.SetChannelRefresher(func(_ context.Context, id int64) error { got = id; return nil })

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cpCols + ` FROM channel_protocols WHERE name=$1 AND deleted_at IS NULL`)).
		WithArgs("p1").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`INSERT INTO channel_protocols`).
		WillReturnRows(cpRow(42, "p1", "https://x.example.com", "", true, SourceActive))

	cp, err := svc.Create(context.Background(), ChannelProtocol{Name: "p1", BaseURL: "https://x.example.com"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if got != cp.ID || got != 42 {
		t.Fatalf("期望刷新钩子以协议 ID 回调，got %d (cp.ID=%d)", got, cp.ID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestUpdate_InvokesRefresher(t *testing.T) {
	svc, mock := newTestService(t, "")
	var got int64
	svc.SetChannelRefresher(func(_ context.Context, id int64) error { got = id; return nil })

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cpCols + ` FROM channel_protocols WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(5)).WillReturnRows(cpRow(5, "old", "https://x.example.com", "", true, SourceActive))
	mock.ExpectExec(`UPDATE channel_protocols SET`).WillReturnResult(sqlmock.NewResult(0, 1))

	if _, err := svc.Update(context.Background(), 5, ChannelProtocol{Name: "new", BaseURL: "https://x.example.com"}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got != 5 {
		t.Fatalf("期望刷新钩子以 5 回调，got %d", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestDelete_InvokesRefresher(t *testing.T) {
	svc, mock := newTestService(t, "")
	var got int64
	svc.SetChannelRefresher(func(_ context.Context, id int64) error { got = id; return nil })

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cpCols + ` FROM channel_protocols WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(8)).WillReturnRows(cpRow(8, "p", "https://x.example.com", "", true, SourceActive))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM channels WHERE channel_protocol_id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(8)).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_protocols SET deleted_at=now(), updated_at=now() WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(8)).WillReturnResult(sqlmock.NewResult(0, 1))

	if err := svc.Delete(context.Background(), 8); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if got != 8 {
		t.Fatalf("期望刷新钩子以 8 回调，got %d", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestSync_InvokesRefresher(t *testing.T) {
	svc, mock := newTestService(t, "http://x")
	var got []int64
	svc.SetChannelRefresher(func(_ context.Context, id int64) error { got = append(got, id); return nil })

	mock.ExpectQuery(`SELECT .* FROM channel_protocols WHERE deleted_at IS NULL AND preset_source_id <> '' ORDER BY id ASC`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "base_url", "protocols", "request_headers",
			"models", "preset_source_id", "edited", "source_status", "created_at", "updated_at"}))
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO channel_protocols`).
		WillReturnRows(cpRow(11, "OpenCode Zen", "https://opencode.ai/zen/v1", "opencode-zen", false, SourceActive))
	mock.ExpectCommit()

	if _, err := svc.Sync(context.Background(), false); err != nil {
		t.Fatalf("sync: %v", err)
	}
	if len(got) != 1 || got[0] != 11 {
		t.Fatalf("期望同步成功后以变更协议 11 回调刷新，got %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ---- 协议模型拉取（/models） ----

func TestPullModels_Success(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Errorf("path want /models, got %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer sk-temp" {
			t.Errorf("auth want Bearer sk-temp, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"qwen-max","object":"model","owned_by":"aliyun"}]}`))
	}))
	defer up.Close()

	svc, mock := newTestService(t, "")
	svc.SetHTTPClient(up.Client())
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cpCols + ` FROM channel_protocols WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(9)).WillReturnRows(cpRow(9, "P", up.URL, "", true, SourceActive))

	list, err := svc.PullModels(context.Background(), 9, "sk-temp")
	if err != nil {
		t.Fatalf("pull models: %v", err)
	}
	if len(list) != 1 || list[0].ID != "qwen-max" || list[0].Object != "model" || list[0].OwnedBy != "aliyun" {
		t.Fatalf("unexpected list: %+v", list)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestPullModels_RejectsEmptyKey(t *testing.T) {
	svc, _ := newTestService(t, "")
	_, err := svc.PullModels(context.Background(), 9, "  ")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("空 ApiKey 应返回 400，got %v", err)
	}
}

// ---- 渠道协议导入导出（Export / Import） ----

// Export：预设来源用 preset_source_id 作键、自建用 name 作键，并承载编辑标记与来源状态。
func TestExport(t *testing.T) {
	svc, mock := newTestService(t, "")
	now := time.Now()
	mock.ExpectQuery(`SELECT .* FROM channel_protocols WHERE deleted_at IS NULL ORDER BY id ASC`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "base_url", "protocols", "request_headers",
			"models", "preset_source_id", "edited", "source_status", "created_at", "updated_at"}).
			AddRow(int64(1), "Anthropic", "https://api.anthropic.com/v1", `{}`, `{}`, `{}`, "anthropic", false, SourceActive, now, now).
			AddRow(int64(2), "my-proto", "https://my.example/v1", `{}`, `{}`, `{}`, "", true, SourceActive, now, now))

	pack, err := svc.Export(context.Background())
	if err != nil {
		t.Fatalf("export: %v", err)
	}
	if len(pack) != 2 {
		t.Fatalf("期望导出 2 个 provider, got %d: %v", len(pack), pack)
	}
	preset, ok := pack["anthropic"]
	if !ok {
		t.Fatalf("预设应以 preset_source_id 作键, got %v", pack)
	}
	if preset.ID != "anthropic" || preset.Name != "Anthropic" {
		t.Fatalf("预设条目 ID/Name 不符, got %+v", preset)
	}
	if preset.Gateway.Edited || preset.Gateway.SourceStatus != SourceActive {
		t.Fatalf("预设条目标记应与库中一致(edited=false/ACTIVE), got %+v", preset.Gateway)
	}
	own, ok := pack["my-proto"]
	if !ok {
		t.Fatalf("自建应以 name 作键, got %v", pack)
	}
	if own.ID != "" || own.Name != "my-proto" {
		t.Fatalf("自建条目 ID 应为空、Name 应为 my-proto, got %+v", own)
	}
	if !own.Gateway.Edited || own.Gateway.SourceStatus != SourceActive {
		t.Fatalf("自建条目标记应与库中一致(edited=true/ACTIVE), got %+v", own.Gateway)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Import：name 已存在 → 跳过（Skipped++），不写库，仅 Begin/Commit。
func TestImport_SkipsExisting(t *testing.T) {
	svc, mock := newTestService(t, "")
	mock.ExpectQuery(`SELECT .* FROM channel_protocols WHERE deleted_at IS NULL ORDER BY id ASC`).
		WillReturnRows(cpRow(1, "dup-proto", "https://dup.example/v1", "", true, SourceActive))
	mock.ExpectBegin()
	mock.ExpectCommit()

	pack := Pack{
		"dup-proto": Provider{
			ID:      "dup-proto",
			Name:    "dup-proto",
			Gateway: GatewayBlock{BaseURL: "https://x.example/v1"},
		},
	}
	res, err := svc.Import(context.Background(), pack)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Skipped != 1 || res.Created != 0 {
		t.Fatalf("同名协议应跳过, got %+v", res)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Import：新协议 → 单事务内插入（Created++），无告警。
func TestImport_Creates(t *testing.T) {
	svc, mock := newTestService(t, "")
	mock.ExpectQuery(`SELECT .* FROM channel_protocols WHERE deleted_at IS NULL ORDER BY id ASC`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "base_url", "protocols", "request_headers",
			"models", "preset_source_id", "edited", "source_status", "created_at", "updated_at"}))
	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO channel_protocols`).
		WillReturnRows(cpRow(101, "new-proto", "https://x.example/v1", "new-proto", false, SourceActive))
	mock.ExpectCommit()

	pack := Pack{
		"new-proto": Provider{
			ID:      "new-proto",
			Name:    "new-proto",
			Gateway: GatewayBlock{BaseURL: "https://x.example/v1"},
		},
	}
	res, err := svc.Import(context.Background(), pack)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Created != 1 || res.Skipped != 0 {
		t.Fatalf("应新增 1 条且无跳过, got %+v", res)
	}
	if len(res.Warnings) != 0 {
		t.Fatalf("不应有告警, got %v", res.Warnings)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Import：条目携带非法 sourceStatus → 校验拦截记为 Warnings 并跳过，事务仍提交（不整批回滚）。
func TestImport_WarnsOnInvalidSourceStatus(t *testing.T) {
	svc, mock := newTestService(t, "")
	mock.ExpectQuery(`SELECT .* FROM channel_protocols WHERE deleted_at IS NULL ORDER BY id ASC`).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "base_url", "protocols", "request_headers",
			"models", "preset_source_id", "edited", "source_status", "created_at", "updated_at"}))
	mock.ExpectBegin()
	mock.ExpectCommit()

	pack := Pack{
		"bad-status": Provider{
			Name:    "bad-status",
			Gateway: GatewayBlock{BaseURL: "https://x.example/v1", SourceStatus: "BOGUS"},
		},
	}
	res, err := svc.Import(context.Background(), pack)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if res.Created != 0 || res.Skipped != 0 {
		t.Fatalf("非法 sourceStatus 应跳过且不计入 Created/Skipped, got %+v", res)
	}
	if len(res.Warnings) != 1 {
		t.Fatalf("应产生 1 条告警, got %v", res.Warnings)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
