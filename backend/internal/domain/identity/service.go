package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/team/llmgateway/internal/domain/audit"
	"github.com/team/llmgateway/internal/pkg/crypto"
	"github.com/team/llmgateway/internal/pkg/jwtx"
	"github.com/team/llmgateway/internal/pkg/reqmeta"
	"github.com/team/llmgateway/internal/pkg/resp"
	"github.com/team/llmgateway/internal/pkg/session"
)

// AuditSink 审计写入抽象：生产实现为 *audit.Store，测试可注入记录型实现，
// 以便断言认证事件确实落库（而不仅是"调用返回了 401"）。
type AuditSink interface {
	Insert(ctx context.Context, e audit.Entry) error
}

// APIError 是带 HTTP 状态码与业务码的领域错误，handler 据此返回响应。
type APIError struct {
	HTTPStatus int
	Code       int
	Message    string
}

func (e *APIError) Error() string { return e.Message }

func errBadRequest(msg string) *APIError {
	return &APIError{HTTPStatus: http.StatusBadRequest, Code: resp.CodeBadRequest, Message: msg}
}
func errUnauthorized() *APIError {
	return &APIError{HTTPStatus: http.StatusUnauthorized, Code: resp.CodeUnauthorized, Message: "用户名或密码错误"}
}
func errDisabled() *APIError {
	return &APIError{HTTPStatus: http.StatusForbidden, Code: resp.CodeForbidden, Message: "账户已被禁用，请联系管理员"}
}
func errForbidden(msg string) *APIError {
	return &APIError{HTTPStatus: http.StatusForbidden, Code: resp.CodeForbidden, Message: msg}
}
func errUsernameExists() *APIError {
	return &APIError{HTTPStatus: http.StatusConflict, Code: resp.CodeConflict, Message: "用户名已存在"}
}
func errNotFound() *APIError {
	return &APIError{HTTPStatus: http.StatusNotFound, Code: resp.CodeNotFound, Message: "用户不存在"}
}
func errInternal() *APIError {
	return &APIError{HTTPStatus: http.StatusInternalServerError, Code: resp.CodeInternalError, Message: "服务器内部错误"}
}

// Service 承载认证与用户管理业务逻辑。
type Service struct {
	store *Store
	jwt   *jwtx.Manager
	// 以下为可选安全依赖（nil 时对应能力降级/跳过）。
	sessions *session.Registry
	audit    AuditSink
	totp     *TOTPService
}

// NewService 创建认证服务。
func NewService(store *Store, jwtMgr *jwtx.Manager) *Service {
	return &Service{store: store, jwt: jwtMgr}
}

func (s *Service) SetSessionRegistry(r *session.Registry) { s.sessions = r }
func (s *Service) SetAudit(a AuditSink)                   { s.audit = a }
func (s *Service) SetTOTP(t *TOTPService)                 { s.totp = t }

// auditLog 写入一条审计记录：nil-safe、失败仅记日志不阻塞业务。
// 自动补齐请求元数据（可信 IP / request id）——service 层拿不到 *http.Request，
// 由 reqmeta 中间件在 handler 层注入上下文。
func (s *Service) auditLog(ctx context.Context, e audit.Entry) {
	if m := reqmeta.From(ctx); m.IP != "" || m.RequestID != "" {
		if e.IP == "" {
			e.IP = m.IP
		}
		if e.RequestID == "" {
			e.RequestID = m.RequestID
		}
	}
	if s.audit == nil {
		return
	}
	if err := s.audit.Insert(ctx, e); err != nil {
		slog.ErrorContext(ctx, "audit insert failed", "action", e.Action, "err", err)
	}
}

// LoginResult 登录结果：未开 2FA 时 Token 非空；已开 2FA 时 NeedTOTP=true 且 Token 为空。
type LoginResult struct {
	Token              string
	User               *User
	NeedTOTP           bool
	MustChangePassword bool
	TokenVersion       int64
}

// validatePasswordStrength 密码强度：≥8 字符且至少含两类字符（小写/大写/数字/符号）。
func validatePasswordStrength(password string) error {
	runes := utf8.RuneCountInString(password)
	if runes < 8 {
		return errBadRequest("密码至少 8 个字符")
	}
	var lower, upper, digit, symbol bool
	for _, r := range password {
		switch {
		case r >= 'a' && r <= 'z':
			lower = true
		case r >= 'A' && r <= 'Z':
			upper = true
		case r >= '0' && r <= '9':
			digit = true
		default:
			symbol = true
		}
	}
	classes := 0
	for _, b := range []bool{lower, upper, digit, symbol} {
		if b {
			classes++
		}
	}
	if classes < 2 {
		return errBadRequest("密码需至少包含字母、数字、符号中的两类")
	}
	return nil
}

// maxUsernameLen 登录用户名写入审计时的字节上限。目的是给审计存储封顶
// （请求体上限 50MB，攻击者可用超长用户名把审计表撑爆）。
const maxUsernameLen = 64

// truncateUTF8 按 rune 边界把 s 截断到不超过 maxBytes 字节，保证结果仍是合法 UTF-8。
// 直接 username[:64] 是字节切片，会切断多字节字符（中文/emoji）产生非法 UTF-8，
// 而 PostgreSQL 对非法字节序列是报错（SQLSTATE 22021）而非静默替换 →
// 会让审计 INSERT 失败、登录失败一条都留不下，等于审计被规避。
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// Login 第一步：校验口令与状态。未开 2FA 时完成登录（bump ver + 签发）；
// 已开 2FA 时返回 NeedTOTP=true（不发 JWT、不 bump ver）。
// 用户不存在、密码错误统一返回"用户名或密码错误"，避免账户枚举。
func (s *Service) Login(ctx context.Context, username, password string) (*LoginResult, error) {
	if username == "" || password == "" {
		return nil, errBadRequest("用户名和密码不能为空")
	}
	// 超长用户名：按认证失败处理（与"用户不存在"同构，不引入枚举差异），
	// 但必须留下审计——否则攻击者可用超长用户名让所有失败尝试不留痕。
	if len(username) > maxUsernameLen {
		s.auditLog(ctx, audit.Entry{Username: truncateUTF8(username, maxUsernameLen), Action: "auth.login.fail",
			Detail: map[string]any{"reason": "username_too_long", "len": len(username)}})
		return nil, errUnauthorized()
	}
	u, err := s.store.GetByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			s.auditLog(ctx, audit.Entry{Username: username, Action: "auth.login.fail", Detail: map[string]any{"reason": "user_not_found"}})
			return nil, errUnauthorized()
		}
		return nil, fmt.Errorf("get user by username: %w", err)
	}
	if !crypto.VerifyPassword(password, u.PasswordHash) {
		s.auditLog(ctx, audit.Entry{UserID: u.ID, Username: u.Username, Action: "auth.login.fail",
			TargetType: "user", TargetID: strconv.FormatInt(u.ID, 10), Detail: map[string]any{"reason": "bad_password"}})
		return nil, errUnauthorized()
	}
	if u.Status == StatusDisabled {
		s.auditLog(ctx, audit.Entry{UserID: u.ID, Username: u.Username, Action: "auth.login.fail",
			TargetType: "user", TargetID: strconv.FormatInt(u.ID, 10), Detail: map[string]any{"reason": "disabled"}})
		return nil, errDisabled()
	}
	u.PasswordHash = ""
	if u.TOTPEnabled {
		return &LoginResult{User: u, NeedTOTP: true, MustChangePassword: u.MustChangePassword}, nil
	}
	token, ver, err := s.issueSession(ctx, u)
	if err != nil {
		return nil, err
	}
	s.auditLog(ctx, audit.Entry{UserID: u.ID, Username: u.Username, Action: "auth.login.success",
		TargetType: "user", TargetID: strconv.FormatInt(u.ID, 10)})
	return &LoginResult{Token: token, User: u, MustChangePassword: u.MustChangePassword, TokenVersion: ver}, nil
}

// issueSession bump token_version 并签发新 JWT，同步内存会话注册表。
func (s *Service) issueSession(ctx context.Context, u *User) (string, int64, error) {
	ver, err := s.store.BumpTokenVersion(ctx, u.ID)
	if err != nil {
		return "", 0, fmt.Errorf("bump token version: %w", err)
	}
	token, err := s.jwt.Sign(u.ID, u.Username, u.Role, int(ver))
	if err != nil {
		return "", 0, fmt.Errorf("sign jwt: %w", err)
	}
	if s.sessions != nil {
		s.sessions.Set(u.ID, session.Entry{Version: int(ver), Status: u.Status, MustChange: u.MustChangePassword})
	}
	return token, ver, nil
}

// LoginTOTP 第二步：校验 preauth 对应用户的 TOTP code，成功则完成登录。
func (s *Service) LoginTOTP(ctx context.Context, userID int64, code string) (*LoginResult, error) {
	if s.totp == nil {
		return nil, errInternal()
	}
	ok, err := s.totp.Verify(ctx, userID, code)
	if err != nil || !ok {
		return nil, errUnauthorized()
	}
	u, err := s.store.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errUnauthorized()
		}
		return nil, fmt.Errorf("get user: %w", err)
	}
	if u.Status != StatusActive {
		return nil, errDisabled()
	}
	u.PasswordHash = ""
	token, _, err := s.issueSession(ctx, u)
	if err != nil {
		return nil, err
	}
	s.auditLog(ctx, audit.Entry{UserID: u.ID, Username: u.Username, Action: "auth.login_totp.success",
		TargetType: "user", TargetID: strconv.FormatInt(u.ID, 10)})
	return &LoginResult{Token: token, User: u, MustChangePassword: u.MustChangePassword}, nil
}

// Logout 登出：会话代数 +1，当前 token 失效。
func (s *Service) Logout(ctx context.Context, userID int64) error {
	ver, err := s.store.BumpTokenVersion(ctx, userID)
	if err != nil {
		return err
	}
	if s.sessions != nil {
		if u, err := s.store.GetByID(ctx, userID); err == nil {
			s.sessions.Set(userID, session.Entry{Version: int(ver), Status: u.Status, MustChange: u.MustChangePassword})
		}
	}
	s.auditLog(ctx, audit.Entry{UserID: userID, Action: "auth.logout", TargetType: "user", TargetID: strconv.FormatInt(userID, 10)})
	return nil
}

// ChangeOwnPassword 本人改密：校验旧口令与强度，改密后会话代数 +1 并签发新 token。
func (s *Service) ChangeOwnPassword(ctx context.Context, userID int64, oldPassword, newPassword string) (string, error) {
	if err := validatePasswordStrength(newPassword); err != nil {
		return "", err
	}
	u, err := s.store.GetByID(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errUnauthorized()
		}
		return "", fmt.Errorf("get user: %w", err)
	}
	if !crypto.VerifyPassword(oldPassword, u.PasswordHash) {
		return "", errUnauthorized()
	}
	hash, err := crypto.HashPassword(newPassword)
	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}
	ver, err := s.store.ChangePassword(ctx, userID, hash)
	if err != nil {
		return "", fmt.Errorf("change password: %w", err)
	}
	u.MustChangePassword = false
	u.TokenVersion = int(ver)
	if s.sessions != nil {
		s.sessions.Set(userID, session.Entry{Version: int(ver), Status: u.Status, MustChange: false})
	}
	token, err := s.jwt.Sign(userID, u.Username, u.Role, int(ver))
	if err != nil {
		return "", fmt.Errorf("sign jwt: %w", err)
	}
	s.auditLog(ctx, audit.Entry{UserID: userID, Username: u.Username, Action: "auth.password.change",
		TargetType: "user", TargetID: strconv.FormatInt(userID, 10)})
	return token, nil
}

// normalizeNickname 清洗昵称：去首尾空白，空视为未设置；超长返回错误。
func normalizeNickname(nickname string) (string, error) {
	n := strings.TrimSpace(nickname)
	if utf8.RuneCountInString(n) > 40 {
		return "", errBadRequest("昵称最长 40 个字符")
	}
	return n, nil
}

// AdminCreateUser 由管理员创建新用户，并同步补充 credit_wallets 行。
func (s *Service) AdminCreateUser(ctx context.Context, username, password, role, pricingMode string, nickname string) (*User, error) {
	if username == "" || password == "" {
		return nil, errBadRequest("用户名和密码不能为空")
	}
	if role != RoleAdmin && role != RoleDeveloper {
		return nil, errBadRequest("角色只允许 ADMIN 或 DEVELOPER")
	}
	if pricingMode != PricingModeSale && pricingMode != PricingModeCost {
		return nil, errBadRequest("计费模式只允许 sale 或 cost")
	}
	nick, err := normalizeNickname(nickname)
	if err != nil {
		return nil, err
	}
	hash, err := crypto.HashPassword(password)
	if err != nil {
		return nil, fmt.Errorf("hash password: %w", err)
	}
	u := &User{
		Username:           username,
		PasswordHash:       hash,
		Nickname:           nick,
		Role:               role,
		Status:             StatusActive,
		PricingMode:        pricingMode,
		MustChangePassword: true, // 管理员新建账号强制首登改密
	}
	created, err := s.store.Create(ctx, u)
	if err != nil {
		if errors.Is(err, ErrUsernameExists) {
			return nil, errUsernameExists()
		}
		return nil, fmt.Errorf("create user: %w", err)
	}
	if err := s.store.EnsureWallet(ctx, created.ID); err != nil {
		return nil, fmt.Errorf("ensure wallet: %w", err)
	}
	created.PasswordHash = ""
	s.auditLog(ctx, audit.Entry{Action: "admin.user.create", TargetType: "user", TargetID: strconv.FormatInt(created.ID, 10),
		Detail: map[string]any{"username": created.Username, "role": created.Role, "pricing_mode": created.PricingMode}})
	return created, nil
}

// ListUsers 分页查询用户，调用方需具备管理权限。q 模糊匹配用户名；role/status 精确过滤。
// 结果附带钱包余额与累计消费额（completed 账单求和）。
func (s *Service) ListUsers(ctx context.Context, q, role, status string, page, size int) ([]User, int64, error) {
	users, total, err := s.store.List(ctx, q, role, status, page, size)
	if err != nil {
		return nil, 0, fmt.Errorf("list users: %w", err)
	}
	ids := make([]int64, 0, len(users))
	for i := range users {
		users[i].PasswordHash = ""
		ids = append(ids, users[i].ID)
	}
	if err := s.enrichLedger(ctx, ids, func(id int64, e Ledger) {
		for i := range users {
			if users[i].ID == id {
				users[i].Balance = e.Balance
				users[i].TotalSpent = e.TotalSpent
				return
			}
		}
	}); err != nil {
		return nil, 0, fmt.Errorf("load ledger: %w", err)
	}
	return users, total, nil
}

// enrichLedger 批量加载钱包余额与累计消费，apply 负责把结果写入目标结构。
func (s *Service) enrichLedger(ctx context.Context, ids []int64, apply func(id int64, e Ledger)) error {
	if len(ids) == 0 {
		return nil
	}
	ledger, err := s.store.LoadLedger(ctx, ids)
	if err != nil {
		return err
	}
	for id, e := range ledger {
		apply(id, e)
	}
	return nil
}

// UpdateUser 更新用户角色/状态/计费模式/昵称。
// 角色是鉴权权威来源之一：角色变更会使旧令牌立即失效（bump token_version）。
func (s *Service) UpdateUser(ctx context.Context, id int64, role, status string, pricingMode string, nickname string) (*User, error) {
	if role != RoleAdmin && role != RoleDeveloper {
		return nil, errBadRequest("角色只允许 ADMIN 或 DEVELOPER")
	}
	if status != StatusActive && status != StatusDisabled {
		return nil, errBadRequest("状态只允许 ACTIVE 或 DISABLED")
	}
	if pricingMode != PricingModeSale && pricingMode != PricingModeCost {
		return nil, errBadRequest("计费模式只允许 sale 或 cost")
	}
	nick, err := normalizeNickname(nickname)
	if err != nil {
		return nil, err
	}
	prev, err := s.store.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound()
		}
		return nil, fmt.Errorf("get user: %w", err)
	}
	u := &User{ID: id, Role: role, Status: status, PricingMode: pricingMode, Nickname: nick}
	// 一条原子 SQL 完成"改属性 +（角色变更时）递增会话代数"，无中间态。
	newVer, newStatus, newMustChange, err := s.store.Update(ctx, u)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound()
		}
		return nil, fmt.Errorf("update user: %w", err)
	}
	got, err := s.store.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound()
		}
		return nil, fmt.Errorf("get user: %w", err)
	}
	got.PasswordHash = ""
	got.TokenVersion = int(newVer)
	if s.sessions != nil {
		s.sessions.Set(id, session.Entry{Version: int(newVer), Status: newStatus, MustChange: newMustChange})
	}
	s.auditLog(ctx, audit.Entry{Action: "admin.user.update", TargetType: "user", TargetID: strconv.FormatInt(id, 10),
		Detail: map[string]any{"role": role, "status": status, "prev_role": prev.Role}})
	return got, nil
}

// AdminResetPassword 管理员重置用户登录口令。
func (s *Service) AdminResetPassword(ctx context.Context, id int64, newPassword string) error {
	if newPassword == "" {
		return errBadRequest("新密码不能为空")
	}
	if _, err := s.store.GetByID(ctx, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound()
		}
		return fmt.Errorf("get user: %w", err)
	}
	hash, err := crypto.HashPassword(newPassword)
	if err != nil {
		return fmt.Errorf("hash password: %w", err)
	}
	ver, err := s.store.UpdatePassword(ctx, id, hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound()
		}
		return fmt.Errorf("reset password: %w", err)
	}
	if s.sessions != nil {
		if u, gerr := s.store.GetByID(ctx, id); gerr == nil {
			s.sessions.Set(id, session.Entry{Version: int(ver), Status: u.Status, MustChange: u.MustChangePassword})
		}
	}
	// 管理员重置他人口令是最高风险动作之一，必须留结构化审计（不含口令/哈希）。
	s.auditLog(ctx, audit.Entry{Action: "admin.user.reset_password", TargetType: "user", TargetID: strconv.FormatInt(id, 10),
		Detail: map[string]any{"token_version": ver}})
	return nil
}

// UpdateNickname 由本人修改自己的昵称（/api/v1/auth/me）。
func (s *Service) UpdateNickname(ctx context.Context, id int64, nickname string) (*User, error) {
	nick, err := normalizeNickname(nickname)
	if err != nil {
		return nil, err
	}
	got, err := s.store.UpdateNickname(ctx, id, nick)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound()
		}
		return nil, fmt.Errorf("update nickname: %w", err)
	}
	got.PasswordHash = ""
	return got, nil
}

func errBillingRecorded(username string) *APIError {
	msg := "存在账单记录的用户无法删除，请改为禁用账户"
	if username != "" {
		msg = "用户「" + username + "」存在账单记录，无法删除，请改为禁用账户"
	}
	return &APIError{HTTPStatus: http.StatusConflict, Code: resp.CodeConflict, Message: msg}
}

// BatchDeleteUsers 批量删除用户。若用户存在账单记录（审计轨迹不可删除），返回
// 409 冲突错误；否则 store 会级联清理引用数据后删除。
func (s *Service) BatchDeleteUsers(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, errBadRequest("未指定要删除的用户")
	}
	n, err := s.store.BatchDelete(ctx, ids)
	if err != nil {
		var brErr *BillingRecordedError
		if errors.As(err, &brErr) {
			return 0, errBillingRecorded(brErr.Username)
		}
		return 0, fmt.Errorf("batch delete users: %w", err)
	}
	// 删除即断权：清缓存后，下次鉴权回落到 Loader，
	// Loader 的 SQL 带 deleted_at IS NULL，软删行返回 sql.ErrNoRows → ErrRevoked。
	if s.sessions != nil {
		for _, id := range ids {
			s.sessions.Delete(id)
		}
	}
	s.auditLog(ctx, audit.Entry{Action: "admin.user.batch_delete", TargetType: "user",
		Detail: map[string]any{"ids": ids, "affected": n}})
	return n, nil
}
