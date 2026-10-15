package channel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/team/llmgateway/internal/pkg/idgen"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// APIError 是带 HTTP 状态码与业务码的领域错误，handler 据此返回响应。
type APIError struct {
	HTTPStatus int
	Code       int
	Message    string
}

func (e *APIError) Error() string { return e.Message }

func errBadRequest(msg string) *APIError {
	return &APIError{HTTPStatus: http.StatusBadRequest, Code: resp.CodeBadRequest, Message: msg}
}
func errConflict(msg string) *APIError {
	return &APIError{HTTPStatus: http.StatusConflict, Code: resp.CodeConflict, Message: msg}
}
func errNotFound(msg string) *APIError {
	return &APIError{HTTPStatus: http.StatusNotFound, Code: resp.CodeNotFound, Message: msg}
}
func errInternal() *APIError {
	return &APIError{HTTPStatus: http.StatusInternalServerError, Code: resp.CodeInternalError, Message: "服务器内部错误"}
}

// Action* 手动状态动作（渠道与内部模型共用）。
const (
	ActionNormal  = "normal"  // 手动置正常
	ActionDrain   = "drain"   // 手动置排空
	ActionDisable = "disable" // 手动置禁用（终态，仅手动退出）
)

// ChannelInput 创建/更新渠道的可写字段。凭据由 channel_keys（密钥管理）承载，渠道层不再持有。
// 权重已废弃并删除数据库列（0005 迁移）：同优先级内改由「成本最低 + 存活会话数」决定。
type ChannelInput struct {
	Name        string
	Protocol    string
	BaseURL     string
	TagIDs      []int64
	Priority    int
	RateLimit   *RateLimitConfig
	HealthProbe *HealthProbeConfig
	Reliability *ReliabilityConfig
}

// TagResolver 由 tag 域经装配层适配实现；channel 不直接依赖 tag 包。
type TagResolver interface {
	ResolveChannelTags(ctx context.Context, ids []int64) ([]TagRef, error)
}

// Service 承载渠道 CRUD 业务逻辑（配置模板，不含凭据）。
type Service struct {
	store       *Store
	cmStore     *channelModelStore // 渠道内部模型存储（双层模型）；装配时经 SetChannelModelStore 注入
	keys        *KeyService        // 密钥服务：枚举渠道密钥（ListProbeLogs/批量删除联动）；装配时经 SetKeyService 注入
	mgr         *Manager           // 可选：装配时注入，用于动态态同步/内部模型手动状态流转
	tagResolver TagResolver        // 可选：装配时注入，创建/更新渠道时解析并绑定标签
}

// NewService 创建渠道服务。
func NewService(store *Store) *Service {
	return &Service{store: store}
}

// SetManager 注入运行时管理器，供创建/更新同步动态态、删除/强状态流转委托。
func (s *Service) SetManager(m *Manager) { s.mgr = m }

// SetChannelModelStore 注入渠道内部模型存储（双层模型）。
func (s *Service) SetChannelModelStore(c *channelModelStore) { s.cmStore = c }

// SetKeyService 注入密钥服务；枚举渠道密钥（ListProbeLogs 等）时依赖它。
func (s *Service) SetKeyService(k *KeyService) { s.keys = k }

// SetTagResolver 注入标签解析器。
func (s *Service) SetTagResolver(r TagResolver) { s.tagResolver = r }

// validateBase 校验 name/base_url/protocol 基础约束。
func validateBase(name, baseURL, protocol string) error {
	if name == "" {
		return errBadRequest("渠道名称不能为空")
	}
	if baseURL == "" {
		return errBadRequest("渠道 BaseURL 不能为空")
	}
	if !strings.HasPrefix(baseURL, "http://") && !strings.HasPrefix(baseURL, "https://") {
		return errBadRequest("BaseURL 必须以 http:// 或 https:// 开头")
	}
	if protocol != ProtocolOpenAICompat {
		return errBadRequest("协议目前仅支持 openai-compat")
	}
	return nil
}

// fillDefaults 补齐默认值：priority=100、rate_limit/health_probe/reliability 默认。
func fillDefaults(c *Channel, in ChannelInput) {
	c.Priority = 100
	c.RateLimit = DefaultRateLimit()
	c.HealthProbe = DefaultHealthProbe()
	c.Reliability = DefaultReliability()
	if in.Priority > 0 {
		c.Priority = in.Priority
	}
	if in.RateLimit != nil {
		c.RateLimit = normalizeRateLimit(*in.RateLimit)
	}
	if in.HealthProbe != nil {
		c.HealthProbe = normalizeHealthProbe(*in.HealthProbe)
	}
	if in.Reliability != nil {
		c.Reliability = normalizeReliability(*in.Reliability)
	}
}

// CreateChannel 创建渠道：校验 + 唯一性 + 默认值。凭据由渠道密钥（channel_keys）承载，不在本层录入。
func (s *Service) CreateChannel(ctx context.Context, in ChannelInput) (*Channel, error) {
	if err := validateBase(in.Name, in.BaseURL, in.Protocol); err != nil {
		return nil, err
	}
	// 名称唯一预检查（友好报错）；store 层 23505 兜底并发冲突。
	if _, err := s.store.GetByName(ctx, in.Name); err == nil {
		return nil, errConflict("渠道名称已存在")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check channel name: %w", err)
	}

	var resolved []TagRef
	if s.tagResolver != nil {
		r, rerr := s.tagResolver.ResolveChannelTags(ctx, in.TagIDs)
		if rerr != nil {
			return nil, rerr
		}
		resolved = r
	}

	c := &Channel{
		Name:     in.Name,
		Protocol: in.Protocol,
		BaseURL:  in.BaseURL,
	}
	fillDefaults(c, in)

	created, err := s.store.InsertWithTags(ctx, c, in.TagIDs)
	if err != nil {
		if errors.Is(err, ErrNameExists) {
			return nil, errConflict("渠道名称已存在")
		}
		return nil, fmt.Errorf("create channel: %w", err)
	}
	created.TagIDs = idgen.IDs(in.TagIDs)
	created.BoundTags = resolved

	if s.mgr != nil {
		upsert := *created
		s.mgr.Upsert(&upsert)
		s.mgr.SetBoundTags(created.ID, created.BoundTags)
	}
	return created, nil
}

// UpdateChannel 更新渠道可写字段。
// name 重复 → 40901；渠道不存在 → 40401。
func (s *Service) UpdateChannel(ctx context.Context, id int64, in ChannelInput) (*Channel, error) {
	if err := validateBase(in.Name, in.BaseURL, in.Protocol); err != nil {
		return nil, err
	}
	existing, err := s.store.Get(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("渠道不存在")
		}
		return nil, fmt.Errorf("get channel: %w", err)
	}

	var resolved []TagRef
	if s.tagResolver != nil {
		r, rerr := s.tagResolver.ResolveChannelTags(ctx, in.TagIDs)
		if rerr != nil {
			return nil, rerr
		}
		resolved = r
	}

	c := &Channel{
		ID:        id,
		Name:      in.Name,
		Protocol:  in.Protocol,
		BaseURL:   in.BaseURL,
		Tags:      existing.Tags, // tags 列保持不变，不再从 input 设置
		CreatedAt: existing.CreatedAt,
	}
	c.RateLimit = existing.RateLimit
	c.HealthProbe = existing.HealthProbe
	c.Reliability = existing.Reliability
	if in.RateLimit != nil {
		c.RateLimit = normalizeRateLimit(*in.RateLimit)
	}
	if in.HealthProbe != nil {
		c.HealthProbe = normalizeHealthProbe(*in.HealthProbe)
	}
	if in.Reliability != nil {
		c.Reliability = normalizeReliability(*in.Reliability)
	}
	if in.Priority > 0 {
		c.Priority = in.Priority
	} else {
		c.Priority = existing.Priority
	}

	if err := s.store.UpdateWithTags(ctx, c, in.TagIDs); err != nil {
		if errors.Is(err, ErrNameExists) {
			return nil, errConflict("渠道名称已存在")
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("渠道不存在")
		}
		return nil, fmt.Errorf("update channel: %w", err)
	}
	c.TagIDs = idgen.IDs(in.TagIDs)
	c.BoundTags = resolved

	if s.mgr != nil {
		upsert := *c
		s.mgr.Upsert(&upsert)
		s.mgr.SetBoundTags(c.ID, c.BoundTags)
	}
	return c, nil
}

// ListChannels 查询全部渠道，并附带逐密钥聚合态 KeyStates。
// 聚合源为注入 Manager 的 KeyViews（内存运行时，状态为准；mgr 未装配时 KeyStates 为空，
// 前端显示「无密钥」）。渠道无独立状态，其可用性以逐密钥状态呈现。
func (s *Service) ListChannels(ctx context.Context) ([]Channel, error) {
	chs, err := s.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("list channels: %w", err)
	}
	if s.mgr != nil {
		for i := range chs {
			if views := s.mgr.KeyViews(chs[i].ID); len(views) > 0 {
				chs[i].KeyStates = make([]ChannelKeyState, 0, len(views))
				for _, v := range views {
					chs[i].KeyStates = append(chs[i].KeyStates, ChannelKeyState{
						KeyID:   v.KeyID,
						KeyName: v.Name,
						State:   v.State,
					})
				}
			}
		}
	}
	ids := make([]int64, 0, len(chs))
	for i := range chs {
		ids = append(ids, chs[i].ID)
	}
	if refs, err := s.store.ListTagRefsByChannels(ctx, ids); err == nil {
		for i := range chs {
			r := refs[chs[i].ID]
			chs[i].BoundTags = r
			chs[i].TagIDs = make(idgen.IDs, 0, len(r))
			for _, tr := range r {
				chs[i].TagIDs = append(chs[i].TagIDs, tr.ID)
			}
		}
	}
	return chs, nil
}

// DeleteChannel 删除单个渠道（软删除；历史账单记录保留，审计不丢失）。
func (s *Service) DeleteChannel(ctx context.Context, id int64) error {
	n, err := s.store.DeleteChannels(ctx, []int64{id})
	if err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}
	if n == 0 {
		return errNotFound("渠道不存在")
	}
	if s.mgr != nil {
		s.mgr.Drop(id)
	}
	return nil
}

// BatchDeleteChannels 批量删除渠道。
func (s *Service) BatchDeleteChannels(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, errBadRequest("未指定要删除的渠道")
	}
	n, err := s.store.DeleteChannels(ctx, ids)
	if err != nil {
		return 0, fmt.Errorf("batch delete channels: %w", err)
	}
	if s.mgr != nil {
		for _, id := range ids {
			s.mgr.Drop(id)
		}
	}
	return n, nil
}

// ModelForceState 手动设置内部模型状态（normal/drain/disable），写模型事件。
func (s *Service) ModelForceState(ctx context.Context, channelID, mid int64, action, reason string) (*ChannelModel, error) {
	if s.mgr == nil {
		return nil, errInternal()
	}
	if s.cmStore == nil {
		return nil, errInternal()
	}
	st, ok := actionState(action)
	if !ok {
		return nil, errBadRequest("action 只允许 normal / drain / disable")
	}
	if reason == "" {
		reason = "manual " + action
	}
	if err := s.mgr.ManualSetModelState(channelID, mid, st); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("渠道内部模型不存在")
		}
		return nil, fmt.Errorf("force model state: %w", err)
	}
	m, err := s.cmStore.GetByID(ctx, mid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("渠道内部模型不存在")
		}
		return nil, fmt.Errorf("get channel model: %w", err)
	}
	return m, nil
}

func actionState(action string) (State, bool) {
	switch action {
	case ActionNormal:
		return StateNormal, true
	case ActionDrain:
		return StateDrain, true
	case ActionDisable:
		return StateDisabled, true
	}
	return "", false
}

// ListModelEvents 查询某渠道的模型状态流转记录。
func (s *Service) ListModelEvents(ctx context.Context, channelID int64) ([]ChannelModelEvent, error) {
	if s.cmStore == nil {
		return nil, errInternal()
	}
	if _, err := s.store.Get(ctx, channelID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("渠道不存在")
		}
		return nil, fmt.Errorf("get channel: %w", err)
	}
	return s.cmStore.ListModelEvents(ctx, channelID)
}

// ListProbeLogs 查询某渠道的探测历史（按渠道聚合）：
// 先取该渠道全部未删除密钥 ID（KeyService.List），再按 key 集合 ANY 查询 probe_logs（合并列表，保留 channel_key_id）。
// 修复：原先直接把路径 channel_id 当 channel_key_id 查询（恒为空）；依赖注入的 KeyService 枚举密钥。
func (s *Service) ListProbeLogs(ctx context.Context, channelID int64, limit int) ([]ProbeLog, error) {
	if s.cmStore == nil || s.keys == nil {
		return nil, errInternal()
	}
	if _, err := s.store.Get(ctx, channelID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("渠道不存在")
		}
		return nil, fmt.Errorf("get channel: %w", err)
	}
	keys, err := s.keys.List(ctx, channelID)
	if err != nil {
		return nil, fmt.Errorf("list channel keys: %w", err)
	}
	keyIDs := make([]int64, 0, len(keys))
	for i := range keys {
		keyIDs = append(keyIDs, keys[i].ID)
	}
	return s.cmStore.ListProbeLogsByKeys(ctx, keyIDs, limit)
}
