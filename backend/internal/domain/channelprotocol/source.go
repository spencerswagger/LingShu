package channelprotocol

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// Pack 是一个预设包：键为 provider ID，值为该 provider 条目。
// 形状对齐 models.dev v1（见 docs/preset-pack-format.md），扩展字段收敛在 gateway 命名空间。
type Pack map[string]Provider

// Provider 预设包中的单个供应商条目。
type Provider struct {
	ID      string           `json:"id"`
	Name    string           `json:"name"`
	Env     []string         `json:"env"`
	Npm     string           `json:"npm"`
	API     *string          `json:"api"`
	Doc     string           `json:"doc"`
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
	Auth           AuthSpec     `json:"auth"`
	RequestHeaders HeaderPolicy `json:"requestHeaders"`
}

// toChannelProtocol 把预设 provider 条目物化为渠道协议（edited=false，来源 ACTIVE）。
// provider ID 作为 preset_source_id；模型级 gateway 覆盖收敛进 Models。
func (p Provider) toChannelProtocol() *ChannelProtocol {
	cp := &ChannelProtocol{
		Name:           p.Name,
		BaseURL:        p.Gateway.BaseURL,
		Protocols:      canonicalProtocols(p.Gateway.Protocols),
		Auth:           p.Gateway.Auth,
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
		if len(m.Gateway.Protocols) == 0 && m.Gateway.BaseURL == "" {
			continue
		}
		key := id
		if m.ID != "" {
			key = m.ID
		}
		cp.Models[key] = ModelGateway{
			BaseURL:   m.Gateway.BaseURL,
			Protocols: canonicalProtocols(m.Gateway.Protocols),
		}
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
