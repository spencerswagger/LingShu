package gateway

import (
	"context"
	"net/http"
	"testing"

	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// buildReq 用给定头策略构建上游请求，便于断言最终上游头集合。
func buildReq(t *testing.T, downstream http.Header, policy channelprotocol.HeaderPolicy, auth channelprotocol.AuthSpec, cred string, vars map[string]string) *http.Request {
	t.Helper()
	req, err := buildUpstreamRequest(context.Background(), UpstreamRequest{
		BaseURL:          "https://up.example.com",
		Cred:             cred,
		Body:             []byte(`{}`),
		DownstreamHeader: downstream,
		HeaderPolicy:     policy,
		Auth:             auth,
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
		channelprotocol.AuthSpec{}, "sk", nil)
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
	}, channelprotocol.AuthSpec{}, "sk", nil)
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
	}, channelprotocol.AuthSpec{}, "sk", nil)
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
		channelprotocol.AuthSpec{}, "sk", nil)
	for _, h := range []string{"Cookie", "Accept-Encoding", "Connection", "Host", "X-Forwarded-For"} {
		if got := req.Header.Get(h); got != "" {
			t.Errorf("强制黑名单头 %s 不应上游，got %q", h, got)
		}
	}
	if got := req.Header.Get("X-Keep"); got != "ok" {
		t.Errorf("all 模式应透传非黑名单头 X-Keep，got %q", got)
	}
}

// AC-9：set 变量替换（api_key/session_id/model/client_header），且鉴权头不出现在用户 set 生效面。
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
			"Authorization": "user-tried-to-override",
		},
	}, channelprotocol.AuthSpec{}, "sk-secret", map[string]string{
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
	// 鉴权头不经由 set 注入（由 auth 注入）：set 中的 Authorization 被跳过，以 auth 注入值为准。
	if got := req.Header.Get("Authorization"); got != "Bearer sk-secret" {
		t.Errorf("鉴权头应以 auth 注入值为准（忽略 set）, got %q", got)
	}
}

// set 无法覆盖 auth 注入的鉴权头（AuthSpec 配置 Authorization 时以 auth 值为准）。
func TestHeaderPolicy_SetCannotOverrideAuth(t *testing.T) {
	req := buildReq(t, http.Header{}, channelprotocol.HeaderPolicy{
		PassthroughMode: channelprotocol.PassthroughNone,
		Set:             map[string]string{"Authorization": "user-tried-to-override"},
	}, channelprotocol.AuthSpec{Header: "Authorization", Scheme: "bearer"}, "sk-secret", nil)
	if got := req.Header.Get("Authorization"); got != "Bearer sk-secret" {
		t.Errorf("鉴权头应以 auth 注入值为准, got %q", got)
	}
}

// 鉴权注入：默认 Authorization: Bearer；x-api-key 走 raw；query 参数注入。
func TestInjectAuth(t *testing.T) {
	req := buildReq(t, http.Header{}, channelprotocol.HeaderPolicy{},
		channelprotocol.AuthSpec{Header: "Authorization", Scheme: "bearer"}, "sk-abc", nil)
	if got := req.Header.Get("Authorization"); got != "Bearer sk-abc" {
		t.Errorf("bearer 注入错误，got %q", got)
	}

	req = buildReq(t, http.Header{}, channelprotocol.HeaderPolicy{},
		channelprotocol.AuthSpec{Header: "x-api-key", Scheme: "raw"}, "sk-raw", nil)
	if got := req.Header.Get("x-api-key"); got != "sk-raw" {
		t.Errorf("raw x-api-key 注入错误，got %q", got)
	}

	req = buildReq(t, http.Header{}, channelprotocol.HeaderPolicy{},
		channelprotocol.AuthSpec{Header: "Authorization", Scheme: "bearer",
			Query: map[string]string{"api-version": "2025-04-01-preview"}}, "sk-q", nil)
	if got := req.URL.Query().Get("api-version"); got != "2025-04-01-preview" {
		t.Errorf("query 鉴权参数注入错误，got %q", got)
	}
}
