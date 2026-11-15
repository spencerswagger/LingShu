// admission.go 是准入层：灵枢只决定「这个租户/令牌能不能用这个模型」，
// 不决定「走哪个渠道」（选渠道与 failover 归 magpie）。
//
// 放行三条同时成立：
//  1. external_models.enabled = true（模型对租户开放）；
//  2. magpie_model 非空（已映射到代理模型，否则无从转发）；
//  3. magpie_model 不命中订阅池前缀（Claude / ChatGPT 订阅类渠道默认不对租户开放）。
//
// 第三条为什么放在灵枢白名单层（而不是靠代理侧配置或自造的拒绝头）：
//   - magpie 没有「拒绝订阅池」的请求头，我方自造的头会被它静默忽略 = 假安全；
//   - 全靠代理侧 callerKey 的 scope 等于把安全边界交给外部配置，漏配即全开。
//
// 前缀名单存 sys_configs['magpie.subscription_prefixes']（seeding 写默认值），
// 读失败时回落到代码内默认名单——失败方向是「仍然拦截」，不是「全放」。
package magpie

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/model"
)

// SubscriptionPrefixesKey 是订阅池前缀名单在 sys_configs 里的键。
const SubscriptionPrefixesKey = "magpie.subscription_prefixes"

// DefaultSubscriptionPrefixes 是代码内默认名单（magpie 把订阅模型以 provider 前缀暴露在选择器里）。
var DefaultSubscriptionPrefixes = []string{
	"claude/",
	"codex/",
	"copilot/",
	"gemini-code-assist/",
	"antigravity/",
	"grok/",
}

// TokenLookup 是准入层需要的令牌能力（identity.TokenService 满足）。
type TokenLookup interface {
	LookupByPlain(ctx context.Context, plain string) (*identity.Token, error)
	TouchLastUsed(ctx context.Context, tokenID int64) error
}

// ModelLookup 是准入层需要的对外模型能力（model.Service 满足）。
type ModelLookup interface {
	GetByExternalName(ctx context.Context, name string) (*model.ExternalModel, error)
	ListModels(ctx context.Context, enabled *bool) ([]model.ExternalModel, error)
}

// KeyLookup 是 callerKey 映射能力（*KeyStore 满足）。
type KeyLookup interface {
	ActiveByTokenID(ctx context.Context, tokenID int64) (*Key, error)
}

// ConfigGetter 读取 sys_configs 的 JSON 值（billing.SqlStore.GetConfig 满足）。
type ConfigGetter func(ctx context.Context, key string) ([]byte, error)

// PrefixProvider 缓存订阅池前缀名单（sys_configs 低频变更；写入后由 hook 失效）。
type PrefixProvider struct {
	get ConfigGetter

	mu     sync.RWMutex
	cached []string
	loaded bool
}

// NewPrefixProvider 创建前缀名单读取器；get 为 nil 时恒用默认名单。
func NewPrefixProvider(get ConfigGetter) *PrefixProvider {
	return &PrefixProvider{get: get}
}

// Invalidate 清空缓存（配置写入后调用，保证下一请求即生效）。
func (p *PrefixProvider) Invalidate() {
	p.mu.Lock()
	p.loaded = false
	p.cached = nil
	p.mu.Unlock()
}

// Prefixes 返回当前生效的前缀名单（读失败/未配置 → 默认名单，失败方向是「继续拦截」）。
func (p *PrefixProvider) Prefixes(ctx context.Context) []string {
	p.mu.RLock()
	if p.loaded {
		out := p.cached
		p.mu.RUnlock()
		return out
	}
	p.mu.RUnlock()

	list := DefaultSubscriptionPrefixes
	if p.get != nil {
		raw, err := p.get(ctx, SubscriptionPrefixesKey)
		if err == nil && len(raw) > 0 {
			var parsed []string
			if json.Unmarshal(raw, &parsed) == nil && len(parsed) > 0 {
				list = normalizePrefixes(parsed)
			}
		}
	}
	p.mu.Lock()
	p.cached, p.loaded = list, true
	p.mu.Unlock()
	return list
}

// normalizePrefixes 去空白、去空项、转小写（模型名比较按前缀且大小写不敏感）。
func normalizePrefixes(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		v = strings.ToLower(strings.TrimSpace(v))
		if v != "" {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return DefaultSubscriptionPrefixes
	}
	return out
}

// isSubscriptionModel 判定代理模型名是否属于订阅池（默认不对租户开放）。
//
// 匹配是「按族」而不是「按字面前缀」：代理侧同一个订阅族既有 `claude/xxx` 写法，
// 也有不带 provider 段的 `claude-sonnet-4`、`codex-latest`。只看字面前缀会让后者绕过。
// 规则（族名 = 名单项去掉尾部 '/'）：
//   - 带 provider 段时只看**第一段**：等于族名或以「族名-」「族名.」开头（`claude/claude-sonnet-4`）；
//   - 不带 '/' 时看整串：等于族名或以「族名-」「族名.」开头（`claude-sonnet-4`、`codex-latest`）。
//
// 反例守卫（不得误拦）：`anthropic/claude-sonnet-4`（API 渠道，不是订阅池）、
// `gemini/gemini-2.0-flash`（Gemini 官方渠道）、`claudette-x`（同前缀的别的模型）。
func isSubscriptionModel(magpieModel string, prefixes []string) bool {
	m := strings.ToLower(strings.TrimSpace(magpieModel))
	if m == "" {
		return false
	}
	head := m
	if i := strings.IndexByte(m, '/'); i >= 0 {
		head = m[:i] // 带 provider 段时只看第一段（订阅池按 provider 暴露）
	}
	for _, raw := range prefixes {
		p := strings.ToLower(strings.TrimSpace(raw))
		if p == "" {
			continue
		}
		if strings.HasPrefix(m, p) {
			return true
		}
		root := strings.TrimSuffix(p, "/")
		if root == "" {
			continue
		}
		if head == root || strings.HasPrefix(head, root+"-") || strings.HasPrefix(head, root+".") {
			return true
		}
	}
	return false
}

// Decision 是准入通过后的转发所需事实。
type Decision struct {
	Token *identity.Token
	// External 是命中的对外模型（售价/计费口径在第③件使用）。
	External *model.ExternalModel
	// MagpieModel 是改写进请求的代理模型名。
	MagpieModel string
	// CallerKey 是逐请求注入的代理凭据（明文，仅在内存中短暂存在）。
	CallerKey string
	// KeyName 是 callerKey 名（代理侧用量归集/对账维度）。
	KeyName string
}

// Admitter 组合令牌鉴权、模型白名单与 callerKey 解析。
type Admitter struct {
	tokens   TokenLookup
	models   ModelLookup
	keys     KeyLookup
	credKey  []byte
	prefixes *PrefixProvider
}

// NewAdmitter 装配准入层。
func NewAdmitter(tokens TokenLookup, models ModelLookup, keys KeyLookup, credKey []byte, prefixes *PrefixProvider) *Admitter {
	return &Admitter{tokens: tokens, models: models, keys: keys, credKey: credKey, prefixes: prefixes}
}

// ExtractPlainToken 取调用凭据明文：Authorization: Bearer 优先，兼容 Anthropic 的 x-api-key
// 与 Gemini 原生的 x-goog-api-key。
func ExtractPlainToken(h http.Header) string {
	if v, ok := strings.CutPrefix(strings.TrimSpace(h.Get("Authorization")), "Bearer "); ok {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	for _, key := range []string{"x-api-key", "x-goog-api-key"} {
		if v := strings.TrimSpace(h.Get(key)); v != "" {
			return v
		}
	}
	return ""
}

// ExtractRequestToken 取调用凭据，并报告是否来自 query（Gemini 原生 ?key=）。
//
// fromQuery=true 时调用方**必须**把该参数从转发查询串里剔除：否则租户令牌会被带到上游。
//
// 键名判定必须与剔除（stripQueryParam）走同一套解码口径：用 net/url 的 Query() 判定、
// 按原始字面剔除，会让 `?ke%79=<令牌>` 这种编码变体「判定带了、实际剔不掉」（回归 P1-1）。
func ExtractRequestToken(r *http.Request) (plain string, fromQuery bool) {
	if v := ExtractPlainToken(r.Header); v != "" {
		return v, false
	}
	if v, ok := queryParamValue(r.URL.RawQuery, "key"); ok {
		if v = strings.TrimSpace(v); v != "" {
			return v, true
		}
	}
	return "", false
}

// Authenticate 只做令牌鉴权（GET /v1/models 这类不需要模型准入的端点使用）。
func (a *Admitter) Authenticate(ctx context.Context, plainToken string) (*identity.Token, *Deny) {
	if plainToken == "" {
		return nil, &Deny{Status: http.StatusUnauthorized, Code: CodeInvalidAPIKey, Message: "缺少调用凭据（Authorization: Bearer 或 x-api-key）"}
	}
	tok, err := a.tokens.LookupByPlain(ctx, plainToken)
	if err != nil {
		return nil, &Deny{Status: http.StatusUnauthorized, Code: CodeInvalidAPIKey, Message: "调用令牌无效或已过期"}
	}
	go func(id int64) { _ = a.tokens.TouchLastUsed(context.WithoutCancel(ctx), id) }(tok.ID)
	return tok, nil
}

// Admit 执行准入判定（令牌 → 模型白名单 → 订阅池 → callerKey）。
// 返回 (decision, nil) 为放行；(*Deny) 非空为拒绝（调用方直接按协议形状写出）。
//
// 转发入口不要用它：转发必须**先鉴权再读请求体**，所以入口先 Authenticate 再调 AdmitToken，
// 否则未鉴权的调用方就能把请求体灌进内存（回归 P1-1）。
func (a *Admitter) Admit(ctx context.Context, plainToken, inboundModel string) (*Decision, *Deny) {
	tok, deny := a.Authenticate(ctx, plainToken)
	if deny != nil {
		return nil, deny
	}
	return a.AdmitToken(ctx, tok, inboundModel)
}

// AdmitToken 用已鉴权的令牌继续做模型白名单 → 订阅池 → callerKey 判定。
func (a *Admitter) AdmitToken(ctx context.Context, tok *identity.Token, inboundModel string) (*Decision, *Deny) {
	if tok == nil {
		return nil, &Deny{Status: http.StatusUnauthorized, Code: CodeInvalidAPIKey, Message: "调用令牌无效或已过期"}
	}
	if strings.TrimSpace(inboundModel) == "" {
		return nil, &Deny{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: "请求缺少 model 字段"}
	}

	ext, err := a.models.GetByExternalName(ctx, inboundModel)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, &Deny{Status: http.StatusNotFound, Code: CodeModelNotFound, Message: "模型不存在或未对租户开放"}
		}
		return nil, &Deny{Status: http.StatusInternalServerError, Code: CodeInternal, Message: "服务内部错误"}
	}
	if !ext.Enabled {
		return nil, &Deny{Status: http.StatusForbidden, Code: CodeModelNotAllowed, Message: "模型未对租户开放"}
	}
	if ext.MagpieModel == nil || strings.TrimSpace(*ext.MagpieModel) == "" {
		return nil, &Deny{Status: http.StatusForbidden, Code: CodeModelNotAllowed, Message: "模型尚未接入代理转发"}
	}
	magpieModel := strings.TrimSpace(*ext.MagpieModel)
	if isSubscriptionModel(magpieModel, a.prefixes.Prefixes(ctx)) {
		return nil, &Deny{Status: http.StatusForbidden, Code: CodeModelNotAllowed, Message: "模型未对租户开放（订阅池渠道默认不开放）"}
	}

	key, err := a.keys.ActiveByTokenID(ctx, tok.ID)
	if err != nil {
		if errors.Is(err, ErrKeyNotProvisioned) {
			return nil, &Deny{Status: http.StatusForbidden, Code: CodeMagpieKeyMissing, Message: "该令牌未开通代理访问"}
		}
		return nil, &Deny{Status: http.StatusInternalServerError, Code: CodeInternal, Message: "服务内部错误"}
	}
	callerKey, err := CallerKey(key, a.credKey)
	if err != nil {
		if errors.Is(err, ErrKeyCipherUnavailable) {
			return nil, &Deny{Status: http.StatusForbidden, Code: CodeMagpieKeyMissing, Message: "该令牌的代理凭据不可用"}
		}
		return nil, &Deny{Status: http.StatusInternalServerError, Code: CodeInternal, Message: "服务内部错误"}
	}

	return &Decision{
		Token:       tok,
		External:    ext,
		MagpieModel: magpieModel,
		CallerKey:   callerKey,
		KeyName:     key.KeyName,
	}, nil
}

// ListTenantModels 返回对租户可见的模型名（GET /v1/models）：
// 与准入同一套口径（enabled + 已映射 + 非订阅池），避免「列出来却调不通」。
func (a *Admitter) ListTenantModels(ctx context.Context) ([]string, error) {
	enabled := true
	models, err := a.models.ListModels(ctx, &enabled)
	if err != nil {
		return nil, fmt.Errorf("list external models: %w", err)
	}
	prefixes := a.prefixes.Prefixes(ctx)
	out := make([]string, 0, len(models))
	for i := range models {
		m := &models[i]
		if m.MagpieModel == nil || strings.TrimSpace(*m.MagpieModel) == "" {
			continue
		}
		if isSubscriptionModel(*m.MagpieModel, prefixes) {
			continue
		}
		out = append(out, m.ExternalName)
	}
	return out, nil
}
