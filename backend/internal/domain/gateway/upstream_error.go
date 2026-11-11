package gateway

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"syscall"

	"github.com/team/llmgateway/internal/pkg/errbody"
)

// 本文件定义「上游（渠道）返回非 2xx」时的**对外错误透传**契约
// （完整口径见 docs/designs/2026-11-11-upstream-error-visibility-design.md）：
//
//  1. HTTP 状态码沿用上游：上游 4xx → 对外 4xx，上游 5xx → 对外 5xx。
//     不再统一改写为 502/503/400（改写会让调用方误判「网关坏了」而放弃按厂商文档自救）。
//  2. 错误内容完整保留，**只按响应体形状分支**（OpenAI 形状 / 其它 JSON / 非 JSON / 空体），
//     绝不按文案、关键词、厂商名、白名单、状态码做特判或映射：
//     - OpenAI 形状（顶层 error 为对象）→ 整段原样透传，只追加我们自己的**顶层**字段
//       （request_id / upstream_status），不删改上游任何字段；
//     - 其它 JSON 形状 → 原始 JSON 文本原样放进 error.message（结构与内容都在，不丢多字段/嵌套）；
//     - 非 JSON（HTML 错误页 / 纯文本）→ 原文原样放进 error.message，绝不替换成中文通用文案；
//     - 空体 → message 给出可读说明（没有内容可透传，但不能给空 message）。
//  3. 超长只按**字节上限**截断（upstreamErrOutBytes），且截断标注与被截原文落在**同一字段**，
//     调用方无需另查元数据即可判断完整性。
//
// 落库/日志仍走清洗后的视图（Clean）：上游可能在报文里回显它收到的凭据，
// 「存储不落明文」是硬红线，与「对外透传原文」并不冲突——对外是还给同一个调用方。

const (
	// upstreamErrReadBytes 读取上游错误响应体的上限。
	// 必须**显著大于**对外上限：否则超长报文在读取层就被静默切掉，对外既拿不到完整内容、
	// 也无法诚实标注「已截断」（截断点会落在读不到的字节上）。
	upstreamErrReadBytes = 64 << 10
	// upstreamErrOutBytes 对外透传上游原文的字节上限（截断标注计入该预算）。
	// 8KB 足以覆盖任何厂商的错误报文（含嵌套 details / HTML 错误页）；
	// 防的是「上游回吐几百 KB 日志/堆栈」把下游客户端打挂。
	upstreamErrOutBytes = 8 << 10
	// upstreamErrCode 透传响应的 error.code：固定标识「本次失败由上游拒绝引起」，
	// 不随状态码/文案变化（上游真实状态码在顶层 upstream_status，无需在此重复编码）。
	upstreamErrCode = "upstream_error"
	// callLogErrBodyMaxBytes 运营侧保存上游错误体原文（call_logs.resp_body）的字节上限。
	// 对外的 8KB 是给客户端的预算，运营侧要能翻更长的原始报文（HTML 错误页 / 堆栈），
	// 但也不能让单行无限膨胀（异步批量写库的内存与页面加载都会被拖垮），故给 32KB。
	// 超限**不静默**：截断标注与被截原文写在同一个字段（见 storeTruncNote）。
	callLogErrBodyMaxBytes = 32 << 10
	// errDetailBodyMaxBytes error_message（账单/调用日志的可读摘要）里保留的原文上限。
	// 与 resp_body 分开：摘要字段是列表页/告警都在读的短字段，不能按 32KB 灌进去；
	// 超限时同样同字段标注，并指向 resp_body 看完整原文。
	errDetailBodyMaxBytes = 4 << 10
)

// upstreamErrBody 一次上游错误响应体的两个视图：
//   - Raw：原样（仅 TrimSpace，≤ upstreamErrReadBytes），供**对外透传**；
//   - Clean：凭据样式清洗后（见 sanitizeErrSummary），供日志短字段与错误分类；
//   - ReadTruncated：读取层已到上限（Raw 只是前缀，原始总字节数不可知）。
//
// 分成多个视图而不是只留一个：对外要「原文原样」，日志短字段要「短且无凭据」，
// 运营落库要「原文原样但绝不含本次凭据」——三处要求互不相同，混用任何一个都会踩另一条红线。
// 运营视图见 operatorView（原文 + 精确遮蔽本次凭据 + 上限标注）。
type upstreamErrBody struct {
	Raw           string
	Clean         string
	ReadTruncated bool
}

// isEmpty 判定是否读到了内容（空体时两个视图都为空）。
func (b upstreamErrBody) isEmpty() bool { return strings.TrimSpace(b.Raw) == "" }

// operatorView 返回**运营可见**的上游原文（落 call_logs.resp_body，账单详情直接展示）：
//
//   - 逐字节保真：不做样式清洗、不看内容白名单、不裁字段、不做文案映射——
//     除「本次实际使用的凭据原值」被替换成 *** 外，其余每个字节（含 <>&、中文、
//     随机 request-id、模型名、URL query）都原样保留；
//   - 只遮蔽已知凭据（精确子串），不猜形状：历史按样式清洗会把 request-id、
//     高熵随机串一并抹掉，运营等于看不到原文；
//   - 超过 callLogErrBodyMaxBytes 时截断，**标注与被截原文同字段**（storeTruncNote）；
//   - 最后过一层 errbody.ToValidText：上游报文可能是任意字节（GBK 错误页、0x00），
//     直接写 PG text 列会让**整行 INSERT 失败**，于是这次失败的账与日志一起消失。
//
// secrets 传本次转发实际使用的凭据（ureq.Cred）。
func (b upstreamErrBody) operatorView(secrets ...string) string {
	if b.isEmpty() {
		return ""
	}
	masked := errbody.MaskSecrets(b.Raw, secrets...)
	if len(masked) <= callLogErrBodyMaxBytes && !b.ReadTruncated {
		return storableText(masked, b.ReadTruncated)
	}
	out, _ := errbody.TruncateUTF8(masked, callLogErrBodyMaxBytes, storeTruncNote(len(masked), b.ReadTruncated))
	return storableText(out, b.ReadTruncated)
}

// storableText 把准备落库的原文整理成 PG text 能接受的合法 UTF-8（非法字节 → U+FFFD、NUL → <0x00>）。
// 真发生过替换时在同字段补一句说明：运营看到 U+FFFD 时不该以为是上游本来就发了这串字符。
func storableText(s string, readTruncated bool) string {
	out, changed := errbody.ToValidText(s)
	if !changed {
		return out
	}
	note := "\n…（原文含非法 UTF-8 字节或 0x00，已替换为 �/<0x00> 以便落库）"
	if readTruncated {
		// 读取层已截断时不再追加，避免两条标注叠在一个字段里把原文挤掉。
		return out
	}
	return out + note
}

// storeTruncNote 运营侧截断标注。数字给的是落库原文的字节数，便于判断丢了多少。
// 读取层就到上限时（ReadTruncated）原始总长度不可知，如实说「超过读取上限」而不是编一个数。
func storeTruncNote(total int, readTruncated bool) string {
	if readTruncated {
		return fmt.Sprintf("\n…（已截断：上游原文超过读取上限 %d 字节，此处只保留前 %d 字节）",
			upstreamErrReadBytes, callLogErrBodyMaxBytes)
	}
	return fmt.Sprintf("\n…（已截断：上游原文共 %d 字节，此处只保留前 %d 字节）", total, callLogErrBodyMaxBytes)
}

// readUpstreamErrBody 读取上游错误响应体（上限 upstreamErrReadBytes；读取失败返回空）。
// 读取失败不阻断错误响应：对外仍有状态码与可读说明，落库仍有状态码字段。
func readUpstreamErrBody(resp *http.Response) upstreamErrBody {
	if resp == nil || resp.Body == nil {
		return upstreamErrBody{}
	}
	// 多读 1 字节用于判定「读取层是否已截断」——否则超长原始报文在读取处被静默切掉，
	// 落库侧既拿不到完整内容、也无法诚实标注截断点。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, upstreamErrReadBytes+1))
	if err != nil {
		return upstreamErrBody{}
	}
	readTruncated := len(raw) > upstreamErrReadBytes
	if readTruncated {
		raw = raw[:upstreamErrReadBytes]
	}
	// Raw **不 TrimSpace**：首尾空白也是上游原文的一部分（截断/裁剪原文正是本任务要杜绝的）。
	// 只有「全是空白」才视作空体（见 isEmpty）。
	s := string(raw)
	return upstreamErrBody{Raw: s, Clean: sanitizeErrSummary(strings.TrimSpace(s)), ReadTruncated: readTruncated}
}

// upstreamTruncNote 截断标注。与截断后的原文写在**同一字段**里（error.message），
// 调用方不必另找元数据就能看出「内容不完整」。数字给的是原始字节数，便于判断丢了多少。
func upstreamTruncNote(total, limit int) string {
	return fmt.Sprintf("\n…（已截断：上游原文共 %d 字节，超过 %d 字节对外上限）", total, limit)
}

// writeUpstreamError 把上游错误响应对外透传。
//
// 两个状态码刻意分开传：
//   - status：**对外**写的 HTTP 状态码（通常等于上游；协议策略显式 DownstreamStatus 时被覆盖；
//     1xx/3xx 按 502 输出）；
//   - upstreamStatus：**上游真实**状态码，写进响应体顶层 upstream_status。
//
// 二者不能混用：策略把 422 覆盖成 418、或上游回 304 被改写成 502 时，
// upstream_status 仍必须报上游真实码，否则字段语义（「上游真实状态码」）就是假的。
func writeUpstreamError(w http.ResponseWriter, status, upstreamStatus int, raw, cred string) {
	// 1xx/3xx 不属错误语义（且 304 禁带响应体），若上游回这类码则按网关错误输出，
	// 否则调用方会拿到「有状态码没内容」或干脆看不到错误。4xx/5xx 一律沿用上游。
	if status < http.StatusBadRequest {
		status = http.StatusBadGateway
	}
	if upstreamStatus <= 0 {
		upstreamStatus = status
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(upstreamErrorPayload(status, upstreamStatus, raw,
		w.Header().Get("x-request-id"), cred))
}

// upstreamErrorPayload 组装对外错误体。返回 any：OpenAI 形状走「整段原样透传」（map），
// 其余形状走网关错误壳（openAIErrorBody）。status=对外状态码，upstreamStatus=上游真实状态码。
//
// secrets：本次转发实际使用的渠道凭据。上游偶尔会把收到的鉴权头原样回显在错误体里，
// 若不遮蔽就等于把**渠道密钥**转手发给下游 API 用户（拥有 token 的租户人人都能拿到）。
// 遮蔽规则与运营侧 operatorView 同一套（internal/pkg/errbody.MaskSecrets：只替换已知凭据原值，
// 不做样式/高熵清洗），因此除凭据外的每个字节仍逐字节透传。
func upstreamErrorPayload(status, upstreamStatus int, raw, requestID string, secrets ...string) any {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return upstreamEnvelope(status, upstreamStatus,
			fmt.Sprintf("上游返回 %d，响应体为空（无可透传的错误内容）", upstreamStatus), requestID)
	}
	// 截断标注里报**原文**字节数（遮蔽前后长度略有差异，不能让调用方看到「对不上账」的数字）。
	origLen := len(raw)
	raw = errbody.MaskSecrets(raw, secrets...)
	// OpenAI 形状且未超上限 → 整段原样透传：顶层每个键（含 error 子对象的全部字段与嵌套）
	// 都以 json.RawMessage 原值搬走，只追加我们自己的顶层字段。
	if len(raw) <= upstreamErrOutBytes {
		if doc, ok := openAIShapeDoc(raw); ok {
			doc["upstream_status"] = json.RawMessage(strconv.Itoa(upstreamStatus))
			if requestID != "" {
				if _, exists := doc["request_id"]; !exists {
					doc["request_id"] = json.RawMessage(strconv.Quote(requestID))
				}
			}
			return doc
		}
	}
	// 其余形状（其它 JSON / 非 JSON / 超长的 OpenAI 形状）：原文原样进 message，
	// 超限时同字段前缀 + 截断标注。其它 JSON 也是原文本，结构与嵌套一并保留。
	text, _ := truncateUTF8(raw, upstreamErrOutBytes, upstreamTruncNote(origLen, upstreamErrOutBytes))
	return upstreamEnvelope(status, upstreamStatus, text, requestID)
}

// upstreamErrorEnvelope 非 OpenAI 形状时用的网关错误壳（message 里是原文，不做任何改写）。
// upstream_status 放**顶层**：与 OpenAI 形状的整段透传保持同一位置（那种形状不允许改 error 子对象），
// 调用方在两种形状下都用同一个路径取到上游状态码。
type upstreamErrorEnvelope struct {
	Error          openAIErrorDetail `json:"error"`
	UpstreamStatus int               `json:"upstream_status"`
	RequestID      string            `json:"request_id,omitempty"`
}

// upstreamEnvelope 组装网关错误壳（不含任何内容改写：message 由调用方给出原文）。
// error.type 按**对外**状态码归类（客户端按它判错误大类），upstream_status 记上游真实码。
func upstreamEnvelope(status, upstreamStatus int, message, requestID string) upstreamErrorEnvelope {
	return upstreamErrorEnvelope{
		Error: openAIErrorDetail{
			Message: message,
			Type:    openAIErrorType(status),
			Code:    upstreamErrCode,
		},
		UpstreamStatus: upstreamStatus,
		RequestID:      requestID,
	}
}

// openAIShapeDoc 判定是否 OpenAI 错误形状（顶层为对象且 error 键的值是对象），
// 是则返回顶层键值（保留每个值的原始字节）。
// 只认形状、不看内容：error.message 里写什么、error 里还有哪些键，一律不参与判定。
func openAIShapeDoc(raw string) (map[string]json.RawMessage, bool) {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || doc == nil {
		return nil, false
	}
	e, ok := doc["error"]
	if !ok || !isJSONObject(e) {
		return nil, false
	}
	return doc, true
}

// isJSONObject 判定一段 JSON 值是否为对象（只看首个非空白字节）。
func isJSONObject(raw json.RawMessage) bool {
	s := bytes.TrimSpace(raw)
	return len(s) > 0 && s[0] == '{'
}

// upstreamTransportNote 把传输层失败（连接被拒/断连/超时）转成**可读原因**，
// 供「所有候选都失败」的终结响应与已送达超时的收尾使用：调用方至少要能看出
// 「连不上」还是「等超时」，而不是一句「服务暂时不可用」。
//
// **硬约束：绝不把渠道地址带出去**。`net/http` 的错误文本形如
// `Post "http://internal-gw.local:18080/chat/completions": dial tcp 10.0.3.7:18080: connect: connection refused`，
// 原样放进对外 message 等于把内网拓扑交给调用方。所以这里只输出「哪一类失败」+
// 不含地址的底层线索（errno 名 / 脱敏后的兜底文本）。
//
// 分类依据是 Go 标准库的错误**类型与 errno**（不是上游响应体内容），因此不属于「按上游文案特判」。
// timedOut 用于选择对外状态码（超时 → 504，其它 → 502）。
func upstreamTransportNote(kind string, err error) (note string, timedOut bool) {
	if err == nil {
		return "上游" + kind + "失败（无错误详情）", false
	}
	reason, timedOut := transportFailureReason(err)
	return "上游" + kind + reason, timedOut
}

// operatorErrDetail 拼「运营侧」的错误原因（落 call_logs.error_message / billing_records.error_message）：
// 保留**完整错误文本**（含渠道地址、主机、端口），因为运营要据此判断是哪台机器/哪个域名出问题；
// 只做凭据样式清洗与预算截断。与对外文案（buildFailureNote / upstreamTransportNote，一律打码）分开口径。
func operatorErrDetail(prefix string, err error) string {
	if err == nil {
		return prefix
	}
	return prefix + "：" + truncateLog(sanitizeErrSummary(err.Error()), errDetailBodyMaxBytes)
}

// buildFailureNote 把「构建上游请求失败」转成对外可读原因。
// 构建失败的错误文本里同样可能带出渠道地址（如 `parse "http://<基址>/chat/completions": invalid character…`），
// 所以对外文案统一过 redactUpstreamAddr；完整错误仍进结构化日志与调用日志。
func buildFailureNote(err error) string {
	if err == nil {
		return "上游请求构建失败（无错误详情）"
	}
	return "上游请求构建失败：" + redactUpstreamAddr(truncateLog(sanitizeErrSummary(err.Error()), errSummaryMaxBytes))
}

// transportFailureReason 把一次传输层失败归一成「可读且不含地址」的原因。
func transportFailureReason(err error) (reason string, timedOut bool) {
	if isTimeoutErr(err) {
		return "超时（等不到上游响应）", true
	}
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "失败：连接被拒绝（connection refused）", false
	case errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
		return "失败：连接被上游重置（connection reset）", false
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "失败：连接被上游中断（对端关闭连接）", false
	case errors.Is(err, syscall.EHOSTUNREACH), errors.Is(err, syscall.ENETUNREACH):
		return "失败：网络不可达（host/network unreachable）", false
	case isDNSErr(err):
		return "失败：域名解析失败（DNS）", false
	case isTLSErr(err):
		return "失败：TLS 握手失败", false
	}
	// 兜底：保留底层错误文本（URL / host:port / IP 一律抹掉），避免退回「服务暂时不可用」这类通用文案。
	detail := redactUpstreamAddr(truncateLog(sanitizeErrSummary(err.Error()), errSummaryMaxBytes))
	if detail == "" {
		return "失败（无错误详情）", false
	}
	return "失败：" + detail, false
}

// isDNSErr 判定是否 DNS 解析失败。
func isDNSErr(err error) bool {
	var d *net.DNSError
	return errors.As(err, &d)
}

// isTLSErr 判定是否 TLS 握手/证书类失败（按错误类型判定，不看文案）。
func isTLSErr(err error) bool {
	var (
		verr   *tls.CertificateVerificationError
		recErr tls.RecordHeaderError
		unkErr x509.UnknownAuthorityError
		hostE  x509.HostnameError
	)
	return errors.As(err, &verr) || errors.As(err, &recErr) || errors.As(err, &unkErr) || errors.As(err, &hostE)
}

// 地址类样式的兜底抹除规则：URL / IPv4(:port) / [IPv6](:port) / 带端口的主机名。
// 只用于「未能归类」的兜底错误文本，正常路径压根不会走到这里（分类命中时不输出底层原文）。
var (
	urlLikeRe  = regexp.MustCompile(`(?i)[a-z][a-z0-9+.\-]*://[^\s"')]+`)
	ipv4Re     = regexp.MustCompile(`\b\d{1,3}(?:\.\d{1,3}){3}(?::\d{1,5})?\b`)
	ipv6Re     = regexp.MustCompile(`\[[0-9a-fA-F:]+\](?::\d{1,5})?`)
	hostPortRe = regexp.MustCompile(`(?i)\b(?:localhost|(?:[a-z0-9](?:[a-z0-9\-]*[a-z0-9])?\.)+[a-z]{2,}):\d{1,5}\b`)
	// anyHostPortRe 兜住前面几条没料到的主机:端口写法（如 URL 解析失败时的 `bad host:18080/chat/completions`，
	// 主机名里带空格/下划线等非法字符，正则域名规则匹配不到）。要求「非空白串 + 冒号 + 端口」，
	// 因此不会误伤正文里的「错误码 400」这类文本。
	anyHostPortRe = regexp.MustCompile(`(?i)[^\s"'(\[{]+:\d{1,5}(?:/[^\s"')]*)?`)
)

// redactUpstreamAddr 抹掉文本里的渠道地址（URL / IP / 带端口主机名），保留其余可读线索。
// 这是「对外文案不出地址」的第二道闸：即使后续新增了未归类的传输错误，也不会把地址漏出去。
func redactUpstreamAddr(s string) string {
	if s == "" {
		return s
	}
	s = urlLikeRe.ReplaceAllString(s, "<地址>")
	s = ipv6Re.ReplaceAllString(s, "<地址>")
	s = ipv4Re.ReplaceAllString(s, "<地址>")
	s = hostPortRe.ReplaceAllString(s, "<地址>")
	s = anyHostPortRe.ReplaceAllString(s, "<地址>")
	return s
}

// isTimeoutErr 判定错误是否属于超时（含 context deadline）。
func isTimeoutErr(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err)
}

// terminalFailure 为「候选全部失败」的终结响应决定状态码与文案：
//   - 有上游报文（lastUpstreamStatus>0）→ 状态码沿用上游、报文一并透出（见 writeUpstreamError）；
//   - 没有报文但有传输层失败（连接被拒/断连/超时）→ 502；超时 → 504，文案点名原因；
//   - 连传输层失败都没有（候选全被本地拦截：会话满/限流/凭据缺失/协议不支持）→ 503
//     「无可用渠道」（这是网关侧没有可用容量，不是上游拒绝，也不是上游不可达）。
func terminalFailure(lastUpstreamStatus int, failureNote string, timedOut bool, fallback string) (int, string) {
	if lastUpstreamStatus > 0 {
		return lastUpstreamStatus, ""
	}
	if failureNote == "" {
		return http.StatusServiceUnavailable, fallback
	}
	status := http.StatusBadGateway
	if timedOut {
		status = http.StatusGatewayTimeout
	}
	return status, "所有候选渠道均失败：" + failureNote
}
