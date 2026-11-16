// Package magpie 是灵枢 → 代理侧（magpie）的转发链：灵枢只做 facade
// （调用令牌鉴权、模型准入、model 改写、计费），转发与协议互译全部由 magpie 承担。
//
// 铁律（V2 第一期契约）：
//   - **只换两样东西**：出站凭证头（一令牌一把 callerKey）与请求里的 model（改成
//     external_models.magpie_model）。其余请求字节、响应字节、stream 分包、SSE、错误体、
//     header 透传一律原样，不注入 include_usage、不改写响应。
//   - model 改写必须发生在灵枢侧：价目表（售价倍率）是灵枢唯一价源，交给代理侧改等于把价源搬走。
//   - 灵枢不再选渠道、不再算路由、不再落 call_logs；租户/令牌/模型白名单准入在这里，
//     渠道挑选与 failover 是 magpie 的职责。
package magpie

import (
	"strings"
	"time"

	appconfig "github.com/team/llmgateway/internal/config"
)

// Config 是转发链的运行参数（全部有默认值，零值可用）。
type Config struct {
	// BaseURL 为 magpie 基址（单端口，四协议同口），不带尾斜杠。
	BaseURL string
	// AuthHeader/AuthScheme 是逐请求注入 callerKey 的头名与前缀（上游头名以实测为准，故可配）。
	AuthHeader string
	AuthScheme string
	// AdminKey 仅服务端使用（管理面/用量统计），不参与转发鉴权。
	AdminKey string
	// Connect 为建连（DNS/TCP/TLS）超时。
	Connect time.Duration
	// Request 为非流式请求总超时。
	Request time.Duration
	// StreamFirstByte 为流式请求「收到响应头」超时。
	StreamFirstByte time.Duration
	// StreamIdle 为流式读空闲超时（连续空闲超过即中断）。
	StreamIdle time.Duration
	// CountTokens 为令牌计数端点超时（不计费、无 usage）。
	CountTokens time.Duration
}

// FromApp 把应用配置里的 magpie 节（已填默认值 + 环境变量覆盖）转为转发链参数。
func FromApp(c *appconfig.Config) Config {
	if c == nil {
		return Config{}.normalized()
	}
	r := c.ResolvedMagpie()
	return Config{
		BaseURL:         r.BaseURL,
		AuthHeader:      r.AuthHeader,
		AuthScheme:      r.AuthScheme,
		AdminKey:        r.AdminKey,
		Connect:         time.Duration(r.ConnectTimeoutMS) * time.Millisecond,
		Request:         time.Duration(r.RequestTimeoutMS) * time.Millisecond,
		StreamFirstByte: time.Duration(r.StreamFirstByteMS) * time.Millisecond,
		StreamIdle:      time.Duration(r.StreamIdleMS) * time.Millisecond,
		CountTokens:     time.Duration(r.CountTokensTimeoutMS) * time.Millisecond,
	}
}

// normalized 为缺省项填默认值（测试与零值构造用）。
func (c Config) normalized() Config {
	if strings.TrimSpace(c.AuthHeader) == "" {
		c.AuthHeader = appconfig.DefaultMagpieAuthHeader
	}
	if strings.TrimSpace(c.AuthScheme) == "" {
		c.AuthScheme = appconfig.DefaultMagpieAuthScheme
	}
	if c.Connect <= 0 {
		c.Connect = appconfig.DefaultMagpieConnectTimeoutMS * time.Millisecond
	}
	if c.Request <= 0 {
		c.Request = appconfig.DefaultMagpieRequestTimeoutMS * time.Millisecond
	}
	if c.StreamFirstByte <= 0 {
		c.StreamFirstByte = appconfig.DefaultMagpieStreamFirstByteMS * time.Millisecond
	}
	if c.StreamIdle <= 0 {
		c.StreamIdle = appconfig.DefaultMagpieStreamIdleMS * time.Millisecond
	}
	if c.CountTokens <= 0 {
		c.CountTokens = appconfig.DefaultMagpieCountTokensTimeoutMS * time.Millisecond
	}
	c.BaseURL = strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	return c
}

// Enabled 表示是否配置了代理基址。未配置时 /v1 保持停用（410），不做半残挂载。
func (c Config) Enabled() bool { return strings.TrimSpace(c.BaseURL) != "" }

// credentialHeaderValue 组装注入的凭据头值（scheme 为空时只放裸 key）。
func (c Config) credentialHeaderValue(callerKey string) string {
	if strings.TrimSpace(c.AuthScheme) == "" {
		return callerKey
	}
	return c.AuthScheme + " " + callerKey
}
