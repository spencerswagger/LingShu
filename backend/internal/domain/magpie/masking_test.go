// masking_test.go 固化契约 §8.1：凭据回显遮蔽是「响应字节原样」的**唯一豁免项**。
//
// 口径：只替换本次请求实际用到的那把凭据（含 `Bearer <key>` 形态）→ `<redacted>`；
// 全部状态码生效；无命中则零改动（字节与 Content-Length 都不变）；不做正则泛化。
// 四组形态各测命中/无命中：非流式、流式、SSE 分包切断、gzip 编码。
package magpie

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/model"
)

// 测试里的 callerKey 明文（fakeKeys 给的是 encryptForTest("caller-secret")）。
const testCallerKey = "caller-secret"

func chatModel() map[string]*model.ExternalModel {
	return map[string]*model.ExternalModel{"tenant-gpt": gptModel("tenant-gpt", "upstream/gpt-4o")}
}

// 非流式命中：状态码 200，body 里的凭据被替换，Content-Length 按替换后长度重算。
func TestMask_NonStream_Hit(t *testing.T) {
	upstream := `{"error":{"message":"Incorrect API key provided: caller-secret (Bearer caller-secret)"}}`
	env := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstream)
	})

	resp, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","messages":[]}`, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if strings.Contains(string(body), testCallerKey) {
		t.Fatalf("凭据回显未遮蔽: %s", body)
	}
	if !strings.Contains(string(body), redactedMarker) {
		t.Fatalf("缺少遮蔽标记: %s", body)
	}
	if strings.Contains(string(body), "Bearer <redacted>") == false {
		t.Fatalf("Authorization 形态也应遮蔽: %s", body)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("遮蔽后仍应是合法 JSON: %v (%s)", err, body)
	}
	if got := resp.Header.Get("Content-Length"); got != "" && got != itoa(len(body)) {
		t.Fatalf("Content-Length=%s 与实际长度 %d 不符", got, len(body))
	}
	if !env.lastResult(t).CredentialMasked {
		t.Fatal("Result.CredentialMasked 应为 true")
	}
}

// 非流式无命中：字节与 Content-Length 都保持上游原样（零改动）。
func TestMask_NonStream_Miss_ByteIdentical(t *testing.T) {
	upstream := `{"error":{"message":"rate limit exceeded, retry later"}}`
	env := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", itoa(len(upstream)))
		_, _ = io.WriteString(w, upstream)
	})

	resp, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","messages":[]}`, nil)
	if string(body) != upstream {
		t.Fatalf("无命中必须零改动: got %q want %q", body, upstream)
	}
	if got := resp.Header.Get("Content-Length"); got != itoa(len(upstream)) {
		t.Fatalf("无命中 Content-Length 不该变: %s", got)
	}
	if env.lastResult(t).CredentialMasked {
		t.Fatal("无命中不该标记 CredentialMasked")
	}
}

// 全部状态码生效：上游 4xx/5xx 的错误体同样遮蔽（命中）。
func TestMask_NonStream_AllStatuses(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusUnauthorized, http.StatusInternalServerError, http.StatusServiceUnavailable} {
		upstream := `{"error":{"message":"bad credential caller-secret"}}`
		env := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = io.WriteString(w, upstream)
		})
		resp, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","messages":[]}`, nil)
		if resp.StatusCode != status {
			t.Fatalf("status=%d want %d", resp.StatusCode, status)
		}
		if strings.Contains(string(body), testCallerKey) || !strings.Contains(string(body), redactedMarker) {
			t.Fatalf("status=%d 未遮蔽: %s", status, body)
		}
	}
}

// 不做正则泛化：长相接近的别的标识原样保留（按子串语义只认我们这一把）。
func TestMask_DoesNotGeneralize(t *testing.T) {
	upstream := `{"error":{"message":"key sk-other-key and api-key-xyz mentioned"}}`
	env := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstream)
	})
	_, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","messages":[]}`, nil)
	if !strings.Contains(string(body), "sk-other-key") || !strings.Contains(string(body), "api-key-xyz") {
		t.Fatalf("不该误伤其它标识: %s", body)
	}
	if strings.Contains(string(body), redactedMarker) {
		t.Fatalf("没有正主凭据就不该出现遮蔽标记: %s", body)
	}

	// 子串语义（契约 §8.1 原话是「命中的子串」）：更长的、以我们这把开头的标识
	// 会被一起遮掉前缀——这是有意的「宁可多遮」，但不扩散到其它 key。
	upstream2 := `{"error":{"message":"key caller-secret-2 mentioned"}}`
	env2 := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, upstream2)
	})
	_, body2 := env2.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","messages":[]}`, nil)
	if strings.Contains(string(body2), testCallerKey) || !strings.Contains(string(body2), "<redacted>-2") {
		t.Fatalf("子串语义应遮掉前缀部分: %s", body2)
	}
}

// 流式命中：SSE 帧里的凭据被替换，且帧结构仍然合法（仍是 data: 行 + 空行分隔）。
func TestMask_Stream_Hit(t *testing.T) {
	frames := "data: {\"error\":{\"message\":\"bad key caller-secret\"}}\n\ndata: [DONE]\n\n"
	env := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, frames)
	})
	_, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","stream":true,"messages":[]}`, nil)
	if strings.Contains(string(body), testCallerKey) {
		t.Fatalf("流式未遮蔽: %s", body)
	}
	if !strings.Contains(string(body), redactedMarker) || !strings.Contains(string(body), "data: [DONE]") {
		t.Fatalf("帧结构被破坏: %s", body)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if line != "" && !strings.HasPrefix(line, "data: ") {
			t.Fatalf("出现非 SSE 行: %q", line)
		}
	}
}

// 流式无命中：字节完全一致。
func TestMask_Stream_Miss_ByteIdentical(t *testing.T) {
	frames := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"
	env := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, frames)
	})
	_, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","stream":true,"messages":[]}`, nil)
	if string(body) != frames {
		t.Fatalf("流式无命中必须零改动:\n got %q\nwant %q", body, frames)
	}
}

// SSE 分包把凭据切成两半：仍要命中（跨包暂存），且帧完整。
func TestMask_Stream_SplitAcrossChunks(t *testing.T) {
	whole := "data: {\"error\":{\"message\":\"bad key caller-secret\"}}\n\ndata: [DONE]\n\n"
	cut := strings.Index(whole, testCallerKey) + 6 // "caller" | "-secret…"
	head, tail := whole[:cut], whole[cut:]
	env := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, head)
		if flusher != nil {
			flusher.Flush()
		}
		time.Sleep(30 * time.Millisecond)
		_, _ = io.WriteString(w, tail)
		if flusher != nil {
			flusher.Flush()
		}
	})
	_, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","stream":true,"messages":[]}`, nil)
	if strings.Contains(string(body), testCallerKey) {
		t.Fatalf("跨包切断未命中（凭据泄漏）: %s", body)
	}
	if !strings.Contains(string(body), "data: [DONE]") || !strings.Contains(string(body), redactedMarker) {
		t.Fatalf("跨包遮蔽破坏了帧: %s", body)
	}
}

// gzip 编码：上游无视 Accept-Encoding: identity 仍压缩，解压后照样遮蔽。
func TestMask_GzipEncoded(t *testing.T) {
	plain := `{"error":{"message":"bad key caller-secret"}}`
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
	if strings.Contains(string(body), testCallerKey) {
		t.Fatalf("gzip 响应里的凭据未遮蔽: %s", body)
	}
	if !strings.Contains(string(body), redactedMarker) {
		t.Fatalf("gzip 应解压后遮蔽: %s", body)
	}
	if enc := resp.Header.Get("Content-Encoding"); enc != "" {
		t.Fatalf("解压后不该保留 Content-Encoding: %s", enc)
	}
	if got := resp.Header.Get("Content-Length"); got != "" && got != itoa(len(body)) {
		t.Fatalf("Content-Length=%s 与实际 %d 不符", got, len(body))
	}
	if env.lastResult(t).UnmaskableEncoding != "" {
		t.Fatalf("gzip 应可解码: %s", env.lastResult(t).UnmaskableEncoding)
	}
}

// 解不了的编码（如 br）：按原字节透传并标记，handler 记告警（边界写进设计稿风险段）。
func TestMask_UnsupportedEncodingMarked(t *testing.T) {
	payload := "\x00\x01brotli-bytes"
	env := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "br")
		_, _ = io.WriteString(w, payload)
	})
	_, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","messages":[]}`, nil)
	if string(body) != payload {
		t.Fatalf("无法解码时应按原字节透传: %q", body)
	}
	if env.lastResult(t).UnmaskableEncoding != "br" {
		t.Fatalf("应标记 UnmaskableEncoding: %+v", env.lastResult(t))
	}
}

// 流式分包暂存的尾部在 EOF 时发完（否则尾部字节丢失）。
func TestMask_Stream_TailFlushed(t *testing.T) {
	frames := "data: {\"choices\":[{\"delta\":{\"content\":\"partial pref\"}}]}\n\n"
	env := newTestEnv(t, baseTestConfig(), chatModel(), func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, frames)
	})
	_, body := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","stream":true,"messages":[]}`, nil)
	if string(body) != frames {
		t.Fatalf("尾部字节丢了:\n got %q\nwant %q", body, frames)
	}
}

// itoa 是测试内的极小整数转字符串（避免多处 import strconv）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}
