package magpie

import (
	"io"
	"net/http"
	"testing"

	"github.com/team/llmgateway/internal/domain/model"
)

// TestCopyResponseHeaders_DropsUpstreamIdentity 是口径②的单元级断言：
// 上游/供应商内部头不得透传给下游；既有透传契约（普通头 / X-Upstream-Trace）不受影响。
func TestCopyResponseHeaders_DropsUpstreamIdentity(t *testing.T) {
	dst, src := http.Header{}, http.Header{}
	src.Set("X-Magpie-Upstream", "magpie-node-7")
	src.Set("X-MAGPIE-FOO", "bar")            // 大小写变体
	src.Set("Server", "magpie-gateway/1.0")   // 实现暴露
	src.Set("X-Powered-By", "PHP/8.1")        // 实现暴露
	src.Set("X-Vendor-Trace", "trace-abc")    // 头名是身份声明
	src.Set("X-OpenAI-Organization", "org-1") // 头名含供应商名
	src.Set("X-Provider", "openai")           // 头名是身份声明
	src.Set("X-Served-By", "pool-a")          // 头名是身份声明
	src.Set("X-Note", "powered by anthropic") // 取值含供应商名
	// 必须透传（既有契约）：
	src.Set("X-Upstream-Trace", "trace-1")
	src.Set("Cache-Control", "no-cache")
	src.Set("Retry-After", "5")
	copyResponseHeaders(dst, src)

	for _, name := range []string{
		"X-Magpie-Upstream", "X-Magpie-Foo", "Server", "X-Powered-By",
		"X-Vendor-Trace", "X-Openai-Organization", "X-Provider", "X-Served-By", "X-Note",
	} {
		if got := dst.Get(name); got != "" {
			t.Fatalf("%s 不应透传到下游（口径②），实际 %q", name, got)
		}
	}
	for name, want := range map[string]string{
		"X-Upstream-Trace": "trace-1",
		"Cache-Control":    "no-cache",
		"Retry-After":      "5",
	} {
		if got := dst.Get(name); got != want {
			t.Fatalf("%s 应透传，实际 %q 期望 %q", name, got, want)
		}
	}
}

// TestCopyResponseHeaders_MultiValueAnyHitDrops 断言多值头任一取值命中即整头丢弃。
func TestCopyResponseHeaders_MultiValueAnyHitDrops(t *testing.T) {
	dst, src := http.Header{}, http.Header{}
	src["X-Note"] = []string{"harmless", "served by openrouter"}
	copyResponseHeaders(dst, src)
	if got := dst.Get("X-Note"); got != "" {
		t.Fatalf("多值头任一取值命中供应商名应整头丢弃，实际 %q", got)
	}
}

// TestCopyResponseHeaders_RequestIDOursWins 断言 x-request-id 保留且只留灵枢自己的：
// 外层中间件已写入链路 ID 时，上游同名头不得并成第二值；中间件未挂载（裸 mux）时
// 上游的原样透传，不算误删。
func TestCopyResponseHeaders_RequestIDOursWins(t *testing.T) {
	dst, src := http.Header{}, http.Header{}
	dst.Set("X-Request-Id", "ls-req-1")
	src.Set("X-Request-Id", "upstream-req-9")
	copyResponseHeaders(dst, src)
	if got := dst.Values("X-Request-Id"); len(got) != 1 || got[0] != "ls-req-1" {
		t.Fatalf("有 ours 时 x-request-id 应只剩灵枢的，实际 %v", got)
	}

	dst2, src2 := http.Header{}, http.Header{}
	src2.Set("X-Request-Id", "upstream-req-9")
	copyResponseHeaders(dst2, src2)
	if got := dst2.Get("X-Request-Id"); got != "upstream-req-9" {
		t.Fatalf("无 ours 时 x-request-id 应原样透传，实际 %q", got)
	}
}

// TestCopyResponseHeaders_HopByHopUnchanged 守住既有 hop-by-hop 丢弃不被本次改动影响。
func TestCopyResponseHeaders_HopByHopUnchanged(t *testing.T) {
	dst, src := http.Header{}, http.Header{}
	src.Set("Connection", "close")
	src.Set("Transfer-Encoding", "chunked")
	src.Set("Content-Type", "application/json")
	copyResponseHeaders(dst, src)
	if dst.Get("Connection") != "" || dst.Get("Transfer-Encoding") != "" {
		t.Fatal("hop-by-hop 头仍应丢弃")
	}
	if dst.Get("Content-Type") != "application/json" {
		t.Fatal("普通头应透传")
	}
}

// TestForward_ResponseHeaders_StripsUpstreamIdentity 是 HTTP 级复测（Owl II 实测口径）：
// 经完整转发链，上游回的 X-Magpie-Upstream / Server / X-Vendor-Trace 不得出现在下游；
// x-request-id 保留（裸 mux 无中间件 → 上游的原样透传）。
func TestForward_ResponseHeaders_StripsUpstreamIdentity(t *testing.T) {
	env := newTestEnv(t, baseTestConfig(),
		map[string]*model.ExternalModel{"tenant-gpt": gptModel("tenant-gpt", "upstream/gpt-4o")},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Magpie-Upstream", "magpie-node-7")
			w.Header().Set("Server", "magpie-gateway/1.0")
			w.Header().Set("X-Vendor-Trace", "trace-abc")
			w.Header().Set("X-Request-Id", "upstream-req-9")
			_, _ = io.WriteString(w, `{"choices":[],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
		})
	resp, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","messages":[]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, 期望 200；body=%s", resp.StatusCode, body)
	}
	for _, name := range []string{"X-Magpie-Upstream", "Server", "X-Vendor-Trace"} {
		if got := resp.Header.Get(name); got != "" {
			t.Fatalf("%s 不应透传到下游（口径②），实际 %q", name, got)
		}
	}
	if got := resp.Header.Get("X-Request-Id"); got != "upstream-req-9" {
		t.Fatalf("x-request-id 应保留不误删，实际 %q", got)
	}
}
