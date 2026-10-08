package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/pkg/crypto"
)

// newHandlerWithKeys 装配：Manager（渠道 1 + 密钥 11/12 运行时）+ KeyService（同一 sqlmock db）+ Handler(keySvc/mgr)。
func newHandlerWithKeys(t *testing.T) (*Handler, *Manager, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }

	ch := &Channel{
		ID: 1, Name: "c1", Protocol: ProtocolOpenAICompat,
		BaseURL: "https://api.example.com", State: StateNormal,
		RateLimit:   RateLimitConfig{MaxConcurrent: 8},
		HealthProbe: HealthProbeConfig{FailThreshold: 1, RecoveryThreshold: 2},
		Reliability: ReliabilityConfig{WindowSeconds: 60, MinSamples: 10},
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE deleted_at IS NULL ORDER BY priority DESC, id ASC`)).
		WillReturnRows(channelRow(ch))

	encA, err := crypto.SM4Encrypt(testSM4Key, []byte(testKeyACred))
	if err != nil {
		t.Fatalf("encrypt key-a: %v", err)
	}
	encB, err := crypto.SM4Encrypt(testSM4Key, []byte(testKeyBCred))
	if err != nil {
		t.Fatalf("encrypt key-b: %v", err)
	}
	keyRows := sqlmock.NewRows([]string{"id", "channel_id", "name", "credential_enc", "state", "last_err", "deleted_at", "created_at", "updated_at"}).
		AddRow(int64(11), int64(1), "key-a", encA, string(StateNormal), "", nil, now, now).
		AddRow(int64(12), int64(1), "key-b", encB, string(StateNormal), "", nil, now, now)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE deleted_at IS NULL ORDER BY channel_id, id`)).
		WithArgs().WillReturnRows(keyRows)

	mgr := NewManager(NewStore(db), nil, testSM4Key, nil, nil, nowFn, NewKeyStore(db))
	if err := mgr.SyncFromDB(context.Background()); err != nil {
		t.Fatalf("SyncFromDB: %v", err)
	}
	keySvc := NewKeyService(NewKeyStore(db), testSM4Key)
	h := NewHandler(nil, nil)
	h.SetKeyDeps(keySvc, mgr)
	return h, mgr, mock
}

// keysListResponse 列表端点的响应结构（Data 为数组）。
type keysListResponse struct {
	Code int              `json:"code"`
	Data []viewChannelKey `json:"data"`
}

// keysItemResponse 单对象端点（创建/更新/切状态）的响应结构（Data 为对象）。
type keysItemResponse struct {
	Code int            `json:"code"`
	Data viewChannelKey `json:"data"`
}

// TestHandler_HandleListKeys 断言列表 = keySvc.List + manager.KeyViews 聚合
// （尾号只出 6 位不落明文/密文、状态取运行时、active_sessions=0）。
func TestHandler_HandleListKeys(t *testing.T) {
	h, _, mock := newHandlerWithKeys(t)
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE channel_id=$1 AND deleted_at IS NULL ORDER BY id`)).
		WithArgs(int64(1)).
		WillReturnRows(channelKeyRow(&ChannelKey{ID: 11, ChannelID: 1, Name: "key-a", CredentialEnc: "enc-a", State: StateNormal, CreatedAt: now, UpdatedAt: now}).AddRow(
			int64(12), int64(1), "key-b", "enc-b", string(StateNormal), "", nil, now, now,
		))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/channels/1/keys", nil)
	req.SetPathValue("id", "1")
	h.HandleListKeys(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body keysListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data) != 2 {
		t.Fatalf("list len=%d, want 2; body=%s", len(body.Data), rec.Body.String())
	}
	first := body.Data[0]
	if first.ID != 11 || first.Name != "key-a" || first.State != StateNormal || first.ChannelID != 1 {
		t.Fatalf("item1=%+v", first)
	}
	if first.CredentialTail != "123456" {
		t.Fatalf("item1 tail want 123456, got %q", first.CredentialTail)
	}
	if first.ActiveSessions != 0 {
		t.Fatalf("active_sessions 本任务应输出 0, got %d", first.ActiveSessions)
	}
	second := body.Data[1]
	if second.ID != 12 || second.CredentialTail != "543210" {
		t.Fatalf("item2=%+v", second)
	}
	if strings.Contains(first.CredentialTail+second.CredentialTail, testKeyACred) || strings.Contains(first.CredentialTail+second.CredentialTail, testKeyBCred) {
		t.Fatalf("响应泄漏了密钥明文: %q/%q", first.CredentialTail, second.CredentialTail)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestHandler_HandleCreateKey 断言创建成功后 mgr.UpsertKey 被调用（运行时出现新密钥）。
func TestHandler_HandleCreateKey(t *testing.T) {
	h, mgr, mock := newHandlerWithKeys(t)
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	encC, err := crypto.SM4Encrypt(testSM4Key, []byte("sk-live-secret"))
	if err != nil {
		t.Fatalf("encrypt key-c: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO channel_keys(id, channel_id, name, credential_enc) VALUES($1,$2,$3,$4) RETURNING id`)).
		WithArgs(sqlmock.AnyArg(), int64(1), "密钥C", credentialEncMatcher{key: testSM4Key, want: "sk-live-secret"}).
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(int64(13)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(13)).
		WillReturnRows(channelKeyRow(&ChannelKey{ID: 13, ChannelID: 1, Name: "密钥C", CredentialEnc: encC, State: StateNormal, CreatedAt: now, UpdatedAt: now}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/channels/1/keys", strings.NewReader(`{"Name":"密钥C","Credential":"sk-live-secret"}`))
	req.SetPathValue("id", "1")
	h.HandleCreateKey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body keysItemResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	item := body.Data
	if item.ID != 13 || item.Name != "密钥C" || item.State != StateNormal {
		t.Fatalf("created item=%+v", item)
	}
	if item.CredentialTail != "secret" {
		t.Fatalf("created tail want secret, got %q", item.CredentialTail)
	}
	// 断言 Create 后被同步进运行时。
	kr, ok := mgr.GetKeyRuntime(13)
	if !ok {
		t.Fatalf("UpsertKey 未执行：运行时缺少密钥 13")
	}
	if kr.Key.Name != "密钥C" || kr.Machine.State() != StateNormal {
		t.Fatalf("runtime key 13=%+v", kr.Key)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestHandler_HandleUpdateKey 断言更新成功后 mgr.UpsertKey 以最新记录重建运行时。
func TestHandler_HandleUpdateKey(t *testing.T) {
	h, mgr, mock := newHandlerWithKeys(t)
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	encA, err := crypto.SM4Encrypt(testSM4Key, []byte(testKeyACred))
	if err != nil {
		t.Fatalf("encrypt key-a: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(11)).
		WillReturnRows(channelKeyRow(&ChannelKey{ID: 11, ChannelID: 1, Name: "key-a", CredentialEnc: encA, State: StateNormal, CreatedAt: now, UpdatedAt: now}))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET name=$1, updated_at=now() WHERE id=$2`)).
		WithArgs("改名", int64(11)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(11)).
		WillReturnRows(channelKeyRow(&ChannelKey{ID: 11, ChannelID: 1, Name: "改名", CredentialEnc: encA, State: StateNormal, CreatedAt: now, UpdatedAt: now}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/channels/1/keys/11", strings.NewReader(`{"Name":"改名"}`))
	req.SetPathValue("id", "1")
	req.SetPathValue("kid", "11")
	h.HandleUpdateKey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body keysItemResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	item := body.Data
	if item.ID != 11 || item.Name != "改名" {
		t.Fatalf("updated item=%+v", item)
	}
	kr, ok := mgr.GetKeyRuntime(11)
	if !ok || kr.Key.Name != "改名" {
		t.Fatalf("运行时未按最新重读：key 11=%+v ok=%v", kr, ok)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestHandler_HandleDeleteKey 断言软删成功后 mgr.RemoveKey 摘除运行时，并联动清理该密钥会话。
func TestHandler_HandleDeleteKey(t *testing.T) {
	h, mgr, mock := newHandlerWithKeys(t)

	var killedKeyID int64
	var killCalled int
	h.SetSessionKiller(func(keyID int64) int {
		killedKeyID = keyID
		killCalled++
		return 2
	})

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(11)).
		WillReturnRows(channelKeyRow(&ChannelKey{ID: 11, ChannelID: 1, Name: "key-a", CredentialEnc: "enc", State: StateNormal}))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET deleted_at=now() WHERE id=$1`)).
		WithArgs(int64(11)).WillReturnResult(sqlmock.NewResult(0, 1))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/channels/1/keys/11", nil)
	req.SetPathValue("id", "1")
	req.SetPathValue("kid", "11")
	h.HandleDeleteKey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Data struct {
			Affected int `json:"affected"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Data.Affected != 1 {
		t.Fatalf("affected want 1, got %d", body.Data.Affected)
	}
	if _, ok := mgr.GetKeyRuntime(11); ok {
		t.Fatalf("RemoveKey 未执行：运行时仍存在密钥 11")
	}
	// D3：密钥删除联动会话清理——注入的 killer 应以被删 keyID 调用（内存+DB 由 KillByFilter 完成）。
	if killCalled != 1 || killedKeyID != 11 {
		t.Fatalf("会话清理钩子应被调用一次且携带 keyID=11，got called=%d keyID=%d", killCalled, killedKeyID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestHandler_HandleDeleteKey_NoKillerSkips D3：未注入会话清理钩子时删除密钥不 panic、不清理。
func TestHandler_HandleDeleteKey_NoKillerSkips(t *testing.T) {
	h, mgr, mock := newHandlerWithKeys(t)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(12)).
		WillReturnRows(channelKeyRow(&ChannelKey{ID: 12, ChannelID: 1, Name: "key-b", CredentialEnc: "enc", State: StateNormal}))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET deleted_at=now() WHERE id=$1`)).
		WithArgs(int64(12)).WillReturnResult(sqlmock.NewResult(0, 1))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/admin/channels/1/keys/12", nil)
	req.SetPathValue("id", "1")
	req.SetPathValue("kid", "12")
	h.HandleDeleteKey(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if _, ok := mgr.GetKeyRuntime(12); ok {
		t.Fatalf("RemoveKey 未执行：运行时仍存在密钥 12")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestHandler_HandleKeyState 断言切状态后 DB 落库且 mgr.ManualSetKeyState 同步内存运行时。
func TestHandler_HandleKeyState(t *testing.T) {
	h, mgr, mock := newHandlerWithKeys(t)
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)
	encA, err := crypto.SM4Encrypt(testSM4Key, []byte(testKeyACred))
	if err != nil {
		t.Fatalf("encrypt key-a: %v", err)
	}

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(11)).
		WillReturnRows(channelKeyRow(&ChannelKey{ID: 11, ChannelID: 1, Name: "key-a", CredentialEnc: encA, State: StateNormal, CreatedAt: now, UpdatedAt: now}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(11)).
		WillReturnRows(channelKeyRow(&ChannelKey{ID: 11, ChannelID: 1, Name: "key-a", CredentialEnc: encA, State: StateNormal, CreatedAt: now, UpdatedAt: now}))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET state=$1, last_err=$2, updated_at=now() WHERE id=$3`)).
		WithArgs(string(StateDisabled), "", int64(11)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO channel_key_events(id, channel_key_id, from_state, to_state, reason) VALUES($1,$2,$3,$4,$5)`)).
		WithArgs(sqlmock.AnyArg(), int64(11), string(StateNormal), string(StateDisabled), reasonManualDisable).WillReturnResult(sqlmock.NewResult(20, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(11)).
		WillReturnRows(channelKeyRow(&ChannelKey{ID: 11, ChannelID: 1, Name: "key-a", CredentialEnc: encA, State: StateDisabled, CreatedAt: now, UpdatedAt: now}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/channels/1/keys/11/state", strings.NewReader(`{"Action":"disable"}`))
	req.SetPathValue("id", "1")
	req.SetPathValue("kid", "11")
	h.HandleKeyState(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body keysItemResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	item := body.Data
	if item.ID != 11 || item.State != StateDisabled {
		t.Fatalf("state item=%+v", item)
	}
	kr, ok := mgr.GetKeyRuntime(11)
	if !ok || kr.Machine.State() != StateDisabled {
		t.Fatalf("ManualSetKeyState 未同步运行时：key 11 state=%s ok=%v", kr.Machine.State(), ok)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestHandler_HandleListKeyEvents(t *testing.T) {
	h, _, mock := newHandlerWithKeys(t)
	now := time.Now()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`)).
		WithArgs(int64(11)).
		WillReturnRows(channelKeyRow(&ChannelKey{ID: 11, ChannelID: 1, Name: "key-a", CredentialEnc: "enc", State: StateNormal}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT `+channelKeyEventCols+` FROM channel_key_events WHERE channel_key_id=$1 ORDER BY created_at DESC LIMIT $2`)).
		WithArgs(int64(11), 50).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_key_id", "from_state", "to_state", "reason", "created_at"}).
			AddRow(int64(10), int64(11), "NORMAL", "DISABLED", reasonManualDisable, now))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/channels/1/keys/11/events", nil)
	req.SetPathValue("id", "1")
	req.SetPathValue("kid", "11")
	h.HandleListKeyEvents(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestHandler_HandleBatchDelete_KillsKeySessions 渠道批量删除后，其下全部密钥的存量会话应联动清理
// （与单密钥删除 killKeySess 同等生命周期语义）。
func TestHandler_HandleBatchDelete_KillsKeySessions(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	now := time.Date(2026, 9, 27, 10, 0, 0, 0, time.UTC)

	ch := &Channel{ID: 1, Name: "c1", Protocol: ProtocolOpenAICompat,
		BaseURL: "https://api.example.com", State: StateNormal}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE deleted_at IS NULL ORDER BY priority DESC, id ASC`)).
		WillReturnRows(channelRow(ch))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE deleted_at IS NULL ORDER BY channel_id, id`)).
		WithArgs().WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name", "credential_enc", "state", "last_err", "deleted_at", "created_at", "updated_at"}))

	mgr := NewManager(NewStore(db), nil, testSM4Key, nil, nil, func() time.Time { return now }, NewKeyStore(db))
	if err := mgr.SyncFromDB(context.Background()); err != nil {
		t.Fatalf("SyncFromDB: %v", err)
	}

	encA, err := crypto.SM4Encrypt(testSM4Key, []byte(testKeyACred))
	if err != nil {
		t.Fatalf("encrypt key-a: %v", err)
	}
	encB, err := crypto.SM4Encrypt(testSM4Key, []byte(testKeyBCred))
	if err != nil {
		t.Fatalf("encrypt key-b: %v", err)
	}
	keySvc := NewKeyService(NewKeyStore(db), testSM4Key)
	svc := NewService(NewStore(db))
	svc.SetManager(mgr)
	svc.SetKeyService(keySvc)
	h := NewHandler(svc, nil)
	h.SetKeyDeps(keySvc, mgr)
	var killed []int64
	h.SetSessionKiller(func(keyID int64) int {
		killed = append(killed, keyID)
		return 1
	})

	// 删除前收集密钥：channel_keys WHERE channel_id=$1 AND deleted_at IS NULL
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE channel_id=$1 AND deleted_at IS NULL ORDER BY id`)).
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name", "credential_enc", "state", "last_err", "deleted_at", "created_at", "updated_at"}).
			AddRow(int64(11), int64(1), "key-a", encA, string(StateNormal), "", nil, now, now).
			AddRow(int64(12), int64(1), "key-b", encB, string(StateNormal), "", nil, now, now))
	// 软删级联事务
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM channel_tags WHERE channel_id = ANY($1::bigint[])`)).
		WithArgs("{1}").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_models SET deleted_at = now() WHERE channel_id = ANY($1::bigint[])`)).
		WithArgs("{1}").WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET deleted_at = now() WHERE channel_id = ANY($1::bigint[])`)).
		WithArgs("{1}").WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channels SET deleted_at = now() WHERE id = ANY($1::bigint[])`)).
		WithArgs("{1}").WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/channels/batch-delete", strings.NewReader(`{"IDs":[1]}`))
	h.HandleBatchDelete(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if !slices.Equal(killed, []int64{11, 12}) {
		t.Fatalf("渠道删除后应清理其全部密钥会话，got killed=%v", killed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}
