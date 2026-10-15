package gateway

import (
	"context"
	"net/http"
	"testing"

	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// buildReq 用给定头策略构建上游请求，便于断言最终上游头集合；protocol 为空时按 openai-chat。
func buildReq(t *testing.T, downstream http.Header, policy channelprotocol.HeaderPolicy, protocol, cred string, vars map[string]string) *http.Request {
	t.Helper()
	if protocol == "" {
		protocol = channelprotocol.ProtocolOpenAIChat
	}
	req, err := buildUpstreamRequest(context.Background(), UpstreamRequest{
		BaseURL:          "https://up.example.com",
		Cred:             cred,
		Body:             []byte(`{}`),
		DownstreamHeader: downstream,
		HeaderPolicy:     policy,
		Protocol:         protocol,
		Vars:             vars,
	}, "/chat/completions", "application/json")
	if err != nil {
		t.Fatalf("build upstream request: %v", err)
	}
	return req
}

// AC-5：passthroughMode=none → 下游自定义头不上游。
func TestHeaderPolicy_None(t *testing.T) {
	down := http.Header{}
	down.Set("x-opencode-session", "sess-123")
	down.Set("X-Custom", "v")

	req := buildReq(t, down, channelprotocol.HeaderPolicy{PassthroughMode: channelprotocol.PassthroughNone},
		"", "sk", nil)
	if got := req.Header.Get("x-opencode-session"); got != "" {
		t.Errorf("none 模式不应透传下游头，got %q", got)
	}
	if got := req.Header.Get("X-Custom"); got != "" {
		t.Errorf("none 模式不应透传下游头，got %q", got)
	}
}

// AC-6：whitelist 仅放行列出的头，其余下游头不上游。
func TestHeaderPolicy_Whitelist(t *testing.T) {
	down := http.Header{}
	down.Set("x-opencode-session", "sess-123")
	down.Set("X-Other", "no")

	req := buildReq(t, down, channelprotocol.HeaderPolicy{
		PassthroughMode: channelprotocol.PassthroughWhitelist,
		Whitelist:       []string{"x-opencode-session"},
	}, "", "sk", nil)
	if got := req.Header.Get("x-opencode-session"); got != "sess-123" {
		t.Errorf("whitelist 应放行 x-opencode-session，got %q", got)
	}
	if got := req.Header.Get("X-Other"); got != "" {
		t.Errorf("whitelist 未列出的头不应透传，got %q", got)
	}
}

// AC-7：blacklist 除列出的头与强制黑名单外全部透传。
func TestHeaderPolicy_Blacklist(t *testing.T) {
	down := http.Header{}
	down.Set("x-foo", "blocked")
	down.Set("x-bar", "allowed")

	req := buildReq(t, down, channelprotocol.HeaderPolicy{
		PassthroughMode: channelprotocol.PassthroughBlacklist,
		Blacklist:       []string{"x-foo"},
	}, "", "sk", nil)
	if got := req.Header.Get("x-foo"); got != "" {
		t.Errorf("blacklist 应排除 x-foo，got %q", got)
	}
	if got := req.Header.Get("x-bar"); got != "allowed" {
		t.Errorf("blacklist 应透传 x-bar，got %q", got)
	}
}

// AC-8：强制黑名单（Cookie/Host/Accept-Encoding/Connection 等）任何模式下都不上游（含 all）。
func TestHeaderPolicy_ForcedBlacklist(t *testing.T) {
	down := http.Header{}
	for _, h := range []string{"Cookie", "Accept-Encoding", "Connection", "Authorization", "Host", "X-Forwarded-For"} {
		down.Set(h, "danger")
	}
	down.Set("X-Keep", "ok")

	req := buildReq(t, down, channelprotocol.HeaderPolicy{PassthroughMode: channelprotocol.PassthroughAll},
		"", "sk", nil)
	for _, h := range []string{"Cookie", "Accept-Encoding", "Connection", "Host", "X-Forwarded-For"} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("强制黑名单头 %s 不应上游，got %q", h, got)
		}
	}
	// 下游 Authorization 属凭据头，透传恒禁止；上游看到的必为协议隐含注入值。
	if got := req.Header.Get("Authorization"); got != "Bearer sk" {
		t.Errorf("下游 Authorization 不应透传，应为协议注入值，got %q", got)
	}
	if got := req.Header.Get("X-Keep"); got != "ok" {
		t.Errorf("all 模式应透传非黑名单头 X-Keep，got %q", got)
	}
}

// set 变量替换（api_key/session_id/model/client_header）与「覆盖(set)」对鉴权头的显式覆盖。
func TestHeaderPolicy_SetVariables(t *testing.T) {
	down := http.Header{}
	down.Set("x-down", "from-client")

	req := buildReq(t, down, channelprotocol.HeaderPolicy{
		PassthroughMode: channelprotocol.PassthroughNone,
		Set: map[string]string{
			"X-Api-Key-Var": "{api_key}",
			"X-Sess":        "{session_id}",
			"X-Model":       "{model}",
			"X-From":        "{client_header:x-down}",
		},
	}, "", "sk-secret", map[string]string{
		"session_id": "sess-1", "request_id": "req-1", "model": "gpt-4",
	})
	if got := req.Header.Get("X-Api-Key-Var"); got != "sk-secret" {
		t.Errorf("{api_key} 应替换为渠道凭据，got %q", got)
	}
	if got := req.Header.Get("X-Sess"); got != "sess-1" {
		t.Errorf("{session_id} 替换错误，got %q", got)
	}
	if got := req.Header.Get("X-Model"); got != "gpt-4" {
		t.Errorf("{model} 替换错误，got %q", got)
	}
	if got := req.Header.Get("X-From"); got != "from-client" {
		t.Errorf("{client_header:x-down} 替换错误，got %q", got)
	}
}

// 「覆盖(set)」在协议隐含鉴权之后应用，可覆盖鉴权头（显式意图优先）。
func TestHeaderPolicy_SetOverridesProtocolAuth(t *testing.T) {
	req := buildReq(t, http.Header{}, channelprotocol.HeaderPolicy{
		PassthroughMode: channelprotocol.PassthroughNone,
		Set:             map[string]string{"Authorization": "Token custom-override"},
	}, channelprotocol.ProtocolOpenAIChat, "sk-secret", nil)
	if got := req.Header.Get("Authorization"); got != "Token custom-override" {
		t.Errorf("set 应覆盖协议隐含鉴权，got %q", got)
	}
}

// AC-8：set 同样受非凭据类强制黑名单约束，任何模式下都不能把 Cookie/Host/Content-Length 等注入上游。
func TestHeaderPolicy_SetCannotInjectForcedBlacklist(t *testing.T) {
	req := buildReq(t, http.Header{}, channelprotocol.HeaderPolicy{
		PassthroughMode: channelprotocol.PassthroughAll,
		Set: map[string]string{
			"Cookie":          "session=evil",
			"Host":            "evil.example.com",
			"Content-Length":  "12345",
			"X-Forwarded-For": "1.2.3.4",
			"X-Ok":            "kept",
		},
	}, "", "sk", nil)
	for _, h := range []string{"Cookie", "Host", "Content-Length", "X-Forwarded-For"} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("set 注入的强制黑名单头 %s 不应上游, got %q", h, got)
		}
	}
	if got := req.Header.Get("X-Ok"); got != "kept" {
		t.Errorf("set 非黑名单头 X-Ok 应生效, got %q", got)
	}
}

// 协议隐含鉴权：openai-chat/openai-responses → Authorization: Bearer；anthropic-messages → x-api-key（原样）。
func TestInjectProtocolAuth(t *testing.T) {
	req := buildReq(t, http.Header{}, channelprotocol.HeaderPolicy{},
		channelprotocol.ProtocolOpenAIChat, "sk-abc", nil)
	if got := req.Header.Get("Authorization"); got != "Bearer sk-abc" {
		t.Errorf("openai-chat 应注入 Authorization Bearer，got %q", got)
	}

	req = buildReq(t, http.Header{}, channelprotocol.HeaderPolicy{},
		channelprotocol.ProtocolOpenAIResponses, "sk-resp", nil)
	if got := req.Header.Get("Authorization"); got != "Bearer sk-resp" {
		t.Errorf("openai-responses 应注入 Authorization Bearer，got %q", got)
	}

	req = buildReq(t, http.Header{}, channelprotocol.HeaderPolicy{},
		channelprotocol.ProtocolAnthropicMessages, "sk-raw", nil)
	if got := req.Header.Get("x-api-key"); got != "sk-raw" {
		t.Errorf("anthropic-messages 应注入 x-api-key 原样凭据，got %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("anthropic-messages 不应注入 Authorization，got %q", got)
	}

	// 无凭据不注入。
	req = buildReq(t, http.Header{}, channelprotocol.HeaderPolicy{},
		channelprotocol.ProtocolOpenAIChat, "", nil)
	if got := req.Header.Get("Authorization"); got != "" {
		t.Errorf("无凭据不应注入鉴权头，got %q", got)
	}
}
