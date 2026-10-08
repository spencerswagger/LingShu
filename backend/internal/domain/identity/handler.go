package identity

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/team/llmgateway/internal/pkg/clientip"
	"github.com/team/llmgateway/internal/pkg/ratelimit"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// Handler 暴露身份认证相关的 HTTP 处理器。
type Handler struct {
	svc        *Service
	credit     *CreditService
	userIDFrom func(ctx context.Context) (int64, bool)
	rl         *ratelimit.Limiter
	preauth    *PreAuthStore
}

// NewHandler 创建身份认证 HTTP 处理器。
func NewHandler(svc *Service, credit *CreditService, userIDFrom func(ctx context.Context) (int64, bool)) *Handler {
	return &Handler{svc: svc, credit: credit, userIDFrom: userIDFrom}
}

func (h *Handler) SetRateLimiter(rl *ratelimit.Limiter) { h.rl = rl }
func (h *Handler) SetPreAuth(p *PreAuthStore)           { h.preauth = p }

type loginRequest struct {
	Username string `json:"Username"`
	Password string `json:"Password"`
}

type loginResponse struct {
	Token              string `json:"Token,omitempty"`
	User               *User  `json:"User,omitempty"`
	NeedTOTP           bool   `json:"NeedTOTP,omitempty"`
	PreAuthToken       string `json:"PreAuthToken,omitempty"`
	MustChangePassword bool   `json:"MustChangePassword,omitempty"`
	TotpEnabled        bool   `json:"TotpEnabled,omitempty"`
}

// HandleLogin POST /api/v1/auth/login 处理登录第一步：校验口令并签发 JWT；
// 已开 2FA 的用户返回 need_totp 与一次性 preauth_token（不发 token）。
func (h *Handler) HandleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	ip := clientip.From(r)
	if h.rl != nil {
		// AllowAndRecord 在同一临界区内完成「放行判断 + 预记一次失败」，
		// 消除并发请求绕过失败窗口上限的竞态；登录成功后由 RecordSuccess 清零。
		if err := h.rl.AllowAndRecord(ip, req.Username); err != nil {
			resp.Err(w, r, http.StatusTooManyRequests, resp.CodeRateLimited, "尝试过于频繁，请稍后再试")
			return
		}
	}
	lr, err := h.svc.Login(r.Context(), req.Username, req.Password)
	if err != nil {
		// 失败计数已在 AllowAndRecord 中预记，此处不再 RecordFailure，避免重复计数
		writeServiceErr(w, r, err)
		return
	}
	if h.rl != nil {
		h.rl.RecordSuccess(ip, req.Username)
	}
	if lr.NeedTOTP {
		if h.preauth == nil {
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "服务器内部错误")
			return
		}
		pt, ok := h.preauth.Issue(lr.User.ID)
		if !ok {
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "服务器内部错误")
			return
		}
		resp.OK(w, r, loginResponse{NeedTOTP: true, PreAuthToken: pt, MustChangePassword: lr.MustChangePassword, TotpEnabled: true})
		return
	}
	resp.OK(w, r, loginResponse{Token: lr.Token, User: lr.User, MustChangePassword: lr.MustChangePassword, TotpEnabled: lr.User.TOTPEnabled})
}

// HandleLoginTOTP POST /api/v1/auth/login/totp 第二步：校验 TOTP code 完成登录。
func (h *Handler) HandleLoginTOTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PreAuthToken string `json:"PreAuthToken"`
		Code         string `json:"Code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	if h.preauth == nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "服务器内部错误")
		return
	}
	// 先只读校验 preauth token（Peek），限流/校验通过后再消费（Consume）：
	// 避免一次输错动态码或一次被限流就把预授权会话销毁，逼用户回退到口令第一步重来。
	userID, ok := h.preauth.Peek(req.PreAuthToken)
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "登录会话已过期，请重新登录")
		return
	}
	// 第二步限流 key 用已握有的 userID（无需再查库取用户名，且不随改名漂移）。
	rlKey := "uid:" + strconv.FormatInt(userID, 10)
	ip := clientip.From(r)
	if h.rl != nil {
		// 与第一步一致：AllowAndRecord 在同一临界区内完成「放行判断 + 预记一次失败」，
		// 消除并发请求绕过失败窗口上限的竞态；校验成功后由 RecordSuccess 清零。
		if err := h.rl.AllowAndRecord(ip, rlKey); err != nil {
			resp.Err(w, r, http.StatusTooManyRequests, resp.CodeRateLimited, "尝试过于频繁，请稍后再试")
			return
		}
	}
	// 先做无副作用校验（只验码 + 查状态）：失败不消费 preauth、不改 token_version，
	// 用户可直接重试（输错动态码/被限流的既有 UX 不变）。
	u, err := h.svc.VerifyTOTP(r.Context(), userID, req.Code)
	if err != nil {
		// 失败计数已在 AllowAndRecord 中预记，此处不再 RecordFailure，避免重复计数
		writeServiceErr(w, r, err)
		return
	}
	// 校验通过才原子消费一次性 preauth token（防重放）：并发下同一 preauth 只允许
	// 兑换一次会话，Consume 失败说明已被另一请求消费或已过期。此刻尚未签发会话，
	// 败者不会 bump token_version 作废胜者刚签发的 JWT。
	if _, ok := h.preauth.Consume(req.PreAuthToken); !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "登录会话已过期，请重新登录")
		return
	}
	lr, err := h.svc.IssueTOTPSession(r.Context(), u)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	if h.rl != nil {
		h.rl.RecordSuccess(ip, rlKey)
	}
	resp.OK(w, r, loginResponse{Token: lr.Token, User: lr.User, MustChangePassword: lr.MustChangePassword, TotpEnabled: true})
}

// HandleLogout POST /api/v1/auth/logout 登出：bump ver 使当前 token 失效。
func (h *Handler) HandleLogout(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return
	}
	if err := h.svc.Logout(r.Context(), userID); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]bool{"Logout": true})
}

// HandleChangePassword PUT /api/v1/auth/me/password 本人改密。
func (h *Handler) HandleChangePassword(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return
	}
	var req struct {
		OldPassword string `json:"OldPassword"`
		NewPassword string `json:"NewPassword"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	token, err := h.svc.ChangeOwnPassword(r.Context(), userID, req.OldPassword, req.NewPassword)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]string{"Token": token})
}

// HandleTOTPSetup POST /api/v1/auth/me/totp/setup 生成 TOTP secret（待确认态）。
// 需当前口令二次验证——已登录会话不足以下达，防会话劫持者静默开启 2FA 抢占账户。
func (h *Handler) HandleTOTPSetup(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return
	}
	var req struct {
		Password string `json:"Password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	if err := h.svc.VerifyCurrentPassword(r.Context(), userID, req.Password); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	u, err := h.svc.store.GetByID(r.Context(), userID)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	if h.svc.totp == nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "服务器内部错误")
		return
	}
	uri, secret, err := h.svc.totp.Setup(r.Context(), userID, u.Username)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]string{"OtpAuthURI": uri, "Secret": secret})
}

// HandleTOTPConfirm POST /api/v1/auth/me/totp/confirm 确认绑定并返回恢复码。
// 需当前口令二次验证；成功后返回恢复码逻辑保持不变。
func (h *Handler) HandleTOTPConfirm(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return
	}
	var req struct {
		Password string `json:"Password"`
		Code     string `json:"Code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	if err := h.svc.VerifyCurrentPassword(r.Context(), userID, req.Password); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	if h.svc.totp == nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "服务器内部错误")
		return
	}
	codes, err := h.svc.totp.Confirm(r.Context(), userID, req.Code)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]any{"RecoveryCodes": codes})
}

// HandleTOTPDisable DELETE /api/v1/auth/me/totp 解绑（需当前口令 + 当前 code）。
func (h *Handler) HandleTOTPDisable(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return
	}
	var req struct {
		Password string `json:"Password"`
		Code     string `json:"Code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	if err := h.svc.VerifyCurrentPassword(r.Context(), userID, req.Password); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	if h.svc.totp == nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "服务器内部错误")
		return
	}
	if err := h.svc.totp.Disable(r.Context(), userID, req.Code); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]bool{"Disabled": true})
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
	resp.OK(w, r, map[string]any{"User": u, "Balance": balance})
}

// HandleUpdateMe PUT /api/v1/auth/me 修改本人昵称。
func (h *Handler) HandleUpdateMe(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return
	}
	var req struct {
		Nickname string `json:"Nickname"`
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
