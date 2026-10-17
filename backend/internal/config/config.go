package config

import (
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// DefaultJWTTTLMinutes 登录令牌默认有效期（分钟）。
// 单一权威默认值：docker-entrypoint.sh 的 JWT_TTL_MINUTES 默认值、README 环境变量表
// 与 config.example.yaml 必须与之保持一致，避免生成配置与代码兜底值漂移。
const DefaultJWTTTLMinutes = 360

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Database  DBConfig        `yaml:"database"`
	JWT       JWTConfig       `yaml:"jwt"`
	Security  SecurityConfig  `yaml:"security"`
	Sync      SyncConfig      `yaml:"sync"`
	Billing   BillingConfig   `yaml:"billing"`
	Preset    PresetConfig    `yaml:"preset"`
	ReadCache ReadCacheConfig `yaml:"read_cache"`
}

// ReadCacheConfig 网关热路径「配置类读缓存」的总开关（直查 fallback 安全开关）。
//
// 默认启用缓存；置 enabled: false（或环境变量 READ_CACHE_ENABLED=false）后，
// 全部配置类读缓存退回直查数据库（安全态，行为与引入缓存前一致，便于异常时一键回退）。
type ReadCacheConfig struct {
	// Enabled 为空（未配置）时视为启用缓存；显式 false 关闭全部缓存。
	Enabled *bool `yaml:"enabled"`
	// TokenTTLSeconds 令牌鉴权读缓存的安全 TTL（秒）。仅作最后防线（非即时失效机制）；
	// <=0 时用 DefaultTokenCacheTTLSeconds。
	TokenTTLSeconds int `yaml:"token_ttl_seconds"`
}

// DefaultTokenCacheTTLSeconds 令牌鉴权读缓存安全 TTL 默认值（秒）。仅最后防线，
// 即时失效由各写路径失效钩子保证，TTL 与之无关。
const DefaultTokenCacheTTLSeconds = 60

// ReadCacheEnabled 解析只读配置缓存是否启用。
// 优先级：环境变量 READ_CACHE_ENABLED（true/false）> 配置文件 read_cache.enabled > 默认启用。
// 默认启用缓存但保留一键退回直查的安全态。
func (c *Config) ReadCacheEnabled() bool {
	if v := strings.TrimSpace(os.Getenv("READ_CACHE_ENABLED")); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	if c.ReadCache.Enabled != nil {
		return *c.ReadCache.Enabled
	}
	return true
}

// TokenCacheTTL 解析令牌鉴权读缓存的安全 TTL。
// 优先级：环境变量 TOKEN_CACHE_TTL_SECONDS（正整数）> 配置文件 read_cache.token_ttl_seconds > 默认 60s。
// 该 TTL 仅作兜底（条目超期一律回源），不承担「写后即时失效」职责。
func (c *Config) TokenCacheTTL() time.Duration {
	if v := strings.TrimSpace(os.Getenv("TOKEN_CACHE_TTL_SECONDS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return time.Duration(n) * time.Second
		}
	}
	if c.ReadCache.TokenTTLSeconds > 0 {
		return time.Duration(c.ReadCache.TokenTTLSeconds) * time.Second
	}
	return DefaultTokenCacheTTLSeconds * time.Second
}

// PresetConfig 渠道预设包配置。PackURL 为外挂预设站点地址：
// 为空表示未配置，首次启动不拉取（AC-12）。
type PresetConfig struct {
	PackURL string `yaml:"pack_url"`
}

// BillingConfig 计费相关配置。
// RetryQueuePath 为账单落库失败时的本地持久重试队列文件（WAL）；置空则关闭该兜底。
type BillingConfig struct {
	RetryQueuePath       string `yaml:"retry_queue_path"`
	RetryIntervalSeconds int    `yaml:"retry_interval_seconds"`
}

// SecurityConfig 国密安全配置。SM4Key 为 16 字节（32 hex 字符）渠道凭据加密密钥，
// 启动时必填（缺失/非法会直接退出）。生产环境务必配置强随机密钥，不得使用内置默认值。
type SecurityConfig struct {
	SM4Key string `yaml:"sm4_key"`
}

type ServerConfig struct {
	Addr      string `yaml:"addr"`
	StaticDir string `yaml:"static_dir"` // 前端静态托管目录，可空
}
type DBConfig struct {
	DSN string `yaml:"dsn"`
}
type JWTConfig struct {
	PrivateKeyPath string `yaml:"private_key_path"`
	PublicKeyPath  string `yaml:"public_key_path"`
	TTLMinutes     int    `yaml:"ttl_minutes"`
}
type SyncConfig struct {
	PriceSourceURL  string `yaml:"price_source_url"`
	IntervalMinutes int    `yaml:"interval_minutes"`
}

func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, err
	}
	if c.Server.Addr == "" {
		c.Server.Addr = ":8080"
	}
	if c.JWT.TTLMinutes == 0 {
		c.JWT.TTLMinutes = DefaultJWTTTLMinutes
	}
	if c.Billing.RetryQueuePath == "" {
		c.Billing.RetryQueuePath = "data/billing_retry.jsonl"
	}
	if c.Billing.RetryIntervalSeconds <= 0 {
		c.Billing.RetryIntervalSeconds = 30
	}
	return &c, nil
}
