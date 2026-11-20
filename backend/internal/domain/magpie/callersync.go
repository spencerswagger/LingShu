package magpie

// callerKeySync 把 identity.TokenService 的 CallerKeySyncer 接到：
//   1) magpie 页面接口（在代理侧真正建/删/改名 callerKey）
//   2) magpie_keys 表（灵枢侧 token ↔ callerKey 映射与密文）
// 令牌明文 = callerKey 明文（灵枢生成；magpie add-key 支持自带 secret）。
//
// V3 改名批（2026-11-20 定案①④）：自动名从 ls-token-<id> 升级为 ls-u<uid>-t<tid>-<slug>。
// 改名走 magpie 的 rename-key 端点（真机实测：ID/secret/限额不变，客户端零感知），
// 密钥值不变；本地 magpie_keys 原行改 key_name 并把旧名存进 legacy_key_name
// （结算按名反查兜底，见 0030 迁移与 v3billing 反查）。
//
// 改名资格（不许凭名字猜测/样本特判）：仅当当前名 == 历史自动名格式
// ls-token-<tokenID>（旧代码唯一生成点）时才自动改名；
// 其它名字一律视为运维手工绑定，保持不动（skip 口径在 identity 回填侧）。
import (
	"context"
	"fmt"
	"strings"

	"github.com/team/llmgateway/internal/pkg/crypto"
)

// CallerKeyStore 是同步需要的映射存储能力（*KeyStore 满足；测试可替身）。
type CallerKeyStore interface {
	KeyBinder
	RenameAndActivate(ctx context.Context, tokenID int64, oldName, newName, keyCipher string) (*Key, error)
}

// CallerKeySync 实现 identity.CallerKeySyncer。
type CallerKeySync struct {
	page      *PageAdmin
	keys      CallerKeyStore
	secretKey []byte
}

func NewCallerKeySync(page *PageAdmin, keys CallerKeyStore, secretKey []byte) *CallerKeySync {
	return &CallerKeySync{page: page, keys: keys, secretKey: secretKey}
}

// LegacyAutoName 是旧口径自动名（ls-token-<id>，旧代码唯一生成点）。
// 改名资格判定与 Remove 的无行兜底都用它。
func LegacyAutoName(tokenID int64) string { return fmt.Sprintf("ls-token-%d", tokenID) }

// KeyNameFor 计算该令牌应有的自动名（新口径唯一实现点，identity 回填 skip 判定用）。
func KeyNameFor(userID, tokenID int64, displayName string) string {
	return keyNameFor(userID, tokenID, slugFromDisplayName(displayName))
}

// Ensure 确保该令牌在代理侧的 callerKey 为「目标名 + 给定明文」（状态终态 ACTIVE）：
//   - 无映射 → 按新名建（建令牌路径）；
//   - 有映射且名字已是目标名 → 幂等确保（明文对齐 + 映射行 ACTIVE）；
//   - 有映射且是历史自动名 ls-token-<id> → magpie 侧 rename-key 原地改名（值不变），
//     再确保明文，本地行改名并记 legacy_key_name（改名回填三步，幂等可重跑）；
//   - 有映射且是其它名字（运维手工绑定）→ 名字不动，只确保代理侧同名 key 存在且明文一致
//     （既有口径：auto 链把值对齐令牌明文）。
//
// 幂等：重复执行要么命中「已是终态」提前收敛，要么从当前实际状态继续推进；
// 顺序是 magpie 侧先改名、本地后改（任何一步中断，重跑都能从实际状态续上，
// 不产生重复数据、不产生「新旧名并存」）。
func (c *CallerKeySync) Ensure(ctx context.Context, userID int64, t TokenRef, secret string) error {
	if len(c.secretKey) == 0 {
		return fmt.Errorf("未配置 SM4 密钥，无法保存 callerKey 密文")
	}
	if strings.TrimSpace(secret) == "" {
		return fmt.Errorf("secret 不能为空")
	}
	wantName := KeyNameFor(userID, t.ID, t.DisplayName)
	// 终态快检（真幂等）：已有映射名字正确 + 密文能解出目标明文 + ACTIVE → 零写零网络直接返回。
	// （SM4-GCM 带随机 nonce，密文不可比较；判定必须走「解密后比明文」。）
	if k0, lerr0 := c.keys.ByTokenID(ctx, t.ID); lerr0 == nil && k0 != nil && k0.KeyName == wantName && k0.KeyCipher != "" && k0.State == KeyStateActive {
		if plain, derr := crypto.SM4Decrypt(c.secretKey, k0.KeyCipher); derr == nil && string(plain) == secret {
			return nil
		}
	}
	cipher, err := crypto.SM4Encrypt(c.secretKey, []byte(secret))
	if err != nil {
		return fmt.Errorf("加密 callerKey 失败：%w", err)
	}

	k, lerr := c.keys.ByTokenID(ctx, t.ID)
	switch {
	case lerr == nil && k != nil && k.KeyName == wantName:
		// 已是目标名。终态判定：
		//   - 密文与目标明文一致（同一 SM4 密钥密文确定性可比较）→ 完全终态，零写零网络；
		//   - 密文不一致（值轮换/漂移）→ 只对齐代理侧明文与本地密文（不重放 rename）。
		if k.KeyCipher == cipher && k.State == KeyStateActive {
			return nil
		}
		if _, err := c.page.EnsureCallerKey(ctx, k.KeyName, secret); err != nil {
			return err
		}
		if _, err := c.keys.Provision(ctx, t.ID, k.KeyName, cipher); err != nil {
			return fmt.Errorf("保存 callerKey 映射失败：%w", err)
		}
		return nil

	case lerr == nil && k != nil && k.KeyName == LegacyAutoName(t.ID):
		// 改名回填三步（幂等）：① 代理侧原地改名 → ② 确保新名明文 → ③ 本地行改名+激活+留旧名。
		if err := c.page.RenameCallerKey(ctx, k.KeyName, wantName); err != nil {
			return fmt.Errorf("代理侧改名失败（%s → %s）：%w", k.KeyName, wantName, err)
		}
		if _, err := c.page.EnsureCallerKey(ctx, wantName, secret); err != nil {
			return err
		}
		if _, err := c.keys.RenameAndActivate(ctx, t.ID, k.KeyName, wantName, cipher); err != nil {
			return fmt.Errorf("保存 callerKey 映射改名失败：%w", err)
		}
		return nil

	case lerr == nil && k != nil:
		// 手工绑定名：名字不动（不抢运维命名），只确保代理侧同名 key 明文一致 + 本地行 ACTIVE。
		if _, err := c.page.EnsureCallerKey(ctx, k.KeyName, secret); err != nil {
			return err
		}
		if _, err := c.keys.Provision(ctx, t.ID, k.KeyName, cipher); err != nil {
			return fmt.Errorf("保存 callerKey 映射失败：%w", err)
		}
		return nil

	case lerr == ErrKeyNotProvisioned:
		// 无映射：按新名建。
		if _, err := c.page.EnsureCallerKey(ctx, wantName, secret); err != nil {
			return err
		}
		if _, err := c.keys.Provision(ctx, t.ID, wantName, cipher); err != nil {
			return fmt.Errorf("保存 callerKey 映射失败：%w", err)
		}
		return nil

	default:
		return fmt.Errorf("查询 callerKey 映射失败：%w", lerr)
	}
}

// Remove 在代理侧删除该令牌的 callerKey，并把映射置为停用。
func (c *CallerKeySync) Remove(ctx context.Context, tokenID int64) error {
	name := LegacyAutoName(tokenID)
	// 以库里记录的 name 为准（历史可能不同名）。
	if k, err := c.keys.ByTokenID(ctx, tokenID); err == nil && k != nil && k.KeyName != "" {
		name = k.KeyName
	}
	if err := c.page.DeleteCallerKey(ctx, name); err != nil {
		return err
	}
	return c.keys.Disable(ctx, tokenID)
}
