package channel

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// HTTP 级用例：覆盖 Gate 指出的两个「只有服务级测试、没有接口级断言」的点
//   ① GET /api/v1/admin/channels 必须返回只读派生 BaseURLEffective（列表页/编辑页依赖它）；
//   ② POST /api/v1/admin/channels/{id}/models/{mid}/reset-from-protocol 的接口行为。

func newOwnershipHandler(t *testing.T) (*Handler, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	svc := NewService(NewStore(db))
	svc.SetChannelModelStore(NewChannelModelStore(db))
	svc.SetChannelProtocolStore(channelprotocol.NewStore(db))
	return NewHandler(svc), mock
}

type channelsListResponse struct {
	Code int       `json:"Code"`
	Data []Channel `json:"Data"`
}

// 引用协议的渠道：列表里 BaseURLEffective 必须是协议基址（不是遗留的 channels.base_url）。
func TestHandler_ListChannels_ReturnsBaseURLEffective(t *testing.T) {
	h, mock := newOwnershipHandler(t)
	now := time.Now()
	pid := int64(7)

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE deleted_at IS NULL ORDER BY priority DESC, id ASC`)).
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "c1", Protocol: "openai-compat",
			BaseURL: "https://legacy.example.com", ChannelProtocolID: &pid, Priority: 100, CreatedAt: now, UpdatedAt: now}))
	// 标签关联查询：本用例不关心，返回空集。
	mock.ExpectQuery(`(?s)SELECT .* FROM channel_tags .*`).WillReturnRows(sqlmock.NewRows([]string{"channel_id", "tag_id"}))
	// 协议基址解析（内联渠道不会走这条）。
	mock.ExpectQuery(cpByIdRe).WithArgs(pid).
		WillReturnRows(cpProtoRow(pid, "https://proto.example.com", `{}`))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/channels", nil)
	h.HandleList(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	// 契约层断言：字段必须出现在 JSON 里（前端按此字段展示「生效基址」）。
	if !strings.Contains(rec.Body.String(), `"BaseURLEffective"`) {
		t.Fatalf("响应缺少 BaseURLEffective 字段: %s", rec.Body.String())
	}
	var body channelsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(body.Data) != 1 {
		t.Fatalf("list len=%d, want 1; body=%s", len(body.Data), rec.Body.String())
	}
	if got := body.Data[0].BaseURLEffective; got != "https://proto.example.com" {
		t.Fatalf("引用协议时应展示协议基址, got %q", got)
	}
	if body.Data[0].BaseURL != "https://legacy.example.com" {
		t.Fatalf("BaseURL 仍是原列值（语义=内联专用，此处保留历史值）, got %q", body.Data[0].BaseURL)
	}
}

// 内联渠道：生效基址 = 自身 base_url。
func TestHandler_ListChannels_BaseURLEffectiveInline(t *testing.T) {
	h, mock := newOwnershipHandler(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE deleted_at IS NULL ORDER BY priority DESC, id ASC`)).
		WillReturnRows(channelRow(&Channel{ID: 2, Name: "inline", Protocol: "openai-chat",
			BaseURL: "https://inline.example.com", Priority: 100, CreatedAt: now, UpdatedAt: now}))
	mock.ExpectQuery(`(?s)SELECT .* FROM channel_tags .*`).WillReturnRows(sqlmock.NewRows([]string{"channel_id", "tag_id"}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/channels", nil)
	h.HandleList(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d; body=%s", rec.Code, rec.Body.String())
	}
	var body channelsListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got := body.Data[0].BaseURLEffective; got != "https://inline.example.com" {
		t.Fatalf("内联渠道生效基址应等于自身 base_url, got %q", got)
	}
}

type channelModelResponse struct {
	Code int              `json:"Code"`
	Data viewChannelModel `json:"Data"`
}

// reset-from-protocol 的接口级用例：200 + 回包 edited=false。
func TestHandler_ResetChannelModelFromProtocol(t *testing.T) {
	h, mock := newOwnershipHandler(t)
	now := time.Now()
	pid := int64(7)
	cmGet := regexp.QuoteMeta(`SELECT ` + cmCols + ` FROM channel_models WHERE id = $1 AND deleted_at IS NULL`)

	mock.ExpectQuery(cmGet).WithArgs(int64(100)).
		WillReturnRows(cmRow(&ChannelModel{ID: 100, ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2,
			CostRates: billing.Rates{"input": 5}, Edited: true, State: StateNormal, CreatedAt: now, UpdatedAt: now}, `{"input":5}`))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE id = $1 AND deleted_at IS NULL`)).
		WithArgs(int64(1)).
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "c", ChannelProtocolID: &pid, Priority: 100, CreatedAt: now, UpdatedAt: now}))
	mock.ExpectQuery(cpByIdRe).WithArgs(pid).
		WillReturnRows(cpProtoRow(pid, "https://proto.example.com",
			`{"qwen-max":{"externalModelID":"2","costRates":{"Input":1,"Output":2,"CacheRead":0.1}}}`))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_models SET cost_rates=$1, external_model_id=$2, updated_at=now() WHERE id=$3`)).
		WithArgs(sqlmock.AnyArg(), int64(2), int64(100)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_models SET edited=$1, updated_at=now() WHERE id=$2`)).
		WithArgs(false, int64(100)).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(cmGet).WithArgs(int64(100)).
		WillReturnRows(cmRow(&ChannelModel{ID: 100, ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2,
			CostRates: billing.Rates{"input": 1, "output": 2, "cache_read": 0.1}, State: StateNormal, CreatedAt: now, UpdatedAt: now}, `{"input":1}`))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/channels/1/models/100/reset-from-protocol", nil)
	req.SetPathValue("id", "1")
	req.SetPathValue("mid", "100")
	h.HandleResetChannelModelFromProtocol(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body channelModelResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Data.Edited {
		t.Fatalf("reset 后 Edited 应为 false; body=%s", rec.Body.String())
	}
	if body.Data.CostRates["input"] != 1 {
		t.Fatalf("reset 后成本应回到种子值 1, got %v", body.Data.CostRates)
	}
}

// 内联渠道调 reset → 400（没有可重置的协议值），且错误码走统一响应。
func TestHandler_ResetChannelModelFromProtocol_InlineChannel400(t *testing.T) {
	h, mock := newOwnershipHandler(t)
	now := time.Now()
	cmGet := regexp.QuoteMeta(`SELECT ` + cmCols + ` FROM channel_models WHERE id = $1 AND deleted_at IS NULL`)

	mock.ExpectQuery(cmGet).WithArgs(int64(100)).
		WillReturnRows(cmRow(&ChannelModel{ID: 100, ChannelID: 1, InternalModelID: "m", ExternalModelID: 2,
			State: StateNormal, CreatedAt: now, UpdatedAt: now}, `{}`))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE id = $1 AND deleted_at IS NULL`)).
		WithArgs(int64(1)).
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "c", BaseURL: "https://inline.example.com",
			Priority: 100, CreatedAt: now, UpdatedAt: now}))

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/channels/1/models/100/reset-from-protocol", nil)
	req.SetPathValue("id", "1")
	req.SetPathValue("mid", "100")
	h.HandleResetChannelModelFromProtocol(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("内联渠道 reset 应 400, got %d; body=%s", rec.Code, rec.Body.String())
	}
}

// 400 文案必须能指导操作（Gate：此前只断言 status）。
func TestValidateBase_ErrorTextIsActionable(t *testing.T) {
	pid := int64(7)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewService(NewStore(db))
	svc.SetChannelProtocolStore(channelprotocol.NewStore(db))
	mock.ExpectQuery(cpByIdRe).WithArgs(pid).
		WillReturnRows(cpProtoRow(pid, "https://proto.example.com", `{}`))

	err = svc.validateBase(ChannelInput{Name: "c", BaseURL: "https://evil.example.com", ChannelProtocolID: &pid})
	if err == nil {
		t.Fatalf("引用协议时非空 BaseURL 应被拒绝")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("期望 *APIError, got %T", err)
	}
	msg := apiErr.Message
	for _, want := range []string{"基址跟随被引用协议", "p", "新建", "渠道协议"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("400 文案缺少「%s」，实际=%q", want, msg)
		}
	}
}

// 服务级：enrichBaseURLEffective 在运行时缺失时按协议计算（协议按 ID 缓存，不 N 次查库）。
func TestService_enrichBaseURLEffective_CachesProtocolLookup(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewService(NewStore(db))
	svc.SetChannelProtocolStore(channelprotocol.NewStore(db))
	pid := int64(7)
	// 只允许一次协议查询（两个渠道引用同一协议）。
	mock.ExpectQuery(cpByIdRe).WithArgs(pid).
		WillReturnRows(cpProtoRow(pid, "https://proto.example.com", `{}`))

	a := &Channel{ID: 1, ChannelProtocolID: &pid, BaseURL: "https://legacy-a.example.com"}
	b := &Channel{ID: 2, ChannelProtocolID: &pid}
	svc.enrichBaseURLEffective(context.Background(), a, b)

	if a.BaseURLEffective != "https://proto.example.com" || b.BaseURLEffective != "https://proto.example.com" {
		t.Fatalf("引用协议时应取协议基址, got a=%q b=%q", a.BaseURLEffective, b.BaseURLEffective)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("协议查询应被缓存（只查一次）: %v", err)
	}
}

// HTTP 级：引用协议 + 非空 BaseURL 必须 400（此前归一化在校验前把值抹空，400 成了不可达代码，
// 真实 server 上返回 200 且配置被静默丢弃）。创建与更新各一条。
func TestHandler_CreateChannel_ProtocolRefWithBaseURL_400(t *testing.T) {
	h, mock := newOwnershipHandler(t)
	pid := int64(7)
	mock.ExpectQuery(cpByIdRe).WithArgs(pid).
		WillReturnRows(cpProtoRow(pid, "https://proto.example.com", `{}`))

	body := `{"Name":"c1","ChannelProtocolID":"7","BaseURL":"https://should-400.example.com"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/channels", strings.NewReader(body))
	h.HandleCreate(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("引用协议 + 非空 BaseURL 应 400, got %d; body=%s", rec.Code, rec.Body.String())
	}
	for _, want := range []string{"基址跟随被引用协议", "p", "新建"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("400 文案缺少「%s」: %s", want, rec.Body.String())
		}
	}
}

func TestHandler_UpdateChannel_ProtocolRefWithBaseURL_400(t *testing.T) {
	h, mock := newOwnershipHandler(t)
	pid := int64(7)
	mock.ExpectQuery(cpByIdRe).WithArgs(pid).
		WillReturnRows(cpProtoRow(pid, "https://proto.example.com", `{}`))

	body := `{"Name":"c1","ChannelProtocolID":"7","BaseURL":"https://should-400.example.com"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/channels/1", strings.NewReader(body))
	req.SetPathValue("id", "1")
	h.HandleUpdate(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("更新时引用协议 + 非空 BaseURL 应 400, got %d; body=%s", rec.Code, rec.Body.String())
	}
}

// HTTP 级：引用协议 + 纯空白 BaseURL → 放行（200），落库为 ”。
func TestHandler_CreateChannel_ProtocolRefWithBlankBaseURL_200(t *testing.T) {
	now := time.Now()
	pid := int64(7)
	h, mock := newOwnershipHandler(t)
	mock.ExpectQuery(cpByIdRe).WithArgs(pid).
		WillReturnRows(cpProtoRow(pid, "https://proto.example.com", `{}`))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE name = $1`)).
		WithArgs("c1").WillReturnRows(sqlmock.NewRows(nil))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(insertChannelSQL)).
		WithArgs(sqlmock.AnyArg(), "c1", "", "", sqlmock.AnyArg(), int64(100),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), pid).
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "c1", ChannelProtocolID: &pid,
			Priority: 100, CreatedAt: now, UpdatedAt: now}))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM channel_tags WHERE channel_id = $1`)).
		WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	body := `{"Name":"c1","ChannelProtocolID":"7","BaseURL":" \t\n "}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/channels", strings.NewReader(body))
	h.HandleCreate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("引用协议 + 空白 BaseURL 应放行, got %d; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data Channel `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Data.BaseURL != "" || out.Data.Protocol != "" {
		t.Fatalf("引用协议时落库必须为空串, got base_url=%q protocol=%q", out.Data.BaseURL, out.Data.Protocol)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("落库值应为空串（server 端归一后再写）: %v", err)
	}
}

func TestHandler_UpdateChannel_ProtocolRefWithBlankBaseURL_200(t *testing.T) {
	now := time.Now()
	pid := int64(7)
	h, mock := newOwnershipHandler(t)
	mock.ExpectQuery(cpByIdRe).WithArgs(pid).
		WillReturnRows(cpProtoRow(pid, "https://proto.example.com", `{}`))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE id = $1 AND deleted_at IS NULL`)).
		WithArgs(int64(1)).
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "c1", ChannelProtocolID: &pid,
			Priority: 100, CreatedAt: now, UpdatedAt: now}))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channels SET`)).
		WithArgs("c1", "", "", sqlmock.AnyArg(), int64(100), sqlmock.AnyArg(), sqlmock.AnyArg(),
			sqlmock.AnyArg(), pid, int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM channel_tags WHERE channel_id = $1`)).
		WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	body := `{"Name":"c1","ChannelProtocolID":"7","BaseURL":"\t\n"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPut, "/api/v1/admin/channels/1", strings.NewReader(body))
	req.SetPathValue("id", "1")
	h.HandleUpdate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("更新时引用协议 + 空白 BaseURL 应放行, got %d; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data Channel `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Data.BaseURL != "" || out.Data.Protocol != "" {
		t.Fatalf("引用协议时落库必须为空串, got base_url=%q protocol=%q", out.Data.BaseURL, out.Data.Protocol)
	}
}

// HTTP 级：内联渠道 + 合法非空 BaseURL → 200 且原值落库（不要被归一化误伤）。
func TestHandler_CreateChannel_InlineWithBaseURL_200(t *testing.T) {
	now := time.Now()
	h, mock := newOwnershipHandler(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE name = $1`)).
		WithArgs("inline").WillReturnRows(sqlmock.NewRows(nil))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(insertChannelSQL)).
		WithArgs(sqlmock.AnyArg(), "inline", "openai-chat", "https://inline.example.com", sqlmock.AnyArg(),
			int64(100), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), nil).
		WillReturnRows(channelRow(&Channel{ID: 2, Name: "inline", Protocol: "openai-chat",
			BaseURL: "https://inline.example.com", Priority: 100, CreatedAt: now, UpdatedAt: now}))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM channel_tags WHERE channel_id = $1`)).
		WithArgs(int64(2)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	body := `{"Name":"inline","Protocol":"openai-chat","BaseURL":"https://inline.example.com"}`
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/channels", strings.NewReader(body))
	h.HandleCreate(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("内联渠道合法 BaseURL 应 200, got %d; body=%s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data Channel `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Data.BaseURL != "https://inline.example.com" || out.Data.Protocol != "openai-chat" {
		t.Fatalf("内联值应原样落库, got base_url=%q protocol=%q", out.Data.BaseURL, out.Data.Protocol)
	}
}
