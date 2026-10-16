package channelprotocol

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Pack 是一个预设包：键为 provider ID，值为该 provider 条目。
// 形状对齐 models.dev v1（见 docs/preset-pack-format.md），扩展字段收敛在 gateway 命名空间。
type Pack map[string]Provider

// Provider 预设包中的单个供应商条目。
type Provider struct {
	ID      string           `json:"id"`
	Name    string           `json:"name"`
	Env     []string         `json:"env,omitempty"`
	Npm     string           `json:"npm,omitempty"`
	API     *string          `json:"api,omitempty"`
	Doc     string           `json:"doc,omitempty"`
	Models  map[string]Model `json:"models"`
	Gateway GatewayBlock     `json:"gateway"`
}

// Model 预设包中的模型条目（本网关运行时不消费 cost/limit，仅保留 gateway 覆盖）。
type Model struct {
	ID          string       `json:"id"`
	Name        string       `json:"name"`
	Description string       `json:"description"`
	Gateway     ModelGateway `json:"gateway"`
}

// GatewayBlock 是 provider 级的本网关扩展块。
type GatewayBlock struct {
	BaseURL        string       `json:"baseUrl"`
	Protocols      Protocols    `json:"protocols"`
	RequestHeaders HeaderPolicy `json:"requestHeaders"`
	// Edited / SourceStatus 仅用于导出/导入承载编辑标记与来源状态，
	// 预设同步物化（toChannelProtocol）不读取它们。
	Edited       bool   `json:"edited,omitempty"`
	SourceStatus string `json:"sourceStatus,omitempty"`
}

// toChannelProtocol 把预设 provider 条目物化为渠道协议（edited=false，来源 ACTIVE）。
// provider ID 作为 preset_source_id；模型级 gateway 覆盖收敛进 Models。
func (p Provider) toChannelProtocol() *ChannelProtocol {
	cp := &ChannelProtocol{
		Name:           p.Name,
		BaseURL:        p.Gateway.BaseURL,
		Protocols:      canonicalProtocols(p.Gateway.Protocols),
		RequestHeaders: normalizePolicy(p.Gateway.RequestHeaders),
		PresetSourceID: p.ID,
		Edited:         false,
		SourceStatus:   SourceActive,
		Models:         make(map[string]ModelGateway),
	}
	if cp.Name == "" {
		cp.Name = p.ID
	}
	for id, m := range p.Models {
		if len(m.Gateway.Protocols) == 0 && m.Gateway.BaseURL == "" && m.Gateway.ExternalModelID <= 0 {
			continue
		}
		key := id
		if m.ID != "" {
			key = m.ID
		}
		mg := m.Gateway
		mg.Protocols = canonicalProtocols(mg.Protocols)
		cp.Models[key] = mg
	}
	return cp
}

// canonicalProtocols 归一化协议键（历史 openai-compat → openai-chat），仅保留基础协议键。
func canonicalProtocols(in Protocols) Protocols {
	if len(in) == 0 {
		return Protocols{}
	}
	out := make(Protocols, len(in))
	for name, spec := range in {
		c := Canonical(name)
		if !IsBaseProtocol(c) {
			continue
		}
		out[c] = spec
	}
	return out
}

// normalizePolicy 补齐请求头策略默认值（缺省 none）。
func normalizePolicy(p HeaderPolicy) HeaderPolicy {
	if p.PassthroughMode == "" {
		p.PassthroughMode = PassthroughNone
	}
	return p
}

// fetchPack 拉取并解析预设包 JSON。
func fetchPack(ctx context.Context, client *http.Client, url string) (Pack, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("构造预设包请求失败: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求预设包失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("预设包返回非 200 状态码: %d", resp.StatusCode)
	}
	var pack Pack
	if err := json.NewDecoder(resp.Body).Decode(&pack); err != nil {
		return nil, fmt.Errorf("解析预设包 JSON 失败: %w", err)
	}
	return pack, nil
}

// PullModelView 协议模型拉取返回给前端的单个模型视图（PascalCase 契约）。
type PullModelView struct {
	ID      string `json:"ID"`
	Object  string `json:"Object"`
	OwnedBy string `json:"OwnedBy"`
}

// pullUpstreamModel 上游 /models 返回的单个模型（外部 snake 契约，仅内部解析用）。
type pullUpstreamModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// pullModelsResponse 是 OpenAI-compat /models 的响应外壳。
type pullModelsResponse struct {
	Data []pullUpstreamModel `json:"data"`
}

// PullModels 用临时 API Key 请求协议 openai-chat 的有效 baseUrl + /models 拉取上游模型列表（不落库）。
// 临时 Key 仅用于本次请求，绝不落库；鉴权固定 Authorization: Bearer <ApiKey>。
func (s *Service) PullModels(ctx context.Context, id int64, apiKey string) ([]PullModelView, error) {
	apiKey = strings.TrimSpace(apiKey)
	if apiKey == "" {
		return nil, errBadRequest("ApiKey 不能为空")
	}
	cp, err := s.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	base := cp.EffectiveBaseURL(ProtocolOpenAIChat)
	if strings.TrimSpace(base) == "" {
		return nil, errBadRequest("该渠道协议未配置 openai-chat 的 Base URL，无法拉取模型")
	}
	target := strings.TrimRight(base, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("构造模型拉取请求失败: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)

	client := s.client
	if client == nil {
		client = http.DefaultClient
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取协议模型失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("上游模型接口返回非 2xx 状态码: %d", resp.StatusCode)
	}
	var out pullModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("解析上游模型响应失败: %w", err)
	}
	views := make([]PullModelView, 0, len(out.Data))
	for _, m := range out.Data {
		views = append(views, PullModelView{ID: m.ID, Object: m.Object, OwnedBy: m.OwnedBy})
	}
	return views, nil
}
