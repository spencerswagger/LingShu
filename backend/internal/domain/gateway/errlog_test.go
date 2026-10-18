package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/pkg/logger"
)

// withLogCapture 把网关日志改写到内存缓冲，返回读取函数（断言日志内容用）。
func withLogCapture(h *harness) func() string {
	var buf bytes.Buffer
	h.gw.logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	return buf.String
}

// 上游 401（报文指向模型维度）必须落一条完整现场日志：
// 请求身份 + 落点 + 凭据载体 + 上游状态 + 报文口径（err_scope），且不含凭据明文。
func TestUpstreamAuthRejectedLogsFullScene(t *testing.T) {
	const upstreamBody = `{"error":{"code":"","message":"The model does not exist or you do not have access to it.","param":"model","type":"invalid_request_error"},"id":"as-cgjj5bitru"}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	dump := withLogCapture(h)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-testtoken",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("want 503（单候选鉴权失败后无可用渠道）, got %d: %s", rec.Code, rec.Body.String())
	}

	out := dump()
	for _, want := range []string{
		"upstream auth rejected",
		"endpoint=/v1/chat/completions",
		"model=gpt-4",
		"internal_model_id=gpt-4-internal",
		"upstream_status=401",
		"err_scope=model",           // 报文指向模型维度（诊断字段，不改变状态机判定）
		"err_param=model",           // 报文 param 原样落库
		"retry=true",                // 本次失败已转入下一候选（可能被 failover 掩盖）
		"auth_header=Authorization", // 凭据载体：头名
		"auth_fp=",                  // 凭据值只落指纹
		"gateway request outcome",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("异常日志缺少字段 %q\n实际日志：\n%s", want, out)
		}
	}
	// 脱敏红线：渠道凭据明文绝不出现在日志里。
	if strings.Contains(out, "test-key") {
		t.Errorf("日志泄漏凭据明文：\n%s", out)
	}
	// Fix 1：候选级日志不再声称"已被掩盖"（写出时无法预知后续候选是否成功）。
	if strings.Contains(out, "masked_by_failover=true") {
		t.Errorf("候选级日志不应声称已被掩盖：\n%s", out)
	}
	// 请求级结论：真实下游状态码 + 未被掩盖 + 会话维度可串联（Fix 2 / Fix 1 / Fix 7）。
	for _, want := range []string{
		"gateway request outcome",
		"status=503",
		"masked_by_failover=false",
		"session_id=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("请求级结论缺少 %q\n实际日志：\n%s", want, out)
		}
	}
}

// 被 failover 掩盖的中间失败（用户侧看到 200）同样必须留痕：
// 这是「用户一切正常、密钥却被禁用」类问题的唯一现场。
func TestMaskedUpstreamFailureIsLogged(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer bad-key" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":{"message":"The model does not exist or you do not have access to it.","param":"model","type":"invalid_request_error"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`))
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	// 两个候选：同渠道两条内部模型行 + 两把密钥；第一把凭据在上游被拒。
	h.rrouter.routes = []*router.ChannelRoute{
		{ChannelID: 1, ChannelKeyID: 1, ModelRowID: 7, InternalModelID: "gpt-4-internal", ExternalModelID: 1},
		{ChannelID: 1, ChannelKeyID: 2, ModelRowID: 8, InternalModelID: "gpt-4-internal", ExternalModelID: 1},
	}
	h.chans.keys[1].CredentialPlain = "bad-key"
	h.chans.keys[2] = &channel.KeyRuntime{
		Key:             channel.ChannelKey{ID: 2, ChannelID: 1, Name: "k2", State: channel.StateNormal},
		Channel:         &channel.Channel{ID: 1, Protocol: channel.ProtocolOpenAICompat, BaseURL: upstream.URL},
		CredentialPlain: "good-key",
		Machine:         channel.NewMachine(channel.DefaultMachineConfig()),
		Limiter:         channel.NewLimiter(1000, 1000000, 1.2),
	}
	dump := withLogCapture(h)
	// Fix 4 不变量：关闭客户端错误日志开关后，「成功请求的隐性失败」结论仍必须落盘
	// （它是排查"用户正常、密钥却被禁用"的唯一现场，属服务端结论而非客户端错误）。
	logger.SetClientErrorLogEnabled(false)
	t.Cleanup(func() { logger.SetClientErrorLogEnabled(true) })

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-testtoken",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("候选 2 应成功返回 200, got %d: %s", rec.Code, rec.Body.String())
	}

	out := dump()
	for _, want := range []string{
		"upstream auth rejected",  // 候选 1 的失败现场
		"候选失败后请求成功（隐性失败）",         // 请求级结论：失败被掩盖
		"1.上游失败 → 2.success",      // 原因链可读
		"failed_candidates=1",     // 失败候选计数
		"status=200",              // Fix 2：真实下游状态码
		"masked_by_failover=true", // Fix 1：仅请求级结论给出"确实被掩盖"
		"session_id=",             // Fix 7：会话维度可串联
	} {
		if !strings.Contains(out, want) {
			t.Errorf("日志缺少 %q\n实际日志：\n%s", want, out)
		}
	}
	if strings.Contains(out, "bad-key") || strings.Contains(out, "good-key") {
		t.Errorf("日志泄漏凭据明文：\n%s", out)
	}
}

// 上游报文口径分类与凭据指纹（纯函数，覆盖脱敏红线与 err_scope 判定顺序）。
func TestErrScopeAndCredentialFingerprint(t *testing.T) {
	cases := []struct {
		name                   string
		param, typ, code, summ string
		want                   string
	}{
		{"模型维度（param=model）", "model", "invalid_request_error", "", "The model does not exist or you do not have access to it.", errScopeModel},
		{"模型维度（文案命中）", "", "invalid_request_error", "model_not_found", "invalid model: x", errScopeModel},
		{"凭据维度", "", "invalid_request_error", "invalid_api_key", "Incorrect API key provided", errScopeCredential},
		{"无法判定", "temperature", "invalid_request_error", "", "out of range", errScopeUnknown},
		{"空报文", "", "", "", "", errScopeUnknown},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := classifyErrScope(c.param, c.typ, c.code, c.summ); got != c.want {
				t.Errorf("classifyErrScope = %q, want %q", got, c.want)
			}
		})
	}

	if got := credFingerprint(""); got != "empty" {
		t.Errorf("空凭据指纹 = %q, want empty", got)
	}
	if got := credFingerprint("short"); got != "***(len=5)" {
		t.Errorf("短凭据应整体脱敏, got %q", got)
	}
	// 阈值边界：长度 9（< fpMinLen=16）必须整体脱敏——按首尾各 4 位会只剩 1 位未知，等于准明文。
	if got := credFingerprint("abcdefghi"); got != "***(len=9)" {
		t.Errorf("9 字符凭据必须整体脱敏, got %q", got)
	}
	if got := credFingerprint("abcdefghijklmno"); got != "***(len=15)" {
		t.Errorf("15 字符凭据必须整体脱敏, got %q", got)
	}
	const cred = "Bearer sk-1234567890"
	got := credFingerprint(cred)
	if !strings.HasPrefix(got, "Bear") || !strings.Contains(got, "len=20") {
		t.Errorf("凭据指纹格式异常: %q", got)
	}
	if strings.Contains(got, "sk-1234567890") {
		t.Errorf("凭据指纹不应含完整值: %q", got)
	}
}

// 上游回显凭据时必须清洗，避免绕过"凭据只落指纹"红线（Fix 4）。
func TestSanitizeErrSummaryStripsCredentials(t *testing.T) {
	cases := []struct {
		in      string
		mustNot string
	}{
		{"Incorrect API key provided: sk-abc123456789", "sk-abc123456789"},
		{"Unauthorized: Bearer test-key-abcdef", "test-key-abcdef"},
		{"invalid credential api_key=AKIA1234567890abcd", "AKIA1234567890abcd"},
		{"xai key xai-abc123456789", "xai-abc123456789"},
		{"gsk token gsk_abc123456789", "gsk_abc123456789"},
		{"nvapi key nvapi-abc123456789", "nvapi-abc123456789"},
		{"hf token hf_abc123456789", "hf_abc123456789"},
	}
	for _, c := range cases {
		got := sanitizeErrSummary(c.in)
		if strings.Contains(got, c.mustNot) {
			t.Errorf("凭据片段未被清洗: in=%q out=%q", c.in, got)
		}
	}
	// URL query 必须被抹除（保留 host+path），避免底层错误串里的 ?key=... 泄漏。
	const urlMsg = `Post "https://api.vendor.com/v1/chat/completions?key=sk-secret123456": dial tcp: timeout`
	gotURL := sanitizeErrSummary(urlMsg)
	if strings.Contains(gotURL, "key=sk-secret123456") {
		t.Errorf("URL query 未抹除: %q", gotURL)
	}
	if !strings.Contains(gotURL, "https://api.vendor.com/v1/chat/completions") {
		t.Errorf("URL host+path 应保留: %q", gotURL)
	}
	// Fix 7：URL authority 中的 userinfo（user:pass@）属凭据，必须连同 query 一并抹除。
	const userinfoMsg = `Post "https://user:pass@api.vendor.com/v1/chat/completions?key=sk-secret123456": dial tcp: timeout`
	gotUI := sanitizeErrSummary(userinfoMsg)
	if strings.Contains(gotUI, "user:pass") || strings.Contains(gotUI, "key=sk-secret123456") {
		t.Errorf("URL userinfo/query 未抹除: %q", gotUI)
	}
	if !strings.Contains(gotUI, "api.vendor.com/v1/chat/completions") {
		t.Errorf("URL host+path 应保留: %q", gotUI)
	}
	// 正常文案不应被改动（避免过度清洗影响排查）。
	const keep = "The model does not exist or you do not have access to it."
	if got := sanitizeErrSummary(keep); got != keep {
		t.Errorf("正常文案被误改: %q", got)
	}
	// Fix 3：模型名不得被当作凭据清洗（glm- 是智谱模型名前缀，不是凭据前缀），
	// 否则恰好抹掉"模型不存在"这类报错里最需要的模型标识。
	for _, modelText := range []string{
		"The model glm-4-plus does not exist or you do not have access to it.",
		"The model glm-4-plus-0111 is not available",
	} {
		if got := sanitizeErrSummary(modelText); got != modelText {
			t.Errorf("模型名被误清洗: in=%q out=%q", modelText, got)
		}
	}
	// 摘要路径也必须清洗。
	_, _, _, summary := upstreamErrParts(`{"error":{"message":"Incorrect API key provided: sk-abc123456789","type":"invalid_request_error"}}`)
	if strings.Contains(summary, "sk-abc123456789") {
		t.Errorf("err_summary 泄漏凭据: %q", summary)
	}
}

// 错误摘要截断必须落在合法 UTF-8 边界（多字节字符不得被切半）。
func TestTruncateLogKeepsValidUTF8(t *testing.T) {
	got := truncateLog("中文中文中文", 7)
	if !utf8.ValidString(got) {
		t.Errorf("截断结果不是合法 UTF-8: %q", got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Errorf("超长摘要应带截断标记: %q", got)
	}
	if got := truncateLog("短", 512); got != "短" {
		t.Errorf("未超限不应改动: %q", got)
	}
}

// Fix 1：日志与落库共用的错误体读取必须清洗凭据——
// 账单 error_message 与调用日志 resp_body 都取自它，漏清洗等于两条链路同时落明文。
func TestReadUpstreamErrBodySanitizesCredentials(t *testing.T) {
	resp := &http.Response{Body: io.NopCloser(strings.NewReader(
		`{"error":{"message":"Incorrect API key provided: sk-abc123456789"}}`))}
	body := readUpstreamErrBody(resp)
	if strings.Contains(body, "sk-abc123456789") {
		t.Errorf("上游错误体未清洗凭据: %q", body)
	}
	if !strings.Contains(body, "***") {
		t.Errorf("清洗标记缺失: %q", body)
	}
	// 同源链路：upstreamErrDetail 拼接的 errMsg 也不得含明文。
	if detail := upstreamErrDetail(http.StatusUnauthorized, body); strings.Contains(detail, "sk-abc123456789") {
		t.Errorf("errMsg 含凭据明文: %q", detail)
	}
}

// Fix 2：语义成败判定不得只看状态码——流式中断时下游状态码仍为 200，但请求是失败的。
func TestOutcomeIsFailure(t *testing.T) {
	cases := []struct {
		name string
		out  *fwdOutcome
		want bool
	}{
		{"成功(200)", &fwdOutcome{responded: true, downstreamStatus: http.StatusOK}, false},
		{"计费失败(402)", &fwdOutcome{responded: true, billFailed: true, downstreamStatus: http.StatusPaymentRequired}, true},
		{"上游失败(502)", &fwdOutcome{responded: true, refundPre: true, downstreamStatus: http.StatusBadGateway}, true},
		{"流式中断(状态码仍 200)", &fwdOutcome{responded: true, interrupted: true, downstreamStatus: http.StatusOK}, true},
		{"nil", nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := outcomeIsFailure(c.out); got != c.want {
				t.Errorf("outcomeIsFailure = %v, want %v", got, c.want)
			}
		})
	}
}

// Fix 5 / Fix 9：网关本地拒绝的分级与开关——
// 4xx 受客户端错误开关控制；5xx 恒以 Error 级落盘（否则按 Error 的告警会漏掉真实故障）。
func TestLogRejectLevelAndSwitch(t *testing.T) {
	h := build(t, "http://127.0.0.1:1")
	body := `{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"ping"}]}`
	logger.SetClientErrorLogEnabled(false)
	t.Cleanup(func() { logger.SetClientErrorLogEnabled(true) })

	// 4xx：模型未配置 → 404；开关关闭时应静默
	h.rrouter.err = &router.RouteError{Kind: router.KindModelNotFound, HTTPStatus: http.StatusNotFound, Message: "模型未配置"}
	dump := withLogCapture(h)
	if rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-testtoken", body); rec.Code != http.StatusNotFound {
		t.Fatalf("want 404, got %d: %s", rec.Code, rec.Body.String())
	}
	if out := dump(); strings.Contains(out, "gateway request rejected") {
		t.Errorf("开关关闭时 4xx 本地拒绝不应落日志：\n%s", out)
	}

	// 5xx：路由内部错误 → 500；必须落 Error
	h.rrouter.err = errors.New("route boom")
	dump = withLogCapture(h)
	if rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-testtoken", body); rec.Code != http.StatusInternalServerError {
		t.Fatalf("want 500, got %d: %s", rec.Code, rec.Body.String())
	}
	out := dump()
	if !strings.Contains(out, "gateway request rejected") {
		t.Errorf("5xx 本地拒绝必须落日志：\n%s", out)
	}
	if !strings.Contains(out, "level=ERROR") || !strings.Contains(out, "status=500") {
		t.Errorf("5xx 本地拒绝应以 Error 级落盘：\n%s", out)
	}
}

// Fix 4：4xx 请求级结论受开关控制；开关打开时照常落盘。
func TestOutcomeLogSwitch4xx(t *testing.T) {
	h := build(t, "http://127.0.0.1:1")
	// anthropic-messages 不在该渠道模型可达协议内 → 未出网即被拒（400）
	body := `{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"ping"}]}`

	logger.SetClientErrorLogEnabled(false)
	t.Cleanup(func() { logger.SetClientErrorLogEnabled(true) })
	dump := withLogCapture(h)
	if rec := doRequest(t, h.gw, "/v1/messages", "sk-gw-testtoken", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400（协议不支持）, got %d: %s", rec.Code, rec.Body.String())
	}
	if out := dump(); strings.Contains(out, "gateway request outcome") {
		t.Errorf("开关关闭时 4xx 请求级结论不应落日志：\n%s", out)
	}

	logger.SetClientErrorLogEnabled(true)
	dump = withLogCapture(h)
	if rec := doRequest(t, h.gw, "/v1/messages", "sk-gw-testtoken", body); rec.Code != http.StatusBadRequest {
		t.Fatalf("want 400, got %d: %s", rec.Code, rec.Body.String())
	}
	if out := dump(); !strings.Contains(out, "gateway request outcome") {
		t.Errorf("开关打开时应落 4xx 请求级结论：\n%s", out)
	}
}

// 请求级结论的开关门控必须只覆盖 4xx 客户端类结论：
// 流式中断（status=200、masked=false）虽非 5xx，却只可能是故障，开关关闭时也必须落 ERROR。
func TestOutcomeLogGate(t *testing.T) {
	h := build(t, "http://127.0.0.1:1")
	ctx := context.Background()
	attempts := []map[string]any{{"order": 1, "reason": "流式中断"}}
	trail, failed := summarizeAttempts(attempts)

	logger.SetClientErrorLogEnabled(false)
	t.Cleanup(func() { logger.SetClientErrorLogEnabled(true) })

	// 4xx 客户端类结论：开关关闭时不落盘。
	dump := withLogCapture(h)
	h.gw.logOutcome(ctx, slog.LevelError, "chat/completions", "gpt-4", "候选失败且请求最终失败", 402, false, nil, "sess-1", attempts, trail, failed, nil)
	if out := dump(); strings.Contains(out, "gateway request outcome") {
		t.Errorf("开关关闭时 4xx（402）结论不应落盘：\n%s", out)
	}

	// 上游 429：与候选级日志同口径——不属客户端错误，开关关闭时也必须落盘。
	dump = withLogCapture(h)
	h.gw.logOutcome(ctx, slog.LevelWarn, "chat/completions", "gpt-4", "全部候选限流（本地限流/上游 429）", http.StatusTooManyRequests, false, nil, "sess-1", attempts, trail, failed, nil)
	if out := dump(); !strings.Contains(out, "gateway request outcome") || !strings.Contains(out, "status=429") {
		t.Errorf("开关关闭时上游 429 请求级结论仍应落盘（保持候选级/请求级同口径）：\n%s", out)
	}

	// 流式中断形态（status=200、masked=false）：只可能是故障，必须落盘且为 ERROR。
	dump = withLogCapture(h)
	h.gw.logOutcome(ctx, slog.LevelError, "chat/completions", "gpt-4", "候选失败且请求最终失败", 200, false, nil, "sess-1", attempts, trail, failed, nil)
	out := dump()
	if !strings.Contains(out, "gateway request outcome") {
		t.Errorf("流式中断（status=200）结论必须落盘：\n%s", out)
	}
	if !strings.Contains(out, "level=ERROR") {
		t.Errorf("流式中断结论应为 Error 级：\n%s", out)
	}

	// 开关打开时 4xx 结论落盘。
	logger.SetClientErrorLogEnabled(true)
	dump = withLogCapture(h)
	h.gw.logOutcome(ctx, slog.LevelError, "chat/completions", "gpt-4", "候选失败且请求最终失败", 402, false, nil, "sess-1", attempts, trail, failed, nil)
	if out := dump(); !strings.Contains(out, "gateway request outcome") {
		t.Errorf("开关打开时 4xx（402）结论应落盘：\n%s", out)
	}
}

// 回归：跳过（未进入上游）候选不计入候选失败，原因链标注 (跳过)。
func TestSummarizeAttemptsSkips(t *testing.T) {
	attempts := []map[string]any{
		{"order": 1, "reason": "限流", "skipped": true},
		{"order": 2, "reason": "上游失败"},
		{"order": 3, "reason": "success"},
	}
	trail, failed := summarizeAttempts(attempts)
	if failed != 1 {
		t.Errorf("failed = %d, want 1（仅真实失败计入）", failed)
	}
	if !strings.Contains(trail, "1.限流(跳过)") || !strings.Contains(trail, "2.上游失败") {
		t.Errorf("原因链标注异常: %q", trail)
	}
}

// 开关关闭时，masked=true（隐性失败，status=200）必须落盘且含 masked_by_failover=true。
func TestOutcomeMaskedPathNotGated(t *testing.T) {
	h := build(t, "http://127.0.0.1:1")
	logger.SetClientErrorLogEnabled(false)
	t.Cleanup(func() { logger.SetClientErrorLogEnabled(true) })
	dump := withLogCapture(h)
	attempts := []map[string]any{{"order": 1, "reason": "上游失败"}, {"order": 2, "reason": "success"}}
	trail, failed := summarizeAttempts(attempts)
	h.gw.logOutcome(context.Background(), slog.LevelWarn, "chat/completions", "gpt-4",
		"候选失败后请求成功（隐性失败）", http.StatusOK, true, nil, "sess-1", attempts, trail, failed, nil)
	out := dump()
	if !strings.Contains(out, "gateway request outcome") || !strings.Contains(out, "masked_by_failover=true") {
		t.Errorf("隐性失败结论必须落盘且带 masked_by_failover=true：\n%s", out)
	}
}

// streamClientGone：上游返回 context.Canceled 或请求 ctx 已取消（含 herr 为空）判定为客户端已离开；
// 正常读流错误（ctx 存活）不算客户端离开。
func TestStreamClientGone(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	alive := context.Background()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"上游返回 context.Canceled", alive, context.Canceled, true},
		{"下游写失败但请求 ctx 已取消", canceled, errors.New("write tcp: broken pipe"), true},
		{"herr 为空但请求 ctx 已取消", canceled, nil, true},
		{"正常读流错误（ctx 存活）", alive, errors.New("unexpected EOF"), false},
		{"无错误", alive, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := streamClientGone(c.ctx, c.err); got != c.want {
				t.Errorf("streamClientGone = %v, want %v", got, c.want)
			}
		})
	}
}

// diagFromTarget（上游请求构建失败、无实际鉴权头）路径下三态必须自洽：
// present=false / empty=false / overridden=false，指纹为 empty——
// 不得把"请求未生成"误报成"凭据被覆盖成空"。
func TestDiagFromTargetAuthSemantics(t *testing.T) {
	d := diagFromTarget("https://api.example.com", "/v1/chat/completions", "openai-chat", "test-key")
	if d.authHeader == "" {
		t.Fatalf("应推断出鉴权头名（Authorization），got %q", d.authHeader)
	}
	if d.authPresent || d.authEmpty || d.authOverridden {
		t.Errorf("请求未生成时应 present/empty/overridden 均为 false，got present=%v empty=%v overridden=%v",
			d.authPresent, d.authEmpty, d.authOverridden)
	}
	if d.authFP != "empty" {
		t.Errorf("无实际凭据时指纹应为 empty，got %q", d.authFP)
	}
	// 对照：确有实际鉴权头且值与协议隐含注入值不一致时，overridden 才为 true。
	req := httptest.NewRequest(http.MethodPost, "https://api.example.com/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer other-key")
	d2 := diagFromRequest(req, "openai-chat", "test-key")
	if !d2.authPresent || !d2.authOverridden {
		t.Errorf("显式覆盖鉴权头时应 present=true overridden=true，got present=%v overridden=%v",
			d2.authPresent, d2.authOverridden)
	}
}

// 上游 429 在 ClientErrorLogEnabled=off 时：候选级（logUpstreamErr）与请求级（logOutcome）
// 都必须落盘——429 不属客户端错误，不得因开关关闭而出现"候选有、请求级无"。
func TestUpstream429OutcomeNotGatedByClientSwitch(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()

	h := build(t, upstream.URL)
	logger.SetClientErrorLogEnabled(false)
	t.Cleanup(func() { logger.SetClientErrorLogEnabled(true) })
	dump := withLogCapture(h)

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-testtoken",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d: %s", rec.Code, rec.Body.String())
	}
	out := dump()
	if !strings.Contains(out, "upstream rate limited") {
		t.Errorf("候选级 429 日志缺失：\n%s", out)
	}
	if !strings.Contains(out, "gateway request outcome") || !strings.Contains(out, "status=429") {
		t.Errorf("开关关闭时请求级 429 结论必须落盘（与候选级同口径）：\n%s", out)
	}
}

// 前缀样式（sk-/rk-/...）之外的"高熵长串"（无前缀明文令牌）也必须被兜底脱敏；
// 同时不得误伤纯小写的模型名/错误标识（既有样式规则保持不变）。
func TestSanitizeErrSummaryMasksHighEntropyToken(t *testing.T) {
	const tok = "A1b2C3d4E5f6G7h8I9j0K1l2M3n4P5"
	got := sanitizeErrSummary("upstream said token " + tok + " is invalid")
	if strings.Contains(got, tok) {
		t.Errorf("高熵长串未被脱敏: %q", got)
	}
	if !strings.Contains(got, "***") {
		t.Errorf("缺少脱敏标记: %q", got)
	}
	// 纯小写（无大写）的模型名 / 错误标识必须原样保留。
	for _, keep := range []string{
		"claude-3-5-sonnet-20241022",
		"gpt-4-turbo-2024-04-09",
		"invalid_request_error",
	} {
		if out := sanitizeErrSummary(keep); out != keep {
			t.Errorf("纯小写标识被误清洗: in=%q out=%q", keep, out)
		}
	}
}

// Step 3：稳定排除原因 → 运维动作提示映射（纯函数）；未知 reason 返回空串。
func TestExclusionHint_MapsReasons(t *testing.T) {
	cases := map[string]string{
		"drained":                "查渠道/密钥状态或手动探测",
		"session_full":           "查会话并发/存活",
		"rate_limited":           "查模型/渠道/密钥 RPM·TPM",
		"unsupported_protocol":   "查协议可达性",
		"credential_unavailable": "查密钥凭据",
		"disabled":               "查禁用状态",
	}
	for reason, want := range cases {
		if got := exclusionHint(reason); got != want {
			t.Errorf("exclusionHint(%q) = %q, want %q", reason, got, want)
		}
	}
	if got := exclusionHint("unknown_reason"); got != "" {
		t.Errorf("未知 reason 应返回空串, got %q", got)
	}
}

// Step 6：errInfo 流式诊断字段（delivered / first_byte_received / elapsed_ms）随日志输出；
// 未填充时不输出，保证非流式失败日志字段集不变。
func TestErrLog_StreamDiagnostics(t *testing.T) {
	h := build(t, "http://unused")
	dump := withLogCapture(h)

	delivered := false
	firstByte := false
	elapsed := int64(123)
	h.gw.logUpstreamErr(context.Background(), slog.LevelWarn, "upstream call failed", "chat/completions", "openai-chat", "gpt-4",
		nil, &router.RouteResult{ChannelID: 1, ChannelKeyID: 1, InternalModelID: "gpt-4-internal"}, diag{},
		errInfo{kind: "connect_failed", stage: "stream", attempt: 1, retry: true,
			reason: "上游流式调用失败", delivered: &delivered, firstByteReceived: &firstByte, elapsedMS: &elapsed})
	out := dump()
	for _, want := range []string{"kind=connect_failed", "stage=stream",
		"delivered=false", "first_byte_received=false", "elapsed_ms=123"} {
		if !strings.Contains(out, want) {
			t.Errorf("errInfo 流式诊断字段缺失 %q\n%s", want, out)
		}
	}

	// 未填充诊断字段时不应输出（非流式失败保持既有字段集）。
	dump2 := withLogCapture(h)
	h.gw.logUpstreamErr(context.Background(), slog.LevelWarn, "upstream error response", "chat/completions", "openai-chat", "gpt-4",
		nil, nil, diag{}, errInfo{kind: "upstream_status", stage: "non_stream", status: http.StatusInternalServerError})
	out2 := dump2()
	if strings.Contains(out2, "delivered=") || strings.Contains(out2, "first_byte_received=") || strings.Contains(out2, "elapsed_ms=") {
		t.Errorf("未填充诊断时不应输出相关字段：\n%s", out2)
	}
}
