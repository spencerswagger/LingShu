package channel

import (
	"context"
	"database/sql"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// ---- 0019 字段归属收口：基址 / 转换 / edited 的回归用例 ----

// B2 回归：引用协议时基址**只**跟随协议，channels.base_url 是历史遗留值、运行时忽略。
// （旧缺陷：后端优先取 channels.base_url，导致「改协议 baseURL 不生效」。）
func TestEffectiveProtocolBaseURL_ProtocolReferenceIgnoresInlineLenacy(t *testing.T) {
	pid := int64(7)
	proto := &channelprotocol.ChannelProtocol{
		ID:      pid,
		BaseURL: "https://proto.example.com",
		Protocols: channelprotocol.Protocols{
			channelprotocol.ProtocolAnthropicMessages: {BaseURL: "https://proto-anthropic.example.com"},
		},
	}
	cases := []struct {
		name     string
		rt       *RuntimeChannel
		protocol string
		want     string
	}{
		{
			name: "引用协议 + 遗留内联 base_url → 走协议地址",
			rt: &RuntimeChannel{
				Channel:  Channel{BaseURL: "https://legacy.example.com", ChannelProtocolID: &pid},
				Protocol: proto,
			},
			protocol: channelprotocol.ProtocolOpenAIChat,
			want:     "https://proto.example.com",
		},
		{
			name: "引用协议 + 协议级 baseUrl → 协议级优先",
			rt: &RuntimeChannel{
				Channel:  Channel{ChannelProtocolID: &pid},
				Protocol: proto,
			},
			protocol: channelprotocol.ProtocolAnthropicMessages,
			want:     "https://proto-anthropic.example.com",
		},
		{
			name:     "内联渠道 → 用自己的 base_url",
			rt:       &RuntimeChannel{Channel: Channel{BaseURL: "https://inline.example.com"}},
			protocol: channelprotocol.ProtocolOpenAIChat,
			want:     "https://inline.example.com",
		},
		{
			name:     "nil 运行时 → 空串（不 panic）",
			rt:       nil,
			protocol: channelprotocol.ProtocolOpenAIChat,
			want:     "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveProtocolBaseURL(tc.rt, tc.protocol); got != tc.want {
				t.Fatalf("effectiveProtocolBaseURL = %q, want %q", got, tc.want)
			}
		})
	}
}

// effectiveChannelBaseURL 是视图/明细页展示用的「最终生效基址」，必须与转发同源。
func TestEffectiveChannelBaseURL_MatchesForwardingChain(t *testing.T) {
	pid := int64(7)
	rt := &RuntimeChannel{
		Channel:  Channel{BaseURL: "https://legacy.example.com", ChannelProtocolID: &pid},
		Protocol: &channelprotocol.ChannelProtocol{ID: pid, BaseURL: "https://proto.example.com"},
	}
	if got := effectiveChannelBaseURL(rt); got != "https://proto.example.com" {
		t.Fatalf("effectiveChannelBaseURL = %q, want 协议地址", got)
	}
}

// 拍板③：引用协议时提交非空 BaseURL → 400（不静默丢弃，也不静默转覆盖）。
func TestValidateBase_RejectsInlineBaseURLWhenProtocolReferenced(t *testing.T) {
	pid := int64(7)
	svc := NewService(NewStore(nil)) // 不触库：本用例在协议存在性校验之前就该被拦下
	err := svc.validateBase(ChannelInput{Name: "c", BaseURL: "https://x.example.com", ChannelProtocolID: &pid})
	if err == nil {
		t.Fatalf("引用协议时非空 BaseURL 应被拒绝")
	}
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("期望 *APIError, got %T (%v)", err, err)
	}
	if apiErr.HTTPStatus != 400 {
		t.Fatalf("期望 400, got %d", apiErr.HTTPStatus)
	}
	// 内联模式仍然要求 base_url 非空。
	if err := svc.validateBase(ChannelInput{Name: "c", Protocol: "openai-chat"}); err == nil {
		t.Fatalf("内联渠道缺 BaseURL 应被拒绝")
	}
}

// 引用协议时更新渠道：channels.base_url 保留原值（回滚老二进制即恢复原地址，不制造不可逆丢失）。
func TestUpdateChannel_PreservesLegacyBaseURLForProtocolReference(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewService(NewStore(db))
	svc.SetChannelProtocolStore(channelprotocol.NewStore(db))
	now := time.Now()
	pid := int64(7)

	mock.ExpectQuery(cpByIdRe).WithArgs(pid).
		WillReturnRows(cpProtoRow(pid, "https://proto.example.com", `{}`))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE id = $1 AND deleted_at IS NULL`)).
		WithArgs(int64(1)).
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "c", BaseURL: "https://legacy.example.com",
			ChannelProtocolID: &pid, Priority: 100, CreatedAt: now, UpdatedAt: now}))
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channels SET`)).
		WithArgs("c2", "", "https://legacy.example.com", sqlmock.AnyArg(), int64(100),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), pid, int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM channel_tags WHERE channel_id = $1`)).
		WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	c, err := svc.UpdateChannel(context.Background(), 1,
		ChannelInput{Name: "c2", ChannelProtocolID: &pid})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if c.BaseURL != "https://legacy.example.com" {
		t.Fatalf("遗留 base_url 应被保留，got %q", c.BaseURL)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// 拍板④：edited 字段级置位——只有商业字段真变才冻结协议同步；
// 等价提交（1 vs 1.0、键序不同）与只改运行策略都不置位。
func TestCommercialFieldsChanged_FieldLevel(t *testing.T) {
	base := func() *ChannelModel {
		tc := &billing.TimeCoeffConfig{}
		return &ChannelModel{
			ExternalModelID: 2,
			CostRates:       billing.Rates{"input": 1, "output": 2, "cache_read": 0.1},
			TimeConfig:      tc,
			ContextTiers:    []billing.TierRule{{}},
			Protocols:       channelprotocol.Protocols{channelprotocol.ProtocolOpenAIChat: {}},
		}
	}
	in := func(mod func(*ChannelModelInput)) ChannelModelInput {
		v := ChannelModelInput{
			ExternalModelID: 2,
			CostRates:       billing.Rates{"input": 1, "output": 2, "cache_read": 0.1},
			TimeConfig:      &billing.TimeCoeffConfig{},
			ContextTiers:    []billing.TierRule{{}},
			Protocols:       channelprotocol.Protocols{channelprotocol.ProtocolOpenAIChat: {}},
		}
		if mod != nil {
			mod(&v)
		}
		return v
	}

	cases := []struct {
		name       string
		mod        func(*ChannelModelInput)
		wantChange bool
	}{
		{name: "完全等价 → 不置位", mod: nil, wantChange: false},
		{
			name: "成本数值等价（多写了历史键 cache_write）→ 不置位",
			mod: func(v *ChannelModelInput) {
				v.CostRates = billing.Rates{"input": 1, "output": 2, "cache_read": 0.1, "cache_write": 3}
			},
			wantChange: false,
		},
		{
			name:       "只改运行策略（输入层不带商业字段变化）→ 不置位",
			mod:        func(v *ChannelModelInput) { v.RateLimit = &RateLimitConfig{} },
			wantChange: false,
		},
		{
			name:       "成本真变 → 置位",
			mod:        func(v *ChannelModelInput) { v.CostRates = billing.Rates{"input": 9, "output": 2, "cache_read": 0.1} },
			wantChange: true,
		},
		{
			name:       "对外模型绑定变 → 置位",
			mod:        func(v *ChannelModelInput) { v.ExternalModelID = 5 },
			wantChange: true,
		},
		{
			name:       "协议子集变 → 置位",
			mod:        func(v *ChannelModelInput) { v.Protocols = channelprotocol.Protocols{} },
			wantChange: true,
		},
		{
			name: "时段系数真变 → 置位",
			mod: func(v *ChannelModelInput) {
				v.TimeConfig = &billing.TimeCoeffConfig{Default: 2}
			},
			wantChange: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := commercialFieldsChanged(base(), in(tc.mod)); got != tc.wantChange {
				t.Fatalf("commercialFieldsChanged = %v, want %v", got, tc.wantChange)
			}
		})
	}
}

// reset-from-protocol：恢复种子值 + 清除 edited（物化守卫的唯一出口）。
func TestService_ResetModelFromProtocol(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewService(NewStore(db))
	svc.SetChannelModelStore(NewChannelModelStore(db))
	svc.SetChannelProtocolStore(channelprotocol.NewStore(db))
	now := time.Now()
	pid := int64(7)

	cmGetByID := regexp.QuoteMeta(`SELECT ` + cmCols + ` FROM channel_models WHERE id = $1 AND deleted_at IS NULL`)
	mock.ExpectQuery(cmGetByID).WithArgs(int64(100)).
		WillReturnRows(cmRow(&ChannelModel{ID: 100, ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2,
			CostRates: billing.Rates{"input": 5}, Edited: true, State: StateNormal, CreatedAt: now, UpdatedAt: now}, `{"input":5}`))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE id = $1 AND deleted_at IS NULL`)).
		WithArgs(int64(1)).
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "c", ChannelProtocolID: &pid, Priority: 100, CreatedAt: now, UpdatedAt: now}))
	mock.ExpectQuery(cpByIdRe).WithArgs(pid).
		WillReturnRows(cpProtoRow(pid, "https://proto.example.com", `{"qwen-max":{"externalModelID":"2","costRates":{"Input":1,"Output":2,"CacheRead":0.1}}}`))
	// 按种子写外部模型绑定 + 成本。
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_models SET cost_rates=$1, external_model_id=$2, updated_at=now() WHERE id=$3`)).
		WithArgs(sqlmock.AnyArg(), int64(2), int64(100)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_models SET edited=$1, updated_at=now() WHERE id=$2`)).
		WithArgs(false, int64(100)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(cmGetByID).WithArgs(int64(100)).
		WillReturnRows(cmRow(&ChannelModel{ID: 100, ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2,
			CostRates: billing.Rates{"input": 1, "output": 2, "cache_read": 0.1}, State: StateNormal, CreatedAt: now, UpdatedAt: now}, `{"input":1}`))

	m, err := svc.ResetModelFromProtocol(context.Background(), 1, 100)
	if err != nil {
		t.Fatalf("reset: %v", err)
	}
	if m.Edited {
		t.Fatalf("重置后 edited 应为 false")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// reset-from-protocol 的前置校验：未引用协议 / 协议里没有该模型 → 400。
func TestService_ResetModelFromProtocol_RejectsNonProtocolChannel(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewService(NewStore(db))
	svc.SetChannelModelStore(NewChannelModelStore(db))
	now := time.Now()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cmCols + ` FROM channel_models WHERE id = $1 AND deleted_at IS NULL`)).WithArgs(int64(100)).
		WillReturnRows(cmRow(&ChannelModel{ID: 100, ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2,
			State: StateNormal, CreatedAt: now, UpdatedAt: now}, `{}`))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE id = $1 AND deleted_at IS NULL`)).
		WithArgs(int64(1)).
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "c", BaseURL: "https://inline.example.com",
			Priority: 100, CreatedAt: now, UpdatedAt: now}))

	_, err = svc.ResetModelFromProtocol(context.Background(), 1, 100)
	if err == nil {
		t.Fatalf("内联渠道不应支持 reset-from-protocol")
	}
	apiErr, ok := err.(*APIError)
	if !ok || apiErr.HTTPStatus != 400 {
		t.Fatalf("期望 400 *APIError, got %T (%v)", err, err)
	}
}

// 模型行不属于该渠道 → 400（防止跨渠道误重置）。
func TestService_ResetModelFromProtocol_RejectsForeignModel(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewService(NewStore(db))
	svc.SetChannelModelStore(NewChannelModelStore(db))
	now := time.Now()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cmCols + ` FROM channel_models WHERE id = $1 AND deleted_at IS NULL`)).WithArgs(int64(100)).
		WillReturnRows(cmRow(&ChannelModel{ID: 100, ChannelID: 2, InternalModelID: "qwen-max", ExternalModelID: 2,
			State: StateNormal, CreatedAt: now, UpdatedAt: now}, `{}`))

	if _, err := svc.ResetModelFromProtocol(context.Background(), 1, 100); err == nil {
		t.Fatalf("跨渠道的重置应被拒绝")
	}
}

// 更新渠道模型：只改运行策略不置 edited；商业字段真变才置（拍板④的落点）。
func TestUpdateChannelModel_EditedFlag(t *testing.T) {
	now := time.Now()
	mk := func(t *testing.T) (*Service, sqlmock.Sqlmock) {
		t.Helper()
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatalf("sqlmock: %v", err)
		}
		t.Cleanup(func() { db.Close() })
		svc := NewService(NewStore(db))
		svc.SetChannelModelStore(NewChannelModelStore(db))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cmCols + ` FROM channel_models WHERE id = $1 AND deleted_at IS NULL`)).WithArgs(int64(100)).
			WillReturnRows(cmRow(&ChannelModel{ID: 100, ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2,
				CostRates: billing.Rates{"input": 1, "output": 2, "cache_read": 0.1},
				State:     StateNormal, Edited: false, CreatedAt: now, UpdatedAt: now}, `{"input":1,"output":2,"cache_read":0.1}`))
		return svc, mock
	}
	base := ChannelModelInput{
		InternalModelID: "qwen-max",
		ExternalModelID: 2,
		CostRates:       billing.Rates{"input": 1, "output": 2, "cache_read": 0.1},
	}

	// 1) 等价提交（成本键序不同 + 多一个历史键）→ edited 仍为 false。
	svc, mock := mk(t)
	in := base
	in.CostRates = billing.Rates{"cache_read": 0.1, "output": 2, "input": 1, "cache_write": 3}
	mock.ExpectExec(`UPDATE channel_models SET`).
		WithArgs("qwen-max", int64(2), sqlmock.AnyArg(), nil, nil, nil, string(StateNormal),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), nil, sqlmock.AnyArg(), false, int64(100)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cmCols + ` FROM channel_models WHERE id = $1 AND deleted_at IS NULL`)).WithArgs(int64(100)).
		WillReturnRows(cmRow(&ChannelModel{ID: 100, ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2,
			State: StateNormal, CreatedAt: now, UpdatedAt: now}, `{}`))
	if _, err := svc.UpdateChannelModel(context.Background(), 1, 100, in); err != nil {
		t.Fatalf("update(等价提交): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("等价提交不应置 edited: %v", err)
	}

	// 2) 成本真变 → edited=true。
	svc, mock = mk(t)
	changed := base
	changed.CostRates = billing.Rates{"input": 9, "output": 2, "cache_read": 0.1}
	mock.ExpectExec(`UPDATE channel_models SET`).
		WithArgs("qwen-max", int64(2), sqlmock.AnyArg(), nil, nil, nil, string(StateNormal),
			sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), nil, sqlmock.AnyArg(), true, int64(100)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cmCols + ` FROM channel_models WHERE id = $1 AND deleted_at IS NULL`)).WithArgs(int64(100)).
		WillReturnRows(cmRow(&ChannelModel{ID: 100, ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2,
			State: StateNormal, Edited: true, CreatedAt: now, UpdatedAt: now}, `{}`))
	if _, err := svc.UpdateChannelModel(context.Background(), 1, 100, changed); err != nil {
		t.Fatalf("update(成本真变): %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("成本真变应置 edited: %v", err)
	}
}

// 人工创建渠道模型 → edited=true（人工创建即视为本地编辑）。
func TestCreateChannelModel_SetsEdited(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewService(NewStore(db))
	svc.SetChannelModelStore(NewChannelModelStore(db))
	now := time.Now()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE id = $1 AND deleted_at IS NULL`)).
		WithArgs(int64(1)).
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "c", BaseURL: "https://x.example.com",
			Priority: 100, CreatedAt: now, UpdatedAt: now}))
	mock.ExpectQuery(`INSERT INTO channel_models`).
		WillReturnRows(cmRow(&ChannelModel{ID: 100, ChannelID: 1, InternalModelID: "m", ExternalModelID: 2,
			State: StateNormal, Edited: true, CreatedAt: now, UpdatedAt: now}, `{}`))

	created, err := svc.CreateChannelModel(context.Background(), 1, ChannelModelInput{
		InternalModelID: "m", ExternalModelID: 2,
		CostRates: billing.Rates{"input": 1, "output": 2, "cache_read": 0.1},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !created.Edited {
		t.Fatalf("人工创建应置 edited=true")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// 保证 sql.ErrNoRows 语义在 GetByID 路径上被正确分类（reset 用 404）。
func TestResetModelFromProtocol_NotFound(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewService(NewStore(db))
	svc.SetChannelModelStore(NewChannelModelStore(db))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cmCols + ` FROM channel_models WHERE id = $1 AND deleted_at IS NULL`)).WithArgs(int64(404)).
		WillReturnError(sql.ErrNoRows)
	if _, err := svc.ResetModelFromProtocol(context.Background(), 1, 404); err == nil {
		t.Fatalf("不存在的模型行应返回 404")
	}
}
