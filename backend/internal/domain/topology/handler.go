package topology

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/pkg/resp"
)

// Handler 是拓扑的两个管理端接口：
//
//	T1 GET  /api/v1/admin/topology       四层聚合 + 同步状态（页面首屏）
//	T3 POST /api/v1/admin/topology/sync  手动触发一轮同步（与后台定时轮同实现）
//
// 字段名与 View 的 `src/api/v2.ts` 的 InternalModelView / TopologySync 逐字对齐（PascalCase）。
type Handler struct {
	store  Store
	syncer *Syncer
	raw    RawSource // 可选：直接读网关 /api/groups 原样返回
}

// RawSource 是最小接口：拿网关 /api/groups 的原始 JSON。
type RawSource interface {
	FetchRawGroups(ctx context.Context) ([]byte, error)
}

// SetRawSource 注入原始组数据源（不起作用时页面只显示镜像层，不报错）。
func (h *Handler) SetRawSource(src RawSource) { h.raw = src }

// HandleGateway 原样下发网关的 /api/groups（账号、路由组、成员、模型都在这份里）。
func (h *Handler) HandleGateway(w http.ResponseWriter, r *http.Request) {
	if h.raw == nil {
		resp.Err(w, r, http.StatusServiceUnavailable, 50301, "拓扑原样视图不可用")
		return
	}
	body, err := h.raw.FetchRawGroups(r.Context())
	if err != nil {
		resp.Err(w, r, http.StatusBadGateway, 50201, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"Data":` + string(body) + `}`))
}

// NewHandler 创建拓扑处理器。
func NewHandler(store Store, syncer *Syncer) *Handler {
	return &Handler{store: store, syncer: syncer}
}

// nodeView 是 T1 的一个内部模型节点。
type nodeView struct {
	Ref         string          `json:"Ref"`
	ProviderRef string          `json:"ProviderRef"`
	Kind        string          `json:"Kind"`
	DisplayName string          `json:"DisplayName"`
	ContextLen  int64           `json:"ContextLen"`
	PosX        float64         `json:"PosX"`
	PosY        float64         `json:"PosY"`
	LastSeenAt  string          `json:"LastSeenAt"`
	Stale       bool            `json:"Stale"`
	Caps        json.RawMessage `json:"Caps,omitempty"`
	ListPrice   json.RawMessage `json:"ListPrice,omitempty"`
}

// linkView 是 T1 的一条画布落位（只影响观感，不影响售卖与计费）。
type linkView struct {
	InternalRef     string  `json:"InternalRef"`
	ExternalModelID string  `json:"ExternalModelID"` // 雪花以字符串下发（超出 JS 安全整数）
	PosX            float64 `json:"PosX"`
	PosY            float64 `json:"PosY"`
}

// syncView 是状态条（与 View 的 TopologySync 对齐）。State 取 ok/failed/never。
type syncView struct {
	LastSuccessAt string `json:"LastSuccessAt"`
	LastRunAt     string `json:"LastRunAt"`
	State         string `json:"State"`
	Message       string `json:"Message"`
	Created       int    `json:"Created"`
	Stale         int    `json:"Stale"`
}

// providerView 是四层图第 1 层（供应商）。
type providerView struct {
	Provider string `json:"Provider"`
	Models   int    `json:"Models"`
	Stale    int    `json:"Stale"`
	Accounts int    `json:"Accounts"`
}

// accountView 是四层图第 2 层（上游账号）。
type accountView struct {
	Provider   string `json:"Provider"`
	Account    string `json:"Account"`
	Subscribed bool   `json:"Subscribed"`
	LastSeenAt string `json:"LastSeenAt"`
}

// externalView 是四层图第 4 层（对外模型）+ 绑定边。
//
// BoundInternalRef 用**不带 omitempty** 的字段下发：null/空串都表示未绑定，
// 前端靠「字段在不在」判断会漏掉「绑定功能未上线」这条事实。
type externalView struct {
	ID               string `json:"ID"` // 雪花以字符串下发（超出 JS 安全整数）
	ExternalName     string `json:"ExternalName"`
	Enabled          bool   `json:"Enabled"`
	BoundInternalRef string `json:"BoundInternalRef"`
	InternalModelHit bool   `json:"InternalModelHit"` // 绑定目标是否仍在镜像里（false = 绑定悬空）
}

// view 是 T1 的响应体：四层（供应商 → 账号 → 内部模型 → 对外模型）+ 同步状态。
//
// 四层必须一起返回：只给 Items 时页面画不出「账号层」与「绑定边」，
// 四层图会退化成供应商卡片 + 内部模型散点（View 的 Topology.vue 也按这四段取值）。
type view struct {
	Providers []providerView `json:"Providers"`
	Accounts  []accountView  `json:"Accounts"`
	Items     []nodeView     `json:"Items"`
	Externals []externalView `json:"Externals"`
	Links     []linkView     `json:"Links"`

	StaleCount      int      `json:"StaleCount"`
	UnboundExternal int      `json:"UnboundExternal"`
	SyncedAt        string   `json:"SyncedAt"`
	Sync            syncView `json:"Sync"`
}

// linksInput 是 T2 的入参：一批画布落位（坐标只影响观感，不影响售卖与计费）。
type linksInput struct {
	Items []struct {
		InternalRef     string  `json:"InternalRef"`
		ExternalModelID string  `json:"ExternalModelID"` // 字符串：雪花超出 JS 安全整数
		PosX            float64 `json:"PosX"`
		PosY            float64 `json:"PosY"`
	} `json:"Items"`
}

// linksResult 是 T2 的响应体。
type linksResult struct {
	Saved int `json:"Saved"`
}

// syncResult 是 T3 的响应体。
//
// 失败时**仍返回 200**（业务结果而非客户端错误）：页面要保留旧快照并显示原因
// （Spec P02 验收 3），所以 Message/State 就是这份契约的一部分。
type syncResult struct {
	Fetched int    `json:"Fetched"`
	Created int    `json:"Created"`
	Stale   int    `json:"Stale"`
	State   string `json:"State"`
	Message string `json:"Message"`
}

// HandleGet 返回四层聚合视图。
func (h *Handler) HandleGet(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.store == nil {
		resp.Err(w, r, http.StatusServiceUnavailable, resp.CodeInternalError, "拓扑服务未装配")
		return
	}
	ctx := r.Context()
	nodes, err := h.store.Nodes(ctx)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, err.Error())
		return
	}
	links, err := h.store.Links(ctx)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, err.Error())
		return
	}
	providers, err := h.store.Providers(ctx)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, err.Error())
		return
	}
	accounts, err := h.store.Accounts(ctx)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, err.Error())
		return
	}
	externals, err := h.store.Externals(ctx)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, err.Error())
		return
	}
	counts, err := h.store.Counts(ctx)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, err.Error())
		return
	}
	synced, hasSynced, err := h.store.LastSeenAt(ctx)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, err.Error())
		return
	}
	st, err := h.store.LoadState(ctx)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, err.Error())
		return
	}

	out := view{
		Providers:       make([]providerView, 0, len(providers)),
		Accounts:        make([]accountView, 0, len(accounts)),
		Items:           make([]nodeView, 0, len(nodes)),
		Externals:       make([]externalView, 0, len(externals)),
		Links:           make([]linkView, 0, len(links)),
		StaleCount:      counts.Stale,
		UnboundExternal: counts.UnboundExternal,
		Sync:            syncViewOf(st),
	}

	// 供应商层的账号计数从账号层回填：一条 SQL 只算一件事，避免交叉聚合写错 join。
	accountCount := make(map[string]int, len(providers))
	for _, a := range accounts {
		accountCount[a.Provider]++
	}
	for _, p := range providers {
		out.Providers = append(out.Providers, providerView{
			Provider: p.Provider, Models: p.Models, Stale: p.Stale, Accounts: accountCount[p.Provider],
		})
	}
	for _, a := range accounts {
		out.Accounts = append(out.Accounts, accountView{
			Provider: a.Provider, Account: a.Account, Subscribed: a.Subscribed,
			LastSeenAt: fmtTime(a.LastSeenAt),
		})
	}
	// 绑定目标是否仍在镜像里：悬空绑定（目标已从网关下线/改名）必须在页面上看得出来，
	// 否则运营会以为「绑定还在、只是没画线」。
	nodeRefs := make(map[string]struct{}, len(nodes))
	for _, n := range nodes {
		nodeRefs[n.Ref] = struct{}{}
	}
	for _, e := range externals {
		hit := false
		if e.BoundInternalRef != "" {
			_, hit = nodeRefs[e.BoundInternalRef]
		}
		out.Externals = append(out.Externals, externalView{
			ID:               strconv.FormatInt(e.ID, 10),
			ExternalName:     e.ExternalName,
			Enabled:          e.Enabled,
			BoundInternalRef: e.BoundInternalRef,
			InternalModelHit: hit,
		})
	}
	if hasSynced {
		out.SyncedAt = fmtTime(synced)
	}
	for _, n := range nodes {
		out.Items = append(out.Items, nodeView{
			Ref:         n.Ref,
			ProviderRef: n.ProviderRef,
			Kind:        n.Kind,
			DisplayName: n.DisplayName,
			ContextLen:  n.ContextLen,
			PosX:        n.PosX,
			PosY:        n.PosY,
			LastSeenAt:  fmtTime(n.LastSeenAt),
			Stale:       n.Stale,
			Caps:        rawOrEmpty(n.Caps),
			ListPrice:   rawOrEmpty(n.ListPrice),
		})
	}
	for _, l := range links {
		out.Links = append(out.Links, linkView{
			InternalRef:     l.InternalRef,
			ExternalModelID: strconv.FormatInt(l.ExternalModelID, 10),
			PosX:            l.PosX,
			PosY:            l.PosY,
		})
	}
	resp.OK(w, r, out)
}

// HandleSync 手动触发一轮同步。
func (h *Handler) HandleSync(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.syncer == nil {
		resp.Err(w, r, http.StatusServiceUnavailable, resp.CodeInternalError, "拓扑同步未装配")
		return
	}
	res, err := h.syncer.RunOnce(r.Context())
	switch {
	case errors.Is(err, ErrSyncInFlight):
		resp.ErrReason(w, r, http.StatusConflict, resp.CodeConflict, "sync_in_flight",
			"已有一轮同步在执行，请稍候刷新查看结果", nil)
		return
	case errors.Is(err, ErrSourceNotConfigured):
		// 503 而不是 200：这不是「同步过但失败」，是「没配">网关地址」——需要运维动作，
		// 页面出错误气泡并点名原因，比给一个「同步完成 0 条」诚实。
		resp.ErrReason(w, r, http.StatusServiceUnavailable, resp.CodeInternalError,
			"topology_source_not_configured",
			"未配置网关拓扑源（magpie.base_url / 拓扑源环境变量），无法同步", nil)
		return
	case err != nil:
		// 已跑过但失败：200 + State/Message，页面据此保留旧快照并显示原因（Spec P02 验收 3）。
		resp.OK(w, r, syncResult{Fetched: res.Fetched, Created: res.Created, Stale: res.Stale,
			State: StateFailed, Message: res.Message})
		return
	}
	resp.OK(w, r, syncResult{Fetched: res.Fetched, Created: res.Created, Stale: res.Stale,
		State: StateOK, Message: ""})
}

// links输入上限：画布是人工（或前端自动布局）落位，几百个节点足够；
// 不设上限等于给一个「一次写十万行」的免费放大器。
const maxLinkItems = 500

// HandlePutLinks 保存画布落位（T2 PUT /api/v1/admin/topology/links）。
//
// 只写坐标，**不写绑定**：绑定真源是 external_models.bound_internal_ref（T5 的事）。
// 把绑定也塞进这张表会造出两个可写源，谁后写谁生效 —— 拓扑与售卖必然漂移。
func (h *Handler) HandlePutLinks(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.store == nil {
		resp.Err(w, r, http.StatusServiceUnavailable, resp.CodeInternalError, "拓扑服务未装配")
		return
	}
	var in linksInput
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体不是合法 JSON："+err.Error())
		return
	}
	if len(in.Items) == 0 {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "Items 不能为空（至少一条画布落位）")
		return
	}
	if len(in.Items) > maxLinkItems {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest,
			"一次最多保存 500 条画布落位，请分批提交")
		return
	}
	links := make([]Link, 0, len(in.Items))
	for i, it := range in.Items {
		ref := strings.TrimSpace(it.InternalRef)
		if ref == "" {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest,
				"第 "+strconv.Itoa(i+1)+" 条缺少 InternalRef")
			return
		}
		id, err := strconv.ParseInt(strings.TrimSpace(it.ExternalModelID), 10, 64)
		if err != nil || id <= 0 {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest,
				"第 "+strconv.Itoa(i+1)+" 条的 ExternalModelID 不是合法的模型 id")
			return
		}
		links = append(links, Link{InternalRef: ref, ExternalModelID: id, PosX: it.PosX, PosY: it.PosY})
	}
	saved, err := h.store.SaveLinks(r.Context(), links)
	if errors.Is(err, ErrLinkTargetMissing) {
		// 400 而不是 500：运营填错 ref 是输入问题，且报文里点名了是哪一条。
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, err.Error())
		return
	}
	resp.OK(w, r, linksResult{Saved: saved})
}

// syncViewOf 把落库状态映射成视图；零值 = 从未同步（State=never，不是 failed）。
func syncViewOf(st SyncState) syncView {
	out := syncView{
		LastSuccessAt: fmtTime(st.LastSuccessAt),
		LastRunAt:     fmtTime(st.LastRunAt),
		State:         st.State,
		Message:       st.Message,
		Created:       st.Created,
		Stale:         st.Stale,
	}
	if out.State == "" {
		out.State = StateNever
	}
	return out
}

// fmtTime 零值返回空串：前端把空串当「从未」渲染，避免出现 0001-01-01。
func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
