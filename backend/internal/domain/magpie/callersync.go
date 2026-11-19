package magpie

// callerKeySync 把 identity.TokenService 的 CallerKeySyncer 接到：
//   1) magpie 页面接口（在代理侧真正建/删 callerKey）
//   2) magpie_keys 表（灵枢侧 token ↔ callerKey 映射与密文）
// 令牌明文 = callerKey 明文（灵枢生成；magpie add-key 支持自带 secret）。
import (
	"context"
	"fmt"

	"github.com/team/llmgateway/internal/pkg/crypto"
)

// CallerKeySync 实现 identity.CallerKeySyncer。
type CallerKeySync struct {
	page      *PageAdmin
	keys      *KeyStore
	secretKey []byte
}

func NewCallerKeySync(page *PageAdmin, keys *KeyStore, secretKey []byte) *CallerKeySync {
	return &CallerKeySync{page: page, keys: keys, secretKey: secretKey}
}

func keyNameFor(tokenID int64) string { return fmt.Sprintf("ls-token-%d", tokenID) }

// Ensure 在代理侧确保该令牌的 callerKey 存在且为给定明文，并在 magpie_keys 落映射。
func (c *CallerKeySync) Ensure(ctx context.Context, tokenID int64, secret string) error {
	if len(c.secretKey) == 0 {
		return fmt.Errorf("未配置 SM4 密钥，无法保存 callerKey 密文")
	}
	name := keyNameFor(tokenID)
	if _, err := c.page.EnsureCallerKey(ctx, name, secret); err != nil {
		return err
	}
	cipher, err := crypto.SM4Encrypt(c.secretKey, []byte(secret))
	if err != nil {
		return fmt.Errorf("加密 callerKey 失败：%w", err)
	}
	if _, err := c.keys.Provision(ctx, tokenID, name, cipher); err != nil {
		return fmt.Errorf("保存 callerKey 映射失败：%w", err)
	}
	return nil
}

// Remove 在代理侧删除该令牌的 callerKey，并把映射置为停用。
func (c *CallerKeySync) Remove(ctx context.Context, tokenID int64) error {
	name := keyNameFor(tokenID)
	// 以库里记录的 name 为准（历史可能不同名）。
	if k, err := c.keys.ByTokenID(ctx, tokenID); err == nil && k != nil && k.KeyName != "" {
		name = k.KeyName
	}
	if err := c.page.DeleteCallerKey(ctx, name); err != nil {
		return err
	}
	return c.keys.Disable(ctx, tokenID)
}
