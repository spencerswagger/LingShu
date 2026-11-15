// upstream.go 是 V2 转发主链（D1/F5 裁定的落点）。
//
// 职责划分**必须**保持这样，别把账务搬进 magpie：
//
//	主链（本文件）    鉴权 → 模型准入/绑定解析 → 预扣 → 调 magpie 转发 → 结算落账单
//	magpie 包        协议识别、path/body 里的 model 改写、头策略、字节转发、usage 旁路读取
//
// 两个钩子是唯一的交接点：
//   - OnUpstreamResponse：回程头到达（归因定稿）→ 主链记下来，落账单时带上；
//   - OnDone：转发结束（usage/状态定稿）→ 主链在这里结算。
//
// 本文件的账单口径沿用既有网关（sale 模式按对外模型售价、PreConsumed 只结算差额），
// cost 模式与七列归因读取留给阶段二（已在交付说明里登记为未完成项）。
package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/magpie"
	"github.com/team/llmgateway/internal/domain/model"
	"github.com/team/llmgateway/internal/domain/upstream"
	"github.com/team/llmgateway/internal/pkg/logger"
)

// RuntimeConfigSource 读上游三键运行参数（upstream.ConfigProvider 满足）。
//
// 转发链**只读 sys_configs 的这三键**（F1），不读任何应用级配置、也不读凭据。
type RuntimeConfigSource interface {
	Load(ctx context.Context) (upstream.RuntimeConfig, []string)
}

// 对外错误文案（不含上游产品名、不含内部术语；§10-F 红线）。
const (
	msgModelNotAvailable   = "模型不存在或未开放"
	msgPassthroughOff      = "转发服务当前不可用，请稍后重试"
	msgUpstreamTimeout     = "上游响应超时"
	msgUpstreamUnreached   = "上游服务不可达"
	msgCreditsInsufficient = "积分不足"
)

// upstreamHandler 返回 V2 转发入口（挂在 /v1、/v1beta 下）。
func (g *Gateway) upstreamHandler() http.Handler {
	mux := http.NewServeMux()
	// /v1/models 保留：客户端靠它列可用模型（只列启用且已绑定的）。
	mux.HandleFunc("GET /v1/models", g.handleModelsV2)
	// 其余入站路径统一进 serveUpstream，由 magpie.Classify 判定协议族：
	// 放开成「任意 path 都转发」等于把灵枢变成开放代理，所以未识别的一律 404。
	mux.HandleFunc("POST /v1/", g.serveUpstream)
	mux.HandleFunc("POST /v1beta/", g.serveUpstream)
	return mux
}

// handleModelsV2 列启用且已绑定的对外模型。
//
// 与 V1 的 /v1/models 差异：多过滤一条「已绑定」（bound_internal_ref 非空）——
// 未绑定的模型转不出去，列出来只会让客户端调用 404。
func (g *Gateway) handleModelsV2(w http.ResponseWriter, r *http.Request) {
	if g.listEnabled == nil {
		writeJSONList(w, nil)
		return
	}
	models, err := g.listEnabled(r.Context())
	if err != nil {
		g.logger.Error("list enabled models", "err", err)
		writeUpstreamError(w, magpie.ProtocolOpenAIChat, http.StatusInternalServerError, msgInternal)
		return
	}
	out := make([]map[string]any, 0, len(models))
	for _, m := range models {
		if !m.Enabled || m.BoundInternalRef == "" {
			continue
		}
		out = append(out, map[string]any{
			"id": m.ExternalName, "object": "model", "owned_by": "lingshu",
		})
	}
	writeJSONList(w, out)
}

// serveUpstream 是转发主链。
func (g *Gateway) serveUpstream(w http.ResponseWriter, r *http.Request) {
	start := g.now()
	ctx := r.Context()

	route, err := magpie.Classify(r.Method, r.URL.Path)
	if err != nil {
		writeUpstreamError(w, magpie.ProtocolOpenAIChat, http.StatusNotFound, "接口路径不存在")
		return
	}

	// 1) 运行参数（三键）与降级开关。
	if g.upstreamCfg == nil {
		writeUpstreamError(w, route.Protocol, http.StatusServiceUnavailable, msgPassthroughOff)
		return
	}
	cfg, problems := g.upstreamCfg.Load(ctx)
	for _, p := range problems {
		g.logFor(ctx).Warn("上游配置有问题（已按字段回落）", "problem", p)
	}
	if !cfg.PassthroughActive() || cfg.BaseURL == "" {
		writeUpstreamError(w, route.Protocol, http.StatusServiceUnavailable, msgPassthroughOff)
		return
	}

	// 2) 鉴权：Bearer / x-api-key / x-goog-api-key / ?key=（Gemini 原生写法）。
	plain, fromQuery := extractInboundToken(r)
	// F6（安全默认）：任何路径上出现 `key` 参数都在转发前剔除并留一条 WARN。
	// 告警的判据是「参数存在」，不是「这次用它鉴权了」——用 header 鉴权却把令牌挂在
	// query 上的调用方同样会把令牌带出去，这种最该被看见。
	if magpie.HasQueryParam(r.URL.RawQuery, "key") {
		g.logFor(ctx).Warn("请求 query 带 key 参数，已剔除、不向上游透传（租户令牌不得透传到上游）",
			"path", r.URL.Path, "auth_from_query", fromQuery)
	}
	if plain == "" {
		writeUpstreamError(w, route.Protocol, http.StatusUnauthorized, msgInvalidToken)
		return
	}
	token, err := g.tokens.LookupByPlain(ctx, plain)
	if err != nil {
		writeUpstreamError(w, route.Protocol, http.StatusUnauthorized, msgInvalidToken)
		return
	}
	if g.touchLastUsed != nil {
		go func(id int64) { _ = g.touchLastUsed(context.Background(), id) }(token.ID)
	}

	// 3) 读请求体（除 model 外逐字节转发，所以必须整段读进来）。
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || int64(len(body)) > maxBodyBytes {
		writeUpstreamError(w, route.Protocol, http.StatusBadRequest, "请求体过大或无法读取")
		return
	}
	if err := requireJSONObject(body); err != nil {
		writeUpstreamError(w, route.Protocol, http.StatusBadRequest, "请求体格式错误")
		return
	}

	// 4) model 解析：Gemini 在 path 里，其余在 body 里。
	modelName := magpie.PathModel(r.URL.Path)
	if !route.ModelInPath {
		modelName, err = magpie.ModelFromJSON(body)
		if err != nil || modelName == "" {
			writeUpstreamError(w, route.Protocol, http.StatusBadRequest, "请求体必须包含 model 字段")
			return
		}
	}
	ext, err := g.models.GetByExternalName(ctx, modelName)
	if err != nil || ext == nil || !ext.Enabled || ext.BoundInternalRef == "" {
		writeUpstreamError(w, route.Protocol, http.StatusNotFound, msgModelNotAvailable)
		return
	}

	// 5) 改写：只换 model 这一处（body 的字节区间 / Gemini 的 path 段）。
	upstreamPath := r.URL.Path
	forwardBody := body
	if route.ModelInPath {
		upstreamPath, err = magpie.RewritePathModel(r.URL.Path, ext.BoundInternalRef)
		if err != nil {
			writeUpstreamError(w, route.Protocol, http.StatusBadRequest, "模型名非法")
			return
		}
	} else {
		forwardBody, err = magpie.RewriteModelJSON(body, ext.BoundInternalRef)
		if err != nil {
			writeUpstreamError(w, route.Protocol, http.StatusBadRequest, "模型名非法")
			return
		}
	}

	// 6) 预扣（主链职责；magpie 碰不到钱）。
	sessionID := strings.TrimSpace(r.Header.Get("x-session-id"))
	var pre float64
	if route.Billable && g.credit != nil {
		pc, _, perr := g.estimatePreConsumeCost(ctx, modelName, body)
		if perr != nil {
			g.logFor(ctx).Warn("预扣估算失败", "model", modelName, "err", perr)
		}
		if pc.Credits > 0 {
			if cerr := g.credit.PreConsume(ctx, token.UserID, pc.Credits, "", sessionID, "预扣费"); cerr != nil {
				writeUpstreamError(w, route.Protocol, http.StatusPaymentRequired, msgCreditsInsufficient)
				return
			}
			pre = pc.Credits
		}
	}

	// 7) 转发（magpie 上手，主链只收事实）。
	reqID := logger.GetRequestID(ctx)
	var facts magpie.ResponseFacts
	settled := false
	res := g.forwarder().Forward(ctx, w, magpie.ForwardRequest{
		BaseURL:            cfg.BaseURL,
		Path:               upstreamPath,
		Query:              r.URL.RawQuery,
		Header:             r.Header.Clone(),
		Body:               forwardBody,
		Streaming:          route.StreamHint || (!route.ModelInPath && magpie.StreamRequested(body)),
		Credential:         g.upstreamCredential,
		RequestID:          reqID,
		Billable:           route.Billable,
		Protocol:           route.Protocol,
		OnUpstreamResponse: func(_ context.Context, f magpie.ResponseFacts) { facts = f },
		OnComplete: func(callbackCtx context.Context, r magpie.Result) {
			if settled {
				return
			}
			settled = true
			g.settleV2(callbackCtx, settleInput{
				token: token, model: ext, modelName: modelName, route: route,
				pre: pre, sessionID: sessionID, facts: facts, result: r, start: start,
				body: body, requestID: reqID,
			})
		},
	}, timeoutsOf(cfg))

	// 8) 传输失败：还没写下游响应，这里定状态码（504 超时 / 502 其余）。
	if res.Err != nil && res.Status == 0 {
		status := http.StatusBadGateway
		msg := msgUpstreamUnreached
		if res.TimedOut {
			status = http.StatusGatewayTimeout
			msg = msgUpstreamTimeout
		}
		writeUpstreamError(w, route.Protocol, status, msg)
	}
}

// settleInput 是结算所需的全部事实（避免结算函数一长串参数）。
type settleInput struct {
	token     *identity.Token
	model     *model.ExternalModel
	modelName string
	route     magpie.Route
	pre       float64
	sessionID string
	facts     magpie.ResponseFacts
	result    magpie.Result
	start     time.Time
	body      []byte
	requestID string
}

// settleV2 按转发事实结算：成功按实际 usage（缺 usage 则估算），失败退预扣 + 落 failed 账单。
func (g *Gateway) settleV2(ctx context.Context, in settleInput) {
	if !in.route.Billable || g.billing == nil {
		return
	}
	// 记账脱离请求取消：上游调用已发生，不能因为下游断开就丢账。
	ctx, cancel := billingCtx(ctx)
	defer cancel()

	res := in.result
	delivered := res.Err == nil && res.Status > 0 && res.Status < 400 && !res.NoContent
	if !delivered {
		// 失败：先退预扣（钱不能吞），再落一条 failed 账单留痕（credits=0）。
		if in.pre > 0 && g.credit != nil {
			g.refundPreConsume(ctx, in.token.UserID, in.pre, "请求失败退回预扣")
		}
		reason := "上游失败"
		if res.TimedOut {
			reason = "上游响应超时"
		} else if res.Err != nil {
			reason = "上游转发失败：" + res.Err.Error()
		} else if res.Status >= 400 {
			reason = fmt.Sprintf("上游返回 %d", res.Status)
		} else if res.NoContent {
			reason = fmt.Sprintf("上游无内容（%d）", res.Status)
		}
		g.recordV2(ctx, in, billing.Usage{}, 0, "", reason, true)
		return
	}

	usage := billing.Usage{Input: res.Usage.Input, CacheRead: res.Usage.CacheRead, Output: res.Usage.Output}
	estimated := false
	remark := ""
	if !res.HasUsage {
		// 无 usage：按请求体估输入、按已见生成内容估输出（口径同既有网关），并在 remark 里点明。
		in1, err := countPromptTokens(in.body, in.modelName)
		if err != nil {
			in1 = estTokens(in.body)
		}
		usage = billing.Usage{Input: int64(in1), Output: int64(res.ObservedTextRunes / 2)}
		estimated = true
		remark = "上游未返回 usage，按估算计费"
	}
	g.recordV2(ctx, in, usage, in.pre, remark, "", estimated)
}

// recordV2 落一条账单（成功或失败），返回 credits（失败为 0）。
//
// 失败可诊断：Record 失败时**退预扣**再返回——预扣是真扣过的钱，
// 结算失败还把钱吞掉就是资损（对齐既有网关「计费失败不赊账、也不私吞」口径）。
func (g *Gateway) recordV2(ctx context.Context, in settleInput, usage billing.Usage,
	pre float64, remark, failReason string, estimated bool) {
	rec := billing.RecordReq{
		UserID:           in.token.UserID,
		TokenID:          &in.token.ID,
		PricingMode:      identity.PricingModeSale,
		ExternalModel:    in.modelName,
		InternalModelID:  in.model.BoundInternalRef,
		SessionID:        in.sessionID,
		CallTime:         in.start,
		Tokens:           usage,
		Rates:            in.model.SaleRates,
		TimeConfig:       in.model.TimeConfig,
		ContextTiers:     in.model.ContextTiers,
		PreConsumed:      pre,
		DurationMs:       msSince(in.start),
		FirstTokenMs:     in.result.FirstByteMs,
		Remark:           remark,
		RequestID:        in.requestID,
		UpstreamProvider: in.result.Boundary.Provider,
		UpstreamAccount:  in.result.Boundary.Account,
		UpstreamProtocol: string(in.route.Protocol),
		UpstreamModel:    in.result.Boundary.Model,
	}
	if estimated {
		// 估算单必须可区分：对账要把它们单独成组，不能混进真实 token 口径。
		rec.CostSource = billing.CostSourceNone
	}
	if failReason != "" {
		rec.Fail = true
		rec.ErrorMessage = failReason
	}
	if _, err := g.billing.Record(ctx, rec); err != nil {
		g.logFor(ctx).Error("账单写入失败", "model", in.modelName, "err", err)
		if pre > 0 && g.credit != nil {
			g.refundPreConsume(ctx, in.token.UserID, pre, "账单写入失败退回预扣")
		}
	}
}

// upstreamCredential 在出站请求上注入上游鉴权头。
//
// 凭据只在装配层（main）持有，经函数注入到这里：不进配置表、不进日志、不进 magpie 字段。
// 未注入时按「上游不需要凭据」处理（本地假上游/内网直连场景）。
func (g *Gateway) upstreamCredential(h http.Header) {
	if g.upstreamAuth == nil {
		return
	}
	name, value := g.upstreamAuth()
	if name == "" || value == "" {
		return
	}
	h.Set(name, value)
}

// forwarder 返回转发执行器（懒建：V1 路径不碰它）。
func (g *Gateway) forwarder() *magpie.Forwarder {
	if g.fwd == nil {
		g.fwd = magpie.NewForwarder()
	}
	return g.fwd
}

// extractInboundToken 按四种入站写法取调用令牌。
//
// fromQuery=true 表示本次鉴权用的是 `?key=`（Gemini 原生写法）——
// 转发时必须把它从 query 里剔掉，否则把租户令牌带给上游（F6）。
func extractInboundToken(r *http.Request) (plain string, fromQuery bool) {
	if p, ok := bearerPlain(r.Header.Get("Authorization")); ok && p != "" {
		return p, false
	}
	if p := strings.TrimSpace(r.Header.Get("x-api-key")); p != "" {
		return p, false
	}
	if p := strings.TrimSpace(r.Header.Get("x-goog-api-key")); p != "" {
		return p, false
	}
	if p := strings.TrimSpace(r.URL.Query().Get("key")); p != "" {
		return p, true
	}
	return "", false
}

// writeUpstreamError 按入站协议写错误体。
//
// 为什么分形状：Gemini/Anthropic 的 SDK 只认自己那套错误结构，
// 统一发 OpenAI 形状会让它们的报错变成「无法解析」而不是我们的文案。
func writeUpstreamError(w http.ResponseWriter, p magpie.Protocol, status int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	var payload any
	switch p {
	case magpie.ProtocolAnthropic:
		payload = map[string]any{"type": "error", "error": map[string]any{"type": "api_error", "message": msg}}
	case magpie.ProtocolGemini:
		payload = map[string]any{"error": map[string]any{"code": status, "message": msg, "status": http.StatusText(status)}}
	default:
		payload = map[string]any{"error": map[string]any{"message": msg, "type": "api_error"}}
	}
	_ = json.NewEncoder(w).Encode(payload)
}

// writeJSONList 写模型列表（OpenAI 形状）。
func writeJSONList(w http.ResponseWriter, items []map[string]any) {
	if items == nil {
		items = []map[string]any{}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": items})
}

// timeoutsOf 把运行参数里的两段超时转成转发链口径（F1：只有这两段，没有第三个总超时）。
func timeoutsOf(cfg upstream.RuntimeConfig) magpie.Timeouts {
	first := cfg.Timeouts.StreamFirstByte
	idle := cfg.Timeouts.StreamIdle
	// 兜底：配置读不到时不能退化成「永不超时」（默认值与 upstream 包同源）。
	if first <= 0 {
		first = upstream.DefaultStreamFirstByte
	}
	if idle <= 0 {
		idle = upstream.DefaultStreamIdle
	}
	return magpie.Timeouts{StreamFirstByte: first, StreamIdle: idle}
}

// logFor 取带请求 ID 的 logger；未装配 logger 时回落 slog 默认（避免 nil panic）。
func (g *Gateway) logFor(ctx context.Context) *slog.Logger {
	base := g.logger
	if base == nil {
		base = slog.Default()
	}
	return logger.WithRequestID(ctx, base)
}
