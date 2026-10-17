package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptrace"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/pkg/logger"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// maxBodyBytes 下游请求体大小的兜底上限（64MB）。
// 它不是业务限制：1M 上下文的长请求很容易超过 5MB，因此这里只做「异常/恶意超大请求体」
// 的兜底拦截，避免 io.ReadAll 无界读入占满内存。
const maxBodyBytes int64 = 64 << 20

// 下游对外错误文案（不泄漏渠道名/上游地址）。
const (
	msgInvalidToken        = "令牌无效或已过期"
	msgModelNotFound       = "模型 %s 未配置或已停用"
	msgNoRoute             = "当前请求的语义标签无可用的服务渠道"
	msgNoAvailable         = "无可用渠道，请稍后重试"
	msgRateLimited         = "请求过于频繁，请稍后重试"
	msgUpstreamDown        = "上游服务暂时不可用"
	msgInsufficient        = "积分余额不足，请先充值"
	msgInternal            = "服务器内部错误"
	msgUnsupportedProtocol = "该模型不支持当前入站协议（无可直通的上游协议）"
)

// bearerPlain 从 Authorization 头提取 Bearer 令牌明文；非 Bearer 形式返回 ("", false)。
func bearerPlain(auth string) (string, bool) {
	return strings.CutPrefix(auth, "Bearer ")
}

// writeUnsupportedProtocol 写出 AC-4 固定错误：HTTP 400 + OpenAI 标准错误体，
// type=unsupported_protocol、code=invalid_request_error。
func writeUnsupportedProtocol(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusBadRequest)
	_ = json.NewEncoder(w).Encode(openAIErrorBody{Error: openAIErrorDetail{
		Message: msgUnsupportedProtocol,
		Type:    "unsupported_protocol",
		Code:    "invalid_request_error",
	}})
}

// sessionTTLFallback 会话过期兜底时长：resolver 缺失或返回非正值时使用（5 分钟）。
const sessionTTLFallback = 5 * time.Minute

// handleModels GET /v1/models 返回启用中的对外模型列表（OpenAI 兼容格式）。
// 鉴权与 serve 一致：校验 Bearer 令牌；列表为空时返回空 data 而非报错。
func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	auth := r.Header.Get("Authorization")
	plain, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || plain == "" {
		g.logReject(ctx, "models", "", "缺少或非法的调用凭据（Authorization 为空）", http.StatusUnauthorized, nil, nil)
		writeOpenAIError(w, http.StatusUnauthorized, msgInvalidToken)
		return
	}
	token, err := g.tokens.LookupByPlain(ctx, plain)
	if err != nil {
		g.logReject(ctx, "models", "", "调用令牌无效或已过期", http.StatusUnauthorized, nil, nil)
		writeOpenAIError(w, http.StatusUnauthorized, msgInvalidToken)
		return
	}

	data := make([]map[string]any, 0)
	if g.listEnabled != nil {
		list, err := g.listEnabled(ctx)
		if err != nil {
			g.logError(0, "list enabled models", err)
			g.logReject(ctx, "models", "", "枚举启用模型失败："+err.Error(), http.StatusInternalServerError, token, nil)
			writeOpenAIError(w, http.StatusInternalServerError, msgInternal)
			return
		}
		for i := range list {
			if !list[i].Enabled {
				continue
			}
			data = append(data, map[string]any{
				"id":       list[i].ExternalName,
				"object":   "model",
				"created":  list[i].CreatedAt.Unix(),
				"owned_by": "llm-gateway",
			})
		}
	}
	// Auto 模型一并列出（全局 + 该令牌用户的私有），否则客户端下拉看不到 auto 名。
	// Auto 名与对外模型名不允许重名，正常无重复；此处仍做去重兜底。
	if g.listAutoModels != nil && token != nil {
		autos, err := g.listAutoModels(ctx, token.UserID)
		if err != nil {
			// 列 Auto 失败不阻断对外模型列表（降级为仅列对外模型）。
			g.logError(0, "list auto models", err)
		} else {
			seen := make(map[string]bool, len(data))
			for i := range data {
				if id, ok := data[i]["id"].(string); ok {
					seen[id] = true
				}
			}
			for i := range autos {
				if !autos[i].Enabled || seen[autos[i].Name] {
					continue
				}
				seen[autos[i].Name] = true
				data = append(data, map[string]any{
					"id":       autos[i].Name,
					"object":   "model",
					"created":  autos[i].CreatedAt.Unix(),
					"owned_by": "llm-gateway",
				})
			}
		}
	}
	writeOpenAI(w, map[string]any{"object": "list", "data": data})
}

// writeOpenAI 直接写 OpenAI 兼容 JSON（不走统一 resp.Box，保持与官方响应形状一致）。
func writeOpenAI(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

// openAIErrorBody 是 OpenAI 标准错误响应体：{"error":{"message","type","code"}}。
// /v1 对外错误一律用此形状，与内部统一壳 resp.Body（{Code,Message,RequestID,Data}）解耦，
// 避免把内部契约外泄给下游客户端。
type openAIErrorBody struct {
	Error openAIErrorDetail `json:"error"`
}

type openAIErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// writeOpenAIError 直接写出 OpenAI 标准错误体（不走内部统一壳 resp.Box），HTTP 状态码沿用业务语义。
// type/code 依据 HTTP 状态码归类为 OpenAI 语义，确保下游客户端按 OpenAI 规范解析错误。
func writeOpenAIError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(openAIErrorBody{Error: openAIErrorDetail{
		Message: msg,
		Type:    openAIErrorType(status),
		Code:    openAIErrorCode(status),
	}})
}

// openAIErrorType 依据 HTTP 状态码映射 OpenAI 错误 type。
func openAIErrorType(status int) string {
	switch {
	case status == http.StatusTooManyRequests:
		return "rate_limit_error"
	case status == http.StatusPaymentRequired:
		return "insufficient_quota"
	case status >= 500:
		return "server_error"
	default:
		return "invalid_request_error"
	}
}

// openAIErrorCode 依据 HTTP 状态码映射 OpenAI 错误 code（字符串，不泄漏内部业务码）。
func openAIErrorCode(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "invalid_api_key"
	case http.StatusNotFound:
		return "model_not_found"
	case http.StatusPaymentRequired:
		return "insufficient_quota"
	case http.StatusTooManyRequests:
		return "rate_limit_exceeded"
	case http.StatusBadGateway:
		return "upstream_error"
	case http.StatusServiceUnavailable:
		return "service_unavailable"
	default:
		if status >= 500 {
			return "internal_error"
		}
		return "invalid_request_error"
	}
}

// serve 是 /v1/{endpoint} 统一入口的核心热路径。baseProtocol 为入站端点对应的基础协议
// （openai-chat / openai-responses / anthropic-messages），用于直通判定与 Provider 选择。
func (g *Gateway) serve(w http.ResponseWriter, r *http.Request, endpoint, baseProtocol string) {
	start := time.Now()
	ctx := r.Context()
	l := logger.WithRequestID(ctx, g.logger)

	// 1) 认证：Authorization: Bearer <plain token>；兼容 Anthropic 客户端的 x-api-key。
	plain, ok := bearerPlain(r.Header.Get("Authorization"))
	if !ok || plain == "" {
		plain = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	if plain == "" {
		g.logReject(ctx, endpoint, "", "缺少或非法的调用凭据（Authorization/x-api-key 均为空）", http.StatusUnauthorized, nil, msSince(start))
		writeOpenAIError(w, http.StatusUnauthorized, msgInvalidToken)
		return
	}
	token, err := g.tokens.LookupByPlain(ctx, plain)
	if err != nil {
		g.logReject(ctx, endpoint, "", "调用令牌无效或已过期", http.StatusUnauthorized, nil, msSince(start))
		writeOpenAIError(w, http.StatusUnauthorized, msgInvalidToken)
		return
	}
	// 鉴权通过后异步记录最后使用时间，不阻塞热路径。
	if g.touchLastUsed != nil {
		go g.touchLastUsed(context.Background(), token.ID)
	}

	// 2) 读取请求体（上限 maxBodyBytes=64MB，异常/恶意超大请求体兜底）并解析 model / stream。
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBodyBytes+1))
	if err != nil || int64(len(body)) > maxBodyBytes {
		g.logReject(ctx, endpoint, "", "请求体过大或无法读取", http.StatusBadRequest, token, msSince(start))
		writeOpenAIError(w, http.StatusBadRequest, "请求体过大或无法读取")
		return
	}
	if err := requireJSONObject(body); err != nil {
		g.logReject(ctx, endpoint, "", "请求体不是合法 JSON 对象："+err.Error(), http.StatusBadRequest, token, msSince(start))
		writeOpenAIError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	model, isStream, err := parseRequestMeta(body)
	if err != nil || model == "" {
		g.logReject(ctx, endpoint, "", "请求体缺少 model 字段或解析失败", http.StatusBadRequest, token, msSince(start))
		writeOpenAIError(w, http.StatusBadRequest, "请求体必须包含 model 字段")
		return
	}

	// 3) 令牌标签 → TagKV。
	var tagKV map[string]string
	if g.tagKV != nil {
		kv, terr := g.tagKV(ctx, token.TagID)
		if terr != nil {
			l.Error("resolve tag kv failed", "user_id", token.UserID, "tag_id", token.TagID, "err", terr)
			g.logReject(ctx, endpoint, model, "解析令牌标签失败："+terr.Error(), http.StatusInternalServerError, token, msSince(start))
			writeOpenAIError(w, http.StatusInternalServerError, msgInternal)
			return
		}
		tagKV = kv
	}

	// 4) 路由：取全部候选（模型映射 + 标签匹配 + 两层级联状态，会话命中排首）。
	// 会话标识：客户端显式 x-session-id 优先；缺省时按请求体前几条消息 hash 生成隐式会话种子
	//（同一对话前缀稳定 → 同一隐式会话，便于会话列表/账单维度可见，无需客户端额外传参）。
	sessionRaw := r.Header.Get("x-session-id")
	if sessionRaw == "" {
		sessionRaw = implicitSessionSeed(body)
	}
	var sessionID string
	var sessionName string
	if sessionRaw != "" {
		sessionID = router.SessionID(token.UserID, token.ID, model, sessionRaw)
		sessionName = sessionNameFor(sessionRaw, body)
	}
	// Auto 是「对外模型之上的一层有序别名组」：请求 Auto 名时按成员 position 升序，
	// 逐个成员以「成员对外模型名」试路由，第一个产生非空候选的成员即被选中；
	// 之后 model 替换为命中成员名，后续路由/亲和/计费/failover 完全沿用既有链路。
	// requestedModel 保留原始请求名（Auto 名），用于账单 auto_model_name 留痕。
	requestedModel := model
	autoName := ""
	var routes []*router.ChannelRoute
	if g.auto != nil {
		members, isAuto, aerr := g.auto.ResolveMembers(ctx, requestedModel, &token.UserID)
		if aerr != nil {
			l.Error("resolve auto model failed", "model", requestedModel, "err", aerr)
			g.logReject(ctx, endpoint, requestedModel, "解析 Auto 模型失败："+aerr.Error(), http.StatusInternalServerError, token, msSince(start))
			writeOpenAIError(w, http.StatusInternalServerError, msgInternal)
			return
		}
		if isAuto {
			autoName = requestedModel
			model = "" // 未命中任何成员前保持空，避免误用 Auto 名做普通模型解析
			// 老会话粘性（D4）：会话已绑定的成员对外模型名（s.Model）优先，直接按该名路由，
			// 不参考当前成员列表——成员重排/增删都不改变老会话落点，与「会话内粘性不变」一致。
			if sessionID != "" {
				if s, ok := g.router.Sessions().Lookup(sessionID); ok && s != nil && s.Model != "" {
					if sRoutes, sErr := g.router.RouteAll(ctx, router.RouteSpec{
						ExternalModel: s.Model,
						TagKV:         tagKV,
						TagID:         token.TagID,
						SessionKey:    sessionID,
					}); sErr == nil && len(sRoutes) > 0 {
						model = s.Model
						routes = sRoutes
					}
				}
			}
			// 新会话，或原成员对外模型已不可用 → 按成员 position 升序取第一个有可用候选的成员。
			if model == "" {
				for _, mem := range members {
					if mem.ExternalName == "" {
						continue // 成员对应对外模型已软删/不可解析 → 跳过
					}
					mRoutes, mErr := g.router.RouteAll(ctx, router.RouteSpec{
						ExternalModel: mem.ExternalName,
						TagKV:         tagKV,
						TagID:         token.TagID,
						SessionKey:    sessionID,
					})
					if mErr != nil {
						// 单个成员路由失败（如该对外模型未配置）不阻断整体：继续尝试下一成员。
						continue
					}
					if len(mRoutes) > 0 {
						model = mem.ExternalName
						routes = mRoutes
						break
					}
				}
			}
			if model == "" {
				// 全部成员均无可用候选（或成员对外模型均不可解析）→ 无可用渠道。
				// 账单保留 auto 名（external_model_name 为空）便于按 Auto 排查。
				g.recordGatewayReject(ctx, model, autoName, token, "", sessionID, msgNoAvailable)
				g.logReject(ctx, endpoint, autoName, "Auto 模型全部成员均无可用候选："+msgNoAvailable, http.StatusServiceUnavailable, token, msSince(start))
				writeOpenAIError(w, http.StatusServiceUnavailable, msgNoAvailable)
				return
			}
		} else {
			routeSpec := router.RouteSpec{
				ExternalModel: model,
				TagKV:         tagKV,
				TagID:         token.TagID,
				SessionKey:    sessionID,
			}
			routes, err = g.router.RouteAll(ctx, routeSpec)
		}
	} else {
		routeSpec := router.RouteSpec{
			ExternalModel: model,
			TagKV:         tagKV,
			TagID:         token.TagID,
			SessionKey:    sessionID,
		}
		routes, err = g.router.RouteAll(ctx, routeSpec)
	}
	if err != nil {
		var re *router.RouteError
		if errors.As(err, &re) {
			switch re.Kind {
			case router.KindModelNotFound:
				// 未进入渠道也留痕：保证「每次请求必有一条账单记录」（模型未配置/停用）。
				g.recordGatewayReject(ctx, model, autoName, token, "", sessionID, fmt.Sprintf(msgModelNotFound, model))
				g.logReject(ctx, endpoint, model, fmt.Sprintf(msgModelNotFound, model), http.StatusNotFound, token, msSince(start))
				writeOpenAIError(w, http.StatusNotFound, fmt.Sprintf(msgModelNotFound, model))
			case router.KindNoRoute:
				g.recordGatewayReject(ctx, model, autoName, token, "", sessionID, msgNoRoute)
				g.logReject(ctx, endpoint, model, msgNoRoute, http.StatusServiceUnavailable, token, msSince(start))
				writeOpenAIError(w, http.StatusServiceUnavailable, msgNoRoute)
			default: // KindNoAvailable
				g.recordGatewayReject(ctx, model, autoName, token, "", sessionID, msgNoAvailable)
				g.logReject(ctx, endpoint, model, msgNoAvailable, http.StatusServiceUnavailable, token, msSince(start))
				writeOpenAIError(w, http.StatusServiceUnavailable, msgNoAvailable)
			}
			return
		}
		l.Error("route failed", "model", model, "err", err)
		g.logReject(ctx, endpoint, model, "路由失败："+err.Error(), http.StatusInternalServerError, token, msSince(start))
		writeOpenAIError(w, http.StatusInternalServerError, msgInternal)
		return
	}

	// 5) 计费模式（默认 sale）。
	pricingMode := identity.PricingModeSale
	if g.pricing != nil {
		if pm, perr := g.pricing(ctx, token.UserID); perr == nil && pm != "" {
			pricingMode = pm
		} else if perr != nil {
			l.Warn("resolve pricing mode failed, fallback to sale", "user_id", token.UserID, "err", perr)
		}
	}

	// 6) 计费前置（三阶段计费，对齐 new-api pre-consume/settle/refund）：
	// 装配 Credit 时按「输入(tiktoken)+模型输出上限」估算金额做原子预扣，余额不足直接 402；
	// 请求结束后按实际账单退补（多退少补）。zero 余额在任何模式下一律拦截，杜绝赊账。
	var pc preCharge
	if g.credit != nil {
		if bal, berr := g.credit.Balance(ctx, token.UserID); berr == nil && bal <= 0 {
			l.Warn("zero balance rejected", "user_id", token.UserID, "balance", bal)
			g.recordGatewayReject(ctx, model, autoName, token, pricingMode, sessionID, "积分余额不足（账户余额为 0）")
			g.logReject(ctx, endpoint, model, "积分余额不足（账户余额为 0）", http.StatusPaymentRequired, token, msSince(start))
			writeOpenAIError(w, http.StatusPaymentRequired, msgInsufficient)
			return
		}
		est, formula, eerr := g.estimatePreConsumeCost(ctx, model, body)
		if eerr != nil {
			l.Error("estimate preconsume failed", "user_id", token.UserID, "model", model, "err", eerr)
			g.logReject(ctx, endpoint, model, "预扣金额估算失败："+eerr.Error(), http.StatusInternalServerError, token, msSince(start))
			writeOpenAIError(w, http.StatusInternalServerError, msgInternal)
			return
		}
		pc = est
		if pc.Credits > 0 {
			pc.Credits = pc.Credits * preConsumeBuffer // 预扣 = 估算输入代入本地公式 × 缓冲；输出留待结算按实际多退少补
			if err := g.credit.PreConsume(ctx, token.UserID, pc.Credits, "", sessionID, "预扣费"); err != nil {
				// 失败也落 failed 账单：余额不足等预扣拒绝必须留痕（含公式与预扣积分），
				// 便于用户核对「为什么没扣成」与对账。
				msg := fmt.Sprintf("积分余额不足，预扣需 %.6f 积分；公式：\n%s", pc.Credits, formula)
				if len(routes) > 0 {
					r0 := routes[0]
					g.recordFailure(ctx, &router.RouteResult{
						ChannelID:       r0.ChannelID,
						ChannelKeyID:    r0.ChannelKeyID,
						InternalModelID: r0.InternalModelID,
						ModelRowID:      r0.ModelRowID,
						ExternalModelID: r0.ExternalModelID,
						AutoModelName:   autoName,
						Revision:        r0.Revision,
					}, model, token, pricingMode, sessionID, sessionName, msg, nil)
				}
				if isInsufficient(err) {
					l.Warn("preconsume insufficient", "user_id", token.UserID, "need", pc.Credits, "err", err)
					g.logReject(ctx, endpoint, model, fmt.Sprintf("积分余额不足，预扣需 %.6f 积分", pc.Credits), http.StatusPaymentRequired, token, msSince(start))
					writeOpenAIError(w, http.StatusPaymentRequired, msgInsufficient)
				} else {
					l.Error("preconsume failed", "user_id", token.UserID, "need", pc.Credits, "err", err)
					g.logReject(ctx, endpoint, model, "预扣失败："+err.Error(), http.StatusInternalServerError, token, msSince(start))
					writeOpenAIError(w, http.StatusInternalServerError, msgInternal)
				}
				return
			}
		}
	} else if g.balance != nil {
		bal, berr := g.balance(ctx, token.UserID)
		if berr != nil {
			l.Warn("resolve balance failed, skip precheck", "user_id", token.UserID, "err", berr)
		} else if bal <= 0 {
			g.recordGatewayReject(ctx, model, autoName, token, pricingMode, sessionID, "积分余额不足（账户余额为 0）")
			g.logReject(ctx, endpoint, model, "积分余额不足（账户余额为 0）", http.StatusPaymentRequired, token, msSince(start))
			writeOpenAIError(w, http.StatusPaymentRequired, msgInsufficient)
			return
		}
	}

	// 7) 按候选顺序尝试（failover）：
	//    - 会话命中（排首，可排空）→ 续行；新会话 → 该渠道存活会话数校验（并发=存活会话）
	//    - 模型级限流 → 渠道级限流，任一超限 → 下一候选
	//    - 上游失败可安全转移（响应未写出且上游未计费，见 retryableUpstreamStatus / withWriteTrace）→ 下一候选
	//    - 全部候选都失败 → 退还预扣并返回聚合错误
	sess := g.router.Sessions()
	// 会话过期 TTL：按上下文长度（prompt token）与用户对话次数双维度分档取较大者，最低兜底 5 分钟。
	// 仅在有会话标识时计算（Attach/Renew 均在 sessionID != "" 下发生），避免无状态请求白跑 token 计数。
	ttl := sessionTTLFallback
	if sessionID != "" && g.sessionExpiry != nil {
		ctxTokens, cerr := countPromptTokens(body, model)
		if cerr != nil {
			ctxTokens = estTokens(body) // 与限流/计费同一粗略口径
		}
		if v := g.sessionExpiry.TTL(ctxTokens, countUserTurns(body)); v > 0 {
			ttl = v
		}
	}
	sessObj, hasSession := sess.Lookup(sessionID)
	// 已关闭会话视为不存在：不参与续行（走下方 Attach 重建，重新占并发槽），
	// 避免「关闭后同一会话标识仍被服务、且绕过并发上限」。sessObj 保留用于请求增量 diff 的指纹对齐。
	if hasSession && sessObj != nil && sessObj.Closed {
		hasSession = false
	}
	// 请求增量 diff：计算本次请求的增量消息与全量指纹（call_log 采集用，
	// 新会话也在此初始化指纹，由下方 Attach 写入会话）。
	reqMsgs, msgFps := g.captureReqIncrement(sessObj, body)
	sawLimit := false       // 出现过限流类失败（本地限流 或 上游 429）
	sawOther := false       // 出现过非限流类的可重试失败（上游 401/402/403/404/5xx、网络类）
	sawUnsupported := false // 出现过「入站协议 ∉ 模型可达 protocols」（AC-4）
	attempted := false      // 是否至少有一次候选真正发出上游调用（用于全被本地拦截时的账单兜底）
	// 决策轨迹：每个候选渠道的处理结果与跳过原因（call_log decision 用，雪花 ID 以字符串序列化）。
	attempts := make([]map[string]any, 0, len(routes))
	for _, cr := range routes {
		order := len(attempts) + 1
		// 渠道/密钥名：渠道 ID 为雪花值，直接展示不具可读性，随候选一并回查落轨迹。
		chName, keyName := g.channelAndKeyNames(cr.ChannelID, cr.ChannelKeyID)
		attempt := func(reason string) map[string]any {
			return map[string]any{
				"order":             order,
				"channel_id":        strconv.FormatInt(cr.ChannelID, 10),
				"channel_name":      chName,
				"channel_key_id":    strconv.FormatInt(cr.ChannelKeyID, 10),
				"channel_key_name":  keyName,
				"internal_model_id": cr.InternalModelID,
				"reason":            reason,
				// 成本标量 / 是否有效 / 时段系数：仅排查用，不参与计费（OQ-3）。
				"cost":             cr.Cost,
				"cost_known":       cr.CostKnown,
				"route_time_coeff": cr.TimeCoeff,
			}
		}
		// skip 标记"未进入上游即被跳过"的候选：不计入候选失败（否则正常请求会被误判为隐性失败）。
		skip := func(reason string) map[string]any {
			m := attempt(reason)
			m["skipped"] = true
			return m
		}
		// 密钥为运行时实体：路由候选已带 ChannelKeyID，这里做入口复查（状态可能已被灰度切换）。
		// 统一读 Machine.State()：401 自动禁用后立即生效（kr.Key.State 是落库快照，可能滞后）。
		kr, ok := g.channels.GetKeyRuntime(cr.ChannelKeyID)
		if !ok || kr.Machine.State() == channel.StateDisabled {
			attempts = append(attempts, skip("禁用"))
			continue
		}
		mr, mok := g.channels.GetModelRuntime(cr.ChannelID, cr.ModelRowID)
		if !mok {
			attempts = append(attempts, skip("无模型运行时"))
			continue
		}
		if mr.Machine.State() == channel.StateDisabled {
			attempts = append(attempts, skip("禁用"))
			continue
		}
		rt, ok := g.channels.GetRuntime(cr.ChannelID)
		if !ok {
			l.Error("channel not in runtime", "channel_id", cr.ChannelID)
			attempts = append(attempts, skip("无渠道运行时"))
			continue
		}
		// 密钥凭据解密失败/为空时，Manager 已把 CredentialPlain 清空（绝不外发密文）。
		if kr.CredentialPlain == "" {
			l.Error("channel key credential unavailable", "channel_key_id", cr.ChannelKeyID, "channel_id", cr.ChannelID)
			g.logErr(ctx, slog.LevelWarn, "channel key credential unavailable", endpoint, baseProtocol, model, token,
				routeResFor(cr, autoName), diag{}, errInfo{
					kind: "credential_unavailable", stage: "route", attempt: order, attemptsTotal: len(routes),
					reason: "密钥凭据缺失（解密失败或为空），本候选未发上游",
				})
			g.channels.FeedResult(cr.ChannelKeyID, cr.ModelRowID, channel.Feedback{IsSuccess: false, Now: g.now()})
			attempted = true // 该分支已自行落 failed 账单，终态不再兜底
			bid := g.recordFailure(ctx, &router.RouteResult{ChannelID: cr.ChannelID, ChannelKeyID: cr.ChannelKeyID, InternalModelID: cr.InternalModelID,
				ModelRowID: cr.ModelRowID, ExternalModelID: cr.ExternalModelID, AutoModelName: autoName, Revision: cr.Revision},
				model, token, "", sessionID, sessionName, "密钥凭据不可用", msSince(start))
			g.writeCallLog(ctx, bid, "", token, routeResFor(cr, autoName), model, "", sessionID, reqMsgs,
				"error", "", "failed", "密钥凭据不可用", msSince(start), nil, g.callLogDecision(attempts, "密钥凭据不可用", pc, routeResFor(cr, autoName)))
			// 该候选未发上游但属"真实失败"（非跳过）：必须入轨迹，否则
			// ① 请求级结论会丢掉"隐性失败"现场；② order 不递增导致候选序号重复；
			// ③ sawOther 未置位会把"限流 + 凭据缺失"的混合失败误判成全部限流(429)。
			attempts = append(attempts, attempt("凭据不可用"))
			sawOther = true
			continue
		}
		// 直通判定（AC-4/AC-18）：入站协议必须 ∈ 该模型可达 protocols，否则不发起上游请求。
		eff := effectiveProtocols(rt, mr)
		if !eff.Has(baseProtocol) {
			sawUnsupported = true
			attempts = append(attempts, skip("协议不支持"))
			continue
		}
		prov, ok := g.providerFactory(baseProtocol, endpoint)
		if !ok {
			l.Error("unsupported protocol", "protocol", baseProtocol)
			attempts = append(attempts, skip("协议不支持"))
			sawUnsupported = true
			continue
		}
		routeRes := &router.RouteResult{ChannelID: cr.ChannelID, ChannelKeyID: cr.ChannelKeyID, InternalModelID: cr.InternalModelID,
			ModelRowID: cr.ModelRowID, ExternalModelID: cr.ExternalModelID, AutoModelName: autoName, Revision: cr.Revision,
			Cost: cr.Cost, CostKnown: cr.CostKnown, TimeCoeff: cr.TimeCoeff}

		// 会话：既有会话命中本候选（密钥+模型）→ 续行；否则为新会话，但 Attach 放在限流通过之后
		// （限流拒绝不占会话名额；仅当限流放行、容量允许时新建会话并 +1 计数）。
		existing := hasSession && sessObj != nil &&
			sessObj.ChannelKeyID == cr.ChannelKeyID && sessObj.InternalModelID == cr.InternalModelID

		// 限流：模型级窗口 → 密钥级窗口（任一超限 → 下一候选）。
		if err := g.acquireLimit(ctx, mr.Model.RateLimit, mr.Limiter, estTokens(body)); err != nil {
			sawLimit = true
			attempts = append(attempts, skip("限流"))
			continue
		}
		if err := g.acquireLimit(ctx, rt.Channel.RateLimit, kr.Limiter, estTokens(body)); err != nil {
			sawLimit = true
			attempts = append(attempts, skip("限流"))
			continue
		}

		// 会话创建：限流已通过，按密钥校验并发上限。槽满只影响该候选 → failover 下一候选
		//（其他密钥/渠道/内部模型行仍有插槽时照常续用），不归为「限流类 429」；
		// 全部候选都因会话满失败时由下面终结分支统一回报 503（无可用渠道）。
		if !existing && sessionID != "" {
			ok2, _ := sess.Attach(&router.Session{
				SessionID:           sessionID,
				UserID:              token.UserID,
				TokenID:             token.ID,
				TokenDisplay:        token.DisplayName,
				Model:               model,
				SessionRaw:          sessionRaw,
				Name:                sessionNameFor(sessionRaw, body),
				ChannelKeyID:        cr.ChannelKeyID,
				InternalModelID:     cr.InternalModelID,
				ExpireAt:            g.now().Add(ttl),
				LastMsgFingerprints: msgFps,
			}, g.channels.KeyMaxSessions(cr.ChannelKeyID))
			if !ok2 {
				attempts = append(attempts, skip("会话满"))
				continue // 该密钥并发会话满 → 下一候选
			}
		}

		// 改写 body.model 为内部模型 ID（各候选渠道的 internal_model_id 可能不同）。
		rewritten, rerr := rewriteModel(body, routeRes.InternalModelID)
		if rerr != nil {
			writeOpenAIError(w, http.StatusBadRequest, "请求体格式错误")
			return
		}

		// 上游请求构造上下文：有效基址（协议级优先）/ 协议路径覆盖 / 头策略 / 入站协议 / 变量。
		ureq := UpstreamRequest{
			BaseURL:          effectiveBaseURL(rt, baseProtocol),
			Path:             eff[baseProtocol].Path,
			Cred:             kr.CredentialPlain,
			Body:             rewritten,
			DownstreamHeader: r.Header,
			HeaderPolicy:     effectiveHeaderPolicy(rt),
			Protocol:         baseProtocol,
			Vars: map[string]string{
				"session_id": sessionID,
				"request_id": logger.GetRequestID(ctx),
				// {model} 恒取内部模型 ID（与探测/拉取口径一致；请求体 model 亦已改写为它）。
				"model": routeRes.InternalModelID,
			},
		}

		var out *fwdOutcome
		// 进行中计数：包住整段上游转发（含流式全程），管理端「实时状态」据此判定「正在请求」。
		if sessionID != "" {
			sess.BeginRequest(sessionID)
		}
		if isStream {
			attempted = true
			out = g.serveStream(w, r, start, endpoint, prov, rt, routeRes, ureq, model, token, pricingMode, sessionID, sessionName, pc, reqMsgs, attempts)
		} else {
			attempted = true
			out = g.serveNonStream(w, r, start, endpoint, prov, rt, routeRes, ureq, model, token, pricingMode, sessionID, sessionName, pc, reqMsgs, attempts)
		}
		if sessionID != "" {
			sess.EndRequest(sessionID)
		}
		if out.responded {
			if sessionID != "" {
				sess.Renew(sessionID, ttl)
			}
			// 实际消费差额已在 recordBilling → Record 内多退少补；若仅落 failed、
			// 未实际消费（上游/转发失败），退回本次预扣。
			if out.refundPre {
				g.refundAllPreConsume(ctx, token.UserID, pc.Credits, "请求失败退回")
			}
			switch {
			case out.billFailed:
				attempts = append(attempts, attempt("计费失败"))
			case out.refundPre:
				attempts = append(attempts, attempt("上游失败"))
			case out.interrupted:
				attempts = append(attempts, attempt("流式中断"))
			default:
				attempts = append(attempts, attempt("success"))
			}
			// 请求级结论：状态码取**实际写出的下游状态码**（由 fwdOutcome 带回），
			// 避免把「计费失败(402/500)」「上游失败(502)」误报成成功；
			// 只有真正成功（非计费失败/退款且 status<400）且中间有候选失败，才是"隐性失败"。
			status := out.downstreamStatus
			if status == 0 {
				status = http.StatusOK // 兜底：已响应但未记录状态码（理论不可达）
			}
			if _, failed := summarizeAttempts(attempts); failed > 0 {
				failedReq := outcomeIsFailure(out) || status >= http.StatusBadRequest
				lvl := slog.LevelWarn
				reason := "候选失败后请求成功（隐性失败）"
				if failedReq {
					lvl = slog.LevelError
					reason = "候选失败且请求最终失败（详见各候选失败日志）"
				}
				g.logOutcome(ctx, lvl, endpoint, model, reason, status, !failedReq, token, sessionID, attempts, msSince(start))
			}
			return
		}
		// retry：本次失败可安全转移（响应未写出、上游未计费），继续尝试下一候选。
		if out.is429 {
			attempts = append(attempts, attempt("上游失败(429)"))
			sawLimit = true
		} else {
			attempts = append(attempts, attempt("上游失败"))
			sawOther = true
		}
	}

	// 所有候选都未成功。仅当失败全部属于限流类（本地限流/上游 429）时才回 429，
	// 否则统一回「无可用渠道」——避免把 401/402/5xx 等误报成限流。
	rejectDecision := func() map[string]any {
		return map[string]any{
			"attempts":         attempts,
			"est_input_tokens": pc.InputTokens,
			"time_coeff":       pc.TimeCoeff,
			"ctx_coeff":        pc.CtxCoeff,
			"pre_consumed":     pc.Credits,
			"result":           "rejected",
		}
	}
	// AC-4/AC-18：全部候选都因「入站协议 ∉ 模型可达 protocols」被拒且未发起任何上游请求 →
	// 返回固定错误（HTTP 400 + OpenAI 错误体，type=unsupported_protocol）。
	if sawUnsupported && !attempted {
		g.refundAllPreConsume(ctx, token.UserID, pc.Credits, "请求失败退回（协议不支持）")
		bid := g.recordGatewayReject(ctx, model, autoName, token, pricingMode, sessionID, msgUnsupportedProtocol)
		g.writeCallLog(ctx, bid, "", token, &router.RouteResult{AutoModelName: autoName}, model, pricingMode, sessionID,
			reqMsgs, "error", "", "failed", msgUnsupportedProtocol, msSince(start), nil, rejectDecision())
		g.logOutcome(ctx, slog.LevelWarn, endpoint, model, msgUnsupportedProtocol, http.StatusBadRequest, false, token, sessionID, attempts, msSince(start))
		writeUnsupportedProtocol(w)
		return
	}
	if sawLimit && !sawOther {
		g.refundAllPreConsume(ctx, token.UserID, pc.Credits, "请求失败退回（限流）")
		// 全部候选被本地拦截（限流/并发/禁用等，未发出任何上游调用）时补一条失败账单；
		// 已发出上游调用并落账的（如 429 重试）不重复落账。
		if !attempted {
			bid := g.recordGatewayReject(ctx, model, autoName, token, pricingMode, sessionID, msgRateLimited)
			g.writeCallLog(ctx, bid, "", token, &router.RouteResult{AutoModelName: autoName}, model, pricingMode, sessionID,
				reqMsgs, "error", "", "failed", msgRateLimited, msSince(start), nil, rejectDecision())
		}
		g.logOutcome(ctx, slog.LevelWarn, endpoint, model, "全部候选限流（本地限流/上游 429）", http.StatusTooManyRequests, false, token, sessionID, attempts, msSince(start))
		writeOpenAIError(w, http.StatusTooManyRequests, msgRateLimited)
		return
	}
	g.refundAllPreConsume(ctx, token.UserID, pc.Credits, "请求失败退回（无可用渠道）")
	if !attempted {
		bid := g.recordGatewayReject(ctx, model, autoName, token, pricingMode, sessionID, msgNoAvailable)
		g.writeCallLog(ctx, bid, "", token, &router.RouteResult{AutoModelName: autoName}, model, pricingMode, sessionID,
			reqMsgs, "error", "", "failed", msgNoAvailable, msSince(start), nil, rejectDecision())
	}
	g.logOutcome(ctx, slog.LevelError, endpoint, model, "全部候选均失败（无可用渠道）", http.StatusServiceUnavailable, false, token, sessionID, attempts, msSince(start))
	writeOpenAIError(w, http.StatusServiceUnavailable, msgNoAvailable)
}

// recordGatewayReject 记录一次未进入渠道的网关拒绝（零余额/未路由/候选全部被拦截等）为 failed 账单，
// 保证「每次请求必有一条账单记录」；渠道维度留空（未实际路由到任何渠道）。
// 返回关联的 billing_id（Record 失败或未装配计费时为空串），供 call_log 关联。
func (g *Gateway) recordGatewayReject(ctx context.Context, model, autoName string, token *identity.Token, pricingMode, sessionID, reason string) string {
	if g.billing == nil || token == nil {
		return ""
	}
	return g.recordFailure(ctx, &router.RouteResult{AutoModelName: autoName}, model, token, pricingMode, sessionID, "", reason, nil)
}

// routeResFor 由路由候选构造最小 RouteResult（入口复查失败——凭据不可用等——用于账单与日志关联）。
// autoName 为请求的 Auto 名（非 Auto 请求为空），随 RouteResult 贯穿到账单/调用日志留痕。
func routeResFor(cr *router.ChannelRoute, autoName string) *router.RouteResult {
	return &router.RouteResult{
		ChannelID:       cr.ChannelID,
		ChannelKeyID:    cr.ChannelKeyID,
		InternalModelID: cr.InternalModelID,
		ModelRowID:      cr.ModelRowID,
		ExternalModelID: cr.ExternalModelID,
		AutoModelName:   autoName,
		Revision:        cr.Revision,
		Cost:            cr.Cost,
		CostKnown:       cr.CostKnown,
		TimeCoeff:       cr.TimeCoeff,
	}
}

// refundPreConsume 退还一次预扣冻结（失败时只记日志，不回滚请求语义）。
func (g *Gateway) refundPreConsume(ctx context.Context, userID int64, amount float64, remark string) {
	if g.credit == nil || amount <= 0 {
		return
	}
	// 退款脱离请求取消：预扣必须能退回，否则会漏掉用户积分。
	ctx, cancel := billingCtx(ctx)
	defer cancel()
	if err := g.credit.Refund(ctx, userID, amount, remark); err != nil {
		g.logger.Error("refund preconsume failed", "user_id", userID, "amount", amount, "err", err)
	}
}

// refundAllPreConsume 全部候选失败时退还预扣（对齐 new-api RefundFailedRequestBilling）。
func (g *Gateway) refundAllPreConsume(ctx context.Context, userID int64, preConsumed float64, reason string) {
	if preConsumed <= 0 {
		return
	}
	g.refundPreConsume(ctx, userID, preConsumed, reason)
}

// statusOriginTimeout 源站超时（Cloudflare 524），不在 net/http 常量表中。
const statusOriginTimeout = 524

// retryableUpstreamStatus 判断「上游已返回状态码」时，本次失败能否安全转移到下一候选。
//
// 判定依据是「该次上游调用是否可能已被计费」——否则转移会造成上游两次计费、下游只计一次：
//   - 可转移（上游明确拒绝，未生成内容、未计费）：401 / 402 / 403 / 404 / 429；
//   - 可转移（上游未完成本次生成）：5xx，但 504 / 524 除外——这两类是网关/边缘侧超时，
//     常发生在源站已生成内容之后，故不转移；
//   - 其余 4xx 视为请求本身的问题，换渠道同样失败，直接返回客户端。
func retryableUpstreamStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusPaymentRequired, http.StatusForbidden,
		http.StatusNotFound, http.StatusTooManyRequests:
		return true
	case http.StatusGatewayTimeout, statusOriginTimeout:
		return false
	}
	return code >= 500
}

// withWriteTrace 给请求挂上 httptrace，记录「请求体是否已完整送达上游」。
// 用于区分两类调用失败：
//   - 连接阶段失败（未送达）：上游不可能收到请求、不可能计费 → 可安全转移；
//   - 请求已送达后失败（多为响应超时）：上游可能已生成并计费 → 不转移，避免上游重复计费。
func withWriteTrace(req *http.Request) (*http.Request, *atomic.Bool) {
	wrote := &atomic.Bool{}
	trace := &httptrace.ClientTrace{
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil {
				wrote.Store(true)
			}
		},
	}
	return req.WithContext(httptrace.WithClientTrace(req.Context(), trace)), wrote
}

// billingTimeout 记账/退款类操作的超时兜底。
const billingTimeout = 30 * time.Second

// billingCtx 记账用的上下文：脱离请求取消信号（客户端提前断开、流式响应结束都会取消请求 ctx），
// 保留 trace/values 并带超时兜底。账单落库与预扣退还都必须用它——上游调用已经发生，
// 不能因为下游断开就记不上账、退不回预扣。
func billingCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), billingTimeout)
}

// fwdOutcome 一次渠道转发的结果。
// responded=true 表示响应已写出（终态）；retry=true 表示本次失败可安全转移到下一候选
// （响应未写出且上游未计费），由调用方继续尝试；is429 标记该可重试失败是否为上游限流；
// charged 为实际计费（扣除）的积分；billFailed=true 表示计费本身失败（内容已响应，
// 预扣不退、按预扣计费，杜绝赊账）；refundPre=true 表示仅落了 failed 账单、
// 未实际消费（上游/转发失败），预扣应全额退回。
type fwdOutcome struct {
	responded bool
	retry     bool
	is429     bool
	charged   float64
	// downstreamStatus 已写出的下游 HTTP 状态码（responded=true 时有效；0=未记录）。
	// 请求级日志必须用它而非固定 200：否则「计费失败(402/500)」「上游失败(502)」会被误报为成功。
	downstreamStatus int
	billFailed       bool
	refundPre        bool
	// interrupted 流式传输中断（内容已部分下发、无法撤回）：语义上请求失败，
	// 但下游状态码仍为 200，故必须单列，不能靠状态码判定成败。
	interrupted bool
}

// serveNonStream 非流式转发：2xx 先计费再响应。
// 响应未写出且上游未计费的失败（构建/连接失败、401/402/403/404/429、5xx 等）返回 retry，
// 由调用方切换到下一候选；其余失败（请求已送达后的超时、已收 2xx 后读体失败、其余 4xx、
// 504/524）直接写出响应，不再转移。
func (g *Gateway) serveNonStream(w http.ResponseWriter, r *http.Request, start time.Time, endpoint string,
	prov Provider, rt *channel.RuntimeChannel, routeRes *router.RouteResult, ureq UpstreamRequest,
	model string, token *identity.Token, pricingMode, sessionID, sessionName string,
	pc preCharge, reqMsgs []json.RawMessage, attempts []map[string]any) *fwdOutcome {

	ctx := r.Context()
	stage := "non_stream"        // 日志用：失败发生的转发阶段
	attempt := len(attempts) + 1 // 当前候选序号（候选总数由请求级 outcome 日志给出）
	req, err := prov.BuildUpstreamRequest(ctx, ureq)
	if err != nil {
		// 本地构建失败，未发出任何上游调用 → 可安全转移。
		g.logError(rt.Channel.ID, "build upstream request", err)
		g.logUpstreamErr(ctx, slog.LevelWarn, "upstream request build failed", endpoint, ureq.Protocol, model, token, routeRes,
			diagFromTarget(ureq.BaseURL, ureq.Path, ureq.Protocol, ureq.Cred),
			errInfo{kind: "build_failed", stage: stage, attempt: attempt,
				retry: true, reason: "构建上游请求失败：" + err.Error(), sessionID: sessionID, durationMs: *msSince(start)})
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, channel.Feedback{IsSuccess: false, Now: g.now()})
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, "构建上游请求失败", msSince(start))
		// 网关侧失败（未发出上游调用）：resp_body 留空，原因只落 error_message 与决策轨迹，
		// 避免「请求增量/响应/决策原因」三处都是同一句文案。
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", "", "failed", "构建上游请求失败", msSince(start), nil, g.callLogDecision(attempts, "上游请求构建失败", pc, routeRes))
		return &fwdOutcome{retry: true}
	}
	req, wrote := withWriteTrace(req)
	up, err := g.client.Do(req)
	if err != nil {
		g.logError(rt.Channel.ID, "upstream call failed", err)
		// 连接阶段失败（未送达，可转移）与已送达后失败（多为超时，不转移）语义不同，现场一并记录。
		g.logUpstreamErr(ctx, slog.LevelWarn, "upstream call failed", endpoint, ureq.Protocol, model, token, routeRes,
			diagFromRequest(req, ureq.Protocol, ureq.Cred),
			errInfo{kind: "connect_failed", stage: stage, attempt: attempt,
				retry: !wrote.Load(), reason: "上游调用失败：" + err.Error(), sessionID: sessionID, durationMs: *msSince(start)})
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, channel.Feedback{IsSuccess: false, Now: g.now()})
		if !wrote.Load() {
			// 请求体未送达上游：上游不可能计费 → 可安全转移。
			bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, "上游连接失败", msSince(start))
			// 连接失败：上游未返回任何响应体，resp_body 留空（原因见 error_message / 决策轨迹）。
			g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
				"error", "", "failed", "上游连接失败", msSince(start), nil, g.callLogDecision(attempts, "上游连接失败", pc, routeRes))
			return &fwdOutcome{retry: true}
		}
		// 请求已送达后失败（多为响应超时）：上游可能已生成并计费，转移会导致上游重复计费 → 不转移。
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, "上游请求失败（已送达，不转移）", msSince(start))
		// 已送达但失败（多为超时）：上游可能已生成，但不返回响应体 → resp_body 留空。
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", "", "failed", "上游请求失败（已送达，不转移）", msSince(start), nil, g.callLogDecision(attempts, "上游请求失败", pc, routeRes))
		writeOpenAIError(w, http.StatusBadGateway, msgUpstreamDown)
		return &fwdOutcome{responded: true, refundPre: true, downstreamStatus: http.StatusBadGateway}
	}
	defer up.Body.Close()

	switch {
	case up.StatusCode >= 200 && up.StatusCode < 300:
		raw, err := io.ReadAll(up.Body)
		if err != nil {
			// 已收到 2xx：上游已完成生成并计费，转移会造成上游重复计费 → 不转移。
			g.logError(rt.Channel.ID, "read upstream body", err)
			g.logUpstreamErr(ctx, slog.LevelError, "read upstream body failed", endpoint, ureq.Protocol, model, token, routeRes,
				diagFromRequest(req, ureq.Protocol, ureq.Cred),
				errInfo{kind: "read_body_failed", stage: stage, status: up.StatusCode, attempt: attempt,
					reason: "读取上游响应失败：" + err.Error(), sessionID: sessionID, durationMs: *msSince(start)})
			bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, "读取上游响应失败", msSince(start))
			// 读取失败：响应体不可用（读坏了），resp_body 留空而不是重复错误文案。
			g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
				"error", "", "failed", "读取上游响应失败", msSince(start), nil, g.callLogDecision(attempts, "读取上游响应失败", pc, routeRes))
			writeOpenAIError(w, http.StatusBadGateway, msgUpstreamDown)
			return &fwdOutcome{responded: true, refundPre: true, downstreamStatus: http.StatusBadGateway}
		}
		dur := msSince(start)
		// upstreamUsage：上游原始 usage（JSONB 值），随调用日志落库用于诊断渠道口径（见 rawusage.go）。
		usage, upstreamUsage, uerr := prov.ExtractUsage(raw)
		usageMissing := uerr != nil || usageZero(usage)
		if usageMissing {
			// AC-17（放宽）：上游未返回/无法解析 usage 属极端协议违规；内容已由上游生成并在此透传，
			// 故不退预扣，按可见内容估算 token 计费，并把渠道置 DISABLED。
			g.channels.DisableChannel(routeRes.ChannelID)
			g.logUpstreamErr(ctx, slog.LevelWarn, "upstream usage missing", endpoint, ureq.Protocol, model, token, routeRes,
				diagFromRequest(req, ureq.Protocol, ureq.Cred),
				errInfo{kind: "usage_missing", stage: stage, status: up.StatusCode, attempt: attempt,
					reason: "上游未返回或无法解析 usage（按估算计费并将渠道置禁用）", sessionID: sessionID, durationMs: *msSince(start)})
			content, reasoning, tools := rawParts(raw)
			usage = missingUsageEstimate(ureq.Body, raw, model, content, reasoning, tools)
		}

		// 非流式：先计费再响应。余额不足 → 402，不返成功 body。
		// 注意：上游已成功返回 2xx，计费失败（含余额不足）属用户侧/网关侧问题，
		// 不反馈渠道失败，避免健康渠道被误判为故障（DRAIN_ONLY）。
		out := &fwdOutcome{responded: true}
		ch, bid, berr := g.recordBilling(ctx, routeRes, model, token, usage, pricingMode, sessionID, sessionName, pc.Credits, dur, nil)
		if berr != nil {
			out.billFailed = true
			if isInsufficient(berr) {
				g.logger.Warn("billing insufficient", "user_id", token.UserID, "err", berr)
				g.logUpstreamErr(ctx, slog.LevelWarn, "billing insufficient", endpoint, ureq.Protocol, model, token, routeRes,
					diagFromRequest(req, ureq.Protocol, ureq.Cred),
					errInfo{kind: "billing_failed", stage: stage, status: up.StatusCode, downstream: http.StatusPaymentRequired,
						attempt: attempt, reason: "积分余额不足，计费失败：" + berr.Error(), sessionID: sessionID, durationMs: deref64(dur)})
				// billing.Record 内部已落一条 failed（含实际 tokens/rates/系数与余额不足原因），
				// 网关不再重复落账，保证每次请求恰好一条记录。
				g.writeCallLogUsage(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
					"non_stream", string(raw), "failed", "积分余额不足，计费失败", upstreamUsage, dur, nil, g.callLogDecision(attempts, "积分余额不足", pc, routeRes))
				writeOpenAIError(w, http.StatusPaymentRequired, msgInsufficient)
				return &fwdOutcome{responded: true, billFailed: true, downstreamStatus: http.StatusPaymentRequired}
			}
			g.logError(rt.Channel.ID, "record billing failed", berr)
			g.logUpstreamErr(ctx, slog.LevelError, "billing failed", endpoint, ureq.Protocol, model, token, routeRes,
				diagFromRequest(req, ureq.Protocol, ureq.Cred),
				errInfo{kind: "billing_failed", stage: stage, status: up.StatusCode, downstream: http.StatusInternalServerError,
					attempt: attempt, reason: "计费失败：" + berr.Error(), sessionID: sessionID, durationMs: deref64(dur)})
			// 计费失败时上游已返回响应体，resp_body 保留其原文（网关侧错误只落 error_message 与决策轨迹）。
			g.writeCallLogUsage(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
				"non_stream", string(raw), "failed", "计费失败", upstreamUsage, dur, nil, g.callLogDecision(attempts, "计费失败", pc, routeRes))
			writeOpenAIError(w, http.StatusInternalServerError, msgInternal)
			return &fwdOutcome{responded: true, billFailed: true, downstreamStatus: http.StatusInternalServerError}
		}
		out.charged = ch
		// 调用日志：成功路径（已知 billingID 且响应写出前）。
		reason := "success"
		if usageMissing {
			reason = "usage缺失(按估算计费)"
		}
		g.writeCallLogUsage(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"non_stream", string(raw), "completed", "", upstreamUsage, dur, nil, g.callLogDecision(attempts, reason, pc, routeRes))

		// 渠道透明：基础协议硬编码响应头 + 全局上游响应头白名单（默认空）；不下发任何 X-Channel* 头。
		applyResponseHeaderWhitelist(w, up.Header, g.upstreamHeaders())
		h := w.Header()
		h.Set("Content-Type", up.Header.Get("Content-Type"))
		out.downstreamStatus = up.StatusCode // 请求级日志用真实状态码
		w.WriteHeader(up.StatusCode)
		_, _ = w.Write(raw)
		// usage 缺失的渠道已置 DISABLED，不再回喂成功反馈（与流式路径一致，避免禁用态被注入成功样本）。
		if !usageMissing {
			g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, feedbackSuccess(start))
		}
		return out

	case up.StatusCode == http.StatusTooManyRequests:
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, channel.Feedback{Is429: true, Now: g.now()})
		upErr := readUpstreamErrBody(up)
		errMsg := upstreamErrDetail(up.StatusCode, upErr)
		param, etype, ecode, summary := upstreamErrParts(upErr)
		g.logUpstreamErr(ctx, slog.LevelWarn, "upstream rate limited", endpoint, ureq.Protocol, model, token, routeRes,
			diagFromRequest(req, ureq.Protocol, ureq.Cred),
			errInfo{kind: "upstream_status", stage: stage, status: up.StatusCode, attempt: attempt, retry: true,
				param: param, errType: etype, errCode: ecode, summary: summary,
				reason: "上游限流(429)，已转入下一候选", sessionID: sessionID, durationMs: *msSince(start)})
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, errMsg, msSince(start))
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", upErr, "failed", errMsg, msSince(start), nil, g.callLogDecision(attempts, "上游限流(429)", pc, routeRes))
		return &fwdOutcome{retry: true, is429: true}
	case up.StatusCode == http.StatusUnauthorized || up.StatusCode == http.StatusForbidden:
		// 凭据可能失效：先转移下一候选；连续失败达阈值由状态机熔断（channel.Machine）。
		// 注意：此处按状态码归类为「凭据失效」，但上游报文可能指向模型维度
		//（日志字段 err_scope=model）；该字段仅用于事后分诊，不改变既有状态机判定。
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, channel.Feedback{IsAuthFailure: true, Now: g.now()})
		upErr := readUpstreamErrBody(up)
		errMsg := upstreamErrDetail(up.StatusCode, upErr)
		param, etype, ecode, summary := upstreamErrParts(upErr)
		g.logUpstreamErr(ctx, slog.LevelWarn, "upstream auth rejected", endpoint, ureq.Protocol, model, token, routeRes,
			diagFromRequest(req, ureq.Protocol, ureq.Cred),
			errInfo{kind: "upstream_status", stage: stage, status: up.StatusCode, attempt: attempt, retry: true,
				param: param, errType: etype, errCode: ecode, summary: summary,
				reason: "上游返回 401/403，已转入下一候选（该失败通常对下游不可见）", sessionID: sessionID, durationMs: *msSince(start)})
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, errMsg, msSince(start))
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", upErr, "failed", errMsg, msSince(start), nil, g.callLogDecision(attempts, "上游鉴权失败", pc, routeRes))
		return &fwdOutcome{retry: true}
	default:
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, feedbackFail(start))
		upErr := readUpstreamErrBody(up)
		errMsg := upstreamErrDetail(up.StatusCode, upErr)
		param, etype, ecode, summary := upstreamErrParts(upErr)
		retryable := retryableUpstreamStatus(up.StatusCode)
		lvl := slog.LevelWarn
		if !retryable {
			lvl = slog.LevelError // 已直接返回下游的失败，属用户可见异常
		}
		g.logUpstreamErr(ctx, lvl, "upstream error response", endpoint, ureq.Protocol, model, token, routeRes,
			diagFromRequest(req, ureq.Protocol, ureq.Cred),
			errInfo{kind: "upstream_status", stage: stage, status: up.StatusCode, attempt: attempt,
				retry: retryable,
				param: param, errType: etype, errCode: ecode, summary: summary,
				reason: errMsg, sessionID: sessionID, durationMs: *msSince(start)})
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, errMsg, msSince(start))
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", upErr, "failed", errMsg, msSince(start), nil, g.callLogDecision(attempts, errMsg, pc, routeRes))
		if retryableUpstreamStatus(up.StatusCode) {
			return &fwdOutcome{retry: true}
		}
		writeOpenAIError(w, http.StatusBadGateway, msgUpstreamDown)
		return &fwdOutcome{responded: true, refundPre: true, downstreamStatus: http.StatusBadGateway}
	}
}

// upstreamErrBytes 上游错误响应体的最大读取字节数（供审计展示，避免大响应体撑爆日志/库）。
const upstreamErrBytes = 4096

// readUpstreamErrBody 读取上游错误响应体（截断至 upstreamErrBytes）用于审计；读取失败返回空串。
// 注意：返回值同时用于**日志摘要**与**落库**（账单 error_message、调用日志 resp_body），
// 因此在唯一出口统一做凭据样式清洗——上游可能在自己回传的报文里回显收到的凭据，
// 不在此处清洗会同时突破"日志只落指纹"与"存储不落明文"两条红线。
func readUpstreamErrBody(resp *http.Response) string {
	b, err := io.ReadAll(io.LimitReader(resp.Body, upstreamErrBytes))
	if err != nil {
		return ""
	}
	return sanitizeErrSummary(strings.TrimSpace(string(b)))
}

// upstreamErrNote 上游错误状态码的中文说明（沿用既有文案，便于告警/检索）。
func upstreamErrNote(status int) string {
	switch status {
	case http.StatusTooManyRequests:
		return fmt.Sprintf("上游返回 %d（触发限流）", status)
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Sprintf("上游返回 %d（渠道凭据可能失效）", status)
	default:
		return fmt.Sprintf("上游返回 %d", status)
	}
}

// upstreamErrDetail 拼接状态码说明与上游响应体（截断；无响应体时仅说明），
// 供 billing_records.error_message 与 call_logs.error_message 落库，管理员据此定位上游故障。
func upstreamErrDetail(status int, body string) string {
	if body == "" {
		return upstreamErrNote(status)
	}
	return upstreamErrNote(status) + ": " + body
}

// msSince 从 start 至今的毫秒数。
func msSince(start time.Time) *int64 {
	v := time.Since(start).Milliseconds()
	return &v
}

// serveStream 流式转发：设置 SSE 头后逐块透传，流结束后依据累计 usage 计费。
// 流式响应已发出，计费失败无法回改响应，仅记审计日志（bio 竞态可接受）。
// 响应未写出且上游未计费的失败（构建/连接失败、401/402/403/404/429、5xx 等）返回 retry，
// 由调用方切换到下一候选；其余失败直接写出响应，不再转移。
func (g *Gateway) serveStream(w http.ResponseWriter, r *http.Request, start time.Time, endpoint string,
	prov Provider, rt *channel.RuntimeChannel, routeRes *router.RouteResult, ureq UpstreamRequest,
	model string, token *identity.Token, pricingMode, sessionID, sessionName string,
	pc preCharge, reqMsgs []json.RawMessage, attempts []map[string]any) *fwdOutcome {

	ctx := r.Context()
	stage := "stream"            // 日志用：失败发生的转发阶段
	attempt := len(attempts) + 1 // 当前候选序号（候选总数由请求级 outcome 日志给出）
	req, err := prov.BuildUpstreamRequest(ctx, ureq)
	if err != nil {
		// 本地构建失败，未发出任何上游调用 → 可安全转移。
		g.logError(rt.Channel.ID, "build upstream stream request", err)
		g.logUpstreamErr(ctx, slog.LevelWarn, "upstream request build failed", endpoint, ureq.Protocol, model, token, routeRes,
			diagFromTarget(ureq.BaseURL, ureq.Path, ureq.Protocol, ureq.Cred),
			errInfo{kind: "build_failed", stage: stage, attempt: attempt,
				retry: true, reason: "构建上游流式请求失败：" + err.Error(), sessionID: sessionID, durationMs: *msSince(start)})
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, channel.Feedback{IsSuccess: false, Now: g.now()})
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, "构建上游流式请求失败", msSince(start))
		// 网关侧失败（未发出上游调用）：resp_body 留空，原因只落 error_message 与决策轨迹。
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", "", "failed", "构建上游流式请求失败", msSince(start), nil, g.callLogDecision(attempts, "上游请求构建失败", pc, routeRes))
		return &fwdOutcome{retry: true}
	}
	req, wrote := withWriteTrace(req)
	up, err := g.streamClient.Do(req)
	if err != nil {
		g.logError(rt.Channel.ID, "upstream stream call failed", err)
		g.logUpstreamErr(ctx, slog.LevelWarn, "upstream call failed", endpoint, ureq.Protocol, model, token, routeRes,
			diagFromRequest(req, ureq.Protocol, ureq.Cred),
			errInfo{kind: "connect_failed", stage: stage, attempt: attempt,
				retry: !wrote.Load(), reason: "上游流式调用失败：" + err.Error(), sessionID: sessionID, durationMs: *msSince(start)})
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, channel.Feedback{IsSuccess: false, Now: g.now()})
		if !wrote.Load() {
			// 请求体未送达上游：上游不可能计费 → 可安全转移。
			bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, "上游流式连接失败", msSince(start))
			// 连接失败：上游未返回任何响应体，resp_body 留空（原因见 error_message / 决策轨迹）。
			g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
				"error", "", "failed", "上游流式连接失败", msSince(start), nil, g.callLogDecision(attempts, "上游连接失败", pc, routeRes))
			return &fwdOutcome{retry: true}
		}
		// 请求已送达后失败（多为响应超时）：上游可能已生成并计费，转移会导致上游重复计费 → 不转移。
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, "上游流式请求失败（已送达，不转移）", msSince(start))
		// 已送达但失败（多为超时）：上游可能已生成，但不返回响应体 → resp_body 留空。
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", "", "failed", "上游流式请求失败（已送达，不转移）", msSince(start), nil, g.callLogDecision(attempts, "上游请求失败", pc, routeRes))
		writeOpenAIError(w, http.StatusBadGateway, msgUpstreamDown)
		return &fwdOutcome{responded: true, refundPre: true, downstreamStatus: http.StatusBadGateway}
	}
	defer up.Body.Close()

	switch {
	case up.StatusCode >= 200 && up.StatusCode < 300:
		// 基础协议硬编码 SSE 头 + 全局上游响应头白名单（须在 HandleStream 写响应头前注入）。
		applyResponseHeaderWhitelist(w, up.Header, g.upstreamHeaders())
		// 记录首个 SSE chunk 写出时机（首字耗时）。
		ft := &firstTokenWriter{ResponseWriter: w}
		// upstreamUsage：上游原始 usage 分片合并后的 JSONB 值（诊断渠道口径，见 rawusage.go）。
		usage, upstreamUsage, assistantMsg, herr := prov.HandleStream(ctx, up, ft)
		dur := msSince(start)
		// 计费上下文（脱离请求取消、带超时）：响应结束后请求 ctx 会被取消，账单不可丢。
		billCtx, cancelBill := billingCtx(ctx)
		defer cancelBill()
		out := &fwdOutcome{responded: true}
		// 流式响应由 HandleStream 按 200 写响应头，内容一旦下发不可撤回：
		// 计费失败/中断只影响账单与日志语义，下游状态码仍是 200，故照实记录。
		out.downstreamStatus = http.StatusOK
		usageMissing := errors.Is(herr, ErrUsageMissing)
		if usageMissing {
			// AC-17（放宽）：上游不符合接口协议的返回属极端情况；内容已透传无法撤回，
			// 故不退预扣，按可见内容估算 token 计费，并把渠道置 DISABLED。
			g.channels.DisableChannel(routeRes.ChannelID)
			g.logUpstreamErr(ctx, slog.LevelWarn, "upstream usage missing", endpoint, ureq.Protocol, model, token, routeRes,
				diagFromRequest(req, ureq.Protocol, ureq.Cred),
				errInfo{kind: "usage_missing", stage: stage, status: up.StatusCode, attempt: attempt,
					reason: "上游流式响应未返回或无法解析 usage（按估算计费并将渠道置禁用）", sessionID: sessionID, durationMs: *dur})
			content, reasoning, tools := assistantParts(assistantMsg)
			usage = missingUsageEstimate(ureq.Body, assistantMsg, model, content, reasoning, tools)
		}
		// 客户端已离开（主动断开 / 下游写失败 / 请求 ctx 已取消）：内容无法送达，
		// 既不算上游中断失败（不置 interrupted、不落 failed 账单），也不应在结论里声称"成功"。
		clientGone := streamClientGone(ctx, herr)
		if clientGone {
			// 客户端已离开：无论是否带回错误都要留痕（herr 为空时只记"已断开"），
			// 避免"客户端中断"在观测面上完全消失。
			reasonText := "客户端已断开，流式响应未完整送达"
			if herr != nil {
				reasonText += "：" + herr.Error()
			}
			g.logUpstreamErr(ctx, slog.LevelInfo, "stream client gone", endpoint, ureq.Protocol, model, token, routeRes,
				diagFromRequest(req, ureq.Protocol, ureq.Cred),
				errInfo{kind: "client_gone", stage: stage, status: up.StatusCode, attempt: attempt,
					reason: reasonText, sessionID: sessionID, durationMs: *dur})
		}
		hardErr := herr != nil && !clientGone && !usageMissing
		if hardErr {
			out.interrupted = true // 语义上失败：内容被截断，请求级结论不得记为"成功"
			g.logError(rt.Channel.ID, "stream handle failed", herr)
			g.logUpstreamErr(ctx, slog.LevelError, "stream interrupted", endpoint, ureq.Protocol, model, token, routeRes,
				diagFromRequest(req, ureq.Protocol, ureq.Cred),
				errInfo{kind: "stream_interrupted", stage: stage, status: up.StatusCode, attempt: attempt,
					reason: "流式传输中断：" + herr.Error(), sessionID: sessionID, durationMs: *dur})
			bid := g.recordFailure(billCtx, routeRes, model, token, pricingMode, sessionID, sessionName, "流式传输中断", dur)
			g.writeCallLogUsage(billCtx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
				"error", string(assistantMsg), "failed", "流式传输中断", upstreamUsage, dur, ft.firstTokenMs(start), g.callLogDecision(attempts, "流式传输中断", pc, routeRes))
		}
		// 流结束后计费（预扣已在前置阶段完成；此处按实际/估算扣费，差额由 serve 结算退补）。
		ch, bid, berr := g.recordBilling(billCtx, routeRes, model, token, usage, pricingMode, sessionID, sessionName, pc.Credits, dur, ft.firstTokenMs(start))
		if berr != nil {
			g.logger.Error("stream billing failed", "user_id", token.UserID,
				"channel_id", rt.Channel.ID, "model", model, "err", berr)
			g.logUpstreamErr(ctx, slog.LevelError, "stream billing failed", endpoint, ureq.Protocol, model, token, routeRes,
				diagFromRequest(req, ureq.Protocol, ureq.Cred),
				errInfo{kind: "billing_failed", stage: stage, status: up.StatusCode, attempt: attempt,
					reason: "流式计费失败：" + berr.Error(), sessionID: sessionID, durationMs: *dur})
			out.billFailed = true // 已响应内容无法撤回：预扣不退（按预扣计费），不允许赊账
		} else {
			out.charged = ch
		}
		// 调用日志：流结束后采集（含 hardErr 失败分支的 resp_kind=error；正常/billFailed 为 stream）。
		switch {
		case hardErr:
			// 失败分支已在上面记录（resp_kind=error）。
		case out.billFailed:
			g.writeCallLogUsage(billCtx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
				"stream", string(assistantMsg), "failed", "流式计费失败", upstreamUsage, dur, ft.firstTokenMs(start), g.callLogDecision(attempts, "计费失败", pc, routeRes))
		default:
			reason := "success"
			if usageMissing {
				reason = "usage缺失(按估算计费)"
			}
			g.writeCallLogUsage(billCtx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
				"stream", string(assistantMsg), "completed", "", upstreamUsage, dur, ft.firstTokenMs(start), g.callLogDecision(attempts, reason, pc, routeRes))
		}
		// usage 缺失的渠道已置 DISABLED，不再回喂成功反馈（避免与禁用态冲突）。
		if !usageMissing {
			g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, feedbackSuccess(start))
		}
		return out
	case up.StatusCode == http.StatusTooManyRequests:
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, channel.Feedback{Is429: true, Now: g.now()})
		upErr := readUpstreamErrBody(up)
		errMsg := upstreamErrDetail(up.StatusCode, upErr)
		param, etype, ecode, summary := upstreamErrParts(upErr)
		g.logUpstreamErr(ctx, slog.LevelWarn, "upstream rate limited", endpoint, ureq.Protocol, model, token, routeRes,
			diagFromRequest(req, ureq.Protocol, ureq.Cred),
			errInfo{kind: "upstream_status", stage: stage, status: up.StatusCode, attempt: attempt, retry: true,
				param: param, errType: etype, errCode: ecode, summary: summary,
				reason: "上游限流(429)，已转入下一候选", sessionID: sessionID, durationMs: *msSince(start)})
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, errMsg, msSince(start))
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", upErr, "failed", errMsg, msSince(start), nil, g.callLogDecision(attempts, "上游限流(429)", pc, routeRes))
		return &fwdOutcome{retry: true, is429: true}
	case up.StatusCode == http.StatusUnauthorized || up.StatusCode == http.StatusForbidden:
		// 凭据可能失效：先转移下一候选；连续失败达阈值由状态机熔断（channel.Machine）。
		// 注意：此处按状态码归类为「凭据失效」，但上游报文可能指向模型维度
		//（日志字段 err_scope=model）；该字段仅用于事后分诊，不改变既有状态机判定。
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, channel.Feedback{IsAuthFailure: true, Now: g.now()})
		upErr := readUpstreamErrBody(up)
		errMsg := upstreamErrDetail(up.StatusCode, upErr)
		param, etype, ecode, summary := upstreamErrParts(upErr)
		g.logUpstreamErr(ctx, slog.LevelWarn, "upstream auth rejected", endpoint, ureq.Protocol, model, token, routeRes,
			diagFromRequest(req, ureq.Protocol, ureq.Cred),
			errInfo{kind: "upstream_status", stage: stage, status: up.StatusCode, attempt: attempt, retry: true,
				param: param, errType: etype, errCode: ecode, summary: summary,
				reason: "上游返回 401/403，已转入下一候选（该失败通常对下游不可见）", sessionID: sessionID, durationMs: *msSince(start)})
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, errMsg, msSince(start))
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", upErr, "failed", errMsg, msSince(start), nil, g.callLogDecision(attempts, "上游鉴权失败", pc, routeRes))
		return &fwdOutcome{retry: true}
	default:
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, feedbackFail(start))
		upErr := readUpstreamErrBody(up)
		errMsg := upstreamErrDetail(up.StatusCode, upErr)
		param, etype, ecode, summary := upstreamErrParts(upErr)
		retryable := retryableUpstreamStatus(up.StatusCode)
		lvl := slog.LevelWarn
		if !retryable {
			lvl = slog.LevelError // 已直接返回下游的失败，属用户可见异常
		}
		g.logUpstreamErr(ctx, lvl, "upstream error response", endpoint, ureq.Protocol, model, token, routeRes,
			diagFromRequest(req, ureq.Protocol, ureq.Cred),
			errInfo{kind: "upstream_status", stage: stage, status: up.StatusCode, attempt: attempt,
				retry: retryable,
				param: param, errType: etype, errCode: ecode, summary: summary,
				reason: errMsg, sessionID: sessionID, durationMs: *msSince(start)})
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, errMsg, msSince(start))
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", upErr, "failed", errMsg, msSince(start), nil, g.callLogDecision(attempts, errMsg, pc, routeRes))
		if retryableUpstreamStatus(up.StatusCode) {
			return &fwdOutcome{retry: true}
		}
		writeOpenAIError(w, http.StatusBadGateway, msgUpstreamDown)
		return &fwdOutcome{responded: true, refundPre: true, downstreamStatus: http.StatusBadGateway}
	}
}

// firstTokenWriter 包装 ResponseWriter，记录首个字节写出的时间（SSE 首字耗时）。
type firstTokenWriter struct {
	http.ResponseWriter
	first time.Time
}

// streamClientGone 判定"客户端已离开"：上游返回 context.Canceled，或请求 ctx 已取消
// （下游写失败通常伴随请求 ctx 取消）。客户端离开不算上游中断失败，但必须留痕。
func streamClientGone(ctx context.Context, herr error) bool {
	if errors.Is(herr, context.Canceled) {
		return true
	}
	return ctx.Err() != nil
}

func (w *firstTokenWriter) Write(p []byte) (int, error) {
	if w.first.IsZero() {
		w.first = time.Now()
	}
	return w.ResponseWriter.Write(p)
}

// Flush 透传底层 ResponseWriter 的 Flush：包装后仍满足 http.Flusher，
// 保证 SSE 逐块下发不被缓冲（否则上游 Provider 的 flusher 断言恒为 nil，流式退化）。
func (w *firstTokenWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *firstTokenWriter) firstTokenMs(start time.Time) *int64 {
	if w.first.IsZero() {
		return nil
	}
	v := w.first.Sub(start).Milliseconds()
	return &v
}

// recordBilling 依据 pricing_mode 选型计费单价并调用计费服务，返回实际扣减的积分、关联 billing_id 与错误。
//
// 双层模型计价：sale 用对外模型 ext.SaleRates + 其模型级 time/tiers；cost 用选中渠道
// channel_models 的 CostRates + 其模型级 time/tiers；任一模型级为空则回落全局（billing 内部实现）。
func (g *Gateway) recordBilling(ctx context.Context, routeRes *router.RouteResult,
	model string, token *identity.Token, usage billing.Usage, pricingMode, sessionID, sessionName string,
	preConsumed float64, durationMs, firstTokenMs *int64) (float64, string, error) {

	// 记账脱离请求取消：上游调用已发生，不能因下游断开/流式结束而丢失账单。
	ctx, cancel := billingCtx(ctx)
	defer cancel()

	ext, err := g.models.GetByExternalName(ctx, model)
	if err != nil {
		return 0, "", fmt.Errorf("resolve external model: %w", err)
	}
	cm, err := g.channelModels.GetByChannelAndInternal(ctx, routeRes.ChannelID, routeRes.InternalModelID)
	if err != nil {
		return 0, "", fmt.Errorf("resolve channel model rates: %w", err)
	}

	var rateCharged billing.Rates
	var timeCfg *billing.TimeCoeffConfig
	var ctxTiers []billing.TierRule
	if pricingMode == identity.PricingModeCost {
		rateCharged = cm.CostRates
		timeCfg = cm.TimeConfig
		ctxTiers = cm.ContextTiers
	} else {
		rateCharged = ext.SaleRates
		timeCfg = ext.TimeConfig
		ctxTiers = ext.ContextTiers
	}
	if len(rateCharged) == 0 {
		return 0, "", errors.New("model rates empty")
	}
	tokenID := token.ID
	req := billing.RecordReq{
		UserID:          token.UserID,
		PricingMode:     pricingMode,
		TokenID:         &tokenID,
		ExternalModel:   model,
		AutoModelName:   routeRes.AutoModelName,
		InternalModelID: routeRes.InternalModelID,
		ChannelKeyID:    routeRes.ChannelKeyID,
		SessionID:       sessionID,
		SessionName:     sessionName,
		CallTime:        g.now(),
		Tokens:          usage,
		Rates:           rateCharged,
		TimeConfig:      timeCfg,
		ContextTiers:    ctxTiers,
		PreConsumed:     preConsumed,
		DurationMs:      durationMs,
		FirstTokenMs:    firstTokenMs,
	}
	// 成本单价始终携带，用于记录成本积分（差异展示，不参与扣减）。
	if len(cm.CostRates) > 0 {
		req.CostRates = billing.Rates(cm.CostRates)
	}
	rec, err := g.billing.Record(ctx, req)
	if err != nil {
		return 0, "", err
	}
	if rec == nil {
		return 0, "", nil
	}
	return rec.CreditsConsumed, rec.BillingID, nil
}

// recordFailure 记录一次失败调用（不扣积分，仅留痕）：上游错误/下游断开等原因。
// 返回关联的 billing_id（Record 失败或未装配计费时为空串），供 call_log 关联。
func (g *Gateway) recordFailure(ctx context.Context, routeRes *router.RouteResult,
	model string, token *identity.Token, pricingMode, sessionID, sessionName, reason string, durationMs *int64) string {
	if g.billing == nil {
		return ""
	}
	// 记账脱离请求取消：失败留痕同样不能因下游断开而丢失。
	ctx, cancel := billingCtx(ctx)
	defer cancel()
	tokenID := token.ID
	req := billing.RecordReq{
		UserID:          token.UserID,
		PricingMode:     pricingMode,
		TokenID:         &tokenID,
		ExternalModel:   model,
		AutoModelName:   routeRes.AutoModelName,
		InternalModelID: routeRes.InternalModelID,
		ChannelKeyID:    routeRes.ChannelKeyID,
		SessionID:       sessionID,
		SessionName:     sessionName,
		CallTime:        g.now(),
		Fail:            true,
		ErrorMessage:    reason,
		DurationMs:      durationMs,
	}
	rec, err := g.billing.Record(ctx, req)
	if err != nil {
		g.logError(routeRes.ChannelID, "record failure billing", err)
		return ""
	}
	if rec == nil {
		return ""
	}
	return rec.BillingID
}

// isInsufficient 判断是否余额不足（40201 CodeInsufficient）。
func isInsufficient(err error) bool {
	var ae *identity.APIError
	if errors.As(err, &ae) {
		return ae.Code == resp.CodeInsufficient
	}
	return false
}

// estTokens 估算一次请求的 token 成本（粗略按请求体字节数，用于 TPM 记账）。
func estTokens(body []byte) int {
	n := len(body) / 4
	if n < 1 {
		n = 1
	}
	if n > 500000 {
		n = 500000
	}
	return n
}

// contentText 从 content/reasoning 字段提取纯文本：兼容 string、null 与 [{type,text}] 数组（多模态）。
func contentText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(raw, &parts) == nil {
		var b strings.Builder
		for _, p := range parts {
			b.WriteString(p.Text)
		}
		return b.String()
	}
	return ""
}

// assistantParts 从拼装好的 assistant 消息 JSON 提取 content / reasoning_content / tool_calls。
func assistantParts(msg []byte) (content, reasoning string, tools []byte) {
	var m struct {
		Content          json.RawMessage   `json:"content"`
		ReasoningContent json.RawMessage   `json:"reasoning_content"`
		ToolCalls        []json.RawMessage `json:"tool_calls"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return "", "", nil
	}
	if len(m.ToolCalls) > 0 {
		tools, _ = json.Marshal(m.ToolCalls)
	}
	return contentText(m.Content), contentText(m.ReasoningContent), tools
}

// rawParts 从上游非流式响应体提取正文/推理/工具调用：聚合全部 choices（与流式拼装口径一致），
// 并兼容 anthropic-messages 的顶层 content[] 数组。
func rawParts(raw []byte) (content, reasoning string, tools []byte) {
	var r struct {
		Choices []struct {
			Message struct {
				Content          json.RawMessage   `json:"content"`
				ReasoningContent json.RawMessage   `json:"reasoning_content"`
				ToolCalls        []json.RawMessage `json:"tool_calls"`
			} `json:"message"`
		} `json:"choices"`
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &r); err != nil {
		return "", "", nil
	}
	var cb, rb strings.Builder
	var tcs []json.RawMessage
	for _, c := range r.Choices {
		cb.WriteString(contentText(c.Message.Content))
		rb.WriteString(contentText(c.Message.ReasoningContent))
		tcs = append(tcs, c.Message.ToolCalls...)
	}
	for _, c := range r.Content {
		cb.WriteString(c.Text)
	}
	if len(tcs) > 0 {
		tools, _ = json.Marshal(tcs)
	}
	return cb.String(), rb.String(), tools
}

// estimateInputTokens 估算请求输入 token：优先 messages 结构（tiktoken），
// 其次 openai-responses / embeddings 的 input 字段，最后按请求体字节数兜底。
func estimateInputTokens(body []byte, model string) int {
	var probe struct {
		Messages json.RawMessage `json:"messages"`
		Input    json.RawMessage `json:"input"`
	}
	_ = json.Unmarshal(body, &probe)
	if len(probe.Messages) > 0 && string(probe.Messages) != "null" {
		if n, err := countPromptTokens(body, model); err == nil {
			return n
		}
	}
	if len(probe.Input) > 0 && string(probe.Input) != "null" {
		if t := contentText(probe.Input); t != "" {
			return countTextTokens(t, model)
		}
	}
	return estTokens(body)
}

// missingUsageEstimate 在「上游未返回/无法解析 usage」时按可见内容估算 token 用量（AC-17 放宽）：
// 输入取请求 prompt 估算；输出取响应正文、推理与工具调用的文本估算，
// 三段口径下推理并入输出（不再单列）。估算时缓存读无法区分，一律计入输入。
func missingUsageEstimate(reqBody, rawResp []byte, model, content, reasoning string, tools []byte) billing.Usage {
	out := countTextTokens(content, model)
	out += countTextTokens(reasoning, model)
	if len(tools) > 0 {
		out += countTextTokens(string(tools), model)
	}
	if out == 0 && len(rawResp) > 0 {
		out = estTokens(rawResp)
	}
	return billing.Usage{
		Input:  int64(estimateInputTokens(reqBody, model)),
		Output: int64(out),
	}
}

// preConsumeBuffer 预扣缓冲系数：预扣按估算输入代入本地公式（含全部倍率）精确冻结，
// 结算按实际多退少补；不放大以放行余额刚够的合理用户。
const preConsumeBuffer = 1.0

// estimatePreConsumeCost 预扣金额估算（对齐 new-api pre-consume：预扣只用输入 token）：
// 以估算输入 token（tiktoken/字符级）代入本地计费公式——billing.EstimateBreakdown 与结算 Record 同一口径
// （时段系数 × 上下文分档 × 全部倍率 ÷ R）；输出 token 不参与预扣，留待结算按实际 usage 多退少补。
// 返回 (预扣决策快照, 可读公式, 错误)；单价缺失/估算不可用时返回 (零值,"",nil)（跳过预扣，由实际计费兜底）。
func (g *Gateway) estimatePreConsumeCost(ctx context.Context, model string, body []byte) (preCharge, string, error) {
	ext, err := g.models.GetByExternalName(ctx, model)
	if err != nil || len(ext.SaleRates) == 0 {
		return preCharge{}, "", nil
	}
	in, cerr := countPromptTokens(body, model)
	if cerr != nil {
		in = estTokens(body)
	}
	credits, timeCoeff, ctxCoeff, r, err := g.billing.EstimateBreakdown(ctx, billing.RecordReq{
		ExternalModel: model,
		Tokens:        billing.Usage{Input: int64(in)},
		Rates:         ext.SaleRates,
		TimeConfig:    ext.TimeConfig,
		ContextTiers:  ext.ContextTiers,
		CallTime:      g.now(),
	})
	if err != nil {
		return preCharge{}, "", err
	}
	formula := formatPreConsumeFormula(in, ext.SaleRates, timeCoeff, ctxCoeff, r, credits)
	return preCharge{Credits: credits, InputTokens: in, TimeCoeff: timeCoeff, CtxCoeff: ctxCoeff}, formula, nil
}

// formatPreConsumeFormula 组装与正常结算账单一致的三步式多行公式（Token × 单价 → 系数调整 → 积分换算），
// 供预扣失败落 failed 账单展示。预扣只按估算输入 token 代入（含全部倍率），
// 输出/缓存等零段自然省略（与正常账单展示规则一致），留待结算按实际 usage 计入。
func formatPreConsumeFormula(in int, rates billing.Rates, timeCoeff, ctxCoeff float64, r int64, credits float64) string {
	rate := rates["input"]
	sub1 := math.Round(float64(in)*rate*1e6) / 1e6
	sub2 := math.Round(sub1*timeCoeff*ctxCoeff*1e6) / 1e6
	final := sub2
	if r > 0 {
		final = math.Round(sub2/float64(r)*1e6) / 1e6
	}
	return fmt.Sprintf("Token × 单价（估算输入）\n"+
		"  输入 %d × %s = %s\n"+
		"  小计 = %s\n"+
		"系数调整：时段 ×%s，分档 ×%s → %s\n"+
		"积分换算：÷ %d (R) = %s 积分",
		in, fmtFloat(rate), fmtFloat(sub1), fmtFloat(sub1),
		fmtFloat(timeCoeff), fmtFloat(ctxCoeff), fmtFloat(sub2), r, fmtFloat(final))
}

// fmtFloat 格式化浮点：去尾零、不用科学计数（账单/公式展示风格，与 0.5/1/10000 等整数友好）。
func fmtFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// acquireLimit 依据 on_exceed 语义对一次请求执行窗口限流：
//   - REJECT：不满足直接拒；
//   - QUEUE：等待传入 wait，受 queue_timeout_ms 上限约束；超时或 ctx 取消则拒。
//
// 并发会话数不在此处（SessionRegistry 负责），RPM/TPM 按请求计。
func (g *Gateway) acquireLimit(ctx context.Context, rlCfg channel.RateLimitConfig, lim *channel.Limiter, tokens int) error {
	queueTimeout := time.Duration(rlCfg.QueueTimeoutMS) * time.Millisecond
	if queueTimeout <= 0 {
		queueTimeout = 5 * time.Second
	}
	deadline := g.now().Add(queueTimeout)

	for {
		ok, wait := lim.Allow(g.now(), tokens)
		if ok {
			return nil
		}
		if rlCfg.OnExceed == channel.OnExceedReject || wait <= 0 {
			return errRateLimited
		}
		// QUEUE：等待至 wait，若超出 queue_timeout 预算则拒。
		if g.now().Add(wait).After(deadline) {
			return errRateLimited
		}
		select {
		case <-ctx.Done():
			return errRateLimited
		case <-time.After(wait):
			if g.now().After(deadline) {
				return errRateLimited
			}
			// 继续重试
		}
	}
}

var errRateLimited = errors.New("rate limited")

// requireJSONObject 校验请求体是合法 JSON 对象（允许含数组等任意内部结构）。
func requireJSONObject(body []byte) error {
	var v map[string]json.RawMessage
	return json.Unmarshal(body, &v)
}

// parseRequestMeta 解析 model 与 stream 字段。
func parseRequestMeta(body []byte) (model string, stream bool, err error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return "", false, err
	}
	if raw, ok := m["stream"]; ok {
		_ = json.Unmarshal(raw, &stream)
	}
	if raw, ok := m["model"]; ok {
		_ = json.Unmarshal(raw, &model)
	}
	return model, stream, nil
}

// implicitSessionMessages 隐式会话取用的消息条数（取对话前几条，前缀稳定 → 同一对话映射同一会话）。
const implicitSessionMessages = 3

// sessionNameLimit 会话可读名称最大长度（rune）。
const sessionNameLimit = 40

// sessionNameFor 生成会话可读名称（通用 XML 结构规则，不依赖任何客户端特有标签白名单）：
//  1. 首个不在任何 XML 标签内的游离文本段；
//  2. 首个被 <user*> 标签（如 <user_input>/<user>）包围的文本段；
//  3. 首条正文（任意首个实质文本段）。
//
// 兼容纯文本 content 与 ChatGPT 多模态数组；无消息/无实质文本时回退原始标识。
// 手动改名走独立接口，网关自动续期不会覆盖。
func sessionNameFor(sessionRaw string, body []byte) string {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return fallbackSessionName(sessionRaw)
	}
	for _, m := range req.Messages {
		if m.Role != "user" {
			continue
		}
		if name := sessionNameFromText(contentTextBlocks(m.Content)); name != "" {
			return name
		}
	}
	return fallbackSessionName(sessionRaw)
}

// contentTextBlocks 兼容 string 与 ChatGPT 数组两种 content 形态，收集全部文本块（join 换行）。
func contentTextBlocks(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err != nil {
		return ""
	}
	var texts []string
	for _, p := range parts {
		if p.Type == "text" || p.Type == "input_text" {
			if strings.TrimSpace(p.Text) != "" {
				texts = append(texts, p.Text)
			}
		}
	}
	return strings.Join(texts, "\n")
}

// xmlTextPart 按 XML 标签切分后的文本片段：tag 为标签名（空串=游离于标签外的正文）。
type xmlTextPart struct {
	tag  string
	body string
}

// xmlSplit 把文本按 XML 标签切分为片段：标签块与其之间的游离文本分开，保持原文顺序。
// Go regexp（RE2）不支持反向引用，这里手写配对扫描 <name…>…</name>。
func xmlSplit(s string) []xmlTextPart {
	var parts []xmlTextPart
	i, n := 0, len(s)
	appendFree := func(free string) {
		if t := strings.TrimSpace(free); t != "" {
			parts = append(parts, xmlTextPart{body: t})
		}
	}
	for i < n {
		lt := strings.IndexByte(s[i:], '<')
		if lt < 0 {
			break
		}
		lt += i
		if lt > i {
			appendFree(s[i:lt])
		}
		gt := strings.IndexByte(s[lt:], '>')
		if gt < 0 { // 无闭合 >：剩余均为游离文本
			appendFree(s[lt:])
			return parts
		}
		hdr := strings.TrimSpace(s[lt+1 : lt+gt])
		if hdr == "" || strings.HasPrefix(hdr, "/") || strings.HasPrefix(hdr, "!") || strings.HasPrefix(hdr, "?") {
			// 闭合标签 / 注释 / 声明：按游离文本跳过本对 < >
			parts = append(parts, xmlTextPart{body: strings.TrimSpace(s[lt : lt+gt+1])})
			i = lt + gt + 1
			continue
		}
		name := hdr
		if sp := strings.IndexAny(hdr, " \t\r\n/"); sp >= 0 {
			name = hdr[:sp]
		}
		if !isXmlName(name) {
			parts = append(parts, xmlTextPart{body: strings.TrimSpace(s[lt : lt+gt+1])})
			i = lt + gt + 1
			continue
		}
		closeTag := "</" + name + ">"
		ci := strings.Index(s[lt+gt+1:], closeTag)
		if ci < 0 { // 无配对闭合：开始标签按游离文本处理
			parts = append(parts, xmlTextPart{body: strings.TrimSpace(s[lt : lt+gt+1])})
			i = lt + gt + 1
			continue
		}
		ci += lt + gt + 1
		if inner := strings.TrimSpace(s[lt+gt+1 : ci]); inner != "" {
			parts = append(parts, xmlTextPart{tag: name, body: inner})
		}
		i = ci + len(closeTag)
	}
	if i < n {
		appendFree(s[i:])
	}
	return parts
}

// isXmlName 是否为合法 XML 标签名（首字符字母/下划线，后续可含数字、连字符、点）。
func isXmlName(s string) bool {
	if s == "" {
		return false
	}
	for k, c := range s {
		switch {
		case k == 0:
			if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
				return false
			}
		case c == '_' || c == '-' || c == '.' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9':
		default:
			return false
		}
	}
	return true
}

// isUserTag 判断标签名是否为 user 语义（<user> / <user_input> / <user-message> 等）。
func isUserTag(tag string) bool {
	return tag == "user" || strings.HasPrefix(tag, "user_") || strings.HasPrefix(tag, "user-")
}

// sessionNameFromText 按三段规则选取会话名称；无候选返回空串。
func sessionNameFromText(text string) string {
	parts := xmlSplit(text)
	if len(parts) == 0 {
		return ""
	}
	// 1) 首个不在任何 XML 标签内的文本段
	for _, p := range parts {
		if p.tag == "" {
			return normalizeSessionName(p.body)
		}
	}
	// 2) 首个被 <user*> 标签包围的文本段
	for _, p := range parts {
		if isUserTag(p.tag) {
			return normalizeSessionName(p.body)
		}
	}
	// 3) 首条正文
	return normalizeSessionName(parts[0].body)
}

// normalizeSessionName 折叠空白并截断到会话名称上限；空串返回空（由调用方兜底）。
func normalizeSessionName(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	runes := []rune(s)
	if len(runes) > sessionNameLimit {
		return string(runes[:sessionNameLimit]) + "…"
	}
	return s
}

// fallbackSessionName 无有效 user 文本时的兜底名称。
// 隐式会话种子（64 位 hex）对人类不可读 → 占位名；显式会话标识直接截断展示。
func fallbackSessionName(sessionRaw string) string {
	trimmed := strings.TrimSpace(sessionRaw)
	if trimmed == "" {
		return "未命名会话"
	}
	if len(trimmed) == 64 {
		if _, err := hex.DecodeString(trimmed); err == nil {
			return "未命名会话"
		}
	}
	return normalizeSessionName(trimmed)
}

// implicitSessionSeed 无客户端 x-session-id 时，以请求体前几条消息（role+content）生成会话种子。
// 同一对话的后续轮次前缀不变，隐式会话稳定；返回空串表示无消息（此时保持无会话语义）。
func implicitSessionSeed(body []byte) string {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &req); err != nil || len(req.Messages) == 0 {
		return ""
	}
	n := len(req.Messages)
	if n > implicitSessionMessages {
		n = implicitSessionMessages
	}
	var sb strings.Builder
	for i := 0; i < n; i++ {
		sb.WriteString(req.Messages[i].Role)
		sb.WriteByte(':')
		sb.Write(req.Messages[i].Content)
		sb.WriteByte('|')
	}
	h := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(h[:])
}

// rewriteModel 在原请求体的 JSON 上替换 model 为内部模型 ID，其余字段字节级保留。
func rewriteModel(body []byte, internalModel string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if _, ok := m["model"]; !ok {
		return nil, errors.New("missing model")
	}
	b, _ := json.Marshal(internalModel)
	m["model"] = b
	return json.Marshal(m)
}

// feedbackSuccess 构造成功反馈（含耗时）。
func feedbackSuccess(start time.Time) channel.Feedback {
	return channel.Feedback{IsSuccess: true, LatencyMS: latencyMS(start), Now: time.Now()}
}

// feedbackFail 构造失败反馈。
func feedbackFail(start time.Time) channel.Feedback {
	return channel.Feedback{IsSuccess: false, LatencyMS: latencyMS(start), Now: time.Now()}
}

func latencyMS(start time.Time) int64 {
	return time.Since(start).Milliseconds()
}

func (g *Gateway) logError(channelID int64, msg string, err error) {
	if g.logger != nil {
		g.logger.Error(msg, "channel_id", channelID, "err", err)
	}
}
