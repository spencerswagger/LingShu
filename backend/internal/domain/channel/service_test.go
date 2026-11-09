package channel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
	"github.com/team/llmgateway/internal/pkg/resp"
)

var testSM4Key = []byte("0123456789abcdef") // 16 字节测试密钥（密钥运行时解密用）

func channelRow(c *Channel) *sqlmock.Rows {
	tags, _ := json.Marshal(c.Tags)
	rl, _ := json.Marshal(c.RateLimit)
	hp, _ := json.Marshal(c.HealthProbe)
	rel, _ := json.Marshal(c.Reliability)
	return sqlmock.NewRows([]string{"id", "name", "protocol", "base_url",
		"tags", "priority", "rate_limit", "health_probe", "reliability",
		"channel_protocol_id", "created_at", "updated_at"}).
		AddRow(c.ID, c.Name, c.Protocol, c.BaseURL, tags, c.Priority,
			rl, hp, rel, nullableID(c.ChannelProtocolID), c.CreatedAt, c.UpdatedAt)
}

func asAPIError(err error) (*APIError, bool) {
	if err == nil {
		return nil, false
	}
	ae, ok := err.(*APIError)
	return ae, ok
}

const insertChannelSQL = `INSERT INTO channels(id, name, protocol, base_url, tags, priority, rate_limit, health_probe, reliability, channel_protocol_id)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		 RETURNING ` + channelCols

func TestService_CreateChannel_Defaults(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewService(NewStore(db))

	// 唯一性预检查：无同名渠道。
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE name = $1`)).
		WithArgs("my-channel").WillReturnRows(sqlmock.NewRows(nil))

	wantRL := marshalJSONB(DefaultRateLimit())
	wantHP := marshalJSONB(DefaultHealthProbe())
	wantRel := marshalJSONB(DefaultReliability())

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(insertChannelSQL)).
		WithArgs(sqlmock.AnyArg(), "my-channel", "openai-compat", "https://api.example.com",
			sqlmock.AnyArg(), 100, wantRL, wantHP, wantRel, nil).
		WillReturnRows(channelRow(&Channel{
			ID: 1, Name: "my-channel", Protocol: "openai-compat", BaseURL: "https://api.example.com",
			Priority:  100,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM channel_tags WHERE channel_id = $1`)).
		WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	ch, err := svc.CreateChannel(context.Background(), ChannelInput{
		Name: "my-channel", Protocol: "openai-compat", BaseURL: "https://api.example.com",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ch.Name != "my-channel" {
		t.Fatalf("unexpected channel: %+v", ch)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

type fakeResolver struct {
	tags []TagRef
	err  error
}

func (f fakeResolver) ResolveChannelTags(_ context.Context, ids []int64) ([]TagRef, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.tags, f.err
}

func TestService_CreateChannel_BindsTags(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	st := NewStore(db)
	s := NewService(st)
	s.SetTagResolver(fakeResolver{tags: []TagRef{{ID: 11, Name: "金融", KV: map[string]string{"region": "cn"}}}})
	ctx := context.Background()

	mock.ExpectQuery(`SELECT ` + regexp.QuoteMeta(channelCols) + ` FROM channels WHERE name = \$1`).
		WithArgs("c").WillReturnRows(sqlmock.NewRows(nil))

	mock.ExpectBegin()
	mock.ExpectQuery(`INSERT INTO channels`).
		WillReturnRows(sqlmock.NewRows(strings.Split(channelCols, ", ")).
			AddRow(int64(7), "c", "openai-compat", "https://x.example.com", []byte("{}"),
				int64(100), []byte("{}"), []byte("{}"), []byte("{}"), nil,
				time.Now(), time.Now()))
	mock.ExpectExec(`DELETE FROM channel_tags`).WithArgs(int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO channel_tags`).WithArgs(int64(7), int64(11)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	ch, err := s.CreateChannel(ctx, ChannelInput{
		Name: "c", Protocol: ProtocolOpenAICompat, BaseURL: "https://x.example.com",
		TagIDs: []int64{11},
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if len(ch.BoundTags) != 1 || ch.BoundTags[0].ID != 11 {
		t.Fatalf("期望回填 BoundTags，got %+v", ch.BoundTags)
	}
	if len(ch.TagIDs) != 1 || ch.TagIDs[0] != 11 {
		t.Fatalf("期望回填 TagIDs，got %+v", ch.TagIDs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
func TestService_CreateChannel_DuplicateName(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewService(NewStore(db))

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE name = $1`)).
		WithArgs("dup").
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "dup"}))

	_, err = svc.CreateChannel(context.Background(), ChannelInput{
		Name: "dup", Protocol: "openai-compat", BaseURL: "https://api.example.com",
	})
	ae, ok := asAPIError(err)
	if !ok || ae.Code != resp.CodeConflict {
		t.Fatalf("expected 40901, got %v", err)
	}
}

func TestService_UpdateChannel(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewService(NewStore(db))

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE id = $1`)).
		WithArgs(int64(5)).
		WillReturnRows(channelRow(&Channel{
			ID: 5, Name: "old", Protocol: "openai-compat", BaseURL: "https://a.example.com",
			Priority:  50,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}))

	updQuery := `UPDATE channels SET name=$1, protocol=$2, base_url=$3,
		        tags=$4, priority=$5, rate_limit=$6, health_probe=$7,
		        reliability=$8, channel_protocol_id=$9, updated_at=now()
		 WHERE id=$10 AND deleted_at IS NULL`
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(updQuery)).
		WithArgs("renamed", "openai-compat", "https://b.example.com",
			sqlmock.AnyArg(), 50, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), nil, int64(5)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM channel_tags WHERE channel_id = $1`)).
		WithArgs(int64(5)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	ch, err := svc.UpdateChannel(context.Background(), 5, ChannelInput{
		Name: "renamed", Protocol: "openai-compat", BaseURL: "https://b.example.com",
	})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if ch.ID != 5 || ch.Name != "renamed" {
		t.Fatalf("unexpected channel: %+v", ch)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestService_DeleteChannel_WithBilling(t *testing.T) {
	// 有账单记录的渠道同样允许软删除；历史账单保留。
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewService(NewStore(db))

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM channel_tags WHERE channel_id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_models SET deleted_at = now() WHERE channel_id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET deleted_at = now() WHERE channel_id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channels SET deleted_at = now() WHERE id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	err = svc.DeleteChannel(context.Background(), 9)
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestService_CreateChannel_InvalidBaseURL(t *testing.T) {
	db, _, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewService(NewStore(db))

	_, err = svc.CreateChannel(context.Background(), ChannelInput{
		Name: "x", Protocol: "openai-compat", BaseURL: "ftp://bad",
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeBadRequest {
		t.Fatalf("expected 40001, got %v", err)
	}
}

// TestService_ListProbeLogs_AggregatesChannelKeys 探测历史读路径按渠道聚合：
// 先取渠道全部未删 keyID，再以 ANY 查询合并列表（修复把 channel_id 当 channel_key_id 查询恒为空的问题）。
func TestService_ListProbeLogs_AggregatesChannelKeys(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewService(NewStore(db))
	svc.SetChannelModelStore(NewChannelModelStore(db))
	svc.SetKeyService(NewKeyService(NewKeyStore(db), testSM4Key))

	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE id = $1 AND deleted_at IS NULL`)).
		WithArgs(int64(1)).
		WillReturnRows(channelRow(&Channel{
			ID: 1, Name: "ch", Protocol: "openai-compat", BaseURL: "https://a.example.com",
			Priority: 100, CreatedAt: now, UpdatedAt: now,
		}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE channel_id=$1 AND deleted_at IS NULL ORDER BY id`)).
		WithArgs(int64(1)).
		WillReturnRows(channelKeyRow(&ChannelKey{ID: 10, ChannelID: 1, Name: "k1", CredentialEnc: "enc", State: StateNormal, CreatedAt: now, UpdatedAt: now}).
			AddRow(int64(11), int64(1), "k2", "enc", string(StateNormal), "", nil, now, now))

	probeLogsSQL := `SELECT id, channel_key_id, model_id, level, target, ok, error, input_tokens, output_tokens, cached_tokens, total_tokens, duration_ms, created_at
		 FROM probe_logs WHERE channel_key_id = ANY($1::bigint[]) ORDER BY id DESC LIMIT $2`
	plCols := []string{"id", "channel_key_id", "model_id", "level", "target", "ok", "error",
		"input_tokens", "output_tokens", "cached_tokens", "total_tokens", "duration_ms", "created_at"}
	mock.ExpectQuery(regexp.QuoteMeta(probeLogsSQL)).
		WithArgs("{10,11}", 100).
		WillReturnRows(sqlmock.NewRows(plCols).
			AddRow(int64(1), int64(10), "m-default", "key", "https://a.example.com/chat/completions", true, "", 2, 3, 0, 5, 12, now).
			AddRow(int64(2), int64(11), "m1", "model", "", false, "boom", 0, 0, 0, 0, 0, now))

	logs, err := svc.ListProbeLogs(context.Background(), 1, 0)
	if err != nil {
		t.Fatalf("list probe logs: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("合并列表应有 2 条，got %d", len(logs))
	}
	if logs[0].ChannelKeyID != 10 || logs[1].ChannelKeyID != 11 {
		t.Fatalf("应保留 channel_key_id（按密钥聚合合并）：%+v", logs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// ---- 渠道协议物化 ----

const cpByIdRe = `(?s)SELECT .* FROM channel_protocols WHERE id=\$1 AND deleted_at IS NULL`

func cpProtoRow(id int64, baseURL, modelsJSON string) *sqlmock.Rows {
	now := time.Now()
	return sqlmock.NewRows([]string{"id", "name", "base_url", "protocols", "request_headers",
		"models", "error_policy", "features", "preset_source_id", "edited", "source_status", "created_at", "updated_at"}).
		AddRow(id, "p", baseURL, `{"openai-chat":{}}`, `{"passthroughMode":"none"}`, modelsJSON, `[]`, `[]`, "", true,
			channelprotocol.SourceActive, now, now)
}

func TestStore_ListByProtocol(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewStore(db)
	now := time.Now()
	pid := int64(7)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE channel_protocol_id = $1 AND deleted_at IS NULL ORDER BY id ASC`)).
		WithArgs(pid).
		WillReturnRows(channelRow(&Channel{
			ID: 3, Name: "c", Protocol: "openai-chat", ChannelProtocolID: &pid,
			Priority: 100, CreatedAt: now, UpdatedAt: now,
		}))

	chs, err := s.ListByProtocol(context.Background(), pid)
	if err != nil {
		t.Fatalf("list by protocol: %v", err)
	}
	if len(chs) != 1 || chs[0].ID != 3 {
		t.Fatalf("unexpected channels: %+v", chs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// RematerializeProtocol（0019 口径）：只物化 externalModelID>0 的行；已存在且种子没带该行任何
// 可写列时不发 UPDATE（既避免 updated_at 抖动，也避免把缺省键写成 null —— B1 的修法）。
func TestService_RematerializeProtocol_InsertsAndPreservesState(t *testing.T) {
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

	modelsJSON := `{"qwen-max":{"externalModelID":"2","costRates":{"input":1,"output":2,"cache_read":0.1},"protocols":{"openai-chat":{}}},` +
		`"qwen-plus":{"externalModelID":"5"},"no-bind":{"externalModelID":"0"}}`
	mock.ExpectQuery(cpByIdRe).WithArgs(pid).WillReturnRows(cpProtoRow(7, "https://x.example.com", modelsJSON))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE channel_protocol_id = $1 AND deleted_at IS NULL ORDER BY id ASC`)).
		WithArgs(pid).
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "c", Protocol: "openai-chat", ChannelProtocolID: &pid, Priority: 100, CreatedAt: now, UpdatedAt: now}))

	cmGetRe := `(?s)SELECT .* FROM channel_models WHERE channel_id=\$1 AND internal_model_id=\$2 AND deleted_at IS NULL ORDER BY id ASC LIMIT 1`
	// qwen-max：不存在 → 按种子插入（状态 NORMAL，edited=false 由库默认）。
	mock.ExpectQuery(cmGetRe).WithArgs(int64(1), "qwen-max").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, state, cost_rates, protocols) VALUES($1, $2, $3, $4, $5, $6, $7) RETURNING ` + cmCols)).
		WillReturnRows(cmRow(&ChannelModel{ID: 100, ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2, State: StateNormal, CreatedAt: now, UpdatedAt: now}, `{}`))
	// qwen-plus：已存在（DRAIN），但种子只给了 externalModelID（与现值一致）→ 没有可写列，不发 UPDATE。
	// 状态仍由既有行持有（物化从不改 state）。
	mock.ExpectQuery(cmGetRe).WithArgs(int64(1), "qwen-plus").
		WillReturnRows(cmRow(&ChannelModel{ID: 9, ChannelID: 1, InternalModelID: "qwen-plus", ExternalModelID: 5, State: StateDrain, CreatedAt: now, UpdatedAt: now}, `{}`))

	if err := svc.RematerializeProtocol(context.Background(), pid); err != nil {
		t.Fatalf("rematerialize: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// B1 回归（更新侧）：种子缺 costRates 时，已存在行的 cost_rates 不得被覆盖成 null。
// 用「SQL 里根本不出现 cost_rates」当断言——这正是缺陷的成因（旧实现无条件全量写）。
func TestService_Materialize_SeedWithoutCostRatesKeepsExistingCost(t *testing.T) {
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

	modelsJSON := `{"qwen-max":{"externalModelID":"2","protocols":{"openai-chat":{}}}}`
	mock.ExpectQuery(cpByIdRe).WithArgs(pid).WillReturnRows(cpProtoRow(7, "https://x.example.com", modelsJSON))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE channel_protocol_id = $1 AND deleted_at IS NULL ORDER BY id ASC`)).
		WithArgs(pid).
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "c", Protocol: "openai-chat", ChannelProtocolID: &pid, Priority: 100, CreatedAt: now, UpdatedAt: now}))
	mock.ExpectQuery(`(?s)SELECT .* FROM channel_models WHERE channel_id=\$1 AND internal_model_id=\$2 AND deleted_at IS NULL ORDER BY id ASC LIMIT 1`).
		WithArgs(int64(1), "qwen-max").
		WillReturnRows(cmRow(&ChannelModel{ID: 100, ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2,
			CostRates: billing.Rates{"input": 1, "output": 2, "cache_read": 0.1}, State: StateNormal, CreatedAt: now, UpdatedAt: now}, `{"input":1,"output":2,"cache_read":0.1}`))
	// 只有 protocols 被写；cost_rates 不出现。
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_models SET protocols=$1, updated_at=now() WHERE id=$2`)).
		WithArgs(sqlmock.AnyArg(), int64(100)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := svc.RematerializeProtocol(context.Background(), pid); err != nil {
		t.Fatalf("rematerialize: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// 物化守卫（R6）+ 拍板②：edited=true 的行商业字段不再被种子覆盖，
// 但转换配置按协议定义照常刷新（渠道层没有差异化写入路径）。
func TestService_Materialize_EditedRowSkipsCommercialButSyncsConvert(t *testing.T) {
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

	modelsJSON := `{"qwen-max":{"externalModelID":"2","costRates":{"Input":9,"Output":9,"CacheRead":9},` +
		`"convertEnabled":false,"convertTargets":["openai-responses"]}}`
	mock.ExpectQuery(cpByIdRe).WithArgs(pid).WillReturnRows(cpProtoRow(7, "https://x.example.com", modelsJSON))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE channel_protocol_id = $1 AND deleted_at IS NULL ORDER BY id ASC`)).
		WithArgs(pid).
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "c", Protocol: "openai-chat", ChannelProtocolID: &pid, Priority: 100, CreatedAt: now, UpdatedAt: now}))
	mock.ExpectQuery(`(?s)SELECT .* FROM channel_models WHERE channel_id=\$1 AND internal_model_id=\$2 AND deleted_at IS NULL ORDER BY id ASC LIMIT 1`).
		WithArgs(int64(1), "qwen-max").
		WillReturnRows(cmRow(&ChannelModel{ID: 100, ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2,
			CostRates: billing.Rates{"input": 1, "output": 2, "cache_read": 0.1}, Edited: true,
			State: StateNormal, CreatedAt: now, UpdatedAt: now}, `{"input":1,"output":2,"cache_read":0.1}`))
	// 只刷新转换两列（成本保持本地值）。
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_models SET convert_enabled=$1, convert_targets=$2, updated_at=now() WHERE id=$3`)).
		WithArgs(false, sqlmock.AnyArg(), int64(100)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	if err := svc.RematerializeProtocol(context.Background(), pid); err != nil {
		t.Fatalf("rematerialize: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// B1 回归（插入侧）：种子缺 costRates 的新行，INSERT 不得写 cost_rates 列（落库取默认价，而非 JSON null）。
func TestService_Materialize_InsertWithoutSeedCostOmitsColumn(t *testing.T) {
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

	modelsJSON := `{"qwen-max":{"externalModelID":"2"}}`
	mock.ExpectQuery(cpByIdRe).WithArgs(pid).WillReturnRows(cpProtoRow(7, "https://x.example.com", modelsJSON))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE channel_protocol_id = $1 AND deleted_at IS NULL ORDER BY id ASC`)).
		WithArgs(pid).
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "c", Protocol: "openai-chat", ChannelProtocolID: &pid, Priority: 100, CreatedAt: now, UpdatedAt: now}))
	mock.ExpectQuery(`(?s)SELECT .* FROM channel_models WHERE channel_id=\$1 AND internal_model_id=\$2 AND deleted_at IS NULL ORDER BY id ASC LIMIT 1`).
		WithArgs(int64(1), "qwen-max").WillReturnError(sql.ErrNoRows)
	// 列清单里没有 cost_rates —— 严格匹配，写进去就会失败。
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, state) VALUES($1, $2, $3, $4, $5) RETURNING `+cmCols)).
		WithArgs(sqlmock.AnyArg(), int64(1), "qwen-max", int64(2), string(StateNormal)).
		WillReturnRows(cmRow(&ChannelModel{ID: 100, ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2, State: StateNormal, CreatedAt: now, UpdatedAt: now}, `{"input":0.8,"output":1.6,"cache_read":0.05}`))

	if err := svc.RematerializeProtocol(context.Background(), pid); err != nil {
		t.Fatalf("rematerialize: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// 渠道引用协议时 CreateChannel 自动物化 channel_models。
func TestService_CreateChannel_AutoMaterializesFromProtocol(t *testing.T) {
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
	modelsJSON := `{"qwen-max":{"externalModelID":"2"}}`

	// 引用协议存在性校验。
	mock.ExpectQuery(cpByIdRe).WithArgs(pid).WillReturnRows(cpProtoRow(7, "https://x.example.com", modelsJSON))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE name = $1`)).
		WithArgs("c1").WillReturnRows(sqlmock.NewRows(nil))
	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(insertChannelSQL)).
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "c1", ChannelProtocolID: &pid, Priority: 100, CreatedAt: now, UpdatedAt: now}))
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM channel_tags WHERE channel_id = $1`)).
		WithArgs(int64(1)).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectCommit()

	// 物化：再读协议 → 查模型 → 插入。
	mock.ExpectQuery(cpByIdRe).WithArgs(pid).WillReturnRows(cpProtoRow(7, "https://x.example.com", modelsJSON))
	mock.ExpectQuery(`(?s)SELECT .* FROM channel_models WHERE channel_id=\$1 AND internal_model_id=\$2 AND deleted_at IS NULL ORDER BY id ASC LIMIT 1`).
		WithArgs(int64(1), "qwen-max").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery(`INSERT INTO channel_models`).
		WillReturnRows(cmRow(&ChannelModel{ID: 200, ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2, State: StateNormal, CreatedAt: now, UpdatedAt: now}, `{}`))

	ch, err := svc.CreateChannel(context.Background(), ChannelInput{Name: "c1", ChannelProtocolID: &pid})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if ch.ID != 1 {
		t.Fatalf("unexpected channel: %+v", ch)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
