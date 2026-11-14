// credentials.go 负责「灵枢令牌 → 代理侧 callerKey」的解析。
//
// 代理侧按 callerKey 归集用量（Stats API 的 caller_key_id/caller_key_name），所以凭据必须是
// **一令牌一把**，不能是全局一把：否则所有租户的用量在代理侧混成一本账，第③件的对账无从下手。
// 映射存 magpie_keys（0019 新建，UNIQUE(token_id)），key_cipher 与 tokens.secret_cipher 同口径
// （SM4 加密，密钥来自 security.sm4_key）。
package magpie

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/team/llmgateway/internal/pkg/crypto"
)

// Key 对应 magpie_keys 表的一行（不含明文凭据）。
type Key struct {
	ID        int64
	TokenID   int64
	KeyName   string
	KeyID     string
	KeyCipher string
	State     string
}

// 同步状态常量（与 0019 的 CHECK 约束一致）。
const (
	KeyStateActive   = "ACTIVE"
	KeyStatePending  = "PENDING"
	KeyStateDisabled = "DISABLED"
)

// ErrKeyNotProvisioned 表示该令牌还没有可用的 callerKey（未同步或已停用）。
var ErrKeyNotProvisioned = errors.New("magpie key not provisioned")

// ErrKeyCipherUnavailable 表示凭据密文缺失或解密失败（配置/密钥不匹配）。
var ErrKeyCipherUnavailable = errors.New("magpie key cipher unavailable")

// KeyStore 提供 magpie_keys 的只读访问（写入方是后续的 key 同步任务，不在本期）。
type KeyStore struct {
	db *sql.DB
}

// NewKeyStore 创建 callerKey 映射存储。
func NewKeyStore(db *sql.DB) *KeyStore { return &KeyStore{db: db} }

// ActiveByTokenID 取该令牌当前生效（state=ACTIVE）的 callerKey 映射。
// 无映射返回 ErrKeyNotProvisioned（不返回 nil,nil：调用方必须显式处理「未开通」）。
func (s *KeyStore) ActiveByTokenID(ctx context.Context, tokenID int64) (*Key, error) {
	var k Key
	err := s.db.QueryRowContext(ctx,
		`SELECT id, token_id, key_name, coalesce(key_id,''), coalesce(key_cipher,''), state
		   FROM magpie_keys WHERE token_id = $1 AND state = 'ACTIVE'`, tokenID).
		Scan(&k.ID, &k.TokenID, &k.KeyName, &k.KeyID, &k.KeyCipher, &k.State)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrKeyNotProvisioned
		}
		return nil, fmt.Errorf("query magpie key: %w", err)
	}
	return &k, nil
}

// CallerKey 解密出 callerKey 明文；密文缺失或密钥不匹配一律 ErrKeyCipherUnavailable。
func CallerKey(k *Key, secretKey []byte) (string, error) {
	if k == nil || k.KeyCipher == "" || len(secretKey) == 0 {
		return "", ErrKeyCipherUnavailable
	}
	plain, err := crypto.SM4Decrypt(secretKey, k.KeyCipher)
	if err != nil {
		return "", fmt.Errorf("%w: %v", ErrKeyCipherUnavailable, err)
	}
	if len(plain) == 0 {
		return "", ErrKeyCipherUnavailable
	}
	return string(plain), nil
}
