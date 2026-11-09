package channel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channelprotocol"
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
	// ChannelProtocolID 引用的渠道协议 ID（可空）；非空时以渠道协议为准，base_url/protocol 仅在覆盖时填写。
	ChannelProtocolID *int64
}

// TagResolver 由 tag 域经装配层适配实现；channel 不直接依赖 tag 包。
type TagResolver interface {
	ResolveChannelTags(ctx context.Context, ids []int64) ([]TagRef, error)
}

// Service 承载渠道 CRUD 业务逻辑（配置模板，不含凭据）。
type Service struct {
	store       *Store
	cmStore     *channelModelStore     // 渠道内部模型存储（双层模型）；装配时经 SetChannelModelStore 注入
	keys        *KeyService            // 密钥服务：枚举渠道密钥（ListProbeLogs/批量删除联动）；装配时经 SetKeyService 注入
	mgr         *Manager               // 可选：装配时注入，用于动态态同步/内部模型手动状态流转
	tagResolver TagResolver            // 可选：装配时注入，创建/更新渠道时解析并绑定标签
	cpStore     *channelprotocol.Store // 可选：引用渠道协议时校验存在性
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

// SetChannelProtocolStore 注入渠道协议存储：引用渠道协议时校验其存在。
func (s *Service) SetChannelProtocolStore(c *channelprotocol.Store) { s.cpStore = c }

// validateBase 校验渠道基础约束：名称必填；引用渠道协议时 base_url 必须为空、protocol 一律落空，
// 未引用时要求内联 base_url + 基础协议。
//
// 基址归属（0019 裁定①③）：**引用协议不覆盖基址**——基址只跟随被引用协议，需要独立地址就
// 显式新建一条协议。因此引用协议时提交非空 BaseURL 一律 400，且错误文案要能指导操作
// （说清「基址跟随哪条协议」+「要改就新建协议」），不做静默覆盖/静默忽略。
//
// Protocol 是例外（裁定③的边界）：老前端总会回传默认值 openai-compat，若也 400 会拦住一切正常保存；
// 该值在引用模式下从不参与解析，因此**忽略并落空**，不报错。
func (s *Service) validateBase(in ChannelInput) error {
	if in.Name == "" {
		return errBadRequest("渠道名称不能为空")
	}
	if in.ChannelProtocolID != nil && *in.ChannelProtocolID > 0 {
		name := ""
		if s.cpStore != nil {
			cp, err := s.cpStore.GetByID(context.Background(), *in.ChannelProtocolID)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					return errBadRequest("引用的渠道协议不存在")
				}
				return fmt.Errorf("check channel protocol: %w", err)
			}
			if cp != nil {
				name = cp.Name
			}
		}
		if strings.TrimSpace(in.BaseURL) != "" {
			if name != "" {
				return errBadRequest(fmt.Sprintf(
					"引用了渠道协议时不允许设置内联 BaseURL：基址跟随被引用协议（%s）；如需独立基址，请新建一个渠道协议并在渠道中引用它。", name))
			}
			return errBadRequest("引用了渠道协议时不允许设置内联 BaseURL：基址跟随被引用协议；如需独立基址，请新建一个渠道协议并在渠道中引用它。")
		}
		return nil
	}
	// 内联配置：要求 base_url + 基础协议。
	if b := in.BaseURL; b == "" {
		return errBadRequest("渠道 BaseURL 不能为空")
	} else if !strings.HasPrefix(b, "http://") && !strings.HasPrefix(b, "https://") {
		return errBadRequest("BaseURL 必须以 http:// 或 https:// 开头")
	}
	if in.Protocol == "" {
		return errBadRequest("渠道协议不能为空")
	}
	if !channelprotocol.IsBaseProtocol(channelprotocol.Canonical(in.Protocol)) {
		return errBadRequest("协议仅支持 openai-chat / openai-responses / anthropic-messages")
	}
	return nil
}

// inlineProtocol 返回应落库的 channels.protocol：引用协议时恒为空（该值不参与解析，
// 老前端回传的默认值一律忽略并落空，见 validateBase 说明）。
func inlineProtocol(in ChannelInput) string {
	if in.ChannelProtocolID != nil && *in.ChannelProtocolID > 0 {
		return ""
	}
	return in.Protocol
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
	if err := s.validateBase(in); err != nil {
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
		Name:              in.Name,
		Protocol:          inlineProtocol(in),
		BaseURL:           in.BaseURL,
		ChannelProtocolID: in.ChannelProtocolID,
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
	// 引用协议时自动物化 channel_models（仅 externalModelID>0 的行）。
	s.materializeForChannel(ctx, created.ID, in.ChannelProtocolID)
	s.enrichBaseURLEffective(ctx, created)
	return created, nil
}

// UpdateChannel 更新渠道可写字段。
// name 重复 → 40901；渠道不存在 → 40401。
func (s *Service) UpdateChannel(ctx context.Context, id int64, in ChannelInput) (*Channel, error) {
	if err := s.validateBase(in); err != nil {
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

	// 基址归属（0019 裁定①）：引用协议时基址只跟随协议。
	// 存量值已由迁移 0019 归档进 channel_base_url_archive 后清空，因此这里直接落空/落传入值：
	// 引用协议时非空已被 validateBase 拦成 400，落库必然是 ''（不会产生「同协议两种基址」）。
	c := &Channel{
		ID:                id,
		Name:              in.Name,
		Protocol:          inlineProtocol(in),
		BaseURL:           in.BaseURL,
		ChannelProtocolID: in.ChannelProtocolID,
		Tags:              existing.Tags, // tags 列保持不变，不再从 input 设置
		CreatedAt:         existing.CreatedAt,
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
	s.materializeForChannel(ctx, c.ID, in.ChannelProtocolID)
	s.enrichBaseURLEffective(ctx, c)
	return c, nil
}

// enrichBaseURLEffective 填充只读派生字段 BaseURLEffective（列表 / 写操作回包统一口径）。
//
// 取值口径（契约 §5.1，与转发/探测共用同一条链，不分叉）：
//   - 引用协议 → 协议基址（协议级 baseUrl 优先，回落协议 base_url）；channels.base_url 不参与；
//   - 内联渠道 → 自身 base_url。
//
// 运行时就绪时直接读运行时快照（与热路径一致）；否则按同一规则用库里的协议计算（协议按 ID 缓存，避免 N 次查询）。
func (s *Service) enrichBaseURLEffective(ctx context.Context, chs ...*Channel) {
	if len(chs) == 0 {
		return
	}
	cpCache := make(map[int64]*channelprotocol.ChannelProtocol, len(chs))
	for _, c := range chs {
		if c == nil {
			continue
		}
		if s.mgr != nil {
			if rt, ok := s.mgr.GetRuntime(c.ID); ok && rt != nil {
				c.BaseURLEffective = effectiveChannelBaseURL(rt)
				continue
			}
		}
		if c.ChannelProtocolID == nil || *c.ChannelProtocolID <= 0 {
			c.BaseURLEffective = strings.TrimSpace(c.BaseURL)
			continue
		}
		pid := *c.ChannelProtocolID
		cp, cached := cpCache[pid]
		if !cached && s.cpStore != nil {
			cp, _ = s.cpStore.GetByID(ctx, pid) // 协议已删/查询失败 → 保持空，不阻断列表
			cpCache[pid] = cp
		}
		if cp != nil {
			c.BaseURLEffective = cp.EffectiveBaseURL(cp.PreferredProtocol())
		}
	}
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
	// 只读派生：列表页/编辑页展示「生效基址」（此前只有 snapshot 有该字段，列表返回空）。
	enriched := make([]*Channel, 0, len(chs))
	for i := range chs {
		enriched = append(enriched, &chs[i])
	}
	s.enrichBaseURLEffective(ctx, enriched...)
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

// materializeForChannel 渠道引用协议时自动物化 channel_models（Create/Update 成功后调用）。
// 未引用协议、未装配 cmStore/cpStore 或协议已不存在时静默跳过。
func (s *Service) materializeForChannel(ctx context.Context, channelID int64, protocolID *int64) {
	if protocolID == nil || *protocolID <= 0 || s.cmStore == nil || s.cpStore == nil {
		return
	}
	cp, err := s.cpStore.GetByID(ctx, *protocolID)
	if err != nil {
		return
	}
	_, _ = s.materializeFromProtocol(ctx, channelID, cp)
}

// seedFromGateway 把协议种子（channel_protocols.models[m]）转成「只含种子定义了的键」的物化补丁。
// 关键点：种子里没有的键在这里保持 nil/空，物化时不写该列（保留实例现值）——B1 的修法。
func seedFromGateway(mg channelprotocol.ModelGateway) channelModelSeed {
	return channelModelSeed{
		ExternalModelID: mg.ExternalModelID,
		CostRates:       billing.Rates(mg.CostRates),
		TimeConfig:      mg.TimeConfig,
		ContextTiers:    mg.ContextTiers,
		Protocols:       mg.Protocols,
	}
}

// materializeFromProtocol 按协议模型清单物化/更新该渠道的 channel_models（契约 §5.4）：
//   - 仅处理 externalModelID>0 的行；按 (channel_id, internal_model_id) upsert；
//   - 种子缺省的键不写（保留实例现值）——不会再把 cost_rates 写成 JSON null；
//   - edited=true 的行不写商业字段（物化守卫）；
//   - 转换配置（convertEnabled/convertTargets）不物化：裁定②「转换按协议定义」，运行时直接读协议；
//   - 永不覆盖 state / rate_limit / health_probe / reliability，永不删除协议未列出的行。
//
// 返回实际处理的行数。
func (s *Service) materializeFromProtocol(ctx context.Context, channelID int64, cp *channelprotocol.ChannelProtocol) (int, error) {
	if cp == nil || s.cmStore == nil || len(cp.Models) == 0 {
		return 0, nil
	}
	// 固定按内部模型 ID 升序写入，保证物化顺序稳定可测。
	keys := make([]string, 0, len(cp.Models))
	for k := range cp.Models {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	n := 0
	for _, internalID := range keys {
		mg := cp.Models[internalID]
		if mg.ExternalModelID <= 0 {
			continue // 未绑定对外模型的行不物化（D5：channel_models.external_model_id NOT NULL）
		}
		seed := seedFromGateway(mg)
		existing, err := s.cmStore.GetByChannelAndInternal(ctx, channelID, internalID)
		switch {
		case err == nil:
			cols := seed.commercial()
			if existing.ExternalModelID != seed.ExternalModelID {
				cols = append(cols, seedColumn{"external_model_id", seed.ExternalModelID})
			}
			if existing.Edited {
				// 守卫：本地编辑过的行商业字段保持不动（R6）。
				cols = nil
			}
			if uerr := s.cmStore.updateSeedColumns(ctx, existing.ID, cols); uerr != nil {
				return n, fmt.Errorf("update channel model %q: %w", internalID, uerr)
			}
			if fresh, ferr := s.cmStore.GetByID(ctx, existing.ID); ferr == nil && s.mgr != nil {
				s.mgr.UpsertModel(fresh)
			}
		case errors.Is(err, sql.ErrNoRows):
			created, cerr := s.cmStore.InsertFromSeed(ctx, channelID, internalID, seed)
			if cerr != nil {
				return n, fmt.Errorf("insert channel model %q: %w", internalID, cerr)
			}
			if s.mgr != nil {
				s.mgr.UpsertModel(created)
			}
		default:
			return n, fmt.Errorf("get channel model %q: %w", internalID, err)
		}
		n++
	}
	return n, nil
}

// RematerializeProtocol 按协议重物化引用该协议的全部渠道的 channel_models（协议 CRUD/Sync 后刷新）。
// 物化仅 upsert、不删除；保留既有渠道模型状态与运行参数。协议已不存在时视为无目标、正常返回。
func (s *Service) RematerializeProtocol(ctx context.Context, protocolID int64) error {
	if s.cmStore == nil || s.cpStore == nil || protocolID <= 0 {
		return nil
	}
	cp, err := s.cpStore.GetByID(ctx, protocolID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return fmt.Errorf("get channel protocol: %w", err)
	}
	chs, err := s.store.ListByProtocol(ctx, protocolID)
	if err != nil {
		return err
	}
	for i := range chs {
		if _, merr := s.materializeFromProtocol(ctx, chs[i].ID, cp); merr != nil {
			return fmt.Errorf("rematerialize channel %d from protocol %d: %w", chs[i].ID, protocolID, merr)
		}
	}
	return nil
}

// ResetModelFromProtocol 把某条渠道模型行恢复为协议种子值，并清除 edited 标记。
// 这是物化守卫（edited=true 后协议同步不再覆盖）的**唯一出口**，避免「标记后再也同步不了」。
// 种子未定义的键保持现值（无从恢复），转换配置也按协议刷新。
func (s *Service) ResetModelFromProtocol(ctx context.Context, channelID, mid int64) (*ChannelModel, error) {
	if s.cmStore == nil {
		return nil, errInternal()
	}
	m, err := s.cmStore.GetByID(ctx, mid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("渠道内部模型不存在")
		}
		return nil, fmt.Errorf("get channel model: %w", err)
	}
	if m.ChannelID != channelID {
		return nil, errBadRequest("内部模型不属于该渠道")
	}
	ch, err := s.store.Get(ctx, channelID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("渠道不存在")
		}
		return nil, fmt.Errorf("get channel: %w", err)
	}
	if ch.ChannelProtocolID == nil || *ch.ChannelProtocolID <= 0 {
		return nil, errBadRequest("该渠道未引用渠道协议，没有可重置的协议值")
	}
	if s.cpStore == nil {
		return nil, errInternal()
	}
	cp, err := s.cpStore.GetByID(ctx, *ch.ChannelProtocolID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errBadRequest("渠道引用的协议已不存在，无法重置")
		}
		return nil, fmt.Errorf("get channel protocol: %w", err)
	}
	mg, ok := cp.Models[m.InternalModelID]
	if !ok || mg.ExternalModelID <= 0 {
		return nil, errBadRequest("协议中不存在该内部模型的配置，无法重置")
	}
	seed := seedFromGateway(mg)
	cols := seed.commercial()
	cols = append(cols, seedColumn{"external_model_id", mg.ExternalModelID})
	if err := s.cmStore.updateSeedColumns(ctx, mid, cols); err != nil {
		return nil, classifyCMServiceErr(err)
	}
	if err := s.cmStore.SetEdited(ctx, mid, false); err != nil {
		return nil, classifyCMServiceErr(err)
	}
	fresh, err := s.cmStore.GetByID(ctx, mid)
	if err != nil {
		return nil, err
	}
	if s.mgr != nil {
		s.mgr.UpsertModel(fresh)
	}
	return fresh, nil
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
