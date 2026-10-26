package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/team/llmgateway/internal/domain/channelprotocol"
)

// 本文件承载「入站请求参数快照」（call_logs.req_params）：
//   - 快照在**组包处**一次性构造（见 handler.go 中 ureq 构造点），随请求 context 下传，
//     由唯一的写入漏斗 writeCallLogUsage 落库——避免在多个 handler 分支里各写一份、字段口径漂移；
//   - 快照不含 messages（那是 req_messages 的职责），也不含任何凭据；
//   - 序列化后硬上限 8KB，超限按字段优先级裁剪并标记（绝不整体丢弃）。

const (
	// reqParamsMaxBytes 单条请求参数快照的落库上限（8KB）。
	// 取 8KB 的理由：req_params 是「诊断快照」而非全量请求体拷贝——正常请求（含工具摘要）远小于此，
	// 只有塞了超大 response_format/stop/logit_bias 之类才会触顶；这类字段对定位价值低，裁掉即可。
	reqParamsMaxBytes = 8 * 1024
	// reqParamsFieldMaxBytes 单个参数的体积上限（2KB）：超限字段不整体丢弃，而是把值替换成
	// "omitted:NB" 标记并把字段名记入 summarized——保证「至少含哪些键」的契约不因体积而破坏。
	reqParamsFieldMaxBytes = 2 * 1024
	// jsonbContainerOverhead jsonb 二进制相对 JSON 文本的固定余量（容器头/对齐/实现细节）。
	jsonbContainerOverhead = 256
	// coreStringMax 核心字段（上游 URL/端点/模型名）的长度上限，异常长时截断并记名。
	coreStringMax = 1024
	// toolsNamesCap tools 摘要里保留的工具名条数上限（只留名字，不留 schema）。
	toolsNamesCap = 20
)

// reqParamsProtected 受保护参数：预算再紧也优先保留（它们是定位「上游为何拒绝」的关键）。
// 与任务约定的「至少含 model/temperature/max_tokens/top_p/tools 摘要/stream/response_format/
// 端点/上游协议/上游 URL/token_id」一一对应：model/stream/端点/上游协议/上游 URL/token_id 属核心字段，
// tools 恒为摘要，其余在本集合内。
var reqParamsProtected = []string{
	"temperature", "max_tokens", "max_completion_tokens", "top_p", "response_format",
	"n", "seed", "stop", "tool_choice", "reasoning_effort", "thinking", "enable_thinking",
}

// reqParamsInput 构造快照时的候选级输入：这些字段是「受保护核心」（永不裁剪）。
type reqParamsInput struct {
	Endpoint         string // 入站端点，如 /v1/chat/completions
	InboundProtocol  string // 入站基础协议
	UpstreamProtocol string // 实际上游协议（直通时与入站相同）
	UpstreamURL      string // 实际上游 URL（写入前脱敏：去 userinfo/query）
	Converted        bool   // 是否发生跨协议转换
	TokenID          int64  // 消费者令牌维度
	Model            string // 对外模型名（Auto 命中后为成员名）
	AutoModelName    string // 请求的 Auto 名（非 Auto 请求为空）
	Stream           bool   // 是否流式
}

// toolsSummary tools 摘要：只留数量与工具名，绝不落 schema（schema 是体积大头且对定位无价值）。
type toolsSummary struct {
	Count int      `json:"count"`
	Names []string `json:"names,omitempty"`
}

// reqParamsSnapshot call_logs.req_params 的落库形状（字段见 0018 迁移注释）。
type reqParamsSnapshot struct {
	Endpoint         string                     `json:"endpoint,omitempty"`
	InboundProtocol  string                     `json:"inbound_protocol,omitempty"`
	UpstreamProtocol string                     `json:"upstream_protocol,omitempty"`
	UpstreamURL      string                     `json:"upstream_url,omitempty"`
	Converted        bool                       `json:"converted,omitempty"`
	TokenID          int64                      `json:"token_id,omitempty"`
	Model            string                     `json:"model,omitempty"`
	AutoModelName    string                     `json:"auto_model_name,omitempty"`
	Stream           bool                       `json:"stream"`
	MessageCount     int                        `json:"message_count,omitempty"`
	Tools            *toolsSummary              `json:"tools,omitempty"`
	Params           map[string]json.RawMessage `json:"params,omitempty"`
	Summarized       []string                   `json:"summarized,omitempty"`
	Dropped          []string                   `json:"dropped,omitempty"`
	OverBudget       bool                       `json:"over_budget,omitempty"`
}

// reqParamsSource 入站请求体的**一次性**解析结果，供每个候选复用（避免逐候选重复解析）。
type reqParamsSource struct {
	top       map[string]json.RawMessage // 入站顶层字段（已剔除 messages/tools/model/stream）
	tools     *toolsSummary
	msgCount  int
	dropOrder []string // 超预算时的裁剪顺序（已按优先级排好）
}

// newReqParamsSource 解析入站请求体，抽出「消息计数 / tools 摘要 / 其余顶层参数」。
// 不修改入参；解析失败不报错（快照降级为核心字段），因为快照绝不能影响转发主链路。
func newReqParamsSource(body []byte) *reqParamsSource {
	src := &reqParamsSource{}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		src.dropOrder = nil
		return src // top 为 nil：buildReqParams 只落核心字段
	}
	src.top = make(map[string]json.RawMessage, len(top))
	for k, v := range top {
		switch k {
		case "model", "stream":
			// 已提升到核心字段（见 reqParamsInput），不再重复落 params。
			continue
		case "messages":
			var msgs []json.RawMessage
			if err := json.Unmarshal(v, &msgs); err == nil {
				src.msgCount = len(msgs)
			}
			continue
		case "input":
			// OpenAI Responses 入站的对话体：既计入消息数，也不进 params（体积大）。
			if n, ok := countResponsesInput(v); ok {
				src.msgCount = n
			}
			continue
		case "tools", "functions":
			src.tools = summarizeTools(v)
			continue
		}
		src.top[k] = v
	}
	src.dropOrder = reqParamDropOrder(src.top)
	return src
}

// countResponsesInput 统计 Responses 入站 input 的条目数（数组按条、字符串按 1 条）。
func countResponsesInput(raw json.RawMessage) (int, bool) {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err == nil {
		return len(arr), true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return 1, true
	}
	return 0, false
}

// summarizeTools 生成 tools 摘要：数量 + 前 toolsNamesCap 个工具名（function.name / 顶层 name）。
func summarizeTools(raw json.RawMessage) *toolsSummary {
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return nil
	}
	out := &toolsSummary{Count: len(arr)}
	for _, item := range arr {
		if len(out.Names) >= toolsNamesCap {
			break
		}
		var t struct {
			Name     string `json:"name"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		}
		if err := json.Unmarshal(item, &t); err != nil {
			continue
		}
		name := strings.TrimSpace(t.Function.Name)
		if name == "" {
			name = strings.TrimSpace(t.Name)
		}
		if name != "" {
			out.Names = append(out.Names, name)
		}
	}
	return out
}

// reqParamDropOrder 给出「非受保护字段」的裁剪顺序：
// 先固定裁「体积大、定位价值低」的常客，其余按体积降序；受保护字段不在此列表内。
func reqParamDropOrder(top map[string]json.RawMessage) []string {
	protect := protectedParamSet()
	fixed := []string{
		"metadata", "logit_bias", "stream_options", "text", "instructions", "functions",
		"extra_body", "parallel_tool_calls",
	}
	order := make([]string, 0, len(top))
	seen := make(map[string]bool, len(top))
	for _, k := range fixed {
		if _, ok := top[k]; ok && !protect[k] {
			order = append(order, k)
			seen[k] = true
		}
	}
	rest := make([]string, 0, len(top))
	for k := range top {
		if protect[k] || seen[k] {
			continue
		}
		rest = append(rest, k)
	}
	// 体积降序（同体积按名字升序，保证裁剪结果确定可复现）。
	sort.Slice(rest, func(i, j int) bool {
		li, lj := len(top[rest[i]]), len(top[rest[j]])
		if li != lj {
			return li > lj
		}
		return rest[i] < rest[j]
	})
	return append(order, rest...)
}

// protectedParamSet 受保护参数集合（见 reqParamsProtected）。
func protectedParamSet() map[string]bool {
	out := make(map[string]bool, len(reqParamsProtected))
	for _, k := range reqParamsProtected {
		out[k] = true
	}
	return out
}

// buildReqParams 构造快照 JSON，保证落库后 `pg_column_size(req_params) ≤ 8192`。
//
// 预算口径：按 **jsonb 二进制**估算，而不是只看 JSON 文本长度（PR #63 验收校准项：
// 文本压线时 jsonb 会因每条 KV 多两个 JEntry 而略超）。见 jsonbSizeBound。
//
// 装箱顺序（先紧着定位时要看的字段）：
//  1. 核心字段（端点/协议/上游 URL/token/model/stream/消息数/tools 摘要）；
//  2. 受保护参数（超单字段上限的换成 "omitted:NB" 标记，键保留、名字进 summarized）；
//  3. 其余参数按体积升序装，装不下就丢并**记名**；
//  4. 若仍超（时间/数量极端）：按 dropOrder 继续丢非受保护参数，再按保护优先级倒序丢受保护参数，
//     **每丢一个都写进 dropped**——不再有「清空 params 但不记名」的静默路径。
//
// 可对账不变量：`len(params 里保留的键) + len(dropped) == 输入顶层参数个数`（summarized 的键仍在 params 里）。
func (s *reqParamsSource) buildReqParams(in reqParamsInput) []byte {
	if s == nil {
		return nil
	}
	snap := reqParamsSnapshot{
		Endpoint:         in.Endpoint,
		InboundProtocol:  in.InboundProtocol,
		UpstreamProtocol: in.UpstreamProtocol,
		UpstreamURL:      sanitizeUpstreamURL(in.UpstreamURL),
		Converted:        in.Converted,
		TokenID:          in.TokenID,
		Model:            in.Model,
		AutoModelName:    in.AutoModelName,
		Stream:           in.Stream,
		MessageCount:     s.msgCount,
		Tools:            s.tools,
	}
	if s.tools != nil {
		snap.Summarized = append(snap.Summarized, "tools")
	}
	// 核心字段也可能异常长（如超长上游 URL/模型名）：先截断并记名，保证预算一定可解。
	snap.Endpoint = capCoreString(snap.Endpoint, coreStringMax, "endpoint", &snap)
	snap.UpstreamURL = capCoreString(snap.UpstreamURL, coreStringMax, "upstream_url", &snap)
	snap.Model = capCoreString(snap.Model, coreStringMax, "model", &snap)
	snap.AutoModelName = capCoreString(snap.AutoModelName, coreStringMax, "auto_model_name", &snap)

	snap.Params = make(map[string]json.RawMessage, len(s.top))
	dropped := make(map[string]bool, len(s.top))
	protect := protectedParamSet()

	// 2) 受保护参数先放（值超上限换标记）。
	for _, k := range reqParamsProtected {
		raw, ok := s.top[k]
		if !ok {
			continue
		}
		snap.Params[k] = capParamValue(k, raw, &snap)
	}
	// 3) 其余参数：体积升序装，装不下就丢并记名。
	rest := make([]string, 0, len(s.top))
	for k := range s.top {
		if protect[k] {
			continue
		}
		rest = append(rest, k)
	}
	sort.Slice(rest, func(i, j int) bool {
		li, lj := len(s.top[rest[i]]), len(s.top[rest[j]])
		if li != lj {
			return li < lj
		}
		return rest[i] < rest[j]
	})
	for _, k := range rest {
		snap.Params[k] = capParamValue(k, s.top[k], &snap)
		if !overBudget(snap) {
			continue
		}
		dropParam(&snap, dropped, k)
	}
	// 4) 仍超预算：按 dropOrder（固定优先项 → 体积降序）继续丢非受保护参数，逐个记名。
	for _, k := range s.dropOrder {
		if !overBudget(snap) {
			break
		}
		if _, ok := snap.Params[k]; !ok {
			continue
		}
		dropParam(&snap, dropped, k)
	}
	// 5) 最后一层：受保护参数按「保护优先级倒序」丢（先丢 seed/n/stop/thinking 一类，最后才动
	//    temperature/max_tokens/top_p/response_format），同样逐个记名 + 置 over_budget。
	if overBudget(snap) {
		snap.OverBudget = true
		for i := len(reqParamsProtected) - 1; i >= 0; i-- {
			if !overBudget(snap) {
				break
			}
			k := reqParamsProtected[i]
			if _, ok := snap.Params[k]; !ok {
				continue
			}
			dropParam(&snap, dropped, k)
		}
	}
	out, err := json.Marshal(snap)
	if err != nil {
		return nil
	}
	return out
}

// dropParam 删除一个参数并**立即**记名到 snap.Dropped。
// 立即写（而不是先记 set 最后再补）是为了让预算判定把「被丢字段名」的字节也算进去——
// 否则丢得越多、名字越多，最后一步补齐名字时体积会二次冲高（这正是旧实现压线超限的成因之一）。
func dropParam(snap *reqParamsSnapshot, dropped map[string]bool, key string) {
	if dropped[key] {
		return
	}
	delete(snap.Params, key)
	dropped[key] = true
	snap.Dropped = append(snap.Dropped, key)
}

// jsonbSizeBound 估算 `pg_column_size(<此 JSONB 值>)` 的上界。
//
// 依据：jsonb 落盘 = varlena 头(4) + 根容器头(4) + 每个 KV 两个 JEntry(共 8B) + 键值字节；
// JSON 文本里每条 KV 的标点（引号/冒号/逗号）约 3B，故净增 ≈ 5B/KV；嵌套的 params 容器再 +8B 头，
// 另留 jsonbContainerOverhead 余量给实现细节（大小端/对齐/容器扩容）。
// 另外 JSON 文本里的 `<>&` 会被转义成 6 字节而 jsonb 里只存 1 字节 → 文本长度本身已偏保守。
func jsonbSizeBound(jsonText []byte, kvCount int) int {
	return len(jsonText) + 8 + 5*kvCount + jsonbContainerOverhead
}

// overBudget 判断当前快照是否已超「落库后 pg_column_size ≤ reqParamsMaxBytes」的预算。
func overBudget(snap reqParamsSnapshot) bool {
	b, err := json.Marshal(snap)
	if err != nil {
		return false
	}
	return jsonbSizeBound(b, coreKVCount+len(snap.Params)+len(snap.Summarized)+len(snap.Dropped)) > reqParamsMaxBytes
}

// coreKVCount 核心字段的 KV 个数上界（endpoint/inbound/upstream/url/converted/token/model/
// auto_model/stream/message_count/tools/params/summarized/dropped/over_budget）——取值偏大即偏安全。
const coreKVCount = 16

// capCoreString 核心字段的长度截断（异常长的 URL/模型名不允许撑爆预算），被截断时记名。
// 截断走 truncateUTF8（按字符边界），避免中文模型名/长 URL 被切成半个字符后
// 被 JSON 编码成 U+FFFD（快照「变花」）。
func capCoreString(v string, max int, name string, snap *reqParamsSnapshot) string {
	out, truncated := truncateUTF8(v, max, "...")
	if !truncated {
		return v
	}
	if snap != nil {
		snap.Summarized = append(snap.Summarized, name)
	}
	return out
}

// capParamValue 单字段体积守卫：超过 reqParamsFieldMaxBytes 时把值替换成标记字符串，
// 字段名记入 summarized（键保留，值可读为「被省略的体积」）。
func capParamValue(key string, raw json.RawMessage, snap *reqParamsSnapshot) json.RawMessage {
	if len(raw) <= reqParamsFieldMaxBytes {
		return raw
	}
	// 标记不用尖括号：JSON 编码会把 < > 转义成 \u003c，落库后不便阅读。
	marker, err := json.Marshal(fmt.Sprintf("omitted:%dB", len(raw)))
	if err != nil {
		return raw
	}
	snap.Summarized = append(snap.Summarized, key)
	return marker
}

// sanitizeUpstreamURL 上游 URL 脱敏：去掉 userinfo、query 与 fragment，只保留 scheme://host/path。
// query 里可能带 api-key/api-version/token 等敏感值（Azure 风格尤其常见），一旦落库就是泄漏。
func sanitizeUpstreamURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "" // 解析不出来就不落，宁可缺字段也不泄漏原文
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	u.RawFragment = ""
	return u.String()
}

// defaultUpstreamPath 求某基础协议的内置默认路径（供快照推算上游 URL，与各 Provider 的常量同源）。
func defaultUpstreamPath(protocol, endpoint string) string {
	switch channelprotocol.Canonical(protocol) {
	case channelprotocol.ProtocolAnthropicMessages:
		return anthropicEndpoint
	case channelprotocol.ProtocolOpenAIResponses:
		return responsesEndpoint
	default:
		return endpoint
	}
}

// ===== 请求 context 透传（单次构造、单点写入） =====

// reqParamsCtxKey 请求参数快照在 context 中的键（私有类型避免与其它键冲突）。
type reqParamsCtxKey struct{}

// withReqParams 把快照挂到 context 上（组包处调用一次；候选级覆盖写一次）。
func withReqParams(ctx context.Context, raw []byte) context.Context {
	return context.WithValue(ctx, reqParamsCtxKey{}, raw)
}

// reqParamsFromCtx 取出当前请求的快照（无则 nil）。写入漏斗 writeCallLogUsage 调用它落库。
func reqParamsFromCtx(ctx context.Context) []byte {
	v, _ := ctx.Value(reqParamsCtxKey{}).([]byte)
	return v
}
