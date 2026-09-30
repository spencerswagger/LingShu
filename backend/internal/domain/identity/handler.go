package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/team/llmgateway/internal/pkg/resp"
)

// Handler 暴露身份认证相关的 HTTP 处理器。
type Handler struct {
	svc        *Service
	credit     *CreditService
	userIDFrom func(ctx context.Context) (int64, bool)
}

// NewHandler 创建身份认证 HTTP 处理器。
func NewHandler(svc *Service, credit *CreditService, userIDFrom func(ctx context.Context) (int64, bool)) *Handler {
	return &Handler{svc: svc, credit: credit, userIDFrom: userIDFrom}
}

type loginRequest struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type loginResponse struct {
	Token string `json:"token"`
	User  *User  `json:"user"`
}

// HandleLogin POST /api/v1/auth/login 处理登录：校验口令并签发 JWT。
func (h *Handler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	token, user, err := h.svc.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, loginResponse{Token: token, User: user})
}

// HandleMe GET /api/v1/auth/me 返回当前登录用户资料与钱包余额（任意角色可用）。
func (h *Handler) HandleMe(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return
	}
	u, err := h.svc.GetUser(r.Context(), userID)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	balance, err := h.credit.GetWallet(r.Context(), userID)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]any{"user": u, "balance": balance})
}

// HandleUpdateMe PUT /api/v1/auth/me 修改本人昵称。
func (h *Handler) HandleUpdateMe(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return
	}
	var req struct {
		Nickname string `json:"nickname"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	u, err := h.svc.UpdateNickname(r.Context(), userID, req.Nickname)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, u)
}

// writeServiceErr 将 identity.Service 返回的错误映射为统一响应。
func writeServiceErr(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		resp.Err(w, r, apiErr.HTTPStatus, apiErr.Code, apiErr.Message)
		return
	}
	resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "服务器内部错误")
}
