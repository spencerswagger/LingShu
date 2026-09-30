package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"unicode/utf8"

	"github.com/team/llmgateway/internal/pkg/crypto"
	"github.com/team/llmgateway/internal/pkg/jwtx"
	"github.com/team/llmgateway/internal/pkg/resp"
)

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
}

// NewService 创建认证服务。
func NewService(store *Store, jwtMgr *jwtx.Manager) *Service {
	return &Service{store: store, jwt: jwtMgr}
}

// Login 校验用户名密码与账户状态，签发 JWT，返回 token 与用户信息。
// 用户不存在、密码错误统一返回"用户名或密码错误"，避免账户枚举。
func (s *Service) Login(ctx context.Context, username, password string) (string, *User, error) {
	if username == "" || password == "" {
		return "", nil, errBadRequest("用户名和密码不能为空")
	}
	u, err := s.store.GetByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil, errUnauthorized()
		}
		return "", nil, fmt.Errorf("get user by username: %w", err)
	}
	if !crypto.VerifyPassword(password, u.PasswordHash) {
		return "", nil, errUnauthorized()
	}
	if u.Status == StatusDisabled {
		return "", nil, errDisabled()
	}
	token, err := s.jwt.Sign(u.ID, u.Username, u.Role, u.TokenVersion)
	if err != nil {
		return "", nil, fmt.Errorf("sign jwt: %w", err)
	}
	u.PasswordHash = "" // 不外泄哈希
	return token, u, nil
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
		Username:     username,
		PasswordHash: hash,
		Nickname:     nick,
		Role:         role,
		Status:       StatusActive,
		PricingMode:  pricingMode,
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
	u := &User{ID: id, Role: role, Status: status, PricingMode: pricingMode, Nickname: nick}
	if err := s.store.Update(ctx, u); err != nil {
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
	if _, err := s.store.UpdatePassword(ctx, id, hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound()
		}
		return fmt.Errorf("reset password: %w", err)
	}
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
	return n, nil
}
