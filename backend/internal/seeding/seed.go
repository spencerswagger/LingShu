// Package seeding 提供系统初始化数据（初始管理员与默认配置），幂等可重复执行。
package seeding

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/pkg/crypto"
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
	"billing.time_config": `{"timezone":"Asia/Shanghai","default_coeff":1.0,"periodic_segments":[
  {"name":"低谷","start":"00:00","end":"08:00","coeff":0.5},
  {"name":"普通","start":"08:00","end":"18:00","coeff":1.0},
  {"name":"高峰","start":"18:00","end":"22:00","coeff":1.5},
  {"name":"低谷","start":"22:00","end":"24:00","coeff":0.5}
],"date_overrides":[]}`,
}

// systemUser / systemToken 常量：健康探测开销的归属身份（成本记账），用户侧界面隐藏。
const (
	systemUsername = "system"
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
		`INSERT INTO tokens(token_hash, token_display, user_id, display_name, status)
		 VALUES ($1, $2, $3, $4, 'ACTIVE')
		 ON CONFLICT DO NOTHING`,
		"system-probe-hash", "sk-gw-system-probe", u.ID, systemTokenName); err != nil {
		return fmt.Errorf("seed system token: %w", err)
	}
	log.Printf("[seed] 系统探测身份就绪 username=%s token=%s", systemUsername, systemTokenName)
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
		Username:     defaultAdminUsername,
		PasswordHash: hash,
		Role:         role,
		Status:       identity.StatusActive,
		PricingMode:  identity.PricingModeSale,
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
	log.Printf("[seed] 已创建初始管理员账户 username=%s password=%s，请尽快修改默认口令", defaultAdminUsername, defaultAdminPassword)
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