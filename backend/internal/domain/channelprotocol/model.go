// Package channelprotocol 承载「渠道协议」实体：由外挂预设包 provider 条目物化而来的
// 接入模板（baseURL / protocols / auth / 特殊请求头 / 模型清单），独立于渠道存在，
// 可被多个渠道引用；未引用时渠道走内联配置。
//
// 基础协议为代码固定的 3 个：openai-chat / openai-responses / anthropic-messages。
package channelprotocol

import "strings"

// 基础协议（代码固定，不可由预设包新增）。
const (
	ProtocolOpenAIChat        = "openai-chat"
	ProtocolOpenAIResponses   = "openai-responses"
	ProtocolAnthropicMessages = "anthropic-messages"
)

// BaseProtocols 返回全部基础协议名（有序，稳定）。
func BaseProtocols() []string {
	return []string{ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolAnthropicMessages}
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

// Names 返回协议集合中的协议名（仅基础协议，未知键忽略），按基础协议顺序稳定输出。
func (p Protocols) Names() []string {
	out := make([]string, 0, len(p))
	for _, name := range BaseProtocols() {
		if _, ok := p[name]; ok {
			out = append(out, name)
		}
	}
	return out
}

// DefaultProtocol 返回标记 default 的协议名；无则返回第一个基础协议（稳定）。
func (p Protocols) DefaultProtocol() string {
	for _, name := range BaseProtocols() {
		if spec, ok := p[name]; ok && spec.Default {
			return name
		}
	}
	for _, name := range BaseProtocols() {
		if _, ok := p[name]; ok {
			return name
		}
	}
	return ""
}

// AuthSpec 凭据注入方式。
type AuthSpec struct {
	Header string            `json:"header,omitempty"` // Authorization | x-api-key | api-key
	Scheme string            `json:"scheme,omitempty"` // bearer | raw
	Query  map[string]string `json:"query,omitempty"`  // 需放 querystring 的鉴权/版本参数
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
	Auth           AuthSpec                `json:"Auth"`
	RequestHeaders HeaderPolicy            `json:"RequestHeaders"`
	Models         map[string]ModelGateway `json:"Models"`
	PresetSourceID string                  `json:"PresetSourceID"`
	Edited         bool                    `json:"Edited"`
	SourceStatus   string                  `json:"SourceStatus"`
	CreatedAt      string                  `json:"CreatedAt,omitempty"`
	UpdatedAt      string                  `json:"UpdatedAt,omitempty"`
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
