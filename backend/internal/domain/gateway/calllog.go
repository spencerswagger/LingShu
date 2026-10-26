package gateway

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
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
	Model           string // 用户请求的对外模型名（Auto 命中后为实际命中的成员对外模型名）
	AutoModelName   string // 请求的 Auto 名（命中 Auto 时非空；非 Auto 请求为空）
	PricingMode     string // sale / cost
	Status          string // completed / failed
	ReqMessages     any    // 请求增量 JSONB 原始值（可为 nil/空数组）
	ReqParams       []byte // 入站请求参数快照 JSONB 原始值（不含 messages，≤8KB，见 reqparams.go）
	RespBody        string // 非流式完整响应体；流式为结构化提取的 assistant 增量 JSON
	RespKind        string // non_stream / stream / error
	Decision        any    // 决策轨迹 JSONB 原始值（候选渠道与原因、系数、预扣与结果）
	UpstreamUsage   []byte // 上游原始 usage（JSONB 值：{"Protocol":...,"Usage":{...}}，见 rawusage.go；nil=无）
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

// callLogsCols 与 0002_call_logs.sql + 0011_auto_models.sql（auto_model_name）+ 0012（upstream_usage）
// 迁移后的列顺序一致的查询列（不含 id/created_at 自动列的读侧拆分）。
const callLogsCols = `billing_id, request_id, session_id, user_id, channel_id, channel_key_id,
	internal_model_id, external_model_id, model, auto_model_name, pricing_mode, status, req_messages, resp_body, resp_kind,
	decision, error_message, duration_ms, first_token_ms, created_at, upstream_usage, req_params`

// insertCallLogSQL 写入一条调用日志。列与占位符必须一一对应（数量不等会让所有写日志失败），
// 由 calllog_test.go 的 TestInsertCallLogSQL_ColumnPlaceholderCount 守卫。
const insertCallLogSQL = `
INSERT INTO call_logs(id, billing_id, request_id, session_id, user_id, channel_id, channel_key_id,
	internal_model_id, external_model_id, model, auto_model_name, pricing_mode, status, req_messages, resp_body, resp_kind,
	decision, error_message, duration_ms, first_token_ms, created_at, upstream_usage, req_params)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
ON CONFLICT (billing_id) DO NOTHING`

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
	_, err = s.db.ExecContext(ctx, insertCallLogSQL,
		id, c.BillingID, c.RequestID, c.SessionID, nullableInt64(c.UserID), nullableInt64(c.ChannelID),
		nullableInt64(c.ChannelKeyID), c.InternalModelID, nullableInt64(c.ExternalModelID), c.Model,
		nullableString(c.AutoModelName), c.PricingMode, c.Status, reqRaw, c.RespBody, c.RespKind, decRaw, c.ErrorMessage,
		nullableInt64(c.DurationMs), nullableInt64(c.FirstTokenMs), created, nullableBytes(c.UpstreamUsage),
		nullableBytes(c.ReqParams))
	return err
}

// nullableBytes JSONB 参数：空值给 NULL（避免写入空字符串被 jsonb 拒绝）。
func nullableBytes(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}

// nullableString 空串 → SQL NULL（auto_model_name 等可空文本列用）。
func nullableString(s string) any {
	if s == "" {
		return nil
	}
	return s
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

// fullReqSnapshot 无上次指纹时的落库内容：**全量消息**（不截断、不抽样）。
// 历史实现只取「最后一条 user 消息及其 tool 链」，导致首轮请求/新会话在调用日志里
// 看不到 system 提示与前文的上下文——账单详情「当初到底发了什么」因此残缺。
// 语义仍是「增量追加」：同一会话后续请求走 diffMessages 只补新增部分。
func fullReqSnapshot(msgs []json.RawMessage) []json.RawMessage {
	if len(msgs) == 0 {
		return []json.RawMessage{}
	}
	out := make([]json.RawMessage, len(msgs))
	copy(out, msgs)
	return out
}

// parseRequestMessages 从入站请求体取出「消息列表」：
//   - OpenAI/Anthropic 形状：顶层 messages 数组；
//   - OpenAI Responses 形状：顶层 input（条目数组或纯字符串）；
//   - 其它形状：无消息（返回 false）。
//
// Responses 入站过去一律取不到消息（只认 messages），调用日志因此缺失请求内容，这里补齐。
func parseRequestMessages(body []byte) ([]json.RawMessage, bool) {
	var req struct {
		Messages []json.RawMessage `json:"messages"`
		Input    json.RawMessage   `json:"input"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, false
	}
	if len(req.Messages) > 0 {
		return req.Messages, true
	}
	if len(req.Input) == 0 {
		return nil, false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(req.Input, &items); err == nil && len(items) > 0 {
		return items, true
	}
	var text string
	if err := json.Unmarshal(req.Input, &text); err == nil && text != "" {
		msg, merr := json.Marshal(map[string]string{"role": "user", "content": text})
		if merr != nil {
			return nil, false
		}
		return []json.RawMessage{msg}, true
	}
	return nil, false
}

// captureReqIncrement 计算本次请求的增量消息与全量指纹：
// 会话已有上次指纹时按 diffMessages 对齐；否则降级取最后一条 user（及其 tool 链）。
// 返回 (增量消息, 本次全量指纹)；body 解析失败返回 (nil, nil)。
// 会更新 sessObj.LastMsgFingerprints 为本次全量指纹（新会话由调用方在 Attach 时设置）。
func (g *Gateway) captureReqIncrement(sessObj *router.Session, body []byte) ([]json.RawMessage, []string) {
	msgs, ok := parseRequestMessages(body)
	if !ok || len(msgs) == 0 {
		return nil, nil
	}
	fps := make([]string, len(msgs))
	for i, m := range msgs {
		fps[i] = msgFingerprint(m)
	}
	var incr []json.RawMessage
	if sessObj != nil && len(sessObj.LastMsgFingerprints) > 0 {
		incr = diffMessages(sessObj.LastMsgFingerprints, msgs)
	} else {
		incr = fullReqSnapshot(msgs)
	}
	if sessObj != nil {
		sessObj.LastMsgFingerprints = fps
	}
	if incr == nil {
		incr = []json.RawMessage{}
	}
	return incr, fps
}

// preCharge 预扣决策快照：预扣阶段（估算输入 token × 时段/分档系数 ÷ R）的关键参数，
// 供决策轨迹展示「预扣积分从何而来」。Credits<=0 表示未发生预扣（无单价/零余额等）。
type preCharge struct {
	Credits     float64
	InputTokens int
	TimeCoeff   float64
	CtxCoeff    float64
}

// callLogDecision 组装本次请求的决策轨迹（供成功/失败路径 writeCallLog 使用）：
// attempts 为「到当前候选为止」已收集的全部候选结果 + 当前候选的最终结果（result 作为该候选理由），
// 前端请求详情页据此展示候选渠道表与终态原因（success/上游失败等）。
// cur 为当前候选的路由结果：除渠道/密钥/模型标识外，还携带成本标量/时段系数（OQ-3 排查用）。
// 渠道/密钥名由运行时按 ID 回查（渠道 ID 为雪花值，直接展示不具可读性）。
func (g *Gateway) callLogDecision(attempts []map[string]any, result string, pc preCharge, cur *router.RouteResult) map[string]any {
	cloned := make([]map[string]any, 0, len(attempts)+1)
	for i := range attempts {
		m := make(map[string]any, len(attempts[i]))
		for k, v := range attempts[i] {
			m[k] = v
		}
		cloned = append(cloned, m)
	}
	chName, keyName := g.channelAndKeyNames(cur.ChannelID, cur.ChannelKeyID)
	cloned = append(cloned, map[string]any{
		"order":             len(cloned) + 1,
		"channel_id":        strconv.FormatInt(cur.ChannelID, 10),
		"channel_name":      chName,
		"channel_key_id":    strconv.FormatInt(cur.ChannelKeyID, 10),
		"channel_key_name":  keyName,
		"internal_model_id": cur.InternalModelID,
		"reason":            result,
		// 成本标量 / 是否有效 / 时段系数：仅排查用，不参与计费（OQ-3）。
		"cost":             cur.Cost,
		"cost_known":       cur.CostKnown,
		"route_time_coeff": cur.TimeCoeff,
	})
	return map[string]any{
		"attempts":         cloned,
		"est_input_tokens": pc.InputTokens,
		"time_coeff":       pc.TimeCoeff,
		"ctx_coeff":        pc.CtxCoeff,
		"pre_consumed":     pc.Credits,
		"result":           result,
	}
}

// channelAndKeyNames 按运行时回查渠道名/密钥名（未命中或运行时缺省时返回空串）。
func (g *Gateway) channelAndKeyNames(channelID, keyID int64) (string, string) {
	if g.channels == nil {
		return "", ""
	}
	var chName, keyName string
	if rt, ok := g.channels.GetRuntime(channelID); ok {
		chName = rt.Channel.Name
	}
	if kr, ok := g.channels.GetKeyRuntime(keyID); ok {
		keyName = kr.Key.Name
	}
	return chName, keyName
}

// writeCallLog 采集一条调用日志（写失败仅记日志，不阻塞/回滚请求语义）。
// 拒绝等 Reject 场景 routeRes 传 &router.RouteResult{}（零值 → 相关引用字段不写入）；
// requestID 为空时从 ctx 取（resp.RequestID 同源）。
//
// 本方法是 writeCallLogUsage 的简写（不带上游原始 usage）：绝大多数调用点（失败/拒绝/构建失败等）
// 没有可采集的上游 usage，只有真正拿到上游响应的成功路径才用 writeCallLogUsage，避免为 20+ 个调用点
// 逐个补参数。
func (g *Gateway) writeCallLog(ctx context.Context, billingID, requestID string, token *identity.Token,
	routeRes *router.RouteResult, model, pricingMode, sessionID string, reqMsgs []json.RawMessage,
	respKind, respBody, status, errMsg string, durationMs, firstTokenMs *int64, decision any) {
	g.writeCallLogUsage(ctx, billingID, requestID, token, routeRes, model, pricingMode, sessionID, reqMsgs,
		respKind, respBody, status, errMsg, nil, durationMs, firstTokenMs, decision)
}

// writeCallLogUsage 同 writeCallLog，额外记录上游原始 usage（call_logs.upstream_usage，诊断渠道口径用）。
func (g *Gateway) writeCallLogUsage(ctx context.Context, billingID, requestID string, token *identity.Token,
	routeRes *router.RouteResult, model, pricingMode, sessionID string, reqMsgs []json.RawMessage,
	respKind, respBody, status, errMsg string, upstreamUsage []byte, durationMs, firstTokenMs *int64, decision any) {
	if g.callLogs == nil {
		return
	}
	if requestID == "" {
		requestID = logger.GetRequestID(ctx)
	}
	cl := &CallLog{
		BillingID: billingID,
		RequestID: requestID,
		// 请求参数快照：在组包处一次性构造并挂到请求 context 上（见 reqparams.go），
		// 这里作为唯一写入漏斗取出落库——避免各个失败/成功分支各写一份、字段口径漂移。
		ReqParams:     reqParamsFromCtx(ctx),
		SessionID:     sessionID,
		Model:         model,
		PricingMode:   pricingMode,
		Status:        status,
		ReqMessages:   reqMsgs,
		RespBody:      respBody,
		RespKind:      respKind,
		Decision:      decision,
		UpstreamUsage: upstreamUsage,
		ErrorMessage:  errMsg,
		DurationMs:    durationMs,
		FirstTokenMs:  firstTokenMs,
	}
	if routeRes != nil {
		cl.AutoModelName = routeRes.AutoModelName
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
