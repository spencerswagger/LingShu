package channel

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"

	"github.com/team/llmgateway/internal/pkg/crypto"
)

// ActionRecover 手动恢复动作：目标 NORMAL，与 ActionNormal 仅事件 reason 不同。
const ActionRecover = "recover"

// keyActionState 手动动作 → 目标状态。密钥级与渠道级批量共用同一张映射，避免两处定义产生二义。
var keyActionState = map[string]State{
	ActionNormal:  StateNormal,
	ActionDrain:   StateDrain,
	ActionDisable: StateDisabled,
	ActionRecover: StateNormal,
}

// keyActionReason 手动动作 → 密钥事件 reason，与 state.go 的 manual_* 常量保持一致；
// recover 为恢复动作，reason 用 "manual recover"。
var keyActionReason = map[string]string{
	ActionNormal:  reasonManualNormal,
	ActionDrain:   reasonManualDrain,
	ActionDisable: reasonManualDisable,
	ActionRecover: "manual recover",
}

// KeyService 承载渠道密钥（channel_keys/channel_key_events）的 CRUD 与状态流转语义。
// 凭据 SM4 加密落库；状态流转同时落库并记录事件。不接运行时：内存 KeyRuntime 的
// 同步归 Task B1，本服务只负责数据库层语义。
type KeyService struct {
	keys   *KeyStore
	sm4Key []byte
}

// NewKeyService 创建密钥服务。sm4Key 为凭据加密密钥（16 字节），与渠道 Service 共用。
func NewKeyService(keys *KeyStore, sm4Key []byte) *KeyService {
	return &KeyService{keys: keys, sm4Key: sm4Key}
}

// randUint32 生成随机 32 位无符号整数，用于拼装默认密钥名。
// crypto/rand 读取失败极罕见，此时退化为 0，仍保证名称格式合法。
func randUint32() uint32 {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

// newDefaultKeyName 生成渠道密钥默认名（名称留空时使用）：形如 key-1a2b3c4d。
func newDefaultKeyName() string {
	return fmt.Sprintf("key-%08x", randUint32())
}

// Create 新增密钥：凭据必填，名称可留空（留空时生成随机默认名），SM4 加密后落库。
// 名称留空生成的默认名在同渠道冲突时重新生成重试（最多 5 次），仍失败返回 409；
// 名称非空时同渠道冲突同样返回 409。
func (s *KeyService) Create(ctx context.Context, channelID int64, name, credential string) (*ChannelKey, error) {
	if credential == "" {
		return nil, errBadRequest("密钥凭据不能为空")
	}
	enc, err := crypto.SM4Encrypt(s.sm4Key, []byte(credential))
	if err != nil {
		return nil, fmt.Errorf("encrypt key credential: %w", err)
	}
	name = strings.TrimSpace(name)
	if name == "" {
		for i := 0; i < 5; i++ {
			id, err := s.keys.Insert(ctx, ChannelKey{ChannelID: channelID, Name: newDefaultKeyName(), CredentialEnc: enc})
			if err == nil {
				return s.keys.GetByID(ctx, id)
			}
			if !errors.Is(err, ErrNameExists) {
				return nil, err
			}
		}
		return nil, errConflict("该渠道下密钥名称已存在")
	}
	id, err := s.keys.Insert(ctx, ChannelKey{ChannelID: channelID, Name: name, CredentialEnc: enc})
	if err != nil {
		if errors.Is(err, ErrNameExists) {
			return nil, errConflict("该渠道下密钥名称已存在")
		}
		return nil, err
	}
	return s.keys.GetByID(ctx, id)
}

// Get 按 ID 查询密钥（含归属渠道），供 HTTP 层做「路径渠道 ↔ 密钥」归属一致性校验。
func (s *KeyService) Get(ctx context.Context, keyID int64) (*ChannelKey, error) {
	k, err := s.keys.GetByID(ctx, keyID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("密钥不存在")
		}
		return nil, err
	}
	return k, nil
}

// Update 修改密钥名称/凭据：名称留空表示不改名，凭据留空表示不改凭据；
// 两者都为空则不做更新，直接读取并返回当前记录。名称冲突返回 409。
// 加密规则与 KeyStore.UpdateInfo(updateCred) 保持一致。
func (s *KeyService) Update(ctx context.Context, keyID int64, name, credential string) (*ChannelKey, error) {
	name = strings.TrimSpace(name)
	updateCred := credential != ""
	if name == "" && !updateCred {
		// 名称与凭据都为空：不改动，直接返回当前记录。
		return s.keys.GetByID(ctx, keyID)
	}
	effectiveName := name
	if effectiveName == "" {
		// 名称为空 = 不改名：取当前名称用于回写，避免置空。
		cur, err := s.keys.GetByID(ctx, keyID)
		if err != nil {
			return nil, err
		}
		effectiveName = cur.Name
	}
	var enc string
	if updateCred {
		var err error
		enc, err = crypto.SM4Encrypt(s.sm4Key, []byte(credential))
		if err != nil {
			return nil, fmt.Errorf("encrypt key credential: %w", err)
		}
	}
	if err := s.keys.UpdateInfo(ctx, keyID, effectiveName, enc, updateCred); err != nil {
		if errors.Is(err, ErrNameExists) {
			return nil, errConflict("该渠道下密钥名称已存在")
		}
		return nil, err
	}
	return s.keys.GetByID(ctx, keyID)
}

// Delete 软删除密钥（历史账单/会话保留名称关联）。
func (s *KeyService) Delete(ctx context.Context, keyID int64) error {
	return s.keys.SoftDelete(ctx, keyID)
}

// List 枚举某渠道全部未删除密钥。
func (s *KeyService) List(ctx context.Context, channelID int64) ([]ChannelKey, error) {
	return s.keys.List(ctx, channelID)
}

// ForceState 切换单个密钥状态：
// GetByID → 同态直接返回；否则 SetState 落库并插一条 channel_key_events。
// action ∈ normal/drain/disable/recover，reason 分别为 manual_normal/manual_drain/manual_disable/manual recover。
func (s *KeyService) ForceState(ctx context.Context, keyID int64, action string) (*ChannelKey, error) {
	to, ok := keyActionState[action]
	if !ok {
		return nil, errBadRequest("action 只允许 normal / drain / disable / recover")
	}
	k, err := s.keys.GetByID(ctx, keyID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("密钥不存在")
		}
		return nil, err
	}
	if k.State == to {
		return k, nil
	}
	old := k.State
	if err := s.keys.SetState(ctx, keyID, to, ""); err != nil {
		return nil, err
	}
	if err := s.keys.InsertEvent(ctx, keyID, old, to, keyActionReason[action]); err != nil {
		return nil, err
	}
	return s.keys.GetByID(ctx, keyID)
}

// ListEvents 查询密钥状态流转事件（created_at 倒序）。
func (s *KeyService) ListEvents(ctx context.Context, keyID int64, limit int) ([]ChannelKeyEvent, error) {
	return s.keys.ListEvents(ctx, keyID, limit)
}
