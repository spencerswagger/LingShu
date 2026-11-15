// body_limits_test.go 固化第二轮复审的三条必改（#81）：
//
//	P1-1 query 里的租户令牌必须剔干净（编码变体 / 头鉴权 + query 混用），且未命中时逐字节原样；
//	P2-1 非流式响应超过遮蔽缓冲上限时**不得静默截断**（已读先写 + 剩余转流式到底）；
//	P2-2 声称 gzip 但字节不是 gzip 时**不得丢整段**（回退原始字节 + 留告警）。
//
// 外加 P3：handler 的「无法解码 → 未做遮蔽」告警要有一条用例守着（此前删掉告警全仓依旧全绿）。
package magpie

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/domain/model"
	"github.com/team/llmgateway/internal/pkg/logger"
)

// ---- P1-1：query 键名判定与剔除同源 ----

// 解码口径的单元层断言：`%6Bey` / `ke%79` / `%6B%65y` 都算「key」。
func TestQueryParamKey_DecodedVariants(t *testing.T) {
	cases := []struct {
		raw   string
		has   bool
		strip string
	}{
		{"key=t", true, ""},
		{"%6Bey=t", true, ""},
		{"ke%79=t", true, ""},
		{"%6B%65y=t", true, ""},
		{"%6Bey=t&alt=sse", true, "alt=sse"},
		{"alt=sse&ke%79=t&x=1%2F2", true, "alt=sse&x=1%2F2"},
		{"key", true, ""},
		{"keys=t", false, "keys=t"},
		{"other=t", false, "other=t"},
		// 未命中时**逐字节**原样（含 '&' 连写、尾部空段、原始转义形式）。
		{"alt=sse&&x=1%2F2&empty=&flag", false, "alt=sse&&x=1%2F2&empty=&flag"},
		{"", false, ""},
	}
	for _, c := range cases {
		if got := hasQueryParam(c.raw, "key"); got != c.has {
			t.Fatalf("hasQueryParam(%q)=%v（want %v）", c.raw, got, c.has)
		}
		if got := stripQueryParam(c.raw, "key"); got != c.strip {
			t.Fatalf("stripQueryParam(%q)=%q（want %q）", c.raw, got, c.strip)
		}
	}
	// 判定与剔除必须同源：hasQueryParam 为真 ⇔ stripQueryParam 真的动了字节。
	for _, raw := range []string{"%6Bey=t", "ke%79=t&alt=sse", "key=", "other=1"} {
		if hasQueryParam(raw, "key") == (stripQueryParam(raw, "key") == raw) {
			t.Fatalf("判定与剔除口径不一致: %q", raw)
		}
	}
}

// 端到端：编码变体的 ?key= 也能通过鉴权，且上游 RawQuery 里既没有令牌也没有该参数。
func TestForward_QueryKeyEncodedVariants_Stripped(t *testing.T) {
	for _, variant := range []string{"%6Bey", "ke%79", "%6B%65y"} {
		t.Run(variant, func(t *testing.T) {
			var gotQuery string
			env := newAuthEnv(t, map[string]*model.ExternalModel{"gpt-x": gptModel("gpt-x", "openai/gpt-4o")},
				func(w http.ResponseWriter, r *http.Request) {
					gotQuery = r.URL.RawQuery
					_, _ = w.Write([]byte(`{}`))
				})
			req, err := http.NewRequest(http.MethodPost,
				env.gateway.URL+"/v1beta/models/gpt-x:generateContent?"+variant+"=tenant-token&alt=sse",
				strings.NewReader(`{"contents":[]}`))
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
				t.Fatalf("status=%d body=%s（编码变体的 ?key= 应能通过鉴权）", resp.StatusCode, body)
			}
			if gotQuery != "alt=sse" {
				t.Fatalf("令牌出境或其余参数被改写: query=%q（want %q）", gotQuery, "alt=sse")
			}
		})
	}
}

// 端到端：请求头鉴权 + 同时带 ?key= —— 令牌同样不得出境（此前 fromQuery=false 完全不剔）。
func TestForward_HeaderAuthWithQueryKey_Stripped(t *testing.T) {
	for _, header := range [][2]string{{"Authorization", "Bearer tenant-token"}, {"x-api-key", "tenant-token"}, {"x-goog-api-key", "tenant-token"}} {
		t.Run(header[0], func(t *testing.T) {
			var gotQuery string
			env := newAuthEnv(t, map[string]*model.ExternalModel{"gpt-x": gptModel("gpt-x", "openai/gpt-4o")},
				func(w http.ResponseWriter, r *http.Request) {
					gotQuery = r.URL.RawQuery
					_, _ = w.Write([]byte(`{}`))
				})
			req, err := http.NewRequest(http.MethodPost,
				env.gateway.URL+"/v1beta/models/gpt-x:generateContent?key=tenant-token&alt=sse",
				strings.NewReader(`{"contents":[]}`))
			if err != nil {
				t.Fatalf("build request: %v", err)
			}
			req.Header.Set(header[0], header[1])
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
				t.Fatalf("头鉴权 + query 带 key 时令牌出境: query=%q", gotQuery)
			}
		})
	}
}

// 端到端不变式：没有 key 参数时，RawQuery **逐字节**不变（含 '&' 连写、空值与原始转义）。
func TestForward_NoQueryKey_RawQueryByteIdentical(t *testing.T) {
	const rawQuery = "alt=sse&&x=1%2F2&empty=&flag"
	var gotQuery string
	env := newAuthEnv(t, map[string]*model.ExternalModel{"gpt-x": gptModel("gpt-x", "openai/gpt-4o")},
		func(w http.ResponseWriter, r *http.Request) {
			gotQuery = r.URL.RawQuery
			_, _ = w.Write([]byte(`{}`))
		})
	req, err := http.NewRequest(http.MethodPost,
		env.gateway.URL+"/v1beta/models/gpt-x:generateContent?"+rawQuery, strings.NewReader(`{"contents":[]}`))
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
	if gotQuery != rawQuery {
		t.Fatalf("无 key 时 RawQuery 必须逐字节不变:\n got %q\nwant %q", gotQuery, rawQuery)
	}
}

// ---- P2-1：超缓冲上限的非流式响应不得静默截断 ----

// 上限被调小后，响应体（远大于上限）必须**完整**转发，且跨界之后的凭据仍被遮蔽。
func TestForward_OversizeNonStream_NoSilentTruncation(t *testing.T) {
	old := maskBufferLimit
	maskBufferLimit = 1024
	t.Cleanup(func() { maskBufferLimit = old })

	head := strings.Repeat("A", 2000)
	needlePart := " caller-secret "
	tail := strings.Repeat("B", 1000)
	payload := head + needlePart + tail

	env := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", itoa(len(payload)))
		_, _ = io.WriteString(w, payload)
	})

	resp, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","messages":[]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	want := strings.ReplaceAll(payload, "caller-secret", redactedMarker)
	if len(body) != len(want) {
		t.Fatalf("响应被截断: got %d bytes, want %d", len(body), len(want))
	}
	if string(body) != want {
		t.Fatalf("越界后的字节不符（跨界遮蔽或剩余转发有损）")
	}
	if strings.Contains(string(body), testCallerKey) {
		t.Fatalf("越界之后的凭据未遮蔽（凭据泄漏）")
	}
	if got := resp.Header.Get("Content-Length"); got != "" {
		t.Fatalf("转 chunked 后不该带 Content-Length: %s", got)
	}
	if !env.lastResult(t).CredentialMasked {
		t.Fatal("Result.CredentialMasked 应为 true")
	}
}

// 未越过上限时行为不变（字节原样、Content-Length 按实际长度重算）。
func TestForward_UnderLimit_Unchanged(t *testing.T) {
	payload := `{"error":{"message":"no credential here"}}`
	env := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	})
	_, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","messages":[]}`, nil)
	if string(body) != payload {
		t.Fatalf("未越界时必须零改动: %q", body)
	}
}

// ---- P2-2：声称压缩但字节不是压缩 → 回退原始字节，不丢整段 ----

func TestForward_BogusEncoding_KeepsAllBytes(t *testing.T) {
	payload := strings.Repeat("plain-not-compressed ", 72) // 1512 字节
	cases := []struct{ encoding, body string }{
		{"gzip", payload},
		{"deflate", payload},
		// 真 gzip 的前缀但被截断（头合法、流损坏）：不能只吐已解压部分或空壳。
		{"gzip", truncatedGzip(t, payload)},
	}
	for i, c := range cases {
		env := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("Content-Encoding", c.encoding)
			_, _ = io.WriteString(w, c.body)
		})
		resp, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","messages":[]}`,
			map[string]string{"Accept-Encoding": "identity"})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("case[%d] status=%d", i, resp.StatusCode)
		}
		if s := env.lastResult(t); s.UnmaskableEncoding == "" {
			t.Fatalf("case[%d] 解码失败必须留痕（UnmaskableEncoding 为空）", i)
		}
		if len(body) == 0 {
			t.Fatalf("case[%d] 整段响应被丢光（0 字节）", i)
		}
		// 解码失败 → 必须逐字节回退上游原始字节（诚实透传上游发出的东西 + 告警）。
		if string(body) != c.body {
			t.Fatalf("case[%d] 原始字节未原样回退: got %d bytes, want %d", i, len(body), len(c.body))
		}
	}
}

// truncatedGzip 造一个「头合法但流被截断」的 gzip 体。
func truncatedGzip(t *testing.T, payload string) string {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(payload))
	_ = zw.Close()
	full := buf.Bytes()
	if len(full) < 24 {
		t.Fatalf("gzip 体过短: %d", len(full))
	}
	return string(full[:len(full)-12])
}

// 合法 gzip 仍要解压后遮蔽（P2-2 的修复不得破坏原有正路）。
func TestForward_ValidGzipStillDecoded(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(`{"error":{"message":"bad key caller-secret"}}`))
	_ = zw.Close()
	gz := buf.Bytes()

	env := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Content-Length", itoa(len(gz)))
		_, _ = w.Write(gz)
	})
	resp, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","messages":[]}`,
		map[string]string{"Accept-Encoding": "identity"})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if strings.Contains(string(body), testCallerKey) || !strings.Contains(string(body), redactedMarker) {
		t.Fatalf("合法 gzip 应解压后遮蔽: %s", body)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("解压后不该保留 Content-Encoding: %s", enc)
	}
}

// 合法 deflate 同样要解码（zlib.NewReader 也有「读走一段再失败」的问题）。
func TestForward_ValidDeflateStillDecoded(t *testing.T) {
	var buf bytes.Buffer
	zw := zlib.NewWriter(&buf)
	_, _ = zw.Write([]byte(`{"error":{"message":"bad key caller-secret"}}`))
	_ = zw.Close()
	raw := buf.Bytes()

	env := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "deflate")
		_, _ = w.Write(raw)
	})
	_, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","messages":[]}`,
		map[string]string{"Accept-Encoding": "identity"})
	if strings.Contains(string(body), testCallerKey) || !strings.Contains(string(body), redactedMarker) {
		t.Fatalf("合法 deflate 应解压后遮蔽: %s", body)
	}
}

// 合法 gzip 的**流式**（SSE）响应同样要解压后遮蔽：快照在解码器创建成功后被丢弃
// （否则整条压缩流会攒在内存里），不能因此把流式路径读坏。
func TestForward_StreamGzipStillMasked(t *testing.T) {
	frames := "data: {\"error\":{\"message\":\"bad key caller-secret\"}}\n\ndata: [DONE]\n\n"
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_, _ = zw.Write([]byte(frames))
	_ = zw.Close()
	gz := buf.Bytes()

	env := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(gz)
	})
	_, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","stream":true,"messages":[]}`,
		map[string]string{"Accept-Encoding": "identity"})
	if strings.Contains(string(body), testCallerKey) || !strings.Contains(string(body), redactedMarker) {
		t.Fatalf("流式 gzip 应解压后遮蔽: %s", body)
	}
	if !strings.Contains(string(body), "data: [DONE]") {
		t.Fatalf("流式 gzip 解码后帧结构应完整: %q", body)
	}
}

// ---- P3：解码失败告警必须有用例守着 ----

func TestHandler_UnmaskableEncodingWarns(t *testing.T) {
	var logs bytes.Buffer
	models := map[string]*model.ExternalModel{"tenant-gpt": gptModel("tenant-gpt", "upstream/gpt-4o")}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br")
		_, _ = io.WriteString(w, "\x00\x01brotli-bytes")
	}))
	t.Cleanup(upstream.Close)

	cfg := baseTestConfig()
	cfg.BaseURL = upstream.URL
	cipher, err := encryptForTest(testCallerKey)
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
		Logger:        logger.New(&logs, -4), // DEBUG：告警必被记录
	})
	mux := http.NewServeMux()
	h.Mount(mux)
	gw := httptest.NewServer(mux)
	t.Cleanup(gw.Close)

	req, err := http.NewRequest(http.MethodPost, gw.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"tenant-gpt","messages":[]}`))
	if err != nil {
		t.Fatalf("build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer tenant-token")
	resp, err := gw.Client().Do(req)
	if err != nil {
		t.Fatalf("do request: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.ReadAll(resp.Body)

	out := logs.String()
	if !strings.Contains(out, "上游响应编码无法解码") || !strings.Contains(out, "encoding=br") {
		t.Fatalf("告警未触发（P3 的 Warn 没有生效）:\n%s", out)
	}
}
