package gateway

import (
	"bytes"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/router"
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

	rec := doRequest(t, h.gw, "/v1/chat/completions", "sk-gw-testtoken",
		`{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"ping"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("候选 2 应成功返回 200, got %d: %s", rec.Code, rec.Body.String())
	}

	out := dump()
	for _, want := range []string{
		"upstream auth rejected", // 候选 1 的失败现场
		"候选失败后请求成功（隐性失败）",        // 请求级结论：失败被掩盖
		"1.上游失败 → 2.success",     // 原因链可读
		"failed_candidates=1",    // 失败候选计数
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
	const cred = "Bearer sk-1234567890"
	got := credFingerprint(cred)
	if !strings.HasPrefix(got, "Bear") || !strings.Contains(got, "len=20") {
		t.Errorf("凭据指纹格式异常: %q", got)
	}
	if strings.Contains(got, "sk-1234567890") {
		t.Errorf("凭据指纹不应含完整值: %q", got)
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
