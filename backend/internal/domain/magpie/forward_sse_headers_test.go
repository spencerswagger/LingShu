package magpie

import (
	"bufio"
	"bytes"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// SSE 流式路径的响应头防线（回归用例：此前没有任何用例钉住 hop-by-hop 剔除）。
//
// 为什么用裸 TCP 上游而不是 httptest.NewServer：Go 的 http.Server 会自己吞掉/改写
// Transfer-Encoding、Content-Length 等头，测不出「上游/中间层把响应头整包拷来、没剔除」
// 时的真实穿透。实测（2026-11-16）：Go 传输层**不**替你剥 hop-by-hop ——
// resp.Header 里 Connection / Keep-Alive / Proxy-Connection / TE / Trailer / Upgrade 全在，
// 最外层唯一防线就是本包的 responseHopByHop + shouldDropResponseHeader。
//
// 收敛树 API 适配说明（2026-11-16）：本文件原为 test/v2-sse-header-filter 上的
// `NewForwarder` + `ForwardRequest` + `Timeouts` 三件套（那条分支基于 977b5d1 的 magpie），
// 收敛树是 v2 那套（`NewProxy` / `Route` / `Prepared` / `Forward(w,r,route,prep)`），
// 因此这里只换调用形态，**用例名、断言语义、判据文字原样保留**。

// rawHeaderUpstream 起一个裸 TCP 上游，手写响应行/头/体（每个连接应答一次，连接可重复）。
// chunked=true 时用 chunked 框架（真实 SSE 的传输形态，且不带 Content-Length）。
func rawHeaderUpstream(t *testing.T, headers [][2]string, body []byte, chunked bool) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("裸上游监听失败: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	go func() {
		for {
			conn, aerr := ln.Accept()
			if aerr != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				br := bufio.NewReader(conn)
				for { // 吃掉请求头，读到空行为止
					line, rerr := br.ReadString('\n')
					if rerr != nil {
						return
					}
					if line == "\r\n" || line == "\n" {
						break
					}
				}
				var b strings.Builder
				b.WriteString("HTTP/1.1 200 OK\r\n")
				for _, h := range headers {
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
	return "http://" + ln.Addr().String()
}

// sseBodyWithUsage 是一段最小 SSE：两个内容帧 + 一个 usage 帧 + [DONE]。
func sseBodyWithUsage() []byte {
	return []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"there\"}}]}\n\n" +
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":7}}\n\n" +
		"data: [DONE]\n\n")
}

// sseTestProxy 按收敛树的装配方式造转发器（超时口径与生产一致）。
func sseTestProxy(baseURL string) *Proxy {
	return NewProxy(Config{
		BaseURL:         baseURL,
		Connect:         5 * time.Second,
		Request:         10 * time.Second,
		StreamFirstByte: 5 * time.Second,
		StreamIdle:      5 * time.Second,
		CountTokens:     5 * time.Second,
	})
}

// forwardSSE 发一次流式 chat 请求。
//
// ourRequestID 非空时先写进响应头，模拟最外层 server.WithRequestID 中间件已经把链路 ID
// 写好的形态（收敛树里 x-request-id 归外层中间件所有，magpie 只负责「有我们的就不叠上游的」）。
func forwardSSE(t *testing.T, rec *httptest.ResponseRecorder, baseURL, ourRequestID string) Result {
	t.Helper()
	if ourRequestID != "" {
		rec.Header().Set("X-Request-Id", ourRequestID)
	}
	reqBody := []byte(`{"model":"m","stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	return sseTestProxy(baseURL).Forward(rec, req,
		Route{
			Protocol:     ProtocolOpenAIChat,
			Endpoint:     "chat.completions",
			UpstreamPath: "/v1/chat/completions",
			Billable:     true,
			StreamHint:   true,
		},
		Prepared{
			TokenID:         1,
			UserID:          1,
			ExternalModel:   "m",
			MagpieModel:     "m-up",
			UpstreamPath:    "/v1/chat/completions",
			Body:            reqBody,
			StreamRequested: true,
		})
}

// TestForward_StreamResponseHeadersFiltered 是「拷贝头但不剔除」的端到端回归用例：
// 上游/中间层把响应头整包拷过来（含 hop-by-hop），下游只许看到经本包过滤后的结果。
//
// 两个方向都要钉：
//   - 正向：hop-by-hop 一律不许穿透（漏一个就是连接级语义跨代理：可能把上游连接头 /
//     内部路由头亮给租户，或让下游按错误的分帧方式读流）；
//   - 反向：SSE 语义头（Content-Type: text/event-stream、Cache-Control）与普通通用头
//     （X-Accel-Buffering、X-RateLimit-*）必须保留，过滤不能退化成「一刀切全剥」。
func TestForward_StreamResponseHeadersFiltered(t *testing.T) {
	body := sseBodyWithUsage()

	cases := []struct {
		name, value string
		drop        bool   // true=下游必须看不到；false=下游必须原值看到
		why         string // 判定依据（漏了要能一眼看出违反了哪条口径）
	}{
		{"Connection", "keep-alive", true, "hop-by-hop（RFC 7230 §6.1）"},
		{"Keep-Alive", "timeout=5", true, "hop-by-hop"},
		{"Proxy-Connection", "keep-alive", true, "hop-by-hop（非标但中间层常发）"},
		{"TE", "trailers", true, "hop-by-hop"},
		{"Trailer", "X-Sum", true, "hop-by-hop"},
		{"Upgrade", "h2c", true, "hop-by-hop（协议切换不能跨代理）"},
		{"Content-Type", "text/event-stream; charset=utf-8", false, "SSE 语义头：掉了下游就不是流"},
		{"Cache-Control", "no-cache", false, "SSE 语义头（禁缓存）"},
		{"X-Accel-Buffering", "no", false, "反代不缓冲 SSE 的语义头"},
		{"X-RateLimit-Limit", "50", false, "普通通用头：陪跑，证明不是一刀切"},
	}

	// 单个上游一次发出全部头，更接近「整包拷贝」；再加一个由 Connection 点名的自定义头。
	headers := make([][2]string, 0, len(cases)+1)
	for _, c := range cases {
		headers = append(headers, [2]string{c.name, c.value})
	}
	headers = append(headers, [2]string{"X-Internal-Hop", "secret-route"})

	rec := httptest.NewRecorder()
	res := forwardSSE(t, rec, rawHeaderUpstream(t, headers, body, false), "our-rid")
	if res.Err != nil {
		t.Fatalf("转发失败: %v", res.Err)
	}
	// 过滤只动头、不动体：字节仍须逐字节相等，usage 仍须旁路读到。
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("SSE 响应体必须逐字节相等：got %d want %d", rec.Body.Len(), len(body))
	}
	if !res.HasUsage || res.Usage.Input != 11 || res.Usage.Output != 7 {
		t.Fatalf("usage 应仍被旁路读到：%+v has=%v", res.Usage, res.HasUsage)
	}
	if got := rec.Header().Get("x-request-id"); got != "our-rid" {
		t.Fatalf("x-request-id 仍是灵枢自己的，got %q", got)
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := rec.Header().Get(c.name)
			if c.drop {
				if got != "" {
					t.Fatalf("%s 必须剔除（%s），下游却收到 %q", c.name, c.why, got)
				}
				return
			}
			if got != c.value {
				t.Fatalf("%s 必须保留 %q（%s），got %q", c.name, c.value, c.why, got)
			}
		})
	}
}

// TestForward_StreamConnectionTokenHeadersNotCopied 单列「Connection 点名的头」这一类：
// RFC 7230 §6.1 第 2 段规定 `Connection: X-Route` 里点名的字段同样是 hop-by-hop，必须一并丢。
// 上游/中间层常把内部路由头写进 Connection，只按静态表匹配头名就会把它原样漏给租户。
//
// 与上面那条用例分开，是为了让「既有防线」和「已知缺口」各自可判：
// 本条红 = 缺口未修；上面那条红 = 已生效的防线被改坏。
func TestForward_StreamConnectionTokenHeadersNotCopied(t *testing.T) {
	body := sseBodyWithUsage()
	headers := [][2]string{
		{"Content-Type", "text/event-stream; charset=utf-8"},
		{"Connection", "keep-alive, x-internal-hop"},
		{"X-Internal-Hop", "secret-route"},
	}

	rec := httptest.NewRecorder()
	res := forwardSSE(t, rec, rawHeaderUpstream(t, headers, body, false), "our-rid")
	if res.Err != nil {
		t.Fatalf("转发失败: %v", res.Err)
	}
	if got := rec.Header().Get("X-Internal-Hop"); got != "" {
		t.Fatalf("Connection 点名的 X-Internal-Hop 属 hop-by-hop（RFC 7230 §6.1），必须剔除，下游却收到 %q", got)
	}
	// 反向：Connection 本身被丢，不影响 SSE 语义头。
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("SSE 语义头必须保留，got %q", got)
	}
}
