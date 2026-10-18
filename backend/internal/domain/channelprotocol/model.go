// Package channelprotocol 承载「渠道协议」实体：由外挂预设包 provider 条目物化而来的
// 接入模板（baseURL / protocols / 特殊请求头 / 模型清单），独立于渠道存在，
// 可被多个渠道引用；未引用时渠道走内联配置。
//
// 鉴权由基础协议隐含（ProtocolAuth），不在协议内单独配置。
// 基础协议为代码固定的 3 个：openai-chat / openai-responses / anthropic-messages。
package channelprotocol

import (
	"strings"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/idgen"
)

// 基础协议（代码固定，不可由预设包新增）。
const (
	ProtocolOpenAIChat        = "openai-chat"
	ProtocolOpenAIResponses   = "openai-responses"
	ProtocolAnthropicMessages = "anthropic-messages"
)

// LegacyProtocolOpenAICompat 是历史单协议枚举值。渠道内联配置沿用该值时，
// 按 openai-chat 语义解释（见 Canonical）。
const LegacyProtocolOpenAICompat = "openai-compat"

// Canonical 把协议名归一：历史值 openai-compat 映射为 openai-chat，其余原样返回。
func Canonical(name string) string {
	if name == LegacyProtocolOpenAICompat {
		return ProtocolOpenAIChat
	}
	return name
}

// IsBaseProtocol 判断协议名是否为基础协议。
func IsBaseProtocol(name string) bool {
	switch name {
	case ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolAnthropicMessages:
		return true
	}
	return false
}

// 来源状态（source_status）。
const (
	SourceActive = "ACTIVE" // 来源正常
	SourceStale  = "STALE"  // 预设条目已删除/改名（协议保留，标记失效）
)

// ProtocolSpec 单个基础协议在渠道协议中的配置项（对齐 docs/preset-pack-format.md）。
type ProtocolSpec struct {
	BaseURL string `json:"baseUrl,omitempty"`
	Path    string `json:"path,omitempty"`
	Default bool   `json:"default,omitempty"`
}

// Protocols 该渠道协议原生支持的上游基础协议集合；键存在 = 支持。
type Protocols map[string]ProtocolSpec

// Has 判断协议集合是否包含某基础协议。
func (p Protocols) Has(name string) bool {
	_, ok := p[name]
	return ok
}

// ProtocolAuth 返回某基础协议隐含的鉴权注入方式（协议不同则不同；凭据由渠道密钥提供）：
// openai-chat / openai-responses → Authorization: Bearer <凭据>；
// anthropic-messages → x-api-key: <凭据>（原样）。
// 若需协议以外的鉴权方式，用请求头策略的「覆盖(set)」显式写入（会覆盖此处注入）。
func ProtocolAuth(protocol string) (header, scheme string) {
	if Canonical(protocol) == ProtocolAnthropicMessages {
		return "x-api-key", "raw"
	}
	return "Authorization", "bearer"
}

// HeaderPolicy 请求头策略（对齐 docs/preset-pack-format.md 6.3）。
type HeaderPolicy struct {
	PassthroughMode string            `json:"passthroughMode,omitempty"` // none | whitelist | blacklist | all（默认 none）
	Whitelist       []string          `json:"whitelist,omitempty"`
	Blacklist       []string          `json:"blacklist,omitempty"`
	Set             map[string]string `json:"set,omitempty"` // 支持变量替换
}

// Passthrough modes.
const (
	PassthroughNone      = "none"
	PassthroughWhitelist = "whitelist"
	PassthroughBlacklist = "blacklist"
	PassthroughAll       = "all"
)

// ModelGateway 预设包中模型级 gateway 覆盖：既承载协议/基址覆盖，也承载该模型
// 在渠道协议清单中的完整配置（对外模型绑定 / 成本 / 时段 / 分档）。
// 追加字段均为 camelCase，向后兼容（旧数据无这些键时零值）。
type ModelGateway struct {
	BaseURL   string    `json:"baseUrl,omitempty"`
	Protocols Protocols `json:"protocols,omitempty"`
	// ExternalModelID 该内部模型绑定的对外模型 ID；>0 才参与自动物化为 channel_models。
	ExternalModelID int64                    `json:"externalModelID,string,omitempty"`
	CostRates       billing.WireRates        `json:"costRates,omitempty"`
	TimeConfig      *billing.TimeCoeffConfig `json:"timeConfig,omitempty"`
	ContextTiers    []billing.TierRule       `json:"contextTiers,omitempty"`
}

// 错误策略的生效范围。
const (
	ErrorScopeChannel = "channel" // 整渠道生效
	ErrorScopeModels  = "models"  // 仅对 ModelIDs 指定的模型生效
)

// 错误命中后的处置动作。
const (
	ErrorActionNone    = "none"    // 不额外动作
	ErrorActionDrain   = "drain"   // 排空
	ErrorActionDisable = "disable" // 禁用
)

// ErrorPolicy 协议级错误策略：对「命中签名」的错误按 Disposition 处置。
// 未配置任何策略（空列表）时保持改造前行为（默认安全态）。
type ErrorPolicy struct {
	// Scope 生效范围：channel=整渠道；models=仅对 ModelIDs 生效。
	Scope string `json:"scope"`
	// ModelIDs scope=models 时生效的内部模型（channel_models 行）ID 列表。
	// 雪花 ID 超出 JS 安全整数，须以字符串数组序列化（idgen.IDs，对齐既有 ID 约定）。
	ModelIDs idgen.IDs `json:"modelIds,omitempty"`
	// Match 错误签名（status + code/type/message）。
	Match ErrorMatch `json:"match"`
	// Disposition 命中后的处置。
	Disposition ErrorDisposition `json:"disposition"`
	// Note 备注（可读说明，不参与匹配）。
	Note string `json:"note,omitempty"`
}

// ErrorMatch 错误签名：各字段为约束，空/零值表示不约束该维度。
type ErrorMatch struct {
	Status       int    `json:"status,omitempty"`
	ErrorType    string `json:"errorType,omitempty"`
	ErrorCode    string `json:"errorCode,omitempty"`
	MessageRegex string `json:"messageRegex,omitempty"`
}

// ErrorDisposition 命中错误后的处置：
//   - CountIntoHealth：是否计入渠道健康错误率/429 与鉴权计数（false=完全不影响熔断）；
//   - Failover：是否 failover（*bool 区分「未设置=沿用现状」与「显式 false=不转移」）；
//   - Action：none|drain|disable；
//   - DownstreamStatus：命中后替代默认 502 的下游状态码（0=沿用默认）。
type ErrorDisposition struct {
	CountIntoHealth  bool   `json:"countIntoHealth"`
	Failover         *bool  `json:"failover,omitempty"`
	Action           string `json:"action,omitempty"`
	DownstreamStatus int    `json:"downstreamStatus,omitempty"`
}

// 协议特性 key：特性逻辑在代码中实现并注册，渠道协议按 key + 参数挂载（可多项启停）。
const (
	FeatureThinkingRoundtrip   = "thinking-roundtrip"   // 请求侧按上游约定回传 reasoning_content
	FeatureToolSchemaNormalize = "tool-schema-normalize" // 对齐工具 input_schema
)

// Feature 渠道协议上挂载的一项协议特性：Key 指定代码实现的特性，Params 为其参数（允许为空）。
type Feature struct {
	Key    string         `json:"key"`
	Params map[string]any `json:"params,omitempty"`
}

// IsValidFeatureKey 判断特性 key 是否为代码实现并注册的合法特性。
func IsValidFeatureKey(key string) bool {
	switch key {
	case FeatureThinkingRoundtrip, FeatureToolSchemaNormalize:
		return true
	}
	return false
}

// ChannelProtocol 对应 channel_protocols 表一行。
type ChannelProtocol struct {
	ID             int64                   `json:"ID,string"`
	Name           string                  `json:"Name"`
	BaseURL        string                  `json:"BaseURL"`
	Protocols      Protocols               `json:"Protocols"`
	RequestHeaders HeaderPolicy            `json:"RequestHeaders"`
	Models         map[string]ModelGateway `json:"Models"`
	// ErrorPolicy 协议级错误策略（空=未配置，行为与改造前一致）。
	ErrorPolicy []ErrorPolicy `json:"errorPolicy"`
	// Features 协议特性集合（空=未挂载任何特性，行为与改造前逐字节一致）。
	Features       []Feature `json:"features"`
	PresetSourceID string    `json:"PresetSourceID"`
	Edited         bool      `json:"Edited"`
	SourceStatus   string    `json:"SourceStatus"`
	CreatedAt      time.Time `json:"CreatedAt,omitempty"`
	UpdatedAt      time.Time `json:"UpdatedAt,omitempty"`
}

// EffectiveBaseURL 返回某基础协议的有效 baseURL：协议级 baseUrl 优先，回落渠道协议级 base_url。
func (cp *ChannelProtocol) EffectiveBaseURL(protocol string) string {
	if cp == nil {
		return ""
	}
	if spec, ok := cp.Protocols[protocol]; ok && strings.TrimSpace(spec.BaseURL) != "" {
		return strings.TrimSpace(spec.BaseURL)
	}
	return strings.TrimSpace(cp.BaseURL)
}

// EffectiveHeaderPolicy 返回该渠道协议的请求头策略；nil 接收者返回零值策略（无透传、无「覆盖」）。
// 网关热路径（真实转发）与渠道健康探测/模型拉取共用同一取值口径，避免各处漂移。
func (cp *ChannelProtocol) EffectiveHeaderPolicy() HeaderPolicy {
	if cp == nil {
		return HeaderPolicy{}
	}
	return cp.RequestHeaders
}

// EffectiveModelProtocols 求某内部模型可达的上游协议集合：
// 模型级覆盖（channel_models.protocols）优先，其次预设模型级定义，最后 provider 级协议集。
func (cp *ChannelProtocol) EffectiveModelProtocols(internalModelID string, modelOverride Protocols) Protocols {
	if len(modelOverride) > 0 {
		return modelOverride
	}
	if cp == nil {
		return nil
	}
	if mg, ok := cp.Models[internalModelID]; ok && len(mg.Protocols) > 0 {
		return mg.Protocols
	}
	return cp.Protocols
}
