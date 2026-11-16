// Package seeding 提供系统初始化数据（初始管理员与默认配置），幂等可重复执行。
package seeding

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/pkg/crypto"
	"github.com/team/llmgateway/internal/pkg/idgen"
)

const (
	defaultAdminUsername = "admin"
	defaultAdminPassword = "admin123"
)

// defaultSysConfigs 是系统默认配置，key 存在则跳过。
var defaultSysConfigs = map[string]string{
	"billing.r": `10000`,
	// USD/CNY 汇率（1 USD = cny_rate CNY），仅用于 model.dev 美元参考价折算为人民币倍率。
	"billing.cny_rate": `6.8`,
	// 上下文分档默认无：不启用分档（模型级/全局均为空时计费不加分档系数）。
	"billing.context_tiers": `[]`,
	// 时段系数默认配置：键名必须与 billing.TimeCoeffConfig 的 JSON 契约一致（PascalCase），
	// 否则解析为零值（Timezone="" → CoeffFor 报错 → 时段系数恒 0 → 计费 0 → 结算失败）。
	"billing.time_config": `{"Timezone":"Asia/Shanghai","Default":1.0,"Periodic":[
  {"Name":"低谷","Start":"00:00","End":"08:00","Coeff":0.5},
  {"Name":"普通","Start":"08:00","End":"18:00","Coeff":1.0},
  {"Name":"高峰","Start":"18:00","End":"22:00","Coeff":1.5},
  {"Name":"低谷","Start":"22:00","End":"24:00","Coeff":0.5}
],"Overrides":[]}`,
	// 路由成本比较样值用量：新会话创建时以该样值代入渠道成本单价求和比较「谁更便宜」
	//（不参与实际计费）。三段（输入/缓存读/输出）均可配置，缺省按 0 计。
	"routing.cost_sample": `{"input":10000,"cache_read":9000,"output":1000}`,
	// 订阅池渠道前缀名单（代理侧把订阅模型以 provider 前缀暴露在选择器里）。
	// 命中即拒绝租户调用（403 model_not_allowed）——订阅类账号默认不对租户开放。
	// 键与默认值由 magpie 包定义，此处保持一致（seeding 不反向依赖域包，见 defaults_test.go 的对齐断言）。
	"magpie.subscription_prefixes": `["claude/","codex/","copilot/","gemini-code-assist/","antigravity/","grok/"]`,
	// 路由样值补齐比例：路由时刻只有「输入 token」可知，按这两个「相对纯输入」的比例
	// 估算缓存读与输出（纯输入 + 缓存读 = 总输入），供会话成本路由的成本比较使用。
	// 默认值与 routing.cost_sample（10000/9000/1000）同源。
	"routing.sample_profile": `{"cache_read_ratio":0.9,"output_ratio":0.1}`,
	// 上游网关（代理侧）三项运行参数：基址 / 超时 / 降级开关。
	// 键名与代码默认值由 upstream 包定义，此处保持一致（对齐断言见 defaults_test.go）。
	// 红线：只允许非敏感运行参数——凭据（admin key / callerKey / token / password）永不进 sys_configs。
	"upstream.base_url": `""`,
	// 两段超时：首包（等响应头）30s、流空闲 120s。连接/非流式总超时是代码常量，不进配置表。
	"upstream.timeouts": `{"stream_first_byte_ms":30000,"stream_idle_ms":120000}`,
	// 三段开关：true = 该段可用（写 true 不是「停」）。
	"upstream.degrade": `{"passthrough_enabled":true,"ledger_enabled":true,"sync_enabled":true}`,
}

// systemUser / systemToken 常量：健康探测开销的归属身份（成本记账），用户侧界面隐藏。
const (
	systemUsername  = "system"
	systemTokenName = "system-probe"
)

// Seed 执行幂等初始化：确保存在 ADMIN 账户、其钱包行、默认 sys_configs 与系统探测身份。
func Seed(ctx context.Context, db *sql.DB) error {
	if err := seedAdmin(ctx, db); err != nil {
		return err
	}
	if err := seedConfigs(ctx, db); err != nil {
		return err
	}
	return seedSystem(ctx, db)
}

// seedSystem 幂等创建系统用户与系统密钥（健康探测开销记账；用户侧界面隐藏）。
func seedSystem(ctx context.Context, db *sql.DB) error {
	store := identity.NewStore(db)
	u, err := store.GetByUsername(ctx, systemUsername)
	if errors.Is(err, sql.ErrNoRows) {
		hash, _ := crypto.HashPassword(randomSecret(32))
		created, cerr := store.Create(ctx, &identity.User{
			Username:     systemUsername,
			PasswordHash: hash,
			Role:         identity.RoleAdmin,
			Status:       identity.StatusActive,
			PricingMode:  identity.PricingModeCost,
			IsSystem:     true,
		})
		if cerr != nil {
			var pgErr *pgconn.PgError
			if errors.As(cerr, &pgErr) && pgErr.Code == "23505" {
				u = nil // 并发已建
			} else {
				return fmt.Errorf("create system user: %w", cerr)
			}
		} else {
			u = created
		}
	} else if err != nil {
		return fmt.Errorf("query system user: %w", err)
	}
	if u == nil {
		u, err = store.GetByUsername(ctx, systemUsername)
		if err != nil {
			return fmt.Errorf("reload system user: %w", err)
		}
	}
	if err := store.EnsureWallet(ctx, u.ID); err != nil {
		return err
	}
	// 系统钱包保持高余额，保证探测账单可成功记账。
	if _, err := db.ExecContext(ctx,
		`UPDATE credit_wallets SET balance = 1000000000 WHERE user_id = $1 AND balance < 1000000000`, u.ID); err != nil {
		return fmt.Errorf("bump system wallet: %w", err)
	}
	// 系统密钥（探测账单中的 token 归属）
	if _, err := db.ExecContext(ctx,
		`INSERT INTO tokens(id, token_hash, token_display, user_id, display_name, status)
		 VALUES ($1, $2, $3, $4, $5, 'ACTIVE')
		 ON CONFLICT DO NOTHING`,
		idgen.New(), "system-probe-hash", "sk-gw-system-probe", u.ID, systemTokenName); err != nil {
		return fmt.Errorf("seed system token: %w", err)
	}
	slog.Info("系统探测身份就绪", "username", systemUsername, "token", systemTokenName)
	return nil
}

// SystemIdentity 返回系统用户与系统密钥 ID（探测开销记账用）。
func SystemIdentity(ctx context.Context, db *sql.DB) (userID, tokenID int64, err error) {
	err = db.QueryRowContext(ctx,
		`SELECT u.id, t.id FROM users u
		 LEFT JOIN tokens t ON t.user_id = u.id AND t.display_name = $1
		 WHERE u.username = $2 AND u.is_system = true`,
		systemTokenName, systemUsername).Scan(&userID, &tokenID)
	return
}

// randomSecret 生成随机 hex 字符串（长度 n 字节）。
func randomSecret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		return "changeme"
	}
	return hex.EncodeToString(b)
}

// seedAdmin 在无 ADMIN 用户时创建默认管理员并同步 credit_wallets 行。
func seedAdmin(ctx context.Context, db *sql.DB) error {
	store := identity.NewStore(db)

	u, err := store.GetByUsername(ctx, defaultAdminUsername)
	if err == nil {
		// 已存在：仅兜底补钱包
		return store.EnsureWallet(ctx, u.ID)
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("query default admin: %w", err)
	}

	hash, err := crypto.HashPassword(defaultAdminPassword)
	if err != nil {
		return fmt.Errorf("hash default admin password: %w", err)
	}
	role := identity.RoleAdmin
	created, err := store.Create(ctx, &identity.User{
		Username:           defaultAdminUsername,
		PasswordHash:       hash,
		Role:               role,
		Status:             identity.StatusActive,
		PricingMode:        identity.PricingModeSale,
		MustChangePassword: true, // 新建默认管理员强制首登改密
	})
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			// 并发下另一个进程已建好，幂等返回
			return nil
		}
		return fmt.Errorf("create default admin: %w", err)
	}
	if err := store.EnsureWallet(ctx, created.ID); err != nil {
		return err
	}
	slog.Info("已创建初始管理员账户，请尽快登录并修改默认口令", "username", defaultAdminUsername)
	return nil
}

// seedConfigs 写入默认 sys_configs，若 key 已存在则跳过。
func seedConfigs(ctx context.Context, db *sql.DB) error {
	for key, val := range defaultSysConfigs {
		_, err := db.ExecContext(ctx,
			`INSERT INTO sys_configs(key, value) VALUES ($1, $2::jsonb)
			 ON CONFLICT (key) DO NOTHING`, key, val)
		if err != nil {
			return fmt.Errorf("seed sys_configs %s: %w", key, err)
		}
	}
	return nil
}
