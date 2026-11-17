package magpie

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
)

// 两条补充用例（2026-11-16 裁定②的后半句：「顺带把 Content-Length 明确保留、
// Content-Encoding 在强制 gzip 下的处置写进注释与用例」）。
//
// 判据不是「头长什么样」，而是**头与体自洽**：
//  ① 本链路不改写响应体（D12 字节透传），所以凡是我们原样转发的体，
//     Content-Length 必须等于实际字节数——不能出现「头说 134、体是 200 字节」；
//  ② 声明了 Content-Encoding: gzip，体就必须真的是 gzip 流——否则下游按 gzip 解压会直接失败
//     （客户端侧表现是「流式中断」而不是「头不对劲」，最难查的一类）。
//
// 这两条都不是「好看」问题：SSE 下游一旦按错误的长度/编码读，就会把后续帧读串。
//
// 收敛树 API 适配说明（2026-11-16）：原文件基于 977b5d1 的 `NewForwarder`/`ForwardRequest`，
// 这里只换成收敛树的 `NewProxy`/`Route`/`Prepared`，**用例名、子场景名、判据原样保留**。
// 一处口径差异要在断言里说清：收敛树的**流式 + 凭据遮蔽**路径会主动删 Content-Length
// （遮蔽可能变长，长度不再成立），所以「长度保留」这条只能用**未挂 callerKey** 的响应来钉；
// 挂上 callerKey 后走 chunked 是设计如此，不是回退。

// gzipBytes 用标准库压一段字节（避免自造压缩器与真实客户端行为不一致）。
func gzipBytes(t *testing.T, in []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(in); err != nil {
		t.Fatalf("gzip 压缩失败: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip 收尾失败: %v", err)
	}
	return buf.Bytes()
}

// forwardStreamWithCallerKey 发一次流式 chat 请求；callerKey 非空表示本次需要走
// 「凭据回显遮蔽」路径（也是唯一会去碰 Content-Encoding 的路径）。
func forwardStreamWithCallerKey(t *testing.T, rec *httptest.ResponseRecorder, baseURL, callerKey string) Result {
	t.Helper()
	rec.Header().Set("X-Request-Id", "our-rid")
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
			CallerKey:       callerKey,
			UpstreamPath:    "/v1/chat/completions",
			Body:            reqBody,
			StreamRequested: true,
		})
}

// TestForward_ContentLengthPreservedOnNonChunked 钉住「Content-Length 明确保留」这条口径。
//
// 上游用**明确长度**（非 chunked）回一段 SSE。本链路不改写体，所以长度仍然成立，
// 必须原样转给下游：下游靠它判「这一响应体有多少字节」，改写或删掉都会让某些客户端
// 提前结束读取（丢帧）或一直等（挂住）。
//
// 用裸 TCP 上游的原因：httptest.NewServer 会自己算/改写长度，测不出「整包拷贝」的真实形态。
// 注意本次**不挂 callerKey**：一旦进入遮蔽路径，流式响应会主动改 chunked（见文件头说明）。
func TestForward_ContentLengthPreservedOnNonChunked(t *testing.T) {
	body := sseBodyWithUsage()

	rec := httptest.NewRecorder()
	res := forwardStreamWithCallerKey(t, rec, rawHeaderUpstream(t, [][2]string{
		{"Content-Type", "text/event-stream; charset=utf-8"},
		{"Cache-Control", "no-cache"},
	}, body, false), /* chunked=false → rawHeaderUpstream 会写 Content-Length */ "")
	if res.Err != nil {
		t.Fatalf("转发失败: %v", res.Err)
	}

	// ① 体逐字节原样（先确认没有改写，长度断言才有意义）
	if !bytes.Equal(rec.Body.Bytes(), body) {
		t.Fatalf("响应体必须逐字节相等：got %d 字节，want %d 字节", rec.Body.Len(), len(body))
	}

	got := rec.Header().Get("Content-Length")
	if got == "" {
		t.Fatalf("Content-Length 必须保留（口径：本条链路不改写体，长度成立）；" +
			"下游拿不到长度只能读到连接关闭，SSE 会退化成「靠关闭判结束」")
	}
	n, err := strconv.Atoi(got)
	if err != nil {
		t.Fatalf("Content-Length 不是整数：%q", got)
	}
	if n != len(body) {
		t.Fatalf("Content-Length 与实际转发字节数不一致：头说 %d，实际 %d —— "+
			"头与体不自洽会让下游读串/提前结束", n, len(body))
	}
}

// TestForward_ForcedGzipContentEncodingSelfConsistent 钉住「强制 gzip」下的处置。
//
// 场景来自实测：中间层（CDN/反代）声明 `Content-Encoding: gzip`，但我们这一跳读到的体可能
// 已经是被解压过的明文（Go 传输层在我们自己带 Accept-Encoding 时会自动解压并**摘掉**这两个头；
// 但中间层硬加的头不会被摘）。这时如果原样把 `Content-Encoding: gzip` 转给下游，
// 下游按 gzip 解一段明文 → 直接报错，表现为「流断了」。
//
// 断言只有一条不变量，不锁具体实现（剥头 or 原样）：
//
//	下游拿到的头与体必须自洽 —— 声明 gzip 就必须能按 gzip 解开；没声明就必须能直接读。
//
// 现状（2026-11-16，收敛树）：解码器在**读头部时**就拒绝（字节根本不是该编码）→
// 剥 `Content-Encoding`、原字节转发、`Content-Length` 保留；「头合法但解压到中途失败（截断）」
// 则保留编码头 + `UnmaskableEncoding` 告警（剥头会让下游把 gzip 字节当明文）。
// 两个子场景分开判，避免「剥头剥过头」这类反向回归。
func TestForward_ForcedGzipContentEncodingSelfConsistent(t *testing.T) {
	plain := sseBodyWithUsage()
	// callerKey 故意不出现在响应体里：本用例只判「头与体自洽」，不掺遮蔽断言。
	const callerKey = "sekret-caller-key-xyz"

	t.Run("真 gzip（体确实是 gzip 流）", func(t *testing.T) {
		gz := gzipBytes(t, plain)
		rec := httptest.NewRecorder()
		res := forwardStreamWithCallerKey(t, rec, rawHeaderUpstream(t, [][2]string{
			{"Content-Type", "text/event-stream; charset=utf-8"},
			{"Content-Encoding", "gzip"},
		}, gz, false), callerKey)
		if res.Err != nil {
			t.Fatalf("转发失败: %v", res.Err)
		}
		// 体是否原样（= 我们没解压）不强求：Go 传输层可能已自行解压。
		// 强求的是自洽：下游看到的体，按下游看到的头能读通。
		assertBodyMatchesEncoding(t, rec, plain)
	})

	t.Run("假 gzip（声明 gzip，体是明文）", func(t *testing.T) {
		rec := httptest.NewRecorder()
		res := forwardStreamWithCallerKey(t, rec, rawHeaderUpstream(t, [][2]string{
			{"Content-Type", "text/event-stream; charset=utf-8"},
			{"Content-Encoding", "gzip"},
		}, plain, false), callerKey)
		if res.Err != nil {
			t.Fatalf("转发失败: %v", res.Err)
		}
		assertBodyMatchesEncoding(t, rec, plain)
	})
}

// assertBodyMatchesEncoding 是上面那条用例的唯一判据：头与体自洽。
//
//	声明 gzip  → 体必须能按 gzip 解开，且解开后等于 want（上游原文）；
//	未声明      → 体必须直接等于 want。
//
// 注意「解开后等于 want」而不是「解开后非空」：后者挡不住「半截明文 + 半截压缩」这类损坏。
func assertBodyMatchesEncoding(t *testing.T, rec *httptest.ResponseRecorder, want []byte) {
	t.Helper()
	got := rec.Body.Bytes()
	if enc := rec.Header().Get("Content-Encoding"); enc == "gzip" {
		zr, err := gzip.NewReader(bytes.NewReader(got))
		if err != nil {
			t.Fatalf("下游收到 Content-Encoding: gzip，但体不是 gzip 流（%v）—— "+
				"客户端解压直接失败，表现为流断了；%d 字节体，头说长度 %q",
				err, len(got), rec.Header().Get("Content-Length"))
		}
		defer zr.Close()
		dec, err := io.ReadAll(zr)
		if err != nil {
			t.Fatalf("gzip 解压失败（体被截断/损坏）：%v", err)
		}
		if !bytes.Equal(dec, want) {
			t.Fatalf("解压后与上游原文不一致：got %d 字节 want %d 字节", len(dec), len(want))
		}
		return
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("未声明 Content-Encoding 时体必须等于上游原文：got %d 字节 want %d 字节",
			len(got), len(want))
	}
	// 长度自洽（只有在我们转发了长度头时才判，避免把「不写长度」判成错）
	if cl := rec.Header().Get("Content-Length"); cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil || n != len(got) {
			t.Fatalf("Content-Length 与体不自洽：头 %q，体 %d 字节（err=%v）", cl, len(got), err)
		}
	}
}
