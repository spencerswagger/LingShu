// Package calllog 提供 call_logs 的**只读**访问。
//
// 灵枢 V2 第一期起，请求日志由代理侧（magpie）负责落库，灵枢不再写入 call_logs：
// 本包刻意不提供 Insert，只保留历史数据的查询能力，供管理端/开发者端账单详情、
// 会话调用列表展示既有记录（表与数据保留，零删除）。管理端日志页改为读代理侧
// 调用记录后，本包即退化为历史查询。
package calllog

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"time"
)

// CallLog 调用日志（call_logs 表行，与 0002_call_logs.sql 列对齐）。
// 与 billing_records 按 billing_id 1:1 关联；无账单（候选全部被拦截等拒绝场景）时 BillingID 为空字符串。
// 字段集合保持与历史表结构一致（不增不减），仅用于读取历史行。
type CallLog struct {
	BillingID       string // 关联 billing_records.billing_id（拒绝场景为空串；表上 UNIQUE 保证幂等）
	RequestID       string // 请求链路 ID（审计/日志关联）
	SessionID       string // 归属会话；空串 = 无会话
	UserID          *int64 // 消费用户（拒绝场景可为空）
	ChannelID       *int64 // 最终命中渠道（历史列，V2 起不再写入）
	ChannelKeyID    *int64 // 最终命中渠道密钥（历史列，V2 起不再写入）
	ExternalModelID *int64 // 最终命中对外模型
	InternalModelID string // 最终命中内部模型（历史列）
	Model           string // 用户请求的对外模型名
	AutoModelName   string // 请求的 Auto 名（历史列）
	PricingMode     string // sale / cost
	Status          string // completed / failed
	ReqMessages     any    // 请求增量 JSONB 原始值（可为 nil/空数组）
	ReqParams       []byte // 入站请求参数快照 JSONB 原始值
	RespBody        string // 非流式完整响应体；流式为结构化提取的 assistant 增量 JSON
	RespKind        string // non_stream / stream / error
	Decision        any    // 决策轨迹 JSONB 原始值
	UpstreamUsage   []byte // 上游原始 usage（JSONB 值）
	ErrorMessage    string
	DurationMs      *int64
	FirstTokenMs    *int64
	CreatedAt       time.Time
}

// Reader 调用日志只读查询抽象（SQLStore 满足），便于测试注入内存 fake。
type Reader interface {
	// GetByBillingID 按 billing_id 查询单条调用日志；无记录返回 (nil, nil)。
	GetByBillingID(ctx context.Context, billingID string) (*CallLog, error)
	// ListBySession 按 session_id 查询会话全部调用日志（created_at 升序）。
	ListBySession(ctx context.Context, sessionID string) ([]CallLog, error)
}

// callLogsCols 与 0002_call_logs.sql + 0011_auto_models.sql（auto_model_name）+ 0012（upstream_usage）
// + 0018（req_params）迁移后的列顺序一致的查询列。
const callLogsCols = `billing_id, request_id, session_id, user_id, channel_id, channel_key_id,
	internal_model_id, external_model_id, model, auto_model_name, pricing_mode, status, req_messages, resp_body, resp_kind,
	decision, error_message, duration_ms, first_token_ms, created_at, upstream_usage, req_params`

// SQLStore call_logs 的 PostgreSQL 只读实现。
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore 创建只读存储，db 为 pgx stdlib 连接池。
func NewSQLStore(db *sql.DB) *SQLStore {
	return &SQLStore{db: db}
}

// GetByBillingID 按 billing_id 查询单条调用日志；无记录返回 (nil, nil)。
func (s *SQLStore) GetByBillingID(ctx context.Context, billingID string) (*CallLog, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+callLogsCols+` FROM call_logs WHERE billing_id=$1`, billingID)
	c, err := scanCallLog(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return c, nil
}

// ListBySession 按 session_id 查询会话的全部调用日志（按 created_at 升序，会话完整记录）。
func (s *SQLStore) ListBySession(ctx context.Context, sessionID string) ([]CallLog, error) {
	if s == nil || s.db == nil {
		return nil, nil
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT `+callLogsCols+` FROM call_logs WHERE session_id=$1 ORDER BY created_at ASC`, sessionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]CallLog, 0, 8)
	for rows.Next() {
		var c CallLog
		if err := scanCallLogInto(rows, &c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// rowScanner 是 *sql.Row 与 *sql.Rows 共有的 Scan 接口（便于共用扫描函数）。
type rowScanner interface {
	Scan(dest ...any) error
}

// scanCallLog 扫描单行（QueryRow 场景，直接返回 *CallLog）。
func scanCallLog(row rowScanner) (*CallLog, error) {
	var c CallLog
	if err := scanCallLogInto(row, &c); err != nil {
		return nil, err
	}
	return &c, nil
}

// scanCallLogInto 把一行 call_logs 扫描进结构体；可空列用 sql.Null*，
// JSONB 列读为 []byte 保留原始 value（null 给 nil）。
func scanCallLogInto(s rowScanner, c *CallLog) error {
	var uid, chID, keyID, extID, dur, ft sql.NullInt64
	var autoName sql.NullString
	var reqRaw, decRaw, upRaw, rpRaw []byte
	if err := s.Scan(&c.BillingID, &c.RequestID, &c.SessionID, &uid, &chID, &keyID,
		&c.InternalModelID, &extID, &c.Model, &autoName, &c.PricingMode, &c.Status, &reqRaw, &c.RespBody, &c.RespKind,
		&decRaw, &c.ErrorMessage, &dur, &ft, &c.CreatedAt, &upRaw, &rpRaw); err != nil {
		return err
	}
	c.AutoModelName = autoName.String
	if uid.Valid {
		v := uid.Int64
		c.UserID = &v
	}
	if chID.Valid {
		v := chID.Int64
		c.ChannelID = &v
	}
	if keyID.Valid {
		v := keyID.Int64
		c.ChannelKeyID = &v
	}
	if extID.Valid {
		v := extID.Int64
		c.ExternalModelID = &v
	}
	if dur.Valid {
		v := dur.Int64
		c.DurationMs = &v
	}
	if ft.Valid {
		v := ft.Int64
		c.FirstTokenMs = &v
	}
	if len(reqRaw) > 0 {
		c.ReqMessages = json.RawMessage(reqRaw)
	}
	if len(decRaw) > 0 {
		c.Decision = json.RawMessage(decRaw)
	}
	if len(upRaw) > 0 {
		c.UpstreamUsage = upRaw
	}
	if len(rpRaw) > 0 {
		c.ReqParams = rpRaw
	}
	return nil
}
