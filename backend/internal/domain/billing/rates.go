// Package billing 提供大模型 API 调用的计费引擎（三段计费公式、上下文分档、
// 时段系数、记账与钱包扣减）。本包只依赖接口，不耦合具体领域实现。
package billing

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Usage 一次调用的 token 用量（单位：个）。基线三段 + v2 增量段：
//   - Input：纯输入，不含缓存读；**不含缓存写**（v2 计价行缓存写独立分桶后）。
//     旧三段口径（0011）上游的「缓存写」token（Anthropic cache_creation_input_tokens、
//     百炼显式缓存 cache_creation_input_tokens）计入本段——该口径在 v2 计价行上
//     由 v3 结算拆出进 CacheWrite（四项定案①：token 分桶互斥，同一 token 只落一个桶）。
//   - CacheRead：命中提示词缓存读取的输入。
//   - CacheWrite：写缓存的 token（v2 计价行才有值；账本行来自 ledger_entries.cache_write，
//     与 Input 互斥分桶）。omitempty：0 时不输出键，旧三段行的 JSONB 形状逐字节不变，
//     历史行缺键按 0 计。
//   - Output：生成 token，含推理（Anthropic 无独立推理字段，推理本就在 output 内）。
//
// 该结构同时是 billing_records.tokens 的 JSONB 存储值，键必须保持 snake（存储契约），
// 对外（HTTP）一律使用 WireUsage。
type Usage struct {
	Input      int64 `json:"input"`
	Output     int64 `json:"output"`
	CacheRead  int64 `json:"cache_read"`
	CacheWrite int64 `json:"cache_write,omitempty"`
}

// WireUsage 是 Usage 的对外视图：JSON 键为 PascalCase（内部 API 契约），
// 与 DB 存储（billing_records.tokens 的 snake 键）区分。仅用于 HTTP 响应/请求边界。
// 与 Usage 之间可直接类型转换（字段同名同类型，仅 tag 不同）。
type WireUsage struct {
	Input      int64 `json:"Input"`
	Output     int64 `json:"Output"`
	CacheRead  int64 `json:"CacheRead"`
	CacheWrite int64 `json:"CacheWrite,omitempty"`
}

// Rates 三段 token 的倍率表（倍率 = 人民币每百万 token 价格，无单位纯数字），键只能是三个 rateKey。
// 该 map 同时是 external_models.sale_rates / channel_models.cost_rates 的 JSONB 存储值，
// 键必须保持 snake（存储契约），对外（HTTP）一律使用 WireRates。
type Rates map[string]float64

// WireRates 是 Rates 的对外视图：JSON 键为 PascalCase（内部 API 契约），
// 内部仍以 snake 键保存（与存储结构一致），因此可与 Rates 直接类型转换。
// 兼容性：Marshal 时未知键（非三段）原样输出；Unmarshal 时既接受 PascalCase 也接受 snake 键。
type WireRates Rates

// rateKeys 计费单价必须包含的三个键（售价/成本通用，与 external_models.sale_rates、
// channel_models.cost_rates 的键保持一致）。
var rateKeys = []string{"input", "output", "cache_read"}

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

// NormalizeRatesJSON 把单价表 JSON 归一为 snake 键形态（存储契约：billing_records.tokens /
// external_models.sale_rates / internal_models.cost_rates 的键一律 snake）。
//
// 为什么必须有它：HTTP 边界（如 PUT cost-rates）允许 PascalCase 进来，如果原样落库，
// 读取侧 `billing.Rates` 的普通 Unmarshal 只认 snake 键 → 成本静默算 0。
// 写入即归一 + 读取侧兜底再归一（两处同用本函数，单一来源）。
// 无法解析时原样返回（形状校验归调用方）。
func NormalizeRatesJSON(raw []byte) []byte {
	var m map[string]float64
	if err := json.Unmarshal(raw, &m); err != nil {
		return raw
	}
	out := make(map[string]float64, len(m))
	for k, v := range m {
		if s, ok := rateKeySnake[k]; ok {
			k = s
		}
		out[k] = v
	}
	b, err := json.Marshal(out)
	if err != nil {
		return raw
	}
	return b
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

// ValidateRates 校验单价表：三个键齐全且均非负（历史数据里的多余键不参与校验、不影响计费）。
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

// sumRates 计算三段 token×单价 之和。
// 三段互斥（Input 为纯输入、不含缓存读），直接相加即可，
// 不存在缓存命中 token 被输出价与缓存价重复累计的问题。
//
// CacheWrite 折回 input：混合层形态（售价 v2 + 成本旧三段，或反向）下 tokens 已是
// 四桶拆分（cache_write 独立成段）；旧三段价没有 write 档，按**历史口径**把该段
// token 以 input 价计——否则该段从成本整段消失。CacheWrite=0（全部旧路径）时 no-op，
// 行为逐字节不变。
func sumRates(u Usage, r Rates) float64 {
	return float64(u.Input+u.CacheWrite)*r["input"] +
		float64(u.Output)*r["output"] +
		float64(u.CacheRead)*r["cache_read"]
}
