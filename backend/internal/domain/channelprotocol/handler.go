package channelprotocol

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

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
//   - POST   /api/v1/admin/channel-protocols/{id}/models/pull {ApiKey}
//   - GET    /api/v1/admin/channel-protocols/export
//   - POST   /api/v1/admin/channel-protocols/import {Pack}
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

// HandlePullModels POST /api/v1/admin/channel-protocols/{id}/models/pull {ApiKey}
// 用临时 API Key 拉取该协议 openai-chat 上游 /models 列表（不落库）。
func (h *Handler) HandlePullModels(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req struct {
		ApiKey string `json:"ApiKey"`
	}
	if !decodeBody(w, r, &req) {
		return
	}
	list, err := h.svc.PullModels(r.Context(), id, req.ApiKey)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]any{"List": list})
}

// HandleExport GET /api/v1/admin/channel-protocols/export
// 导出全部渠道协议为预设包形状 JSON。
func (h *Handler) HandleExport(w http.ResponseWriter, r *http.Request) {
	pack, err := h.svc.Export(r.Context())
	if err != nil {
		writeErr(w, r, err)
		return
	}
	resp.OK(w, r, pack)
}

// HandleImport POST /api/v1/admin/channel-protocols/import {Pack}
// 从预设包形状 JSON 导入渠道协议，返回新增/跳过计数与告警。
func (h *Handler) HandleImport(w http.ResponseWriter, r *http.Request) {
	var pack Pack
	if !decodeBody(w, r, &pack) {
		return
	}
	result, err := h.svc.Import(r.Context(), pack)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	resp.OK(w, r, result)
}

// HandleErrorStats GET /api/v1/admin/channel-protocols/{id}/error-stats?from=&to=
// 返回该渠道协议在 [from, to]（YYYY-MM-DD）内按 category 聚合的错误计数；
// from/to 缺省为「近 30 天（含今日）」。
func (h *Handler) HandleErrorStats(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	from, to, ok := parseDayRange(w, r)
	if !ok {
		return
	}
	stats, err := h.svc.ErrorStats(r.Context(), id, from, to)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]any{
		"ID":    strconv.FormatInt(id, 10),
		"From":  from.Format(dayLayout),
		"To":    to.Format(dayLayout),
		"Stats": stats,
	})
}

// parseDayRange 解析 from/to 查询参数（YYYY-MM-DD）；缺省为「近 30 天（含今日）」。
// 解析失败写出 400 并返回 false。
func parseDayRange(w http.ResponseWriter, r *http.Request) (time.Time, time.Time, bool) {
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	from := today.AddDate(0, 0, -29)
	to := today
	q := r.URL.Query()
	if s := strings.TrimSpace(q.Get("from")); s != "" {
		t, err := time.Parse(dayLayout, s)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "from 需为 YYYY-MM-DD")
			return time.Time{}, time.Time{}, false
		}
		from = t
	}
	if s := strings.TrimSpace(q.Get("to")); s != "" {
		t, err := time.Parse(dayLayout, s)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "to 需为 YYYY-MM-DD")
			return time.Time{}, time.Time{}, false
		}
		to = t
	}
	return from, to, true
}
