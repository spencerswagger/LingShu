package config

import (
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server   ServerConfig   `yaml:"server"`
	Database DBConfig       `yaml:"database"`
	JWT      JWTConfig      `yaml:"jwt"`
	Security SecurityConfig `yaml:"security"`
	Sync     SyncConfig     `yaml:"sync"`
	Billing  BillingConfig  `yaml:"billing"`
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
		c.JWT.TTLMinutes = 720
	}
	if c.Billing.RetryQueuePath == "" {
		c.Billing.RetryQueuePath = "data/billing_retry.jsonl"
	}
	if c.Billing.RetryIntervalSeconds <= 0 {
		c.Billing.RetryIntervalSeconds = 30
	}
	return &c, nil
}
