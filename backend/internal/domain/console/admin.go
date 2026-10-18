// Package console 提供控制台聚合查询 API：账单查询、统计总览、计费配置读写、
// 渠道状态快照，以及账单三步拆解数据结构（admin/dev 详情共用）。
// 金额统一以 decimalx.Round5/Round2 展示。
package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/gateway"
	"github.com/team/llmgateway/internal/domain/model"
	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/pkg/decimalx"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// Admin 承载管理端控制台聚合查询处理器。
type Admin struct {
	db             *sql.DB
	billing        *billing.SqlStore
	channelMgr     *channel.Manager
	userIDFrom     func(ctx context.Context) (int64, bool)
	now            func() time.Time
	sessions       *router.SessionRegistry                                          // 会话管理 API（F1 经 SetSessions 注入）
	keyNameByIDs   func(ctx context.Context, ids []int64) (map[int64]string, error) // 渠道密钥名批量反查钩子（F1 装配真实实现）
	sessionSetName func(ctx context.Context, sessionID, name string) (bool, error)  // 会话改名钩子（F1 装配 SessionStore.SetName）
	callLogs       callLogReader                                                    // 调用日志只读查询（经 SetCallLogStore 注入；nil=未装配）
	routeEng       routeEngine                                                      // 路由引擎（服务状态候选顺序来源；经 SetRouterEngine 注入；nil=不计算候选）
	models         externalModelSource                                              // 对外模型只读枚举（经 SetModelSource 注入；nil=列表为空）
	sessionCostEvt *router.SessionCostEventStore                                    // 会话成本决策产物进程内存储（经 SetSessionCostEventStore 注入；nil=事件流为空）
}

// routeEngine 聚合端点所需的路由引擎能力（router.Engine 满足）。
type routeEngine interface {
	RouteAll(ctx context.Context, spec router.RouteSpec) ([]*router.ChannelRoute, error)
	CostSample() billing.Usage
	ActiveKeySessions(keyID int64) int
}

// externalModelSource 对外模型只读枚举（model.Service 满足）。
type externalModelSource interface {
	ListModels(ctx context.Context, enabled *bool) ([]model.ExternalModel, error)
}

// callLogReader 调用日志只读查询抽象（gateway.SQLCallLogStore 满足；nil=未装配时查询为空）。
type callLogReader interface {
	GetByBillingID(ctx context.Context, billingID string) (*gateway.CallLog, error)
	ListBySession(ctx context.Context, sessionID string) ([]gateway.CallLog, error)
}

// NewAdmin 创建控制台聚合处理器。
func NewAdmin(db *sql.DB, billingStore *billing.SqlStore, chMgr *channel.Manager, userIDFrom func(ctx context.Context) (int64, bool)) *Admin {
	return &Admin{
		db:         db,
		billing:    billingStore,
		channelMgr: chMgr,
		userIDFrom: userIDFrom,
		now:        time.Now,
	}
}

// SetSessions 注入会话注册表（会话管理 API 的数据源；nil 时列表/踢下线为空操作）。
func (h *Admin) SetSessions(reg *router.SessionRegistry) { h.sessions = reg }

// SetKeyNameResolver 注入渠道密钥名称批量解析钩子（F1 在装配层提供真实 DB 实现；
// nil/查询失败时 channel_key_name 留空，前端以 channel_key_id 兜底）。
func (h *Admin) SetKeyNameResolver(fn func(ctx context.Context, ids []int64) (map[int64]string, error)) {
	h.keyNameByIDs = fn
}

// SetSessionNameSetter 注入会话改名钩子（F1 装配 SessionStore.SetName；nil 时改名接口返回 404 语义）。
func (h *Admin) SetSessionNameSetter(fn func(ctx context.Context, sessionID, name string) (bool, error)) {
	h.sessionSetName = fn
}

// SetCallLogStore 注入调用日志只读查询（nil=未装配：BillingDetail.CallLog 为 nil、会话调用列表为空）。
func (h *Admin) SetCallLogStore(s callLogReader) { h.callLogs = s }

// SetRouterEngine 注入路由引擎（服务状态候选顺序来源）。nil 时不计算候选、不报错。
func (h *Admin) SetRouterEngine(e routeEngine) { h.routeEng = e }

// SetModelSource 注入对外模型只读枚举。nil 时对外模型列表为空、不报错。
func (h *Admin) SetModelSource(s externalModelSource) { h.models = s }

// SetSessionCostEventStore 注入会话成本决策产物进程内存储（「会话成本路由」面板 /events、/distribution 数据源）。
// nil 时事件流/分布为空、不报错。
func (h *Admin) SetSessionCostEventStore(s *router.SessionCostEventStore) { h.sessionCostEvt = s }

// attachCallLog 查 call_logs 并填充账单详情（未装配/无记录时为 nil）。
func (h *Admin) attachCallLog(ctx context.Context, bid string) *CallLogView {
	if h.callLogs == nil || bid == "" {
		return nil
	}
	cl, err := h.callLogs.GetByBillingID(ctx, bid)
	if err != nil || cl == nil {
		return nil
	}
	return toCallLogView(cl)
}

// ===== 账单查询 =====

// billingListItem 账单列表项（user_id 仅内部传参用，前端展示一律用 Username）。
// Tokens 为三段用量（输入/缓存读/输出）；Rates/CoeffTime/CoeffContext/RValue 供前端悬停展示计费公式。
// Tokens/Rates 对外为 PascalCase 视图（billing.WireUsage/WireRates），存储仍为 snake（JSONB 契约）。
type billingListItem struct {
	BillingID       string            `json:"BillingID"`
	UserID          int64             `json:"UserID,string"`
	Username        string            `json:"Username,omitempty"`
	UserNickname    string            `json:"UserNickname,omitempty"` // 昵称（join users；为空表示未设置）
	TokenName       string            `json:"TokenName,omitempty"`
	ExternalModel   string            `json:"ExternalModel"`
	AutoModelName   string            `json:"AutoModelName,omitempty"` // 请求的 Auto 名（非 Auto 请求为空）
	PricingMode     string            `json:"PricingMode"`
	CreditsConsumed float64           `json:"CreditsConsumed"`
	Status          string            `json:"Status"`
	CallTime        time.Time         `json:"CallTime"`
	InternalModelID string            `json:"InternalModelID"`
	ChannelKeyID    int64             `json:"ChannelKeyID,string"`
	KeyName         string            `json:"KeyName,omitempty"` // 密钥名（join channel_keys；缺失时兜底 #<id>）
	SessionID       string            `json:"SessionID,omitempty"`
	SessionName     string            `json:"SessionName,omitempty"` // 会话可读名（sessions.name；缺失留空）
	ChannelName     string            `json:"ChannelName,omitempty"`
	Tokens          billing.WireUsage `json:"Tokens"`
	Rates           billing.WireRates `json:"Rates,omitempty"`
	CoeffTime       float64           `json:"CoeffTime,omitempty"`
	CoeffContext    float64           `json:"CoeffContext,omitempty"`
	RValue          int64             `json:"RValue,omitempty"`
	ErrorMessage    string            `json:"ErrorMessage,omitempty"`
	DurationMs      *int64            `json:"DurationMs,omitempty"`
	FirstTokenMs    *int64            `json:"FirstTokenMs,omitempty"`
}

func toListItem(rec *billing.Record) billingListItem {
	return billingListItem{
		BillingID:       rec.BillingID,
		UserID:          rec.UserID,
		ExternalModel:   rec.ExternalModel,
		AutoModelName:   rec.AutoModelName,
		PricingMode:     rec.PricingMode,
		CreditsConsumed: decimalx.Round5(rec.CreditsConsumed),
		Status:          rec.Status,
		CallTime:        rec.CallTime,
		InternalModelID: rec.InternalModelID,
		ChannelKeyID:    rec.ChannelKeyID,
		SessionID:       rec.SessionID,
		Tokens:          billing.WireUsage(rec.Tokens),
		Rates:           billing.WireRates(rec.Rates),
		CoeffTime:       rec.Coefficients.Time,
		CoeffContext:    rec.Coefficients.Context,
		RValue:          rec.RValue,
		ErrorMessage:    rec.ErrorMessage,
		DurationMs:      rec.DurationMs,
		FirstTokenMs:    rec.FirstTokenMs,
	}
}

// buildBillingsFilter 解析账单列表/统计共用的查询参数（词法相同，避免两处漂移）。
func buildBillingsFilter(q url.Values) (billing.RecordFilter, error) {
	f := billing.RecordFilter{
		Model:       q.Get("Model"),
		PricingMode: q.Get("PricingMode"),
		Status:      q.Get("Status"),
		SessionID:   q.Get("SessionID"),
	}
	if v := q.Get("UserID"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return f, errors.New("user_id 必须为数字")
		}
		f.UserID = &id
	}
	if v := q.Get("TokenID"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return f, errors.New("token_id 必须为数字")
		}
		f.TokenID = &id
	}
	if v := q.Get("ChannelKeyID"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return f, errors.New("channel_key_id 必须为数字")
		}
		f.ChannelKeyID = &id
	}
	if v := q.Get("From"); v != "" {
		t, err := parseRFC3339Lenient(v)
		if err != nil {
			return f, errors.New("from 时间格式应为 RFC3339")
		}
		f.From = &t
	}
	if v := q.Get("To"); v != "" {
		t, err := parseRFC3339Lenient(v)
		if err != nil {
			return f, errors.New("to 时间格式应为 RFC3339")
		}
		f.To = &t
	}
	return f, nil
}

// HandleListBillings GET /api/v1/admin/billings
func (h *Admin) HandleListBillings(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f, err := buildBillingsFilter(q)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	f.Page = parsePage(q.Get("Page"))
	f.Size = parseSize(q.Get("Size"))
	records, total, err := h.billing.ListRecords(r.Context(), f)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询账单失败")
		return
	}
	list := make([]billingListItem, 0, len(records))
	for i := range records {
		list = append(list, toListItem(&records[i]))
	}
	if len(list) > 0 {
		h.decorateList(r.Context(), list, records)
	}
	resp.OK(w, r, map[string]any{"List": list, "Total": total, "Page": f.Page, "Size": f.Size})
}

// HandleBillingsStats GET /api/v1/admin/billings/stats 账单筛选聚合统计（总积分/token/平均 RPM·TPM 基础数据）。
func (h *Admin) HandleBillingsStats(w http.ResponseWriter, r *http.Request) {
	f, err := buildBillingsFilter(r.URL.Query())
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	summary, err := h.billing.Summarize(r.Context(), f)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "统计账单失败")
		return
	}
	resp.OK(w, r, summary)
}

// decorateList 批量补全账单列表的用户名/昵称/渠道名/密钥名/令牌名（避免 N+1）。
// key_name 兜底：channel_keys 缺失（如密钥被物理删除）时展示 `#<id>`。
func (h *Admin) decorateList(ctx context.Context, list []billingListItem, recs []billing.Record) {
	userNames := h.usernames(ctx, recs)
	keyInfos := h.keyInfos(ctx, recs)
	channelNames := h.channelNames(ctx, collectChannelIDs(keyInfos))
	tokenNames := h.tokenNames(ctx, recs)
	sessionNames := h.sessionNames(ctx, recs)
	for i := range list {
		if un, ok := userNames[list[i].UserID]; ok {
			list[i].Username = un.Username
			list[i].UserNickname = un.Nickname
		}
		if name := sessionNames[list[i].SessionID]; name != "" {
			list[i].SessionName = name
		}
		if info, ok := keyInfos[list[i].ChannelKeyID]; ok {
			if info.Name != "" {
				list[i].KeyName = info.Name
			}
			if name := channelNames[info.ChannelID]; name != "" {
				list[i].ChannelName = name
			}
		}
		if list[i].KeyName == "" {
			list[i].KeyName = fmt.Sprintf("#%d", list[i].ChannelKeyID)
		}
		if list[i].BillingID != "" {
			list[i].TokenName = tokenNames[list[i].BillingID]
		}
	}
}

// keyInfo 单个密钥的展示信息（密钥名 + 所属渠道）。
type keyInfo struct {
	ChannelID int64
	Name      string
}

// keyInfos 批量查询 channel_key_id -> {所属渠道, 密钥名}；失败返回空 map。
func (h *Admin) keyInfos(ctx context.Context, recs []billing.Record) map[int64]keyInfo {
	ids := uniqueInt64(recs, func(r billing.Record) int64 { return r.ChannelKeyID })
	if len(ids) == 0 {
		return nil
	}
	rows, err := h.db.QueryContext(ctx,
		`SELECT id, channel_id, name FROM channel_keys WHERE id = ANY('{`+int64List(ids)+`}'::bigint[])`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make(map[int64]keyInfo, len(ids))
	for rows.Next() {
		var id, channelID int64
		var name string
		if err := rows.Scan(&id, &channelID, &name); err != nil {
			continue
		}
		out[id] = keyInfo{ChannelID: channelID, Name: name}
	}
	return out
}

// collectChannelIDs 提取密钥信息中的渠道 ID 去重列表。
func collectChannelIDs(infos map[int64]keyInfo) []int64 {
	seen := make(map[int64]struct{}, len(infos))
	ids := make([]int64, 0, len(infos))
	for _, info := range infos {
		if info.ChannelID == 0 {
			continue
		}
		if _, ok := seen[info.ChannelID]; ok {
			continue
		}
		seen[info.ChannelID] = struct{}{}
		ids = append(ids, info.ChannelID)
	}
	return ids
}

// channelNames 批量查询渠道 ID -> 名称；失败返回空 map。
func (h *Admin) channelNames(ctx context.Context, ids []int64) map[int64]string {
	if len(ids) == 0 {
		return nil
	}
	rows, err := h.db.QueryContext(ctx,
		`SELECT id, name FROM channels WHERE id = ANY('{`+int64List(ids)+`}'::bigint[])`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make(map[int64]string, len(ids))
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			continue
		}
		out[id] = name
	}
	return out
}

// channelNameByKeyID 由密钥 ID 反查所属渠道名（账单详情展示）；密钥/渠道不存在返回空串。
func (h *Admin) channelNameByKeyID(ctx context.Context, keyID int64) string {
	var name string
	if err := h.db.QueryRowContext(ctx,
		`SELECT c.name FROM channels c JOIN channel_keys k ON k.channel_id = c.id WHERE k.id = $1`, keyID).
		Scan(&name); err != nil {
		return ""
	}
	return name
}

// tokenNames 批量查询令牌 display_name：BillingID -> 令牌名（billing_records 通过 token_id 关联 tokens）。
// 令牌已删除（软删）时返回空串，前端展示 「-」。
func (h *Admin) tokenNames(ctx context.Context, recs []billing.Record) map[string]string {
	ids := make([]int64, 0, len(recs))
	seen := make(map[int64]struct{}, len(recs))
	for i := range recs {
		t := recs[i].TokenID
		if t == nil {
			continue
		}
		if _, ok := seen[*t]; ok {
			continue
		}
		seen[*t] = struct{}{}
		ids = append(ids, *t)
	}
	if len(ids) == 0 {
		return nil
	}
	rows, err := h.db.QueryContext(ctx,
		`SELECT id, display_name FROM tokens WHERE id = ANY('{`+int64List(ids)+`}'::bigint[])`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	byID := make(map[int64]string, len(ids))
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			continue
		}
		byID[id] = name
	}
	out := make(map[string]string, len(recs))
	for i := range recs {
		t := recs[i].TokenID
		if t != nil {
			if name := byID[*t]; name != "" {
				out[recs[i].BillingID] = name
			}
		}
	}
	return out
}

// uniqueInt64 提取记录中某列的去重 ID 列表。
func uniqueInt64(recs []billing.Record, get func(billing.Record) int64) []int64 {
	seen := make(map[int64]struct{}, len(recs))
	ids := make([]int64, 0, len(recs))
	for i := range recs {
		id := get(recs[i])
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// int64List 格式化为 pg array 字面量内容。
func int64List(ids []int64) string {
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	return strings.Join(parts, ",")
}

// userName 是批量反查用户展示信息的结构：登录名与昵称（昵称为空表示未设置）。
type userName struct {
	Username string
	Nickname string
}

// usernames 批量查询一批用户的用户名与昵称（userID -> userName）。查询失败返回空 map，不影响账单主查询。
func (h *Admin) usernames(ctx context.Context, recs []billing.Record) map[int64]userName {
	ids := make([]int64, 0, len(recs))
	seen := make(map[int64]struct{}, len(recs))
	for i := range recs {
		id := recs[i].UserID
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return nil
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	rows, err := h.db.QueryContext(ctx, `SELECT id, username, nickname FROM users WHERE id = ANY('{`+strings.Join(parts, ",")+`}'::bigint[])`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make(map[int64]userName, len(ids))
	for rows.Next() {
		var id int64
		var un userName
		if err := rows.Scan(&id, &un.Username, &un.Nickname); err != nil {
			continue
		}
		out[id] = un
	}
	return out
}

// HandleGetBilling GET /api/v1/admin/billings/{billing_id} admin 详情（含内部模型与渠道名）。
func (h *Admin) HandleGetBilling(w http.ResponseWriter, r *http.Request) {
	bid := r.PathValue("billing_id")
	rec, err := h.billing.GetByBillingID(r.Context(), bid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			resp.Err(w, r, http.StatusNotFound, resp.CodeNotFound, "账单不存在")
			return
		}
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询账单失败")
		return
	}
	steps, routeDiff := buildBreakdown(rec)
	detail := BillingDetail{
		BillingID:       rec.BillingID,
		CallTime:        rec.CallTime.Format(time.RFC3339),
		Model:           rec.ExternalModel,
		AutoModelName:   rec.AutoModelName,
		PricingMode:     rec.PricingMode,
		CreditsConsumed: decimalx.Round5(rec.CreditsConsumed),
		Status:          rec.Status,
		Steps:           steps,
		RouteDiff:       routeDiff,
		InternalModelID: rec.InternalModelID,
		ChannelName:     h.channelNameByKeyID(r.Context(), rec.ChannelKeyID),
		CallLog:         h.attachCallLog(r.Context(), rec.BillingID),
	}
	resp.OK(w, r, detail)
}

// HandleListSessionCalls GET /api/v1/admin/sessions/{session_id}/calls
// 返回会话的完整调用记录（call_logs 按 created_at 升序）；未装配调用日志存储时返回空列表。
func (h *Admin) HandleListSessionCalls(w http.ResponseWriter, r *http.Request) {
	sid := r.PathValue("session_id")
	if sid == "" {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "缺少会话 ID")
		return
	}
	if h.callLogs == nil {
		resp.OK(w, r, map[string]any{"List": []CallLogView{}, "Total": 0})
		return
	}
	logs, err := h.callLogs.ListBySession(r.Context(), sid)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询调用日志失败")
		return
	}
	list := make([]CallLogView, 0, len(logs))
	for i := range logs {
		if v := toCallLogView(&logs[i]); v != nil {
			list = append(list, *v)
		}
	}
	resp.OK(w, r, map[string]any{"List": list, "Total": len(list)})
}

// HandleChannelSnapshot GET /api/v1/admin/channels/snapshot
func (h *Admin) HandleChannelSnapshot(w http.ResponseWriter, r *http.Request) {
	resp.OK(w, r, h.channelMgr.Snapshot())
}

// ===== 系统配置读写（计费 / 会话过期 / 网关白名单 统一入口）=====

// sessionExpirationDefault 内置默认会话过期配置（与 seeding 的 session.expiration 种子一致），
// 配置键缺失时回退，保证前端可展示与保存。
const sessionExpirationDefault = `{"default_ttl_minutes":5,"context_tiers":[{"max_tokens":2000,"ttl_minutes":5},{"max_tokens":8000,"ttl_minutes":30},{"max_tokens":32000,"ttl_minutes":180},{"max_tokens":128000,"ttl_minutes":1440}],"turn_tiers":[{"max_turns":2,"ttl_minutes":5},{"max_turns":10,"ttl_minutes":30},{"max_turns":50,"ttl_minutes":180},{"max_turns":200,"ttl_minutes":1440}]}`

// readBillingConfigRaw 读取计费配置各键原文，返回对外 PascalCase 键映射（CreditValue 由 R 派生）。
func (h *Admin) readBillingConfigRaw(ctx context.Context) (map[string]json.RawMessage, error) {
	rRaw, err := h.billing.GetConfig(ctx, "billing.r")
	if err != nil {
		return nil, err
	}
	cnyRaw, err := h.billing.GetConfig(ctx, "billing.cny_rate")
	if err != nil {
		// 兼容旧库未初始化汇率的情况，回退默认 6.8。
		cnyRaw = json.RawMessage(`6.8`)
	}
	tiersRaw, err := h.billing.GetConfig(ctx, "billing.context_tiers")
	if err != nil {
		return nil, err
	}
	timeRaw, err := h.billing.GetConfig(ctx, "billing.time_config")
	if err != nil {
		return nil, err
	}
	// 路由成本比较样值（routing.cost_sample，三段口径）：旧库未初始化时回退内置默认，保证前端可展示与保存。
	sampleRaw, err := h.billing.GetConfig(ctx, "routing.cost_sample")
	if err != nil {
		sampleRaw = json.RawMessage(`{"input":10000,"cache_read":9000,"output":1000}`)
	}
	// credit_value 由 r 派生（R / 1e6，每积分对应人民币元），不落库，解析失败时回退 0。
	creditV, _ := creditValueFromRaw(rRaw)
	creditRaw, _ := json.Marshal(creditV)
	// 数据库配置键（billing.r 等）为存储层契约，保持不变；此处显式映射为对外 PascalCase 键。
	return map[string]json.RawMessage{
		"R":            rRaw,
		"CNYRate":      cnyRaw,
		"CreditValue":  creditRaw,
		"ContextTiers": tiersRaw,
		"TimeConfig":   timeRaw,
		"CostSample":   sampleRaw,
	}, nil
}

// readSessionExpirationRaw 读取会话过期配置原文，缺失回退内置默认。
func (h *Admin) readSessionExpirationRaw(ctx context.Context) json.RawMessage {
	raw, err := h.billing.GetConfig(ctx, "session.expiration")
	if err != nil {
		return json.RawMessage(sessionExpirationDefault)
	}
	return json.RawMessage(raw)
}

// readGatewayWhitelistRaw 读取全局上游响应头白名单原文，缺失回退空数组。
func (h *Admin) readGatewayWhitelistRaw(ctx context.Context) json.RawMessage {
	list := []string{}
	if raw, err := h.billing.GetConfig(ctx, gateway.UpstreamResponseHeaderWhitelistKey); err == nil {
		_ = json.Unmarshal(raw, &list)
	}
	if list == nil {
		list = []string{}
	}
	b, _ := json.Marshal(list)
	return json.RawMessage(b)
}

// HandleGetSystemConfig GET /api/v1/admin/configs/system
// 一次返回系统全部配置：计费 Billing、会话过期 SessionExpiration、上游响应头白名单。
func (h *Admin) HandleGetSystemConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	billingMap, err := h.readBillingConfigRaw(ctx)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "读取计费配置失败")
		return
	}
	billingRaw, _ := json.Marshal(billingMap)
	resp.OK(w, r, map[string]json.RawMessage{
		"Billing":                         billingRaw,
		"SessionExpiration":               h.readSessionExpirationRaw(ctx),
		"UpstreamResponseHeaderWhitelist": h.readGatewayWhitelistRaw(ctx),
	})
}

// HandlePutSystemConfig PUT /api/v1/admin/configs/system
// 一次落库系统全部配置（计费 + 会话过期 + 上游响应头白名单）；入参三组全量，任一校验失败返回 400。
func (h *Admin) HandlePutSystemConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Billing struct {
			R            json.RawMessage `json:"R"`
			CnyRate      json.RawMessage `json:"CnyRate"`
			ContextTiers json.RawMessage `json:"ContextTiers"`
			TimeConfig   json.RawMessage `json:"TimeConfig"`
			CostSample   json.RawMessage `json:"CostSample"`
		} `json:"Billing"`
		SessionExpiration               json.RawMessage `json:"SessionExpiration"`
		UpstreamResponseHeaderWhitelist []string        `json:"UpstreamResponseHeaderWhitelist"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	if !validR(req.Billing.R) {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "r 必须是整数或数字字符串")
		return
	}
	if len(req.Billing.CnyRate) > 0 && !validPositiveNumber(req.Billing.CnyRate) {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "cny_rate 必须是正数或数字字符串")
		return
	}
	if !isJSONArray(req.Billing.ContextTiers) {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "context_tiers 必须是 JSON 数组")
		return
	}
	if !isJSONObject(req.Billing.TimeConfig) {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "time_config 必须是 JSON 对象")
		return
	}
	if len(req.Billing.CostSample) > 0 && !validCostSample(req.Billing.CostSample) {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "cost_sample 必须是各段非负的 JSON 对象")
		return
	}
	if err := validateSessionExpiration(req.SessionExpiration); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	for _, name := range req.UpstreamResponseHeaderWhitelist {
		if !validHeaderName(name) {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "非法的响应头名称："+name)
			return
		}
	}
	whitelist := req.UpstreamResponseHeaderWhitelist
	if whitelist == nil {
		whitelist = []string{}
	}
	whitelistRaw, _ := json.Marshal(whitelist)

	operator, _ := h.userIDFrom(r.Context())
	ctx := r.Context()
	// 依次落库三组配置；任一失败即返回明确失败项（同一请求内顺序写入，非单事务）。
	if err := h.billing.UpsertConfig(ctx, "billing.r", req.Billing.R, operator); err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "更新计费配置失败（billing.r）")
		return
	}
	if len(req.Billing.CnyRate) > 0 {
		if err := h.billing.UpsertConfig(ctx, "billing.cny_rate", req.Billing.CnyRate, operator); err != nil {
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "更新计费配置失败（billing.cny_rate）")
			return
		}
	}
	if err := h.billing.UpsertConfig(ctx, "billing.context_tiers", req.Billing.ContextTiers, operator); err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "更新计费配置失败（billing.context_tiers）")
		return
	}
	if err := h.billing.UpsertConfig(ctx, "billing.time_config", req.Billing.TimeConfig, operator); err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "更新计费配置失败（billing.time_config）")
		return
	}
	if len(req.Billing.CostSample) > 0 {
		if err := h.billing.UpsertConfig(ctx, "routing.cost_sample", req.Billing.CostSample, operator); err != nil {
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "更新计费配置失败（routing.cost_sample）")
			return
		}
	}
	if err := h.billing.UpsertConfig(ctx, "session.expiration", req.SessionExpiration, operator); err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "更新会话过期配置失败")
		return
	}
	if err := h.billing.UpsertConfig(ctx, gateway.UpstreamResponseHeaderWhitelistKey, whitelistRaw, operator); err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "更新网关设置失败")
		return
	}
	resp.OK(w, r, map[string]any{"Affected": 1})
}

// validHeaderName 校验是否为合法 HTTP 头字段名（RFC 7230 token，非空）。
func validHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
		case c == '!' || c == '#' || c == '$' || c == '%' || c == '&' || c == '\'' ||
			c == '*' || c == '+' || c == '-' || c == '.' || c == '^' || c == '_' ||
			c == '`' || c == '|' || c == '~':
		default:
			return false
		}
	}
	return true
}

// validateSessionExpiration 校验会话过期配置，返回 nil 表示合法；否则返回具体的中文原因（不含技术细节）。
func validateSessionExpiration(raw []byte) error {
	if !isJSONObject(raw) {
		return errors.New("会话过期配置必须是 JSON 对象")
	}
	var cfg struct {
		DefaultTTLMinutes int `json:"default_ttl_minutes"`
		ContextTiers      []struct {
			MaxTokens  int `json:"max_tokens"`
			TTLMinutes int `json:"ttl_minutes"`
		} `json:"context_tiers"`
		TurnTiers []struct {
			MaxTurns   int `json:"max_turns"`
			TTLMinutes int `json:"ttl_minutes"`
		} `json:"turn_tiers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		return errors.New("会话过期配置格式不合法")
	}
	if cfg.DefaultTTLMinutes <= 0 {
		return errors.New("default_ttl_minutes 必须为正整数")
	}
	prevTokens := 0
	for _, tier := range cfg.ContextTiers {
		if tier.MaxTokens <= 0 || tier.TTLMinutes <= 0 {
			return errors.New("context_tiers 的 max_tokens/ttl_minutes 必须为正整数")
		}
		if tier.MaxTokens <= prevTokens {
			return errors.New("context_tiers 上界必须严格升序")
		}
		prevTokens = tier.MaxTokens
	}
	prevTurns := 0
	for _, tier := range cfg.TurnTiers {
		if tier.MaxTurns <= 0 || tier.TTLMinutes <= 0 {
			return errors.New("turn_tiers 的 max_turns/ttl_minutes 必须为正整数")
		}
		if tier.MaxTurns <= prevTurns {
			return errors.New("turn_tiers 上界必须严格升序")
		}
		prevTurns = tier.MaxTurns
	}
	return nil
}

// validCostSample 校验路由成本比较样值：JSON 对象且各段为非负数值（缺省段按 0 处理）。
func validCostSample(b []byte) bool {
	if !isJSONObject(b) {
		return false
	}
	var m map[string]float64
	if err := json.Unmarshal(b, &m); err != nil {
		return false
	}
	for _, v := range m {
		if v < 0 {
			return false
		}
	}
	return true
}

func validPositiveNumber(b []byte) bool {
	var f float64
	if json.Unmarshal(b, &f) == nil {
		return f > 0
	}
	var s string
	if json.Unmarshal(b, &s) == nil {
		v, err := strconv.ParseFloat(s, 64)
		return err == nil && v > 0
	}
	return false
}

// validR 判断 r 是否为合法的正整数（计费引擎要求 R > 0）。
func validR(b []byte) bool {
	positive := func(v int64) bool { return v > 0 }
	var n int64
	if json.Unmarshal(b, &n) == nil {
		return positive(n)
	}
	var s string
	if json.Unmarshal(b, &s) == nil {
		v, err := strconv.ParseInt(s, 10, 64)
		return err == nil && positive(v)
	}
	return false
}

// creditValueFromRaw 由 r 原文派生每积分对应人民币元（R / 1e6），兼容整数与数字字符串。
func creditValueFromRaw(raw []byte) (float64, error) {
	var n int64
	if json.Unmarshal(raw, &n) == nil {
		return float64(n) / 1e6, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		v, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 0, err
		}
		return float64(v) / 1e6, nil
	}
	return 0, errors.New("无法解析 r 为整数或数字字符串")
}

func isJSONArray(b []byte) bool  { return len(b) > 0 && b[0] == '[' }
func isJSONObject(b []byte) bool { return len(b) > 0 && b[0] == '{' }

func parsePage(v string) int {
	if n, err := strconv.Atoi(v); err == nil && n >= 1 {
		return n
	}
	return 1
}
func parseSize(v string) int {
	if n, err := strconv.Atoi(v); err == nil && n >= 1 {
		return n
	}
	return 20
}

// ===== 会话管理 =====

// sessionListItem 会话列表项：user/token/key 名称在可能时批量反查填充（查询失败为空串）；
// 原始 ID 一并输出，供前端在名称缺失时兜底展示。
// 实时状态字段（Inflight/Calls/Tokens/Credits/ContextLen/RPM/TPM/UserMsgs/ToolCalls/CacheHitRate）
// 由 billing_records / call_logs 按 session_id 批量聚合得到（查询失败为 0）。
type sessionListItem struct {
	SessionID      string    `json:"SessionID"`
	UserID         int64     `json:"UserID,string"`
	UserName       string    `json:"UserName,omitempty"`
	UserNickname   string    `json:"UserNickname,omitempty"` // 昵称（为空表示未设置）
	TokenID        int64     `json:"TokenID,string,omitempty"`
	TokenName      string    `json:"TokenName,omitempty"`
	Model          string    `json:"Model"`
	ChannelKeyID   int64     `json:"ChannelKeyID,string"`
	ChannelKeyName string    `json:"ChannelKeyName,omitempty"`
	ChannelName    string    `json:"ChannelName,omitempty"` // 所属渠道名（按密钥反查；缺失为空）
	SessionRaw     string    `json:"SessionRaw"`
	SessionName    string    `json:"SessionName"`
	Closed         bool      `json:"Closed"`
	CreatedAt      time.Time `json:"CreatedAt"`
	LastActive     time.Time `json:"LastActive"`
	ExpireAt       time.Time `json:"ExpireAt"`
	Expired        bool      `json:"Expired"`
	// 实时状态
	Inflight     int     `json:"Inflight"`     // 进行中请求数（>0 表示正在请求）
	Calls        int64   `json:"Calls"`        // 总调用次数
	Tokens       int64   `json:"Tokens"`       // 总消耗 Token（三段求和口径：输入+缓存读+输出）
	Credits      float64 `json:"Credits"`      // 总消耗积分
	ContextLen   int64   `json:"ContextLen"`   // 总输入 Token（输入+缓存读，取最近一次请求）
	RPM          float64 `json:"RPM"`          // 近 5 分钟每分钟请求数
	TPM          float64 `json:"TPM"`          // 近 5 分钟每分钟 Token 数
	UserMsgs     int64   `json:"UserMsgs"`     // 用户消息次数
	ToolCalls    int64   `json:"ToolCalls"`    // 工具调用次数
	CacheHitRate float64 `json:"CacheHitRate"` // 缓存命中率（缓存读 ÷ 总输入(输入+缓存读)，0~1）
}

// sessionListCols 会话列表查询列（与 router.sessionCols 对齐；token_id 可空单独处理）。
const sessionListCols = `session_id, user_id, token_id, model, session_raw, channel_key_id, internal_model_id, created_at, last_active, expire_at, COALESCE(name, ''), COALESCE(closed, false)`

// sessionListWhere 构建会话列表过滤 WHERE 子句与参数（prepared 绑定，避免注入）。
// expiredMode=active 仅未过期、expired 仅已过期；closedMode="1" 仅已关闭、"0" 仅未关闭；空串不过滤。
// 无任何条件时 where 为空串（配合 WHERE 拼接使用）。
func sessionListWhere(userID, tokenID, keyID int64, prefix, expiredMode, closedMode string, now time.Time) (string, []any) {
	conds := make([]string, 0, 8)
	args := make([]any, 0, 8)
	bind := func(v any) string {
		args = append(args, v)
		return "$" + strconv.Itoa(len(args))
	}
	if userID != 0 {
		conds = append(conds, "user_id = "+bind(userID))
	}
	if tokenID != 0 {
		conds = append(conds, "token_id = "+bind(tokenID))
	}
	if keyID != 0 {
		conds = append(conds, "channel_key_id = "+bind(keyID))
	}
	if prefix != "" {
		conds = append(conds, "starts_with(session_id, "+bind(prefix)+")")
	}
	switch expiredMode {
	case "active":
		conds = append(conds, "expire_at > "+bind(now))
	case "expired":
		conds = append(conds, "expire_at <= "+bind(now))
	}
	switch closedMode {
	case "1":
		conds = append(conds, "closed = true")
	case "0":
		conds = append(conds, "closed = false")
	}
	if len(conds) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// HandleListSessions GET /api/v1/admin/sessions 会话列表（数据源为 sessions 表，含历史会话）。
// 过滤：UserID / TokenID / ChannelKeyID / Q（session_id 前缀）/ Expired=active|expired / Closed=1|0；
// 分页：Page（≥1，缺省 1）、Size（≥1，缺省 20）；排序：last_active 倒序，同值按 session_id 升序。
// 进行中请求数（Inflight）取自内存注册表（DB 无该运行时字段），其余实时状态按 session_id 聚合。
func (h *Admin) HandleListSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var userID, tokenID, keyID int64
	var err error
	if v := q.Get("UserID"); v != "" {
		userID, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "user_id 必须为数字")
			return
		}
	}
	if v := q.Get("TokenID"); v != "" {
		tokenID, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "token_id 必须为数字")
			return
		}
	}
	if v := q.Get("ChannelKeyID"); v != "" {
		keyID, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "channel_key_id 必须为数字")
			return
		}
	}
	prefix := q.Get("Q")
	expiredMode := q.Get("Expired")
	closedMode := q.Get("Closed") // "1"=仅已关闭；"0"=仅未关闭；空=不过滤
	page := parsePage(q.Get("Page"))
	size := parseSize(q.Get("Size"))
	now := h.now()

	if h.db == nil {
		resp.OK(w, r, map[string]any{"List": []sessionListItem{}, "Total": 0, "Page": page, "Size": size})
		return
	}

	whereSQL, args := sessionListWhere(userID, tokenID, keyID, prefix, expiredMode, closedMode, now)

	var total int64
	if err := h.db.QueryRowContext(r.Context(), `SELECT count(*) FROM sessions`+whereSQL, args...).Scan(&total); err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询会话失败")
		return
	}

	sizeArg := "$" + strconv.Itoa(len(args)+1)
	offsetArg := "$" + strconv.Itoa(len(args)+2)
	listArgs := append(append([]any{}, args...), size, (page-1)*size)
	rows, err := h.db.QueryContext(r.Context(),
		`SELECT `+sessionListCols+` FROM sessions`+whereSQL+
			` ORDER BY last_active DESC, session_id ASC LIMIT `+sizeArg+` OFFSET `+offsetArg, listArgs...)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询会话失败")
		return
	}
	defer rows.Close()

	items := make([]*router.Session, 0, size)
	for rows.Next() {
		var s router.Session
		var tok sql.NullInt64
		if err := rows.Scan(&s.SessionID, &s.UserID, &tok, &s.Model, &s.SessionRaw, &s.ChannelKeyID,
			&s.InternalModelID, &s.CreatedAt, &s.LastActive, &s.ExpireAt, &s.Name, &s.Closed); err != nil {
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询会话失败")
			return
		}
		if tok.Valid {
			s.TokenID = tok.Int64
		}
		items = append(items, &s)
	}
	if err := rows.Err(); err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询会话失败")
		return
	}

	// 进行中请求数：仅内存注册表持有该运行时字段（DB 为投影，不含 Inflight）。
	inflight := map[string]int{}
	if h.sessions != nil {
		for _, s := range h.sessions.ListAll() {
			if s.Inflight > 0 {
				inflight[s.SessionID] = s.Inflight
			}
		}
	}

	// 名称反查仅针对当页展示项（N+1 规避：按 id 批量查询）。
	keyIDs := collectSessionIDs(items, func(s *router.Session) int64 { return s.ChannelKeyID })
	userNames := h.userNamesByIDs(r.Context(), collectSessionIDs(items, func(s *router.Session) int64 { return s.UserID }))
	tokenNames := h.tokenNamesByIDs(r.Context(), collectSessionIDs(items, func(s *router.Session) int64 { return s.TokenID }))
	keyNames := map[int64]string{}
	if h.keyNameByIDs != nil {
		if m, err := h.keyNameByIDs(r.Context(), keyIDs); err == nil && m != nil {
			keyNames = m
		}
	}
	channelNames := h.channelNamesByKeyIDs(r.Context(), keyIDs)

	// 实时状态：按 session_id 批量聚合（billing_records + call_logs）。
	sessionIDs := make([]string, 0, len(items))
	for _, s := range items {
		if s.SessionID != "" {
			sessionIDs = append(sessionIDs, s.SessionID)
		}
	}
	runtimes := sessionRuntimes(r.Context(), h.db, sessionIDs, now.Add(-sessionStatsWindow))

	list := make([]sessionListItem, 0, len(items))
	for _, s := range items {
		item := sessionListItem{
			SessionID:    s.SessionID,
			UserID:       s.UserID,
			TokenID:      s.TokenID,
			Model:        s.Model,
			ChannelKeyID: s.ChannelKeyID,
			SessionRaw:   s.SessionRaw,
			SessionName:  s.Name,
			Closed:       s.Closed,
			CreatedAt:    s.CreatedAt,
			LastActive:   s.LastActive,
			ExpireAt:     s.ExpireAt,
			Expired:      !s.ExpireAt.After(now),
			Inflight:     inflight[s.SessionID],
		}
		if un, ok := userNames[s.UserID]; ok {
			item.UserName = un.Username
			item.UserNickname = un.Nickname
		}
		if s.TokenID != 0 {
			item.TokenName = tokenNames[s.TokenID]
		}
		item.ChannelKeyName = keyNames[s.ChannelKeyID]
		item.ChannelName = channelNames[s.ChannelKeyID]
		if rt, ok := runtimes[s.SessionID]; ok {
			item.Calls = rt.Calls
			item.Tokens = rt.Tokens
			item.Credits = decimalx.Round5(rt.Credits)
			item.ContextLen = rt.ContextLen
			item.RPM = decimalx.Round2(rt.RPM())
			item.TPM = decimalx.Round2(rt.TPM())
			item.UserMsgs = rt.UserMsgs
			item.ToolCalls = rt.ToolCalls
			item.CacheHitRate = decimalx.Round5(rt.CacheHitRate())
		}
		list = append(list, item)
	}
	resp.OK(w, r, map[string]any{"List": list, "Total": total, "Page": page, "Size": size})
}

// kickSessionsRequest 踢下线请求体：任一维度非空即执行，多条件并存为 AND（与 B4 KillByFilter 语义一致）。
type kickSessionsRequest struct {
	SessionIDs   []string `json:"SessionIDs"`
	UserID       int64    `json:"UserID,string"`
	TokenID      int64    `json:"TokenID,string"`
	ChannelKeyID int64    `json:"ChannelKeyID,string"`
}

// HandleKickSessions POST /api/v1/admin/sessions/kick
// 语义为「关闭」：会话不再被路由命中、释放并发槽，但记录保留在列表（Closed 状态）。
// 返回 {affected}。实现：session_ids 先逐个 Close（内存标记 Closed + 清除计数），
// 其余非零维度再调 CloseByFilter（AND 过滤），两者合并计数。
func (h *Admin) HandleKickSessions(w http.ResponseWriter, r *http.Request) {
	var req kickSessionsRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	if len(req.SessionIDs) == 0 && req.UserID == 0 && req.TokenID == 0 && req.ChannelKeyID == 0 {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "至少指定一种关闭维度")
		return
	}
	if h.sessions == nil {
		resp.OK(w, r, map[string]any{"Affected": 0})
		return
	}
	affected := 0
	for _, id := range req.SessionIDs {
		if h.sessions.Close(id) {
			affected++
		}
	}
	if req.UserID != 0 || req.TokenID != 0 || req.ChannelKeyID != 0 {
		affected += h.sessions.CloseByFilter(req.UserID, req.TokenID, req.ChannelKeyID)
	}
	resp.OK(w, r, map[string]any{"Affected": affected})
}

// renameSessionRequest 会话改名请求体。
type renameSessionRequest struct {
	Name string `json:"Name"`
}

// HandleRenameSession PUT /api/v1/admin/sessions/{id}/name
// 返回 {updated}。改名落库（SessionStore.SetName）；未装配/会话不存在时 updated=false。
func (h *Admin) HandleRenameSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "缺少会话 ID")
		return
	}
	var req renameSessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "会话名称不能为空")
		return
	}
	if utf8.RuneCountInString(req.Name) > 100 {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "会话名称过长（≤100 字符）")
		return
	}
	if h.sessionSetName == nil {
		resp.OK(w, r, map[string]any{"Updated": false})
		return
	}
	if h.sessions != nil {
		// 同步内存注册表（列表读内存优先；改名后立即生效）
		if s, ok := h.sessions.Lookup(id); ok {
			s.Name = req.Name
		}
	}
	updated, err := h.sessionSetName(r.Context(), id, req.Name)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "会话改名失败")
		return
	}
	resp.OK(w, r, map[string]any{"Updated": updated})
}

// sessionNames 批量反查会话可读名：优先用账单行冗余的 session_name 快照
// （写入时固化，不随 sessions 投影过期清理丢失），缺失的再回查 sessions 表。
func (h *Admin) sessionNames(ctx context.Context, recs []billing.Record) map[string]string {
	out := make(map[string]string)
	seen := make(map[string]struct{}, len(recs))
	var ids []string
	for i := range recs {
		id := recs[i].SessionID
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		if recs[i].SessionName != "" {
			out[id] = recs[i].SessionName
			continue
		}
		ids = append(ids, id)
	}
	if len(ids) == 0 || h.db == nil {
		return out
	}
	rows, err := h.db.QueryContext(ctx,
		`SELECT session_id, COALESCE(name, '') FROM sessions WHERE session_id = ANY($1)`, ids)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err == nil {
			out[id] = name
		}
	}
	return out
}

// sessionStatsWindow 会话「实时状态」中 RPM/TPM 的统计窗口（近 5 分钟）。
const sessionStatsWindow = 5 * time.Minute

// sessionRuntime 会话实时聚合（按 session_id 汇总；来源 billing_records 与 call_logs）。
type sessionRuntime struct {
	Calls       int64   // 总调用次数
	Tokens      int64   // 总消耗 Token（输入+缓存读+输出，三段互斥直接求和）
	Credits     float64 // 总消耗积分
	InputTokens int64   // 纯输入总量（不含缓存读）
	CacheRead   int64   // 缓存读命中量
	ContextLen  int64   // 最近一次请求的总输入 Token 数（输入+缓存读，即当前上下文规模）
	WinCalls    int64   // 窗口内调用次数
	WinTokens   int64   // 窗口内 Token 数
	UserMsgs    int64   // 用户消息次数
	ToolCalls   int64   // 工具调用次数（req_messages 中 role=tool 计数）
}

// RPM 窗口内每分钟平均请求数。
func (r sessionRuntime) RPM() float64 {
	return float64(r.WinCalls) / sessionStatsWindow.Minutes()
}

// TPM 窗口内每分钟平均 Token 数。
func (r sessionRuntime) TPM() float64 {
	return float64(r.WinTokens) / sessionStatsWindow.Minutes()
}

// CacheHitRate 缓存命中率 = 缓存读 ÷ 总输入(输入+缓存读)；无输入返回 0。
func (r sessionRuntime) CacheHitRate() float64 {
	totalIn := r.InputTokens + r.CacheRead
	if totalIn <= 0 {
		return 0
	}
	return float64(r.CacheRead) / float64(totalIn)
}

// sessionRuntimes 批量聚合会话实时状态：billing_records（次数/Token/积分/上下文/窗口速率）
// 与 call_logs（用户消息数/工具调用数）。查询失败仅返回已得部分，不影响会话列表主流程。
func sessionRuntimes(ctx context.Context, db *sql.DB, ids []string, windowStart time.Time) map[string]sessionRuntime {
	out := make(map[string]sessionRuntime, len(ids))
	if db == nil || len(ids) == 0 {
		return out
	}
	// 汇总 + 窗口（FILTER）+ 输入/缓存读。
	rows, err := db.QueryContext(ctx, `
SELECT session_id,
       count(*),
       COALESCE(sum(credits_consumed), 0),
       COALESCE(sum(
           COALESCE(NULLIF(tokens->>'input','')::bigint, 0) +
           COALESCE(NULLIF(tokens->>'cache_read','')::bigint, 0) +
           COALESCE(NULLIF(tokens->>'output','')::bigint, 0)), 0),
       COALESCE(sum(COALESCE(NULLIF(tokens->>'input','')::bigint, 0)), 0),
       COALESCE(sum(COALESCE(NULLIF(tokens->>'cache_read','')::bigint, 0)), 0),
       COALESCE(count(*) FILTER (WHERE call_time >= $2), 0),
       COALESCE(sum(
           COALESCE(NULLIF(tokens->>'input','')::bigint, 0) +
           COALESCE(NULLIF(tokens->>'cache_read','')::bigint, 0) +
           COALESCE(NULLIF(tokens->>'output','')::bigint, 0)) FILTER (WHERE call_time >= $2), 0)
FROM billing_records
WHERE session_id = ANY($1)
GROUP BY session_id`, ids, windowStart)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var sid string
			var rt sessionRuntime
			if err := rows.Scan(&sid, &rt.Calls, &rt.Credits, &rt.Tokens, &rt.InputTokens,
				&rt.CacheRead, &rt.WinCalls, &rt.WinTokens); err != nil {
				continue
			}
			out[sid] = rt
		}
	}
	// 上下文长度：最近一次请求的总输入 Token 数（输入 + 缓存读，三段口径下二者互斥）。
	crows, err := db.QueryContext(ctx, `
SELECT DISTINCT ON (session_id) session_id,
       COALESCE(NULLIF(tokens->>'input','')::bigint, 0) + COALESCE(NULLIF(tokens->>'cache_read','')::bigint, 0)
FROM billing_records
WHERE session_id = ANY($1)
ORDER BY session_id, call_time DESC, id DESC`, ids)
	if err == nil {
		defer crows.Close()
		for crows.Next() {
			var sid string
			var ctxLen int64
			if err := crows.Scan(&sid, &ctxLen); err != nil {
				continue
			}
			rt := out[sid]
			rt.ContextLen = ctxLen
			out[sid] = rt
		}
	}
	// 用户消息数 / 工具调用数：来自 call_logs.req_messages（增量消息数组）。
	lrows, err := db.QueryContext(ctx, `
SELECT session_id,
       COALESCE(sum(CASE WHEN jsonb_typeof(req_messages) = 'array'
           THEN (SELECT count(*) FROM jsonb_array_elements(req_messages) e WHERE e->>'role' = 'user') ELSE 0 END), 0),
       COALESCE(sum(CASE WHEN jsonb_typeof(req_messages) = 'array'
           THEN (SELECT count(*) FROM jsonb_array_elements(req_messages) e WHERE e->>'role' = 'tool') ELSE 0 END), 0)
FROM call_logs
WHERE session_id = ANY($1)
GROUP BY session_id`, ids)
	if err == nil {
		defer lrows.Close()
		for lrows.Next() {
			var sid string
			var userMsgs, toolCalls int64
			if err := lrows.Scan(&sid, &userMsgs, &toolCalls); err != nil {
				continue
			}
			rt := out[sid]
			rt.UserMsgs = userMsgs
			rt.ToolCalls = toolCalls
			out[sid] = rt
		}
	}
	return out
}

// channelNamesByKeyIDs 批量由渠道密钥 ID 反查所属渠道名；失败返回 nil（会话列表以空串兜底）。
func (h *Admin) channelNamesByKeyIDs(ctx context.Context, keyIDs []int64) map[int64]string {
	if h.db == nil || len(keyIDs) == 0 {
		return nil
	}
	rows, err := h.db.QueryContext(ctx,
		`SELECT k.id, c.name FROM channel_keys k JOIN channels c ON c.id = k.channel_id
		 WHERE k.id = ANY('{`+int64List(keyIDs)+`}'::bigint[])`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make(map[int64]string, len(keyIDs))
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			continue
		}
		out[id] = name
	}
	return out
}

func collectSessionIDs(ss []*router.Session, get func(*router.Session) int64) []int64 {
	seen := make(map[int64]struct{}, len(ss))
	ids := make([]int64, 0, len(ss))
	for _, s := range ss {
		id := get(s)
		if id == 0 {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

// userNamesByIDs 批量查询 users.username 与 nickname；失败返回 nil（不影响会话主查询）。
func (h *Admin) userNamesByIDs(ctx context.Context, ids []int64) map[int64]userName {
	if h.db == nil || len(ids) == 0 {
		return nil
	}
	rows, err := h.db.QueryContext(ctx,
		`SELECT id, username, nickname FROM users WHERE id = ANY('{`+int64List(ids)+`}'::bigint[])`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make(map[int64]userName, len(ids))
	for rows.Next() {
		var id int64
		var un userName
		if err := rows.Scan(&id, &un.Username, &un.Nickname); err != nil {
			continue
		}
		out[id] = un
	}
	return out
}

// tokenNamesByIDs 批量查询 tokens.display_name；失败返回 nil。
func (h *Admin) tokenNamesByIDs(ctx context.Context, ids []int64) map[int64]string {
	if h.db == nil || len(ids) == 0 {
		return nil
	}
	rows, err := h.db.QueryContext(ctx,
		`SELECT id, display_name FROM tokens WHERE id = ANY('{`+int64List(ids)+`}'::bigint[])`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := make(map[int64]string, len(ids))
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			continue
		}
		out[id] = name
	}
	return out
}

// ===== 会话成本路由面板（Step 17）=====
//
// 三个只读端点，所有值均显式标注来源标签（Source/*Source 字段），便于前端区分
// 「模型成本 / 真实 token / 当前时刻 / 实际分档」四类数据来源：
//   - GET /api/v1/admin/session-cost/config       各候选渠道三档成本价 + 时段/分档倍率 + 当前时刻倍率 +
//     routing.cost_sample 默认样值 + k 系数与门控阈值；
//   - GET /api/v1/admin/session-cost/events       读 KindSessionCostEvent 落地的事件流（有界进程内存储）；
//   - GET /api/v1/admin/session-cost/distribution 按会话聚合临界缓存读（crossCacheRead）的分位/直方。

// 来源标签常量：面板所有值均显式标注来源，避免前端把「模型成本」与「售价/真实 token」混淆。
const (
	sourceModelCost      = "模型成本"
	sourceCurrentTime    = "当前时刻"
	sourceContextTier    = "实际分档"
	sourceCostSample     = "routing.cost_sample 默认样值（真实 token）"
	sourceTimeConfig     = "billing.time_config"
	sourceTiersConfig    = "billing.context_tiers"
	sourceSessionCostCfg = "routing.session_cost"
	sourceSessionCostEvt = "asyncwrite KindSessionCostEvent（进程内落地）"
	sourceCrossCacheRead = "crossCacheRead（模型成本 × 真实 token）"
)

// sessionCostTierView 单一候选渠道的三档成本价（模型成本来源）。
type sessionCostTierView struct {
	Input     float64 `json:"Input"`
	CacheRead float64 `json:"CacheRead"`
	Output    float64 `json:"Output"`
	Source    string  `json:"Source"`
}

// sessionCostCandidateView 单个候选渠道的面板视图：三档成本价 + 时段/分档倍率（含来源标签）。
type sessionCostCandidateView struct {
	ChannelID       int64               `json:"ChannelID,string"`
	InternalModelID string              `json:"InternalModelID"`
	Cost            sessionCostTierView `json:"Cost"`
	TimeCoeff       float64             `json:"TimeCoeff"`
	TimeCoeffSource string              `json:"TimeCoeffSource"`
	CtxCoeff        float64             `json:"CtxCoeff"`
	CtxCoeffSource  string              `json:"CtxCoeffSource"`
}

// sessionCostGates 经验式剩余轮数 k 系数与中途切换门控阈值（均取自 routing.session_cost）。
type sessionCostGates struct {
	RemainingA   float64 `json:"RemainingA"`
	RemainingB   float64 `json:"RemainingB"`
	RemainingMin int     `json:"RemainingMin"`
	RemainingMax int     `json:"RemainingMax"`
	MinRemaining int     `json:"MinRemaining"`
	CooldownSec  int     `json:"CooldownSec"`
	MinImprove   float64 `json:"MinImprove"`
	Source       string  `json:"Source"`
}

// sessionCostConfigResponse 「会话成本路由」配置面板响应。
type sessionCostConfigResponse struct {
	Now              time.Time                  `json:"Now"`
	NowTimeCoeff     float64                    `json:"NowTimeCoeff"`
	NowTimeCoeffSrc  string                     `json:"NowTimeCoeffSource"`
	TimeConfig       json.RawMessage            `json:"TimeConfig,omitempty"`
	TimeConfigSource string                     `json:"TimeConfigSource"`
	ContextTiers     json.RawMessage            `json:"ContextTiers,omitempty"`
	TiersSource      string                     `json:"ContextTiersSource"`
	CostSample       billing.WireUsage          `json:"CostSample"`
	CostSampleSource string                     `json:"CostSampleSource"`
	SessionCost      router.SessionCostConfig   `json:"SessionCost"`
	SessionCostSrc   string                     `json:"SessionCostSource"`
	Gates            sessionCostGates           `json:"Gates"`
	Candidates       []sessionCostCandidateView `json:"Candidates"`
	CandidatesSource string                     `json:"CandidatesSource"`
}

// HandleSessionCostConfig GET /api/v1/admin/session-cost/config
// 返回面板所需的全部配置项与候选渠道成本视图；?Model=<对外模型名> 时附带该模型的候选渠道。
func (h *Admin) HandleSessionCostConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	now := h.now()

	globalTC, timeRaw := h.sessionCostGlobalTimeConfig(ctx)
	globalTiers, tiersRaw := h.sessionCostGlobalTiers(ctx)
	sample := h.sessionCostSample(ctx)
	cfg, _ := h.sessionCostRoutingConfig(ctx)

	nowCoeff := 1.0
	if globalTC != nil {
		if v, err := globalTC.CoeffFor(now); err == nil {
			nowCoeff = v
		}
	}
	cands := h.sessionCostCandidates(ctx, r.URL.Query().Get("Model"), now, sample, globalTC, globalTiers)

	resp.OK(w, r, sessionCostConfigResponse{
		Now:              now,
		NowTimeCoeff:     nowCoeff,
		NowTimeCoeffSrc:  sourceCurrentTime,
		TimeConfig:       json.RawMessage(timeRaw),
		TimeConfigSource: sourceTimeConfig,
		ContextTiers:     json.RawMessage(tiersRaw),
		TiersSource:      sourceTiersConfig,
		CostSample:       billing.WireUsage(sample),
		CostSampleSource: sourceCostSample,
		SessionCost:      cfg,
		SessionCostSrc:   sourceSessionCostCfg,
		Gates: sessionCostGates{
			RemainingA: cfg.RemainingA, RemainingB: cfg.RemainingB,
			RemainingMin: cfg.RemainingMin, RemainingMax: cfg.RemainingMax,
			MinRemaining: cfg.MinRemaining, CooldownSec: cfg.CooldownSec,
			MinImprove: cfg.MinImprove, Source: sourceSessionCostCfg,
		},
		Candidates:       cands,
		CandidatesSource: sourceModelCost,
	})
}

// sessionCostGlobalTimeConfig 读取全局时段配置（billing.time_config）。
// 缺失/非法返回 (nil, nil)；解析成功返回配置与原始 JSON（供前端展示）。
func (h *Admin) sessionCostGlobalTimeConfig(ctx context.Context) (*billing.TimeCoeffConfig, []byte) {
	if h.billing == nil {
		return nil, nil
	}
	raw, err := h.billing.GetConfig(ctx, "billing.time_config")
	if err != nil {
		return nil, nil
	}
	tc, err := billing.ParseTimeConfig(raw)
	if err != nil {
		return nil, raw
	}
	return &tc, raw
}

// sessionCostGlobalTiers 读取全局上下文分档（billing.context_tiers）；缺失（未装配/读取失败）返回 (nil, nil)，
// 解析失败返回 (nil, raw)（原始 JSON 供前端展示），成功返回 (tiers, raw)。
func (h *Admin) sessionCostGlobalTiers(ctx context.Context) ([]billing.TierRule, []byte) {
	if h.billing == nil {
		return nil, nil
	}
	raw, err := h.billing.GetConfig(ctx, "billing.context_tiers")
	if err != nil {
		return nil, nil
	}
	var tiers []billing.TierRule
	if err := json.Unmarshal(raw, &tiers); err != nil {
		return nil, raw
	}
	return tiers, raw
}

// sessionCostSample 读取路由成本比较样值（routing.cost_sample，三段口径）；缺失/非法回落内置默认。
func (h *Admin) sessionCostSample(ctx context.Context) billing.Usage {
	if h.billing != nil {
		if raw, err := h.billing.GetConfig(ctx, "routing.cost_sample"); err == nil {
			var u billing.Usage
			if json.Unmarshal(raw, &u) == nil {
				return u
			}
		}
	}
	return billing.Usage{Input: 10000, CacheRead: 9000, Output: 1000}
}

// sessionCostRoutingConfig 读取会话成本路由配置（routing.session_cost）并解析为结构化配置；缺失回落零值。
func (h *Admin) sessionCostRoutingConfig(ctx context.Context) (router.SessionCostConfig, []byte) {
	var raw []byte
	if h.billing != nil {
		if b, err := h.billing.GetConfig(ctx, "routing.session_cost"); err == nil {
			raw = b
		}
	}
	var cfg router.SessionCostConfig
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &cfg)
	}
	return cfg, raw
}

// sessionCostCandidates 枚举某对外模型下绑定的候选渠道：三档成本价（模型成本）+ 时段/分档倍率。
// 倍率口径与路由决策同源（router.ResolveTimeCoeff/ResolveCtxCoeff：模型级优先，回落全局）。
func (h *Admin) sessionCostCandidates(ctx context.Context, model string, now time.Time, sample billing.Usage, globalTC *billing.TimeCoeffConfig, globalTiers []billing.TierRule) []sessionCostCandidateView {
	out := []sessionCostCandidateView{}
	if h.channelMgr == nil || model == "" {
		return out
	}
	extID, ok := h.externalModelID(ctx, model)
	if !ok {
		return out
	}
	cms, err := h.channelMgr.ListByExternal(ctx, extID)
	if err != nil {
		return out
	}
	for i := range cms {
		cm := cms[i]
		tc := router.ResolveTimeCoeff(cm.TimeConfig, globalTC, now)
		ctxCoeff := router.ResolveCtxCoeff(cm.ContextTiers, globalTiers, sample)
		out = append(out, sessionCostCandidateView{
			ChannelID:       cm.ChannelID,
			InternalModelID: cm.InternalModelID,
			Cost: sessionCostTierView{
				Input:     cm.CostRates["input"],
				CacheRead: cm.CostRates["cache_read"],
				Output:    cm.CostRates["output"],
				Source:    sourceModelCost,
			},
			TimeCoeff:       tc,
			TimeCoeffSource: sourceCurrentTime,
			CtxCoeff:        ctxCoeff,
			CtxCoeffSource:  sourceContextTier,
		})
	}
	return out
}

// externalModelID 由对外模型名解析其 ID（仅启用项）；未装配/未命中返回 (0, false)。
func (h *Admin) externalModelID(ctx context.Context, name string) (int64, bool) {
	if h.models == nil {
		return 0, false
	}
	enabled := true
	list, err := h.models.ListModels(ctx, &enabled)
	if err != nil {
		return 0, false
	}
	for i := range list {
		if list[i].ExternalName == name {
			return list[i].ID, true
		}
	}
	return 0, false
}

// sessionCostEventView 单条会话成本决策产物的事件视图（含来源标签，PaybackTurns/PaybackLimit 为 NaN/Inf 时置 null）。
type sessionCostEventView struct {
	SessionID          string   `json:"SessionID"`
	Turn               int      `json:"Turn"`
	Shadow             bool     `json:"Shadow"`
	Switch             bool     `json:"Switch"`
	FromChannelKeyID   int64    `json:"FromChannelKeyID,string"`
	ToChannelKeyID     int64    `json:"ToChannelKeyID,string"`
	TurnCostOld        float64  `json:"TurnCostOld"`
	TurnCostNew        float64  `json:"TurnCostNew"`
	RechargeCost       float64  `json:"RechargeCost"`
	PaybackTurns       *float64 `json:"PaybackTurns"`
	PaybackLimit       *float64 `json:"PaybackLimit"`
	RemainingTurns     int      `json:"RemainingTurns"`
	Reason             string   `json:"Reason"`
	CrossCacheRead     float64  `json:"CrossCacheRead"`
	CrossCacheReadOK   bool     `json:"CrossCacheReadKnown"`
	CrossCacheReadFrom string   `json:"CrossCacheReadSource"`
}

// sessionCostEventsResponse /events 响应。
type sessionCostEventsResponse struct {
	List   []sessionCostEventView `json:"List"`
	Total  int                    `json:"Total"`
	Source string                 `json:"Source"`
}

// HandleSessionCostEvents GET /api/v1/admin/session-cost/events
// 读 KindSessionCostEvent 落地的事件流（有界进程内存储）；?SessionID= 过滤、?Limit= 截取最近 N 条。
func (h *Admin) HandleSessionCostEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sid := q.Get("SessionID")
	limit := parsePositiveInt(q.Get("Limit"))

	events := h.sessionCostEventSnapshot()
	list := make([]sessionCostEventView, 0, len(events))
	for i := range events {
		if sid != "" && events[i].SessionID != sid {
			continue
		}
		list = append(list, toSessionCostEventView(events[i]))
	}
	if limit > 0 && len(list) > limit {
		list = list[len(list)-limit:] // 保留最近的 limit 条（事件流按写入顺序旧→新）
	}
	resp.OK(w, r, sessionCostEventsResponse{List: list, Total: len(list), Source: sourceSessionCostEvt})
}

// sessionCostHistogramBin 直方图分桶（UpperBound 为该桶上界，末桶含最大值）。
type sessionCostHistogramBin struct {
	UpperBound float64 `json:"UpperBound"`
	Count      int     `json:"Count"`
}

// sessionCostDistributionItem 单会话的临界缓存读分位与直方。
type sessionCostDistributionItem struct {
	SessionID       string                    `json:"SessionID"`
	Count           int                       `json:"Count"`
	P10             float64                   `json:"P10"`
	Median          float64                   `json:"Median"`
	P90             float64                   `json:"P90"`
	QuantilesSource string                    `json:"QuantilesSource"`
	Histogram       []sessionCostHistogramBin `json:"Histogram"`
}

// sessionCostDistributionResponse /distribution 响应。
type sessionCostDistributionResponse struct {
	Sessions      []sessionCostDistributionItem `json:"Sessions"`
	TotalSessions int                           `json:"TotalSessions"`
	Source        string                        `json:"Source"`
}

// HandleSessionCostDistribution GET /api/v1/admin/session-cost/distribution
// 按会话聚合临界缓存读（crossCacheRead，仅取 CrossCacheReadKnown=true 的事件）的分位（P10/中位/P90）与直方。
func (h *Admin) HandleSessionCostDistribution(w http.ResponseWriter, r *http.Request) {
	events := h.sessionCostEventSnapshot()
	bySession := make(map[string][]float64, len(events))
	for i := range events {
		ev := events[i]
		if !ev.CrossCacheReadKnown {
			continue
		}
		v := ev.CrossCacheRead
		if math.IsNaN(v) || math.IsInf(v, 0) {
			continue
		}
		bySession[ev.SessionID] = append(bySession[ev.SessionID], v)
	}
	ids := make([]string, 0, len(bySession))
	for id := range bySession {
		ids = append(ids, id)
	}
	sort.Strings(ids) // 输出确定性：按 session_id 升序

	items := make([]sessionCostDistributionItem, 0, len(ids))
	for _, id := range ids {
		vals := bySession[id]
		sort.Float64s(vals)
		items = append(items, sessionCostDistributionItem{
			SessionID:       id,
			Count:           len(vals),
			P10:             quantile(vals, 0.10),
			Median:          quantile(vals, 0.50),
			P90:             quantile(vals, 0.90),
			QuantilesSource: sourceCrossCacheRead,
			Histogram:       histogram(vals, 10),
		})
	}
	resp.OK(w, r, sessionCostDistributionResponse{
		Sessions: items, TotalSessions: len(items), Source: sourceCrossCacheRead,
	})
}

// sessionCostEventSnapshot 读取事件存储快照（未注入时返回 nil）。
func (h *Admin) sessionCostEventSnapshot() []router.SessionCostEvent {
	if h.sessionCostEvt == nil {
		return nil
	}
	return h.sessionCostEvt.Snapshot()
}

// toSessionCostEventView 把事件转为面板视图（PaybackTurns 为 NaN/Inf 时序列化为 null，避免响应序列化失败）。
func toSessionCostEventView(ev router.SessionCostEvent) sessionCostEventView {
	d := ev.Decision
	return sessionCostEventView{
		SessionID:          ev.SessionID,
		Turn:               ev.Turn,
		Shadow:             ev.Shadow,
		Switch:             d.Switch,
		FromChannelKeyID:   d.FromChannelKeyID,
		ToChannelKeyID:     d.ToChannelKeyID,
		TurnCostOld:        d.TurnCostOld,
		TurnCostNew:        d.TurnCostNew,
		RechargeCost:       d.RechargeCost,
		PaybackTurns:       finitePtr(d.PaybackTurns),
		PaybackLimit:       finitePtr(d.PaybackLimit),
		RemainingTurns:     d.RemainingTurns,
		Reason:             d.Reason,
		CrossCacheRead:     ev.CrossCacheRead,
		CrossCacheReadOK:   ev.CrossCacheReadKnown,
		CrossCacheReadFrom: sourceCrossCacheRead,
	}
}

// finitePtr 把有限浮点转为指针；NaN/±Inf 返回 nil（JSON null）。
func finitePtr(v float64) *float64 {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return nil
	}
	return &v
}

// parsePositiveInt 解析正整数参数；非法/≤0 返回 0（表示不限制）。
func parsePositiveInt(v string) int {
	if n, err := strconv.Atoi(v); err == nil && n > 0 {
		return n
	}
	return 0
}

// quantile 对已升序排列的样本求分位（q∈[0,1]，线性插值）；空样本返回 0。
func quantile(sorted []float64, q float64) float64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return sorted[0]
	}
	if q < 0 {
		q = 0
	}
	if q > 1 {
		q = 1
	}
	pos := q * float64(n-1)
	lo := int(math.Floor(pos))
	hi := int(math.Ceil(pos))
	if lo == hi {
		return sorted[lo]
	}
	frac := pos - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}

// histogram 对已升序样本做等宽分桶（bins 个桶，UpperBound 为桶上界）；样本全等时返回单桶。
func histogram(sorted []float64, bins int) []sessionCostHistogramBin {
	if len(sorted) == 0 {
		return []sessionCostHistogramBin{}
	}
	if bins <= 0 {
		bins = 1
	}
	lo, hi := sorted[0], sorted[len(sorted)-1]
	if hi <= lo {
		return []sessionCostHistogramBin{{UpperBound: hi, Count: len(sorted)}}
	}
	width := (hi - lo) / float64(bins)
	counts := make([]int, bins)
	for _, v := range sorted {
		idx := int((v - lo) / width)
		if idx >= bins {
			idx = bins - 1
		}
		counts[idx]++
	}
	out := make([]sessionCostHistogramBin, 0, bins)
	for i := 0; i < bins; i++ {
		out = append(out, sessionCostHistogramBin{UpperBound: lo + width*float64(i+1), Count: counts[i]})
	}
	return out
}
