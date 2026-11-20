// keyadmin.go 是 magpie_keys 的写入方：管理端把「代理侧 callerKey」绑定到某个灵枢令牌。
//
// 为什么由运维录入而不是灵枢自举：callerKey 必须在 magpie 侧先存在（代理侧按它归集用量并鉴权），
// 灵枢自造一把 magpie 不认的 key 只会把失败推迟到转发时（403/401），等于假开通。
// 所以路径是「运维在 magpie 侧创建 callerKey → 把明文交给灵枢 → 灵枢 SM4 落库」。
//
// 端点（管理端）：
//
//	GET    /api/v1/admin/tokens/{id}/magpie-key   查看映射（只回元数据，永不回显明文）
//	PUT    /api/v1/admin/tokens/{id}/magpie-key   绑定/更新（幂等 upsert → ACTIVE）
//	DELETE /api/v1/admin/tokens/{id}/magpie-key   停用（保留行，state=DISABLED）
package magpie

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/team/llmgateway/internal/pkg/crypto"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// KeyBinder 是映射存储的能力（*KeyStore 满足；测试可替身）。
type KeyBinder interface {
	ByTokenID(ctx context.Context, tokenID int64) (*Key, error)
	Provision(ctx context.Context, tokenID int64, keyName, keyCipher string) (*Key, error)
	Disable(ctx context.Context, tokenID int64) error
}

// KeyAdminHandler 暴露 callerKey 映射的管理端接口。
type KeyAdminHandler struct {
	store    KeyBinder
	credKey  []byte
	adminKey string // 兼容保留（auto 不再使用）
	// secretOf 取令牌明文（identity 注入）；sync 把令牌明文注册为代理侧 callerKey。
	secretOf func(ctx context.Context, tokenID int64) (string, error)
	// displayNameOf 取令牌 display_name（auto 改名时做可读段原料；identity 注入）。
	displayNameOf func(ctx context.Context, tokenID int64) string
	// usernameOf 取令牌所属用户的用户名（key 名第一段原料；identity 注入）。
	usernameOf func(ctx context.Context, tokenID int64) string
	// ownerOf 取令牌所属用户 ID（key 名段退化坐标 u<uid>；identity 注入）。
	ownerOf func(ctx context.Context, tokenID int64) (int64, bool)
	sync    *CallerKeySync
}

// NewKeyAdminHandler 装配管理端处理器（credKey 为 security.sm4_key）。
func NewKeyAdminHandler(store KeyBinder, credKey []byte, adminKey string) *KeyAdminHandler {
	return &KeyAdminHandler{store: store, credKey: credKey, adminKey: strings.TrimSpace(adminKey)}
}

// SetAutoSync 注入令牌明文、令牌名、所属用户名读取器与同步器（auto 端点用；不注入则 auto 拒绝）。
func (h *KeyAdminHandler) SetAutoSync(secretOf func(ctx context.Context, tokenID int64) (string, error), sync *CallerKeySync, displayNameOf ...func(ctx context.Context, tokenID int64) string) {
	h.secretOf = secretOf
	h.sync = sync
	if len(displayNameOf) > 0 {
		h.displayNameOf = displayNameOf[0]
	}
}

// SetUsernameOf 注入令牌所属用户名的读取器（key 名第一段原料；不注入则段退化为 u<uid> 口径）。
func (h *KeyAdminHandler) SetUsernameOf(fn func(ctx context.Context, tokenID int64) string) {
	h.usernameOf = fn
}

// SetOwnerOf 注入令牌所属用户 ID 的读取器（key 名段退化坐标；auto 开通链用）。
func (h *KeyAdminHandler) SetOwnerOf(fn func(ctx context.Context, tokenID int64) (int64, bool)) {
	h.ownerOf = fn
}

// tokenUsername 取令牌所属用户名；未注入读取器时返回空（key 名用户段退化为 u<uid>）。
func (h *KeyAdminHandler) tokenUsername(ctx context.Context, tokenID int64) string {
	if h.usernameOf == nil {
		return ""
	}
	return h.usernameOf(ctx, tokenID)
}

// tokenDisplayName 取令牌 display_name；未注入读取器时返回空（slug 退化为随机段，名字仍唯一）。
func (h *KeyAdminHandler) tokenDisplayName(ctx context.Context, tokenID int64) string {
	if h.displayNameOf == nil {
		return ""
	}
	return h.displayNameOf(ctx, tokenID)
}

// HandleAutoPut POST /api/v1/admin/tokens/{id}/magpie-key/auto
// 一键开通：把**该令牌自己的明文**注册为代理侧 callerKey（名字 ls-token-<id>）。
// 客户端用令牌直连 /v1，magpie 看到的就是这把有效 callerKey。
// 历史令牌无密文（取不到明文）时明确提示「轮换后开通」，而不是绑共享 key 造成 401。
func (h *KeyAdminHandler) HandleAutoPut(w http.ResponseWriter, r *http.Request) {
	tokenID, ok := h.tokenID(w, r)
	if !ok {
		return
	}
	if h.sync == nil || h.secretOf == nil {
		resp.Err(w, r, http.StatusServiceUnavailable, resp.CodeInternalError, "未装配代理凭据同步（缺页面口令或 SM4）")
		return
	}
	plain, err := h.secretOf(r.Context(), tokenID)
	if err != nil || strings.TrimSpace(plain) == "" {
		resp.Err(w, r, http.StatusNotFound, resp.CodeNotFound, "该令牌未备份密钥（历史令牌），请轮换令牌后再开通")
		return
	}
	userID := tokenID
	if h.ownerOf != nil {
		if uid, ok := h.ownerOf(r.Context(), tokenID); ok && uid > 0 {
			userID = uid
		}
	}
	if err := h.sync.Ensure(r.Context(), userID, TokenRef{ID: tokenID, DisplayName: h.tokenDisplayName(r.Context(), tokenID)}, h.tokenUsername(r.Context(), tokenID), plain); err != nil {
		resp.Err(w, r, http.StatusBadGateway, resp.CodeInternalError, "开通失败："+err.Error())
		return
	}
	k, err := h.store.ByTokenID(r.Context(), tokenID)
	if err != nil || k == nil {
		resp.OK(w, r, keyBindingView{TokenID: tokenID, Bound: true})
		return
	}
	resp.OK(w, r, keyBindingView{TokenID: k.TokenID, KeyName: k.KeyName, State: k.State, Bound: true, HasKeyID: k.KeyID != ""})
}

// 绑定入参的边界（过短=误填，过长=拒绝，避免把整段配置粘贴进来）。
// maxKeyNameLen 按 magpie 真机上限收紧：0.1.4353 实测 120 字符（原 128 会在
// magpie 侧被 "Use a name between 1 and 120 characters" 拒掉）。
const (
	maxKeyNameLen   = MagpieKeyNameLimit // 120（真机实测，见 notes/magpie-keyname-limit-2026-11-20.md）
	maxCallerKeyLen = 512
)

// keyBindingRequest 是 PUT 的请求体。
type keyBindingRequest struct {
	// KeyName 是代理侧 callerKey 名（对账分组维度），必填。
	KeyName string `json:"KeyName"`
	// CallerKey 是代理侧 callerKey 明文，必填；落库只存 SM4 密文。
	CallerKey string `json:"CallerKey"`
}

// keyBindingView 是回包（永不含明文）。
type keyBindingView struct {
	TokenID  int64  `json:"TokenID,string"`
	KeyName  string `json:"KeyName"`
	State    string `json:"State"`
	Bound    bool   `json:"Bound"`
	HasKeyID bool   `json:"HasKeyID"`
}

// HandleGet GET /api/v1/admin/tokens/{id}/magpie-key
func (h *KeyAdminHandler) HandleGet(w http.ResponseWriter, r *http.Request) {
	tokenID, ok := h.tokenID(w, r)
	if !ok {
		return
	}
	k, err := h.store.ByTokenID(r.Context(), tokenID)
	if err != nil {
		if errors.Is(err, ErrKeyNotProvisioned) {
			resp.OK(w, r, keyBindingView{TokenID: tokenID, Bound: false})
			return
		}
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询代理凭据映射失败")
		return
	}
	resp.OK(w, r, keyBindingView{TokenID: tokenID, KeyName: k.KeyName, State: k.State, Bound: true, HasKeyID: k.KeyID != ""})
}

// HandlePut PUT /api/v1/admin/tokens/{id}/magpie-key
func (h *KeyAdminHandler) HandlePut(w http.ResponseWriter, r *http.Request) {
	tokenID, ok := h.tokenID(w, r)
	if !ok {
		return
	}
	if len(h.credKey) == 0 {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "凭据加密密钥未配置")
		return
	}
	var req keyBindingRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	keyName := strings.TrimSpace(req.KeyName)
	plain := strings.TrimSpace(req.CallerKey)
	switch {
	case keyName == "":
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "KeyName 不能为空")
		return
	case plain == "":
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "CallerKey 不能为空")
		return
	case len(keyName) > maxKeyNameLen:
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "KeyName 过长")
		return
	case len(plain) > maxCallerKeyLen:
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "CallerKey 过长")
		return
	}
	cipher, err := crypto.SM4Encrypt(h.credKey, []byte(plain))
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "凭据加密失败")
		return
	}
	k, err := h.store.Provision(r.Context(), tokenID, keyName, cipher)
	if err != nil {
		if isUniqueKeyName(err) {
			resp.Err(w, r, http.StatusConflict, resp.CodeConflict, "该 KeyName 已被其他令牌占用")
			return
		}
		if isForeignKeyViolation(err) {
			resp.Err(w, r, http.StatusNotFound, resp.CodeNotFound, "令牌不存在")
			return
		}
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "保存代理凭据映射失败")
		return
	}
	resp.OK(w, r, keyBindingView{TokenID: k.TokenID, KeyName: k.KeyName, State: k.State, Bound: true, HasKeyID: k.KeyID != ""})
}

// HandleDelete DELETE /api/v1/admin/tokens/{id}/magpie-key （停用，不删行）
func (h *KeyAdminHandler) HandleDelete(w http.ResponseWriter, r *http.Request) {
	tokenID, ok := h.tokenID(w, r)
	if !ok {
		return
	}
	if err := h.store.Disable(r.Context(), tokenID); err != nil {
		if errors.Is(err, ErrKeyNotProvisioned) {
			resp.Err(w, r, http.StatusNotFound, resp.CodeNotFound, "该令牌没有代理凭据映射")
			return
		}
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "停用代理凭据失败")
		return
	}
	resp.OK(w, r, keyBindingView{TokenID: tokenID, State: KeyStateDisabled, Bound: true})
}

// tokenID 解析路径里的令牌 id。
func (h *KeyAdminHandler) tokenID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ID 参数无效")
		return 0, false
	}
	return id, true
}

// isUniqueKeyName 判定 key_name 唯一约束冲突（另一令牌已占用同名 callerKey）。
func isUniqueKeyName(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}

// isForeignKeyViolation 判定 token_id 外键失败（令牌不存在）。
func isForeignKeyViolation(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23503"
	}
	return false
}

// rollbackLister 是回滚端点需要的附加存储能力（*KeyStore 满足；handler 侧类型升级获取）。
type rollbackLister interface {
	RenameRollbackTargets(ctx context.Context, limit int) ([]Key, error)
}

// rollbackTargetView 回滚目标的只读预览行（dry-run 与执行响应共用）。
type rollbackTargetView struct {
	TokenID     int64  `json:"TokenID,string"`
	CurrentName string `json:"CurrentName"`
	LegacyName  string `json:"LegacyName"`
}

// rollbackResultView 回滚响应。
type rollbackResultView struct {
	DryRun   bool                 `json:"DryRun"`
	Targets  []rollbackTargetView `json:"Targets"`
	Restored int                  `json:"Restored"`
	Skipped  int                  `json:"Skipped"`
	Failed   []map[string]any     `json:"Failed"`
}

// HandleRenameRollback POST /api/v1/admin/tokens/magpie-keys/rename-rollback
// 按 legacy_key_name 复原旧名（终表 §3.5，与正向改名三步镜像、幂等可重跑）。
// dry-run 默认开启：只列出目标（当前名/旧名），?dry_run=false 或请求体 {"DryRun":false} 执行。
// 执行语义：逐令牌 RollbackToken——代理侧旧名复原（值不变）→ 本地行 key_name=旧名 → legacy 清空。
func (h *KeyAdminHandler) HandleRenameRollback(w http.ResponseWriter, r *http.Request) {
	dryRun := true // 默认只判定不执行
	if v := strings.TrimSpace(r.URL.Query().Get("dry_run")); v != "" {
		dryRun = !strings.EqualFold(v, "false") && v != "0"
	}
	if r.Body != nil {
		var req struct {
			DryRun *bool `json:"DryRun"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
			return
		} else if req.DryRun != nil {
			dryRun = *req.DryRun
		}
	}
	rs, ok := h.store.(rollbackLister)
	if !ok {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "存储不支持回滚清单查询")
		return
	}
	if !dryRun && (h.sync == nil || len(h.credKey) == 0) {
		resp.Err(w, r, http.StatusServiceUnavailable, resp.CodeInternalError, "未装配代理凭据同步（缺页面口令或 SM4）")
		return
	}
	const limit = 5000 // 一轮上限；更大规模分轮跑（幂等，重跑续上）
	keys, err := rs.RenameRollbackTargets(r.Context(), limit)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询回滚目标失败")
		return
	}
	view := rollbackResultView{DryRun: dryRun, Targets: []rollbackTargetView{}, Failed: []map[string]any{}}
	for _, k := range keys {
		view.Targets = append(view.Targets, rollbackTargetView{TokenID: k.TokenID, CurrentName: k.KeyName, LegacyName: k.LegacyKeyName})
		if dryRun {
			continue
		}
		done, rerr := h.sync.RollbackToken(r.Context(), k.TokenID)
		switch {
		case rerr != nil:
			view.Failed = append(view.Failed, map[string]any{"TokenID": k.TokenID, "Reason": rerr.Error()})
		case done:
			view.Restored++
		default:
			view.Skipped++ // 无旧名可复原（并发下已回滚）：幂等跳过
		}
	}
	resp.OK(w, r, view)
}
