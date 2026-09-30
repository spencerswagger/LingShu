package sync

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
)

// ExternalPrice 外部价格源的一个模型条目（来自 models.dev 的 /api.json 嵌套结构）。
type ExternalPrice struct {
	ID       string  `json:"id"`       // 模型 ID（models.dev 内层 key）
	Provider string  `json:"provider"` // 供应商名（models.dev 外层 key）
	Pricing  Pricing `json:"pricing"`
}

// Pricing 模型单价（美元 / 百万 token）。除 prompt/completion 外保留缓存读写与推理价，
// 供对外模型「查看/应用参考价」与价格目录展示使用。
type Pricing struct {
	Prompt     float64 `json:"prompt"`
	Completion float64 `json:"completion"`
	CacheRead  float64 `json:"cache_read"`
	CacheWrite float64 `json:"cache_write"`
	Reasoning  float64 `json:"reasoning"`
}

// sourcePayload 是 https://models.dev/api.json 的真实结构：
// 顶层 key 为 provider 名，每 provider 的 models 下 key 才为模型 ID。
// 价格单位是美元 / 百万 token；input=输入、output=输出、cache_read=缓存读（可选）。
type sourcePayload map[string]struct {
	Models map[string]struct {
		ID   string `json:"id"`
		Cost struct {
			Input      *float64 `json:"input"`
			Output     *float64 `json:"output"`
			CacheRead  *float64 `json:"cache_read"`
			CacheWrite *float64 `json:"cache_write"`
			Reasoning  *float64 `json:"reasoning"`
		} `json:"cost"`
	} `json:"models"`
}

// flatten 把嵌套 payload 摊平为 []ExternalPrice。
// 模型 ID 取 models 的内层 key（等于模型自身的 id 字段），不要用 provider 名。
// 若某模型没有 cost 或 input/output 任一缺失，则跳过该项，避免产生无意义的 0 价格条目。
func (p sourcePayload) flatten() []ExternalPrice {
	var out []ExternalPrice
	for providerName, provider := range p {
		for modelID, model := range provider.Models {
			if model.Cost.Input == nil || model.Cost.Output == nil {
				continue
			}
			cacheRead, cacheWrite, reasoning := 0.0, 0.0, 0.0
			if model.Cost.CacheRead != nil {
				cacheRead = *model.Cost.CacheRead
			}
			if model.Cost.CacheWrite != nil {
				cacheWrite = *model.Cost.CacheWrite
			}
			if model.Cost.Reasoning != nil {
				reasoning = *model.Cost.Reasoning
			}
			out = append(out, ExternalPrice{
				ID:       modelID,
				Provider: providerName,
				Pricing: Pricing{
					Prompt:     *model.Cost.Input,
					Completion: *model.Cost.Output,
					CacheRead:  cacheRead,
					CacheWrite: cacheWrite,
					Reasoning:  reasoning,
				},
			})
		}
	}
	return out
}

// fetchSource 从 sourceURL 拉取并解析为模型价格列表。
// 价格字段位于真实的 cost 字段（input→Prompt、output→Completion），非数组的 pricing。
// 真实拉取失败不 panic，由调用方（Syncer.RetryOnce / Run）记录日志并返回错误。
func fetchSource(ctx context.Context, client *http.Client, sourceURL string) ([]ExternalPrice, error) {
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, fmt.Errorf("构造价格源请求失败: %w", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("请求价格源失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("价格源返回非 200 状态码: %d", resp.StatusCode)
	}
	var payload sourcePayload
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("解析价格源 JSON 失败: %w", err)
	}
	return payload.flatten(), nil
}
