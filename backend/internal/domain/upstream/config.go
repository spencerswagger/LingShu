// Package upstream 是灵枢 V2 的「上游网关客户端 + 同步器」域。
//
// 阶段一范围（本包当前只落地协议无关部分）：
//   - 同步器骨架：严格串行、失败退避重试 5s/20s/60s 最多 3 次、耗尽后留可诊断错误；
//   - sys_configs 三项运行参数：网关基址 / 超时 / 降级开关；
//   - 账本写通道的接口与占位实现（字段名等 Ark 定稿后补齐）。
//
// 铁律（与已批准的实施计划一致，别自行扩面）：
//   - **不改网关代码**：本包只读上游数据与落库，转发/协议互译仍在网关侧；
//   - **任何密钥一律不进 sys_configs、不进代码、不进日志**：上游鉴权凭据只以
//     「逐请求注入」的方式由装配层持有，不落到配置表与错误文案里；
//   - 对外口径里不出现代理侧产品名（界面文案另有约定，后端也不主动写进日志字段）。
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

// sys_configs 键（唯一来源：seeding 的默认值与这里的常量必须对齐，见 seeding/defaults_test.go）。
//
// 只允许出现「非敏感」运行参数：基址、超时、降级开关。凭据类（admin key / callerKey /
// 任何 token、password、secret）**永不**进配置表。
const (
	// ConfigKeyBaseURL 是上游网关基址（空 = 未接入，同步不启动、转发侧保持停用）。
	ConfigKeyBaseURL = "upstream.base_url"
	// ConfigKeyTimeoutMS 是上游单次调用的超时（毫秒）。
	ConfigKeyTimeoutMS = "upstream.timeout_ms"
	// ConfigKeyDegrade 是降级开关：true 时同步停摆、沿用上期快照（不清数据）。
	ConfigKeyDegrade = "upstream.degrade"
)

// 超时边界：太小的超时会把正常长耗时接口打成失败，太大则一轮同步可能跨周期。
const (
	DefaultTimeoutMS = 30000
	MinTimeout       = time.Second
	MaxTimeout       = 10 * time.Minute
)

// ConfigKeys 返回本期上游链路的 sys_configs 键白名单（供种子对齐与「无密钥」守卫用例使用）。
func ConfigKeys() []string {
	return []string{ConfigKeyBaseURL, ConfigKeyTimeoutMS, ConfigKeyDegrade}
}

// RuntimeConfig 是同步链路当前生效的运行参数（全部非敏感）。
type RuntimeConfig struct {
	// BaseURL 是上游网关基址，已去空白与尾部斜杠；空表示未接入。
	BaseURL string
	// Timeout 是单次调用超时（已按 MinTimeout~MaxTimeout 校验）。
	Timeout time.Duration
	// Degraded 为 true 表示降级：本轮不同步，沿用上期快照。
	Degraded bool
}

// DefaultRuntimeConfig 返回代码内默认配置（基址空 = 未接入）。
func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{Timeout: DefaultTimeoutMS * time.Millisecond}
}

// Enabled 表示基址已配置（未配置时同步不启动，避免对着空地址狂重试）。
func (c RuntimeConfig) Enabled() bool { return strings.TrimSpace(c.BaseURL) != "" }

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

	if raw, err := p.src.GetConfig(ctx, ConfigKeyTimeoutMS); err != nil {
		problems = append(problems, fmt.Sprintf("读取 %s 失败：%v（沿用上期值 %s）", ConfigKeyTimeoutMS, err, cfg.Timeout))
	} else if v, verr := parseTimeoutMS(raw); verr != nil {
		problems = append(problems, fmt.Sprintf("%s 非法：%v（沿用上期值 %s）", ConfigKeyTimeoutMS, verr, cfg.Timeout))
	} else {
		cfg.Timeout = v
	}

	if raw, err := p.src.GetConfig(ctx, ConfigKeyDegrade); err != nil {
		problems = append(problems, fmt.Sprintf("读取 %s 失败：%v（沿用上期值 %v）", ConfigKeyDegrade, err, cfg.Degraded))
	} else if v, verr := parseBool(raw); verr != nil {
		problems = append(problems, fmt.Sprintf("%s 非法：%v（沿用上期值 %v）", ConfigKeyDegrade, verr, cfg.Degraded))
	} else {
		cfg.Degraded = v
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

// ConfigGate 把配置接成同步闸门：基址未配置或降级开关打开 → 本轮跳过（不是故障）。
//
// 基址未配置时**不做**「对着空地址狂重试」——直接跳过并由装配层决定要不要告警；
// 配置读取问题只记 WARN 不阻断，沿用上期值继续跑（失败方向是「用旧配置」）。
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
		if cfg.Degraded {
			return &SkipError{Op: "上游同步", Reason: fmt.Sprintf("%s=true（降级），本轮沿用上期快照", ConfigKeyDegrade)}
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

// parseTimeoutMS 解析超时毫秒数（兼容 JSON 数字与 JSON 字符串），并校验 1s~10min。
func parseTimeoutMS(raw []byte) (time.Duration, error) {
	var ms int64
	s := strings.TrimSpace(string(raw))
	if err := json.Unmarshal([]byte(s), &ms); err != nil {
		var str string
		if uerr := json.Unmarshal([]byte(s), &str); uerr != nil {
			return 0, fmt.Errorf("不是整数毫秒：%q", s)
		}
		n, perr := strconv.ParseInt(strings.TrimSpace(str), 10, 64)
		if perr != nil {
			return 0, fmt.Errorf("不是整数毫秒：%q", str)
		}
		ms = n
	}
	d := time.Duration(ms) * time.Millisecond
	if d < MinTimeout {
		return 0, fmt.Errorf("%s 小于下限 %s", d, MinTimeout)
	}
	if d > MaxTimeout {
		return 0, fmt.Errorf("%s 超过上限 %s", d, MaxTimeout)
	}
	return d, nil
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
		if v, perr := strconv.ParseBool(strings.TrimSpace(str)); perr == nil {
			return v, nil
		}
	}
	return false, fmt.Errorf("不是布尔值：%q", s)
}
