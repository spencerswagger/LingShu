package channel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// ChannelModelInput 创建/更新渠道内部模型的可写字段。
type ChannelModelInput struct {
	InternalModelID string
	ExternalModelID int64
	CostRates       billing.Rates
	TimeConfig      *billing.TimeCoeffConfig
	ContextTiers    []billing.TierRule
	RateLimit       *RateLimitConfig
	HealthProbe     *HealthProbeConfig
	Reliability     *ReliabilityConfig
}

// validateCMInput 校验渠道内部模型：内部ID非空、外部模型绑定、成本五键非负、可选时段/分档合法。
func validateCMInput(channelID int64, in ChannelModelInput) error {
	if channelID <= 0 {
		return errBadRequest("渠道 ID 缺失")
	}
	if in.InternalModelID == "" {
		return errBadRequest("内部模型 ID 不能为空")
	}
	if in.ExternalModelID <= 0 {
		return errBadRequest("必须绑定一个对外模型(external_model_id)")
	}
	if err := billing.ValidateRates(in.CostRates); err != nil {
		return errBadRequest("cost_rates 非法：" + err.Error())
	}
	if in.TimeConfig != nil {
		if err := billing.ValidateTimeConfig(*in.TimeConfig); err != nil {
			return errBadRequest("time_config 非法：" + err.Error())
		}
	}
	if len(in.ContextTiers) > 0 {
		if err := billing.ValidateTiers(in.ContextTiers); err != nil {
			return errBadRequest("context_tiers 非法：" + err.Error())
		}
	}
	return nil
}

func rateOrDefault(r billing.Rates) billing.Rates {
	if r == nil {
		return billing.Rates{"input": 0.8, "output": 1.6, "cache_read": 0.05, "cache_write": 0.2, "reasoning": 0.8}
	}
	return r
}

// CreateChannelModel 为渠道绑定一个内部模型：校验渠道存在 + 字段校验 + 入库。
// 外部模型不存在（外键 23503）→ 40001；渠道不存在 → 40401；(channel, internal) 冲突 → 40901。
func (s *Service) CreateChannelModel(ctx context.Context, channelID int64, in ChannelModelInput) (*ChannelModel, error) {
	if err := validateCMInput(channelID, in); err != nil {
		return nil, err
	}
	if _, err := s.store.Get(ctx, channelID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("渠道不存在")
		}
		return nil, fmt.Errorf("get channel: %w", err)
	}
	if s.cmStore == nil {
		return nil, errInternal()
	}
	m := &ChannelModel{
		ChannelID:       channelID,
		InternalModelID: in.InternalModelID,
		ExternalModelID: in.ExternalModelID,
		CostRates:       rateOrDefault(in.CostRates),
		TimeConfig:      in.TimeConfig,
		ContextTiers:    in.ContextTiers,
		State:           StateNormal,
	}
	if in.RateLimit != nil {
		m.RateLimit = *in.RateLimit
	}
	if in.HealthProbe != nil {
		m.HealthProbe = *in.HealthProbe
	}
	if in.Reliability != nil {
		m.Reliability = *in.Reliability
	}
	created, err := s.cmStore.Insert(ctx, m)
	if err != nil {
		return nil, classifyCMServiceErr(err)
	}
	if s.mgr != nil {
		s.mgr.UpsertModel(created)
	}
	return created, nil
}

// UpdateChannelModel 更新渠道内部模型（修复历史 bug：此前把模型行 ID 当渠道 ID 传，导致“渠道 ID 缺失”）。
// 渠道模型不存在 → 40401；冲突 → 40901。
func (s *Service) UpdateChannelModel(ctx context.Context, channelID, mid int64, in ChannelModelInput) (*ChannelModel, error) {
	if channelID <= 0 {
		return nil, errBadRequest("渠道 ID 缺失")
	}
	if mid <= 0 {
		return nil, errBadRequest("渠道内部模型 ID 缺失")
	}
	if err := validateCMInput(channelID, in); err != nil {
		return nil, err
	}
	if s.cmStore == nil {
		return nil, errInternal()
	}
	existing, err := s.cmStore.GetByID(ctx, mid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("渠道内部模型不存在")
		}
		return nil, fmt.Errorf("get channel model: %w", err)
	}
	if existing.ChannelID != channelID {
		return nil, errBadRequest("内部模型不属于该渠道")
	}
	m := &ChannelModel{
		ID:              mid,
		ChannelID:       existing.ChannelID,
		InternalModelID: in.InternalModelID,
		ExternalModelID: in.ExternalModelID,
		CostRates:       rateOrDefault(in.CostRates),
		TimeConfig:      in.TimeConfig,
		ContextTiers:    in.ContextTiers,
		State:           existing.State, // 状态由状态机/手动操作管理，CRUD 不改变
		RateLimit:       existing.RateLimit,
		HealthProbe:     existing.HealthProbe,
		Reliability:     existing.Reliability,
	}
	if in.RateLimit != nil {
		m.RateLimit = *in.RateLimit
	}
	if in.HealthProbe != nil {
		m.HealthProbe = *in.HealthProbe
	}
	if in.Reliability != nil {
		m.Reliability = *in.Reliability
	}
	if err := s.cmStore.Update(ctx, m); err != nil {
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

// DeleteChannelModel 删除单条渠道内部模型。
func (s *Service) DeleteChannelModel(ctx context.Context, channelID, mid int64) error {
	if s.cmStore == nil {
		return errInternal()
	}
	if err := s.cmStore.Delete(ctx, mid); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound("渠道内部模型不存在")
		}
		return fmt.Errorf("delete channel model: %w", err)
	}
	if s.mgr != nil {
		s.mgr.DropModel(channelID, mid)
	}
	return nil
}

// ListChannelModels 查询某渠道的全部内部模型（含禁用）。
func (s *Service) ListChannelModels(ctx context.Context, channelID int64) ([]ChannelModel, error) {
	if _, err := s.store.Get(ctx, channelID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("渠道不存在")
		}
		return nil, fmt.Errorf("get channel: %w", err)
	}
	if s.cmStore == nil {
		return nil, errInternal()
	}
	return s.cmStore.ListByChannel(ctx, channelID)
}

func classifyCMServiceErr(err error) error {
	if errors.Is(err, ErrChannelModelExists) {
		return errConflict("该渠道已存在相同内部模型 ID")
	}
	if errors.Is(err, ErrExternalModelMissing) {
		return errBadRequest("绑定的对外模型不存在")
	}
	return fmt.Errorf("channel model store: %w", err)
}

// ---- 拉取上游模型 ----

// HTTPDoer 抽象 HTTP 客户端，供 PullModels 可测注入（nil 时用 http.DefaultClient）。
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// PullModel 是上游 /v1/models 返回的单个模型。
type PullModel struct {
	ID      string `json:"id"`
	Object  string `json:"object"`
	OwnedBy string `json:"owned_by"`
}

// PullModelsIndex 是 OpenAI-compat /v1/models 的响应外壳。
type pullModelsIndex struct {
	Data []PullModel `json:"data"`
}

// PullModels 请求渠道的 base_url + /models 拉取模型列表，供管理员选择填入该渠道的内部模型。
// 凭据来源为渠道第一个可用密钥（State 非 DISABLED 且明文非空）的 KeyRuntime.CredentialPlain；
// 无可用密钥 → 50001「暂无可用渠道密钥」；上游 4xx/5xx/网络超时 → 50001（不泄漏上游名）。
// 此接口只返回列表，不落库。
func (s *Service) PullModels(ctx context.Context, channelID int64, doer HTTPDoer) ([]PullModel, error) {
	if s.mgr == nil {
		return nil, errInternal()
	}
	rt, ok := s.mgr.GetRuntime(channelID)
	if !ok {
		return nil, errNotFound("渠道不存在或未加载到运行时")
	}
	cred, ok := firstAvailableKeyCredential(rt)
	if !ok {
		return nil, termsInternal("暂无可用渠道密钥")
	}
	baseURL := strings.TrimRight(rt.Channel.BaseURL, "/")
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/models", nil)
	if err != nil {
		return nil, termsInternal("构造模型拉取请求失败")
	}
	req.Header.Set("Authorization", "Bearer "+cred)

	if doer == nil {
		doer = http.DefaultClient
	}
	resp, err := doer.Do(req)
	if err != nil {
		return nil, termsInternal("拉取渠道模型失败，请稍后重试")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, termsInternal("渠道返回异常，请稍后重试")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, termsInternal("读取渠道模型响应失败")
	}
	var idx pullModelsIndex
	if err := json.Unmarshal(body, &idx); err != nil {
		return nil, termsInternal("渠道模型响应格式无法解析")
	}
	return idx.Data, nil
}

// firstAvailableKeyCredential 返回渠道第一个可用密钥的明文凭据：
// 按密钥 ID 升序遍历 Keys 视图，取第一个 State 非 DISABLED 且明文非空的密钥
// （明文为空即解密失败/未装配，与网关转发语义一致，绝不外发空凭据）。
func firstAvailableKeyCredential(rt *RuntimeChannel) (string, bool) {
	keys := append([]*KeyRuntime(nil), rt.Keys...)
	sort.Slice(keys, func(i, j int) bool { return keys[i].Key.ID < keys[j].Key.ID })
	for _, kr := range keys {
		if kr.Key.State == StateDisabled || kr.CredentialPlain == "" {
			continue
		}
		return kr.CredentialPlain, true
	}
	return "", false
}

// termsInternal 返回 50001 人话错误（不泄漏上游细节）。
func termsInternal(msg string) error {
	return &APIError{HTTPStatus: http.StatusInternalServerError, Code: resp.CodeInternalError, Message: msg}
}
