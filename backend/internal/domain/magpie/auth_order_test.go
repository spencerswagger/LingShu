// auth_order_test.go 覆盖两条回归：
//   - P1-1：鉴权必须早于读请求体（未鉴权一律 401，且请求体不被读进内存）；
//   - P1-2：Gemini 原生取凭据方式（x-goog-api-key / ?key=）要支持，
//     且用作鉴权的 ?key= 必须从转发查询串里剔除（否则租户令牌被带到上游）。
package magpie

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/team/llmgateway/internal/domain/model"
)

// countingBody 记录请求体是否被读过（未鉴权时不该读）。
type countingBody struct {
	reads atomic.Int64
	r     io.Reader
}

func (c *countingBody) Read(p []byte) (int, error) {
	c.reads.Add(1)
	return c.r.Read(p)
}

func (c *countingBody) Close() error { return nil }

// 未鉴权 + 坏 JSON → 401（不是 400），且请求体一次都没被读。
func TestHandleProxy_AuthBeforeBody(t *testing.T) {
	h := newHandlerForAuthTest(t)
	cases := map[string]*http.Request{
		"坏 JSON 且无凭据": httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{not json`)),
		"坏 JSON 且令牌无效": func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{not json`))
			r.Header.Set("Authorization", "Bearer bogus")
			return r
		}(),
	}
	for name, r := range cases {
		cb := &countingBody{r: strings.NewReader(`{not json`)}
		r.Body = cb
		w := httptest.NewRecorder()
		h.handleProxy(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: status=%d body=%s（应为 401）", name, w.Code, w.Body.String())
		}
		if n := cb.reads.Load(); n != 0 {
			t.Fatalf("%s: 未鉴权就读了请求体 %d 次", name, n)
		}
	}
}

// newHandlerForAuthTest 装配一个「令牌永远无效」的 handler（鉴权层是唯一变量）。
func newHandlerForAuthTest(t *testing.T) *Handler {
	t.Helper()
	cfg := baseTestConfig()
	cfg.BaseURL = "http://127.0.0.1:1" // 不会真的被访问：请求在鉴权/准入阶段就被拒
	h := NewHandler(Deps{
		Config:        cfg,
		Tokens:        &fakeTokens{err: errors.New("token not found")},
		Models:        &fakeModels{byName: map[string]*model.ExternalModel{"gpt-x": gptModel("gpt-x", "openai/gpt-4o")}},
		Keys:          &fakeKeys{key: nil},
		CredentialKey: testCredKey,
		Prefixes:      NewPrefixProvider(nil),
	})
	if h == nil {
		t.Fatal("handler 未装配")
	}
	return h
}

// Gemini 原生 x-goog-api-key 头可直接鉴权。
func TestExtractRequestToken_HeaderVariants(t *testing.T) {
	mk := func(k, v string) *http.Request {
		r := httptest.NewRequest(http.MethodPost, "/v1beta/models/gpt-x:generateContent", nil)
		if k != "" {
			r.Header.Set(k, v)
		}
		return r
	}
	cases := []struct {
		name      string
		req       *http.Request
		want      string
		fromQuery bool
	}{
		{"bearer", mk("Authorization", "Bearer t1"), "t1", false},
		{"x-api-key", mk("x-api-key", "t2"), "t2", false},
		{"x-goog-api-key", mk("x-goog-api-key", "t3"), "t3", false},
		{"query key", httptest.NewRequest(http.MethodPost, "/v1beta/models/gpt-x:generateContent?key=t4&alt=sse", nil), "t4", true},
		{"header 优先于 query", func() *http.Request {
			r := httptest.NewRequest(http.MethodPost, "/v1beta/models/gpt-x:generateContent?key=t4", nil)
			r.Header.Set("Authorization", "Bearer t1")
			return r
		}(), "t1", false},
		{"都没有", mk("", ""), "", false},
	}
	for _, c := range cases {
		plain, fromQuery := ExtractRequestToken(c.req)
		if plain != c.want || fromQuery != c.fromQuery {
			t.Fatalf("%s: plain=%q fromQuery=%v（want %q/%v）", c.name, plain, fromQuery, c.want, c.fromQuery)
		}
	}
}

// query 里的 key 用于鉴权时必须从转发 query 剔除；其余参数逐字保留（含顺序与原样转义）。
func TestStripQueryParam_KeepsEverythingElse(t *testing.T) {
	cases := []struct{ in, want string }{
		{"key=abc", ""},
		{"key=abc&alt=sse", "alt=sse"},
		{"alt=sse&key=abc", "alt=sse"},
		{"alt=sse&key=abc&x=1%2F2", "alt=sse&x=1%2F2"},
		{"key=", ""},
		{"other=1", "other=1"},
		{"", ""},
	}
	for _, c := range cases {
		if got := stripQueryParam(c.in, "key"); got != c.want {
			t.Fatalf("stripQueryParam(%q)=%q（want %q）", c.in, got, c.want)
		}
	}
}

// 端到端：?key= 能通过鉴权，且上游收到的 query 里没有 key，只有其余参数。
func TestForward_QueryKeyUsedForAuthIsStripped(t *testing.T) {
	var gotQuery string
	env := newAuthEnv(t, map[string]*model.ExternalModel{"gpt-x": gptModel("gpt-x", "openai/gpt-4o")},
		func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			_, _ = w.Write([]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
		})

	// 不带 Authorization，用 Gemini 原生 ?key= 鉴权。
	req, err := http.NewRequest(http.MethodPost,
		env.gateway.URL+"/v1beta/models/gpt-x:generateContent?key=tenant-token&alt=sse", strings.NewReader(`{"contents":[]}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := env.gateway.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s（?key= 应能通过鉴权）", resp.StatusCode, body)
	}
	if strings.Contains(gotQuery, "key=") || strings.Contains(gotQuery, "tenant-token") {
		t.Fatalf("租户令牌被带到上游: query=%q", gotQuery)
	}
	if !strings.Contains(gotQuery, "alt=sse") {
		t.Fatalf("其余 query 应原样保留: query=%q", gotQuery)
	}
}

// x-goog-api-key 头端到端可用（不落到 query，也不需要剔除）。
func TestForward_GoogleAPIKeyHeaderWorks(t *testing.T) {
	var gotAuth, gotQuery string
	env := newAuthEnv(t, map[string]*model.ExternalModel{"gpt-x": gptModel("gpt-x", "openai/gpt-4o")},
		func(w http.ResponseWriter, r *http.Request) {
			gotAuth, gotQuery = r.Header.Get("Authorization"), r.URL.RawQuery
			_, _ = w.Write([]byte(`{}`))
		})

	req, err := http.NewRequest(http.MethodPost,
		env.gateway.URL+"/v1beta/models/gpt-x:generateContent?alt=sse", strings.NewReader(`{"contents":[]}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("x-goog-api-key", "tenant-token")
	resp, err := env.gateway.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if gotQuery != "alt=sse" {
		t.Fatalf("query=%q", gotQuery)
	}
	// 出站凭证头换成 callerKey，且不含租户令牌。
	if strings.Contains(gotAuth, "tenant-token") || gotAuth == "" {
		t.Fatalf("出站凭证头=%q", gotAuth)
	}
}

// authEnv 是「假上游 + facade」最小环境（本文件只用来看鉴权与 query 行为）。
type authEnv struct{ gateway *httptest.Server }

func newAuthEnv(t *testing.T, models map[string]*model.ExternalModel, upstream http.HandlerFunc) *authEnv {
	t.Helper()
	srv := httptest.NewServer(upstream)
	t.Cleanup(srv.Close)

	cfg := baseTestConfig()
	cfg.BaseURL = srv.URL
	cipher, err := encryptForTest("caller-secret")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	h := NewHandler(Deps{
		Config:        cfg,
		Tokens:        &fakeTokens{tok: activeToken()},
		Models:        &fakeModels{byName: models},
		Keys:          &fakeKeys{key: &Key{ID: 1, TokenID: 7, KeyName: "core-k1", KeyCipher: cipher, State: KeyStateActive}},
		CredentialKey: testCredKey,
		Prefixes:      NewPrefixProvider(nil),
	})
	mux := http.NewServeMux()
	h.Mount(mux)
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)
	return &authEnv{gateway: gw}
}

var _ = context.Background
