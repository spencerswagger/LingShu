package magpie_test

// 这道防线的由来（Owl 复审指出）：
// 上游响应头的处理有两条互相拉扯的要求 ——「不许泄漏上游身份」与「正常头必须透传」。
// 最容易出现的错误修法是「为绝不泄漏，干脆一个头都不拷」，那会把 Content-Type、
// Cache-Control、x-ratelimit-* 这些客户端/中间层真正依赖的头一起弄没（流式路径尤其致命：
// 少了 Content-Type: text/event-stream，很多 SDK 直接不按 SSE 解析）。
//
// 所以这里正向锁定「拷贝」这一半：流式路径上，允许的头必须逐值出现在下游，禁止的头必须不在。
// 反向的一半（身份头/供应商头剔除）见 forward_bytes_test.go 与 headers_test.go。

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/domain/magpie"
)

func TestForward_StreamingCopiesAllowedHeaders(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("x-ratelimit-limit", "100")
		w.Header().Add("x-ratelimit-remaining", "9")
		w.Header().Add("x-ratelimit-remaining", "8") // 多值头：不许被压成一个
		w.Header().Set("x-cost-hint", "hint-1")
		w.Header().Set("x-magpie-provider", "vendor-x")
		w.Header().Set("server", "magpie/1.0")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[]}\n\ndata: [DONE]\n\n")
	}))
	defer up.Close()

	f := magpie.NewForwarder()
	rec := httptest.NewRecorder()
	res := f.Forward(t.Context(), rec, magpie.ForwardRequest{
		BaseURL: up.URL, Path: "/v1/chat/completions", Header: http.Header{},
		Body: []byte(`{"model":"m","stream":true}`), RequestID: "our-rid",
		Streaming: true, Billable: true, Protocol: magpie.ProtocolOpenAIChat,
	}, magpie.Timeouts{StreamFirstByte: 5 * time.Second, StreamIdle: 5 * time.Second})
	if res.Err != nil {
		t.Fatalf("转发失败: %v", res.Err)
	}

	// 允许的头：必须拷。
	if got := rec.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type 必须透传（流式缺它 SDK 不按 SSE 解析），got %q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("Cache-Control 必须透传，got %q", got)
	}
	if got := rec.Header().Get("x-ratelimit-limit"); got != "100" {
		t.Fatalf("通用限流头必须透传，got %q", got)
	}
	// 多值头：逐值都在（不是只留一个）。
	remaining := rec.Header().Values("x-ratelimit-remaining")
	if len(remaining) != 2 || remaining[0] != "9" || remaining[1] != "8" {
		t.Fatalf("多值头必须逐值透传，got %v", remaining)
	}
	// 上游自定义头（非身份类）也照常透传 —— 「不拷头」的修法会同时打掉它。
	// 注意：`x-upstream-*` 前缀在本分支的 F8 口径里属于「可能带上游语义」的头，是**刻意剔除**的
	// （见 headers.go 的三段判定），所以这里用中性自定义头验证「拷贝」这一半。
	if got := rec.Header().Get("x-cost-hint"); got != "hint-1" {
		t.Fatalf("上游自定义头应透传，got %q", got)
	}

	// 禁止的头：必须不在。
	if got := rec.Header().Get("x-magpie-provider"); got != "" {
		t.Fatalf("内部标识头必须剔除，got %q", got)
	}
	if got := rec.Header().Get("server"); got != "" {
		t.Fatalf("上游 Server 头必须剔除，got %q", got)
	}
}
