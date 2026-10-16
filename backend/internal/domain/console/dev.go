// dev.go 开发端控制台查询：账单列表/详情（三步拆解）、本人会话列表与调用记录。
package console

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/pkg/decimalx"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// Dev 承载开发端控制台处理器。userID 从鉴权上下文取（userIDFrom 注入）。
type Dev struct {
	db           *sql.DB // DB 句柄（经 SetDB 注入；nil=未装配）
	billing      *billing.SqlStore
	userIDFrom   func(ctx context.Context) (int64, bool)
	callLogs     callLogReader           // 调用日志只读查询（经 SetCallLogStore 注入；nil=未装配）
	sessions     *router.SessionRegistry // 会话注册表（经 SetSessions 注入；nil=未装配）
	now          func() time.Time        // 时钟（测试可替换）
	tokenCounter TokenCounter            // 令牌计数（经 SetTokenCounter 注入；nil=未装配）
}

// NewDev 创建开发端控制台处理器。
func NewDev(billingStore *billing.SqlStore, userIDFrom func(ctx context.Context) (int64, bool)) *Dev {
	return &Dev{billing: billingStore, userIDFrom: userIDFrom, now: time.Now}
}

// SetCallLogStore 注入调用日志只读查询（nil=未装配：BillingDetail.CallLog 为 nil）。
func (h *Dev) SetCallLogStore(s callLogReader) { h.callLogs = s }

// SetDB 注入数据库句柄（nil=未装配：依赖 DB 的查询返回空）。
func (h *Dev) SetDB(db *sql.DB) { h.db = db }

// attachCallLog 查 call_logs 并填充账单详情（未装配/无记录时为 nil）。
func (h *Dev) attachCallLog(ctx context.Context, bid string) *CallLogView {
	if h.callLogs == nil || bid == "" {
		return nil
	}
	cl, err := h.callLogs.GetByBillingID(ctx, bid)
	if err != nil || cl == nil {
		return nil
	}
	return toCallLogView(cl)
}

func (h *Dev) currentUser(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return 0, false
	}
	return id, true
}

// HandleListBillings GET /api/v1/dev/billings?page=&size=&from=&to= 查询本人账单。
func (h *Dev) HandleListBillings(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	f := billing.RecordFilter{
		UserID: &userID,
		Page:   parsePage(q.Get("Page")),
		Size:   parseSize(q.Get("Size")),
	}
	if v := q.Get("From"); v != "" {
		t, err := parseRFC3339Lenient(v)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "from 时间格式应为 RFC3339")
			return
		}
		f.From = &t
	}
	if v := q.Get("To"); v != "" {
		t, err := parseRFC3339Lenient(v)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "to 时间格式应为 RFC3339")
			return
		}
		f.To = &t
	}
	records, total, err := h.billing.ListRecords(r.Context(), f)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询账单失败")
		return
	}
	list := make([]billingListItem, 0, len(records))
	for i := range records {
		list = append(list, toListItem(&records[i]))
	}
	resp.OK(w, r, map[string]any{"List": list, "Total": total, "Page": f.Page, "Size": f.Size})
}

// HandleGetBilling GET /api/v1/dev/billings/{billing_id} 本人账单详情（不含内部模型/渠道名）。
func (h *Dev) HandleGetBilling(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
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
	// 开发端仅能查看本人账单。
	if rec.UserID != userID {
		resp.Err(w, r, http.StatusNotFound, resp.CodeNotFound, "账单不存在")
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
		CallLog:         h.attachCallLog(r.Context(), rec.BillingID),
	}
	resp.OK(w, r, detail)
}

// HandleListSessions GET /api/v1/dev/sessions 本人会话列表（只读）。
// 强制 user_id = 当前登录用户：客户端传入的 UserID 一律忽略，杜绝越权查看他人会话。
// 过滤：TokenID / ChannelKeyID / Q（session_id 前缀）/ Expired=active|expired / Closed=1|0；
// 分页：Page（≥1，缺省 1）、Size（≥1，缺省 20）；排序：last_active 倒序，同值按 session_id 升序。
func (h *Dev) HandleListSessions(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	var tokenID, keyID int64
	var err error
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
	closedMode := q.Get("Closed")
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

	// 进行中请求数：仅统计本人的会话，避免越权泄露他人运行态。
	inflight := map[string]int{}
	if h.sessions != nil {
		for _, s := range h.sessions.ListAll() {
			if s.UserID == userID && s.Inflight > 0 {
				inflight[s.SessionID] = s.Inflight
			}
		}
	}

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

// HandleListSessionCalls GET /api/v1/dev/sessions/{session_id}/calls 本人会话的完整调用记录（只读）。
// 先校验会话归属：不存在与不属于本人返回同一个 404，既防越权读取他人调用内容，也不泄露会话是否存在。
func (h *Dev) HandleListSessionCalls(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	sid := r.PathValue("session_id")
	if sid == "" {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "缺少会话 ID")
		return
	}
	if h.db == nil {
		resp.OK(w, r, map[string]any{"List": []CallLogView{}, "Total": 0})
		return
	}
	var owner int64
	if err := h.db.QueryRowContext(r.Context(),
		`SELECT user_id FROM sessions WHERE session_id = $1`, sid).Scan(&owner); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			resp.Err(w, r, http.StatusNotFound, resp.CodeNotFound, "会话不存在")
			return
		}
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询会话失败")
		return
	}
	if owner != userID {
		resp.Err(w, r, http.StatusNotFound, resp.CodeNotFound, "会话不存在")
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

// HandleBillingsStats GET /api/v1/dev/billings/stats 本人账单聚合统计（强制本人维度）。
func (h *Dev) HandleBillingsStats(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	f, err := buildBillingsFilter(r.URL.Query())
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	// 强制覆盖为当前用户：客户端传入的 UserID 无效，杜绝越权统计他人账单。
	f.UserID = &userID
	summary, err := h.billing.Summarize(r.Context(), f)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "统计账单失败")
		return
	}
	resp.OK(w, r, summary)
}
