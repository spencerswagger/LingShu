// token.go 令牌（API Key）领域实现：SM3 哈希、CRUD、滚动、开关，以及对应 HTTP 处理器。
// 令牌明文以 SM4 加密后落库（secret_cipher），支持随时查看/复制；校验仍走 SM3 哈希。
// 令牌可空的 tag_id 用于默认路由（nil = 默认路由）。
package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/emmansun/gmsm/sm3"
	"github.com/team/llmgateway/internal/pkg/crypto"
	"github.com/team/llmgateway/internal/pkg/idgen"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// 令牌状态常量。
const (
	StatusTokenActive   = "ACTIVE"
	StatusTokenDisabled = "DISABLED"
)

// Token 对应 tokens 表一行。TokenHash 为明文 SM3 哈希，TokenDisplay 为脱敏展示串，
// SecretCipher 为明文 SM4 加密密文（支持事后查看/复制；未启用 SM4 或历史数据为空串）。
type Token struct {
	ID           int64
	TokenHash    string
	TokenDisplay string
	SecretCipher string
	UserID       int64
	DisplayName  string
	TagID        *int64
	ExpiresAt    *time.Time
	LastUsedAt   *time.Time
	Status       string
	CreatedAt    time.Time
}

// TokenInfo 为令牌列表项，附带所属用户名与昵称（管理端列表需要）。
type TokenInfo struct {
	Token
	Username string
	Nickname string
}

// tokenCols 列出 tokens 表查询使用的全部列。
const tokenCols = `id, token_hash, token_display, user_id, display_name, tag_id, expires_at, last_used_at, status, created_at`

// tokenInfoCols 列出列表 JOIN users 后使用的全部列（tokens 列 + users.username + users.nickname）。
// 注意：拼接进 JOIN 查询的 SELECT 列表，列必须带 t. 前缀，避免与 users 的 id 等列歧义。
const tokenInfoCols = `t.id, t.token_hash, t.token_display, t.user_id, t.display_name, t.tag_id, t.expires_at, t.last_used_at, t.status, t.created_at, u.username, u.nickname`

// scanToken 将一行扫描到 *Token。
func scanToken(row interface{ Scan(...any) error }) (*Token, error) {
	var t Token
	var tagID sql.NullInt64
	var expiresAt, lastUsedAt sql.NullTime
	err := row.Scan(&t.ID, &t.TokenHash, &t.TokenDisplay, &t.UserID, &t.DisplayName,
		&tagID, &expiresAt, &lastUsedAt, &t.Status, &t.CreatedAt)
	if err != nil {
		return nil, err
	}
	if tagID.Valid {
		t.TagID = &tagID.Int64
	}
	if expiresAt.Valid {
		t.ExpiresAt = &expiresAt.Time
	}
	if lastUsedAt.Valid {
		t.LastUsedAt = &lastUsedAt.Time
	}
	return &t, nil
}

// tokenHash 计算令牌明文的 SM3 哈希（十六进制）。crypto 包未导出该 helper，
// 且口令哈希带盐不适用于令牌，故此处直接调用 gmsm 的 sm3.Sum。
func tokenHash(plain string) string {
	h := sm3.Sum([]byte(plain))
	return hex.EncodeToString(h[:])
}

// GenerateToken 生成令牌：plain = "sk-gw-"+32 hex；display = 脱敏串，仅保留末尾 4 位 hex。
func GenerateToken() (plain, display string) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		// 随机源失败时退化为时间戳哈希，保证可用性（实际几乎不会发生）。
		sum := sm3.Sum([]byte(strconv.FormatInt(time.Now().UnixNano(), 10)))
		b = sum[:16]
	}
	hexStr := hex.EncodeToString(b)
	plain = "sk-gw-" + hexStr
	display = tokenDisplay(plain)
	return plain, display
}

// tokenDisplay 由明文令牌推导脱敏展示串：前缀 "sk-gw-" 保留，其余除末尾 4 位外打码。
func tokenDisplay(plain string) string {
	const prefix = "sk-gw-"
	if len(plain) <= len(prefix)+4 {
		return prefix + "****"
	}
	return prefix + strings.Repeat("*", len(plain)-len(prefix)-4) + plain[len(plain)-4:]
}

// TokenStore 提供 tokens 表数据访问。
type TokenStore struct {
	db *sql.DB
}

// NewTokenStore 创建令牌存储。
func NewTokenStore(db *sql.DB) *TokenStore {
	return &TokenStore{db: db}
}

// UserStatus 查询用户存在性与状态：返回 (角色, 状态, 是否存在)。
func (s *TokenStore) UserStatus(ctx context.Context, userID int64) (role, status string, exists bool, err error) {
	var r, st string
	err = s.db.QueryRowContext(ctx,
		`SELECT role, status FROM users WHERE id = $1 AND deleted_at IS NULL`, userID).Scan(&r, &st)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", false, nil
	}
	if err != nil {
		return "", "", false, err
	}
	return r, st, true, nil
}

// CreateToken 插入令牌并回填主键与创建时间。
func (s *TokenStore) CreateToken(ctx context.Context, t *Token) error {
	if t.ID == 0 {
		t.ID = idgen.New()
	}
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO tokens(id, token_hash, token_display, secret_cipher, user_id, display_name, tag_id, expires_at)
		 VALUES($1, $2, $3, $4, $5, $6, $7, $8)
		 RETURNING id, created_at`,
		t.ID, t.TokenHash, t.TokenDisplay, nullString(t.SecretCipher), t.UserID, t.DisplayName, t.TagID, t.ExpiresAt)
	var id int64
	var createdAt time.Time
	if err := row.Scan(&id, &createdAt); err != nil {
		return err
	}
	t.ID = id
	t.CreatedAt = createdAt
	return nil
}

// SecretCipherByID 按主键查询令牌的 SM4 密文（用于事后查看密钥）；不存在返回 sql.ErrNoRows。
func (s *TokenStore) SecretCipherByID(ctx context.Context, id int64) (string, error) {
	var cipher string
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(secret_cipher, '') FROM tokens WHERE id = $1 AND deleted_at IS NULL`, id).Scan(&cipher)
	if err != nil {
		return "", err
	}
	return cipher, nil
}

// TouchLastUsed 更新令牌最后使用时间（网关鉴权成功后异步调用，低频率因此直写）。
func (s *TokenStore) TouchLastUsed(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET last_used_at = now() WHERE id = $1 AND deleted_at IS NULL`, id)
	return err
}

func nullString(v string) any {
	if v == "" {
		return nil
	}
	return v
}

// GetByHash 按哈希查询令牌；不存在返回 sql.ErrNoRows。
func (s *TokenStore) GetByHash(ctx context.Context, hash string) (*Token, error) {
	return scanToken(s.db.QueryRowContext(ctx,
		`SELECT `+tokenCols+` FROM tokens WHERE token_hash = $1 AND deleted_at IS NULL`, hash))
}

// GetByID 按主键查询令牌；不存在返回 sql.ErrNoRows。
func (s *TokenStore) GetByID(ctx context.Context, id int64) (*Token, error) {
	return scanToken(s.db.QueryRowContext(ctx,
		`SELECT `+tokenCols+` FROM tokens WHERE id = $1 AND deleted_at IS NULL`, id))
}

// UpdateStatus 更新令牌状态；无匹配行返回 sql.ErrNoRows。
func (s *TokenStore) UpdateStatus(ctx context.Context, id int64, status string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET status = $1 WHERE id = $2`, status, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// ListTokens 分页查询令牌。userID 非 nil 时按用户过滤（开发端仅本人），
// status 非空时按状态过滤。返回列表（含用户名）与总条数。
func (s *TokenStore) ListTokens(ctx context.Context, userID *int64, status string, page, size int) ([]TokenInfo, int64, error) {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	offset := int64((page - 1) * size)

	var total int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM tokens t JOIN users u ON u.id = t.user_id
		 WHERE t.deleted_at IS NULL AND u.deleted_at IS NULL
		   AND ($1::bigint IS NULL OR t.user_id = $1) AND ($2 = '' OR t.status = $2)`,
		userID, status).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count tokens: %w", err)
	}

	rows, err := s.db.QueryContext(ctx,
		`SELECT `+tokenInfoCols+` FROM tokens t JOIN users u ON u.id = t.user_id
		 WHERE t.deleted_at IS NULL AND u.deleted_at IS NULL
		   AND ($1::bigint IS NULL OR t.user_id = $1) AND ($2 = '' OR t.status = $2)
		 ORDER BY t.id DESC LIMIT $3 OFFSET $4`,
		userID, status, size, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()

	list := make([]TokenInfo, 0, size)
	for rows.Next() {
		ti, err := scanTokenInfo(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("scan token: %w", err)
		}
		list = append(list, *ti)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// scanTokenInfo 将列表 JOIN 行（tokens 列 + username）扫描到 *TokenInfo。
func scanTokenInfo(row interface{ Scan(...any) error }) (*TokenInfo, error) {
	var t Token
	var username, nickname string
	var tagID sql.NullInt64
	var expiresAt, lastUsedAt sql.NullTime
	err := row.Scan(&t.ID, &t.TokenHash, &t.TokenDisplay, &t.UserID, &t.DisplayName,
		&tagID, &expiresAt, &lastUsedAt, &t.Status, &t.CreatedAt, &username, &nickname)
	if err != nil {
		return nil, err
	}
	t.TagID = nullToPtr(tagID)
	t.ExpiresAt = nullToTimePtr(expiresAt)
	t.LastUsedAt = nullToTimePtr(lastUsedAt)
	return &TokenInfo{Token: t, Username: username, Nickname: nickname}, nil
}

func nullToPtr(n sql.NullInt64) *int64 {
	if n.Valid {
		return &n.Int64
	}
	return nil
}
func nullToTimePtr(n sql.NullTime) *time.Time {
	if n.Valid {
		return &n.Time
	}
	return nil
}

// DeleteTokens 批量软删除令牌，返回删除数量。
// 软删除不触发删除物理行，billing_records.token_id 等审计引用保持完整；
// 令牌删除后鉴权（GetByHash 过滤 deleted_at）即视为不存在。
func (s *TokenStore) DeleteTokens(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	anyArg := "{" + strings.Join(parts, ",") + "}"

	res, err := s.db.ExecContext(ctx,
		`UPDATE tokens SET deleted_at = now() WHERE id = ANY($1::bigint[])`, anyArg)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TokenService 承载令牌业务逻辑。
type TokenService struct {
	store *TokenStore
	// secretKey 为令牌明文 SM4 加密密钥（与渠道凭据同源）。nil 时创建仍返回明文但不落库，
	// 事后无法查看/复制密钥（历史/测试场景）。
	secretKey []byte
}

// NewTokenService 创建令牌服务。
func NewTokenService(store *TokenStore) *TokenService {
	return &TokenService{store: store}
}

// SetSecretKey 注入 SM4 密钥（创建令牌时加密明文落库，支持事后查看/复制密钥）。
func (s *TokenService) SetSecretKey(key []byte) {
	s.secretKey = key
}

func errTokenNotFound() *APIError {
	return &APIError{HTTPStatus: http.StatusNotFound, Code: resp.CodeNotFound, Message: "令牌不存在"}
}
func errInvalidToken() *APIError {
	return &APIError{HTTPStatus: http.StatusUnauthorized, Code: resp.CodeUnauthorized, Message: "令牌无效或已失效"}
}
func errTokenForbidden() *APIError {
	return &APIError{HTTPStatus: http.StatusForbidden, Code: resp.CodeForbidden, Message: "无权操作该令牌"}
}
func errSecretUnavailable() *APIError {
	return &APIError{HTTPStatus: http.StatusNotFound, Code: resp.CodeNotFound, Message: "该令牌未备份密钥（历史令牌），可轮换后查看新密钥"}
}

// Create 创建新令牌：校验用户存在且 ACTIVE、displayName 非空、
// expiresAt（若提供）必须未来时间。明文以 SM4 加密落库支持事后查看；仍返回明文一次。
func (s *TokenService) Create(ctx context.Context, userID int64, displayName string, tagID *int64, expiresAt *time.Time) (string, error) {
	if displayName == "" {
		return "", errBadRequest("令牌名称不能为空")
	}
	if expiresAt != nil && !expiresAt.After(time.Now()) {
		return "", errBadRequest("过期时间必须是未来时间")
	}
	_, status, exists, err := s.store.UserStatus(ctx, userID)
	if err != nil {
		return "", fmt.Errorf("get token user: %w", err)
	}
	if !exists {
		return "", errNotFound()
	}
	if status != StatusActive {
		return "", errForbidden("账户已被禁用，无法创建令牌")
	}

	plain, display := GenerateToken()
	t := &Token{
		TokenHash:    tokenHash(plain),
		TokenDisplay: display,
		UserID:       userID,
		DisplayName:  displayName,
		TagID:        tagID,
		ExpiresAt:    expiresAt,
		Status:       StatusTokenActive,
	}
	if len(s.secretKey) > 0 {
		cipher, cerr := crypto.SM4Encrypt(s.secretKey, []byte(plain))
		if cerr != nil {
			return "", fmt.Errorf("encrypt token secret: %w", cerr)
		}
		t.SecretCipher = cipher
	}
	if err := s.store.CreateToken(ctx, t); err != nil {
		return "", fmt.Errorf("create token: %w", err)
	}
	return plain, nil
}

// Secret 返回令牌明文密钥（解密 secret_cipher）。未配置 SM4 密钥或历史令牌无密文时报错。
func (s *TokenService) Secret(ctx context.Context, tokenID int64) (string, error) {
	cipher, err := s.store.SecretCipherByID(ctx, tokenID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errTokenNotFound()
		}
		return "", fmt.Errorf("get token secret: %w", err)
	}
	if len(s.secretKey) == 0 || cipher == "" {
		return "", errSecretUnavailable()
	}
	plain, err := crypto.SM4Decrypt(s.secretKey, cipher)
	if err != nil {
		return "", fmt.Errorf("decrypt token secret: %w", err)
	}
	return string(plain), nil
}

// TouchLastUsed 记录令牌最后使用时间（网关鉴权通过后异步调用）。
func (s *TokenService) TouchLastUsed(ctx context.Context, tokenID int64) error {
	return s.store.TouchLastUsed(ctx, tokenID)
}

// LookupByPlain 按明文令牌校验，供网关鉴权使用。
// 校验：status = ACTIVE、未过期、关联 user 存在且 ACTIVE。
func (s *TokenService) LookupByPlain(ctx context.Context, plain string) (*Token, error) {
	t, err := s.store.GetByHash(ctx, tokenHash(plain))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errInvalidToken()
		}
		return nil, fmt.Errorf("lookup token: %w", err)
	}
	if t.Status != StatusTokenActive {
		return nil, errInvalidToken()
	}
	if t.ExpiresAt != nil && time.Now().After(*t.ExpiresAt) {
		return nil, errInvalidToken()
	}
	// 关联用户需存在且 ACTIVE，防止账号被删/禁用后凭据仍可用。
	_, status, exists, err := s.store.UserStatus(ctx, t.UserID)
	if err != nil {
		return nil, fmt.Errorf("lookup token user: %w", err)
	}
	if !exists || status != StatusActive {
		return nil, errInvalidToken()
	}
	return t, nil
}

// List 分页查询令牌；userID 非 nil 时仅查该用户（开发端本人）。
func (s *TokenService) List(ctx context.Context, userID *int64, status string, page, size int) ([]TokenInfo, int64, error) {
	return s.store.ListTokens(ctx, userID, status, page, size)
}

// BatchDelete 批量删除令牌，返回删除数量。
func (s *TokenService) BatchDelete(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, errBadRequest("未指定要删除的令牌")
	}
	n, err := s.store.DeleteTokens(ctx, ids)
	if err != nil {
		return 0, fmt.Errorf("batch delete tokens: %w", err)
	}
	return n, nil
}

// Rotate 作废旧令牌并换发新令牌（返回新明文，仅一次）。校验所有权归 userID。
func (s *TokenService) Rotate(ctx context.Context, userID, tokenID int64) (string, error) {
	t, err := s.store.GetByID(ctx, tokenID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", errTokenNotFound()
		}
		return "", fmt.Errorf("get token to rotate: %w", err)
	}
	if t.UserID != userID {
		return "", errTokenForbidden()
	}
	if err := s.store.UpdateStatus(ctx, tokenID, StatusTokenDisabled); err != nil {
		return "", fmt.Errorf("disable old token: %w", err)
	}
	return s.Create(ctx, t.UserID, t.DisplayName, t.TagID, t.ExpiresAt)
}

// Toggle 切换令牌状态（ACTIVE ⇄ DISABLED），返回更新后的令牌。
func (s *TokenService) Toggle(ctx context.Context, userID, tokenID int64) (*Token, error) {
	t, err := s.store.GetByID(ctx, tokenID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errTokenNotFound()
		}
		return nil, fmt.Errorf("get token to toggle: %w", err)
	}
	if t.UserID != userID {
		return nil, errTokenForbidden()
	}
	newStatus := StatusTokenActive
	if t.Status == StatusTokenActive {
		newStatus = StatusTokenDisabled
	}
	if err := s.store.UpdateStatus(ctx, tokenID, newStatus); err != nil {
		return nil, fmt.Errorf("toggle token: %w", err)
	}
	t.Status = newStatus
	return t, nil
}

// TokenHandler 暴露令牌 HTTP 处理器。userIDFrom 从 context 解析当前登录用户
// （由 server 装配传入，避免 identity 反向依赖 server 造成循环引用）。
type TokenHandler struct {
	svc            *TokenService
	userIDFrom     func(ctx context.Context) (int64, bool)
	verifyPassword func(ctx context.Context, userID int64, password string) error
}

// NewTokenHandler 创建令牌处理器。
func NewTokenHandler(svc *TokenService, userIDFrom func(ctx context.Context) (int64, bool)) *TokenHandler {
	return &TokenHandler{svc: svc, userIDFrom: userIDFrom}
}

// SetPasswordVerifier 注入当前口令校验（查看明文密钥等高危操作二次验证；nil=跳过校验）。
func (h *TokenHandler) SetPasswordVerifier(fn func(ctx context.Context, userID int64, password string) error) {
	h.verifyPassword = fn
}

type tokenResponse struct {
	ID           int64      `json:"ID,string"`
	TokenDisplay string     `json:"TokenDisplay,omitempty"`
	UserID       int64      `json:"UserID,string,omitempty"`
	Username     string     `json:"Username,omitempty"`
	UserNickname string     `json:"UserNickname,omitempty"` // 所属用户昵称（为空表示未设置）
	DisplayName  string     `json:"DisplayName"`
	TagID        *int64     `json:"TagID,string"`
	ExpiresAt    *time.Time `json:"ExpiresAt,omitempty"`
	LastUsedAt   *time.Time `json:"LastUsedAt,omitempty"`
	Status       string     `json:"Status"`
	CreatedAt    time.Time  `json:"CreatedAt"`
}

func (t *Token) response() tokenResponse {
	return tokenResponse{
		ID:           t.ID,
		TokenDisplay: t.TokenDisplay,
		UserID:       t.UserID,
		DisplayName:  t.DisplayName,
		TagID:        t.TagID,
		ExpiresAt:    t.ExpiresAt,
		LastUsedAt:   t.LastUsedAt,
		Status:       t.Status,
		CreatedAt:    t.CreatedAt,
	}
}

func (ti *TokenInfo) response() tokenResponse {
	r := ti.Token.response()
	r.Username = ti.Username
	r.UserNickname = ti.Nickname
	return r
}

// adminTokenCreateRequest 管理端创建令牌请求体。
type adminTokenCreateRequest struct {
	UserID      int64      `json:"UserID,string"`
	DisplayName string     `json:"DisplayName"`
	TagID       *int64     `json:"TagID,string"`
	ExpiresAt   *time.Time `json:"ExpiresAt"`
}

// devTokenCreateRequest 开发端创建令牌请求体。
type devTokenCreateRequest struct {
	DisplayName string     `json:"DisplayName"`
	TagID       *int64     `json:"TagID,string"`
	ExpiresAt   *time.Time `json:"ExpiresAt"`
}

type createdTokenResponse struct {
	Plain   string `json:"Plain"`
	Display string `json:"Display"`
}

// HandleAdminList GET /api/v1/admin/tokens 管理端令牌列表（可选 user_id/status）。
func (h *TokenHandler) HandleAdminList(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	var userID *int64
	if v := q.Get("UserID"); v != "" {
		id, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "user_id 必须为数字")
			return
		}
		userID = &id
	}
	status := q.Get("Status")
	page, size := parsePageSize(q)
	list, total, err := h.svc.List(r.Context(), userID, status, page, size)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	items := make([]tokenResponse, 0, len(list))
	for i := range list {
		items = append(items, list[i].response())
	}
	resp.OK(w, r, map[string]any{"List": items, "Total": total, "Page": page, "Size": size})
}

// HandleAdminCreate POST /api/v1/admin/tokens 管理端创建令牌。
func (h *TokenHandler) HandleAdminCreate(w http.ResponseWriter, r *http.Request) {
	var req adminTokenCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	plain, err := h.svc.Create(r.Context(), req.UserID, req.DisplayName, req.TagID, req.ExpiresAt)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, createdTokenResponse{Plain: plain, Display: tokenDisplay(plain)})
}

// HandleAdminBatchDelete POST /api/v1/admin/tokens/batch-delete 批量删除令牌。
func (h *TokenHandler) HandleAdminBatchDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs idgen.IDs `json:"IDs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	n, err := h.svc.BatchDelete(r.Context(), []int64(req.IDs))
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]any{"Affected": n})
}

// HandleAdminSecret GET /api/v1/admin/tokens/{id}/secret 管理端查看令牌密钥（可反复复制）。
// 敏感操作：需当前口令二次验证（X-Current-Password 头），防会话持有者静默查看明文密钥。
func (h *TokenHandler) HandleAdminSecret(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ID 参数无效")
		return
	}
	userID, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return
	}
	if h.verifyPassword != nil {
		if verr := h.verifyPassword(r.Context(), userID, r.Header.Get("X-Current-Password")); verr != nil {
			writeServiceErr(w, r, verr)
			return
		}
	}
	plain, err := h.svc.Secret(r.Context(), id)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, createdTokenResponse{Plain: plain, Display: tokenDisplay(plain)})
}

// HandleDevList GET /api/v1/dev/tokens 开发端本人令牌列表。
func (h *TokenHandler) HandleDevList(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return
	}
	q := r.URL.Query()
	page, size := parsePageSize(q)
	list, total, err := h.svc.List(r.Context(), &userID, q.Get("Status"), page, size)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	items := make([]tokenResponse, 0, len(list))
	for i := range list {
		items = append(items, list[i].response())
	}
	resp.OK(w, r, map[string]any{"List": items, "Total": total, "Page": page, "Size": size})
}

// HandleDevCreate POST /api/v1/dev/tokens 开发端创建本人令牌。
func (h *TokenHandler) HandleDevCreate(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return
	}
	var req devTokenCreateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	plain, err := h.svc.Create(r.Context(), userID, req.DisplayName, req.TagID, req.ExpiresAt)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, createdTokenResponse{Plain: plain, Display: tokenDisplay(plain)})
}

// HandleDevRotate POST /api/v1/dev/tokens/{id}/rotate 作废旧令牌换发新令牌。
func (h *TokenHandler) HandleDevRotate(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return
	}
	id, err := pathID(r)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ID 参数无效")
		return
	}
	plain, err := h.svc.Rotate(r.Context(), userID, id)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, createdTokenResponse{Plain: plain, Display: tokenDisplay(plain)})
}

// HandleDevSecret GET /api/v1/dev/tokens/{id}/secret 查看本人令牌密钥（可反复复制）。
// 敏感操作：需当前口令二次验证（X-Current-Password 头）。
func (h *TokenHandler) HandleDevSecret(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return
	}
	id, err := pathID(r)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ID 参数无效")
		return
	}
	if h.verifyPassword != nil {
		if verr := h.verifyPassword(r.Context(), userID, r.Header.Get("X-Current-Password")); verr != nil {
			writeServiceErr(w, r, verr)
			return
		}
	}
	t, err := h.svc.store.GetByID(r.Context(), id)
	if err != nil {
		writeServiceErr(w, r, errTokenNotFound())
		return
	}
	if t.UserID != userID {
		writeServiceErr(w, r, errTokenForbidden())
		return
	}
	plain, err := h.svc.Secret(r.Context(), id)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, createdTokenResponse{Plain: plain, Display: tokenDisplay(plain)})
}

// HandleDevToggle POST /api/v1/dev/tokens/{id}/toggle 切换令牌状态。
func (h *TokenHandler) HandleDevToggle(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return
	}
	id, err := pathID(r)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ID 参数无效")
		return
	}
	t, err := h.svc.Toggle(r.Context(), userID, id)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, t.response())
}

// pathID 从 Go 1.22 路由 PathValue 解析数字 ID。
func pathID(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// parsePageSize 解析分页参数，返回 (page, size)，非法时用默认值。
func parsePageSize(q url.Values) (page, size int) {
	page = 1
	size = 20
	if v := q.Get("Page"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			page = n
		}
	}
	if v := q.Get("Size"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			size = n
		}
	}
	return page, size
}
