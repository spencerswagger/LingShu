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

// viewModel 是对外模型的对外展示结构（PascalCase 契约）。
type viewModel struct {
	ID           int64                    `json:"ID,string"`
	ExternalName string                   `json:"ExternalName"`
	Description  string                   `json:"Description"`
	Enabled      bool                     `json:"Enabled"`
	SaleRates    billing.WireRates        `json:"SaleRates"`
	TimeConfig   *billing.TimeCoeffConfig `json:"TimeConfig,omitempty"`
	ContextTiers []billing.TierRule       `json:"ContextTiers,omitempty"`
	CreatedAt    time.Time                `json:"CreatedAt"`
	UpdatedAt    time.Time                `json:"UpdatedAt"`
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
		CreatedAt:    m.CreatedAt,
		UpdatedAt:    m.UpdatedAt,
	}
}

type createModelRequest struct {
	ExternalName string                   `json:"ExternalName"`
	Description  string                   `json:"Description"`
	Enabled      *bool                    `json:"Enabled"`
	SaleRates    billing.WireRates        `json:"SaleRates"`
	TimeConfig   *billing.TimeCoeffConfig `json:"TimeConfig"`
	ContextTiers []billing.TierRule       `json:"ContextTiers"`
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
	}
}

// HandleList GET /api/v1/admin/models（可选 ?Enabled=true|false）
func (h *Handler) HandleList(w http.ResponseWriter, r *http.Request) {
	var enabled *bool
	if v := r.URL.Query().Get("Enabled"); v != "" {
		b := v == "true"
		enabled = &b
	}
	models, err := h.svc.ListModels(r.Context(), enabled)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	list := make([]viewModel, 0, len(models))
	for i := range models {
		list = append(list, toView(&models[i]))
	}
	resp.OK(w, r, list)
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
	resp.OK(w, r, toView(m))
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
	resp.OK(w, r, toView(m))
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
			"Input":      ref.Input,
			"Output":     ref.Output,
			"CacheRead":  ref.CacheRead,
			"CacheWrite": ref.CacheWrite,
			"Reasoning":  ref.Reasoning,
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
	resp.OK(w, r, toView(m))
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
