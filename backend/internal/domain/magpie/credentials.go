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
	"strings"

	"github.com/team/llmgateway/internal/pkg/crypto"
	"github.com/team/llmgateway/internal/pkg/idgen"
)

// Key 对应 magpie_keys 表的一行（不含明文凭据）。
type Key struct {
	ID            int64
	TokenID       int64
	KeyName       string
	LegacyKeyName string // 改名前的旧名（结算按名反查兜底；未改过名为空串）
	KeyID         string
	KeyCipher     string
	State         string
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

// KeyStore 提供 magpie_keys 的读写。//
// 写入方 = 管理端绑定端点（keyadmin.go）：代理侧的 callerKey 由运维在 magpie 侧创建，
// 灵枢只负责「落库 + 加密保存 + 一令牌一把」的映射，不自行签发凭据
// （自造一把 magpie 不认的 key 只会把失败推迟到转发时）。
type KeyStore struct {
	db *sql.DB
}

// NewKeyStore 创建 callerKey 映射存储。
func NewKeyStore(db *sql.DB) *KeyStore { return &KeyStore{db: db} }

// keyScanCols 是 magpie_keys 查询列（含 0030 的 legacy_key_name）。
const keyScanCols = `id, token_id, key_name, coalesce(legacy_key_name,''), coalesce(key_id,''), coalesce(key_cipher,''), state`

// NamesByTokenIDs 批量取令牌当前映射的 callerKey 名（管理端令牌列表展示用；
// 只读，不区分 state——绑定端点语义是「查看映射」，停用的映射名字照样显示）。
// 无映射的令牌不出现在返回 map 里（展示层按空串渲染）。
func (s *KeyStore) NamesByTokenIDs(ctx context.Context, tokenIDs []int64) (map[int64]string, error) {
	out := make(map[int64]string, len(tokenIDs))
	if len(tokenIDs) == 0 {
		return out, nil
	}
	const batchSize = 500
	for start := 0; start < len(tokenIDs); start += batchSize {
		end := start + batchSize
		if end > len(tokenIDs) {
			end = len(tokenIDs)
		}
		batch := tokenIDs[start:end]
		// IN ($1..$n) 而不是 ANY($1::text[])：sqlmock 不支持切片参数（生产 pgx 可以）。
		placeholders := make([]string, len(batch))
		args := make([]any, len(batch))
		for i, id := range batch {
			placeholders[i] = fmt.Sprintf("$%d", i+1)
			args[i] = id
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT token_id, key_name FROM magpie_keys WHERE token_id IN (`+strings.Join(placeholders, ",")+`)`,
			args...)
		if err != nil {
			return nil, fmt.Errorf("query magpie key names: %w", err)
		}
		for rows.Next() {
			var id int64
			var name string
			if err := rows.Scan(&id, &name); err != nil {
				rows.Close()
				return nil, fmt.Errorf("scan magpie key name: %w", err)
			}
			out[id] = name
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("iterate magpie key names: %w", err)
		}
		rows.Close()
	}
	return out, nil
}

func scanKey(row interface{ Scan(...any) error }) (*Key, error) {
	var k Key
	if err := row.Scan(&k.ID, &k.TokenID, &k.KeyName, &k.LegacyKeyName, &k.KeyID, &k.KeyCipher, &k.State); err != nil {
		return nil, err
	}
	return &k, nil
}

// ActiveByTokenID 取该令牌当前生效（state=ACTIVE）的 callerKey 映射。
// 无映射返回 ErrKeyNotProvisioned（不返回 nil,nil：调用方必须显式处理「未开通」）。
func (s *KeyStore) ActiveByTokenID(ctx context.Context, tokenID int64) (*Key, error) {
	k, err := scanKey(s.db.QueryRowContext(ctx,
		`SELECT `+keyScanCols+` FROM magpie_keys WHERE token_id = $1 AND state = 'ACTIVE'`, tokenID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrKeyNotProvisioned
		}
		return nil, fmt.Errorf("query magpie key: %w", err)
	}
	return k, nil
}

// ByTokenID 取该令牌的映射（不要求 ACTIVE，用于管理端查看/复核）。
func (s *KeyStore) ByTokenID(ctx context.Context, tokenID int64) (*Key, error) {
	k, err := scanKey(s.db.QueryRowContext(ctx,
		`SELECT `+keyScanCols+` FROM magpie_keys WHERE token_id = $1`, tokenID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrKeyNotProvisioned
		}
		return nil, fmt.Errorf("query magpie key: %w", err)
	}
	return k, nil
}

// NameOccupied 查该存储名是否已被其它令牌占用（重命名消歧用；调用方把
// tokenID 传 0 或未绑定的 ID 即查全表语义——占用的行属于「其它令牌」才返回 true）。
// 名字同时出现在 key_name 与 legacy_key_name 都算占用：消歧要避开的不只是
// 当前名，还有历史名（否则消歧结果会撞上某令牌的旧名，回滚时无法复原）。
func (s *KeyStore) NameOccupied(ctx context.Context, keyName string, excludeTokenID int64) (bool, error) {
	var n int
	if err := s.db.QueryRowContext(ctx, `
		SELECT count(*) FROM magpie_keys
		 WHERE (key_name = $1 OR legacy_key_name = $1) AND token_id <> $2 AND state <> 'DISABLED'`,
		keyName, excludeTokenID).Scan(&n); err != nil {
		return false, fmt.Errorf("query key name occupation: %w", err)
	}
	return n > 0, nil
}

// RenameRollbackTargets 列出「旧名仍是 legacy_key_name」的映射行（回滚演练/执行的数据源）。
// 只取 ACTIVE 行：停用行的旧名复原无意义（代理侧 key 已删）。
func (s *KeyStore) RenameRollbackTargets(ctx context.Context, limit int) ([]Key, error) {
	if limit <= 0 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT `+keyScanCols+` FROM magpie_keys
		 WHERE state = 'ACTIVE' AND legacy_key_name <> ''
		 ORDER BY updated_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, fmt.Errorf("query rename rollback targets: %w", err)
	}
	defer rows.Close()
	var out []Key
	for rows.Next() {
		k, err := scanKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// Provision 写入/更新该令牌的 callerKey 映射并置为 ACTIVE（幂等：唯一键是 token_id）。
//
// keyName 冲突（另一令牌已占用同名 callerKey，key_name 有 UNIQUE）由调用方翻成 409。
func (s *KeyStore) Provision(ctx context.Context, tokenID int64, keyName, keyCipher string) (*Key, error) {
	// Provision 不动 legacy_key_name：首建保持默认空串；改名走 RenameAndActivate（它负责写旧名）。
	k, err := scanKey(s.db.QueryRowContext(ctx,
		`INSERT INTO magpie_keys(id, token_id, key_name, key_cipher, state, last_synced_at)
		 VALUES ($1, $2, $3, $4, 'ACTIVE', now())
		 ON CONFLICT (token_id) DO UPDATE
		   SET key_name = EXCLUDED.key_name, key_cipher = EXCLUDED.key_cipher,
		       state = 'ACTIVE', last_synced_at = now(), last_error = NULL, updated_at = now()
		 RETURNING `+keyScanCols,
		idgen.New(), tokenID, keyName, keyCipher))
	if err != nil {
		return nil, err
	}
	return k, nil
}

// Disable 把该令牌的映射置为 DISABLED（保留行，便于复核与后续重新启用）。
func (s *KeyStore) Disable(ctx context.Context, tokenID int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE magpie_keys SET state = 'DISABLED', updated_at = now() WHERE token_id = $1`, tokenID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrKeyNotProvisioned
	}
	return nil
}

// Rename 原地改名该令牌的映射：key_name 换新名，旧名落 legacy_key_name（结算反查兜底）。
// 行不存在返回 ErrKeyNotProvisioned。幂等：重复执行结果一致。
//
// 前提：代理侧已 rename 成功（调用方顺序保证）；单语句原子。
func (s *KeyStore) Rename(ctx context.Context, tokenID int64, oldName, newName string) (*Key, error) {
	k, err := scanKey(s.db.QueryRowContext(ctx,
		`UPDATE magpie_keys SET key_name = $3, legacy_key_name = $2, updated_at = now()
		 WHERE token_id = $1 AND state <> 'DISABLED'
		 RETURNING `+keyScanCols, tokenID, oldName, newName))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrKeyNotProvisioned
		}
		return nil, fmt.Errorf("rename magpie key: %w", err)
	}
	return k, nil
}

// RenameAndActivate = Rename + 保存密文并置 ACTIVE（改名回填第③步的完整语义）。
// 幂等：重复执行同值收敛；停用行（DISABLED）不抬升（启用走 Provision）。
func (s *KeyStore) RenameAndActivate(ctx context.Context, tokenID int64, oldName, newName, keyCipher string) (*Key, error) {
	k, err := scanKey(s.db.QueryRowContext(ctx,
		`UPDATE magpie_keys SET key_name = $3, legacy_key_name = $2, key_cipher = $4,
		        state = 'ACTIVE', last_synced_at = now(), last_error = NULL, updated_at = now()
		 WHERE token_id = $1 AND state <> 'DISABLED'
		 RETURNING `+keyScanCols, tokenID, oldName, newName, keyCipher))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrKeyNotProvisioned
		}
		return nil, fmt.Errorf("rename+activate magpie key: %w", err)
	}
	return k, nil
}

// ClearLegacyName 清空该令牌映射行的 legacy_key_name（回滚复原完成的收尾步；
// 回滚后旧名回到 key_name，旧名不再是「历史名」）。行不存在返回 ErrKeyNotProvisioned。
func (s *KeyStore) ClearLegacyName(ctx context.Context, tokenID int64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE magpie_keys SET legacy_key_name = '', updated_at = now() WHERE token_id = $1`, tokenID)
	if err != nil {
		return fmt.Errorf("clear legacy key name: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrKeyNotProvisioned
	}
	return nil
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
