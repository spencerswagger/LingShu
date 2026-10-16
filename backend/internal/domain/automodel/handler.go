package automodel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/team/llmgateway/internal/pkg/resp"
)

// Handler 暴露 Auto 模型 HTTP 处理器（路由在 server 挂载）。
//
//   - 管理端：GET /api/v1/admin/auto-models、GET/PUT/DELETE /api/v1/admin/auto-models/{id}、POST /api/v1/admin/auto-models
//   - 开发端：GET/POST /api/v1/dev/auto-models、PUT/DELETE /api/v1/dev/auto-models/{id}
type Handler struct {
	svc *Service
	// userIDFrom 从 context 解析当前登录用户（经 SetUserIDFrom 注入；
	// 未注入时视为未登录，fail-closed）。
	userIDFrom func(ctx context.Context) (int64, bool)
}

// NewHandler 创建 Auto 模型 HTTP 处理器。
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// SetUserIDFrom 注入当前登录用户解析函数（开发端接口使用；未设置则开发端一律返回未登录）。
func (h *Handler) SetUserIDFrom(fn func(ctx context.Context) (int64, bool)) {
	h.userIDFrom = fn
}

// currentUser 解析当前登录用户；取不到时写出 401 并返回 false。
func (h *Handler) currentUser(w http.ResponseWriter, r *http.Request) (int64, bool) {
	if h.userIDFrom != nil {
		if id, ok := h.userIDFrom(r.Context()); ok {
			return id, true
		}
	}
	resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
	return 0, false
}

// memberView 是 Auto 成员的对外展示结构（PascalCase 契约）。
type memberView struct {
	ID              int64  `json:"ID,string"`
	ExternalModelID int64  `json:"ExternalModelID,string"`
	ExternalName    string `json:"ExternalName"`
	Enabled         bool   `json:"Enabled"`
	Position        int    `json:"Position"`
}

// autoModelView 是 Auto 的对外展示结构（PascalCase 契约）。
type autoModelView struct {
	ID          int64        `json:"ID,string"`
	Name        string       `json:"Name"`
	Scope       string       `json:"Scope"`
	OwnerUserID *int64       `json:"OwnerUserID,string,omitempty"`
	Enabled     bool         `json:"Enabled"`
	Description string       `json:"Description"`
	Members     []memberView `json:"Members"`
	CreatedAt   time.Time    `json:"CreatedAt"`
	UpdatedAt   time.Time    `json:"UpdatedAt"`
}

// autoModelRequest 是创建/更新 Auto 的请求体（PascalCase 契约）。
type autoModelRequest struct {
	Name        string `json:"Name"`
	Scope       string `json:"Scope"`
	OwnerUserID *int64 `json:"OwnerUserID,string"`
	Enabled     *bool  `json:"Enabled"`
	Description string `json:"Description"`
	Members     []struct {
		ExternalModelID int64 `json:"ExternalModelID,string"`
		Position        int   `json:"Position"`
	} `json:"Members"`
}

// toView 把领域 Auto 转为对外展示结构。
func toView(m *AutoModel) autoModelView {
	members := make([]memberView, 0, len(m.Members))
	for _, mem := range m.Members {
		members = append(members, memberView{
			ID:              mem.ID,
			ExternalModelID: mem.ExternalModelID,
			ExternalName:    mem.ExternalName,
			Enabled:         mem.Enabled,
			Position:        mem.Position,
		})
	}
	return autoModelView{
		ID:          m.ID,
		Name:        m.Name,
		Scope:       m.Scope,
		OwnerUserID: m.OwnerUserID,
		Enabled:     m.Enabled,
		Description: m.Description,
		Members:     members,
		CreatedAt:   m.CreatedAt,
		UpdatedAt:   m.UpdatedAt,
	}
}

// toInput 把请求体转为领域可写字段。
func toInput(req *autoModelRequest) AutoModelInput {
	members := make([]MemberInput, 0, len(req.Members))
	for _, m := range req.Members {
		members = append(members, MemberInput{ExternalModelID: m.ExternalModelID, Position: m.Position})
	}
	return AutoModelInput{
		Name:        req.Name,
		Scope:       req.Scope,
		OwnerUserID: req.OwnerUserID,
		Enabled:     req.Enabled,
		Description: req.Description,
		Members:     members,
	}
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return false
	}
	return true
}

// pathID 从 Go 1.22 路由 PathValue 解析数字 ID。
func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "无效的 Auto 模型 ID")
		return 0, false
	}
	return id, true
}

func writeServiceErr(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		resp.Err(w, r, apiErr.HTTPStatus, apiErr.Code, apiErr.Message)
		return
	}
	resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "服务器内部错误")
}

// HandleList GET /api/v1/admin/auto-models
// 可选 query：Scope（global/private，空=全部）、OwnerUserID（按归属用户过滤）。
func (h *Handler) HandleList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var ownerUserID *int64
	if v := q.Get("OwnerUserID"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "OwnerUserID 必须为数字")
			return
		}
		ownerUserID = &id
	}
	list, err := h.svc.List(r.Context(), q.Get("Scope"), ownerUserID)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	views := make([]autoModelView, 0, len(list))
	for i := range list {
		views = append(views, toView(&list[i]))
	}
	resp.OK(w, r, views)
}

// HandleGet GET /api/v1/admin/auto-models/{id}
func (h *Handler) HandleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	m, err := h.svc.Get(r.Context(), id)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, toView(m))
}

// HandleCreate POST /api/v1/admin/auto-models
func (h *Handler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	var req autoModelRequest
	if !decodeBody(w, r, &req) {
		return
	}
	m, err := h.svc.Create(r.Context(), toInput(&req))
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, toView(m))
}

// HandleUpdate PUT /api/v1/admin/auto-models/{id}
func (h *Handler) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req autoModelRequest
	if !decodeBody(w, r, &req) {
		return
	}
	m, err := h.svc.Update(r.Context(), id, toInput(&req))
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, toView(m))
}

// HandleDelete DELETE /api/v1/admin/auto-models/{id}
func (h *Handler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), id); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]bool{"Deleted": true})
}

// HandleDevList GET /api/v1/dev/auto-models 开发端列表（global + 本人 private）。
func (h *Handler) HandleDevList(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	list, err := h.svc.List(r.Context(), "", &userID)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	views := make([]autoModelView, 0, len(list))
	for i := range list {
		views = append(views, toView(&list[i]))
	}
	resp.OK(w, r, views)
}

// HandleDevCreate POST /api/v1/dev/auto-models 开发端创建（强制 private + 当前用户）。
func (h *Handler) HandleDevCreate(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	var req autoModelRequest
	if !decodeBody(w, r, &req) {
		return
	}
	in := toInput(&req)
	// 强制私有作用域与归属用户，忽略请求体传入值。
	in.Scope = ScopePrivate
	uid := userID
	in.OwnerUserID = &uid
	m, err := h.svc.Create(r.Context(), in)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, toView(m))
}

// HandleDevUpdate PUT /api/v1/dev/auto-models/{id} 开发端更新本人 private（含归属校验）。
func (h *Handler) HandleDevUpdate(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	// 归属校验：仅本人可操作。
	m, err := h.svc.Get(r.Context(), id)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	if m.OwnerUserID == nil || *m.OwnerUserID != userID {
		resp.Err(w, r, http.StatusForbidden, resp.CodeForbidden, "无权操作该 Auto 模型")
		return
	}
	var req autoModelRequest
	if !decodeBody(w, r, &req) {
		return
	}
	in := toInput(&req)
	in.Scope = ScopePrivate
	uid := userID
	in.OwnerUserID = &uid
	updated, err := h.svc.Update(r.Context(), id, in)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, toView(updated))
}

// HandleDevDelete DELETE /api/v1/dev/auto-models/{id} 开发端删除本人 private（含归属校验）。
func (h *Handler) HandleDevDelete(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.currentUser(w, r)
	if !ok {
		return
	}
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	m, err := h.svc.Get(r.Context(), id)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	if m.OwnerUserID == nil || *m.OwnerUserID != userID {
		resp.Err(w, r, http.StatusForbidden, resp.CodeForbidden, "无权操作该 Auto 模型")
		return
	}
	if err := h.svc.Delete(r.Context(), id); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]bool{"Deleted": true})
}
