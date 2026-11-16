package magpie

import (
	"io"
	"net/http"
	"testing"

	"github.com/team/llmgateway/internal/domain/model"
)

// DownstreamBytes 是「无 usage 白送」这条链路的唯一度量（Owl 复审 P1-4）：
// 上游 2xx 却拿不到 usage 时本网关不计费、整笔退预扣，如果连「送出去多少字节」都不记，
// 事后既查不出规模也定不了上限。用例钉住「计数 == 下游真收到的字节数」，非流式与流式都要对。

// TestForward_DownstreamBytesMatchesBody_NonStream 非流式：计数等于下游收到的字节数。
func TestForward_DownstreamBytesMatchesBody_NonStream(t *testing.T) {
	upstreamBody := `{"id":"c1","object":"chat.completion","choices":[{"message":{"content":"hi"}}]}` // 故意不带 usage
	env := newTestEnv(t, baseTestConfig(),
		map[string]*model.ExternalModel{"tenant-gpt": gptModel("tenant-gpt", "upstream/gpt-4o")},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, upstreamBody)
		})

	_, got := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","messages":[{"role":"user","content":"hi"}]}`, nil)
	res := env.lastResult(t)
	if int64(len(got)) != res.DownstreamBytes {
		t.Fatalf("非流式 DownstreamBytes=%d，实际下游收到 %d 字节", res.DownstreamBytes, len(got))
	}
	if res.HasUsage {
		t.Fatalf("上游没给 usage，HasUsage 应为 false")
	}
}

// TestForward_DownstreamBytesMatchesBody_Stream 流式：计数含全部 SSE 帧（无 usage 时最需要这个数）。
func TestForward_DownstreamBytesMatchesBody_Stream(t *testing.T) {
	chunks := []string{
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"he\"}}]}\n\n",
		"data: {\"id\":\"c1\",\"choices\":[{\"delta\":{\"content\":\"llo\"}}]}\n\n",
		"data: [DONE]\n\n",
	}
	env := newTestEnv(t, baseTestConfig(),
		map[string]*model.ExternalModel{"tenant-gpt": gptModel("tenant-gpt", "upstream/gpt-4o")},
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for _, c := range chunks {
				_, _ = io.WriteString(w, c)
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		})

	_, got := env.doPost(t, "/v1/chat/completions", `{"model":"tenant-gpt","messages":[],"stream":true}`, nil)
	res := env.lastResult(t)
	if !res.Streamed {
		t.Fatalf("应识别为流式")
	}
	if int64(len(got)) != res.DownstreamBytes {
		t.Fatalf("流式 DownstreamBytes=%d，实际下游收到 %d 字节", res.DownstreamBytes, len(got))
	}
	if res.DownstreamBytes == 0 {
		t.Fatalf("流式无 usage 时 DownstreamBytes 必须非 0（否则量不出白送规模）")
	}
	if res.HasUsage {
		t.Fatalf("上游没给 usage，HasUsage 应为 false")
	}
}
