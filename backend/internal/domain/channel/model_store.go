package channel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/idgen"
)

// ChannelModel 对应 channel_models 表一行：渠道 + 内部模型ID + 成本定价 + 绑定某对外模型。
// 运行时字段（State/RateLimit/HealthProbe/Reliability）与渠道级完全同构，0/空值由运行时兜底渠道级。
// TimeConfig/ContextTiers 可空，空则运行时回落全局。
type ChannelModel struct {
	ID              int64
	ChannelID       int64
	InternalModelID string
	ExternalModelID int64
	CostRates       billing.Rates
	TimeConfig      *billing.TimeCoeffConfig
	ContextTiers    []billing.TierRule
	State           State
	RateLimit       RateLimitConfig
	HealthProbe     HealthProbeConfig
	Reliability     ReliabilityConfig
	ExternalName    string // JOIN 展示用（对外模型名）
	ChannelName     string // JOIN 展示用（所属渠道名，仅 ListAll 查询填充）
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// cmCols 列出 channel_models 表查询使用的列。
const cmCols = `id, channel_id, internal_model_id, external_model_id, cost_rates,
	time_config, context_tiers, state, rate_limit, health_probe, reliability, created_at, updated_at`

// scanChannelModel 将一行扫描到 *ChannelModel，解析 JSONB 列。
func scanChannelModel(row interface{ Scan(...any) error }) (*ChannelModel, error) {
	var m ChannelModel
	var costRaw, timeRaw, tiersRaw, rlRaw, hpRaw, relRaw []byte
	var state string
	err := row.Scan(&m.ID, &m.ChannelID, &m.InternalModelID, &m.ExternalModelID,
		&costRaw, &timeRaw, &tiersRaw, &state, &rlRaw, &hpRaw, &relRaw,
		&m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return nil, err
	}
	m.State = State(state)
	if len(costRaw) > 0 {
		if err := json.Unmarshal(costRaw, &m.CostRates); err != nil {
			return nil, fmt.Errorf("parse cost_rates: %w", err)
		}
	}
	if len(timeRaw) > 0 && string(timeRaw) != "null" {
		var tc billing.TimeCoeffConfig
		if err := json.Unmarshal(timeRaw, &tc); err != nil {
			return nil, fmt.Errorf("parse time_config: %w", err)
		}
		m.TimeConfig = &tc
	}
	if len(tiersRaw) > 0 && string(tiersRaw) != "null" {
		if err := json.Unmarshal(tiersRaw, &m.ContextTiers); err != nil {
			return nil, fmt.Errorf("parse context_tiers: %w", err)
		}
	}
	if len(rlRaw) > 0 {
		if err := json.Unmarshal(rlRaw, &m.RateLimit); err != nil {
			return nil, fmt.Errorf("parse rate_limit: %w", err)
		}
	}
	if len(hpRaw) > 0 {
		if err := json.Unmarshal(hpRaw, &m.HealthProbe); err != nil {
			return nil, fmt.Errorf("parse health_probe: %w", err)
		}
	}
	if len(relRaw) > 0 {
		if err := json.Unmarshal(relRaw, &m.Reliability); err != nil {
			return nil, fmt.Errorf("parse reliability: %w", err)
		}
	}
	return &m, nil
}

// channelModelStore 提供 channel_models 表的基础数据访问。归属于 Store 结构（复用同一 db 连接池）。
//
// 由于 ChannelModel 与 Channel 同属本包 Store 的不同业务，这里用独立方法挂载在 *Store 上。
type channelModelStore struct {
	db *sql.DB
}

// NewChannelModelStore 创建渠道内部模型存储。
func NewChannelModelStore(db *sql.DB) *channelModelStore {
	return &channelModelStore{db: db}
}

const cmColsJoinExt = `cm.id, cm.channel_id, cm.internal_model_id, cm.external_model_id,
	cm.cost_rates, cm.time_config, cm.context_tiers, cm.state, cm.rate_limit, cm.health_probe,
	cm.reliability, cm.created_at, cm.updated_at, em.external_name`

func scanChannelModelJoinExt(row interface{ Scan(...any) error }) (*ChannelModel, error) {
	var m ChannelModel
	var costRaw, timeRaw, tiersRaw, rlRaw, hpRaw, relRaw []byte
	var state string
	err := row.Scan(&m.ID, &m.ChannelID, &m.InternalModelID, &m.ExternalModelID,
		&costRaw, &timeRaw, &tiersRaw, &state, &rlRaw, &hpRaw, &relRaw,
		&m.CreatedAt, &m.UpdatedAt, &m.ExternalName)
	if err != nil {
		return nil, err
	}
	m.State = State(state)
	if len(costRaw) > 0 {
		if err := json.Unmarshal(costRaw, &m.CostRates); err != nil {
			return nil, fmt.Errorf("parse cost_rates: %w", err)
		}
	}
	if len(timeRaw) > 0 && string(timeRaw) != "null" {
		var tc billing.TimeCoeffConfig
		if err := json.Unmarshal(timeRaw, &tc); err != nil {
			return nil, fmt.Errorf("parse time_config: %w", err)
		}
		m.TimeConfig = &tc
	}
	if len(tiersRaw) > 0 && string(tiersRaw) != "null" {
		if err := json.Unmarshal(tiersRaw, &m.ContextTiers); err != nil {
			return nil, fmt.Errorf("parse context_tiers: %w", err)
		}
	}
	if len(rlRaw) > 0 {
		if err := json.Unmarshal(rlRaw, &m.RateLimit); err != nil {
			return nil, fmt.Errorf("parse rate_limit: %w", err)
		}
	}
	if len(hpRaw) > 0 {
		if err := json.Unmarshal(hpRaw, &m.HealthProbe); err != nil {
			return nil, fmt.Errorf("parse health_probe: %w", err)
		}
	}
	if len(relRaw) > 0 {
		if err := json.Unmarshal(relRaw, &m.Reliability); err != nil {
			return nil, fmt.Errorf("parse reliability: %w", err)
		}
	}
	return &m, nil
}

// cmColsJoinExtChannel 列出「JOIN 对外模型 + 渠道」查询使用的列（ListAll 用）。
const cmColsJoinExtChannel = `cm.id, cm.channel_id, cm.internal_model_id, cm.external_model_id,
	cm.cost_rates, cm.time_config, cm.context_tiers, cm.state, cm.rate_limit, cm.health_probe,
	cm.reliability, cm.created_at, cm.updated_at, em.external_name, ch.name`

// scanChannelModelJoinExtChannel 将「JOIN 对外模型 + 渠道」的一行扫描到 *ChannelModel，解析 JSONB 列。
func scanChannelModelJoinExtChannel(row interface{ Scan(...any) error }) (*ChannelModel, error) {
	var m ChannelModel
	var costRaw, timeRaw, tiersRaw, rlRaw, hpRaw, relRaw []byte
	var state string
	err := row.Scan(&m.ID, &m.ChannelID, &m.InternalModelID, &m.ExternalModelID,
		&costRaw, &timeRaw, &tiersRaw, &state, &rlRaw, &hpRaw, &relRaw,
		&m.CreatedAt, &m.UpdatedAt, &m.ExternalName, &m.ChannelName)
	if err != nil {
		return nil, err
	}
	m.State = State(state)
	if len(costRaw) > 0 {
		if err := json.Unmarshal(costRaw, &m.CostRates); err != nil {
			return nil, fmt.Errorf("parse cost_rates: %w", err)
		}
	}
	if len(timeRaw) > 0 && string(timeRaw) != "null" {
		var tc billing.TimeCoeffConfig
		if err := json.Unmarshal(timeRaw, &tc); err != nil {
			return nil, fmt.Errorf("parse time_config: %w", err)
		}
		m.TimeConfig = &tc
	}
	if len(tiersRaw) > 0 && string(tiersRaw) != "null" {
		if err := json.Unmarshal(tiersRaw, &m.ContextTiers); err != nil {
			return nil, fmt.Errorf("parse context_tiers: %w", err)
		}
	}
	if len(rlRaw) > 0 {
		if err := json.Unmarshal(rlRaw, &m.RateLimit); err != nil {
			return nil, fmt.Errorf("parse rate_limit: %w", err)
		}
	}
	if len(hpRaw) > 0 {
		if err := json.Unmarshal(hpRaw, &m.HealthProbe); err != nil {
			return nil, fmt.Errorf("parse health_probe: %w", err)
		}
	}
	if len(relRaw) > 0 {
		if err := json.Unmarshal(relRaw, &m.Reliability); err != nil {
			return nil, fmt.Errorf("parse reliability: %w", err)
		}
	}
	return &m, nil
}

// GetByID 按主键查询渠道内部模型；不存在返回 sql.ErrNoRows。
func (s *channelModelStore) GetByID(ctx context.Context, id int64) (*ChannelModel, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+cmCols+` FROM channel_models WHERE id = $1 AND deleted_at IS NULL`, id)
	return scanChannelModel(row)
}

// GetByChannelAndInternal 按 渠道+内部模型ID 查询（计费/路由取单条成本定价用）。
// 不存在返回 sql.ErrNoRows。
func (s *channelModelStore) GetByChannelAndInternal(ctx context.Context, channelID int64, internalModelID string) (*ChannelModel, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+cmCols+` FROM channel_models WHERE channel_id=$1 AND internal_model_id=$2 AND deleted_at IS NULL ORDER BY id ASC LIMIT 1`,
		channelID, internalModelID)
	return scanChannelModel(row)
}

// ListByChannel 查询某渠道的全部内部模型（含禁用），按 id 升序。
func (s *channelModelStore) ListByChannel(ctx context.Context, channelID int64) ([]ChannelModel, error) {
	return s.list(ctx,
		`SELECT `+cmColsJoinExt+` FROM channel_models cm
		 JOIN external_models em ON em.id = cm.external_model_id
		 WHERE cm.channel_id = $1 AND cm.deleted_at IS NULL AND em.deleted_at IS NULL ORDER BY cm.id ASC`, channelID)
}

// ListByExternal 查询绑定某对外模型的全部渠道内部模型（非禁用行），供路由候选。
// 同一对外模型在同一渠道/密钥下可绑定多个内部模型：每行都是一个独立候选
// （禁用/排空某内部模型只跳过该行，同渠道/密钥的其他内部模型仍候选）。
func (s *channelModelStore) ListByExternal(ctx context.Context, externalModelID int64) ([]ChannelModel, error) {
	return s.listPlain(ctx,
		`SELECT `+cmCols+` FROM channel_models
		 WHERE external_model_id = $1 AND state != 'DISABLED' AND deleted_at IS NULL ORDER BY channel_id ASC, id ASC`, externalModelID)
}

// ListAll 查询全量渠道内部模型（跨渠道，含禁用），JOIN 出对外模型名与渠道名，按 渠道+id 升序。
func (s *channelModelStore) ListAll(ctx context.Context) ([]ChannelModel, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+cmColsJoinExtChannel+` FROM channel_models cm
		 LEFT JOIN external_models em ON em.id = cm.external_model_id
		 JOIN channels ch ON ch.id = cm.channel_id
		 WHERE cm.deleted_at IS NULL
		 ORDER BY cm.channel_id ASC, cm.id ASC`)
	if err != nil {
		return nil, fmt.Errorf("list channel models: %w", err)
	}
	defer rows.Close()
	list := make([]ChannelModel, 0, 8)
	for rows.Next() {
		m, err := scanChannelModelJoinExtChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("scan channel model: %w", err)
		}
		list = append(list, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// listPlain 用非 JOIN 的 10 列解析（ListByExternal 用）。
func (s *channelModelStore) listPlain(ctx context.Context, query string, args ...any) ([]ChannelModel, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list channel models: %w", err)
	}
	defer rows.Close()
	list := make([]ChannelModel, 0, 8)
	for rows.Next() {
		m, err := scanChannelModel(rows)
		if err != nil {
			return nil, fmt.Errorf("scan channel model: %w", err)
		}
		list = append(list, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// GetByChannelAndExternal 查询某渠道绑定某对外模型的第一条非禁用记录（ORDER BY id ASC LIMIT 1），
// 供路由选中渠道后取 internal_model_id。
func (s *channelModelStore) GetByChannelAndExternal(ctx context.Context, channelID, externalModelID int64) (*ChannelModel, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+cmCols+` FROM channel_models
		 WHERE channel_id=$1 AND external_model_id=$2 AND state != 'DISABLED' AND deleted_at IS NULL ORDER BY id ASC LIMIT 1`,
		channelID, externalModelID)
	return scanChannelModel(row)
}

func (s *channelModelStore) list(ctx context.Context, query string, args ...any) ([]ChannelModel, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list channel models: %w", err)
	}
	defer rows.Close()
	list := make([]ChannelModel, 0, 8)
	for rows.Next() {
		m, err := scanChannelModelJoinExt(rows)
		if err != nil {
			return nil, fmt.Errorf("scan channel model: %w", err)
		}
		list = append(list, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// ErrChannelModelExists 表示 (channel_id, internal_model_id) 唯一约束冲突（PG 23505）。
var ErrChannelModelExists = errors.New("channel model already exists")

// ErrExternalModelMissing 表示 external_model 外键不存在（PG 23503）。
var ErrExternalModelMissing = errors.New("external model not found")

// Insert 插入渠道内部模型并返回回填主键后的完整记录。
// (channel_id, internal_model_id) 冲突返回 ErrChannelModelExists；external_model 不存在返回 ErrExternalModelMissing。
func (s *channelModelStore) Insert(ctx context.Context, m *ChannelModel) (*ChannelModel, error) {
	if m.State == "" {
		m.State = StateNormal // 新模型默认正常
	}
	if m.ID == 0 {
		m.ID = idgen.New()
	}
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO channel_models(id, channel_id, internal_model_id, external_model_id, cost_rates, time_config, context_tiers, state, rate_limit, health_probe, reliability)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+cmCols,
		m.ID, m.ChannelID, m.InternalModelID, m.ExternalModelID, cmJSON(m.CostRates),
		cmNullableJSON(m.TimeConfig), cmNullableJSON(m.ContextTiers),
		string(m.State), marshalJSONB(m.RateLimit), marshalJSONB(m.HealthProbe),
		marshalJSONB(m.Reliability))
	created, err := scanChannelModel(row)
	if err != nil {
		return nil, classifyCMErr(err)
	}
	return created, nil
}

// Update 全量更新渠道内部模型可变字段，并刷新 updated_at。
func (s *channelModelStore) Update(ctx context.Context, m *ChannelModel) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE channel_models SET internal_model_id=$1, external_model_id=$2, cost_rates=$3,
		        time_config=$4, context_tiers=$5, state=$6, rate_limit=$7, health_probe=$8,
		        reliability=$9, updated_at=now()
		 WHERE id=$10`,
		m.InternalModelID, m.ExternalModelID, cmJSON(m.CostRates),
		cmNullableJSON(m.TimeConfig), cmNullableJSON(m.ContextTiers),
		string(m.State), marshalJSONB(m.RateLimit), marshalJSONB(m.HealthProbe),
		marshalJSONB(m.Reliability), m.ID)
	if err != nil {
		return classifyCMErr(err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateModelState 仅更新内部模型权威状态（状态机/手动操作持久化）。
func (s *channelModelStore) UpdateModelState(ctx context.Context, channelID int64, internalModelID string, st State) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE channel_models SET state=$1, updated_at=now() WHERE channel_id=$2 AND internal_model_id=$3`,
		string(st), channelID, internalModelID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// InsertModelEvent 记录一条内部模型状态流转事件。
func (s *channelModelStore) InsertModelEvent(ctx context.Context, e *ChannelModelEvent) (*ChannelModelEvent, error) {
	if e.ID == 0 {
		e.ID = idgen.New()
	}
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO channel_model_events(id, channel_id, model_id, from_state, to_state, reason)
		 VALUES($1,$2,$3,$4,$5,$6) RETURNING `+modelEventCols,
		e.ID, e.ChannelID, e.ModelID, string(e.FromState), string(e.ToState), e.Reason)
	var created ChannelModelEvent
	err := row.Scan(&created.ID, &created.ChannelID, &created.ModelID, &created.FromState,
		&created.ToState, &created.Reason, &created.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &created, nil
}

// ListModelEvents 查询某渠道的模型状态流转事件（倒序）。
func (s *channelModelStore) ListModelEvents(ctx context.Context, channelID int64) ([]ChannelModelEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+modelEventCols+` FROM channel_model_events WHERE channel_id=$1 ORDER BY id DESC`, channelID)
	if err != nil {
		return nil, fmt.Errorf("list model events: %w", err)
	}
	defer rows.Close()
	list := make([]ChannelModelEvent, 0, 8)
	for rows.Next() {
		var e ChannelModelEvent
		if err := rows.Scan(&e.ID, &e.ChannelID, &e.ModelID, &e.FromState, &e.ToState, &e.Reason, &e.CreatedAt); err != nil {
			return nil, err
		}
		list = append(list, e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// InsertProbeLog 落库一次探测记录（结果 + token 开销 + 层级）。probe_logs 唯一写入口。
// ProbeLog.ChannelKeyID 对应 probe_logs.channel_key_id（密钥维度）。
func (s *channelModelStore) InsertProbeLog(ctx context.Context, p *ProbeLog) error {
	if p.ID == 0 {
		p.ID = idgen.New()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO probe_logs(id, channel_key_id, model_id, level, target, ok, error, input_tokens, output_tokens, cached_tokens, total_tokens, duration_ms)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		p.ID, p.ChannelKeyID, p.ModelID, p.Level, p.Target, p.OK, p.Error,
		p.InputTokens, p.OutputTokens, p.CachedTokens, p.TotalTokens, p.DurationMS)
	return err
}

// ListProbeLogs 查询某密钥的探测历史（倒序）。keyID 对应 probe_logs.channel_key_id。
func (s *channelModelStore) ListProbeLogs(ctx context.Context, keyID int64, limit int) ([]ProbeLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, channel_key_id, model_id, level, target, ok, error, input_tokens, output_tokens, cached_tokens, total_tokens, duration_ms, created_at
		 FROM probe_logs WHERE channel_key_id=$1 ORDER BY id DESC LIMIT $2`, keyID, limit)
	if err != nil {
		return nil, fmt.Errorf("list probe logs: %w", err)
	}
	defer rows.Close()
	return s.scanProbeLogs(rows)
}

// ListProbeLogsByKeys 查询一批密钥（某渠道全部未删 keyID）的探测历史（倒序、合并列表），
// 控制台按渠道聚合读路径使用：先取渠道全部 keyID，再 ANY 查询，避免把渠道 ID 当 key ID 查询恒为空。
func (s *channelModelStore) ListProbeLogsByKeys(ctx context.Context, keyIDs []int64, limit int) ([]ProbeLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if len(keyIDs) == 0 {
		return []ProbeLog{}, nil
	}
	parts := make([]string, 0, len(keyIDs))
	for _, id := range keyIDs {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	anyArg := "{" + strings.Join(parts, ",") + "}"
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, channel_key_id, model_id, level, target, ok, error, input_tokens, output_tokens, cached_tokens, total_tokens, duration_ms, created_at
		 FROM probe_logs WHERE channel_key_id = ANY($1::bigint[]) ORDER BY id DESC LIMIT $2`, anyArg, limit)
	if err != nil {
		return nil, fmt.Errorf("list probe logs by keys: %w", err)
	}
	defer rows.Close()
	return s.scanProbeLogs(rows)
}

// scanProbeLogs 扫描探测日志行集（ListProbeLogs/ListProbeLogsByKeys 共用）。
func (s *channelModelStore) scanProbeLogs(rows interface {
	Next() bool
	Scan(...any) error
	Err() error
}) ([]ProbeLog, error) {
	list := make([]ProbeLog, 0, 16)
	for rows.Next() {
		var p ProbeLog
		if err := rows.Scan(&p.ID, &p.ChannelKeyID, &p.ModelID, &p.Level, &p.Target, &p.OK, &p.Error,
			&p.InputTokens, &p.OutputTokens, &p.CachedTokens, &p.TotalTokens, &p.DurationMS, &p.CreatedAt); err != nil {
			return nil, err
		}
		if p.Level == "" {
			p.Level = "model" // 旧数据兜底
		}
		list = append(list, p)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return list, nil
}

// Delete 软删除单条渠道内部模型。不存在返回 sql.ErrNoRows。
func (s *channelModelStore) Delete(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `UPDATE channel_models SET deleted_at = now() WHERE id=$1`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func classifyCMErr(err error) error {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "23505":
			return ErrChannelModelExists
		case "23503":
			return ErrExternalModelMissing
		}
	}
	return err
}

func cmJSON(v billing.Rates) []byte {
	b, _ := json.Marshal(v)
	if b == nil {
		b = []byte("{}")
	}
	return b
}

func cmNullableJSON(v any) any {
	if v == nil {
		return nil
	}
	if tc, ok := v.(*billing.TimeCoeffConfig); ok && tc == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return nil
	}
	return json.RawMessage(b)
}
