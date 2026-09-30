// admin_user.go 管理端用户管理 HTTP 处理器：用户 CRUD 列表/创建/更新/详情/批量删除。
// 详情页（GET /users/{id}）返回用户 + 钱包余额 + 最近流水摘要。
package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/team/llmgateway/internal/pkg/resp"
)

// GetUser 按主键查询单个用户（脱敏口令哈希），附带钱包余额与累计消费额。
func (s *Service) GetUser(ctx context.Context, id int64) (*User, error) {
	u, err := s.store.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound()
		}
		return nil, err
	}
	u.PasswordHash = ""
	if err := s.enrichLedger(ctx, []int64{id}, func(_ int64, e Ledger) {
		u.Balance = e.Balance
		u.TotalSpent = e.TotalSpent
	}); err != nil {
		return nil, fmt.Errorf("load ledger: %w", err)
	}
	return u, nil
}

// AdminUserHandler 暴露管理端用户管理 HTTP 处理器。
type AdminUserHandler struct {
	svc        *Service
	credit     *CreditService
	userIDFrom func(ctx context.Context) (int64, bool)
}

// NewAdminUserHandler 创建管理端用户处理器。
func NewAdminUserHandler(svc *Service, credit *CreditService, userIDFrom func(ctx context.Context) (int64, bool)) *AdminUserHandler {
	return &AdminUserHandler{svc: svc, credit: credit, userIDFrom: userIDFrom}
}

// HandleListUsers GET /api/v1/admin/users?q=&role=&status=&page=&size=
func (h *AdminUserHandler) HandleListUsers(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, size := parsePageSize(q)
	users, total, err := h.svc.ListUsers(r.Context(), q.Get("q"), q.Get("role"), q.Get("status"), page, size)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	respOK(w, r, map[string]any{"list": users, "total": total, "page": page, "size": size})
}

// HandleCreateUser POST /api/v1/admin/users
func (h *AdminUserHandler) HandleCreateUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Username    string `json:"username"`
		Password    string `json:"password"`
		Nickname    string `json:"nickname"`
		Role        string `json:"role"`
		PricingMode string `json:"pricing_mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	u, err := h.svc.AdminCreateUser(r.Context(), req.Username, req.Password, req.Role, req.PricingMode, req.Nickname)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	respOK(w, r, u)
}

// HandleUpdateUser PUT /api/v1/admin/users/{id}
func (h *AdminUserHandler) HandleUpdateUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ID 参数无效")
		return
	}
	var req struct {
		Role        string `json:"role"`
		Status      string `json:"status"`
		PricingMode string `json:"pricing_mode"`
		Nickname    string `json:"nickname"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	u, err := h.svc.UpdateUser(r.Context(), id, req.Role, req.Status, req.PricingMode, req.Nickname)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	respOK(w, r, u)
}

// HandleResetPassword POST /api/v1/admin/users/{id}/reset-password
func (h *AdminUserHandler) HandleResetPassword(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ID 参数无效")
		return
	}
	var req struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	if err := h.svc.AdminResetPassword(r.Context(), id, req.Password); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	respOK(w, r, map[string]bool{"reset": true})
}

// HandleBatchDeleteUsers POST /api/v1/admin/users/batch-delete
func (h *AdminUserHandler) HandleBatchDeleteUsers(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []int64 `json:"ids"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	n, err := h.svc.BatchDeleteUsers(r.Context(), req.IDs)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	respOK(w, r, map[string]int64{"affected": n})
}

// HandleGetUser GET /api/v1/admin/users/{id} 返回用户 + 钱包余额 + 最近流水摘要。
func (h *AdminUserHandler) HandleGetUser(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ID 参数无效")
		return
	}
	u, err := h.svc.GetUser(r.Context(), id)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	balance, err := h.credit.GetWallet(r.Context(), id)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	flows, _, err := h.credit.ListFlows(r.Context(), id, 1, 10)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	respOK(w, r, map[string]any{
		"user":         u,
		"wallet":       map[string]any{"balance": balance},
		"recent_flows": flows,
	})
}

func respOK(w http.ResponseWriter, r *http.Request, data any) {
	resp.OK(w, r, data)
}
