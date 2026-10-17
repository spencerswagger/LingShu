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
	Input     float64
	Output    float64
	CacheRead float64
	UpdatedAt time.Time
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
// 三段口径下只保留 input/output/cache_read（models.dev 的 cache_write/reasoning 价无对应计费段，忽略）。
type CatalogEntry struct {
	Provider  string  `json:"Provider"`
	ModelID   string  `json:"ModelID"`
	InputUSD  float64 `json:"InputUSD"` // 美元 / 百万 token
	OutputUSD float64 `json:"OutputUSD"`
	CacheUSD  float64 `json:"CacheReadUSD"`
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
	// autoNameExists 跨表重名校验：判断某名称是否已被 auto_models.name 占用
	// （经 SetAutoNameChecker 由装配层注入；nil=跳过该检查）。
	autoNameExists func(ctx context.Context, name string) (bool, error)
	// nameWriteHook 对外模型名/启用态写入后的跨包失效钩子（经 SetNameWriteHook 注入，nil=跳过）。
	// automodel.ResolveMembers 的结果 JOIN 了 external_models 的 external_name/enabled，
	// 故本包任一写路径成功后需回调其读缓存失效（P1-4）。
	nameWriteHook func()
}

// NewService 创建对外模型服务。
func NewService(store *Store) *Service {
	return &Service{store: store}
}

// SetNameWriteHook 注入对外模型写路径后的失效钩子（装配层传入，用于失效 automodel 的跨包读缓存）。
func (s *Service) SetNameWriteHook(fn func()) { s.nameWriteHook = fn }

// notifyNameWrite 在写路径成功后回调失效钩子（nil 时跳过）。
func (s *Service) notifyNameWrite() {
	if s.nameWriteHook != nil {
		s.nameWriteHook()
	}
}

// SetPriceSource 注入 models.dev 价格源（价格参考/应用售价接口使用）。
func (s *Service) SetPriceSource(p PriceSource) { s.pricer = p }

// SetAutoNameChecker 注入「是否与 Auto 模型重名」的校验（装配层传入，查 auto_models）。
func (s *Service) SetAutoNameChecker(fn func(ctx context.Context, name string) (bool, error)) {
	s.autoNameExists = fn
}

// checkAutoName 跨表重名校验（对外模型名不得与任意 Auto 名冲突）；未注入检查器时跳过。
func (s *Service) checkAutoName(ctx context.Context, name string) error {
	if s.autoNameExists == nil {
		return nil
	}
	exists, err := s.autoNameExists(ctx, name)
	if err != nil {
		return fmt.Errorf("check auto name: %w", err)
	}
	if exists {
		return errConflict("模型名已被 Auto 模型占用")
	}
	return nil
}

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

// GetByName 按对外名称查询（忽略 enabled）；无记录返回 sql.ErrNoRows。
// 供 Auto 模型跨表重名校验等场景使用（需检查全部对外模型名，含未启用者）。
func (s *Service) GetByName(ctx context.Context, name string) (*ExternalModel, error) {
	return s.store.GetByName(ctx, name)
}

// validateInput 校验对外模型可写字段：名称非空、售价三段（三键）非负、可选时段/分档合法。
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

// CreateModel 创建对外模型：校验 + 名称唯一 + 跨表重名（Auto）+ 入库。
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
	// 跨表重名：对外模型名不得与任意 Auto 名冲突（与 automodel 侧对称）。
	if err := s.checkAutoName(ctx, in.ExternalName); err != nil {
		return nil, err
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
	// 缓存失效：新增对外名后，GetByExternalName 的名称缓存必须立即刷新（写路径显式失效，无 TTL）。
	s.store.invalidateExternalName(created.ExternalName)
	// 跨包失效：通知 automodel 失效其 ResolveMembers 缓存（结果含 external_name）。
	s.notifyNameWrite()
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
		// 跨表重名：改名后不得与任意 Auto 名冲突。
		if err := s.checkAutoName(ctx, in.ExternalName); err != nil {
			return nil, err
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
	// 缓存失效：外部名/启用态可能变化，旧名与新名两条键都需立即失效（写路径显式失效，无 TTL）。
	s.store.invalidateExternalName(existing.ExternalName)
	s.store.invalidateExternalName(in.ExternalName)
	// 跨包失效：external_name/enabled 变更影响 automodel.ResolveMembers 结果，通知其失效缓存。
	s.notifyNameWrite()
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
	// 缓存失效：批量删除无旧名可依（仅 id），整体清空对外名缓存（写路径显式失效，无 TTL）。
	s.store.invalidateAllExternalNames()
	// 跨包失效：删除使部分 external_name 不再存在，通知 automodel 失效其解析缓存。
	s.notifyNameWrite()
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

// ApplyPrice 把 models.dev 参考价折算后的人民币倍率应用到售价 input/output/cache_read，
// 其余键保持不变，更新入库（三段口径无缓存写/推理段，models.dev 对应价格忽略）。
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
	if err := s.store.ApplySaleRates(ctx, id, m.SaleRates); err != nil {
		return nil, fmt.Errorf("apply sale rates: %w", err)
	}
	// 缓存失效：售价变化会使 GetByExternalName 缓存的模型快照过期，按对外名立即失效（无 TTL）。
	s.store.invalidateExternalName(m.ExternalName)
	// 跨包失效：统一通知 automodel 失效解析缓存（与其它写路径一致，避免口径不一致）。
	s.notifyNameWrite()
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
	Updated []string `json:"Updated"`
	Skipped []string `json:"Skipped"`
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
	// 缓存失效：只要有任何一行售价成功写入，就必须失效一次——用 defer 保证即便中途出错提前
	// return 也会执行（否则已提交的售价变更会因对外名缓存无 TTL 而无限期滞留旧价）。
	// 统一在循环后整体清空一次，避免循环内逐次清空造成写放大；随后跨包失效 automodel 解析缓存。
	invalidated := false
	defer func() {
		if invalidated {
			s.store.invalidateAllExternalNames()
			s.notifyNameWrite()
		}
	}()
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
		if err := s.store.ApplySaleRates(ctx, m.ID, rates); err != nil {
			return nil, fmt.Errorf("apply sale rates for %s: %w", m.ExternalName, err)
		}
		invalidated = true
		res.Updated = append(res.Updated, m.ExternalName)
	}
	return res, nil
}

// SetReadCacheEnabled 开关对外模型名读缓存（默认开启）；关闭后 GetByExternalName 直查 DB。
// 由装配层在启动时按配置设置，属于「直查 fallback 安全开关」。
func (s *Service) SetReadCacheEnabled(on bool) { s.store.SetReadCacheEnabled(on) }
