package model

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/idgen"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// Handler 暴露对外模型管理 HTTP 处理器（路由在 server 挂载）。
//
//   - GET     /api/v1/admin/models
//   - POST    /api/v1/admin/models
//   - PUT     /api/v1/admin/models/{id}
//   - POST    /api/v1/admin/models/batch-delete {ids:[]}
//   - GET     /api/v1/admin/models/{id}/price-reference  查看 models.dev 参考价
//   - POST    /api/v1/admin/models/{id}/apply-price      把参考价应用到售价
type Handler struct {
	svc *Service
}

// NewHandler 创建对外模型 HTTP 处理器。
func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// viewModel 是**租户/开发者可见**的模型视图（PascalCase 契约）。
//
// 不包含代理侧映射（magpie_model）：那是内部代号，只给管理端看——
// /api/v1/dev/models 复用同一处理器，若带上就等于把上游映射漏给租户。
type viewModel struct {
	ID           int64                    `json:"ID,string"`
	ExternalName string                   `json:"ExternalName"`
	Description  string                   `json:"Description"`
	Enabled      bool                     `json:"Enabled"`
	SaleRates    billing.WireRates        `json:"SaleRates"`
	TimeConfig   *billing.TimeCoeffConfig `json:"TimeConfig,omitempty"`
	ContextTiers []billing.TierRule       `json:"ContextTiers,omitempty"`
	Aliases      []string                 `json:"Aliases,omitempty"`
	CreatedAt    time.Time                `json:"CreatedAt"`
	UpdatedAt    time.Time                `json:"UpdatedAt"`
}

// adminViewModel 是管理端视图：在对外视图之上补代理侧映射（仅管理端可达）。
type adminViewModel struct {
	viewModel
	// MagpieModel 为代理侧模型名；nil = 未映射（不转发）。
	MagpieModel *string `json:"MagpieModel"`
	// BoundInternalRef 为绑定的内部模型 ref；null = 未绑定（绑定功能未上线，暂不拦截）。
	// 管理端列表必须返回它：前端靠「字段存在且为空」渲染「未绑定」提醒条，
	// 字段缺失时该提醒永不出现，运营就看不到哪些模型还没绑内部模型。
	// 口径：本期只登记与展示，**不做拦截**（未绑定的启用模型仍可被调用，
	// 见 0019_v2_topology.sql 头部「事实校正」）；要改成「未绑定即不售卖」需产品拍板。
	// 无一处 omitempty —— 未绑定要显式给 null，而不是「字段不存在」。
	BoundInternalRef *string `json:"BoundInternalRef"`
}

func toView(m *ExternalModel) viewModel {
	return viewModel{
		ID:           m.ID,
		ExternalName: m.ExternalName,
		Description:  m.Description,
		Enabled:      m.Enabled,
		SaleRates:    billing.WireRates(m.SaleRates),
		TimeConfig:   m.TimeConfig,
		ContextTiers: m.ContextTiers,
		Aliases:      m.Aliases,
		CreatedAt:    m.CreatedAt,
		UpdatedAt:    m.UpdatedAt,
	}
}

// toAdminView 在对外视图上补代理侧映射（只用于管理端响应）。
func toAdminView(m *ExternalModel) adminViewModel {
	return adminViewModel{viewModel: toView(m), MagpieModel: m.MagpieModel, BoundInternalRef: m.BoundInternalRef}
}

type createModelRequest struct {
	ExternalName string                   `json:"ExternalName"`
	Description  string                   `json:"Description"`
	Enabled      *bool                    `json:"Enabled"`
	SaleRates    billing.WireRates        `json:"SaleRates"`
	TimeConfig   *billing.TimeCoeffConfig `json:"TimeConfig"`
	ContextTiers []billing.TierRule       `json:"ContextTiers"`
	Aliases      []string                 `json:"Aliases"`
	// MagpieModel 传空串或 null 表示「不映射」（不转发到代理侧）。
	MagpieModel *string `json:"MagpieModel"`
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return false
	}
	return true
}

func toInput(req *createModelRequest) ExternalModelInput {
	return ExternalModelInput{
		ExternalName: req.ExternalName,
		Description:  req.Description,
		Enabled:      req.Enabled,
		SaleRates:    billing.Rates(req.SaleRates),
		TimeConfig:   req.TimeConfig,
		ContextTiers: req.ContextTiers,
		Aliases:      req.Aliases,
		MagpieModel:  req.MagpieModel,
	}
}

// HandleList GET /api/v1/dev/models —— 租户/开发者可见的模型列表（不含代理侧映射）。
func (h *Handler) HandleList(w http.ResponseWriter, r *http.Request) {
	models, ok := h.listForResponse(w, r)
	if !ok {
		return
	}
	list := make([]viewModel, 0, len(models))
	for i := range models {
		list = append(list, toView(&models[i]))
	}
	resp.OK(w, r, list)
}

// HandleListAdmin GET /api/v1/admin/models —— 管理端列表（含代理侧映射，可读写配置）。
func (h *Handler) HandleListAdmin(w http.ResponseWriter, r *http.Request) {
	models, ok := h.listForResponse(w, r)
	if !ok {
		return
	}
	list := make([]adminViewModel, 0, len(models))
	for i := range models {
		list = append(list, toAdminView(&models[i]))
	}
	resp.OK(w, r, list)
}

// listForResponse 解析 ?Enabled= 并取模型（两个列表入口共用）。
func (h *Handler) listForResponse(w http.ResponseWriter, r *http.Request) ([]ExternalModel, bool) {
	var enabled *bool
	if v := r.URL.Query().Get("Enabled"); v != "" {
		b := v == "true"
		enabled = &b
	}
	models, err := h.svc.ListModels(r.Context(), enabled)
	if err != nil {
		writeServiceErr(w, r, err)
		return nil, false
	}
	return models, true
}

// HandleCreate POST /api/v1/admin/models
func (h *Handler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	var req createModelRequest
	if !decodeBody(w, r, &req) {
		return
	}
	m, err := h.svc.CreateModel(r.Context(), toInput(&req))
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, toAdminView(m))
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "无效的模型 ID")
		return 0, false
	}
	return id, true
}

// HandleUpdate PUT /api/v1/admin/models/{id}
func (h *Handler) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req createModelRequest
	if !decodeBody(w, r, &req) {
		return
	}
	m, err := h.svc.UpdateModel(r.Context(), id, toInput(&req))
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, toAdminView(m))
}

type batchDeleteRequest struct {
	IDs idgen.IDs `json:"IDs"`
}

// HandleBatchDelete POST /api/v1/admin/models/batch-delete
func (h *Handler) HandleBatchDelete(w http.ResponseWriter, r *http.Request) {
	var req batchDeleteRequest
	if !decodeBody(w, r, &req) {
		return
	}
	n, err := h.svc.BatchDeleteModels(r.Context(), []int64(req.IDs))
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]int64{"Deleted": n})
}

// HandlePriceReference GET /api/v1/admin/models/{id}/price-reference
func (h *Handler) HandlePriceReference(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	ref, err := h.svc.PriceReference(r.Context(), id)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]any{
		"Reference": map[string]float64{
			"Input":     ref.Input,
			"Output":    ref.Output,
			"CacheRead": ref.CacheRead,
		},
		"UpdatedAt": ref.UpdatedAt.Format(time.RFC3339),
	})
}

// HandleApplyPrice POST /api/v1/admin/models/{id}/apply-price
func (h *Handler) HandleApplyPrice(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	m, err := h.svc.ApplyPrice(r.Context(), id)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, toAdminView(m))
}

// HandleSyncPrices POST /api/v1/admin/models/sync-prices
func (h *Handler) HandleSyncPrices(w http.ResponseWriter, r *http.Request) {
	res, err := h.svc.SyncPrices(r.Context())
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, res)
}

// HandlePriceCatalog GET /api/v1/admin/models/price-catalog?Q=关键词
// 搜索 models.dev 价格目录（供应商 + 模型 ID + 美元原始价），供前端浏览选择。
// 返回 {List, UpdatedAt, Total}：UpdatedAt 为最近同步时间（从未同步为 0），Total 为命中数。
func (h *Handler) HandlePriceCatalog(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("Q")
	list, updatedAt, total, err := h.svc.PriceCatalog(r.Context(), q)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	if list == nil {
		list = []CatalogEntry{}
	}
	updated := ""
	if !updatedAt.IsZero() {
		updated = updatedAt.Format(time.RFC3339)
	}
	resp.OK(w, r, map[string]any{
		"List":      list,
		"Total":     total,
		"UpdatedAt": updated,
	})
}

func writeServiceErr(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		resp.Err(w, r, apiErr.HTTPStatus, apiErr.Code, apiErr.Message)
		return
	}
	resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "服务器内部错误")
}
