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
	trail := []map[string]any{{"order": 1, "reason": "流式中断"}}

	logger.SetClientErrorLogEnabled(false)
	t.Cleanup(func() { logger.SetClientErrorLogEnabled(true) })

	// 4xx 客户端类结论：开关关闭时不落盘。
	dump := withLogCapture(h)
	h.gw.logOutcome(ctx, slog.LevelError, "chat/completions", "gpt-4", "候选失败且请求最终失败", 402, false, nil, "sess-1", trail, nil)
	if out := dump(); strings.Contains(out, "gateway request outcome") {
		t.Errorf("开关关闭时 4xx（402）结论不应落盘：\n%s", out)
	}

	// 流式中断形态（status=200、masked=false）：只可能是故障，必须落盘且为 ERROR。
	dump = withLogCapture(h)
	h.gw.logOutcome(ctx, slog.LevelError, "chat/completions", "gpt-4", "候选失败且请求最终失败", 200, false, nil, "sess-1", trail, nil)
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
	h.gw.logOutcome(ctx, slog.LevelError, "chat/completions", "gpt-4", "候选失败且请求最终失败", 402, false, nil, "sess-1", trail, nil)
	if out := dump(); !strings.Contains(out, "gateway request outcome") {
		t.Errorf("开关打开时 4xx（402）结论应落盘：\n%s", out)
	}
}
