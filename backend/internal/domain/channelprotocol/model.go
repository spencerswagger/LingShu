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

// ModelGateway 预设包中模型级 gateway 覆盖（本网关仅消费 protocols / baseUrl）。
type ModelGateway struct {
	BaseURL   string    `json:"baseUrl,omitempty"`
	Protocols Protocols `json:"protocols,omitempty"`
}

// ChannelProtocol 对应 channel_protocols 表一行。
type ChannelProtocol struct {
	ID             int64                   `json:"ID,string"`
	Name           string                  `json:"Name"`
	BaseURL        string                  `json:"BaseURL"`
	Protocols      Protocols               `json:"Protocols"`
	RequestHeaders HeaderPolicy            `json:"RequestHeaders"`
	Models         map[string]ModelGateway `json:"Models"`
	PresetSourceID string                  `json:"PresetSourceID"`
	Edited         bool                    `json:"Edited"`
	SourceStatus   string                  `json:"SourceStatus"`
	CreatedAt      time.Time               `json:"CreatedAt,omitempty"`
	UpdatedAt      time.Time               `json:"UpdatedAt,omitempty"`
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
