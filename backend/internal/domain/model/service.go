package model

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
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

// PriceRef 是 models.dev 参考价按 billing.cny_rate 折算后的人民币倍率（人民币 / 百万 token）。
type PriceRef struct {
	Input      float64
	Output     float64
	CacheRead  float64
	CacheWrite float64
	Reasoning  float64
	UpdatedAt  time.Time
}

// PriceSource 供价格参考接口查询 models.dev 缓存价格（由装配层以 sync.Syncer 适配）。
type PriceSource interface {
	ReferencePrice(externalName string) (PriceRef, bool)
	// SearchPrices 按关键词（模型 ID/供应商）搜索价格目录，返回美元/百万 token 原始价。
	SearchPrices(q string) []CatalogEntry
	// LastUpdated 返回最近一次成功同步价格的时间；从未同步返回零值。
	LastUpdated() time.Time
}

// CatalogEntry 价格目录条目（供应商维度），供搜索浏览 models.dev 参考价使用。
type CatalogEntry struct {
	Provider      string  `json:"provider"`
	ModelID       string  `json:"model_id"`
	InputUSD      float64 `json:"input_usd"` // 美元 / 百万 token
	OutputUSD     float64 `json:"output_usd"`
	CacheUSD      float64 `json:"cache_read_usd"`
	CacheWriteUSD float64 `json:"cache_write_usd"`
	ReasoningUSD  float64 `json:"reasoning_usd"`
}

// ExternalModelInput 创建/更新对外模型的可写字段。
type ExternalModelInput struct {
	ExternalName string
	Description  string
	Enabled      *bool
	SaleRates    billing.Rates
	TimeConfig   *billing.TimeCoeffConfig
	ContextTiers []billing.TierRule
}

// Service 承载对外模型 CRUD 与价格应用业务逻辑。
type Service struct {
	store  *Store
	pricer PriceSource
}

// NewService 创建对外模型服务。
func NewService(store *Store) *Service {
	return &Service{store: store}
}

// SetPriceSource 注入 models.dev 价格源（价格参考/应用售价接口使用）。
func (s *Service) SetPriceSource(p PriceSource) { s.pricer = p }

// GetByExternalName 返回指定对外名称的启用模型。无启用模型返回 ErrNotFound。
func (s *Service) GetByExternalName(ctx context.Context, externalName string) (*ExternalModel, error) {
	m, err := s.store.GetByExternalName(ctx, externalName)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return m, nil
}

// GetByID 按主键查询对外模型（探测开销记账等场景用）。
func (s *Service) GetByID(ctx context.Context, id int64) (*ExternalModel, error) {
	return s.store.GetByID(ctx, id)
}

// validateInput 校验对外模型可写字段：名称非空、售价五键非负、可选时段/分档合法。
func (s *Service) validateInput(in ExternalModelInput) error {
	if in.ExternalName == "" {
		return errBadRequest("模型对外名称不能为空")
	}
	if err := billing.ValidateRates(in.SaleRates); err != nil {
		return errBadRequest("sale_rates 非法：" + err.Error())
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

func normalizeEnabled(in *bool, def bool) bool {
	if in != nil {
		return *in
	}
	return def
}

// CreateModel 创建对外模型：校验 + 名称唯一 + 入库。
func (s *Service) CreateModel(ctx context.Context, in ExternalModelInput) (*ExternalModel, error) {
	if err := s.validateInput(in); err != nil {
		return nil, err
	}
	// 名称唯一预检查（友好报错）；store 层 23505 兜底并发冲突。
	if _, err := s.store.GetByName(ctx, in.ExternalName); err == nil {
		return nil, errConflict("模型外部名已存在")
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("check external name: %w", err)
	}

	created, err := s.store.Insert(ctx, &ExternalModel{
		ExternalName: in.ExternalName,
		Description:  in.Description,
		Enabled:      normalizeEnabled(in.Enabled, true),
		SaleRates:    in.SaleRates,
		TimeConfig:   in.TimeConfig,
		ContextTiers: in.ContextTiers,
	})
	if err != nil {
		if errors.Is(err, ErrNameExists) {
			return nil, errConflict("模型外部名已存在")
		}
		return nil, fmt.Errorf("create external model: %w", err)
	}
	return created, nil
}

// UpdateModel 更新对外模型。不存在 → 40401；对外名冲突（改到其它已存在名）→ 40901。
func (s *Service) UpdateModel(ctx context.Context, id int64, in ExternalModelInput) (*ExternalModel, error) {
	if err := s.validateInput(in); err != nil {
		return nil, err
	}
	existing, err := s.store.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("模型不存在")
		}
		return nil, fmt.Errorf("get external model: %w", err)
	}
	if in.ExternalName != existing.ExternalName {
		if other, err := s.store.GetByName(ctx, in.ExternalName); err == nil && other.ID != id {
			return nil, errConflict("模型外部名已存在")
		} else if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("check external name: %w", err)
		}
	}

	m := &ExternalModel{
		ID:           id,
		ExternalName: in.ExternalName,
		Description:  in.Description,
		Enabled:      normalizeEnabled(in.Enabled, existing.Enabled),
		SaleRates:    in.SaleRates,
		TimeConfig:   in.TimeConfig,
		ContextTiers: in.ContextTiers,
	}
	if err := s.store.Update(ctx, m); err != nil {
		if errors.Is(err, ErrNameExists) {
			return nil, errConflict("模型外部名已存在")
		}
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("模型不存在")
		}
		return nil, fmt.Errorf("update external model: %w", err)
	}
	return s.store.GetByID(ctx, id)
}

// ListModels 按 enabled 过滤查询对外模型。
func (s *Service) ListModels(ctx context.Context, enabled *bool) ([]ExternalModel, error) {
	models, err := s.store.List(ctx, enabled)
	if err != nil {
		return nil, fmt.Errorf("list external models: %w", err)
	}
	return models, nil
}

// BatchDeleteModels 批量删除对外模型。
func (s *Service) BatchDeleteModels(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, errBadRequest("未指定要删除的模型")
	}
	n, err := s.store.BatchDelete(ctx, ids)
	if err != nil {
		return 0, fmt.Errorf("batch delete external models: %w", err)
	}
	return n, nil
}

// PriceReference 返回该模型的 models.dev 参考价；缓存无此项返回 40401。
func (s *Service) PriceReference(ctx context.Context, id int64) (*PriceRef, error) {
	m, err := s.store.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("模型不存在")
		}
		return nil, fmt.Errorf("get external model: %w", err)
	}
	if s.pricer == nil {
		return nil, errInternal()
	}
	ref, ok := s.pricer.ReferencePrice(m.ExternalName)
	if !ok {
		return nil, errNotFound("models.dev 尚无该模型价格")
	}
	return &ref, nil
}

// ApplyPrice 把 models.dev 参考价折算后的人民币倍率应用到售价 input/output 及缓存读写/推理，其余键保持不变，更新入库。
func (s *Service) ApplyPrice(ctx context.Context, id int64) (*ExternalModel, error) {
	m, err := s.store.GetByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, errNotFound("模型不存在")
		}
		return nil, fmt.Errorf("get external model: %w", err)
	}
	if s.pricer == nil {
		return nil, errInternal()
	}
	ref, ok := s.pricer.ReferencePrice(m.ExternalName)
	if !ok {
		return nil, errNotFound("models.dev 尚无该模型价格")
	}
	if m.SaleRates == nil {
		m.SaleRates = billing.Rates{}
	}
	if ref.Input > 0 {
		m.SaleRates["input"] = ref.Input
	}
	if ref.Output > 0 {
		m.SaleRates["output"] = ref.Output
	}
	if ref.CacheRead > 0 {
		m.SaleRates["cache_read"] = ref.CacheRead
	}
	if ref.CacheWrite > 0 {
		m.SaleRates["cache_write"] = ref.CacheWrite
	}
	if ref.Reasoning > 0 {
		m.SaleRates["reasoning"] = ref.Reasoning
	}
	if err := s.store.ApplySaleRates(ctx, id, m.SaleRates); err != nil {
		return nil, fmt.Errorf("apply sale rates: %w", err)
	}
	return s.store.GetByID(ctx, id)
}

// PriceCatalog 按关键词搜索 models.dev 价格目录；返回条目列表与同步元信息。
// 无价格源返回空列表与零值时间。
func (s *Service) PriceCatalog(ctx context.Context, q string) ([]CatalogEntry, time.Time, int, error) {
	if s.pricer == nil {
		return []CatalogEntry{}, time.Time{}, 0, nil
	}
	list := s.pricer.SearchPrices(q)
	return list, s.pricer.LastUpdated(), len(list), nil
}

// SyncPricesResult 批量同步结果。
type SyncPricesResult struct {
	Updated []string `json:"updated"`
	Skipped []string `json:"skipped"`
}

// SyncPrices 用 models.dev 参考价折算后的人民币倍率批量刷新所有对外模型的售价；
// models.dev 中无参考价的模型跳过。
func (s *Service) SyncPrices(ctx context.Context) (*SyncPricesResult, error) {
	if s.pricer == nil {
		return nil, errInternal()
	}
	models, err := s.store.List(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("list external models: %w", err)
	}
	res := &SyncPricesResult{Updated: []string{}, Skipped: []string{}}
	for _, m := range models {
		ref, ok := s.pricer.ReferencePrice(m.ExternalName)
		if !ok {
			res.Skipped = append(res.Skipped, m.ExternalName)
			continue
		}
		rates := m.SaleRates
		if rates == nil {
			rates = billing.Rates{}
		}
		if ref.Input > 0 {
			rates["input"] = ref.Input
		}
		if ref.Output > 0 {
			rates["output"] = ref.Output
		}
		if ref.CacheRead > 0 {
			rates["cache_read"] = ref.CacheRead
		}
		if ref.CacheWrite > 0 {
			rates["cache_write"] = ref.CacheWrite
		}
		if ref.Reasoning > 0 {
			rates["reasoning"] = ref.Reasoning
		}
		if err := s.store.ApplySaleRates(ctx, m.ID, rates); err != nil {
			return nil, fmt.Errorf("apply sale rates for %s: %w", m.ExternalName, err)
		}
		res.Updated = append(res.Updated, m.ExternalName)
	}
	return res, nil
}
