package channelprotocol

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/team/llmgateway/internal/pkg/resp"
)

// Handler 暴露渠道协议管理 HTTP 处理器。
//
//   - GET    /api/v1/admin/channel-protocols
//   - POST   /api/v1/admin/channel-protocols
//   - GET    /api/v1/admin/channel-protocols/{id}
//   - PUT    /api/v1/admin/channel-protocols/{id}
//   - DELETE /api/v1/admin/channel-protocols/{id}
//   - POST   /api/v1/admin/channel-protocols/sync {confirm}
type Handler struct {
	svc *Service
}

// NewHandler 创建渠道协议 HTTP 处理器。
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "无效的渠道协议 ID")
		return 0, false
	}
	return id, true
}

func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		resp.Err(w, r, apiErr.HTTPStatus, apiErr.Code, apiErr.Message)
		return
	}
	resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "服务器内部错误")
}

// HandleList GET /api/v1/admin/channel-protocols
func (h *Handler) HandleList(w http.ResponseWriter, r *http.Request) {
	list, err := h.svc.List(r.Context())
	if err != nil {
		writeErr(w, r, err)
		return
	}
	resp.OK(w, r, list)
}

// HandleGet GET /api/v1/admin/channel-protocols/{id}
func (h *Handler) HandleGet(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	cp, err := h.svc.Get(r.Context(), id)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	resp.OK(w, r, cp)
}

// HandleCreate POST /api/v1/admin/channel-protocols
func (h *Handler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	var in ChannelProtocol
	if !decodeBody(w, r, &in) {
		return
	}
	cp, err := h.svc.Create(r.Context(), in)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	resp.OK(w, r, cp)
}

// HandleUpdate PUT /api/v1/admin/channel-protocols/{id}
func (h *Handler) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var in ChannelProtocol
	if !decodeBody(w, r, &in) {
		return
	}
	cp, err := h.svc.Update(r.Context(), id, in)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	resp.OK(w, r, cp)
}

// HandleDelete DELETE /api/v1/admin/channel-protocols/{id}
func (h *Handler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if err := h.svc.Delete(r.Context(), id); err != nil {
		writeErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]any{"Affected": 1})
}

// HandleSync POST /api/v1/admin/channel-protocols/sync {confirm:bool}
func (h *Handler) HandleSync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	res, err := h.svc.Sync(r.Context(), req.Confirm)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	resp.OK(w, r, res)
}
