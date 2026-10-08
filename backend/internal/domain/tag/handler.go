package tag

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/team/llmgateway/internal/pkg/idgen"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// Handler 暴露标签管理 HTTP 处理器（路由由 Task 8 挂载）。
//
//   - GET     /api/v1/admin/tags
//   - POST    /api/v1/admin/tags
//   - PUT     /api/v1/admin/tags/{id}
//   - POST    /api/v1/admin/tags/batch-delete {ids:[]}
type Handler struct {
	svc *Service
}

// NewHandler 创建标签 HTTP 处理器。
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

type createTagRequest struct {
	Name        string            `json:"Name"`
	Description string            `json:"Description"`
	KVPairs     map[string]string `json:"KVPairs"`
	Enabled     *bool             `json:"Enabled"`
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := parseID(r.PathValue("id"))
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "无效的标签 ID")
		return 0, false
	}
	return id, true
}

// HandleList GET /api/v1/admin/tags（可选 ?Enabled=true|false）
func (h *Handler) HandleList(w http.ResponseWriter, r *http.Request) {
	var enabled *bool
	if v := r.URL.Query().Get("Enabled"); v != "" {
		b := v == "true"
		enabled = &b
	}
	tags, err := h.svc.ListTags(r.Context(), enabled)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, tags)
}

// HandleCreate POST /api/v1/admin/tags
func (h *Handler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	var req createTagRequest
	if !decodeBody(w, r, &req) {
		return
	}
	// creatorID 需从鉴权上下文取（当前占位 0，admin handler 后续注入）。
	t, err := h.svc.CreateTag(r.Context(), toInput(&req), 0)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, t)
}

// HandleUpdate PUT /api/v1/admin/tags/{id}
func (h *Handler) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req createTagRequest
	if !decodeBody(w, r, &req) {
		return
	}
	t, err := h.svc.UpdateTag(r.Context(), id, toInput(&req))
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, t)
}

type batchDeleteRequest struct {
	IDs idgen.IDs `json:"IDs"`
}

// HandleBatchDelete POST /api/v1/admin/tags/batch-delete
func (h *Handler) HandleBatchDelete(w http.ResponseWriter, r *http.Request) {
	var req batchDeleteRequest
	if !decodeBody(w, r, &req) {
		return
	}
	n, err := h.svc.BatchDeleteTags(r.Context(), []int64(req.IDs))
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]int64{"Deleted": n})
}

func toInput(req *createTagRequest) TagInput {
	return TagInput{
		Name:        req.Name,
		Description: req.Description,
		KVPairs:     req.KVPairs,
		Enabled:     req.Enabled,
	}
}

func parseID(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}

func writeServiceErr(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		resp.Err(w, r, apiErr.HTTPStatus, apiErr.Code, apiErr.Message)
		return
	}
	resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "服务器内部错误")
}
