package gateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/pkg/idgen"
	"github.com/team/llmgateway/internal/pkg/logger"
)

// CallLog 调用日志（call_logs 表行，与 0002_call_logs.sql 列对齐）。
// 与 billing_records 按 billing_id 1:1 关联；无账单（候选全部被拦截等拒绝场景）时 BillingID 为空字符串。
type CallLog struct {
	BillingID       string // 关联 billing_records.billing_id（拒绝场景为空串；表上 UNIQUE 保证幂等）
	RequestID       string // 网关请求链路 ID（审计/日志关联）
	SessionID       string // 归属会话；空串 = 无会话
	UserID          *int64 // 消费用户（拒绝场景可为空）
	ChannelID       *int64 // 最终命中渠道
	ChannelKeyID    *int64 // 最终命中渠道密钥
	ExternalModelID *int64 // 最终命中对外模型
	InternalModelID string // 最终命中内部模型
	Model           string // 用户请求的对外模型名
	PricingMode     string // sale / cost
	Status          string // completed / failed
	ReqMessages     any    // 请求增量 JSONB 原始值（可为 nil/空数组）
	RespBody        string // 非流式完整响应体；流式为结构化提取的 assistant 增量 JSON
	RespKind        string // non_stream / stream / error
	Decision        any    // 决策轨迹 JSONB 原始值（候选渠道与原因、系数、预扣与结果）
	ErrorMessage    string
	DurationMs      *int64
	FirstTokenMs    *int64
	CreatedAt       time.Time
}

// CallLogStore 调用日志写入抽象（SQLCallLogStore 满足），便于测试注入内存 fake。
type CallLogStore interface {
	// Insert 幂等写入：与 billing_records 按 billing_id 1:1，冲突时静默跳过。
	Insert(ctx context.Context, c *CallLog) error
}

// SQLCallLogStore 调用日志的 PostgreSQL 存储实现。
type SQLCallLogStore struct {
	db *sql.DB
}

// NewSQLCallLogStore 创建调用日志存储，db 为 pgx stdlib 连接池。
func NewSQLCallLogStore(db *sql.DB) *SQLCallLogStore {
	return &SQLCallLogStore{db: db}
}

// callLogsCols 与 0002_call_logs.sql 列顺序一致的查询列（不含 id/created_at 自动列的读侧拆分）。
const callLogsCols = `billing_id, request_id, session_id, user_id, channel_id, channel_key_id,
	internal_model_id, external_model_id, model, pricing_mode, status, req_messages, resp_body, resp_kind,
	decision, error_message, duration_ms, first_token_ms, created_at`

// Insert 幂等插入一条调用日志：billing_id 冲突（ON CONFLICT DO NOTHING）静默跳过；
// billing_id 为空串（拒绝场景）同样允许，靠 UNIQUE 约束保证拒绝场景也不重复落日志。
func (s *SQLCallLogStore) Insert(ctx context.Context, c *CallLog) error {
	if s == nil || s.db == nil || c == nil {
		return nil
	}
	id := idgen.New()
	created := c.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	reqRaw, err := marshalCallLogValue(c.ReqMessages)
	if err != nil {
		return err
	}
	decRaw, err := marshalCallLogValue(c.Decision)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
INSERT INTO call_logs(id, billing_id, request_id, session_id, user_id, channel_id, channel_key_id,
	internal_model_id, external_model_id, model, pricing_mode, status, req_messages, resp_body, resp_kind,
	decision, error_message, duration_ms, first_token_ms, created_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
ON CONFLICT (billing_id) DO NOTHING`,
		id, c.BillingID, c.RequestID, c.SessionID, nullableInt64(c.UserID), nullableInt64(c.ChannelID),
		nullableInt64(c.ChannelKeyID), c.InternalModelID, nullableInt64(c.ExternalModelID), c.Model,
		c.PricingMode, c.Status, reqRaw, c.RespBody, c.RespKind, decRaw, c.ErrorMessage,
		nullableInt64(c.DurationMs), nullableInt64(c.FirstTokenMs), created)
	return err
}

// nullableInt64 指针 int64 → 可空参数（nil → nil）。
func nullableInt64(v *int64) any {
	if v == nil {
		return nil
	}
	return *v
}

// marshalCallLogValue 把 JSONB 字段（any/map/slice/RawMessage）序列化为 []byte；nil 返回 nil。
func marshalCallLogValue(v any) ([]byte, error) {
	if v == nil {
		return nil, nil
	}
	return json.Marshal(v)
}

// GetByBillingID 按 billing_id 查询单条调用日志；无记录返回 (nil, nil)。
func (s *SQLCallLogStore) GetByBillingID(ctx context.Context, billingID string) (*CallLog, error) {
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
func (s *SQLCallLogStore) ListBySession(ctx context.Context, sessionID string) ([]CallLog, error) {
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

// scanCallLogInto 把一行 call_logs 扫描进结构体；user_id 等可空列用 sql.NullInt64，
// JSONB 列读为 []byte 保留原始 value（查询侧再反序列化，null 给 nil）。
func scanCallLogInto(s rowScanner, c *CallLog) error {
	var uid, chID, keyID, extID, dur, ft sql.NullInt64
	var reqRaw, decRaw []byte
	if err := s.Scan(&c.BillingID, &c.RequestID, &c.SessionID, &uid, &chID, &keyID,
		&c.InternalModelID, &extID, &c.Model, &c.PricingMode, &c.Status, &reqRaw, &c.RespBody, &c.RespKind,
		&decRaw, &c.ErrorMessage, &dur, &ft, &c.CreatedAt); err != nil {
		return err
	}
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
	return nil
}

// msgFingerprint 单条消息全文的短 hash：sha256 前 16 字节 hex，用于 call_log 增量 diff 对齐。
func msgFingerprint(msgBytes []byte) string {
	h := sha256.Sum256(msgBytes)
	return hex.EncodeToString(h[:16])
}

// diffMessages 与上次指纹对齐做增量 diff：
// 逐条与 prev 对齐，hash 相同跳过；一旦不匹配或 prev 耗尽，从该位置起全部算增量。
func diffMessages(prev []string, msgs []json.RawMessage) []json.RawMessage {
	start := 0
	for start < len(prev) && start < len(msgs) {
		if msgFingerprint(msgs[start]) != prev[start] {
			break
		}
		start++
	}
	if start >= len(msgs) {
		return []json.RawMessage{}
	}
	out := make([]json.RawMessage, 0, len(msgs)-start)
	out = append(out, msgs[start:]...)
	return out
}

// fallbackReqIncrement 无上次指纹时的增量降级：取最后一条 user 消息；
// 若其后紧随 tool 角色，则把最近的连续 tool 链一并收起（工具调用回传场景）。
func fallbackReqIncrement(msgs []json.RawMessage) []json.RawMessage {
	if len(msgs) == 0 {
		return []json.RawMessage{}
	}
	lastUser := -1
	for i := len(msgs) - 1; i >= 0; i-- {
		var m struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(msgs[i], &m) != nil {
			continue
		}
		if m.Role == "user" {
			lastUser = i
			break
		}
	}
	if lastUser < 0 {
		// 无 user 消息（纯系统提示等）：取最后一条。
		return []json.RawMessage{msgs[len(msgs)-1]}
	}
	end := lastUser + 1
	for end < len(msgs) {
		var m struct {
			Role string `json:"role"`
		}
		if json.Unmarshal(msgs[end], &m) != nil || m.Role != "tool" {
			break
		}
		end++
	}
	out := make([]json.RawMessage, 0, end-lastUser)
	out = append(out, msgs[lastUser:end]...)
	return out
}

// captureReqIncrement 计算本次请求的增量消息与全量指纹：
// 会话已有上次指纹时按 diffMessages 对齐；否则降级取最后一条 user（及其 tool 链）。
// 返回 (增量消息, 本次全量指纹)；body 解析失败返回 (nil, nil)。
// 会更新 sessObj.LastMsgFingerprints 为本次全量指纹（新会话由调用方在 Attach 时设置）。
func (g *Gateway) captureReqIncrement(sessObj *router.Session, body []byte) ([]json.RawMessage, []string) {
	var req struct {
		Messages []json.RawMessage `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return nil, nil
	}
	fps := make([]string, len(req.Messages))
	for i, m := range req.Messages {
		fps[i] = msgFingerprint(m)
	}
	var incr []json.RawMessage
	if sessObj != nil && len(sessObj.LastMsgFingerprints) > 0 {
		incr = diffMessages(sessObj.LastMsgFingerprints, req.Messages)
	} else {
		incr = fallbackReqIncrement(req.Messages)
	}
	if sessObj != nil {
		sessObj.LastMsgFingerprints = fps
	}
	if incr == nil {
		incr = []json.RawMessage{}
	}
	return incr, fps
}

// writeCallLog 采集一条调用日志（写失败仅记日志，不阻塞/回滚请求语义）。
// 拒绝等 Reject 场景 routeRes 传 &router.RouteResult{}（零值 → 相关引用字段不写入）；
// requestID 为空时从 ctx 取（resp.RequestID 同源）。
func (g *Gateway) writeCallLog(ctx context.Context, billingID, requestID string, token *identity.Token,
	routeRes *router.RouteResult, model, pricingMode, sessionID string, reqMsgs []json.RawMessage,
	respKind, respBody, status, errMsg string, durationMs, firstTokenMs *int64, decision any) {
	if g.callLogs == nil {
		return
	}
	if requestID == "" {
		requestID = logger.GetRequestID(ctx)
	}
	cl := &CallLog{
		BillingID:    billingID,
		RequestID:    requestID,
		SessionID:    sessionID,
		Model:        model,
		PricingMode:  pricingMode,
		Status:       status,
		ReqMessages:  reqMsgs,
		RespBody:     respBody,
		RespKind:     respKind,
		Decision:     decision,
		ErrorMessage: errMsg,
		DurationMs:   durationMs,
		FirstTokenMs: firstTokenMs,
	}
	if token != nil {
		uid := token.UserID
		cl.UserID = &uid
	}
	if routeRes != nil {
		if routeRes.ChannelID > 0 {
			v := routeRes.ChannelID
			cl.ChannelID = &v
		}
		if routeRes.ChannelKeyID > 0 {
			v := routeRes.ChannelKeyID
			cl.ChannelKeyID = &v
		}
		if routeRes.ExternalModelID > 0 {
			v := routeRes.ExternalModelID
			cl.ExternalModelID = &v
		}
		cl.InternalModelID = routeRes.InternalModelID
	}
	// 日志采集脱离请求取消（流式结束/客户端断开后也要落日志），带超时兜底。
	bctx, cancel := billingCtx(ctx)
	defer cancel()
	if err := g.callLogs.Insert(bctx, cl); err != nil {
		g.logger.Error("insert call log failed", "billing_id", billingID, "session_id", sessionID, "err", err)
	}
}
