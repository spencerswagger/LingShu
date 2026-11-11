package gateway

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"syscall"
	"unicode"

	"github.com/team/llmgateway/internal/pkg/errbody"
)

// 本文件定义「上游（渠道）返回非 2xx」时的对外错误处理，以及**运营侧**的上游原文可见性。
//
// 对外契约（与既有网关语义一致，不做任何结构性变更）：
//   - HTTP 状态码走**网关语义**：未转移的 4xx → 502（默认）；可转移失败耗尽 → 503；
//     全部候选被上游 429 限流 → 429；已送达超时 → 504；协议策略显式 DownstreamStatus 时按策略值。
//     上游真实码**只**落在运营侧（call_logs.upstream_status）与 error.message 的自由文本里。
//   - 响应体只有 main 的既有形状：{"error":{"message","type","code"},"request_id"}。
//     type/code 由对外状态码按 OpenAI 语义归类（见 openAIErrorType / openAIErrorCode）。
//   - 上游原文（清洗后的可读文本）**只**出现在 error.message 这个自由文本字段里：
//     客户端不应按 message 分支，因此不构成契约变更；内容是原文按 UTF-8 边界截断到 8KB。
//   - 对外仍只遮蔽**本次实际使用的凭据**（与运营侧同一函数 errbody.MaskSecrets），
//     其余内容（含第三方 token、request-id、模型名）原样保留。
//
// 运营侧（账单/探活）见 operatorView：原文逐字节，只遮蔽本次凭据，32KB 上限 + 同字段标注。

const (
	// upstreamErrReadBytes 读取上游错误响应体的上限。
	// 必须**显著大于**对外上限：否则超长报文在读取层就被静默切掉，对外既拿不到完整内容、
	// 也无法诚实标注「已截断」（截断点会落在读不到的字节上）。
	upstreamErrReadBytes = 64 << 10
	// upstreamMsgMaxBytes error.message 里保留的上游原文字节上限（截断标注计入该预算）。
	// 8KB 足以覆盖任何厂商的错误报文（含 HTML 错误页）；防的是「上游回吐几百 KB 日志/堆栈」
	// 把下游客户端打挂。message 是自由文本，客户端不应据此分支，故不影响契约。
	upstreamMsgMaxBytes = 8 << 10
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

// upstreamErrBody 一次上游错误响应体的多个视图：
//   - Raw：原样（不做 TrimSpace，≤ upstreamErrReadBytes），供**对外透传**；
//   - Clean：**日志短字段 / 错误分类专用，禁止用于任何面向人的展示**（见其注释）；
//   - ReadTruncated：读取层已到上限（Raw 只是前缀，原始总字节数不可知）；
//   - ReadInterrupted：读取中断（连接断了），Raw 是已读到的前缀。
//
// 分成多个视图而不是只留一个：对外要「原文原样」，日志短字段要「短且无凭据」，
// 运营落库要「原文原样但绝不含本次凭据」——三处要求互不相同，混用任何一个都会踩另一条红线。
// 运营视图见 operatorView（原文 + 精确遮蔽本次凭据 + 上限标注）。
type upstreamErrBody struct {
	Raw string
	// Clean 是**按样式清洗**后的短文本（前缀/Bearer/KV/高熵长串 → ***），只给结构化日志的
	// err_param/err_type/err_code/err_summary 与错误分类用。
	//
	// 禁止用于任何面向人的展示（账单、探活、对外响应）：它会把 request-id、模型名、
	// 上游自造 id 一并抹掉，展示出来就不是原文了——展示一律走 operatorView。
	Clean           string
	ReadTruncated   bool
	ReadInterrupted bool
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
	// 顺序很关键：先遮蔽、再把任意字节整理成合法 UTF-8，**然后**才做长度判定与截断。
	// 反过来（先判定再整理）会让非法字节在落库时膨胀 3 倍（每个 GBK 字节 → 3 字节 U+FFFD），
	// 实测 32000 B 的 GBK 报文落地约 96KB，「32KB 上限」名不副实。
	text, replaced := errbody.ToValidText(errbody.MaskSecrets(b.Raw, secrets...))
	return capOperatorText(text, replaced, len(b.Raw), b.ReadTruncated, b.ReadInterrupted)
}

// capOperatorText 把（已遮蔽、已整理成合法 UTF-8 的）运营侧文本按上限收口，并把各种「不完整」
// 的原因写在**同一个字段**里：截断 / 读取上限 / 读取中断 / 非法字节替换。
//   - origLen 是**遮蔽前**的上游原文长度：标注里的「共 N 字节」与对外的口径一致，
//     不会因为遮蔽后少了几字节而对不上账；
//   - suffix 计入字节预算，所以结果永远不会超过 callLogErrBodyMaxBytes。
func capOperatorText(text string, replaced bool, origLen int, readTruncated, readInterrupted bool) string {
	var suffix string
	if readTruncated || len(text) > callLogErrBodyMaxBytes {
		suffix += storeTruncNote(origLen, readTruncated, replaced)
	}
	if readInterrupted {
		suffix += fmt.Sprintf("\n…（读取中断：上游响应体只读到前 %d 字节）", len(text))
	}
	if replaced {
		suffix += illegalByteNote
	}
	if suffix == "" {
		return text
	}
	// 放得下就直接拼（TruncateUTF8 只在真的超限时才会拼 suffix，未超限时原样返回——标注会丢）。
	if len(text)+len(suffix) <= callLogErrBodyMaxBytes {
		return text + suffix
	}
	out, _ := errbody.TruncateUTF8(text, callLogErrBodyMaxBytes, suffix)
	return out
}

// illegalByteNote 非法字节替换的标注。运营看到 U+FFFD 时不该以为是上游本来就发了这串字符。
const illegalByteNote = "\n…（原文含非法 UTF-8 字节或 0x00，已替换为 �/<0x00> 以便落库）"

// storeTruncNote 运营侧截断标注。数字给的是**遮蔽前**的上游原文字节数（与对外同口径），
// 便于判断丢了多少。
// 读取层就到上限时（ReadTruncated）原始总长度不可知，如实说「超过读取上限」而不是编一个数。
func storeTruncNote(total int, readTruncated, replaced bool) string {
	if readTruncated {
		return fmt.Sprintf("\n…（已截断：上游原文超过读取上限 %d 字节，此处只保留前 %d 字节）",
			upstreamErrReadBytes, callLogErrBodyMaxBytes)
	}
	if replaced {
		// 非法字节被替换成 3 字节的 U+FFFD 后会「膨胀」，此时的 32KB 是替换后文本的上限：
		// 说清楚，否则「共 N 字节 / 只保留前 32768」两个数字看着对不上（原文可能只显示了三分之一）。
		return fmt.Sprintf("\n…（已截断：上游原文共 %d 字节，其中非法字节替换为 � 后超出 %d 字节上限，"+
			"此处只保留替换后文本的前 %d 字节）", total, callLogErrBodyMaxBytes, callLogErrBodyMaxBytes)
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
	// io.ReadAll 在出错时也会返回已读到的前缀：那部分内容对定位问题仍然有价值，
	// 不能因为「没读完」就整段丢弃（丢弃=运营看到「响应体为空」，正好掩盖上游到底回了什么）。
	raw, err := io.ReadAll(io.LimitReader(resp.Body, upstreamErrReadBytes+1))
	interrupted := err != nil
	if interrupted && len(raw) == 0 {
		return upstreamErrBody{ReadInterrupted: true}
	}
	readTruncated := len(raw) > upstreamErrReadBytes
	if readTruncated {
		raw = raw[:upstreamErrReadBytes]
	}
	// Raw **不 TrimSpace**：首尾空白也是上游原文的一部分（截断/裁剪原文正是本任务要杜绝的）。
	// 只有「全是空白」才视作空体（见 isEmpty）。
	s := string(raw)
	return upstreamErrBody{
		Raw: s, Clean: sanitizeErrSummary(strings.TrimSpace(s)),
		ReadTruncated: readTruncated, ReadInterrupted: interrupted,
	}
}

// upstreamTruncNote 截断标注。与截断后的原文写在**同一字段**（error.message）里，
// 调用方不必另找元数据就能看出「内容不完整」。数字给的是**遮蔽前**的原始字节数。
func upstreamTruncNote(total, limit int) string {
	return fmt.Sprintf("\n…（已截断：上游原文共 %d 字节，超过 %d 字节上限）", total, limit)
}

// upstreamErrMessage 组装对外 error.message：可读中文文案 + 「：」+ 上游原文（遮蔽本次凭据、按 8KB 截断）。
//
// 为什么原文只放 message：
//   - 对外契约（状态码、error.type/code、字段集合）与既有网关语义保持一致，不做结构性变更；
//   - message 是自由文本，客户端不应据此做分支判断，所以「把上游到底说了什么」放这里
//     既能让调用方自救，又不改变任何可以被程序依赖的语义。
//
// secrets 传本次转发实际使用的渠道凭据：上游偶尔会回显鉴权头，原样输出等于把**渠道密钥**
// 交给任意持有自己令牌的租户；遮蔽规则与运营侧 operatorView 同一套（只替换已知凭据原值），
// 因此除凭据外的每个字节仍逐字节保留。
func upstreamErrMessage(base, raw, cred string) string {
	if strings.TrimSpace(raw) == "" {
		return base
	}
	origLen := len(raw)
	masked := errbody.MaskSecrets(raw, cred)
	text, _ := truncateUTF8(masked, upstreamMsgMaxBytes, upstreamTruncNote(origLen, upstreamMsgMaxBytes))
	return base + "：" + text
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
	// bareHostRe 裸主机名（**无端口**）：DNS 解析失败的错误文本形如
	// `dial tcp: lookup internal-gw.local: no such host`——没有 scheme、没有端口，前面几条都匹配不到。
	// 末尾要求「点 + ≥2 个字母」的 TLD 样式，避免误伤 `3.14`、`v1.23`、`Go 1.21` 这类文本。
	bareHostRe = regexp.MustCompile(`(?i)\b(?:[a-z0-9](?:[a-z0-9\-]*[a-z0-9])?\.)+[a-z]{2,}\b`)
	// nonASCIIHostRe 切出候选 token（含非 ASCII 的域名无法用 ASCII 域名规则表达），
	// 再由 maskNonASCIIHost 判断「含点 + 含非 ASCII 字符」才替换。
	nonASCIIHostRe = regexp.MustCompile(`[^\s"'()\[\]{}<>，。；：！？、]+`)
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
	s = bareHostRe.ReplaceAllString(s, "<地址>")
	s = maskNonASCIIHost(s)
	return s
}

// maskNonASCIIHost 抹掉「含点且含非 ASCII 字符」的 token（如 `内部网关.公司`、`内部网关.公司:18080`）。
// 纯 ASCII token 一律原样返回（ASCII 场景已由前面的规则处理，避免重复替换把 `<地址>` 再切碎）。
func maskNonASCIIHost(s string) string {
	if s == "" {
		return s
	}
	return nonASCIIHostRe.ReplaceAllStringFunc(s, func(tok string) string {
		if !strings.Contains(tok, ".") {
			return tok
		}
		for _, r := range tok {
			if r > unicode.MaxASCII {
				return "<地址>"
			}
		}
		return tok
	})
}

// isTimeoutErr 判定错误是否属于超时（含 context deadline）。
func isTimeoutErr(err error) bool {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return true
	}
	return errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err)
}

// terminalFailure 为「候选全部失败」给出对外状态码与可读原因串。
//
// 对外状态码走**网关语义**、与既有行为一致：一律 503「无可用渠道」——上游真实码只落运营侧
// （call_logs.upstream_status）与 message 的自由文本，不参与对外状态码判定。
//   - 有上游报文（可转移的 4xx/5xx 耗尽）→ 503，原文进 message；
//   - 连接失败/构建失败/超时/全部被本地拦截 → 503，原因串进 message。
func terminalFailure(lastUpstreamStatus int, failureNote string, timedOut bool, fallback string) (int, string) {
	_ = lastUpstreamStatus
	_ = timedOut
	_ = failureNote
	return http.StatusServiceUnavailable, fallback
}
