package channel

import (
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/pkg/resp"
)

var testSM4Key = []byte("0123456789abcdef") // 16 字节测试密钥（密钥运行时解密用）

func channelRow(c *Channel) *sqlmock.Rows {
	tags, _ := json.Marshal(c.Tags)
	rl, _ := json.Marshal(c.RateLimit)
	hp, _ := json.Marshal(c.HealthProbe)
	rel, _ := json.Marshal(c.Reliability)
	return sqlmock.NewRows([]string{"id", "name", "protocol", "base_url",
		"tags", "priority", "weight", "state", "rate_limit", "health_probe", "reliability",
		"session_ttl_minutes", "created_at", "updated_at"}).
		AddRow(c.ID, c.Name, c.Protocol, c.BaseURL, tags, c.Priority, c.Weight,
			string(c.State), rl, hp, rel, c.SessionTTLMinutes, c.CreatedAt, c.UpdatedAt)
}

func asAPIError(err error) (*APIError, bool) {
	if err == nil {
		return nil, false
	}
	ae, ok := err.(*APIError)
	return ae, ok
}

const insertChannelSQL = `INSERT INTO channels(id, name, protocol, base_url, tags, priority, weight, state, rate_limit, health_probe, reliability, session_ttl_minutes)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
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

	mock.ExpectBegin()
	mock.ExpectQuery(regexp.QuoteMeta(insertChannelSQL)).
		WithArgs(sqlmock.AnyArg(), "my-channel", "openai-compat", "https://api.example.com",
			sqlmock.AnyArg(), 100, 1, "NORMAL", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), DefaultSessionTTLMinutes).
		WillReturnRows(channelRow(&Channel{
			ID: 1, Name: "my-channel", Protocol: "openai-compat", BaseURL: "https://api.example.com",
			Priority: 100, Weight: 1, State: StateNormal,
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
	if ch.State != StateNormal {
		t.Fatalf("expected HEALTHY default, got %s", ch.State)
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
				int64(100), int64(1), "NORMAL", []byte("{}"), []byte("{}"), []byte("{}"),
				int64(60), time.Now(), time.Now()))
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
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "dup", State: StateNormal}))

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
			Priority: 50, State: StateNormal,
			CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}))

	updQuery := `UPDATE channels SET name=$1, protocol=$2, base_url=$3,
		        tags=$4, priority=$5, weight=$6, state=$7, rate_limit=$8, health_probe=$9,
		        reliability=$10, session_ttl_minutes=$11, updated_at=now()
		 WHERE id=$12 AND deleted_at IS NULL`
	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(updQuery)).
		WithArgs("renamed", "openai-compat", "https://b.example.com",
			sqlmock.AnyArg(), 50, 0, "NORMAL", sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), int64(0), int64(5)).
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
			Priority: 100, State: StateNormal, CreatedAt: now, UpdatedAt: now,
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

func TestService_ForceState_BatchKeys(t *testing.T) {
	// 渠道级批量状态：枚举两密钥逐一 SetState + InsertEvent，最后写一条渠道级事件（顺序断言）。
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewService(NewStore(db))
	svc.SetKeyService(NewKeyService(NewKeyStore(db), testSM4Key))

	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE id = $1 AND deleted_at IS NULL`)).
		WithArgs(int64(1)).
		WillReturnRows(channelRow(&Channel{
			ID: 1, Name: "ch", Protocol: "openai-compat", BaseURL: "https://a.example.com",
			Priority: 100, State: StateNormal, CreatedAt: now, UpdatedAt: now,
		}))

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE channel_id=$1 AND deleted_at IS NULL ORDER BY id`)).
		WithArgs(int64(1)).
		WillReturnRows(channelKeyRow(&ChannelKey{
			ID: 10, ChannelID: 1, Name: "k1", CredentialEnc: "enc", State: StateNormal,
			CreatedAt: now, UpdatedAt: now,
		}).AddRow(int64(11), int64(1), "k2", "enc", string(StateNormal), "", nil, now, now))

	for _, k := range []struct {
		id   int64
		name string
	}{{10, "k1"}, {11, "k2"}} {
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`)).
			WithArgs(k.id).
			WillReturnRows(channelKeyRow(&ChannelKey{
				ID: k.id, ChannelID: 1, Name: k.name, CredentialEnc: "enc", State: StateNormal,
				CreatedAt: now, UpdatedAt: now,
			}))
		mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET state=$1, last_err=$2, updated_at=now() WHERE id=$3`)).
			WithArgs(string(StateDisabled), "", k.id).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO channel_key_events(id, channel_key_id, from_state, to_state, reason) VALUES($1,$2,$3,$4,$5)`)).
			WithArgs(sqlmock.AnyArg(), k.id, string(StateNormal), string(StateDisabled), "manual_disable").
			WillReturnResult(sqlmock.NewResult(10, 1))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`)).
			WithArgs(k.id).
			WillReturnRows(channelKeyRow(&ChannelKey{
				ID: k.id, ChannelID: 1, Name: k.name, CredentialEnc: "enc", State: StateDisabled,
				CreatedAt: now, UpdatedAt: now,
			}))
	}

	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO channel_events(id, channel_id, from_state, to_state, reason)
		 VALUES($1,$2,$3,$4,$5) RETURNING `+eventCols)).
		WithArgs(sqlmock.AnyArg(), int64(1), string(StateNormal), string(StateDisabled), "manual disable").
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "from_state", "to_state", "reason", "created_at"}).
			AddRow(int64(30), int64(1), string(StateNormal), string(StateDisabled), "manual disable", now))

	ch, err := svc.ForceState(context.Background(), 1, ActionDisable, 0, "")
	if err != nil {
		t.Fatalf("force state: %v", err)
	}
	if ch.ID != 1 {
		t.Fatalf("unexpected channel view: %+v", ch)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestService_ForceState_BatchKeys_SyncMemory 渠道级批量状态落库后必须同步内存 KeyRuntime：
// ForceChannelStateBatch 把该渠道全部密钥状态机切到目标状态（路由/探测只读 Machine.State()），
// 手动切换渠道状态立即对运行时生效。回归：审查发现批量落库后内存 KeyRuntime 未联动。
func TestService_ForceState_BatchKeys_SyncMemory(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	// 内存运行时：渠道 1 + 密钥 11/12（机器初态 NORMAL）。
	mgr := NewManager(nil, nil, testSM4Key, nil, nil, time.Now)
	ch := &Channel{ID: 1, Name: "ch", Protocol: "openai-compat", BaseURL: "https://a.example.com", State: StateNormal}
	mgr.Upsert(ch)
	mgr.UpsertKey(ChannelKey{ID: 11, ChannelID: 1, Name: "k1", State: StateNormal})
	mgr.UpsertKey(ChannelKey{ID: 12, ChannelID: 1, Name: "k2", State: StateNormal})

	svc := NewService(NewStore(db))
	svc.SetKeyService(NewKeyService(NewKeyStore(db), testSM4Key))
	svc.SetManager(mgr)

	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE id = $1 AND deleted_at IS NULL`)).
		WithArgs(int64(1)).
		WillReturnRows(channelRow(&Channel{
			ID: 1, Name: "ch", Protocol: "openai-compat", BaseURL: "https://a.example.com",
			Priority: 100, State: StateNormal, CreatedAt: now, UpdatedAt: now,
		}))

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE channel_id=$1 AND deleted_at IS NULL ORDER BY id`)).
		WithArgs(int64(1)).
		WillReturnRows(channelKeyRow(&ChannelKey{
			ID: 11, ChannelID: 1, Name: "k1", CredentialEnc: "enc", State: StateNormal,
			CreatedAt: now, UpdatedAt: now,
		}).AddRow(int64(12), int64(1), "k2", "enc", string(StateNormal), "", nil, now, now))

	for _, k := range []struct {
		id   int64
		name string
	}{{11, "k1"}, {12, "k2"}} {
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`)).
			WithArgs(k.id).
			WillReturnRows(channelKeyRow(&ChannelKey{
				ID: k.id, ChannelID: 1, Name: k.name, CredentialEnc: "enc", State: StateNormal,
				CreatedAt: now, UpdatedAt: now,
			}))
		mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET state=$1, last_err=$2, updated_at=now() WHERE id=$3`)).
			WithArgs(string(StateDisabled), "", k.id).WillReturnResult(sqlmock.NewResult(0, 1))
		mock.ExpectExec(regexp.QuoteMeta(`INSERT INTO channel_key_events(id, channel_key_id, from_state, to_state, reason) VALUES($1,$2,$3,$4,$5)`)).
			WithArgs(sqlmock.AnyArg(), k.id, string(StateNormal), string(StateDisabled), "manual_disable").
			WillReturnResult(sqlmock.NewResult(10, 1))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`)).
			WithArgs(k.id).
			WillReturnRows(channelKeyRow(&ChannelKey{
				ID: k.id, ChannelID: 1, Name: k.name, CredentialEnc: "enc", State: StateDisabled,
				CreatedAt: now, UpdatedAt: now,
			}))
	}

	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO channel_events(id, channel_id, from_state, to_state, reason)
		 VALUES($1,$2,$3,$4,$5) RETURNING `+eventCols)).
		WithArgs(sqlmock.AnyArg(), int64(1), string(StateNormal), string(StateDisabled), "manual disable").
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "from_state", "to_state", "reason", "created_at"}).
			AddRow(int64(30), int64(1), string(StateNormal), string(StateDisabled), "manual disable", now))

	if _, err := svc.ForceState(context.Background(), 1, ActionDisable, 0, ""); err != nil {
		t.Fatalf("force state: %v", err)
	}

	// 内存 KeyRuntime 已联动：两密钥状态机均 DISABLED（路由入口读 kr.Machine.State() 即拒绝）。
	krA, _ := mgr.GetKeyRuntime(11)
	krB, _ := mgr.GetKeyRuntime(12)
	if krA.Machine.State() != StateDisabled || krB.Machine.State() != StateDisabled {
		t.Fatalf("批量状态后内存 KeyRuntime 应为 DISABLED，got key11=%s key12=%s",
			krA.Machine.State(), krB.Machine.State())
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}
