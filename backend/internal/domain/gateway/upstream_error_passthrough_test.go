package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"os"
	"reflect"
	"strings"
	"syscall"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/team/llmgateway/internal/domain/channelprotocol"
	"github.com/team/llmgateway/internal/domain/router"
	"github.com/team/llmgateway/internal/pkg/resp"
	"github.com/team/llmgateway/internal/testutil/randbody"
)

// 本文件钉住「上游错误对外透传」契约（设计见
// docs/designs/2026-11-11-upstream-error-visibility-design.md），覆盖**形状**而不是文案：
//
//  1. 对外 HTTP 状态码沿用上游（4xx → 4xx、5xx → 5xx）；
//  2. 上游错误体内容完整可见：OpenAI 形状整段原样（每键每值逐路径保留）、
//     其它 JSON 的原文文本、非 JSON 的原文文本、超长的前缀 + 同字段「已截断」标注；
//  3. 失败同样落账单 + 调用日志，按 request_id 可查，带上游状态码/错误体原文/渠道/模型/耗时；
//  4. 流式与非流式各跑一遍（同一分支代码，两侧断言一致）。
//
// 为什么必须走 HTTP 层：request_id 来自中间件（context + 响应头），透传发生在写出响应的最后一步，
// 只调内部函数会绕过「响应头 x-request-id → 错误体 request_id」这条链路。

// doRequestWithID 以「中间件已生效」的姿态发一次请求：request_id 同时进 context 与响应头
// （对齐 server.WithRequestID 的真实行为），从而能断言错误体 request_id 与调用日志 request_id 同源。
func doRequestWithID(t *testing.T, gw *Gateway, path, token, body, requestID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	req = resp.WithRequestID(req, requestID)
	rec := httptest.NewRecorder()
	rec.Header().Set("x-request-id", requestID)
	gw.Handler().ServeHTTP(rec, req)
	return rec
}

// streamFlagged 返回非流式/流式两份请求体（同一用例两侧各跑一遍）。
func streamFlagged(stream bool) string {
	if stream {
		return `{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	}
	return `{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`
}

// upstreamErrCase 一种上游错误**形状**（不绑定文案，body 由随机生成器产出）。
type upstreamErrCase struct {
	name        string
	status      int // 上游状态码（= 期望的对外状态码）
	contentType string
	body        string
	// mode 决定「内容完整可见」的判据：
	//   verbatim  —— JSON 且 error 为对象：对外 error 子树必须与上游逐路径相等；
	//   rawtext   —— 其它 JSON / 非 JSON：原文文本必须完整出现在 error.message；
	//   truncated —— 超长：message ≤ 上限、含可辨认前缀、同字段带「已截断」标注；
	//   empty     —— 空体：message 非空可读。
	mode string
}

// randomErrCases 随机生成用例集：**不绑定任何固定样例文案**。
//
// 覆盖要求（本任务的验收口径）：
//   - ≥10 组不同的随机值（每轮运行都不同，种子打印在日志里便于复现）；
//   - 8 类响应体形状各至少一组：OpenAI 形状 / 深嵌套 JSON / 平铺 JSON / 顶层数组 /
//     纯文本 / HTML / 空体 / 超长。
//
// 随机化的意义：只要实现里存在「按内容白名单/字段裁剪/文案映射」的特判，随机值就会让它露馅
// （固定样例做不到这点）。
func randomErrCases(rnd *rand.Rand, t *testing.T) []upstreamErrCase {
	t.Helper()
	cases := make([]upstreamErrCase, 0, 20)
	appendCase := func(name string, status int, ct, body, mode string) {
		cases = append(cases, upstreamErrCase{
			name: fmt.Sprintf("%s#%s", name, randbody.Digest(body)), status: status,
			contentType: ct, body: body, mode: mode,
		})
	}

	// ①-⑩ 十组不同的随机值（形状轮换，内容全随机）。
	statuses := []int{http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusConflict,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusNotFound,
		http.StatusServiceUnavailable, http.StatusTeapot, http.StatusBadRequest, http.StatusConflict}
	for i := 0; i < 10; i++ {
		status := statuses[i%len(statuses)]
		switch i % 5 {
		case 0:
			appendCase(fmt.Sprintf("随机OpenAI形状-%d", i), status, "application/json",
				randbody.JSONText(randbody.OpenAIError(rnd)), "verbatim")
		case 1:
			appendCase(fmt.Sprintf("随机深嵌套JSON-%d", i), status, "application/json",
				randbody.JSONText(randbody.Doc(rnd, 4)), "rawtext")
		case 2:
			appendCase(fmt.Sprintf("随机平铺JSON-%d", i), status, "application/json",
				randbody.JSONText(map[string]any{randbody.Word(rnd): randbody.Text(rnd, 5, 60),
					randbody.Word(rnd): rnd.Intn(9999)}), "rawtext")
		case 3:
			appendCase(fmt.Sprintf("随机纯文本-%d", i), status, "text/plain",
				"upstream rejected: "+randbody.Text(rnd, 20, 400), "rawtext")
		case 4:
			appendCase(fmt.Sprintf("随机HTML-%d", i), status, "text/html", randbody.HTML(rnd), "rawtext")
		}
	}

	// ⑪-⑱ 八类形状各一组（内容同样随机）。
	appendCase("形状-OpenAI错误对象", http.StatusBadRequest, "application/json",
		randbody.JSONText(randbody.OpenAIError(rnd)), "verbatim")
	appendCase("形状-深嵌套JSON", http.StatusUnprocessableEntity, "application/json",
		randbody.JSONText(randbody.Doc(rnd, 5)), "rawtext")
	appendCase("形状-平铺JSON", http.StatusBadRequest, "application/json",
		randbody.JSONText(map[string]any{randbody.Word(rnd): randbody.Text(rnd, 1, 40)}), "rawtext")
	appendCase("形状-顶层数组", http.StatusConflict, "application/json",
		randbody.JSONText([]any{randbody.Doc(rnd, 2), randbody.Text(rnd, 3, 30)}), "rawtext")
	// 结尾刻意带换行：`Raw` 不做 TrimSpace，首尾空白也是原文的一部分。
	appendCase("形状-纯文本(带首尾空白)", http.StatusBadRequest, "text/plain",
		"\nupstream says: "+randbody.Text(rnd, 10, 200)+"\n", "rawtext")
	appendCase("形状-HTML错误页", http.StatusBadGateway, "text/html", randbody.HTML(rnd), "rawtext")
	appendCase("形状-空body", http.StatusBadRequest, "application/json", "", "empty")
	appendCase("形状-超长", http.StatusBadRequest, "application/json",
		randbody.JSONText(map[string]any{"error": map[string]any{
			"message":          randbody.Text(rnd, 40000, 60000),
			randbody.Word(rnd): randbody.Doc(rnd, 2),
		}}), "truncated")
	return cases
}

func TestServeUpstreamError_RandomShapes(t *testing.T) {
	seed := time.Now().UnixNano()
	rnd := rand.New(rand.NewSource(seed))
	t.Logf("随机种子 seed=%d（失败可用它复现）", seed)

	for _, tc := range randomErrCases(rnd, t) {
		tc := tc
		for _, stream := range []bool{false, true} {
			stream := stream
			name := tc.name + "/非流式"
			if stream {
				name = tc.name + "/流式"
			}
			t.Run(name, func(t *testing.T) {
				up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if tc.contentType != "" {
						w.Header().Set("Content-Type", tc.contentType)
					}
					w.WriteHeader(tc.status)
					_, _ = w.Write([]byte(tc.body))
				}))
				defer up.Close()

				h := build(t, up.URL)
				const rid = "req-shape"
				rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(stream), rid)

				// ① 状态码沿用上游（不再统一改写为 502/503/400）。
				if rec.Code != tc.status {
					t.Fatalf("对外状态码必须沿用上游 %d, got %d: %s", tc.status, rec.Code, rec.Body.String())
				}
				var out map[string]any
				if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
					t.Fatalf("对外响应必须是 JSON: %v (%s)", err, rec.Body.String())
				}
				if out["request_id"] != rid {
					t.Errorf("顶层 request_id want %q，got %v", rid, out["request_id"])
				}
				if got, ok := out["upstream_status"].(float64); !ok || int(got) != tc.status {
					t.Errorf("顶层 upstream_status want %d，got %v", tc.status, out["upstream_status"])
				}
				msg := errorMessage(t, out)

				// ② 内容完整可见（按形状断言：只看形状，不看文案）。
				switch tc.mode {
				case "verbatim":
					assertErrorSubtreeVerbatim(t, tc.body, out)
				case "rawtext":
					if strings.TrimSpace(msg) != strings.TrimSpace(tc.body) {
						t.Fatalf("原文必须完整进 message，want %q，got %q", tc.body, msg)
					}
				case "truncated":
					if len(msg) > upstreamErrOutBytes {
						t.Fatalf("message 应截断到 %d 字节内，got %d", upstreamErrOutBytes, len(msg))
					}
					if !utf8.ValidString(msg) {
						t.Fatal("截断后的 message 必须是合法 UTF-8（不能切出半个字符）")
					}
					if !strings.Contains(msg, "已截断") {
						t.Fatalf("截断必须在**同一字段**里显式标注，got %q", tail(msg, 120))
					}
					if len(rec.Body.Bytes()) > 64<<10 {
						t.Fatalf("对外体量应 ≤64KB，got %d", len(rec.Body.Bytes()))
					}
				case "empty":
					if strings.TrimSpace(msg) == "" {
						t.Fatal("空上游体也必须给出可读 message，不能是空串")
					}
				}

				// ③ 失败必须落账单 + 调用日志，且**原文逐字节**在运营可见处。
				assertFailureTraced(t, h, rid, tc.status, tc.body)
			})
		}
	}
}

// assertErrorSubtreeVerbatim 断言「OpenAI 形状整段原样」：对外 error 子树与上游逐路径相等
// （多字段、嵌套、未知字段一律不得丢改），且上游响应的其它顶层字段也照旧保留。
func assertErrorSubtreeVerbatim(t *testing.T, upBody string, out map[string]any) {
	t.Helper()
	var upDoc map[string]any
	if err := json.Unmarshal([]byte(upBody), &upDoc); err != nil {
		t.Fatalf("样本必须是 JSON: %v", err)
	}
	wantErr, ok := upDoc["error"]
	if !ok {
		t.Fatalf("样本必须是 OpenAI 形状: %s", upBody)
	}
	if !reflect.DeepEqual(out["error"], wantErr) {
		t.Fatalf("error 子树必须原样透传（不许删改任何字段）\nwant %#v\ngot  %#v", wantErr, out["error"])
	}
	for k, v := range upDoc {
		if k == "error" {
			continue
		}
		if !reflect.DeepEqual(out[k], v) {
			t.Errorf("上游顶层字段 %q 不得被丢弃/改写: want %#v got %#v", k, v, out[k])
		}
	}
}

// errorMessage 取对外错误体的 error.message（两种形状共用同一路径）。
func errorMessage(t *testing.T, out map[string]any) string {
	t.Helper()
	e, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("对外响应必须带 error 对象: %#v", out["error"])
	}
	m, _ := e["message"].(string)
	return m
}

// assertOperatorSeesExact 断言**运营侧**（调用日志 resp_body，账单详情直接展示）拿到的就是上游原文：
// 除「本次实际使用的凭据原值」外**逐字节**一致——不做字段挑选、不比对某个片段。
//
// 超长（> callLogErrBodyMaxBytes）时改为断言两件事：
//  1. 保留的前缀与上游原文逐字节一致（不是「含有某段」）；
//  2. 同一字段里有显式截断标注，且标注里的原始字节数正确。
func assertOperatorSeesExact(t *testing.T, what, stored, upstream string, secrets ...string) {
	t.Helper()
	want := upstream
	for _, sec := range secrets {
		want = strings.ReplaceAll(want, sec, "***")
	}
	if len(want) <= callLogErrBodyMaxBytes {
		if stored != want {
			t.Fatalf("%s 必须是上游原文（逐字节），len(want)=%d len(got)=%d\nwant=%q\ngot =%q",
				what, len(want), len(stored), head(want, 400), head(stored, 400))
		}
		return
	}
	marker := "\n…（已截断"
	idx := strings.Index(stored, marker)
	if idx < 0 {
		t.Fatalf("%s 超长必须带显式截断标注（同字段），got=%q", what, head(stored, 400))
	}
	kept := stored[:idx]
	if !strings.HasPrefix(want, kept) {
		t.Fatalf("%s 截断前缀必须与上游原文逐字节一致（kept=%d 字节）", what, len(kept))
	}
	if len(kept) < callLogErrBodyMaxBytes-512 {
		t.Fatalf("%s 保留量过小（%d 字节），上限 %d 字节利用率不足", what, len(kept), callLogErrBodyMaxBytes)
	}
	// 标注必须给出**可核对**的原始规模：读侧未到上限时是精确字节数，到了读取上限就如实说「超过上限」
	// （不能编一个数：超过 64KB 的报文我们根本没读完）。
	wantNote := fmt.Sprintf("共 %d 字节", len(want))
	if len(want) > upstreamErrReadBytes {
		wantNote = fmt.Sprintf("超过读取上限 %d 字节", upstreamErrReadBytes)
	}
	if !strings.Contains(stored[idx:], wantNote) {
		t.Fatalf("%s 截断标注必须说明原始规模（want 含 %q），got=%q", what, wantNote, stored[idx:])
	}
}

// head 取字符串前 n 字节（失败信息展示用）。
func head(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// assertFailureTraced 账单 + 调用日志成对留痕：按 request_id 可查，带上游状态码/原文/渠道/模型/耗时。
func assertFailureTraced(t *testing.T, h *harness, rid string, upStatus int, upBody string) {
	t.Helper()
	if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail {
		t.Fatalf("失败必须落 1 条 failed 账单, got %+v", h.billing.reqs)
	}
	if len(h.clogs.inserted) != 1 {
		t.Fatalf("失败必须落 1 条调用日志（账单页按 request_id 检索经 call_logs 关联）, got %d", len(h.clogs.inserted))
	}
	cl := h.clogs.inserted[0]
	if cl.RequestID != rid {
		t.Errorf("调用日志 request_id want %q，got %q", rid, cl.RequestID)
	}
	if cl.Status != "failed" || cl.RespKind != "error" {
		t.Errorf("调用日志 want status=failed/resp_kind=error, got %s/%s", cl.Status, cl.RespKind)
	}
	if cl.UpstreamStatus == nil || *cl.UpstreamStatus != upStatus {
		t.Errorf("调用日志 upstream_status want %d，got %v", upStatus, cl.UpstreamStatus)
	}
	if cl.ChannelID == nil || *cl.ChannelID == 0 {
		t.Errorf("调用日志必须记渠道, got %v", cl.ChannelID)
	}
	if cl.Model != "gpt-4" {
		t.Errorf("调用日志 model want gpt-4，got %q", cl.Model)
	}
	if cl.DurationMs == nil {
		t.Error("调用日志必须记耗时 duration_ms")
	}
	// created_at 由 SQLCallLogStore.Insert 在落库时补齐（内存 fake 拿不到），故此处只校验耗时字段。
	if upBody == "" {
		return
	}
	// 运营侧可见：resp_body 是原文（逐字节），error_message 保留同一原文（摘要上限内原样）。
	assertOperatorSeesExact(t, "调用日志 resp_body", cl.RespBody, upBody, harnessCredential)
	prefix := upstreamErrNote(upStatus) + ": "
	if !strings.HasPrefix(cl.ErrorMessage, prefix) {
		t.Fatalf("error_message 应以可读状态说明开头，got %q", head(cl.ErrorMessage, 120))
	}
	got := strings.TrimPrefix(cl.ErrorMessage, prefix)
	if len(upBody) <= errDetailBodyMaxBytes {
		if got != upBody {
			t.Fatalf("error_message 必须原样保留上游原文，want=%q got=%q", head(upBody, 200), head(got, 200))
		}
		return
	}
	kept := strings.SplitN(got, "\n…（已截断", 2)[0]
	if !strings.HasPrefix(upBody, kept) {
		t.Fatalf("error_message 截断前缀必须与原文逐字节一致（kept=%d 字节）", len(kept))
	}
}

// harnessCredential 与 build() 里注入给密钥运行时的明文凭据一致：
// 运营视图只允许遮蔽它（精确值），其它内容一律原样。
const harnessCredential = "test-key"

// TestServeUpstreamError_TerminalKeepsLastUpstream 可转移状态（401/403/404/429/5xx）在上游只有一个候选时
// 会走到「候选全部失败」的终结分支：此时对外必须是**上游状态码 + 上游报文**，
// 不能退化成 503「无可用渠道」聚合文案。
func TestServeUpstreamError_TerminalKeepsLastUpstream(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "非流式"
		if stream {
			name = "流式"
		}
		t.Run(name, func(t *testing.T) {
			body := `{"error":{"type":"server_error","message":"upstream overloaded: retry after 30s","code":"overloaded"}}`
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte(body))
			}))
			defer up.Close()

			h := build(t, up.URL)
			const rid = "req-terminal"
			rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(stream), rid)
			if rec.Code != http.StatusInternalServerError {
				t.Fatalf("终结响应必须沿用上游 500, got %d: %s", rec.Code, rec.Body.String())
			}
			var out map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			assertErrorSubtreeVerbatim(t, body, out)
			if out["request_id"] != rid {
				t.Errorf("request_id want %q，got %v", rid, out["request_id"])
			}
			assertFailureTraced(t, h, rid, http.StatusInternalServerError, body)
		})
	}
}

// TestServeUpstreamError_429TerminalKeepsUpstreamBody 上游 429 全候选限流时，
// 终结响应仍是 429，但必须带上游报文（是配额还是速率、何时重试，调用方需要知道）。
func TestServeUpstreamError_429TerminalKeepsUpstreamBody(t *testing.T) {
	body := `{"error":{"type":"rate_limit_error","message":"Rate limit reached for gpt-4: 20 req/min","code":"rate_limit_exceeded"}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(body))
	}))
	defer up.Close()

	h := build(t, up.URL)
	rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), "req-429")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("want 429, got %d: %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	assertErrorSubtreeVerbatim(t, body, out)
}

// TestServeUpstreamError_ConnectionFailure 断网（连接被拒）：没有上游报文可透传，
// 对外 502 且**点名连接失败原因**（不再只说「服务暂时不可用」），调用日志不写 upstream_status。
func TestServeUpstreamError_ConnectionFailure(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // 关闭监听 → 连接被拒

	h := build(t, deadURL)
	rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), "req-dead")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", rec.Code, rec.Body.String())
	}
	rb := decodeOpenAIError(t, rec)
	if !strings.Contains(rb.Error.Message, "连接失败") {
		t.Fatalf("必须点名连接失败原因, got %q", rb.Error.Message)
	}
	if !strings.Contains(rb.Error.Message, "connect") && !strings.Contains(rb.Error.Message, "refused") {
		t.Errorf("应带上底层错误线索, got %q", rb.Error.Message)
	}
	if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail {
		t.Fatalf("连接失败须落 failed 账单, got %+v", h.billing.reqs)
	}
	if len(h.clogs.inserted) != 1 {
		t.Fatalf("连接失败须落调用日志, got %d", len(h.clogs.inserted))
	}
	cl := h.clogs.inserted[0]
	if cl.RequestID != "req-dead" {
		t.Errorf("调用日志 request_id want req-dead，got %q", cl.RequestID)
	}
	if cl.UpstreamStatus != nil {
		t.Errorf("连接失败没有上游状态码，不应编造, got %v", *cl.UpstreamStatus)
	}
	if !strings.Contains(cl.ErrorMessage, "连接失败") {
		t.Errorf("调用日志应写清连接失败原因，got %q", cl.ErrorMessage)
	}
}

// TestServeUpstreamError_Timeout 已送达后上游无响应（超时/断连）：对外 504 并点名超时，
// 调用日志留痕且不写 upstream_status（本次没有上游状态码）。
func TestServeUpstreamError_Timeout(t *testing.T) {
	live := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer live.Close()

	h := build(t, live.URL)
	h.gw.client = &http.Client{Transport: &timeoutFailTransport{}}

	rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), "req-timeout")
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("超时应回 504, got %d: %s", rec.Code, rec.Body.String())
	}
	rb := decodeOpenAIError(t, rec)
	if !strings.Contains(rb.Error.Message, "超时") {
		t.Fatalf("必须点名超时原因, got %q", rb.Error.Message)
	}
	if len(h.clogs.inserted) != 1 {
		t.Fatalf("超时须落调用日志, got %d", len(h.clogs.inserted))
	}
	cl := h.clogs.inserted[0]
	if cl.RequestID != "req-timeout" || cl.Status != "failed" {
		t.Errorf("调用日志 want request_id=req-timeout/status=failed, got %q/%q", cl.RequestID, cl.Status)
	}
	if cl.UpstreamStatus != nil {
		t.Errorf("超时不应写 upstream_status, got %v", *cl.UpstreamStatus)
	}
}

// TestServeUpstreamError_SuccessStaysSuccess 回归守卫：上游 2xx 一律不得被改写成错误，
// 响应体逐字节不变（[DONE] 保留），且调用日志不写 upstream_status。
func TestServeUpstreamError_SuccessStaysSuccess(t *testing.T) {
	vendorJSON := `{"id":"x","object":"chat.completion","vendor_ext":{"region":"eu"},"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`
	sse := "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n" +
		"data: [DONE]\n\n"
	cases := []struct {
		name        string
		stream      bool
		contentType string
		body        string
	}{
		{name: "普通JSON", contentType: "application/json", body: testOKBody},
		{name: "含厂商扩展字段", contentType: "application/json", body: vendorJSON},
		{name: "SSE流式", stream: true, contentType: "text/event-stream", body: sse},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", tc.contentType)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer up.Close()

			h := build(t, up.URL)
			rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(tc.stream), "req-ok")
			if rec.Code != http.StatusOK {
				t.Fatalf("上游成功不得被改写, got %d: %s", rec.Code, rec.Body.String())
			}
			if got := rec.Body.String(); got != tc.body {
				t.Fatalf("成功响应必须逐字节透传\nwant %q\ngot  %q", tc.body, got)
			}
			if len(h.clogs.inserted) != 1 || h.clogs.inserted[0].Status != "completed" {
				t.Fatalf("want 1 条 completed 调用日志, got %+v", h.clogs.inserted)
			}
			if h.clogs.inserted[0].UpstreamStatus != nil {
				t.Errorf("成功记录不应写 upstream_status, got %v", *h.clogs.inserted[0].UpstreamStatus)
			}
		})
	}
}

// TestUpstreamErrorPayload_Shapes 直接对组装函数做形状级断言（不经 HTTP），
// 覆盖 HTTP 层不易构造的边界：截断后不再是合法 JSON、顶层数组、标量、只有 error 字符串。
func TestUpstreamErrorPayload_Shapes(t *testing.T) {
	// 截断后的 OpenAI 形状不再是合法 JSON → 退化为「原文文本进 message + 同字段标注」。
	longBody := `{"error":{"message":"` + strings.Repeat("x", 9000) + `"}}`
	payload, _ := upstreamErrorPayload(http.StatusBadRequest, http.StatusBadRequest, longBody, "rid").(upstreamErrorEnvelope)
	if !strings.Contains(payload.Error.Message, "已截断") || len(payload.Error.Message) > upstreamErrOutBytes {
		t.Fatalf("超长必须在同一字段标注截断且不超上限, got len=%d", len(payload.Error.Message))
	}
	// 标量 JSON（非对象）→ 原文文本进 message。
	payload, _ = upstreamErrorPayload(http.StatusBadRequest, http.StatusBadRequest, `"plain string error"`, "rid").(upstreamErrorEnvelope)
	if payload.Error.Message != `"plain string error"` {
		t.Fatalf("标量 JSON 应按原文文本透出, got %q", payload.Error.Message)
	}
	// error 是字符串（非 OpenAI 形状）→ 原文文本进 message，不硬解成 error 子对象。
	payload, _ = upstreamErrorPayload(http.StatusBadRequest, http.StatusBadRequest, `{"error":"boom"}`, "rid").(upstreamErrorEnvelope)
	if payload.Error.Message != `{"error":"boom"}` {
		t.Fatalf("非 OpenAI 形状应按原文文本透出, got %q", payload.Error.Message)
	}
	// OpenAI 形状且上游自带 request_id 时不覆盖上游的（只追加我们缺的字段）。
	m, ok := upstreamErrorPayload(http.StatusBadRequest, http.StatusBadRequest,
		`{"error":{"message":"x"},"request_id":"upstream-rid"}`, "gw-rid").(map[string]json.RawMessage)
	if !ok {
		t.Fatal("OpenAI 形状应走整段透传")
	}
	if string(m["request_id"]) != `"upstream-rid"` {
		t.Fatalf("不得改写上游自带的 request_id, got %s", m["request_id"])
	}
}

// TestServeRejectPaths_RecordBillingAndCallLog 网关自身拒绝（未进入上游转发）同样必须「账单 + 调用日志」成对：
// 账单页按 request_id 检索是经 call_logs 关联的（billing/query.go），只落账单不落调用日志时，
// 调用方拿着 response 里的 request_id 在账单页搜不到这次失败。
func TestServeRejectPaths_RecordBillingAndCallLog(t *testing.T) {
	cases := []struct {
		name     string
		body     string
		wantCode int
		setup    func(h *harness)
	}{
		{
			name:     "请求体格式错误",
			body:     `{not-json`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "请求体缺少 model",
			body:     `{"stream":false}`,
			wantCode: http.StatusBadRequest,
		},
		{
			name:     "余额不足(预扣拒绝)",
			body:     `{"model":"gpt-4","stream":false,"messages":[{"role":"user","content":"hi"}]}`,
			wantCode: http.StatusPaymentRequired,
			setup: func(h *harness) {
				fc := &fakeCredit{balance: 0}
				h.gw.credit = fc
			},
		},
		{
			name:     "模型未配置",
			body:     `{"model":"ghost","stream":false}`,
			wantCode: http.StatusNotFound,
			setup: func(h *harness) {
				h.rrouter.result = nil
				h.rrouter.err = &router.RouteError{Kind: router.KindModelNotFound,
					HTTPStatus: http.StatusNotFound, Code: resp.CodeNotFound, Message: "model not found"}
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := build(t, "http://unused")
			if tc.setup != nil {
				tc.setup(h)
			}
			const rid = "req-reject"
			rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", tc.body, rid)
			if rec.Code != tc.wantCode {
				t.Fatalf("want %d, got %d: %s", tc.wantCode, rec.Code, rec.Body.String())
			}
			if len(h.billing.reqs) != 1 || !h.billing.reqs[0].Fail {
				t.Fatalf("拒绝路径应落 1 条 failed 账单, got %+v", h.billing.reqs)
			}
			if len(h.clogs.inserted) != 1 {
				t.Fatalf("拒绝路径应落 1 条调用日志（否则账单页按 request_id 搜不到）, got %d", len(h.clogs.inserted))
			}
			cl := h.clogs.inserted[0]
			if cl.RequestID != rid {
				t.Errorf("调用日志 request_id want %q，got %q", rid, cl.RequestID)
			}
			if cl.Status != "failed" || cl.ErrorMessage == "" {
				t.Errorf("调用日志 want status=failed 且带原因, got status=%q err=%q", cl.Status, cl.ErrorMessage)
			}
			if cl.BillingID != h.billing.bids[0] {
				t.Errorf("调用日志应与账单 1:1 关联, got billing_id=%q want %q", cl.BillingID, h.billing.bids[0])
			}
			if rb := decodeOpenAIError(t, rec); rb.RequestID != rid {
				t.Errorf("错误体 request_id want %q，got %q", rb.RequestID, rid)
			}
		})
	}
}

// TestCallLogOptWithUpstreamStatus 可选字段填充器不改变默认行为（未传 opt 时 UpstreamStatus 为 nil）。
func TestCallLogOptWithUpstreamStatus(t *testing.T) {
	cl := &CallLog{}
	for _, opt := range []callLogOpt{nil, withUpstreamStatus(http.StatusTooManyRequests)} {
		if opt != nil {
			opt(cl)
		}
	}
	if cl.UpstreamStatus == nil || *cl.UpstreamStatus != http.StatusTooManyRequests {
		t.Fatalf("withUpstreamStatus 未生效: %+v", cl.UpstreamStatus)
	}
	var other CallLog
	if other.UpstreamStatus != nil {
		t.Fatalf("未传 opt 时应为空, got %v", *other.UpstreamStatus)
	}
}

// timeoutErr 满足 net.Error 且 Timeout()=true（模拟上游响应超时）。
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// timeoutFailTransport 模拟「请求已送达后上游超时」：先触发 WroteRequest，再返回超时错误。
type timeoutFailTransport struct{}

func (timeoutFailTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if tr := httptrace.ContextClientTrace(req.Context()); tr != nil && tr.WroteRequest != nil {
		tr.WroteRequest(httptrace.WroteRequestInfo{Err: nil})
	}
	return nil, timeoutErr{}
}

// tail 取字符串末尾 n 字节（断言失败时展示截断标注用）。
func tail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

// TestUpstreamTransportNote_NoAddressLeak 传输层失败的对外文案**不得带渠道地址**（内网拓扑泄露），
// 但仍要能看出「哪一类失败」——不能退回「服务暂时不可用」这种通用文案。
func TestUpstreamTransportNote_NoAddressLeak(t *testing.T) {
	opErr := &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: &os.SyscallError{Syscall: "connect", Err: syscall.ECONNREFUSED},
	}
	urlErr := &url.Error{Op: "Post", URL: "http://internal-gw.corp.local:18080/chat/completions", Err: opErr}
	cases := []struct {
		name     string
		err      error
		wantSub  string
		timedOut bool
	}{
		{"连接被拒", urlErr, "连接被拒绝", false},
		{"超时", &url.Error{Op: "Post", URL: "https://10.1.2.3:8443/v1", Err: timeoutErr{}}, "超时", true},
		{"对端断开", &url.Error{Op: "Post", URL: "http://10.1.2.3:8443/v1", Err: io.ErrUnexpectedEOF}, "连接被上游中断", false},
		{"DNS", &url.Error{Op: "Post", URL: "https://gw.internal:443/x", Err: &net.DNSError{Err: "no such host", Name: "gw.internal"}}, "域名解析失败", false},
		{"未归类", errors.New(`Post "http://10.1.2.3:9000/v1/chat/completions": some weird transport failure`), "失败", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			note, timedOut := upstreamTransportNote("连接", tc.err)
			if timedOut != tc.timedOut {
				t.Errorf("timedOut want %v, got %v", tc.timedOut, timedOut)
			}
			if !strings.Contains(note, tc.wantSub) {
				t.Errorf("原因必须可读（含 %q），got %q", tc.wantSub, note)
			}
			for _, leak := range []string{"internal-gw", "corp.local", "10.1.2.3", "18080", "8443", "9000", "http://", "https://", "gw.internal"} {
				if strings.Contains(note, leak) {
					t.Fatalf("对外文案泄露了渠道地址片段 %q: %q", leak, note)
				}
			}
		})
	}
}

// TestServeUpstreamError_ConnectionFailureNoAddress HTTP 级：断网时对外 message 不含上游地址/端口，
// 但要点名「连接失败」。
func TestServeUpstreamError_ConnectionFailureNoAddress(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	deadURL := dead.URL // http://127.0.0.1:<port>
	dead.Close()

	h := build(t, deadURL)
	rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), "req-noleak")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d: %s", rec.Code, rec.Body.String())
	}
	rb := decodeOpenAIError(t, rec)
	if !strings.Contains(rb.Error.Message, "连接失败") {
		t.Fatalf("要点名连接失败, got %q", rb.Error.Message)
	}
	if strings.Contains(rb.Error.Message, "127.0.0.1") || strings.Contains(rb.Error.Message, "http://") {
		t.Fatalf("对外文案不得出现上游地址/端口: %q", rb.Error.Message)
	}
}

// TestWriteUpstreamError_UpstreamStatusIsRealCode 响应体顶层的 upstream_status 必须是**上游真实状态码**，
// 不是对外改写后的码：上游 304（对外按 502 输出）与「策略把 422 覆盖成 418」两种路径都要如实上报。
func TestWriteUpstreamError_UpstreamStatusIsRealCode(t *testing.T) {
	// 路径一：上游 304 → 对外 502，但 upstream_status 仍是 304。
	rec := httptest.NewRecorder()
	rec.Header().Set("x-request-id", "rid-304")
	writeUpstreamError(rec, http.StatusNotModified, http.StatusNotModified, `{"error":{"message":"not modified"}}`, "")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("304 应对外 502, got %d", rec.Code)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := out["upstream_status"].(float64); int(got) != http.StatusNotModified {
		t.Fatalf("upstream_status 必须是上游真实码 304, got %v", out["upstream_status"])
	}

	// 路径二：协议策略把上游 422 覆盖成 418 → upstream_status 仍是 422。
	up := upstreamErrServer(t, http.StatusUnprocessableEntity, errBody422)
	h := build(t, up.URL)
	no := false
	withPolicy(h, 9, []channelprotocol.ErrorPolicy{{
		Scope: channelprotocol.ErrorScopeChannel,
		Match: channelprotocol.ErrorMatch{Status: http.StatusUnprocessableEntity},
		Disposition: channelprotocol.ErrorDisposition{
			Failover:         &no,
			DownstreamStatus: http.StatusTeapot,
		},
	}})
	rec2 := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), "rid-418")
	if rec2.Code != http.StatusTeapot {
		t.Fatalf("策略覆盖应对外 418, got %d: %s", rec2.Code, rec2.Body.String())
	}
	var out2 map[string]any
	if err := json.Unmarshal(rec2.Body.Bytes(), &out2); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, _ := out2["upstream_status"].(float64); int(got) != http.StatusUnprocessableEntity {
		t.Fatalf("策略覆盖时 upstream_status 必须仍是上游真实码 422, got %v", out2["upstream_status"])
	}
}

// TestOperatorView_SanitizesInvalidUTF8 上游报文含非法 UTF-8 / 0x00 时，
// 运营视图必须产出合法 UTF-8（否则 PG text 列整行 INSERT 失败，这次失败的账与日志一起消失）。
func TestOperatorView_SanitizesInvalidUTF8(t *testing.T) {
	raw := "{\"error\":\"\xd6\xd0\xce\xc4\x00tail\"}"
	view := upstreamErrBody{Raw: raw}.operatorView()
	if !utf8.ValidString(view) {
		t.Fatalf("运营视图必须是合法 UTF-8: %q", view)
	}
	if strings.ContainsRune(view, 0) {
		t.Fatalf("不能含 NUL（PG text 存不了）: %q", view)
	}
	if !strings.Contains(view, "已替换") {
		t.Fatalf("替换必须在同一字段标注出来: %q", view)
	}
	// 合法 UTF-8 且无 NUL 时零改动（不因清洗动到原文）。
	clean := `{"error":"中文 ok"}`
	if got := (upstreamErrBody{Raw: clean}).operatorView(); got != clean {
		t.Fatalf("合法原文不得被改写: want=%q got=%q", clean, got)
	}
}

// TestServeUpstreamError_DownstreamNeverSeesChannelCredential 上游把收到的鉴权头回显在错误体里时，
// **对外响应**必须把渠道凭据遮蔽掉：下游 API 用户只持有自己的令牌，不该拿到我们调上游用的渠道密钥。
//
// 与运营侧共用同一套规则（internal/pkg/errbody.MaskSecrets：只替换本次实际使用的凭据原值），
// 于是「凭据出不去」和「除凭据外原文逐字节可见」同时成立——两条都要断言，防止修一边坏另一边。
func TestServeUpstreamError_DownstreamNeverSeesChannelCredential(t *testing.T) {
	seed := time.Now().UnixNano()
	rnd := rand.New(rand.NewSource(seed))
	t.Logf("随机种子 seed=%d（失败可用它复现）", seed)

	// 上游报文里同时出现：明文凭据（两次，含 Bearer 前缀）、随机 request-id、随机模型名、
	// 以及随机正文——遮蔽只能命中凭据那两处。
	reqID := strings.ToUpper(randbody.Word(rnd)) + "-" + randbody.Word(rnd)
	modelName := randbody.Word(rnd) + "-" + randbody.Word(rnd)
	noise := randbody.Text(rnd, 30, 200)

	cases := []struct {
		name   string
		status int
		body   string
	}{
		{
			name: "OpenAI形状-直接返回(401)", status: http.StatusUnauthorized,
			body: randbody.JSONText(map[string]any{
				"error": map[string]any{
					"message": "upstream rejected credential Bearer " + harnessCredential + " for key " + modelName,
					"echo":    "Bearer " + harnessCredential,
					"noise":   noise,
				},
				"request_id": reqID,
			}),
		},
		{
			name: "纯文本-候选全败终结分支(500)", status: http.StatusInternalServerError,
			body: "upstream failed: auth=" + harnessCredential + " rid=" + reqID + " model=" + modelName + " " + noise,
		},
		{
			name: "非JSON对象-429终结分支", status: http.StatusTooManyRequests,
			body: `[{"detail":"quota exhausted for ` + harnessCredential + `","rid":"` + reqID + `"}]`,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer up.Close()

			h := build(t, up.URL)
			const rid = "req-leak"
			rec := doRequestWithID(t, h.gw, "/v1/chat/completions", "sk-gw-t", streamFlagged(false), rid)

			// ① 对外响应不得出现渠道凭据（这是本轮 Gate 的必改项）。
			if strings.Contains(rec.Body.String(), harnessCredential) {
				t.Fatalf("对外响应泄漏渠道凭据: %s", rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "***") {
				t.Fatalf("凭据位置应替换成 ***（而不是整段丢弃内容）: %s", rec.Body.String())
			}
			// ② 对外仍然逐字节透传（只有凭据那一段变成 ***）。
			wantMasked := strings.ReplaceAll(tc.body, harnessCredential, "***")
			var out map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("decode: %v", err)
			}
			msg := errorMessage(t, out)
			if !strings.Contains(wantMasked, msg) && msg != wantMasked {
				t.Fatalf("对外内容必须等于原文（仅凭据遮蔽）\nwant 含=%q\ngot=%q", head(wantMasked, 300), head(msg, 300))
			}
			if !strings.Contains(rec.Body.String(), reqID) && !strings.Contains(msg, reqID) {
				t.Fatalf("非凭据内容（request-id）必须保留: %q", head(msg, 300))
			}

			// ③ 运营侧同样不泄漏，但保留的仍是原文（不是把整段改写掉）。
			if len(h.clogs.inserted) != 1 {
				t.Fatalf("失败必须落 1 条调用日志, got %d", len(h.clogs.inserted))
			}
			cl := h.clogs.inserted[0]
			if strings.Contains(cl.RespBody, harnessCredential) {
				t.Fatalf("call_logs.resp_body 泄漏渠道凭据: %q", head(cl.RespBody, 300))
			}
			if strings.Contains(cl.ErrorMessage, harnessCredential) {
				t.Fatalf("error_message 泄漏渠道凭据: %q", head(cl.ErrorMessage, 300))
			}
			if cl.RespBody != "" {
				assertOperatorSeesExact(t, "调用日志 resp_body", cl.RespBody, tc.body, harnessCredential)
			}
			if !strings.Contains(cl.RespBody, reqID) {
				t.Fatalf("运营侧不得因遮蔽丢掉非凭据内容（request-id）: %q", head(cl.RespBody, 300))
			}
		})
	}
}
