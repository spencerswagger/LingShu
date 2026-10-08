package sync

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/team/llmgateway/internal/pkg/idgen"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// APIError 是带 HTTP 状态码与业务码的领域错误。
type APIError struct {
	HTTPStatus int
	Code       int
	Message    string
}

func (e *APIError) Error() string { return e.Message }

func errBadRequest(msg string) *APIError {
	return &APIError{HTTPStatus: http.StatusBadRequest, Code: resp.CodeBadRequest, Message: msg}
}
func errNotFound(msg string) *APIError {
	return &APIError{HTTPStatus: http.StatusNotFound, Code: resp.CodeNotFound, Message: msg}
}

// Handler 暴露 watchlist 与价格同步 HTTP 处理器（admin 路由，Task 8 挂载）。
//
//   - GET  /api/v1/admin/watchlist
//   - POST /api/v1/admin/watchlist                     {external_model_id, local_model_name, alert_on_change}
//   - PUT  /api/v1/admin/watchlist                     {external_model_id, local_model_name, alert_on_change}
//   - POST /api/v1/admin/watchlist/batch-delete        {ids:[]}
//   - POST /api/v1/admin/sync/run
//   - GET  /api/v1/admin/sync/alerts                   ?Status=
//   - POST /api/v1/admin/sync/alerts/{id}/resolve
type Handler struct {
	svc        *Syncer
	userIDFrom func(ctx context.Context) (int64, bool)
}

// NewHandler 创建同步处理器。
func NewHandler(svc *Syncer, userIDFrom func(ctx context.Context) (int64, bool)) *Handler {
	return &Handler{svc: svc, userIDFrom: userIDFrom}
}

type watchlistInput struct {
	ExternalModelID string `json:"ExternalModelID"`
	LocalModelName  string `json:"LocalModelName"`
	AlertOnChange   *bool  `json:"AlertOnChange"`
}

type watchlistResponse struct {
	ID              int64   `json:"ID,string"`
	ExternalModelID string  `json:"ExternalModelID"`
	LocalModelName  string  `json:"LocalModelName"`
	AlertOnChange   bool    `json:"AlertOnChange"`
	LastSyncedAt    *string `json:"LastSyncedAt,omitempty"`
	CreatedAt       string  `json:"CreatedAt"`
}

func toResponse(it *WatchlistItem) watchlistResponse {
	r := watchlistResponse{
		ID:              it.ID,
		ExternalModelID: it.ExternalModelID,
		LocalModelName:  it.LocalModelName,
		AlertOnChange:   it.AlertOnChange,
		CreatedAt:       it.CreatedAt.Format("2006-01-02T15:04:05Z07:00"),
	}
	if it.LastSyncedAt != nil {
		v := it.LastSyncedAt.Format("2006-01-02T15:04:05Z07:00")
		r.LastSyncedAt = &v
	}
	return r
}

// HandleListWatchlist GET /api/v1/admin/watchlist
func (h *Handler) HandleListWatchlist(w http.ResponseWriter, r *http.Request) {
	items, err := h.svc.ListWatchlist(r.Context())
	if err != nil {
		writeErr(w, r, err)
		return
	}
	list := make([]watchlistResponse, 0, len(items))
	for i := range items {
		list = append(list, toResponse(&items[i]))
	}
	resp.OK(w, r, map[string]any{"List": list})
}

// HandleUpsertWatchlist POST/PUT /api/v1/admin/watchlist（按 external_model_id 幂等）
func (h *Handler) HandleUpsertWatchlist(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeWatchlistBody(w, r)
	if !ok {
		return
	}
	alert := true
	if req.AlertOnChange != nil {
		alert = *req.AlertOnChange
	}
	it, err := h.svc.UpsertWatchlist(r.Context(), &WatchlistItem{
		ExternalModelID: req.ExternalModelID,
		LocalModelName:  req.LocalModelName,
		AlertOnChange:   alert,
	})
	if err != nil {
		writeErr(w, r, err)
		return
	}
	resp.OK(w, r, toResponse(it))
}

type batchDeleteRequest struct {
	IDs idgen.IDs `json:"IDs"`
}

// HandleBatchDeleteWatchlist POST /api/v1/admin/watchlist/batch-delete
func (h *Handler) HandleBatchDeleteWatchlist(w http.ResponseWriter, r *http.Request) {
	var req batchDeleteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	n, err := h.svc.DeleteWatchlist(r.Context(), []int64(req.IDs))
	if err != nil {
		writeErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]int64{"Deleted": n})
}

// HandleRunSync POST /api/v1/admin/sync/run 立即执行一次同步。
func (h *Handler) HandleRunSync(w http.ResponseWriter, r *http.Request) {
	summary, err := h.svc.SyncOnce(r.Context())
	if err != nil {
		writeErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]string{"Summary": summary})
}

type alertResponse struct {
	ID              int64             `json:"ID,string"`
	ExternalModelID string            `json:"ExternalModelID"`
	LocalModelName  string            `json:"LocalModelName"`
	Changes         map[string]Change `json:"Changes"`
	Status          string            `json:"Status"`
	DetectedAt      string            `json:"DetectedAt"`
	ResolvedAt      *string           `json:"ResolvedAt,omitempty"`
}

// HandleListAlerts GET /api/v1/admin/sync/alerts?Status=
func (h *Handler) HandleListAlerts(w http.ResponseWriter, r *http.Request) {
	status := r.URL.Query().Get("Status")
	if status != "" && status != AlertStatusPending && status != AlertStatusResolved && status != AlertStatusIgnored {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "status 只允许 pending/resolved/ignored")
		return
	}
	alerts, err := h.svc.ListAlerts(r.Context(), status)
	if err != nil {
		writeErr(w, r, err)
		return
	}
	list := make([]alertResponse, 0, len(alerts))
	for i := range alerts {
		a := alerts[i]
		item := alertResponse{
			ID:              a.ID,
			ExternalModelID: a.ExternalModelID,
			LocalModelName:  a.LocalModelName,
			Changes:         a.Changes,
			Status:          a.Status,
			DetectedAt:      a.DetectedAt.Format("2006-01-02T15:04:05Z07:00"),
		}
		if a.ResolvedAt != nil {
			v := a.ResolvedAt.Format("2006-01-02T15:04:05Z07:00")
			item.ResolvedAt = &v
		}
		list = append(list, item)
	}
	resp.OK(w, r, map[string]any{"List": list})
}

// HandleResolveAlert POST /api/v1/admin/sync/alerts/{id}/resolve
func (h *Handler) HandleResolveAlert(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "无效的告警 ID")
		return
	}
	operatorID, _ := h.userIDFrom(r.Context())
	if err := h.svc.ResolveAlert(r.Context(), id, operatorID); err != nil {
		if IsNoRows(err) {
			resp.Err(w, r, http.StatusNotFound, resp.CodeNotFound, "告警不存在")
			return
		}
		writeErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]any{"Affected": 1})
}

func decodeWatchlistBody(w http.ResponseWriter, r *http.Request) (watchlistInput, bool) {
	var req watchlistInput
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return req, false
	}
	if req.ExternalModelID == "" {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "external_model_id 不能为空")
		return req, false
	}
	if req.LocalModelName == "" {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "local_model_name 不能为空")
		return req, false
	}
	return req, true
}

// writeErr 将 sync 领域错误映射为统一响应。
func writeErr(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		resp.Err(w, r, apiErr.HTTPStatus, apiErr.Code, apiErr.Message)
		return
	}
	resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "同步服务内部错误")
}
