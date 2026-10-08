// Package channel 提供渠道（上游 LLM 供应商）的存储、CRUD、三态状态机、
// 健康探测与 RPM/TPM 内存限流。
//
// 数据模型对齐 db/migrations/0001_init.sql 中 channels / channel_events 表。
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
	"github.com/team/llmgateway/internal/pkg/idgen"
)

// State 渠道/内部模型三态。渠道级与内部模型级完全同构。
type State string

const (
	StateNormal   State = "NORMAL"
	StateDrain    State = "DRAIN"
	StateDisabled State = "DISABLED"
)

// Valid 校验状态是否被 DB CHECK / 状态机支持。
func (s State) Valid() bool {
	switch s {
	case StateNormal, StateDrain, StateDisabled:
		return true
	}
	return false
}

// 渠道协议支持列表。
const (
	ProtocolOpenAICompat = "openai-compat"
)

// RateLimitConfig 限流配置（rate_limit JSONB 反序列化目标）。
type RateLimitConfig struct {
	RPM             int     `json:"RPM"`             // 每分钟请求数；0 表示不限
	TPM             int     `json:"TPM"`             // 每分钟 token 数；0 表示不限
	BurstMultiplier float64 `json:"BurstMultiplier"` // 瞬时超发系数
	OnExceed        string  `json:"OnExceed"`        // QUEUE 或 REJECT
	QueueSize       int     `json:"QueueSize"`
	QueueTimeoutMS  int     `json:"QueueTimeoutMS"`
	MaxConcurrent   int     `json:"MaxConcurrent"` // 同时进行的请求上限（并发会话数）；0 表示不限
}

// HealthProbeConfig 健康探测配置（health_probe JSONB）。
// 探测与「可靠性」（真实调用窗口）是两个独立子系统：探测是主动行为，可靠性是被动统计。
type HealthProbeConfig struct {
	Interval             string `json:"Interval"`             // 正常态探测频率（6 段 cron，含秒）
	DrainIntervalSeconds int    `json:"DrainIntervalSeconds"` // 排空态探测频率（秒）
	TimeoutMS            int    `json:"TimeoutMS"`            // 单次探测超时
	FailThreshold        int    `json:"FailThreshold"`        // 连续失败阈值 → DRAIN，默认 1
	RecoveryThreshold    int    `json:"RecoveryThreshold"`    // 连续成功阈值 → NORMAL
	ProbeModel           string `json:"ProbeModel"`           // 渠道级显式探测模型；空则取渠道内内部模型
}

// ReliabilityConfig 可靠性配置（reliability JSONB）：真实调用的滑动窗口自动评估。
// 任一窗口指标超限（且样本数达标）→ DRAIN；不承担恢复职责（恢复仅由健康探测驱动）。
// 真实调用连续鉴权失败(401/403)达 AuthFailThreshold → DISABLED。
type ReliabilityConfig struct {
	WindowSeconds     int     `json:"WindowSeconds"`     // 滑动窗口时长（秒）
	MinSamples        int     `json:"MinSamples"`        // 窗口内最小样本数，不足不评估
	ErrorRatePct      float64 `json:"ErrorRatePct"`      // (失败+超时)/总数 阈值 %
	Rate429Pct        float64 `json:"Rate429Pct"`        // 429 占比阈值 %
	P99LatencyMS      int64   `json:"P99LatencyMS"`      // 成功样本 P99 耗时阈值 ms
	AuthFailThreshold int     `json:"AuthFailThreshold"` // 连续鉴权失败阈值 → DISABLED，默认 3
}

// ChannelKeyState 渠道视图中的密钥聚合项（key_states）：渠道列表逐密钥状态快照。
// KeyID/KeyName/State 取自运行时 KeyViews（内存权威）。
type ChannelKeyState struct {
	KeyID   int64  `json:"KeyID,string"`
	KeyName string `json:"KeyName"`
	State   State  `json:"State"`
}

// Channel 对应 channels 表的一行（配置模板，不含凭据；凭据收敛到 channel_keys.credential_enc）。
// 雪花 ID 超出 JS 安全整数，ID/tag_ids 等 ID 字段以字符串序列化。
type Channel struct {
	ID                int64             `json:"ID,string"`
	Name              string            `json:"Name"`
	Protocol          string            `json:"Protocol"`
	BaseURL           string            `json:"BaseURL"`
	Tags              map[string]string `json:"Tags"`
	TagIDs            idgen.IDs         `json:"TagIDs"`
	BoundTags         []TagRef          `json:"BoundTags"`
	Priority          int               `json:"Priority"`
	Weight            int               `json:"Weight"`
	State             State             `json:"State"`
	RateLimit         RateLimitConfig   `json:"RateLimit"`
	HealthProbe       HealthProbeConfig `json:"HealthProbe"`
	Reliability       ReliabilityConfig `json:"Reliability"`
	SessionTTLMinutes int               `json:"SessionTTLMinutes"` // 会话存活时长（分钟）；0 由运行时兜底 60
	KeyStates         []ChannelKeyState `json:"KeyStates"`
	CreatedAt         time.Time         `json:"CreatedAt"`
	UpdatedAt         time.Time         `json:"UpdatedAt"`
}

// ChannelEvent 对应 channel_events 表一行，记录一次渠道状态流转。
type ChannelEvent struct {
	ID        int64     `json:"ID,string"`
	ChannelID int64     `json:"ChannelID,string"`
	FromState State     `json:"FromState"`
	ToState   State     `json:"ToState"`
	Reason    string    `json:"Reason"`
	CreatedAt time.Time `json:"CreatedAt"`
}

// ChannelModelEvent 对应 channel_model_events 表一行，记录一次内部模型状态流转。
type ChannelModelEvent struct {
	ID        int64     `json:"ID,string"`
	ChannelID int64     `json:"ChannelID,string"`
	ModelID   string    `json:"ModelID"`
	FromState State     `json:"FromState"`
	ToState   State     `json:"ToState"`
	Reason    string    `json:"Reason"`
	CreatedAt time.Time `json:"CreatedAt"`
}

// ProbeLog 对应 probe_logs 表一行，记录一次健康探测（结果+开销，供后续统计）。
// ChannelKeyID 对应 probe_logs.channel_key_id（密钥维度；level=key 时为密钥默认探测目标）。
type ProbeLog struct {
	ID           int64     `json:"ID,string"`
	ChannelKeyID int64     `json:"ChannelKeyID,string"`
	ModelID      string    `json:"ModelID"` // 探测目标模型；空=渠道级探测
	Level        string    `json:"Level"`   // key=密钥级探测；model=模型级探测
	Target       string    `json:"Target"`
	OK           bool      `json:"OK"`
	Error        string    `json:"Error,omitempty"`
	InputTokens  int       `json:"InputTokens"`
	OutputTokens int       `json:"OutputTokens"`
	CachedTokens int       `json:"CachedTokens"`
	TotalTokens  int       `json:"TotalTokens"`
	DurationMS   int       `json:"DurationMS"`
	CreatedAt    time.Time `json:"CreatedAt"`
}

// channelCols 列出 channels 表查询时使用的全部列，保持各查询一致。
const channelCols = `id, name, protocol, base_url, tags, priority, weight, state, rate_limit, health_probe, reliability, session_ttl_minutes, created_at, updated_at`

const eventCols = `id, channel_id, from_state, to_state, reason, created_at`

const modelEventCols = `id, channel_id, model_id, from_state, to_state, reason, created_at`

// scanChannel 将一行扫描到 *Channel，解析 tags/rate_limit/health_probe/reliability JSONB。
func scanChannel(row interface{ Scan(...any) error }) (*Channel, error) {
	var c Channel
	var tagsRaw, rlRaw, hpRaw, relRaw []byte
	var state string
	err := row.Scan(&c.ID, &c.Name, &c.Protocol, &c.BaseURL,
		&tagsRaw, &c.Priority, &c.Weight, &state, &rlRaw, &hpRaw, &relRaw, &c.SessionTTLMinutes,
		&c.CreatedAt, &c.UpdatedAt)
	if err != nil {
		return nil, err
	}
	c.State = State(state)
	if len(tagsRaw) > 0 {
		if err := json.Unmarshal(tagsRaw, &c.Tags); err != nil {
			return nil, fmt.Errorf("parse tags: %w", err)
		}
	}
	if len(rlRaw) > 0 {
		if err := json.Unmarshal(rlRaw, &c.RateLimit); err != nil {
			return nil, fmt.Errorf("parse rate_limit: %w", err)
		}
	}
	if len(hpRaw) > 0 {
		if err := json.Unmarshal(hpRaw, &c.HealthProbe); err != nil {
			return nil, fmt.Errorf("parse health_probe: %w", err)
		}
	}
	if len(relRaw) > 0 {
		if err := json.Unmarshal(relRaw, &c.Reliability); err != nil {
			return nil, fmt.Errorf("parse reliability: %w", err)
		}
	}
	return &c, nil
}

func scanEvent(row interface{ Scan(...any) error }) (*ChannelEvent, error) {
	var e ChannelEvent
	var from, to string
	err := row.Scan(&e.ID, &e.ChannelID, &from, &to, &e.Reason, &e.CreatedAt)
	if err != nil {
		return nil, err
	}
	e.FromState = State(from)
	e.ToState = State(to)
	return &e, nil
}

// marshalJSONB 将值序列化为 JSON，供 JSONB 列写入。写空值会序列化为 '{}' 而非 null。
func marshalJSONB(v any) []byte {
	b, _ := json.Marshal(v)
	if b == nil {
		b = []byte("{}")
	}
	return b
}

// Store 提供 channels / channel_events 表的基础数据访问。
type Store struct {
	db *sql.DB
}

// NewStore 创建渠道存储，db 为 pgx stdlib 连接池。
func NewStore(db *sql.DB) *Store {
	return &Store{db: db}
}

// Get 按主键查询渠道；不存在返回 sql.ErrNoRows。
func (s *Store) Get(ctx context.Context, id int64) (*Channel, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+channelCols+` FROM channels WHERE id = $1 AND deleted_at IS NULL`, id)
	return scanChannel(row)
}

// GetByName 按名称查询渠道；不存在返回 sql.ErrNoRows。
func (s *Store) GetByName(ctx context.Context, name string) (*Channel, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+channelCols+` FROM channels WHERE name = $1 AND deleted_at IS NULL`, name)
	return scanChannel(row)
}

// List 按条件查询渠道。state 非空时按 state 过滤。
func (s *Store) List(ctx context.Context, state State) ([]Channel, error) {
	query := `SELECT ` + channelCols + ` FROM channels WHERE deleted_at IS NULL`
	args := []any{}
	if state != "" {
		query += ` AND state = $1`
		args = append(args, string(state))
	}
	query += ` ORDER BY priority DESC, id ASC`

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	defer rows.Close()

	channels := make([]Channel, 0, 8)
	for rows.Next() {
		c, err := scanChannel(rows)
		if err != nil {
			return nil, fmt.Errorf("scan channel: %w", err)
		}
		channels = append(channels, *c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return channels, nil
}

// ErrNameExists 表示渠道名称唯一约束冲突（PG 23505）。
var ErrNameExists = errors.New("channel name already exists")

// Insert 插入新渠道并返回回填主键后完整记录，name 冲突返回 ErrNameExists。
func (s *Store) Insert(ctx context.Context, c *Channel) (*Channel, error) {
	if c.ID == 0 {
		c.ID = idgen.New()
	}
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO channels(id, name, protocol, base_url, tags, priority, weight, state, rate_limit, health_probe, reliability, session_ttl_minutes)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 RETURNING `+channelCols,
		c.ID, c.Name, c.Protocol, c.BaseURL, marshalJSONB(c.Tags),
		c.Priority, c.Weight, string(c.State), marshalJSONB(c.RateLimit), marshalJSONB(c.HealthProbe),
		marshalJSONB(c.Reliability), c.SessionTTLMinutes)
	created, err := scanChannel(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrNameExists
		}
		return nil, err
	}
	return created, nil
}

// Update 全量更新渠道可变字段（不含 id/created_at）。name 冲突返回 ErrNameExists。
func (s *Store) Update(ctx context.Context, c *Channel) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE channels SET name=$1, protocol=$2, base_url=$3,
		        tags=$4, priority=$5, weight=$6, state=$7, rate_limit=$8, health_probe=$9,
		        reliability=$10, session_ttl_minutes=$11, updated_at=now()
		 WHERE id=$12`,
		c.Name, c.Protocol, c.BaseURL, marshalJSONB(c.Tags),
		c.Priority, c.Weight, string(c.State), marshalJSONB(c.RateLimit), marshalJSONB(c.HealthProbe),
		marshalJSONB(c.Reliability), c.SessionTTLMinutes, c.ID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrNameExists
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// UpdateState 仅更新渠道权威状态并刷新 updated_at，供状态机/手动操作持久化使用。
func (s *Store) UpdateState(ctx context.Context, id int64, st State) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE channels SET state=$1, updated_at=now() WHERE id=$2`, string(st), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// InsertEvent 记录一条渠道状态流转事件，返回回填主键后的完整事件。
func (s *Store) InsertEvent(ctx context.Context, e *ChannelEvent) (*ChannelEvent, error) {
	if e.ID == 0 {
		e.ID = idgen.New()
	}
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO channel_events(id, channel_id, from_state, to_state, reason)
		 VALUES($1,$2,$3,$4,$5) RETURNING `+eventCols,
		e.ID, e.ChannelID, string(e.FromState), string(e.ToState), e.Reason)
	created, err := scanEvent(row)
	if err != nil {
		return nil, err
	}
	return created, nil
}

// ListEvents 按渠道查询事件（倒序），用于控制台展示状态变更历史。
func (s *Store) ListEvents(ctx context.Context, channelID int64) ([]ChannelEvent, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+eventCols+` FROM channel_events WHERE channel_id=$1 ORDER BY id DESC`, channelID)
	if err != nil {
		return nil, fmt.Errorf("list events: %w", err)
	}
	defer rows.Close()

	events := make([]ChannelEvent, 0, 8)
	for rows.Next() {
		e, err := scanEvent(rows)
		if err != nil {
			return nil, fmt.Errorf("scan event: %w", err)
		}
		events = append(events, *e)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return events, nil
}

// DeleteChannels 批量软删除渠道，返回删除行数。软删除 channels 行、其 channel_keys 与 channel_models，
// 历史 billing_records 保留（审计不丢失；列表展示渠道名时不按 deleted_at 过滤，仍可回显）。
// channel_keys 级联软删保证：删除渠道后其密钥不再参与路由/探测/会话（内存运行时由 manager.Drop 一并摘除）。
func (s *Store) DeleteChannels(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	anyArg := "{" + strings.Join(parts, ",") + "}"

	var res sql.Result
	for _, stmt := range []string{
		`DELETE FROM channel_tags WHERE channel_id = ANY($1::bigint[])`,
		`UPDATE channel_models SET deleted_at = now() WHERE channel_id = ANY($1::bigint[])`,
		`UPDATE channel_keys SET deleted_at = now() WHERE channel_id = ANY($1::bigint[])`,
		`UPDATE channels SET deleted_at = now() WHERE id = ANY($1::bigint[])`,
	} {
		res, err = tx.ExecContext(ctx, stmt, anyArg)
		if err != nil {
			return 0, fmt.Errorf("soft delete cascade: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TagRef 渠道已绑定标签的解析结果（channel 域不依赖 tag 包，故自带结构）。
type TagRef struct {
	ID   int64             `json:"ID,string"`
	Name string            `json:"Name"`
	KV   map[string]string `json:"KV,omitempty"`
}

// ListTagRefsByChannel 查询某渠道已绑定标签（含 KV），按 tag_id 升序。
func (s *Store) ListTagRefsByChannel(ctx context.Context, channelID int64) ([]TagRef, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT ct.tag_id, st.name, st.kv_pairs
		   FROM channel_tags ct
		   JOIN semantic_tags st ON st.id = ct.tag_id AND st.deleted_at IS NULL
		  WHERE ct.channel_id = $1
		  ORDER BY ct.tag_id ASC`, channelID)
	if err != nil {
		return nil, fmt.Errorf("list channel tag refs: %w", err)
	}
	defer rows.Close()
	return scanTagRefs(rows)
}

// ListTagRefsByChannels 批量查询多个渠道的绑定标签。
func (s *Store) ListTagRefsByChannels(ctx context.Context, channelIDs []int64) (map[int64][]TagRef, error) {
	out := make(map[int64][]TagRef, len(channelIDs))
	if len(channelIDs) == 0 {
		return out, nil
	}
	parts := make([]string, 0, len(channelIDs))
	for _, id := range channelIDs {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	anyArg := "{" + strings.Join(parts, ",") + "}"
	rows, err := s.db.QueryContext(ctx,
		`SELECT ct.channel_id, ct.tag_id, st.name, st.kv_pairs
		   FROM channel_tags ct
		   JOIN semantic_tags st ON st.id = ct.tag_id AND st.deleted_at IS NULL
		  WHERE ct.channel_id = ANY($1::bigint[])
		  ORDER BY ct.channel_id ASC, ct.tag_id ASC`, anyArg)
	if err != nil {
		return nil, fmt.Errorf("list channel tag refs batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var chID, tagID int64
		var name string
		var kvRaw []byte
		if err := rows.Scan(&chID, &tagID, &name, &kvRaw); err != nil {
			return nil, fmt.Errorf("scan tag ref: %w", err)
		}
		kv := map[string]string{}
		if len(kvRaw) > 0 {
			if err := json.Unmarshal(kvRaw, &kv); err != nil {
				return nil, fmt.Errorf("parse tag kv: %w", err)
			}
		}
		out[chID] = append(out[chID], TagRef{ID: tagID, Name: name, KV: kv})
	}
	return out, rows.Err()
}

func scanTagRefs(rows *sql.Rows) ([]TagRef, error) {
	refs := make([]TagRef, 0, 4)
	for rows.Next() {
		var r TagRef
		var kvRaw []byte
		if err := rows.Scan(&r.ID, &r.Name, &kvRaw); err != nil {
			return nil, err
		}
		r.KV = map[string]string{}
		if len(kvRaw) > 0 {
			if err := json.Unmarshal(kvRaw, &r.KV); err != nil {
				return nil, fmt.Errorf("parse tag kv: %w", err)
			}
		}
		refs = append(refs, r)
	}
	return refs, rows.Err()
}

// ReplaceChannelTags 在给定事务（nil 则新建事务）内全量替换渠道绑定标签。
func (s *Store) ReplaceChannelTags(ctx context.Context, tx *sql.Tx, channelID int64, tagIDs []int64) error {
	own := tx == nil
	if own {
		var err error
		tx, err = s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM channel_tags WHERE channel_id = $1`, channelID); err != nil {
		return fmt.Errorf("clear channel tags: %w", err)
	}
	seen := make(map[int64]bool, len(tagIDs))
	for _, id := range tagIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO channel_tags(channel_id, tag_id) VALUES ($1,$2)`, channelID, id); err != nil {
			return fmt.Errorf("insert channel tag: %w", err)
		}
	}
	if own {
		return tx.Commit()
	}
	return nil
}

// CountChannelRefs 统计给定标签被多少渠道绑定。
func (s *Store) CountChannelRefs(ctx context.Context, tagIDs []int64) (int64, error) {
	if len(tagIDs) == 0 {
		return 0, nil
	}
	parts := make([]string, 0, len(tagIDs))
	for _, id := range tagIDs {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	anyArg := "{" + strings.Join(parts, ",") + "}"
	var n int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM channel_tags WHERE tag_id = ANY($1::bigint[])`, anyArg).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count channel refs: %w", err)
	}
	return n, nil
}

// InsertWithTags 在一个事务内插入渠道并全量替换标签绑定。
func (s *Store) InsertWithTags(ctx context.Context, c *Channel, tagIDs []int64) (*Channel, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if c.ID == 0 {
		c.ID = idgen.New()
	}
	row := tx.QueryRowContext(ctx,
		`INSERT INTO channels(id, name, protocol, base_url, tags, priority, weight, state, rate_limit, health_probe, reliability, session_ttl_minutes)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		 RETURNING `+channelCols,
		c.ID, c.Name, c.Protocol, c.BaseURL, marshalJSONB(c.Tags),
		c.Priority, c.Weight, string(c.State), marshalJSONB(c.RateLimit), marshalJSONB(c.HealthProbe),
		marshalJSONB(c.Reliability), c.SessionTTLMinutes)
	created, err := scanChannel(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrNameExists
		}
		return nil, err
	}
	if err := s.replaceChannelTagsTx(ctx, tx, created.ID, tagIDs); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return created, nil
}

// UpdateWithTags 在一个事务内更新渠道并全量替换标签绑定。
func (s *Store) UpdateWithTags(ctx context.Context, c *Channel, tagIDs []int64) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE channels SET name=$1, protocol=$2, base_url=$3,
		        tags=$4, priority=$5, weight=$6, state=$7, rate_limit=$8, health_probe=$9,
		        reliability=$10, session_ttl_minutes=$11, updated_at=now()
		 WHERE id=$12 AND deleted_at IS NULL`,
		c.Name, c.Protocol, c.BaseURL, marshalJSONB(c.Tags),
		c.Priority, c.Weight, string(c.State), marshalJSONB(c.RateLimit), marshalJSONB(c.HealthProbe),
		marshalJSONB(c.Reliability), c.SessionTTLMinutes, c.ID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrNameExists
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	if err := s.replaceChannelTagsTx(ctx, tx, c.ID, tagIDs); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) replaceChannelTagsTx(ctx context.Context, tx *sql.Tx, channelID int64, tagIDs []int64) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM channel_tags WHERE channel_id = $1`, channelID); err != nil {
		return fmt.Errorf("clear channel tags: %w", err)
	}
	seen := make(map[int64]bool, len(tagIDs))
	for _, id := range tagIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO channel_tags(channel_id, tag_id) VALUES ($1,$2)`, channelID, id); err != nil {
			return fmt.Errorf("insert channel tag: %w", err)
		}
	}
	return nil
}
