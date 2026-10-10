// Package billing 提供大模型 API 调用的计费引擎（五段计费公式、上下文分档、
// 时段系数、记账与钱包扣减）。本包只依赖接口，不耦合具体领域实现。
package billing

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Usage 一次调用的五段 token 用量（单位：个）。
// 该结构同时是 billing_records.tokens 的 JSONB 存储值，键必须保持 snake（存储契约），
// 对外（HTTP）一律使用 WireUsage。
type Usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write"`
	Reasoning  int64 `json:"reasoning"`
}

// WireUsage 是 Usage 的对外视图：JSON 键为 PascalCase（内部 API 契约），
// 与 DB 存储（billing_records.tokens 的 snake 键）区分。仅用于 HTTP 响应/请求边界。
// 与 Usage 之间可直接类型转换（字段同名同类型，仅 tag 不同）。
type WireUsage struct {
	Input      int64 `json:"Input"`
	Output     int64 `json:"Output"`
	CacheRead  int64 `json:"CacheRead"`
	CacheWrite int64 `json:"CacheWrite"`
	Reasoning  int64 `json:"Reasoning"`
}

// Rates 五段 token 的倍率表（倍率 = 人民币每百万 token 价格，无单位纯数字），键只能是五个 rateKey。
// 该 map 同时是 external_models.sale_rates / channel_models.cost_rates 的 JSONB 存储值，
// 键必须保持 snake（存储契约），对外（HTTP）一律使用 WireRates。
type Rates map[string]float64

// WireRates 是 Rates 的对外视图：JSON 键为 PascalCase（内部 API 契约），
// 内部仍以 snake 键保存（与存储结构一致），因此可与 Rates 直接类型转换。
// 兼容性：Marshal 时未知键（非五段）原样输出；Unmarshal 时既接受 PascalCase 也接受 snake 键。
type WireRates Rates

// rateKeys 计费单价必须包含的五个键（售价/成本通用，与 external_models.sale_rates、
// channel_models.cost_rates 的键保持一致）。
var rateKeys = []string{"input", "output", "cache_read", "cache_write", "reasoning"}

// rateKeyPascal / rateKeySnake 由 rateKeys 推导出的双向命名映射（如 cache_read ↔ CacheRead）。
var rateKeyPascal, rateKeySnake = buildRateKeyMaps()

func buildRateKeyMaps() (toPascal, toSnake map[string]string) {
	toPascal = make(map[string]string, len(rateKeys))
	toSnake = make(map[string]string, len(rateKeys))
	for _, k := range rateKeys {
		p := pascalName(k)
		toPascal[k] = p
		toSnake[p] = k
	}
	return toPascal, toSnake
}

// pascalName 把 snake_case 键转为 PascalCase（input→Input，cache_read→CacheRead）。
func pascalName(s string) string {
	parts := strings.Split(s, "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, "")
}

// MarshalJSON 以 PascalCase 键输出；nil 仍序列化为 null（保持与 Rates 一致）。
func (w WireRates) MarshalJSON() ([]byte, error) {
	if w == nil {
		return []byte("null"), nil
	}
	m := make(map[string]float64, len(w))
	for k, v := range w {
		if p, ok := rateKeyPascal[k]; ok {
			k = p
		}
		m[k] = v
	}
	return json.Marshal(m)
}

// UnmarshalJSON 接受 PascalCase 键（新契约），也兼容 snake 键（旧客户端/存储），
// 统一归一为内部 snake 键保存。
func (w *WireRates) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		*w = nil
		return nil
	}
	var m map[string]float64
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	out := make(WireRates, len(m))
	for k, v := range m {
		if s, ok := rateKeySnake[k]; ok {
			k = s
		}
		out[k] = v
	}
	*w = out
	return nil
}

// ValidateRates 校验单价表：键齐全且均非负。
func ValidateRates(r Rates) error {
	for _, k := range rateKeys {
		v, ok := r[k]
		if !ok {
			return fmt.Errorf("计费单价缺少键 %q", k)
		}
		if v < 0 {
			return fmt.Errorf("计费单价 %q 不能为负", k)
		}
	}
	return nil
}

// sumRates 计算五段 token×单价 之和。
func sumRates(u Usage, r Rates) float64 {
	return float64(u.Input)*r["input"] +
		float64(u.Output)*r["output"] +
		float64(u.CacheRead)*r["cache_read"] +
		float64(u.CacheWrite)*r["cache_write"] +
		float64(u.Reasoning)*r["reasoning"]
}
