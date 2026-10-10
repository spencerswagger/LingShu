package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	msgInvalidToken  = "令牌无效或已过期"
	msgModelNotFound = "模型 %s 未配置或已停用"
	msgNoRoute       = "当前请求的语义标签无可用的服务渠道"
	msgNoAvailable   = "无可用渠道，请稍后重试"
	msgRateLimited   = "请求过于频繁，请稍后重试"
	msgUpstreamDown  = "上游服务暂时不可用"
	msgInsufficient  = "积分余额不足，请先充值"
	msgInternal      = "服务器内部错误"
)

// handleModels GET /v1/models 返回启用中的对外模型列表（OpenAI 兼容格式）。
// 鉴权与 serve 一致：校验 Bearer 令牌；列表为空时返回空 data 而非报错。
func (g *Gateway) handleModels(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	auth := r.Header.Get("Authorization")
	plain, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || plain == "" {
		writeOpenAIError(w, http.StatusUnauthorized, msgInvalidToken)
		return
	}
	if _, err := g.tokens.LookupByPlain(ctx, plain); err != nil {
		writeOpenAIError(w, http.StatusUnauthorized, msgInvalidToken)
		return
	}

	data := make([]map[string]any, 0)
	if g.listEnabled != nil {
		list, err := g.listEnabled(ctx)
		if err != nil {
			g.logError(0, "list enabled models", err)
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

// serve 是 /v1/{endpoint} 统一入口的核心热路径。
func (g *Gateway) serve(w http.ResponseWriter, r *http.Request, endpoint string) {
	start := time.Now()
	ctx := r.Context()
	l := logger.WithRequestID(ctx, g.logger)

	// 1) 认证：Authorization: Bearer <plain token>。
	auth := r.Header.Get("Authorization")
	plain, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok || plain == "" {
		writeOpenAIError(w, http.StatusUnauthorized, msgInvalidToken)
		return
	}
	token, err := g.tokens.LookupByPlain(ctx, plain)
	if err != nil {
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
		writeOpenAIError(w, http.StatusBadRequest, "请求体过大或无法读取")
		return
	}
	if err := requireJSONObject(body); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "请求体格式错误")
		return
	}
	model, isStream, err := parseRequestMeta(body)
	if err != nil || model == "" {
		writeOpenAIError(w, http.StatusBadRequest, "请求体必须包含 model 字段")
		return
	}

	// 3) 令牌标签 → TagKV。
	var tagKV map[string]string
	if g.tagKV != nil {
		kv, terr := g.tagKV(ctx, token.TagID)
		if terr != nil {
			l.Error("resolve tag kv failed", "user_id", token.UserID, "tag_id", token.TagID, "err", terr)
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
	routeSpec := router.RouteSpec{
		ExternalModel: model,
		TagKV:         tagKV,
		TagID:         token.TagID,
		SessionKey:    sessionID,
	}
	routes, err := g.router.RouteAll(ctx, routeSpec)
	if err != nil {
		var re *router.RouteError
		if errors.As(err, &re) {
			switch re.Kind {
			case router.KindModelNotFound:
				// 未进入渠道也留痕：保证「每次请求必有一条账单记录」（模型未配置/停用）。
				g.recordGatewayReject(ctx, model, token, "", sessionID, fmt.Sprintf(msgModelNotFound, model))
				writeOpenAIError(w, http.StatusNotFound, fmt.Sprintf(msgModelNotFound, model))
			case router.KindNoRoute:
				g.recordGatewayReject(ctx, model, token, "", sessionID, msgNoRoute)
				writeOpenAIError(w, http.StatusServiceUnavailable, msgNoRoute)
			default: // KindNoAvailable
				g.recordGatewayReject(ctx, model, token, "", sessionID, msgNoAvailable)
				writeOpenAIError(w, http.StatusServiceUnavailable, msgNoAvailable)
			}
			return
		}
		l.Error("route failed", "model", model, "err", err)
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
	var preConsumed float64
	if g.credit != nil {
		if bal, berr := g.credit.Balance(ctx, token.UserID); berr == nil && bal <= 0 {
			l.Warn("zero balance rejected", "user_id", token.UserID, "balance", bal)
			g.recordGatewayReject(ctx, model, token, pricingMode, sessionID, "积分余额不足（账户余额为 0）")
			writeOpenAIError(w, http.StatusPaymentRequired, msgInsufficient)
			return
		}
		est, formula, eerr := g.estimatePreConsumeCost(ctx, model, body)
		if eerr != nil {
			l.Error("estimate preconsume failed", "user_id", token.UserID, "model", model, "err", eerr)
			writeOpenAIError(w, http.StatusInternalServerError, msgInternal)
			return
		}
		if est > 0 {
			est = est * preConsumeBuffer // 预扣 = 估算输入代入本地公式 × 缓冲；输出留待结算按实际多退少补
			if err := g.credit.PreConsume(ctx, token.UserID, est, "", sessionID, "预扣费"); err != nil {
				// 失败也落 failed 账单：余额不足等预扣拒绝必须留痕（含公式与预扣积分），
				// 便于用户核对「为什么没扣成」与对账。
				msg := fmt.Sprintf("积分余额不足，预扣需 %.6f 积分；公式：\n%s", est, formula)
				if len(routes) > 0 {
					r0 := routes[0]
					g.recordFailure(ctx, &router.RouteResult{
						ChannelID:       r0.ChannelID,
						ChannelKeyID:    r0.ChannelKeyID,
						InternalModelID: r0.InternalModelID,
						ModelRowID:      r0.ModelRowID,
						ExternalModelID: r0.ExternalModelID,
						Revision:        r0.Revision,
					}, model, token, pricingMode, sessionID, sessionName, msg, nil)
				}
				if isInsufficient(err) {
					l.Warn("preconsume insufficient", "user_id", token.UserID, "need", est, "err", err)
					writeOpenAIError(w, http.StatusPaymentRequired, msgInsufficient)
				} else {
					l.Error("preconsume failed", "user_id", token.UserID, "need", est, "err", err)
					writeOpenAIError(w, http.StatusInternalServerError, msgInternal)
				}
				return
			}
			preConsumed = est
		}
	} else if g.balance != nil {
		bal, berr := g.balance(ctx, token.UserID)
		if berr != nil {
			l.Warn("resolve balance failed, skip precheck", "user_id", token.UserID, "err", berr)
		} else if bal <= 0 {
			g.recordGatewayReject(ctx, model, token, pricingMode, sessionID, "积分余额不足（账户余额为 0）")
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
	sessObj, hasSession := sess.Lookup(sessionID)
	// 请求增量 diff：计算本次请求的增量消息与全量指纹（call_log 采集用，
	// 新会话也在此初始化指纹，由下方 Attach 写入会话）。
	reqMsgs, msgFps := g.captureReqIncrement(sessObj, body)
	sawLimit := false  // 出现过限流类失败（本地限流 或 上游 429）
	sawOther := false  // 出现过非限流类的可重试失败（上游 401/402/403/404/5xx、网络类）
	attempted := false // 是否至少有一次候选真正发出上游调用（用于全被本地拦截时的账单兜底）
	// 决策轨迹：每个候选渠道的处理结果与跳过原因（call_log decision 用，雪花 ID 以字符串序列化）。
	attempts := make([]map[string]any, 0, len(routes))
	for _, cr := range routes {
		order := len(attempts) + 1
		attempt := func(reason string) map[string]any {
			return map[string]any{
				"order":             order,
				"channel_id":        strconv.FormatInt(cr.ChannelID, 10),
				"channel_key_id":    strconv.FormatInt(cr.ChannelKeyID, 10),
				"internal_model_id": cr.InternalModelID,
				"reason":            reason,
			}
		}
		// 密钥为运行时实体：路由候选已带 ChannelKeyID，这里做入口复查（状态可能已被灰度切换）。
		// 统一读 Machine.State()：401 自动禁用后立即生效（kr.Key.State 是落库快照，可能滞后）。
		kr, ok := g.channels.GetKeyRuntime(cr.ChannelKeyID)
		if !ok || kr.Machine.State() == channel.StateDisabled {
			attempts = append(attempts, attempt("禁用"))
			continue
		}
		mr, mok := g.channels.GetModelRuntime(cr.ChannelID, cr.ModelRowID)
		if !mok {
			attempts = append(attempts, attempt("无模型运行时"))
			continue
		}
		if mr.Machine.State() == channel.StateDisabled {
			attempts = append(attempts, attempt("禁用"))
			continue
		}
		rt, ok := g.channels.GetRuntime(cr.ChannelID)
		if !ok {
			l.Error("channel not in runtime", "channel_id", cr.ChannelID)
			attempts = append(attempts, attempt("无渠道运行时"))
			continue
		}
		// 密钥凭据解密失败/为空时，Manager 已把 CredentialPlain 清空（绝不外发密文）。
		if kr.CredentialPlain == "" {
			l.Error("channel key credential unavailable", "channel_key_id", cr.ChannelKeyID, "channel_id", cr.ChannelID)
			g.channels.FeedResult(cr.ChannelKeyID, cr.ModelRowID, channel.Feedback{IsSuccess: false, Now: g.now()})
			attempted = true // 该分支已自行落 failed 账单，终态不再兜底
			bid := g.recordFailure(ctx, &router.RouteResult{ChannelID: cr.ChannelID, ChannelKeyID: cr.ChannelKeyID, InternalModelID: cr.InternalModelID,
				ModelRowID: cr.ModelRowID, ExternalModelID: cr.ExternalModelID, Revision: cr.Revision},
				model, token, "", sessionID, sessionName, "密钥凭据不可用", msSince(start))
			g.writeCallLog(ctx, bid, "", token, routeResFor(cr), model, "", sessionID, reqMsgs,
				"error", "", "failed", "密钥凭据不可用", msSince(start), nil, nil)
			attempts = append(attempts, attempt("凭据不可用"))
			continue
		}
		prov, ok := g.providerFactory(rt.Channel.Protocol, endpoint)
		if !ok {
			l.Error("unsupported protocol", "protocol", rt.Channel.Protocol)
			attempts = append(attempts, attempt("协议不支持"))
			continue
		}
		routeRes := &router.RouteResult{ChannelID: cr.ChannelID, ChannelKeyID: cr.ChannelKeyID, InternalModelID: cr.InternalModelID,
			ModelRowID: cr.ModelRowID, ExternalModelID: cr.ExternalModelID, Revision: cr.Revision}

		// 会话：既有会话命中本候选（密钥+模型）→ 续行；否则为新会话，但 Attach 放在限流通过之后
		// （限流拒绝不占会话名额；仅当限流放行、容量允许时新建会话并 +1 计数）。
		existing := hasSession && sessObj != nil &&
			sessObj.ChannelKeyID == cr.ChannelKeyID && sessObj.InternalModelID == cr.InternalModelID

		// 限流：模型级窗口 → 密钥级窗口（任一超限 → 下一候选）。
		if err := g.acquireLimit(ctx, mr.Model.RateLimit, mr.Limiter, estTokens(body)); err != nil {
			sawLimit = true
			attempts = append(attempts, attempt("限流"))
			continue
		}
		if err := g.acquireLimit(ctx, rt.Channel.RateLimit, kr.Limiter, estTokens(body)); err != nil {
			sawLimit = true
			attempts = append(attempts, attempt("限流"))
			continue
		}

		// 会话创建：限流已通过，按密钥校验并发上限。槽满只影响该候选 → failover 下一候选
		//（其他密钥/渠道/内部模型行仍有插槽时照常续用），不归为「限流类 429」；
		// 全部候选都因会话满失败时由下面终结分支统一回报 503（无可用渠道）。
		if !existing && sessionID != "" {
			ttl := time.Duration(g.channels.SessionTTL(cr.ChannelID)) * time.Minute
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
				attempts = append(attempts, attempt("会话满"))
				continue // 该密钥并发会话满 → 下一候选
			}
		}

		// 改写 body.model 为内部模型 ID（各候选渠道的 internal_model_id 可能不同）。
		rewritten, rerr := rewriteModel(body, routeRes.InternalModelID)
		if rerr != nil {
			writeOpenAIError(w, http.StatusBadRequest, "请求体格式错误")
			return
		}

		var out *fwdOutcome
		if isStream {
			attempted = true
			out = g.serveStream(w, r, start, prov, rt, routeRes, kr.CredentialPlain, rewritten, model, token, pricingMode, sessionID, sessionName, preConsumed, reqMsgs)
		} else {
			attempted = true
			out = g.serveNonStream(w, r, start, prov, rt, routeRes, kr.CredentialPlain, rewritten, model, token, pricingMode, sessionID, sessionName, preConsumed, reqMsgs)
		}
		if out.responded {
			if sessionID != "" {
				sess.Renew(sessionID, time.Duration(g.channels.SessionTTL(cr.ChannelID))*time.Minute)
			}
			// 实际消费差额已在 recordBilling → Record 内多退少补；若仅落 failed、
			// 未实际消费（上游/转发失败），退回本次预扣。
			if out.refundPre {
				g.refundAllPreConsume(ctx, token.UserID, preConsumed, "请求失败退回")
			}
			switch {
			case out.billFailed:
				attempts = append(attempts, attempt("计费失败"))
			case out.refundPre:
				attempts = append(attempts, attempt("上游失败"))
			default:
				attempts = append(attempts, attempt("success"))
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
			"attempts":     attempts,
			"time_coeff":   0,
			"ctx_coeff":    0,
			"pre_consumed": preConsumed,
			"result":       "rejected",
		}
	}
	if sawLimit && !sawOther {
		g.refundAllPreConsume(ctx, token.UserID, preConsumed, "请求失败退回（限流）")
		// 全部候选被本地拦截（限流/并发/禁用等，未发出任何上游调用）时补一条失败账单；
		// 已发出上游调用并落账的（如 429 重试）不重复落账。
		if !attempted {
			bid := g.recordGatewayReject(ctx, model, token, pricingMode, sessionID, msgRateLimited)
			g.writeCallLog(ctx, bid, "", token, &router.RouteResult{}, model, pricingMode, sessionID,
				reqMsgs, "error", "", "failed", msgRateLimited, msSince(start), nil, rejectDecision())
		}
		writeOpenAIError(w, http.StatusTooManyRequests, msgRateLimited)
		return
	}
	g.refundAllPreConsume(ctx, token.UserID, preConsumed, "请求失败退回（无可用渠道）")
	if !attempted {
		bid := g.recordGatewayReject(ctx, model, token, pricingMode, sessionID, msgNoAvailable)
		g.writeCallLog(ctx, bid, "", token, &router.RouteResult{}, model, pricingMode, sessionID,
			reqMsgs, "error", "", "failed", msgNoAvailable, msSince(start), nil, rejectDecision())
	}
	writeOpenAIError(w, http.StatusServiceUnavailable, msgNoAvailable)
}

// recordGatewayReject 记录一次未进入渠道的网关拒绝（零余额/未路由/候选全部被拦截等）为 failed 账单，
// 保证「每次请求必有一条账单记录」；渠道维度留空（未实际路由到任何渠道）。
// 返回关联的 billing_id（Record 失败或未装配计费时为空串），供 call_log 关联。
func (g *Gateway) recordGatewayReject(ctx context.Context, model string, token *identity.Token, pricingMode, sessionID, reason string) string {
	if g.billing == nil || token == nil {
		return ""
	}
	return g.recordFailure(ctx, &router.RouteResult{}, model, token, pricingMode, sessionID, "", reason, nil)
}

// routeResFor 由路由候选构造最小 RouteResult（入口复查失败——凭据不可用等——用于账单与日志关联）。
func routeResFor(cr *router.ChannelRoute) *router.RouteResult {
	return &router.RouteResult{
		ChannelID:       cr.ChannelID,
		ChannelKeyID:    cr.ChannelKeyID,
		InternalModelID: cr.InternalModelID,
		ModelRowID:      cr.ModelRowID,
		ExternalModelID: cr.ExternalModelID,
		Revision:        cr.Revision,
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
	responded  bool
	retry      bool
	is429      bool
	charged    float64
	billFailed bool
	refundPre  bool
}

// serveNonStream 非流式转发：2xx 先计费再响应。
// 响应未写出且上游未计费的失败（构建/连接失败、401/402/403/404/429、5xx 等）返回 retry，
// 由调用方切换到下一候选；其余失败（请求已送达后的超时、已收 2xx 后读体失败、其余 4xx、
// 504/524）直接写出响应，不再转移。
func (g *Gateway) serveNonStream(w http.ResponseWriter, r *http.Request, start time.Time,
	prov Provider, rt *channel.RuntimeChannel, routeRes *router.RouteResult, cred string,
	body []byte, model string, token *identity.Token, pricingMode, sessionID, sessionName string,
	preConsumed float64, reqMsgs []json.RawMessage) *fwdOutcome {

	ctx := r.Context()
	req, err := prov.BuildUpstreamRequest(ctx, rt.Channel.BaseURL, cred, body, false)
	if err != nil {
		// 本地构建失败，未发出任何上游调用 → 可安全转移。
		g.logError(rt.Channel.ID, "build upstream request", err)
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, channel.Feedback{IsSuccess: false, Now: g.now()})
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, "构建上游请求失败", msSince(start))
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", "构建上游请求失败", "failed", "构建上游请求失败", msSince(start), nil, nil)
		return &fwdOutcome{retry: true}
	}
	req, wrote := withWriteTrace(req)
	up, err := g.client.Do(req)
	if err != nil {
		g.logError(rt.Channel.ID, "upstream call failed", err)
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, channel.Feedback{IsSuccess: false, Now: g.now()})
		if !wrote.Load() {
			// 请求体未送达上游：上游不可能计费 → 可安全转移。
			bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, "上游连接失败", msSince(start))
			g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
				"error", "上游连接失败", "failed", "上游连接失败", msSince(start), nil, nil)
			return &fwdOutcome{retry: true}
		}
		// 请求已送达后失败（多为响应超时）：上游可能已生成并计费，转移会导致上游重复计费 → 不转移。
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, "上游请求失败（已送达，不转移）", msSince(start))
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", "上游请求失败（已送达，不转移）", "failed", "上游请求失败（已送达，不转移）", msSince(start), nil, nil)
		writeOpenAIError(w, http.StatusBadGateway, msgUpstreamDown)
		return &fwdOutcome{responded: true, refundPre: true}
	}
	defer up.Body.Close()

	switch {
	case up.StatusCode >= 200 && up.StatusCode < 300:
		raw, err := io.ReadAll(up.Body)
		if err != nil {
			// 已收到 2xx：上游已完成生成并计费，转移会造成上游重复计费 → 不转移。
			g.logError(rt.Channel.ID, "read upstream body", err)
			bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, "读取上游响应失败", msSince(start))
			g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
				"error", "读取上游响应失败", "failed", "读取上游响应失败", msSince(start), nil, nil)
			writeOpenAIError(w, http.StatusBadGateway, msgUpstreamDown)
			return &fwdOutcome{responded: true, refundPre: true}
		}
		dur := msSince(start)
		usage, _ := prov.ExtractUsage(raw)

		// 非流式：先计费再响应。余额不足 → 402，不返成功 body。
		// 注意：上游已成功返回 2xx，计费失败（含余额不足）属用户侧/网关侧问题，
		// 不反馈渠道失败，避免健康渠道被误判为故障（DRAIN_ONLY）。
		out := &fwdOutcome{responded: true}
		if usageZero(usage) {
			// 上游未返回 usage：按输入估算记账（保证每次调用都消耗积分，不允许赊账）。
			usage = billing.Usage{Input: int64(estTokens(body))}
		}
		ch, bid, berr := g.recordBilling(ctx, routeRes, model, token, usage, pricingMode, sessionID, sessionName, preConsumed, dur, nil)
		if berr != nil {
			out.billFailed = true
			if isInsufficient(berr) {
				g.logger.Warn("billing insufficient", "user_id", token.UserID, "err", berr)
				// billing.Record 内部已落一条 failed（含实际 tokens/rates/系数与余额不足原因），
				// 网关不再重复落账，保证每次请求恰好一条记录。
				g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
					"non_stream", string(raw), "failed", "积分余额不足，计费失败", dur, nil, nil)
				writeOpenAIError(w, http.StatusPaymentRequired, msgInsufficient)
				return &fwdOutcome{responded: true, billFailed: true}
			}
			g.logError(rt.Channel.ID, "record billing failed", berr)
			g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
				"non_stream", string(raw), "failed", "计费失败", dur, nil, nil)
			writeOpenAIError(w, http.StatusInternalServerError, msgInternal)
			return &fwdOutcome{responded: true, billFailed: true}
		}
		out.charged = ch
		// 调用日志：成功路径（已知 billingID 且响应写出前）。
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"non_stream", string(raw), "completed", "", dur, nil, nil)

		// 渠道透明：只保留下游 Content-Type，清掉上游杂项（request-id 等），不下发任何 X-Channel* 头。
		h := w.Header()
		h.Set("Content-Type", up.Header.Get("Content-Type"))
		w.WriteHeader(up.StatusCode)
		_, _ = w.Write(raw)
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, feedbackSuccess(start))
		return out

	case up.StatusCode == http.StatusTooManyRequests:
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, channel.Feedback{Is429: true, Now: g.now()})
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, "上游返回 429（触发限流）", msSince(start))
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", "", "failed", "上游返回 429（触发限流）", msSince(start), nil, nil)
		return &fwdOutcome{retry: true, is429: true}
	case up.StatusCode == http.StatusUnauthorized || up.StatusCode == http.StatusForbidden:
		// 凭据可能失效：先转移下一候选；连续失败达阈值由状态机熔断（channel.Machine）。
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, channel.Feedback{IsAuthFailure: true, Now: g.now()})
		errMsg := fmt.Sprintf("上游返回 %d（渠道凭据可能失效）", up.StatusCode)
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, errMsg, msSince(start))
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", "", "failed", errMsg, msSince(start), nil, nil)
		return &fwdOutcome{retry: true}
	default:
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, feedbackFail(start))
		errMsg := fmt.Sprintf("上游返回 %d", up.StatusCode)
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, errMsg, msSince(start))
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", "", "failed", errMsg, msSince(start), nil, nil)
		if retryableUpstreamStatus(up.StatusCode) {
			return &fwdOutcome{retry: true}
		}
		writeOpenAIError(w, http.StatusBadGateway, msgUpstreamDown)
		return &fwdOutcome{responded: true, refundPre: true}
	}
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
func (g *Gateway) serveStream(w http.ResponseWriter, r *http.Request, start time.Time,
	prov Provider, rt *channel.RuntimeChannel, routeRes *router.RouteResult, cred string,
	body []byte, model string, token *identity.Token, pricingMode, sessionID, sessionName string,
	preConsumed float64, reqMsgs []json.RawMessage) *fwdOutcome {

	ctx := r.Context()
	req, err := prov.BuildUpstreamRequest(ctx, rt.Channel.BaseURL, cred, body, true)
	if err != nil {
		// 本地构建失败，未发出任何上游调用 → 可安全转移。
		g.logError(rt.Channel.ID, "build upstream stream request", err)
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, channel.Feedback{IsSuccess: false, Now: g.now()})
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, "构建上游流式请求失败", msSince(start))
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", "构建上游流式请求失败", "failed", "构建上游流式请求失败", msSince(start), nil, nil)
		return &fwdOutcome{retry: true}
	}
	req, wrote := withWriteTrace(req)
	up, err := g.streamClient.Do(req)
	if err != nil {
		g.logError(rt.Channel.ID, "upstream stream call failed", err)
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, channel.Feedback{IsSuccess: false, Now: g.now()})
		if !wrote.Load() {
			// 请求体未送达上游：上游不可能计费 → 可安全转移。
			bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, "上游流式连接失败", msSince(start))
			g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
				"error", "上游流式连接失败", "failed", "上游流式连接失败", msSince(start), nil, nil)
			return &fwdOutcome{retry: true}
		}
		// 请求已送达后失败（多为响应超时）：上游可能已生成并计费，转移会导致上游重复计费 → 不转移。
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, "上游流式请求失败（已送达，不转移）", msSince(start))
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", "上游流式请求失败（已送达，不转移）", "failed", "上游流式请求失败（已送达，不转移）", msSince(start), nil, nil)
		writeOpenAIError(w, http.StatusBadGateway, msgUpstreamDown)
		return &fwdOutcome{responded: true, refundPre: true}
	}
	defer up.Body.Close()

	switch {
	case up.StatusCode >= 200 && up.StatusCode < 300:
		// 记录首个 SSE chunk 写出时机（首字耗时）。
		ft := &firstTokenWriter{ResponseWriter: w}
		usage, assistantMsg, herr := prov.HandleStream(ctx, up, ft)
		dur := msSince(start)
		if herr != nil && !errors.Is(herr, context.Canceled) {
			g.logError(rt.Channel.ID, "stream handle failed", herr)
			bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, "流式传输中断", dur)
			g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
				"error", string(assistantMsg), "failed", "流式传输中断", dur, ft.firstTokenMs(start), nil)
		}
		// 流结束后计费（预扣已在前置阶段完成；此处按实际扣费，差额由 serve 结算退补）。
		// 此时请求 context 已随响应结束被取消：用脱离取消、带超时的 context，避免计费被误中断。
		billCtx, cancelBill := billingCtx(ctx)
		defer cancelBill()
		out := &fwdOutcome{responded: true}
		if usageZero(usage) {
			// 上游未返回 usage（流式响应无 usage 块）：按输入估算记账，保证每次调用都消耗积分。
			usage = billing.Usage{Input: int64(estTokens(body))}
		}
		ch, bid, berr := g.recordBilling(billCtx, routeRes, model, token, usage, pricingMode, sessionID, sessionName, preConsumed, dur, ft.firstTokenMs(start))
		if berr != nil {
			g.logger.Error("stream billing failed", "user_id", token.UserID,
				"channel_id", rt.Channel.ID, "model", model, "err", berr)
			out.billFailed = true // 已响应内容无法撤回：预扣不退（按预扣计费），不允许赊账
		} else {
			out.charged = ch
		}
		// 调用日志：流结束后采集（含 herr 失败分支的 resp_kind=error；正常/billFailed 为 stream）。
		switch {
		case herr != nil && !errors.Is(herr, context.Canceled):
			// 失败分支已在上面记录（resp_kind=error）。
		case out.billFailed:
			g.writeCallLog(billCtx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
				"stream", string(assistantMsg), "failed", "流式计费失败", dur, ft.firstTokenMs(start), nil)
		default:
			g.writeCallLog(billCtx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
				"stream", string(assistantMsg), "completed", "", dur, ft.firstTokenMs(start), nil)
		}
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, feedbackSuccess(start))
		return out
	case up.StatusCode == http.StatusTooManyRequests:
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, channel.Feedback{Is429: true, Now: g.now()})
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, "上游返回 429（触发限流）", msSince(start))
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", "", "failed", "上游返回 429（触发限流）", msSince(start), nil, nil)
		return &fwdOutcome{retry: true, is429: true}
	case up.StatusCode == http.StatusUnauthorized || up.StatusCode == http.StatusForbidden:
		// 凭据可能失效：先转移下一候选；连续失败达阈值由状态机熔断（channel.Machine）。
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, channel.Feedback{IsAuthFailure: true, Now: g.now()})
		errMsg := fmt.Sprintf("上游返回 %d（渠道凭据可能失效）", up.StatusCode)
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, errMsg, msSince(start))
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", "", "failed", errMsg, msSince(start), nil, nil)
		return &fwdOutcome{retry: true}
	default:
		g.channels.FeedResult(routeRes.ChannelKeyID, routeRes.ModelRowID, feedbackFail(start))
		errMsg := fmt.Sprintf("上游返回 %d", up.StatusCode)
		bid := g.recordFailure(ctx, routeRes, model, token, pricingMode, sessionID, sessionName, errMsg, msSince(start))
		g.writeCallLog(ctx, bid, "", token, routeRes, model, pricingMode, sessionID, reqMsgs,
			"error", "", "failed", errMsg, msSince(start), nil, nil)
		if retryableUpstreamStatus(up.StatusCode) {
			return &fwdOutcome{retry: true}
		}
		writeOpenAIError(w, http.StatusBadGateway, msgUpstreamDown)
		return &fwdOutcome{responded: true, refundPre: true}
	}
}

// firstTokenWriter 包装 ResponseWriter，记录首个字节写出的时间（SSE 首字耗时）。
type firstTokenWriter struct {
	http.ResponseWriter
	first time.Time
}

func (w *firstTokenWriter) Write(p []byte) (int, error) {
	if w.first.IsZero() {
		w.first = time.Now()
	}
	return w.ResponseWriter.Write(p)
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

// preConsumeBuffer 预扣缓冲系数：预扣按估算输入代入本地公式（含全部倍率）精确冻结，
// 结算按实际多退少补；不放大以放行余额刚够的合理用户。
const preConsumeBuffer = 1.0

// estimatePreConsumeCost 预扣金额估算（对齐 new-api pre-consume：预扣只用输入 token）：
// 以估算输入 token（tiktoken/字符级）代入本地计费公式——billing.EstimateBreakdown 与结算 Record 同一口径
// （时段系数 × 上下文分档 × 全部倍率 ÷ R）；输出 token 不参与预扣，留待结算按实际 usage 多退少补。
// 返回 (预扣积分, 可读公式, 错误)；单价缺失/估算不可用时返回 (0,"",nil)（跳过预扣，由实际计费兜底）。
func (g *Gateway) estimatePreConsumeCost(ctx context.Context, model string, body []byte) (float64, string, error) {
	ext, err := g.models.GetByExternalName(ctx, model)
	if err != nil || len(ext.SaleRates) == 0 {
		return 0, "", nil
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
		return 0, "", err
	}
	formula := formatPreConsumeFormula(in, ext.SaleRates, timeCoeff, ctxCoeff, r, credits)
	return credits, formula, nil
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

// sessionNameFor 生成会话可读名称：优先取首条 user 消息内容摘要；无消息时取原始标识。
// 手动改名走独立接口，网关自动续期不会覆盖。
func sessionNameFor(sessionRaw string, body []byte) string {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	name := ""
	if err := json.Unmarshal(body, &req); err == nil {
		for _, m := range req.Messages {
			if m.Role != "user" {
				continue
			}
			if name = firstTextContent(m.Content); name != "" {
				break
			}
		}
	}
	if name == "" {
		name = sessionRaw
	}
	name = strings.Join(strings.Fields(name), " ")
	runes := []rune(name)
	if len(runes) > sessionNameLimit {
		return string(runes[:sessionNameLimit]) + "…"
	}
	if name == "" {
		return "未命名会话"
	}
	return name
}

// firstTextContent 兼容 string 与 ChatGPT 数组两种 content 形态，提取首段 text。
func firstTextContent(raw json.RawMessage) string {
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &parts); err == nil {
		for _, p := range parts {
			if p.Type == "text" || p.Type == "input_text" {
				return strings.TrimSpace(p.Text)
			}
		}
	}
	return ""
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
