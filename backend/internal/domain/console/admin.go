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
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
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

// ===== 账单查询 =====

// billingListItem 账单列表项（user_id 仅内部传参用，前端展示一律用 Username）。
// Tokens 为五段用量；Rates/CoeffTime/CoeffContext/RValue 供前端悬停展示计费公式。
// Tokens/Rates 为 billing 存储结构（JSONB 契约），内部字段保持 snake_case。
type billingListItem struct {
	BillingID       string        `json:"BillingID"`
	UserID          int64         `json:"UserID,string"`
	Username        string        `json:"Username,omitempty"`
	UserNickname    string        `json:"UserNickname,omitempty"` // 昵称（join users；为空表示未设置）
	TokenName       string        `json:"TokenName,omitempty"`
	ExternalModel   string        `json:"ExternalModel"`
	PricingMode     string        `json:"PricingMode"`
	CreditsConsumed float64       `json:"CreditsConsumed"`
	Status          string        `json:"Status"`
	CallTime        time.Time     `json:"CallTime"`
	InternalModelID string        `json:"InternalModelID"`
	ChannelKeyID    int64         `json:"ChannelKeyID,string"`
	KeyName         string        `json:"KeyName,omitempty"` // 密钥名（join channel_keys；缺失时兜底 #<id>）
	SessionID       string        `json:"SessionID,omitempty"`
	SessionName     string        `json:"SessionName,omitempty"` // 会话可读名（sessions.name；缺失留空）
	ChannelName     string        `json:"ChannelName,omitempty"`
	Tokens          billing.Usage `json:"Tokens"`
	Rates           billing.Rates `json:"Rates,omitempty"`
	CoeffTime       float64       `json:"CoeffTime,omitempty"`
	CoeffContext    float64       `json:"CoeffContext,omitempty"`
	RValue          int64         `json:"RValue,omitempty"`
	ErrorMessage    string        `json:"ErrorMessage,omitempty"`
	DurationMs      *int64        `json:"DurationMs,omitempty"`
	FirstTokenMs    *int64        `json:"FirstTokenMs,omitempty"`
}

func toListItem(rec *billing.Record) billingListItem {
	return billingListItem{
		BillingID:       rec.BillingID,
		UserID:          rec.UserID,
		ExternalModel:   rec.ExternalModel,
		PricingMode:     rec.PricingMode,
		CreditsConsumed: decimalx.Round5(rec.CreditsConsumed),
		Status:          rec.Status,
		CallTime:        rec.CallTime,
		InternalModelID: rec.InternalModelID,
		ChannelKeyID:    rec.ChannelKeyID,
		SessionID:       rec.SessionID,
		Tokens:          rec.Tokens,
		Rates:           rec.Rates,
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
		Model:       q.Get("model"),
		PricingMode: q.Get("pricing_mode"),
		Status:      q.Get("status"),
		SessionID:   q.Get("session_id"),
	}
	if v := q.Get("user_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return f, errors.New("user_id 必须为数字")
		}
		f.UserID = &id
	}
	if v := q.Get("token_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return f, errors.New("token_id 必须为数字")
		}
		f.TokenID = &id
	}
	if v := q.Get("channel_key_id"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return f, errors.New("channel_key_id 必须为数字")
		}
		f.ChannelKeyID = &id
	}
	if v := q.Get("from"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
		if err != nil {
			return f, errors.New("from 时间格式应为 RFC3339")
		}
		f.From = &t
	}
	if v := q.Get("to"); v != "" {
		t, err := time.Parse(time.RFC3339, v)
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
	f.Page = parsePage(q.Get("page"))
	f.Size = parseSize(q.Get("size"))
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
	resp.OK(w, r, map[string]any{"list": list, "total": total, "page": f.Page, "size": f.Size})
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
		PricingMode:     rec.PricingMode,
		CreditsConsumed: decimalx.Round5(rec.CreditsConsumed),
		Status:          rec.Status,
		Steps:           steps,
		RouteDiff:       routeDiff,
		InternalModelID: rec.InternalModelID,
		ChannelName:     h.channelNameByKeyID(r.Context(), rec.ChannelKeyID),
	}
	resp.OK(w, r, detail)
}

// HandleChannelSnapshot GET /api/v1/admin/channels/snapshot
func (h *Admin) HandleChannelSnapshot(w http.ResponseWriter, r *http.Request) {
	resp.OK(w, r, h.channelMgr.Snapshot())
}

// ===== 计费配置读写 =====

// HandleGetBillingConfig GET /api/v1/admin/configs/billing
func (h *Admin) HandleGetBillingConfig(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	rRaw, err := h.billing.GetConfig(ctx, "billing.r")
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "读取计费配置失败")
		return
	}
	cnyRaw, err := h.billing.GetConfig(ctx, "billing.cny_rate")
	if err != nil {
		// 兼容旧库未初始化汇率的情况，回退默认 6.8。
		cnyRaw = json.RawMessage(`6.8`)
	}
	tiersRaw, err := h.billing.GetConfig(ctx, "billing.context_tiers")
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "读取计费配置失败")
		return
	}
	timeRaw, err := h.billing.GetConfig(ctx, "billing.time_config")
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "读取计费配置失败")
		return
	}
	// credit_value 由 r 派生（R / 1e6，每积分对应人民币元），不落库，解析失败时回退 0。
	creditV, _ := creditValueFromRaw(rRaw)
	creditRaw, _ := json.Marshal(creditV)
	resp.OK(w, r, map[string]json.RawMessage{
		"r":             rRaw,
		"cny_rate":      cnyRaw,
		"credit_value":  creditRaw,
		"context_tiers": tiersRaw,
		"time_config":   timeRaw,
	})
}

// HandlePutBillingConfig PUT /api/v1/admin/configs/billing
func (h *Admin) HandlePutBillingConfig(w http.ResponseWriter, r *http.Request) {
	var req struct {
		R            json.RawMessage `json:"R"`
		CnyRate      json.RawMessage `json:"CnyRate"`
		ContextTiers json.RawMessage `json:"ContextTiers"`
		TimeConfig   json.RawMessage `json:"TimeConfig"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	if !validR(req.R) {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "r 必须是整数或数字字符串")
		return
	}
	if len(req.CnyRate) > 0 && !validPositiveNumber(req.CnyRate) {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "cny_rate 必须是正数或数字字符串")
		return
	}
	if !isJSONArray(req.ContextTiers) {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "context_tiers 必须是 JSON 数组")
		return
	}
	if !isJSONObject(req.TimeConfig) {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "time_config 必须是 JSON 对象")
		return
	}
	operator, _ := h.userIDFrom(r.Context())
	ctx := r.Context()
	// 仅更新前端显式传入的字段；r/cny_rate 未传时保留数据库原值（兼容旧前端不全量提交）。
	// r 必须为正整数（validR 已校验 > 0），防止把 0/负值写入导致计费引擎运行时报错。
	if len(req.R) > 0 {
		if err := h.billing.UpsertConfig(ctx, "billing.r", req.R, operator); err != nil {
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "更新计费配置失败")
			return
		}
	}
	if len(req.CnyRate) > 0 {
		if err := h.billing.UpsertConfig(ctx, "billing.cny_rate", req.CnyRate, operator); err != nil {
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "更新计费配置失败")
			return
		}
	}
	if err := h.billing.UpsertConfig(ctx, "billing.context_tiers", req.ContextTiers, operator); err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "更新计费配置失败")
		return
	}
	if err := h.billing.UpsertConfig(ctx, "billing.time_config", req.TimeConfig, operator); err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "更新计费配置失败")
		return
	}
	resp.OK(w, r, map[string]any{"affected": 1})
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
	SessionRaw     string    `json:"SessionRaw"`
	SessionName    string    `json:"SessionName"`
	Closed         bool      `json:"Closed"`
	CreatedAt      time.Time `json:"CreatedAt"`
	LastActive     time.Time `json:"LastActive"`
	ExpireAt       time.Time `json:"ExpireAt"`
	Expired        bool      `json:"Expired"`
}

// HandleListSessions GET /api/v1/admin/sessions
// 过滤：user_id / token_id / channel_key_id / q（session_id 前缀）/ expired=active|expired|all（缺省 all）；
// 分页：page（≥1，缺省 1）、size（≥1，缺省 20）；排序：last_active 倒序，同值按 session_id 升序。
func (h *Admin) HandleListSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var userID, tokenID, keyID int64
	var err error
	if v := q.Get("user_id"); v != "" {
		userID, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "user_id 必须为数字")
			return
		}
	}
	if v := q.Get("token_id"); v != "" {
		tokenID, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "token_id 必须为数字")
			return
		}
	}
	if v := q.Get("channel_key_id"); v != "" {
		keyID, err = strconv.ParseInt(v, 10, 64)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "channel_key_id 必须为数字")
			return
		}
	}
	prefix := q.Get("q")
	expiredMode := q.Get("expired")
	closedMode := q.Get("closed") // "1"=仅已关闭；"0"=仅未关闭（进行中+未过期）；空=不过滤
	page := parsePage(q.Get("page"))
	size := parseSize(q.Get("size"))

	all := []*router.Session{}
	if h.sessions != nil {
		all = h.sessions.ListAll()
	}
	now := h.now()
	filtered := make([]*router.Session, 0, len(all))
	for _, s := range all {
		if userID != 0 && s.UserID != userID {
			continue
		}
		if tokenID != 0 && s.TokenID != tokenID {
			continue
		}
		if keyID != 0 && s.ChannelKeyID != keyID {
			continue
		}
		if prefix != "" && !strings.HasPrefix(s.SessionID, prefix) {
			continue
		}
		expired := !s.ExpireAt.After(now)
		switch expiredMode {
		case "active":
			if expired {
				continue
			}
		case "expired":
			if !expired {
				continue
			}
		}
		switch closedMode {
		case "1":
			if !s.Closed {
				continue
			}
		case "0":
			if s.Closed {
				continue
			}
		}
		filtered = append(filtered, s)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].LastActive.Equal(filtered[j].LastActive) {
			return filtered[i].SessionID < filtered[j].SessionID
		}
		return filtered[i].LastActive.After(filtered[j].LastActive)
	})

	total := len(filtered)
	start := (page - 1) * size
	if start > total {
		start = total
	}
	end := start + size
	if end > total {
		end = total
	}
	items := filtered[start:end]

	// 名称反查仅针对当页展示项（N+1 规避：按 id 批量查询）。
	userNames := h.userNamesByIDs(r.Context(), collectSessionIDs(items, func(s *router.Session) int64 { return s.UserID }))
	tokenNames := h.tokenNamesByIDs(r.Context(), collectSessionIDs(items, func(s *router.Session) int64 { return s.TokenID }))
	keyNames := map[int64]string{}
	if h.keyNameByIDs != nil {
		if m, err := h.keyNameByIDs(r.Context(), collectSessionIDs(items, func(s *router.Session) int64 { return s.ChannelKeyID })); err == nil && m != nil {
			keyNames = m
		}
	}

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
		}
		if un, ok := userNames[s.UserID]; ok {
			item.UserName = un.Username
			item.UserNickname = un.Nickname
		}
		if s.TokenID != 0 {
			item.TokenName = tokenNames[s.TokenID]
		}
		item.ChannelKeyName = keyNames[s.ChannelKeyID]
		list = append(list, item)
	}
	resp.OK(w, r, map[string]any{"list": list, "total": total, "page": page, "size": size})
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
		resp.OK(w, r, map[string]any{"affected": 0})
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
	resp.OK(w, r, map[string]any{"affected": affected})
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
		resp.OK(w, r, map[string]any{"updated": false})
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
	resp.OK(w, r, map[string]any{"updated": updated})
}

// sessionNames 批量反查会话可读名：优先用账单行冗余的 session_name 快照
//（写入时固化，不随 sessions 投影过期清理丢失），缺失的再回查 sessions 表。
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
