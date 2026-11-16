// Package upstream 是灵枢 V2 的「上游网关客户端 + 同步器」域。
//
// 阶段一范围（本包当前只落地协议无关部分）：
//   - 同步器骨架：严格串行、失败退避重试 5s/20s/60s 最多 3 次、耗尽后留可诊断错误；
//   - sys_configs 三项运行参数：网关基址 / 超时（流式两项）/ 降级开关（三开关）；
//   - 账本写通道的接口与占位实现（字段名等 Ark 定稿后补齐）。
//
// 铁律（与已批准的实施计划一致，别自行扩面）：
//   - **不改网关代码**：本包只读上游数据与落库，转发/协议互译仍在网关侧；
//   - **任何密钥一律不进 sys_configs、不进代码、不进日志**：上游鉴权凭据只以
//     「逐请求注入」的方式由装配层持有，不落到配置表与错误文案里；
//   - 对外口径里不出现代理侧产品名（界面文案另有约定，后端也不主动写进日志字段）。
//
// 配置形态（裁定 F1，统一为 Ark §9.4 的三键）：
//
//	upstream.base_url   "http://127.0.0.1:3425"
//	upstream.timeouts   {"stream_first_byte_ms":30000,"stream_idle_ms":120000}
//	upstream.degrade    {"passthrough_enabled":true,"ledger_enabled":true,"sync_enabled":true}
//
// 上一版的单值 `upstream.timeout_ms` **已删除**：转发链的超时语义本来就分两段
// （等响应头的首包超时、流中途的空闲超时），塞进一个数只能表达其中一段，
// 另一段永远靠代码默认值 —— 那是看不见的配置。转发链只读这里，不再读任何
// 应用级配置（上一版的 magpie.* 键全部作废）。
package upstream

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/team/llmgateway/internal/pkg/logger"
)

// sys_configs 键（唯一来源：seeding 的默认值与这里的常量必须对齐，见 seeding/seed_test.go）。
//
// 只允许出现「非敏感」运行参数：基址、超时、降级开关。凭据类（admin key / callerKey /
// 任何 token、password、secret）**永不**进配置表。
const (
	// ConfigKeyBaseURL 是上游网关基址（空 = 未接入，同步不启动、转发侧保持停用）。
	ConfigKeyBaseURL = "upstream.base_url"
	// ConfigKeyTimeouts 是超时块：{"stream_first_byte_ms":N,"stream_idle_ms":N}。
	ConfigKeyTimeouts = "upstream.timeouts"
	// ConfigKeyDegrade 是降级开关块：{"passthrough_enabled":b,"ledger_enabled":b,"sync_enabled":b}。
	ConfigKeyDegrade = "upstream.degrade"
)

// 超时字段名（与 JSON 键逐字一致；改键名等于改配置契约）。
const (
	FieldStreamFirstByteMS = "stream_first_byte_ms"
	FieldStreamIdleMS      = "stream_idle_ms"
	FieldPassthroughOn     = "passthrough_enabled"
	FieldLedgerOn          = "ledger_enabled"
	FieldSyncOn            = "sync_enabled"
)

// 超时边界：太小的超时会把正常长耗时接口打成失败，太大则一轮同步可能跨周期。
const (
	DefaultStreamFirstByte = 30 * time.Second  // stream_first_byte_ms 默认 30000
	DefaultStreamIdle      = 120 * time.Second // stream_idle_ms 默认 120000
	MinTimeout             = time.Second
	MaxTimeout             = 10 * time.Minute
)

// ConfigKeys 返回本期上游链路的 sys_configs 键白名单（供种子对齐与「无密钥」守卫用例使用）。
func ConfigKeys() []string {
	return []string{ConfigKeyBaseURL, ConfigKeyTimeouts, ConfigKeyDegrade}
}

// Timeouts 是转发链的两段超时（毫秒入库，解析后为 Duration）。
type Timeouts struct {
	// StreamFirstByte 是「收到响应头」的等待上限；超时 = 504（此时还没写响应头，改得动状态码）。
	StreamFirstByte time.Duration
	// StreamIdle 是流式中途「连续多久没有数据」的上限；超时只能断流（响应头已发出）。
	StreamIdle time.Duration
}

// MarshalJSON 按**裁定的键名**输出两段超时（毫秒整数）。
//
// 为什么必须自己实现：结构体字段是 Go 命名 + time.Duration（秒），直接 json.Marshal 会得到
// `{"StreamFirstByte":30000000000,...}` —— 既不是配置键名、单位也不是毫秒。
// 管理端回显/写种子/静态检查都按这里的形状对账，别让它漂。
func (t Timeouts) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]int64{
		FieldStreamFirstByteMS: int64(t.StreamFirstByte / time.Millisecond),
		FieldStreamIdleMS:      int64(t.StreamIdle / time.Millisecond),
	})
}

// MarshalJSON 按裁定的键名输出三段降级开关。
func (d Degrade) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]bool{
		FieldPassthroughOn: d.PassthroughEnabled,
		FieldLedgerOn:      d.LedgerEnabled,
		FieldSyncOn:        d.SyncEnabled,
	})
}

// Degrade 是三段降级开关（关 = 该段停摆并告警，不把错误抛给前端、不清已有数据）。
type Degrade struct {
	// PassthroughEnabled 关 = /v1 转发停用（返回 503 + 明确文案，不代理）。
	PassthroughEnabled bool
	// LedgerEnabled 关 = 账本拉取停摆（对账窗口置 skipped，不是 blocked）。
	LedgerEnabled bool
	// SyncEnabled 关 = 模型镜像同步停摆（沿用上期快照，stale 不刷新）。
	SyncEnabled bool
}

// RuntimeConfig 是同步链路当前生效的运行参数（全部非敏感）。
type RuntimeConfig struct {
	// BaseURL 是上游网关基址，已去空白与尾部斜杠；空表示未接入。
	BaseURL string
	// Timeouts 是两段超时（已按 MinTimeout~MaxTimeout 校验）。
	Timeouts Timeouts
	// Degrade 是三段降级开关。
	Degrade Degrade
}

// DefaultRuntimeConfig 返回代码内默认配置（基址空 = 未接入；开关默认全开）。
func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		Timeouts: Timeouts{
			StreamFirstByte: DefaultStreamFirstByte,
			StreamIdle:      DefaultStreamIdle,
		},
		Degrade: Degrade{PassthroughEnabled: true, LedgerEnabled: true, SyncEnabled: true},
	}
}

// Enabled 表示基址已配置（未配置时同步不启动，避免对着空地址狂重试）。
func (c RuntimeConfig) Enabled() bool { return strings.TrimSpace(c.BaseURL) != "" }

// SyncActive 表示「基址已配置且同步开关未关」——同步轮的闸门。
func (c RuntimeConfig) SyncActive() bool { return c.Enabled() && c.Degrade.SyncEnabled }

// LedgerActive 表示账本拉取可用（基址 + 账本开关）。
func (c RuntimeConfig) LedgerActive() bool { return c.Enabled() && c.Degrade.LedgerEnabled }

// PassthroughActive 表示转发可用（基址 + 转发开关）。
func (c RuntimeConfig) PassthroughActive() bool { return c.Enabled() && c.Degrade.PassthroughEnabled }

// ConfigSource 是 sys_configs 单键读取能力（billing.SqlStore 的 GetConfig 满足）。
type ConfigSource interface {
	GetConfig(ctx context.Context, key string) ([]byte, error)
}

// ConfigProvider 读取并缓存三项运行参数。
//
// 缓存口径与仓库既有配置读取一致：只在「三项都干净读到」时写缓存；读取/解析出问题时不写缓存
// （瞬时故障可自愈），并把问题交给调用方记 WARN。**读失败绝不把已有值抹成空**——沿用上期
// 成功值（lastGood），失败方向是「继续用旧配置跑」，不是「停摆并清数据」。
type ConfigProvider struct {
	src ConfigSource

	mu       sync.RWMutex
	gen      uint64
	cached   *RuntimeConfig
	lastGood *RuntimeConfig
}

// NewConfigProvider 创建配置提供方；src 为 nil 时恒用默认值并在 Load 里报问题。
func NewConfigProvider(src ConfigSource) *ConfigProvider {
	return &ConfigProvider{src: src}
}

// Invalidate 清空缓存（sys_configs 写入后由装配层调用，保证下一轮同步即生效）。
func (p *ConfigProvider) Invalidate() {
	p.mu.Lock()
	p.gen++
	p.cached = nil
	p.mu.Unlock()
}

// Load 返回当前生效配置与本次读取发现的问题（问题不影响生效值，仅需告警）。
func (p *ConfigProvider) Load(ctx context.Context) (RuntimeConfig, []string) {
	if p == nil {
		return DefaultRuntimeConfig(), []string{"配置提供方未装配，使用代码默认值"}
	}
	p.mu.RLock()
	cached := p.cached
	gen := p.gen
	lastGood := p.lastGood
	p.mu.RUnlock()
	if cached != nil {
		return *cached, nil
	}

	cfg := DefaultRuntimeConfig()
	if lastGood != nil {
		cfg = *lastGood
	}
	if p.src == nil {
		return cfg, []string{"配置源未装配，使用代码默认值（或上期值）"}
	}

	var problems []string
	if raw, err := p.src.GetConfig(ctx, ConfigKeyBaseURL); err != nil {
		problems = append(problems, fmt.Sprintf("读取 %s 失败：%v（沿用上期值 %q）", ConfigKeyBaseURL, err, cfg.BaseURL))
	} else if v, verr := normalizeBaseURL(parseText(raw)); verr != nil {
		problems = append(problems, fmt.Sprintf("%s 非法：%v（沿用上期值 %q）", ConfigKeyBaseURL, verr, cfg.BaseURL))
	} else {
		cfg.BaseURL = v
	}

	if raw, err := p.src.GetConfig(ctx, ConfigKeyTimeouts); err != nil {
		problems = append(problems, fmt.Sprintf("读取 %s 失败：%v（沿用上期值）", ConfigKeyTimeouts, err))
	} else if v, vproblems := parseTimeouts(raw); len(vproblems) > 0 {
		problems = append(problems, vproblems...)
		// 只覆盖解析成功的字段，坏字段保留上期值（逐字段回落，不是整块丢弃）。
		if v.StreamFirstByte > 0 {
			cfg.Timeouts.StreamFirstByte = v.StreamFirstByte
		}
		if v.StreamIdle > 0 {
			cfg.Timeouts.StreamIdle = v.StreamIdle
		}
	} else {
		cfg.Timeouts = v
	}

	if raw, err := p.src.GetConfig(ctx, ConfigKeyDegrade); err != nil {
		problems = append(problems, fmt.Sprintf("读取 %s 失败：%v（沿用上期值）", ConfigKeyDegrade, err))
	} else if v, vproblems := parseDegrade(raw); len(vproblems) > 0 {
		problems = append(problems, vproblems...)
		// 逐开关回落：坏的那一项保留上期值，其余照常生效。
		if v.PassthroughEnabled != nil {
			cfg.Degrade.PassthroughEnabled = *v.PassthroughEnabled
		}
		if v.LedgerEnabled != nil {
			cfg.Degrade.LedgerEnabled = *v.LedgerEnabled
		}
		if v.SyncEnabled != nil {
			cfg.Degrade.SyncEnabled = *v.SyncEnabled
		}
	} else {
		cfg.Degrade = v.degrade(cfg.Degrade)
	}

	if len(problems) == 0 {
		p.mu.Lock()
		// 仅当回源期间未发生失效（代际未变）才回填：避免写入前的旧值滞留（无 TTL）。
		if p.gen == gen {
			stored := cfg
			p.cached = &stored
			p.lastGood = &stored
		}
		p.mu.Unlock()
	}
	return cfg, problems
}

// ConfigGate 把配置接成同步闸门：基址未配置或同步开关关闭 → 本轮跳过（不是故障）。
//
// 基址未配置时**不做**「对着空地址狂重试」——直接跳过并由装配层决定要不要告警；
// 配置读取问题只记 WARN 不阻断，沿用上期值继续跑（失败方向是「用旧配置」）。
//
// 注意开关语义与上一版的区别：上一版的 `upstream.degrade` 是**单个布尔**（true = 降级停摆），
// 现在改为三段开关，**值方向是「true = 该段可用」**——名字从 degraded 改成 enabled，
// 避免「配置写 true 到底是停还是跑」这种要回读代码才敢改的坑。
func ConfigGate(p *ConfigProvider, log *slog.Logger) func(ctx context.Context) error {
	if log == nil {
		log = logger.Fallback()
	}
	return func(ctx context.Context) error {
		cfg, problems := p.Load(ctx)
		for _, msg := range problems {
			log.Warn("上游同步配置异常，沿用上期值", "problem", msg)
		}
		if !cfg.Enabled() {
			return &SkipError{Op: "上游同步", Reason: fmt.Sprintf("%s 未配置，本轮跳过", ConfigKeyBaseURL)}
		}
		if !cfg.Degrade.SyncEnabled {
			return &SkipError{Op: "上游同步", Reason: fmt.Sprintf("%s.%s=false（该段关闭），本轮沿用上期快照",
				ConfigKeyDegrade, FieldSyncOn)}
		}
		return nil
	}
}

// parseText 解析 sys_configs 文本值：兼容 JSON 字符串（"http://..."）与裸文本（http://...）。
func parseText(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return ""
	}
	var quoted string
	if err := json.Unmarshal([]byte(s), &quoted); err == nil {
		return strings.TrimSpace(quoted)
	}
	return s
}

// normalizeBaseURL 规范化基址：只接受 http/https，且**不许带凭据**（user:pass@）。
// 后者是硬性红线：把令牌写进 URL 等于把密钥存进了 sys_configs。
func normalizeBaseURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("不是合法 URL：%w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("协议必须是 http/https，got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("缺少主机名")
	}
	if u.User != nil {
		return "", fmt.Errorf("基址里不许带凭据（user:pass@）：密钥不得进配置")
	}
	return strings.TrimRight(s, "/"), nil
}

// parseTimeouts 解析 {"stream_first_byte_ms":N,"stream_idle_ms":N}。
//
// 宽容但**不静默**：缺键回落到代码默认（不算错，运维只写一项也合法），未知键记问题
// （拼错的键会被当成「没写」而静默走默认值，这类故障最难查）。
func parseTimeouts(raw []byte) (Timeouts, []string) {
	out := Timeouts{}
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return Timeouts{StreamFirstByte: DefaultStreamFirstByte, StreamIdle: DefaultStreamIdle}, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return out, []string{fmt.Sprintf("%s 不是 JSON 对象：%v", ConfigKeyTimeouts, err)}
	}
	var problems []string
	for key := range m {
		if key != FieldStreamFirstByteMS && key != FieldStreamIdleMS {
			problems = append(problems, fmt.Sprintf("%s 含未知键 %q（拼错会被当成未配置而静默走默认值）",
				ConfigKeyTimeouts, key))
		}
	}
	out.StreamFirstByte = DefaultStreamFirstByte
	out.StreamIdle = DefaultStreamIdle
	if v, ok, err := parseMillisField(m, FieldStreamFirstByteMS); err != nil {
		problems = append(problems, fmt.Sprintf("%s.%s 非法：%v（沿用上期值）", ConfigKeyTimeouts, FieldStreamFirstByteMS, err))
		out.StreamFirstByte = 0
	} else if ok {
		out.StreamFirstByte = v
	}
	if v, ok, err := parseMillisField(m, FieldStreamIdleMS); err != nil {
		problems = append(problems, fmt.Sprintf("%s.%s 非法：%v（沿用上期值）", ConfigKeyTimeouts, FieldStreamIdleMS, err))
		out.StreamIdle = 0
	} else if ok {
		out.StreamIdle = v
	}
	return out, problems
}

// parseMillisField 取一个毫秒字段（兼容 JSON 数字与字符串数字），并校验边界；缺键 ok=false。
func parseMillisField(m map[string]json.RawMessage, key string) (time.Duration, bool, error) {
	raw, present := m[key]
	if !present {
		return 0, false, nil
	}
	var ms int64
	s := strings.TrimSpace(string(raw))
	if err := json.Unmarshal([]byte(s), &ms); err != nil {
		var str string
		if uerr := json.Unmarshal([]byte(s), &str); uerr != nil {
			return 0, true, fmt.Errorf("不是整数毫秒：%q", s)
		}
		var perr error
		ms, perr = parseInt64(strings.TrimSpace(str))
		if perr != nil {
			return 0, true, fmt.Errorf("不是整数毫秒：%q", str)
		}
	}
	d := time.Duration(ms) * time.Millisecond
	if d < MinTimeout {
		return 0, true, fmt.Errorf("%s 小于下限 %s", d, MinTimeout)
	}
	if d > MaxTimeout {
		return 0, true, fmt.Errorf("%s 超过上限 %s", d, MaxTimeout)
	}
	return d, true, nil
}

// degradeParsed 是三开关的「解析结果」，nil 表示该键缺失或非法（回落上期值）。
type degradeParsed struct {
	PassthroughEnabled *bool
	LedgerEnabled      *bool
	SyncEnabled        *bool
}

// degrade 把解析结果套到兜底值上（缺键保留兜底值）。
func (d degradeParsed) degrade(fallback Degrade) Degrade {
	out := fallback
	if d.PassthroughEnabled != nil {
		out.PassthroughEnabled = *d.PassthroughEnabled
	}
	if d.LedgerEnabled != nil {
		out.LedgerEnabled = *d.LedgerEnabled
	}
	if d.SyncEnabled != nil {
		out.SyncEnabled = *d.SyncEnabled
	}
	return out
}

// parseDegrade 解析 {"passthrough_enabled":b,"ledger_enabled":b,"sync_enabled":b}。
func parseDegrade(raw []byte) (degradeParsed, []string) {
	var out degradeParsed
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return out, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return out, []string{fmt.Sprintf("%s 不是 JSON 对象：%v", ConfigKeyDegrade, err)}
	}
	var problems []string
	for key := range m {
		switch key {
		case FieldPassthroughOn, FieldLedgerOn, FieldSyncOn:
		default:
			problems = append(problems, fmt.Sprintf("%s 含未知键 %q（开关拼错等于没关）", ConfigKeyDegrade, key))
		}
	}
	set := func(key string) (*bool, error) {
		raw, present := m[key]
		if !present {
			return nil, nil
		}
		v, err := parseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("%s 非法：%v", key, err)
		}
		return &v, nil
	}
	if v, err := set(FieldPassthroughOn); err != nil {
		problems = append(problems, fmt.Sprintf("%s.%v（沿用上期值）", ConfigKeyDegrade, err))
	} else {
		out.PassthroughEnabled = v
	}
	if v, err := set(FieldLedgerOn); err != nil {
		problems = append(problems, fmt.Sprintf("%s.%v（沿用上期值）", ConfigKeyDegrade, err))
	} else {
		out.LedgerEnabled = v
	}
	if v, err := set(FieldSyncOn); err != nil {
		problems = append(problems, fmt.Sprintf("%s.%v（沿用上期值）", ConfigKeyDegrade, err))
	} else {
		out.SyncEnabled = v
	}
	return out, problems
}

// parseBool 解析布尔值（兼容 JSON bool 与 "true"/"false" 文本）。
func parseBool(raw []byte) (bool, error) {
	s := strings.TrimSpace(string(raw))
	var b bool
	if err := json.Unmarshal([]byte(s), &b); err == nil {
		return b, nil
	}
	var str string
	if err := json.Unmarshal([]byte(s), &str); err == nil {
		if v, perr := parseBoolText(strings.TrimSpace(str)); perr == nil {
			return v, nil
		}
	}
	return false, fmt.Errorf("不是布尔值：%q", s)
}

// parseBoolText 只接受 true/false（含 1/0），别的一律报错——不要用「非 false 即 true」的模糊判定。
func parseBoolText(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "t", "yes":
		return true, nil
	case "false", "0", "f", "no":
		return false, nil
	}
	return false, fmt.Errorf("不是布尔值：%q", s)
}

// parseInt64 是 strconv.ParseInt(10, 64) 的薄包装（保留原样以便错误文案统一）。
func parseInt64(s string) (int64, error) {
	return strconv.ParseInt(s, 10, 64)
}
