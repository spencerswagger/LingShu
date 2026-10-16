package gateway

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/team/llmgateway/internal/domain/channelprotocol"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/pkg/logger"
)

// 本文件定义「接口异常日志」的统一契约。目标：任何一次失败——无论最终是否返回给用户——
// 都留下一条**自洽可分析**的结构化日志，运维凭 request_id 单条即可定位，
// 不必依赖访问日志（只见 method/path/status）、也不必依赖落库账单（字段有限、有延迟）。
//
// 为什么必须单列一套：候选失败会被 failover 掩盖（用户看到 200），访问日志只留一条 200，
// 账单里只有渠道/密钥/模型三列，而真正决定成败的维度——入站端点/协议、实际上游地址、
// 实际发出的凭据头、转发出去的头名——在原观测面上全部缺失，
// 导致「同渠道/同密钥/同模型，某人 401」这类问题无法分析。
//
// 字段分组：
//  1. 请求身份：endpoint（入站端点路径，如 /v1/chat/completions，与访问日志 path 同源）
//     / protocol / model / user_id / token_id / session_id / requestId(由 logger 注入)
//  2. 上游落点：upstream(host+path) / channel_id / channel_name / channel_key_id / channel_key_name
//     / internal_model_id / external_model_id
//  3. 凭据载体：auth_header / auth_present / auth_empty / auth_overridden / auth_fp
//  4. 转发头：header_names / cred_headers（**仅头名**）
//  5. 判定线索：upstream_status / err_scope / err_param / err_type / err_code / err_summary
//  6. 尝试轨迹：attempt / attempts_total / retry / reason / duration_ms
//  7. 请求级结论（logOutcome）：status（真实下游状态码）/ attempts / failed_candidates
//     / masked_by_failover（仅"确实成功且中间有候选失败"才为 true）/ attempt_trail / session_id
//
// 脱敏红线（硬约束，任何新增字段都不得突破）：
//   - 绝不落凭据明文：凭据只落指纹（前 4 + 末 4 + 长度；短值整体脱敏），网关令牌同理；
//   - 绝不落完整 URL：只落 host + path，query 一律丢弃（避免 ?api_key=... 泄漏）；
//   - 绝不落请求体/响应体全文：只落错误摘要（按 UTF-8 边界截断）；
//   - 请求头只落头名，绝不落头值。

const (
	// errSummaryMaxBytes 错误摘要落日志的字节上限（防超长上游错误体撑爆日志）。
	errSummaryMaxBytes = 512
	// fpPrefixRunes / fpSuffixRunes 凭据指纹保留的首尾字符数（rune，保证 UTF-8 安全）。
	fpPrefixRunes = 4
	fpSuffixRunes = 4
	// fpMinLen 可安全展示首尾指纹的最小字符数：短于该长度一律整体遮蔽。
	// 取 16 是为了避免"指纹"退化成可暴力补齐的准明文（n=9 时首尾各 4 位只剩 1 位未知）。
	fpMinLen = 16
	// inboundPrefix 入站端点的注册前缀，与网关 Handler 的注册路径一致。
	// 集中为常量，避免前缀字面量在多处重复、改挂载点时日志与访问日志失配。
	inboundPrefix = "/v1/"
)

// 上游错误维度（诊断字段 err_scope，**不参与**状态机判定，仅用于事后分诊）：
//   - model：上游报错指向"模型不存在/无权访问"，属模型维度；
//   - credential：上游报错指向"凭据无效/未认证"，属凭据维度；
//   - unknown：无法判定。
const (
	errScopeModel      = "model"
	errScopeCredential = "credential"
	errScopeUnknown    = "unknown"
)

// credHeaderNames 疑似「凭据类」请求头名（小写）。
// 用途：落日志时突出"被转发/被覆盖的凭据头"，用于发现"下游凭据被带上游"这类泄漏
// （强制黑名单之外的头名同样纳入，因为它们的值可能是调用方的凭据）。
var credHeaderNames = map[string]bool{
	"authorization": true, "proxy-authorization": true,
	"x-api-key": true, "api-key": true, "apikey": true, "x-apikey": true,
	"x-auth-token": true, "x-token": true, "access-token": true, "x-access-token": true,
	"x-goog-api-key": true, "x-hf-token": true, "x-ms-token": true,
	"openai-api-key": true, "x-openai-api-key": true,
	"secret-key": true, "x-secret-key": true, "access_key": true, "x-app-key": true,
}

// credFingerprint 返回凭据值的指纹：前 fpPrefixRunes + 末 fpSuffixRunes + 长度。
// 空值返回 "empty"；**长度不足 fpMinLen 时整体脱敏为 "***(len=N)"**——
// 短凭据若按首尾各取 4 位，几乎等于明文（如 n=9 只剩 1 位未知），故以长度阈值兜住。
func credFingerprint(v string) string {
	if v == "" {
		return "empty"
	}
	r := []rune(v)
	n := len(r)
	if n < fpMinLen {
		return "***(len=" + strconv.Itoa(n) + ")"
	}
	return string(r[:fpPrefixRunes]) + "…" + string(r[n-fpSuffixRunes:]) + "(len=" + strconv.Itoa(n) + ")"
}

// inboundPath 把入站端点常量还原为注册路径（chat/completions → /v1/chat/completions），
// 使异常日志的 endpoint 与 nginx/网关访问日志的 path 直接同源可对照——
// 这正是排查「同一渠道/密钥/模型，不同客户端走不同端点」类问题所需的字段。
func inboundPath(endpoint string) string {
	if endpoint == "" || strings.HasPrefix(endpoint, "/") {
		return endpoint
	}
	return inboundPrefix + endpoint
}

// diag 是一次上游调用的「落点现场」：只记录结构（地址、头名、鉴权头名与值指纹），不记录任何明文。
type diag struct {
	upstream       string   // host + path（不含 query）
	headerNames    []string // 最终发出的请求头名（升序，仅名）
	credHeaders    []string // 最终发出的凭据类头名（仅名，取自 credHeaderNames）
	authHeader     string   // 按入站协议隐含的鉴权头名
	authPresent    bool     // 该头是否存在
	authEmpty      bool     // 存在但值为空（典型：覆盖写入了空值）
	authOverridden bool     // 实际值与"协议隐含注入值"不一致（含被覆盖/置空）
	authFP         string   // 实际发出的凭据值指纹
}

// diagFromRequest 从"已构建好的上游请求"提取落点现场（得到最终生效的头，含 set 覆盖的效果）。
func diagFromRequest(req *http.Request, protocol, cred string) diag {
	d := diag{}
	if req == nil || req.URL == nil {
		return d
	}
	host := req.URL.Host
	if host == "" {
		host = req.Host
	}
	d.upstream = host + req.URL.Path
	d.headerNames, d.credHeaders = headerNameSets(req.Header)
	d.inspectAuth(req.Header, protocol, cred)
	return d
}

// diagFromTarget 在"上游请求尚未构建成功"时给出意图落点（无头信息），保证构建失败也有现场可查。
func diagFromTarget(baseURL, path, protocol, cred string) diag {
	d := diag{}
	if strings.TrimSpace(baseURL) != "" || strings.TrimSpace(path) != "" {
		d.upstream = joinURL(baseURL, path)
	}
	d.inspectAuth(http.Header{}, protocol, cred)
	return d
}

// inspectAuth 比对实际鉴权头与「协议隐含注入」的期望值，得出 present/empty/overridden 与指纹。
func (d *diag) inspectAuth(h http.Header, protocol, cred string) {
	header, scheme := channelprotocol.ProtocolAuth(protocol)
	d.authHeader = header
	expected := cred
	if scheme == "bearer" {
		expected = "Bearer " + cred
	}
	vals := h.Values(header)
	d.authPresent = len(vals) > 0
	actual := strings.Join(vals, ",")
	d.authEmpty = d.authPresent && strings.TrimSpace(actual) == ""
	d.authFP = credFingerprint(actual)
	d.authOverridden = cred != "" && actual != expected
}

// headerNameSets 返回请求头名升序列表，以及其中的凭据类头名（均只含头名，无头值）。
func headerNameSets(h http.Header) ([]string, []string) {
	names := make([]string, 0, len(h))
	creds := make([]string, 0, 4)
	for k := range h {
		names = append(names, k)
		if credHeaderNames[strings.ToLower(k)] {
			creds = append(creds, k)
		}
	}
	sort.Strings(names)
	sort.Strings(creds)
	return names, creds
}

// kv 把落点现场展开为日志键值对。
func (d diag) kv() []any {
	args := make([]any, 0, 12)
	if d.upstream != "" {
		args = append(args, "upstream", d.upstream)
	}
	if d.authHeader != "" {
		args = append(args,
			"auth_header", d.authHeader,
			"auth_present", d.authPresent,
			"auth_empty", d.authEmpty,
			"auth_overridden", d.authOverridden,
			"auth_fp", d.authFP,
		)
	}
	if len(d.headerNames) > 0 {
		args = append(args, "header_names", strings.Join(d.headerNames, ","))
	}
	if len(d.credHeaders) > 0 {
		args = append(args, "cred_headers", strings.Join(d.credHeaders, ","))
	}
	return args
}

// upstreamErrParts 解析上游错误体中的稳定字段（OpenAI 兼容形状 {error:{param,type,code,message}}）。
// 非该形状时退化为整段摘要。返回值均经过**凭据样式清洗**与截断——
// 上游可能在文案里回显它收到的凭据，直接落盘会突破"凭据只落指纹"的红线。
func upstreamErrParts(raw string) (param, errType, code, summary string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", "", ""
	}
	var v struct {
		Error struct {
			Param   string `json:"param"`
			Type    string `json:"type"`
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &v); err == nil && (v.Error.Message != "" || v.Error.Type != "" || v.Error.Code != "") {
		return v.Error.Param, v.Error.Type, v.Error.Code, truncateLog(sanitizeErrSummary(v.Error.Message), errSummaryMaxBytes)
	}
	return "", "", "", truncateLog(sanitizeErrSummary(raw), errSummaryMaxBytes)
}

// 上游可能在错误文案里回显它收到的凭据（如 "Incorrect API key provided: sk-abc…"）。
// 三组样式覆盖常见写法：厂商前缀令牌、Bearer 头、key/token/secret=值。
var (
	credValueRe  = regexp.MustCompile(`(?i)\b(?:sk|rk|pk|ak|gsk|xai|nvapi|hf|glm)[-_][A-Za-z0-9._\-]{6,}`)
	credBearerRe = regexp.MustCompile(`(?i)\b(bearer)\s+[A-Za-z0-9._\-]{6,}`)
	credKVRe     = regexp.MustCompile(`(?i)(api[\s_-]?key|access[\s_-]?token|secret|token)(\s*[:=]\s*)["']?[A-Za-z0-9._\-]{8,}`)
)

// sanitizeErrSummary 对上游错误文案做凭据样式清洗，避免第三方回显把凭据带进日志。
func sanitizeErrSummary(s string) string {
	s = credValueRe.ReplaceAllString(s, "***")
	s = credBearerRe.ReplaceAllString(s, "$1 ***")
	s = credKVRe.ReplaceAllString(s, "$1$2***")
	return s
}

// modelScopeHints / credentialScopeHints 是上游报文的口径线索（小写匹配）。
var (
	modelScopeHints = []string{
		"model does not exist", "does not exist or you do not have access",
		"model_not_found", "model not found", "no access to the model", "invalid model",
		"模型不存在", "模型未开通", "模型不可用", "无权访问该模型", "不支持该模型",
	}
	credentialScopeHints = []string{
		"invalid api key", "incorrect api key", "invalid_api_key", "api key is invalid",
		"authentication", "unauthorized", "missing api key", "invalid token",
		"密钥无效", "密钥不正确", "未授权", "凭据无效", "认证失败",
	}
)

// classifyErrScope 依据上游报文判定错误维度（诊断用）。先判模型维度（更具体），再判凭据维度。
func classifyErrScope(param, errType, code, summary string) string {
	haystack := strings.ToLower(strings.Join([]string{param, errType, code, summary}, " "))
	if strings.TrimSpace(haystack) == "" {
		return errScopeUnknown
	}
	if strings.EqualFold(strings.TrimSpace(param), "model") || containsAny(haystack, modelScopeHints) {
		return errScopeModel
	}
	if containsAny(haystack, credentialScopeHints) {
		return errScopeCredential
	}
	return errScopeUnknown
}

// containsAny 判断 haystack（应已小写）是否含任一 hint（小写）。
func containsAny(haystack string, hints []string) bool {
	for _, h := range hints {
		if h != "" && strings.Contains(haystack, strings.ToLower(h)) {
			return true
		}
	}
	return false
}

// errInfo 描述一次异常的上下文（与 diag 一并落日志）。
type errInfo struct {
	kind          string // 失败类型：rejected / build_failed / connect_failed / upstream_status / usage_missing / billing_failed / stream_interrupted ...
	stage         string // request / route / non_stream / stream
	status        int    // 上游状态码；0 表示未收到响应
	downstream    int    // 返回给下游的状态码（本地拒绝等非上游失败场景）
	attempt       int    // 当前候选序号（1 起）
	attemptsTotal int    // 候选总数
	retry         bool   // 本次失败是否已转入下一候选（true=可能被后续候选掩盖）
	scope         string // err_scope
	param         string
	errType       string
	errCode       string
	summary       string
	reason        string // 人类可读原因（中文）
	sessionID     string
	durationMs    int64
}

// logErr 落一条网关侧异常日志。任何失败分支都应调用它——包括最终返回给用户的失败，
// 以及被 failover 掩盖、用户侧完全不可见的中间失败。
func (g *Gateway) logErr(ctx context.Context, lvl slog.Level, msg, endpoint, protocol, model string,
	token *identity.Token, routeRes *router.RouteResult, d diag, e errInfo) {
	if g.logger == nil {
		return
	}
	args := make([]any, 0, 40)
	args = append(args, "endpoint", inboundPath(endpoint))
	if protocol != "" {
		args = append(args, "protocol", protocol)
	}
	args = append(args, "stage", e.stage, "kind", e.kind, "model", model)
	if token != nil {
		args = append(args, "user_id", token.UserID, "token_id", token.ID)
	}
	if e.sessionID != "" {
		args = append(args, "session_id", e.sessionID)
	}
	if routeRes != nil {
		if routeRes.ChannelID != 0 {
			chName, keyName := g.channelAndKeyNames(routeRes.ChannelID, routeRes.ChannelKeyID)
			args = append(args,
				"channel_id", strconv.FormatInt(routeRes.ChannelID, 10),
				"channel_name", chName,
				"channel_key_id", strconv.FormatInt(routeRes.ChannelKeyID, 10),
				"channel_key_name", keyName,
			)
		}
		if routeRes.InternalModelID != "" {
			args = append(args, "internal_model_id", routeRes.InternalModelID)
		}
		if routeRes.ExternalModelID != 0 {
			args = append(args, "external_model_id", strconv.FormatInt(routeRes.ExternalModelID, 10))
		}
		if routeRes.AutoModelName != "" {
			args = append(args, "auto_model_name", routeRes.AutoModelName)
		}
	}
	if e.status != 0 {
		args = append(args, "upstream_status", e.status)
	}
	if e.downstream != 0 {
		args = append(args, "status", e.downstream)
	}
	if e.attempt > 0 {
		args = append(args, "attempt", e.attempt, "attempts_total", e.attemptsTotal)
	}
	// masked_by_failover 不在此输出：候选级日志无法预知后续候选是否成功，
	// 该结论只由请求级 logOutcome 在"确实成功"时给出，避免把"可能被掩盖"说成"已掩盖"。
	args = append(args, "retry", e.retry)
	if e.scope != "" {
		args = append(args, "err_scope", e.scope)
	}
	if e.param != "" {
		args = append(args, "err_param", e.param)
	}
	if e.errType != "" {
		args = append(args, "err_type", e.errType)
	}
	if e.errCode != "" {
		args = append(args, "err_code", e.errCode)
	}
	if e.summary != "" {
		args = append(args, "err_summary", e.summary)
	}
	args = append(args, "reason", e.reason, "duration_ms", e.durationMs)
	args = append(args, d.kv()...)
	logger.WithRequestID(ctx, g.logger).Log(ctx, lvl, msg, args...)
}

// logReject 记录一次「未进入上游」的网关拒绝（认证/参数/路由/余额/限流/协议不支持等）。
// durationMs 允许为 nil（未计时），与账单侧 *int64 口径保持一致。
// 客户端类拒绝（status < 500）受 logger.ClientErrorLogEnabled 开关控制：
// 高并发下失效令牌/脚本化错误调用可能刷屏，可用 LOG_CLIENT_ERRORS=off 关闭；5xx 始终记录。
func (g *Gateway) logReject(ctx context.Context, endpoint, model, reason string, status int,
	token *identity.Token, durationMs *int64) {
	if status < http.StatusInternalServerError && !logger.ClientErrorLogEnabled() {
		return
	}
	e := errInfo{kind: "rejected", stage: "request", downstream: status, reason: reason, durationMs: deref64(durationMs)}
	g.logErr(ctx, slog.LevelWarn, "gateway request rejected", endpoint, "", model, token, nil, diag{}, e)
}

// logUpstreamErr 记录一次「上游已参与」的失败：带上落点现场、上游状态码与报文口径。
// masked 用于标记该失败已被后续候选掩盖（用户侧不可见）——这正是"用户正常、密钥被禁"的现场。
func (g *Gateway) logUpstreamErr(ctx context.Context, lvl slog.Level, msg, endpoint, protocol, model string,
	token *identity.Token, routeRes *router.RouteResult, d diag, e errInfo) {
	if e.scope == "" && (e.summary != "" || e.param != "" || e.errType != "" || e.errCode != "") {
		e.scope = classifyErrScope(e.param, e.errType, e.errCode, e.summary)
	}
	g.logErr(ctx, lvl, msg, endpoint, protocol, model, token, routeRes, d, e)
}

// logOutcome 在请求终态落一条「请求级结论」日志：
//   - status 为**实际写出的下游状态码**，reason 与之一致（成功/失败不混淆）；
//   - masked_by_failover 只在此处给出：仅当请求确实成功且中间有候选失败时为 true
//     （候选级日志不再输出该字段，见 logErr 注释）；
//   - 成功但中间有候选失败（隐性失败）落 Warn，这是排查"用户正常、密钥却被禁用"的关键线索。
func (g *Gateway) logOutcome(ctx context.Context, lvl slog.Level, endpoint, model, reason string,
	status int, masked bool, token *identity.Token, sessionID string, attempts []map[string]any, durationMs *int64) {
	if g.logger == nil {
		return
	}
	trail, failed := summarizeAttempts(attempts)
	args := []any{
		"endpoint", inboundPath(endpoint),
		"model", model,
		"status", status,
		"reason", reason,
		"attempts", len(attempts),
		"failed_candidates", failed,
		"masked_by_failover", masked,
		"attempt_trail", trail,
		"duration_ms", deref64(durationMs),
	}
	if token != nil {
		args = append(args, "user_id", token.UserID, "token_id", token.ID)
	}
	if sessionID != "" {
		args = append(args, "session_id", sessionID)
	}
	logger.WithRequestID(ctx, g.logger).Log(ctx, lvl, "gateway request outcome", args...)
}

// summarizeAttempts 把决策轨迹压缩为「原因链 + 失败候选数」，便于一条日志看清路由过程。
func summarizeAttempts(attempts []map[string]any) (trail string, failed int) {
	if len(attempts) == 0 {
		return "", 0
	}
	parts := make([]string, 0, len(attempts))
	for _, a := range attempts {
		reason, _ := a["reason"].(string)
		order, _ := a["order"]
		if order != nil {
			parts = append(parts, fmtAny(order)+"."+reason)
		} else {
			parts = append(parts, reason)
		}
		if reason != "" && reason != "success" {
			failed++
		}
	}
	return strings.Join(parts, " → "), failed
}

// fmtAny 把决策轨迹里的序号（int）转为字符串。
func fmtAny(v any) string {
	switch n := v.(type) {
	case int:
		return strconv.Itoa(n)
	case int64:
		return strconv.FormatInt(n, 10)
	case string:
		return n
	}
	return ""
}

// deref64 解引用可空耗时（nil 视为 0），与账单侧 *int64 口径一致。
func deref64(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}

// truncateLog 按 UTF-8 边界截断字符串：先按字节上限粗切，再退回合法 UTF-8 边界，
// 避免产生非法 UTF-8 残字节（日志/采集链路对非法序列容错都不好）。
func truncateLog(s string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(s) <= maxBytes {
		return s
	}
	b := s[:maxBytes]
	for len(b) > 0 && !utf8.ValidString(b) {
		b = b[:len(b)-1]
	}
	return b + "…"
}
