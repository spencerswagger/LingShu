package channel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

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

// Create 新增密钥：名称/凭据非空校验，SM4 加密后落库（state 由 DDL 默认 NORMAL）。
// 同渠道下名称冲突返回 409。
func (s *KeyService) Create(ctx context.Context, channelID int64, name, credential string) (*ChannelKey, error) {
	if name == "" || credential == "" {
		return nil, errBadRequest("密钥名称与凭据不能为空")
	}
	enc, err := crypto.SM4Encrypt(s.sm4Key, []byte(credential))
	if err != nil {
		return nil, fmt.Errorf("encrypt key credential: %w", err)
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

// Update 修改密钥名称/凭据：credential 为空表示不修改。
// 加密规则与 KeyStore.UpdateInfo(updateCred) 保持一致。
func (s *KeyService) Update(ctx context.Context, keyID int64, name, credential string) (*ChannelKey, error) {
	if name == "" {
		return nil, errBadRequest("密钥名称不能为空")
	}
	updateCred := credential != ""
	var enc string
	if updateCred {
		var err error
		enc, err = crypto.SM4Encrypt(s.sm4Key, []byte(credential))
		if err != nil {
			return nil, fmt.Errorf("encrypt key credential: %w", err)
		}
	}
	if err := s.keys.UpdateInfo(ctx, keyID, name, enc, updateCred); err != nil {
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
