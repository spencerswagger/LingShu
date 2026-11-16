package magpie

import (
	"bytes"
	"compress/gzip"
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

// TestCopyResponseHeaders_ConnectionNamedFields 断言 `Connection` 点名的字段被剥。
//
// 这一条是「固定黑名单挡不住」的补丁：`Connection` 是逐跳声明，点名的可以是任意扩展头
// （`Connection: x-internal-trace`），不解析它就会顺着透传链路出境。
func TestCopyResponseHeaders_ConnectionNamedFields(t *testing.T) {
	dst, src := http.Header{}, http.Header{}
	// 多值 + 逗号分隔 + 大小写混用：三种写法都要认。
	src.Add("Connection", "x-internal-trace, x-inner-span")
	src.Add("Connection", "keep-alive")
	src.Set("X-Internal-Trace", "t-1")
	src.Set("X-INNER-SPAN", "s-1")
	// 没被点名的普通头照旧透传（不许因为解析 Connection 而扩大剥离面）。
	src.Set("X-Neutral", "ok")
	src.Set("Cache-Control", "no-store")

	copyResponseHeaders(dst, src)

	for _, name := range []string{"X-Internal-Trace", "X-Inner-Span", "Connection"} {
		if got := dst.Get(name); got != "" {
			t.Fatalf("%s 被 Connection 点名（或本身是逐跳头），不应透传，实际 %q", name, got)
		}
	}
	if got := dst.Get("X-Neutral"); got != "ok" {
		t.Fatalf("未被点名的普通头应透传，实际 %q", got)
	}
	if got := dst.Get("Cache-Control"); got != "no-store" {
		t.Fatalf("Cache-Control 应透传，实际 %q", got)
	}
}

// TestCopyResponseHeaders_ContentLengthKept 断言 Content-Length **不被剥**（含被 Connection 点名的情况）。
//
// 依据：响应体未被改写时长度仍然成立；剥掉只会让 Go 退化成 chunked，
// 下游流式库要自己重算、HTTP/1.0 客户端拿不到长度。
// 真正的长度处置在转发层：遮蔽后长度变 → 显式 Del；解压后 → 显式 Del（见 proxy.go）。
func TestCopyResponseHeaders_ContentLengthKept(t *testing.T) {
	dst, src := http.Header{}, http.Header{}
	src.Set("Connection", "content-length") // 恶意/异常点名：仍不剥
	src.Set("Content-Length", "42")
	copyResponseHeaders(dst, src)

	if got := dst.Get("Content-Length"); got != "42" {
		t.Fatalf("Content-Length 必须保留（被 Connection 点名也不剥），实际 %q", got)
	}
}

// TestCopyRequestHeaders_ConnectionNamedFields 断言入站请求上的点名同样剥（不上行）。
func TestCopyRequestHeaders_ConnectionNamedFields(t *testing.T) {
	dst, src := http.Header{}, http.Header{}
	src.Set("Connection", "x-hop-only")
	src.Set("X-Hop-Only", "should-not-travel")
	src.Set("Anthropic-Version", "2023-06-01")

	copyRequestHeaders(dst, src)

	if got := dst.Get("X-Hop-Only"); got != "" {
		t.Fatalf("Connection 点名的请求头不应带到上游，实际 %q", got)
	}
	if got := dst.Get("Anthropic-Version"); got != "2023-06-01" {
		t.Fatalf("普通请求头应透传，实际 %q", got)
	}
	if got := dst.Get("Accept-Encoding"); got != "identity" {
		t.Fatalf("Accept-Encoding 必须固定 identity（流式不能二次压缩），实际 %q", got)
	}
}

// TestContentEncoding_ForcedGzipDisposition 把「强制 gzip / 解不开的编码」两条处置写成用例，
// 依据见 headers.go 里 copyResponseHeaders 的注释段（这一段是契约，不是实现细节）：
//
//	① 上游无视 Accept-Encoding: identity 仍 gzip → 为遮蔽凭据先解压 →
//	   Content-Encoding **必须剥掉**（body 已是明文，留着下游会再解一次）；
//	   Content-Length 也**不许保留压缩前长度**（要么重算成解压后长度，要么交给 chunked）。
//	② 解不开的编码（br 等）→ 原字节透传 → Content-Encoding **必须保留**，
//	   否则下游把压缩字节当明文（乱码/解析失败）。
func TestContentEncoding_ForcedGzipDisposition(t *testing.T) {
	t.Run("gzip 已解压：剥 Content-Encoding，长度不复用压缩前值", func(t *testing.T) {
		plain := `{"id":"x","choices":[{"message":{"content":"hello"}}]}`
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		_, _ = zw.Write([]byte(plain))
		_ = zw.Close()
		gz := buf.Bytes()

		env := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Content-Length", itoa(len(gz)))
			_, _ = w.Write(gz)
		})
		resp, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","messages":[]}`, nil)

		if string(body) != plain {
			t.Fatalf("解压后应得到明文 body，实际 %q", string(body))
		}
		if enc := resp.Header.Get("Content-Encoding"); enc != "" {
			t.Fatalf("解压后 Content-Encoding 必须剥掉（下游会二次解压失败），实际 %q", enc)
		}
		if got := resp.Header.Get("Content-Length"); got != "" && got != itoa(len(plain)) {
			t.Fatalf("Content-Length 不许复用压缩前长度 %d，实际 %q（解压后为 %d）", len(gz), got, len(plain))
		}
	})

	t.Run("解不开的编码：原字节透传且保留 Content-Encoding", func(t *testing.T) {
		raw := []byte("not-really-brotli-bytes")
		env := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", "br")
			_, _ = w.Write(raw)
		})
		resp, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","messages":[]}`, nil)

		if got := resp.Header.Get("Content-Encoding"); got != "br" {
			t.Fatalf("解不开的编码必须原样保留 Content-Encoding（否则下游按明文解析），实际 %q", got)
		}
		if string(body) != string(raw) {
			t.Fatalf("解不开的编码必须原字节透传（不许丢成空壳），实际 %q", string(body))
		}
	})
}
