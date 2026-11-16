package magpie

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/textproto"
	"strings"
	"sync"
	"testing"
)

// 本文件补的是 forward_sse_headers_test.go 那两条用例**没覆盖到的形态**（同一批防线，
// 换传输框架与点名写法），用来保证「拷贝正常头但不剔除」这条回归不会因为框架差异漏网：
//
//  1. 真实 SSE 的传输形态是 chunked（无 Content-Length），已有一条用例只跑了定长响应；
//     上游/中间层在 chunked 下额外带 `Keep-Alive` / `Upgrade`，剔除逻辑不能只在定长分支生效。
//  2. `Connection` 点名的写法不止一种：多值头、多条 Connection 行、点名大小写与实体头大小写
//     不一致（`Connection: X-Internal-Hop` vs 头名 `x-internal-hop`）。只按字面量比较会漏。
//  3. `Connection` 点名在**请求方向**同样成立（入站点名的字段不许上行），这条只在单元层
//     被 `TestCopyRequestHeaders_ConnectionNamedFields` 覆盖，端到端没钉（装配掉了不会有人喊）。
//  4. `content-length` 被点名时的豁免（本链路不改写体，长度仍成立）需要端到端确认，
//     否则「豁免」只写在注释里。
//
// 判据写法保持与既有两条一致：正向（必须剔）+ 反向（必须留），缺一个方向都可能把
// 「一刀切全剥」误判成通过。

// captureUpstream 是带请求头抓取的裸 TCP 上游：手写响应（chunked 或定长），
// 并把收到的请求头解析成 http.Header 供断言（用 http.Server 会把逐跳头吃掉，测不出上行穿透）。
type captureUpstream struct {
	addr string

	mu   sync.Mutex
	reqs []http.Header
}

func newCaptureUpstream(t *testing.T, respHeaders [][2]string, body []byte, chunked bool) *captureUpstream {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("裸上游监听失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	up := &captureUpstream{addr: "http://" + ln.Addr().String()}

	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				br := bufio.NewReader(conn)
				tp := textproto.NewReader(br)
				line, rerr := tp.ReadLine()
				if rerr != nil {
					return
				}
				mh, rerr := tp.ReadMIMEHeader()
				if rerr != nil {
					return
				}
				hdr := http.Header(mh)
				// 记录请求行首（方法/path），便于失败时一眼看出打到了哪。
				hdr.Set("X-Capture-Request-Line", line)

				up.mu.Lock()
				up.reqs = append(up.reqs, hdr)
				up.mu.Unlock()

				var b strings.Builder
				b.WriteString("HTTP/1.1 200 OK\r\n")
				for _, h := range respHeaders {
					b.WriteString(h[0] + ": " + h[1] + "\r\n")
				}
				if chunked {
					b.WriteString("Transfer-Encoding: chunked\r\n\r\n")
					b.WriteString(fmt.Sprintf("%x\r\n", len(body)))
					b.Write(body)
					b.WriteString("\r\n0\r\n\r\n")
				} else {
					b.WriteString(fmt.Sprintf("Content-Length: %d\r\n\r\n", len(body)))
					b.Write(body)
				}
				_, _ = conn.Write([]byte(b.String()))
			}(conn)
		}
	}()
	return up
}

func (u *captureUpstream) first(t *testing.T) http.Header {
	t.Helper()
	u.mu.Lock()
	defer u.mu.Unlock()
	if len(u.reqs) == 0 {
		t.Fatalf("上游没有收到任何请求")
	}
	return u.reqs[0]
}

// TestForward_StreamConnectionTokensChunked 是 chunked SSE 下的「拷贝头但不剔除」回归：
// 上游一次发来 hop-by-hop + Connection 点名头 + SSE 语义头 + 普通通用头，
// 下游只能看到过滤后的结果，且响应体逐字节不变、usage 仍被旁路读到。
func TestForward_StreamConnectionTokensChunked(t *testing.T) {
	body := sseBodyWithUsage()
	headers := [][2]string{
		// 连接级语义（真实 SSE 下上游通常也会带）
		{"Connection", "keep-alive, X-Internal-Hop, x-inner-span"},
		{"Keep-Alive", "timeout=5"},
		{"Upgrade", "h2c"},
		{"Trailer", "X-Sum"},
		// Connection 点名的两个扩展头：一个与点名大小写不同，一个与点名同形
		{"x-internal-hop", "secret-route"},
		{"X-Inner-Span", "42"},
		// 必须原样保留的两类
		{"Content-Type", "text/event-stream; charset=utf-8"},
		{"Cache-Control", "no-cache"},
		{"X-Accel-Buffering", "no"},
		{"X-RateLimit-Limit", "50"},
		// 上游自己的链路 ID：灵枢已有自己的，不许并成第二个值
		{"X-Request-Id", "upstream-rid"},
	}

	rec := httptest.NewRecorder()
	up := newCaptureUpstream(t, headers, body, true)
	res := forwardSSE(t, rec, up.addr, "our-rid")
	if res.Err != nil {
		t.Fatalf("转发失败: %v", res.Err)
	}

	mustDrop := []string{"Connection", "Keep-Alive", "Upgrade", "Trailer", "X-Internal-Hop", "X-Inner-Span"}
	for _, name := range mustDrop {
		if got := rec.Header().Get(name); got != "" {
			t.Fatalf("%s 必须剔除（hop-by-hop 或 Connection 点名），下游却收到 %q", name, got)
		}
	}
	mustKeep := map[string]string{
		"Cache-Control":     "no-cache",
		"X-Accel-Buffering": "no",
		"X-RateLimit-Limit": "50",
	}
	for name, want := range mustKeep {
		if got := rec.Header().Get(name); got != want {
			t.Fatalf("%s 必须原值保留（%q），got %q —— 过滤不能退化成一刀切", name, want, got)
		}
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("SSE 语义头必须保留，got %q", got)
	}
	if got := rec.Header().Values("x-request-id"); len(got) != 1 || got[0] != "our-rid" {
		t.Fatalf("x-request-id 只许灵枢自己那一个值，got %v", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("SSE 响应体必须逐字节相等：got %d want %d", rec.Body.Len(), len(body))
	}
	if !res.HasUsage || res.Usage.Input != 11 || res.Usage.Output != 7 {
		t.Fatalf("usage 应仍被旁路读到：%+v has=%v", res.Usage, res.HasUsage)
	}
}

// TestForward_RequestConnectionTokensNotForwarded 钉请求方向：入站 `Connection` 点名的字段
// 与 `Connection` 本身都不许上行，未被点名的自定义头照旧透传（不许扩大剥离面）。
func TestForward_RequestConnectionTokensNotForwarded(t *testing.T) {
	body := sseBodyWithUsage()
	up := newCaptureUpstream(t, [][2]string{{"Content-Type", "text/event-stream"}}, body, true)

	rec := httptest.NewRecorder()
	reqBody := []byte(`{"model":"m","stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	// 多条 Connection 行 + 大小写混用：逐条解析，不许只认第一条。
	req.Header.Add("Connection", "X-Hop-Only")
	req.Header.Add("Connection", "keep-alive, x-inner-span")
	req.Header.Set("X-Hop-Only", "must-not-leak")
	req.Header.Set("X-Inner-Span", "must-not-leak")
	req.Header.Set("X-Keep-Me", "forwarded")
	req.Header.Set("Anthropic-Beta", "tools-2024")

	res := sseTestProxy(up.addr).Forward(rec, req,
		Route{Protocol: ProtocolOpenAIChat, Endpoint: "chat.completions", UpstreamPath: "/v1/chat/completions", Billable: true, StreamHint: true},
		Prepared{
			TokenID: 1, UserID: 1, ExternalModel: "m", MagpieModel: "m-up",
			UpstreamPath: "/v1/chat/completions", Body: reqBody, StreamRequested: true,
		})
	if res.Err != nil {
		t.Fatalf("转发失败: %v", res.Err)
	}

	got := up.first(t)
	for _, name := range []string{"Connection", "X-Hop-Only", "X-Inner-Span", "Keep-Alive"} {
		if v := got.Get(name); v != "" {
			t.Fatalf("入站点名的 %s 不许上行（逐跳语义），上游却收到 %q", name, v)
		}
	}
	if v := got.Get("X-Keep-Me"); v != "forwarded" {
		t.Fatalf("未被点名的自定义头必须原样上行，got %q", v)
	}
	if v := got.Get("Anthropic-Beta"); v != "tools-2024" {
		t.Fatalf("协议语义头必须原样上行，got %q", v)
	}
	if v := got.Get("Accept-Encoding"); v != "identity" {
		t.Fatalf("Accept-Encoding 固定 identity（流式不能二次压缩），got %q", v)
	}
	if ct := got.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type 按下游原值重设，got %q", ct)
	}
}

// TestForward_StreamConnectionTokenContentLengthExempt 钉「Content-Length 即使被 Connection
// 点名也不剥」：本链路不改写响应体时长度仍成立，剥掉只会让 Go 退化成 chunked。
// 这条是豁免口径的唯一端到端证据（单元层那条只调 copyResponseHeaders）。
func TestForward_StreamConnectionTokenContentLengthExempt(t *testing.T) {
	body := sseBodyWithUsage()
	// 定长响应（rawHeaderUpstream 自己补 Content-Length），并让 Connection 「恶意」点名它。
	up := newCaptureUpstream(t, [][2]string{
		{"Content-Type", "text/event-stream; charset=utf-8"},
		{"Connection", "content-length"},
	}, body, false)

	rec := httptest.NewRecorder()
	res := forwardSSE(t, rec, up.addr, "")
	if res.Err != nil {
		t.Fatalf("转发失败: %v", res.Err)
	}
	if got := rec.Header().Get("Content-Length"); got != fmt.Sprintf("%d", len(body)) {
		t.Fatalf("Content-Length 必须保留原值 %d（被 Connection 点名也不剥），got %q", len(body), got)
	}
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("响应体必须逐字节相等：got %d want %d", rec.Body.Len(), len(body))
	}
}
