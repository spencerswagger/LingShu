package magpie

// callerKeySync 把 identity.TokenService 的 CallerKeySyncer 接到：
//   1) magpie 页面接口（在代理侧真正建/删/改名 callerKey）
//   2) magpie_keys 表（灵枢侧 token ↔ callerKey 映射与密文）
// 令牌明文 = callerKey 明文（灵枢生成；magpie add-key 支持自带 secret）。
//
// V3 改名批（契约定稿 §1.1/终表 §3，2026-11-20）：存储名 = 「用户名转写-令牌名转写」
// （keyname.go 唯一生成点），重名由 EnsureNameUnique 尾号消歧（分配后固化不回收）。
// 改名走 magpie 的 rename-key 端点（真机实测：ID/secret/限额不变，客户端零感知）；
// 本地 magpie_keys 原行改 key_name 并把旧名存进 legacy_key_name（结算反查兜底）。
//
// 改名资格（不许凭名字猜测/样本特判）：仅当当前名 == 历史自动名格式
// ls-token-<tokenID>（旧代码唯一生成点）时才自动改名；其它名字一律视为运维
// 手工绑定——**名字保持不动**，但执行态 Ensure 仍会把代理侧同名 key 的值收敛
// 为该令牌当前明文（值对齐，改名资格≠零动作）。是否连值轮换也跳过属口径
// 拍板项（见 PR/任务书），拍板前 dry-run 报 manual-named、执行照常值对齐。
import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/pkg/crypto"
)

// CallerKeyStore 是同步需要的映射存储能力（*KeyStore 满足；测试可替身）。
type CallerKeyStore interface {
	KeyBinder
	NameOccupied(ctx context.Context, keyName string, excludeTokenID int64) (bool, error)
	RenameRollbackTargets(ctx context.Context, limit int) ([]Key, error)
	RenameAndActivate(ctx context.Context, tokenID int64, oldName, newName, keyCipher string) (*Key, error)
	ClearLegacyName(ctx context.Context, tokenID int64) error
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

// ErrNameUnavailable 表示消歧重试一轮后仍撞名（契约：409 挂起人工，不自动放大轮次）。
// 哨兵实体在 identity.ErrCallerKeyNameTaken（identity 不能 import magpie，
// 回填层按 errors.Is 判定后映射 409）；此处为既有调用点保留别名。
var ErrNameUnavailable = identity.ErrCallerKeyNameTaken

// ErrKeyDisabled 表示该令牌的映射行已停用（令牌停用时 Remove 删过代理侧 key）。
// Ensure（自动路径）对停用行返回它、不碰代理侧、不翻状态；显式启用走 Reactivate。
// 哨兵实体在 identity.ErrCallerKeyDisabled（同 ErrNameUnavailable 的循环依赖口径）。
var ErrKeyDisabled = identity.ErrCallerKeyDisabled

// LegacyAutoName 是旧口径自动名（ls-token-<id>，旧代码唯一生成点）。
// 改名资格判定与 Remove 的无行兜底都用它。
func LegacyAutoName(tokenID int64) string { return fmt.Sprintf("ls-token-%d", tokenID) }

// EnsureNameUnique 为该令牌分配最终存储名：基础名不撞直接用；
// 撞了按 -2/-3… 顺序找空位（消歧一轮 maxTries 个候选，都不空 → ErrNameUnavailable）。
// 已绑定行（same token）的名字不算撞（excludeTokenID）。分配后固化：同一令牌重跑得同名。
func (c *CallerKeySync) EnsureNameUnique(ctx context.Context, userID, tokenID int64, username, displayName string) (string, error) {
	base := KeyNameFor(userID, tokenID, username, displayName)
	occupied, err := c.keys.NameOccupied(ctx, base, tokenID)
	if err != nil {
		return "", err
	}
	if !occupied {
		return base, nil
	}
	for i := 2; i < maxDisambiguationTries+2; i++ {
		cand := fmt.Sprintf("%s-%d", base, i)
		occupied, err := c.keys.NameOccupied(ctx, cand, tokenID)
		if err != nil {
			return "", err
		}
		if !occupied {
			return cand, nil
		}
	}
	return "", ErrNameUnavailable
}

const maxDisambiguationTries = 8

// Ensure 确保该令牌在代理侧的 callerKey 为「按新口径分配的名 + 给定明文」（终态 ACTIVE）：
//   - 无映射 → 分配名字（含消歧）并按新名建；
//   - 有映射且名字已是终态名 → 幂等确保（明文对齐 + 映射行 ACTIVE）；
//   - 有映射且是历史自动名 ls-token-<id> → 消歧分配新名，magpie 侧 rename-key 原地改名
//     （值不变），本地行改名并记 legacy_key_name（改名回填三步，幂等可重跑）；
//   - 有映射且是其它名字（运维手工绑定）→ 名字不动，只确保代理侧同名 key 存在且明文一致。
//
// 状态纪律（评审必改①）：DISABLED 行一律拒绝（ErrKeyDisabled），不碰代理侧、不翻状态。
// 此前两洞：①手工名/终态名分支 Provision upsert 会把停用行翻回 ACTIVE 并重建代理 key
// （Toggle 刚删的被搬回）；②旧自动名分支先 rename 代理侧、本地 RenameAndActivate 被
// state<>'DISABLED' 拒 → 本地永远追不上、每轮重试死循环。
// 显式重新启用（Toggle 启用 / 管理端一键开通）走 Reactivate。
//
// 幂等：重复执行要么命中「已是终态」提前收敛，要么从当前实际状态继续推进；
// 顺序是 magpie 侧先改名、本地后改（任何一步中断，重跑都能从实际状态续上）。
func (c *CallerKeySync) Ensure(ctx context.Context, userID int64, t TokenRef, username, secret string) error {
	if len(c.secretKey) == 0 {
		return fmt.Errorf("未配置 SM4 密钥，无法保存 callerKey 密文")
	}
	if strings.TrimSpace(secret) == "" {
		return fmt.Errorf("secret 不能为空")
	}
	// 终态名字 = 既有终态名（手工名/已分配名）或本次消歧分配的名。
	// 先取映射再定名：无映射时才做「分配」动作（分配会写 magpie_keys 时固化）。
	k, lerr := c.keys.ByTokenID(ctx, t.ID)
	if lerr != nil && !errors.Is(lerr, ErrKeyNotProvisioned) {
		return fmt.Errorf("查询 callerKey 映射失败：%w", lerr)
	}
	// 停用行守卫：在任何代理侧动作（ensure/rename）与本地写（Provision 翻状态）之前。
	if k != nil && k.State == KeyStateDisabled {
		return ErrKeyDisabled
	}
	var wantName string
	switch {
	case k == nil:
		name, nerr := c.EnsureNameUnique(ctx, userID, t.ID, username, t.DisplayName)
		if nerr != nil {
			return nerr
		}
		wantName = name
	case k.KeyName == LegacyAutoName(t.ID):
		name, nerr := c.EnsureNameUnique(ctx, userID, t.ID, username, t.DisplayName)
		if nerr != nil {
			return nerr
		}
		wantName = name
	default:
		wantName = k.KeyName // 终态名（手工绑定名或已分配名），名字不动。
	}

	// 终态快检（真幂等）：映射名字正确 + 密文能解出目标明文 + ACTIVE → 零写零网络直接返回。
	// （SM4-GCM 带随机 nonce，密文不可比较；判定必须走「解密后比明文」。）
	if k != nil && k.KeyName == wantName && k.KeyCipher != "" && k.State == KeyStateActive {
		if plain, derr := crypto.SM4Decrypt(c.secretKey, k.KeyCipher); derr == nil && string(plain) == secret {
			return nil
		}
	}
	cipher, err := crypto.SM4Encrypt(c.secretKey, []byte(secret))
	if err != nil {
		return fmt.Errorf("加密 callerKey 失败：%w", err)
	}

	switch {
	case k == nil:
		// 无映射：按分配名建。
		if _, err := c.page.EnsureCallerKey(ctx, wantName, secret); err != nil {
			return err
		}
		if _, err := c.keys.Provision(ctx, t.ID, wantName, cipher); err != nil {
			return fmt.Errorf("保存 callerKey 映射失败：%w", err)
		}
		return nil

	case k.KeyName == wantName:
		// 已是终态名：只对齐代理侧明文与本地密文（不重放 rename/分配）。
		if _, err := c.page.EnsureCallerKey(ctx, k.KeyName, secret); err != nil {
			return err
		}
		if _, err := c.keys.Provision(ctx, t.ID, k.KeyName, cipher); err != nil {
			return fmt.Errorf("保存 callerKey 映射失败：%w", err)
		}
		return nil

	case k.KeyName == LegacyAutoName(t.ID):
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

	default:
		// 手工绑定名：名字不动（不抢运维命名），只确保代理侧同名 key 明文一致 + 本地行 ACTIVE。
		if _, err := c.page.EnsureCallerKey(ctx, k.KeyName, secret); err != nil {
			return err
		}
		if _, err := c.keys.Provision(ctx, t.ID, k.KeyName, cipher); err != nil {
			return fmt.Errorf("保存 callerKey 映射失败：%w", err)
		}
		return nil
	}
}

// Reactivate 显式重新启用该令牌的 callerKey 映射（Toggle 启用侧 / 管理端一键开通专用）。
// 与 Ensure 的唯一差别：允许把 DISABLED 行翻回 ACTIVE —— 自动路径（存量回填/启动补建）
// 一律走 Ensure（停用行拒绝），只有用户/管理员显式动作可以恢复映射。
//
// 顺序与其它分支一致（代理侧先行）：先确保代理侧**当前名**的 key 存在，本地行翻回 ACTIVE
// （不抬名）；随后复用 Ensure 推进剩余收敛（旧自动名 → 改名三步；明文/密文对齐）。
// 幂等：重复执行命中 Ensure 的终态快检零写返回。
func (c *CallerKeySync) Reactivate(ctx context.Context, userID int64, t TokenRef, username, secret string) error {
	if len(c.secretKey) == 0 {
		return fmt.Errorf("未配置 SM4 密钥，无法保存 callerKey 密文")
	}
	if strings.TrimSpace(secret) == "" {
		return fmt.Errorf("secret 不能为空")
	}
	k, lerr := c.keys.ByTokenID(ctx, t.ID)
	if lerr != nil && !errors.Is(lerr, ErrKeyNotProvisioned) {
		return fmt.Errorf("查询 callerKey 映射失败：%w", lerr)
	}
	if lerr == nil && k != nil && k.State == KeyStateDisabled {
		cipher, err := crypto.SM4Encrypt(c.secretKey, []byte(secret))
		if err != nil {
			return fmt.Errorf("加密 callerKey 失败：%w", err)
		}
		// 先代理后本地：代理侧当前名 key 存在后，本地行才翻 ACTIVE（不抬名）。
		if _, err := c.page.EnsureCallerKey(ctx, k.KeyName, secret); err != nil {
			return err
		}
		if _, err := c.keys.Provision(ctx, t.ID, k.KeyName, cipher); err != nil {
			return fmt.Errorf("恢复 callerKey 映射失败：%w", err)
		}
	}
	// 行已 ACTIVE（或本就未停用）：走常规收敛（改名/对齐/终态快检）。
	return c.Ensure(ctx, userID, t, username, secret)
}

// ProbeName 预演「该令牌此刻执行 Ensure 会分到什么名」：终态名（手工绑定/已分配）原样返回；
// 无映射或旧自动名则跑与 Ensure 相同的消歧分配。撞名消歧耗尽 → ErrCallerKeyNameTaken
// （identity 哨兵）。零写零代理侧动作（只读本地行 + 名字占用查询）——rename 端点
// dry-run 探 Conflict 用；dry-run 因此能给出与执行态一致的冲突预览。
func (c *CallerKeySync) ProbeName(ctx context.Context, userID int64, t TokenRef, username string) (string, error) {
	k, lerr := c.keys.ByTokenID(ctx, t.ID)
	if lerr != nil && !errors.Is(lerr, ErrKeyNotProvisioned) {
		return "", fmt.Errorf("查询 callerKey 映射失败：%w", lerr)
	}
	if k != nil && k.State == KeyStateDisabled {
		return "", ErrKeyDisabled
	}
	if k == nil || k.KeyName == LegacyAutoName(t.ID) {
		return c.EnsureNameUnique(ctx, userID, t.ID, username, t.DisplayName)
	}
	return k.KeyName, nil
}

// RollbackToken 把该令牌的映射按 legacy_key_name 复原旧名（终表 §3.5：三步与正向镜像、可幂等重跑）：
//
//	① magpie 侧 rename-key（新名 → 旧名，值不变）→ ② 确保旧名明文 → ③ 本地行复原
//	   （key_name=旧名、legacy_key_name=''）。幂等：重复执行结果一致；无旧名（未改名过）
//	   直接跳过（不算失败）。
func (c *CallerKeySync) RollbackToken(ctx context.Context, tokenID int64) (bool, error) {
	if len(c.secretKey) == 0 {
		return false, fmt.Errorf("未配置 SM4 密钥，无法保存 callerKey 密文")
	}
	k, err := c.keys.ByTokenID(ctx, tokenID)
	if err != nil {
		if errors.Is(err, ErrKeyNotProvisioned) {
			return false, nil // 无映射：无事可做（幂等）。
		}
		return false, fmt.Errorf("查询 callerKey 映射失败：%w", err)
	}
	if k.LegacyKeyName == "" {
		return false, nil // 未改名过：无旧名可复原（幂等，不报错）。
	}
	legacy := k.LegacyKeyName
	if k.KeyName == legacy {
		return true, nil // 已复原（重跑命中）。
	}
	plain := ""
	if k.KeyCipher != "" {
		if p, derr := crypto.SM4Decrypt(c.secretKey, k.KeyCipher); derr == nil {
			plain = string(p)
		}
	}
	if plain == "" {
		return false, fmt.Errorf("令牌 %d 的 callerKey 密文不可用，回滚中止", tokenID)
	}
	if err := c.page.RenameCallerKey(ctx, k.KeyName, legacy); err != nil {
		return false, fmt.Errorf("代理侧回滚改名失败（%s → %s）：%w", k.KeyName, legacy, err)
	}
	if _, err := c.page.EnsureCallerKey(ctx, legacy, plain); err != nil {
		return false, err
	}
	if _, err := c.keys.RenameAndActivate(ctx, tokenID, k.KeyName, legacy, k.KeyCipher); err != nil {
		return false, fmt.Errorf("保存 callerKey 映射回滚失败：%w", err)
	}
	// RenameAndActivate 会把 legacy 写成旧 key_name；这里清空 legacy（复原完成）。
	if err := c.keys.ClearLegacyName(ctx, tokenID); err != nil {
		return false, fmt.Errorf("清理 legacy_key_name 失败：%w", err)
	}
	return true, nil
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
