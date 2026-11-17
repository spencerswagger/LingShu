package topology

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
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

// view 是 T1 的响应体。
type view struct {
	Items           []nodeView `json:"Items"`
	Links           []linkView `json:"Links"`
	StaleCount      int        `json:"StaleCount"`
	UnboundExternal int        `json:"UnboundExternal"`
	SyncedAt        string     `json:"SyncedAt"`
	Sync            syncView   `json:"Sync"`
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
		Items:           make([]nodeView, 0, len(nodes)),
		Links:           make([]linkView, 0, len(links)),
		StaleCount:      counts.Stale,
		UnboundExternal: counts.UnboundExternal,
		Sync:            syncViewOf(st),
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
