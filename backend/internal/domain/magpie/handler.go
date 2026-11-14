// handler.go 是 /v1（及 Gemini 的 /v1beta）入口：识别协议 → 鉴权 → 准入 → 改写 model → 转发。
//
// 这一层**不做**：选渠道、failover、协议互译、日志落库（call_logs 已停写）、计费扣费（第③件）。
package magpie

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/team/llmgateway/internal/pkg/logger"
)

// maxBodyBytes 是入站请求体上限（与既有网关口径一致，异常/恶意超大请求体兜底）。
const maxBodyBytes = 64 << 20

// Prepared 是准入 + 改写后的转发产物。
type Prepared struct {
	TokenID       int64
	UserID        int64
	ExternalModel string
	MagpieModel   string
	CallerKey     string
	CallerKeyName string
	// UpstreamPath 是改写后的上游 path（Gemini 的 model 段已替换）。
	UpstreamPath string
	// Body 是改写后的请求体（OpenAI/Anthropic 只换 model；Gemini 原样）。
	Body []byte
	// StreamRequested 表示请求体声明了流式（body.stream=true）。
	StreamRequested bool
	// StripQueryKey 表示本次鉴权用的是 query 里的 key（Gemini 原生写法），
	// 转发时该参数必须剔除：否则租户令牌会被带到上游。
	StripQueryKey bool
}

// Deps 是入口装配依赖。
type Deps struct {
	Config Config
	Tokens TokenLookup
	Models ModelLookup
	Keys   KeyLookup
	// CredentialKey 是 magpie_keys.key_cipher 的 SM4 密钥（与 security.sm4_key 同一把）。
	CredentialKey []byte
	// Prefixes 提供订阅池前缀名单；nil 时恒用默认名单。
	Prefixes *PrefixProvider
	// OnComplete 是转发结束回调（第③件接入；nil = 不产生任何计费动作）。
	OnComplete Hook
	Logger     *slog.Logger
}

// Handler 是 /v1 入口。
type Handler struct {
	cfg      Config
	admitter *Admitter
	proxy    *Proxy
	logger   *slog.Logger
}

// NewHandler 装配入口；Config 未配置基址时返回 nil（调用方保持 410 停用口径）。
func NewHandler(d Deps) *Handler {
	cfg := d.Config.normalized()
	if !cfg.Enabled() {
		return nil
	}
	prefixes := d.Prefixes
	if prefixes == nil {
		prefixes = NewPrefixProvider(nil)
	}
	lg := d.Logger
	if lg == nil {
		lg = logger.NewDefault()
	}
	p := NewProxy(cfg)
	if d.OnComplete != nil {
		p.SetOnComplete(d.OnComplete)
	}
	return &Handler{
		cfg:      cfg,
		admitter: NewAdmitter(d.Tokens, d.Models, d.Keys, d.CredentialKey, prefixes),
		proxy:    p,
		logger:   lg,
	}
}

// logFor 取带 requestId 的 logger：优先用请求上下文注入的（真实服务），
// 未注入（单测/嵌入）时回落到装配 logger，避免 nil 解引用把转发打成 500。
func (h *Handler) logFor(ctx context.Context) *slog.Logger {
	l := logger.From(ctx)
	if l == nil {
		l = h.logger
	}
	return logger.WithRequestID(ctx, l)
}

// Mount 把入口挂到 mux 上：
//
//	GET  /v1/models  租户可见模型列表（与准入同一口径）
//	POST /v1/...     四类协议端点（其余方法/路径一律 405/404，不做开放代理）
//	POST /v1beta/... Gemini（model 在 path）
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/models", h.handleModels)
	mux.HandleFunc("/v1/", h.handleProxy)
	mux.HandleFunc("/v1beta/", h.handleProxy)
}

// handleModels 返回租户可见的模型列表（OpenAI list 形状，与既有 /v1/models 兼容）。
func (h *Handler) handleModels(w http.ResponseWriter, r *http.Request) {
	if _, deny := h.admitter.Authenticate(r.Context(), ExtractPlainToken(r.Header)); deny != nil {
		WriteDeny(w, ProtocolOpenAIChat, *deny)
		return
	}
	names, err := h.admitter.ListTenantModels(r.Context())
	if err != nil {
		WriteDeny(w, ProtocolOpenAIChat, Deny{Status: http.StatusInternalServerError, Code: CodeInternal, Message: "服务内部错误"})
		return
	}
	type modelItem struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]modelItem, 0, len(names))
	for _, n := range names {
		data = append(data, modelItem{ID: n, Object: "model", OwnedBy: "lingshu"})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// handleProxy 是四类协议端点的统一入口。
func (h *Handler) handleProxy(w http.ResponseWriter, r *http.Request) {
	route, err := Classify(r.Method, r.URL.Path)
	if err != nil {
		if r.Method != http.MethodPost {
			WriteDeny(w, ProtocolOpenAIChat, Deny{Status: http.StatusMethodNotAllowed, Code: CodeInvalidRequest, Message: "接口仅支持 POST"})
			return
		}
		WriteDeny(w, ProtocolOpenAIChat, Deny{Status: http.StatusNotFound, Code: CodeModelNotFound, Message: "不支持的接口"})
		return
	}
	start := time.Now()

	// 1) 先鉴权、后才读请求体：未鉴权的调用方不得把请求体灌进内存
	// （/v1 无限流，64MB 上限只保护「已鉴权」的调用方）。凭据来源含 Gemini 原生写法
	// （x-goog-api-key 头 / ?key= query）。
	plainToken, fromQuery := ExtractRequestToken(r)
	tok, deny := h.admitter.Authenticate(r.Context(), plainToken)
	if deny != nil {
		h.logFor(r.Context()).Warn("代理鉴权拒绝",
			"endpoint", route.Endpoint, "status", deny.Status, "code", deny.Code)
		WriteDeny(w, route.Protocol, *deny)
		return
	}

	// 2) 读请求体（上限 64MB）。
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || int64(len(body)) > maxBodyBytes {
		WriteDeny(w, route.Protocol, Deny{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: "请求体过大或无法读取"})
		return
	}

	// 3) 取入站 model：OpenAI/Anthropic 在 body，Gemini 在 path。
	inboundModel := ""
	if route.ModelInPath {
		inboundModel = pathModel(r.URL.Path)
	} else {
		m, err := ModelFromJSON(body)
		if err != nil {
			WriteDeny(w, route.Protocol, Deny{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: "请求体必须包含 model 字段"})
			return
		}
		inboundModel = m
	}

	// 4) 准入：模型白名单 → 订阅池 → callerKey（令牌已在上一步鉴权）。
	dec, deny := h.admitter.AdmitToken(r.Context(), tok, inboundModel)
	if deny != nil {
		h.logFor(r.Context()).Warn("代理准入拒绝",
			"endpoint", route.Endpoint, "model", inboundModel, "status", deny.Status, "code", deny.Code)
		WriteDeny(w, route.Protocol, *deny)
		return
	}

	// 5) 只改 model（其余字节原样）。
	prep := Prepared{
		TokenID:       dec.Token.ID,
		UserID:        dec.Token.UserID,
		ExternalModel: dec.External.ExternalName,
		MagpieModel:   dec.MagpieModel,
		CallerKey:     dec.CallerKey,
		CallerKeyName: dec.KeyName,
		UpstreamPath:  route.UpstreamPath,
		Body:          body,
		StripQueryKey: fromQuery,
	}
	if route.ModelInPath {
		p, err := rewritePathModel(r.URL.Path, dec.MagpieModel)
		if err != nil {
			WriteDeny(w, route.Protocol, Deny{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: "请求路径不合法"})
			return
		}
		prep.UpstreamPath = p
	} else {
		rewritten, err := RewriteModelJSON(body, dec.MagpieModel)
		if err != nil {
			WriteDeny(w, route.Protocol, Deny{Status: http.StatusBadRequest, Code: CodeInvalidRequest, Message: "请求体必须包含 model 字段"})
			return
		}
		prep.Body = rewritten
		prep.StreamRequested = StreamRequested(body)
	}

	// 6) 转发（响应字节原样回流）。
	res := h.proxy.Forward(w, r, route, prep)
	if res.Err != nil {
		h.logFor(r.Context()).Warn("代理转发失败",
			"endpoint", route.Endpoint, "model", prep.MagpieModel,
			"status", res.Status, "canceled", res.Canceled, "timed_out", res.TimedOut,
			"duration_ms", time.Since(start).Milliseconds(), "err", res.Err.Error())
	}
}
