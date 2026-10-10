package channel

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/cronx"
	"github.com/team/llmgateway/internal/pkg/idgen"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// Handler 暴露渠道管理 HTTP 处理器（路由由 Task 8 挂载）。
//
// 渠道：
//   - GET    /api/v1/admin/channels
//   - POST   /api/v1/admin/channels
//   - PUT    /api/v1/admin/channels/{id}
//   - POST   /api/v1/admin/channels/batch-delete {ids:[]}
//   - GET    /api/v1/admin/channels/{id}/events
//   - POST   /api/v1/admin/channels/{id}/state {action:"recover"|"disable"}
//
// 渠道内部模型（双层模型）：
//   - GET    /api/v1/admin/channels/{id}/models
//   - POST   /api/v1/admin/channels/{id}/models
//   - PUT    /api/v1/admin/channels/{id}/models/{mid}
//   - DELETE /api/v1/admin/channels/{id}/models/{mid}
//   - POST   /api/v1/admin/channels/{id}/models/pull
//
// 渠道密钥（channel_keys 运行时实体，需 SetKeyDeps 注入 KeyService + Manager）：
//   - GET    /api/v1/admin/channels/{id}/keys
//   - POST   /api/v1/admin/channels/{id}/keys
//   - PUT    /api/v1/admin/channels/{id}/keys/{kid}
//   - DELETE /api/v1/admin/channels/{id}/keys/{kid}
//   - POST   /api/v1/admin/channels/{id}/keys/{kid}/state {action}
type Handler struct {
	svc          *Service
	operatorIDFn func(ctx context.Context) (int64, bool)
	keySvc       *KeyService
	mgr          *Manager
	killKeySess  func(keyID int64) int // 密钥删除联动会话清理（装配层注入；nil=跳过）
}

// NewHandler 创建渠道 HTTP 处理器。operatorIDFn 从鉴权上下文取操作者 ID（管理员手动状态切换记录到 channel_events）。
func NewHandler(svc *Service, operatorIDFn func(ctx context.Context) (int64, bool)) *Handler {
	if operatorIDFn == nil {
		operatorIDFn = func(ctx context.Context) (int64, bool) { return 0, false }
	}
	return &Handler{svc: svc, operatorIDFn: operatorIDFn}
}

// SetKeyDeps 注入密钥服务与运行时管理器（Task D2：KeyService 负责落库，成功后在 Manager 同步内存运行时）。
func (h *Handler) SetKeyDeps(keySvc *KeyService, mgr *Manager) {
	h.keySvc = keySvc
	h.mgr = mgr
}

// SetSessionKiller 注入密钥删除时的会话清理钩子（装配层提供 SessionRegistry.KillByFilter 适配；
// channel 包不直接依赖 router，避免导入环）。nil 表示跳过会话清理。
func (h *Handler) SetSessionKiller(fn func(keyID int64) int) {
	h.killKeySess = fn
}

type createChannelRequest struct {
	Name              string             `json:"Name"`
	Protocol          string             `json:"Protocol"`
	BaseURL           string             `json:"BaseURL"`
	TagIDs            idgen.IDs          `json:"TagIDs"`
	Priority          int                `json:"Priority"`
	Weight            int                `json:"Weight"`
	RateLimit         *RateLimitConfig   `json:"RateLimit"`
	HealthProbe       *HealthProbeConfig `json:"HealthProbe"`
	Reliability       *ReliabilityConfig `json:"Reliability"`
	SessionTTLMinutes int                `json:"SessionTTLMinutes"`
}

func decodeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return false
	}
	return true
}

func pathID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "无效的渠道 ID")
		return 0, false
	}
	return id, true
}

// requireKeyOwnership 校验密钥 kid 归属路径中的渠道 channelID（资源-路径一致性）。
// 不满足返回 404，避免泄露跨渠道密钥的存在性；keyDepsReady 已确保 keySvc 就绪。
func (h *Handler) requireKeyOwnership(w http.ResponseWriter, r *http.Request, channelID, kid int64) bool {
	k, err := h.keySvc.Get(r.Context(), kid)
	if err != nil {
		writeServiceErr(w, r, err)
		return false
	}
	if k.ChannelID != channelID {
		writeServiceErr(w, r, errNotFound("密钥不存在"))
		return false
	}
	return true
}

func toInput(req *createChannelRequest) ChannelInput {
	return ChannelInput{
		Name:              req.Name,
		Protocol:          req.Protocol,
		BaseURL:           req.BaseURL,
		TagIDs:            []int64(req.TagIDs),
		Priority:          req.Priority,
		Weight:            req.Weight,
		RateLimit:         req.RateLimit,
		HealthProbe:       req.HealthProbe,
		Reliability:       req.Reliability,
		SessionTTLMinutes: req.SessionTTLMinutes,
	}
}

// HandleList GET /api/v1/admin/channels
func (h *Handler) HandleList(w http.ResponseWriter, r *http.Request) {
	// 可选过滤：?State=NORMAL|DRAIN|DISABLED
	state := State(r.URL.Query().Get("State"))

	chs, err := h.svc.ListChannels(r.Context(), state)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, chs)
}

// HandleCreate POST /api/v1/admin/channels
func (h *Handler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	var req createChannelRequest
	if !decodeBody(w, r, &req) {
		return
	}
	ch, err := h.svc.CreateChannel(r.Context(), toInput(&req))
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, ch)
}

// HandleUpdate PUT /api/v1/admin/channels/{id}
func (h *Handler) HandleUpdate(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req createChannelRequest
	if !decodeBody(w, r, &req) {
		return
	}
	ch, err := h.svc.UpdateChannel(r.Context(), id, toInput(&req))
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, ch)
}

type batchDeleteRequest struct {
	IDs idgen.IDs `json:"IDs"`
}

// HandleBatchDelete POST /api/v1/admin/channels/batch-delete
func (h *Handler) HandleBatchDelete(w http.ResponseWriter, r *http.Request) {
	var req batchDeleteRequest
	if !decodeBody(w, r, &req) {
		return
	}
	// 删除前收集各渠道未删密钥 ID：渠道软删后其密钥随之软删，无法再查询。
	keyIDsByChannel := make(map[int64][]int64, len(req.IDs))
	if h.keySvc != nil {
		for _, id := range req.IDs {
			if ks, err := h.keySvc.List(r.Context(), id); err == nil {
				ids := make([]int64, 0, len(ks))
				for i := range ks {
					ids = append(ids, ks[i].ID)
				}
				keyIDsByChannel[id] = ids
			}
		}
	}
	n, err := h.svc.BatchDeleteChannels(r.Context(), []int64(req.IDs))
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	// 渠道删除联动清理其密钥的存量会话（内存+DB），与单密钥删除语义一致。
	if h.killKeySess != nil {
		for _, ids := range keyIDsByChannel {
			for _, kid := range ids {
				h.killKeySess(kid)
			}
		}
	}
	resp.OK(w, r, map[string]int64{"Deleted": n})
}

// HandleListEvents GET /api/v1/admin/channels/{id}/events
func (h *Handler) HandleListEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	evs, err := h.svc.ListEvents(r.Context(), id)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, evs)
}

type stateRequest struct {
	Action string `json:"Action"` // normal | drain | disable
	Reason string `json:"Reason,omitempty"`
}

// HandleState POST /api/v1/admin/channels/{id}/state
func (h *Handler) HandleState(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req stateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	operatorID, _ := h.operatorIDFn(r.Context())
	ch, err := h.svc.ForceState(r.Context(), id, req.Action, operatorID, req.Reason)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, ch)
}

// writeServiceErr 将 channel.Service 返回的错误映射为统一响应。
func writeServiceErr(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		resp.Err(w, r, apiErr.HTTPStatus, apiErr.Code, apiErr.Message)
		return
	}
	resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "服务器内部错误")
}

// ===== 渠道内部模型（双层模型） =====

// viewChannelModel 是渠道内部模型的对外展示结构（含 JOIN 出的对外模型名与运行态）。
type viewChannelModel struct {
	ID              int64                    `json:"ID,string"`
	ChannelID       int64                    `json:"ChannelID,string"`
	InternalModelID string                   `json:"InternalModelID"`
	ExternalModelID int64                    `json:"ExternalModelID,string"`
	ExternalName    string                   `json:"ExternalName,omitempty"`
	ChannelName     string                   `json:"ChannelName,omitempty"`
	CostRates       billing.WireRates        `json:"CostRates"`
	TimeConfig      *billing.TimeCoeffConfig `json:"TimeConfig,omitempty"`
	ContextTiers    []billing.TierRule       `json:"ContextTiers,omitempty"`
	State           State                    `json:"State"`
	RateLimit       RateLimitConfig          `json:"RateLimit"`
	HealthProbe     HealthProbeConfig        `json:"HealthProbe"`
	Reliability     ReliabilityConfig        `json:"Reliability"`
	CreatedAt       time.Time                `json:"CreatedAt"`
	UpdatedAt       time.Time                `json:"UpdatedAt"`
}

func toChannelModelView(m *ChannelModel) viewChannelModel {
	return viewChannelModel{
		ID:              m.ID,
		ChannelID:       m.ChannelID,
		InternalModelID: m.InternalModelID,
		ExternalModelID: m.ExternalModelID,
		ExternalName:    m.ExternalName,
		ChannelName:     m.ChannelName,
		CostRates:       billing.WireRates(m.CostRates),
		TimeConfig:      m.TimeConfig,
		ContextTiers:    m.ContextTiers,
		State:           m.State,
		RateLimit:       m.RateLimit,
		HealthProbe:     m.HealthProbe,
		Reliability:     m.Reliability,
		CreatedAt:       m.CreatedAt,
		UpdatedAt:       m.UpdatedAt,
	}
}

type channelModelRequest struct {
	InternalModelID string                   `json:"InternalModelID"`
	ExternalModelID int64                    `json:"ExternalModelID,string"`
	CostRates       billing.WireRates        `json:"CostRates"`
	TimeConfig      *billing.TimeCoeffConfig `json:"TimeConfig"`
	ContextTiers    []billing.TierRule       `json:"ContextTiers"`
	RateLimit       *RateLimitConfig         `json:"RateLimit"`
	HealthProbe     *HealthProbeConfig       `json:"HealthProbe"`
	Reliability     *ReliabilityConfig       `json:"Reliability"`
}

func (req *channelModelRequest) toInput() ChannelModelInput {
	return ChannelModelInput{
		InternalModelID: req.InternalModelID,
		ExternalModelID: req.ExternalModelID,
		CostRates:       billing.Rates(req.CostRates),
		TimeConfig:      req.TimeConfig,
		ContextTiers:    req.ContextTiers,
		RateLimit:       req.RateLimit,
		HealthProbe:     req.HealthProbe,
		Reliability:     req.Reliability,
	}
}

// HandleListChannelModels GET /api/v1/admin/channels/{id}/models
func (h *Handler) HandleListChannelModels(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	models, err := h.svc.ListChannelModels(r.Context(), id)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	list := make([]viewChannelModel, 0, len(models))
	for i := range models {
		list = append(list, toChannelModelView(&models[i]))
	}
	resp.OK(w, r, list)
}

// HandleListAllChannelModels GET /api/v1/admin/channel-models
// 返回跨渠道全量渠道内部模型（含所属渠道名与绑定对外模型名），供对外模型编辑页「从内部模型同步定价」。
func (h *Handler) HandleListAllChannelModels(w http.ResponseWriter, r *http.Request) {
	models, err := h.svc.ListAllChannelModels(r.Context())
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	list := make([]viewChannelModel, 0, len(models))
	for i := range models {
		list = append(list, toChannelModelView(&models[i]))
	}
	resp.OK(w, r, list)
}

// HandleCreateChannelModel POST /api/v1/admin/channels/{id}/models
func (h *Handler) HandleCreateChannelModel(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	var req channelModelRequest
	if !decodeBody(w, r, &req) {
		return
	}
	m, err := h.svc.CreateChannelModel(r.Context(), id, req.toInput())
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, toChannelModelView(m))
}

// pathMid 解析路径中的渠道内部模型 ID。
func pathMid(mid string) (int64, bool) {
	id, err := strconv.ParseInt(mid, 10, 64)
	return id, err == nil
}

// HandleUpdateChannelModel PUT /api/v1/admin/channels/{id}/models/{mid}
func (h *Handler) HandleUpdateChannelModel(w http.ResponseWriter, r *http.Request) {
	channelID, ok := pathID(w, r)
	if !ok {
		return
	}
	mid, ok := pathMid(r.PathValue("mid"))
	if !ok {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "无效的渠道内部模型 ID")
		return
	}
	var req channelModelRequest
	if !decodeBody(w, r, &req) {
		return
	}
	m, err := h.svc.UpdateChannelModel(r.Context(), channelID, mid, req.toInput())
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, toChannelModelView(m))
}

// HandleDeleteChannelModel DELETE /api/v1/admin/channels/{id}/models/{mid}
func (h *Handler) HandleDeleteChannelModel(w http.ResponseWriter, r *http.Request) {
	channelID, ok := pathID(w, r)
	if !ok {
		return
	}
	mid, ok := pathMid(r.PathValue("mid"))
	if !ok {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "无效的渠道内部模型 ID")
		return
	}
	if err := h.svc.DeleteChannelModel(r.Context(), channelID, mid); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]any{"Affected": 1})
}

// HandleCronPreview GET /api/v1/admin/channels/cron-preview?Expr=...&Limit=5
// 依据 6 段 cron 表达式计算后续最近几次执行时间，供前端在编辑时预览校验。
func (h *Handler) HandleCronPreview(w http.ResponseWriter, r *http.Request) {
	expr := strings.TrimSpace(r.URL.Query().Get("Expr"))
	if expr == "" {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "expr 不能为空")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("Limit"))
	if limit <= 0 || limit > 10 {
		limit = 5
	}
	spec, err := cronx.Parse(expr)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "cron 表达式非法: "+err.Error())
		return
	}
	times := make([]string, 0, limit)
	after := time.Now()
	for i := 0; i < limit; i++ {
		next := spec.Next(after)
		if next.IsZero() {
			break
		}
		times = append(times, next.Format(time.RFC3339))
		after = next
	}
	resp.OK(w, r, map[string]any{"Times": times})
}

// HandlePullModels POST /api/v1/admin/channels/{id}/models/pull
// 拉取渠道 /v1/models 列表（不落库），供管理员选择填入内部模型。
func (h *Handler) HandlePullModels(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	models, err := h.svc.PullModels(r.Context(), id, nil)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	views := make([]PullModelView, 0, len(models))
	for _, m := range models {
		views = append(views, PullModelView{ID: m.ID, Object: m.Object, OwnedBy: m.OwnedBy})
	}
	resp.OK(w, r, map[string]any{"List": views})
}

// HandleModelState PUT/POST /api/v1/admin/channels/{id}/models/{mid}/state
// 手动设置内部模型状态：{action: normal|drain|disable}
func (h *Handler) HandleModelState(w http.ResponseWriter, r *http.Request) {
	channelID, ok := pathID(w, r)
	if !ok {
		return
	}
	mid, ok := pathMid(r.PathValue("mid"))
	if !ok {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "无效的渠道内部模型 ID")
		return
	}
	var req stateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	m, err := h.svc.ModelForceState(r.Context(), channelID, mid, req.Action, req.Reason)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, toChannelModelView(m))
}

// HandleListModelEvents GET /api/v1/admin/channels/{id}/model-events
func (h *Handler) HandleListModelEvents(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	evs, err := h.svc.ListModelEvents(r.Context(), id)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, evs)
}

// HandleListProbeLogs GET /api/v1/admin/channels/{id}/probe-logs?Limit=N
func (h *Handler) HandleListProbeLogs(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("Limit"))
	logs, err := h.svc.ListProbeLogs(r.Context(), id, limit)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, logs)
}

// ===== 渠道密钥（channel_keys）管理 =====

// viewChannelKey 密钥管理 API 输出视图：凭据仅暴露明文尾号（KeyViews 已脱敏）。
type viewChannelKey struct {
	ID             int64     `json:"ID,string"`
	ChannelID      int64     `json:"ChannelID,string"`
	Name           string    `json:"Name"`
	CredentialTail string    `json:"CredentialTail"`
	State          State     `json:"State"`
	LastErr        string    `json:"LastErr,omitempty"`
	ActiveSessions int       `json:"ActiveSessions"`
	CreatedAt      time.Time `json:"CreatedAt"`
	UpdatedAt      time.Time `json:"UpdatedAt"`
}

// toChannelKeyView 以 DB 行为基底、可按运行时视图覆盖状态/尾号/最近错误。
func toChannelKeyView(k *ChannelKey, rv *KeyRuntimeView) viewChannelKey {
	v := viewChannelKey{
		ID: k.ID, ChannelID: k.ChannelID, Name: k.Name,
		State: k.State, LastErr: k.LastErr,
		CreatedAt: k.CreatedAt, UpdatedAt: k.UpdatedAt,
	}
	if rv != nil {
		v.State = rv.State
		if rv.LastErr != "" {
			v.LastErr = rv.LastErr
		}
		v.CredentialTail = rv.CredentialTail
	}
	return v
}

// keyRuntimeView 取单密钥运行时视图（凭据尾号）；密钥不在运行时返回 nil。
func (h *Handler) keyRuntimeView(keyID int64) *KeyRuntimeView {
	if h.mgr == nil {
		return nil
	}
	kr, ok := h.mgr.GetKeyRuntime(keyID)
	if !ok {
		return nil
	}
	return &KeyRuntimeView{
		KeyID:          kr.Key.ID,
		Name:           kr.Key.Name,
		State:          kr.Machine.State(),
		LastErr:        kr.Key.LastErr,
		CredentialTail: credentialTail(kr.CredentialPlain),
	}
}

// keyDepsReady 校验密钥端点依赖已装配（F1 前未装配时返回 500，避免 nil panic）。
func (h *Handler) keyDepsReady(w http.ResponseWriter, r *http.Request) bool {
	if h.keySvc == nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "密钥服务未装配")
		return false
	}
	return true
}

// pathKeyID 解析路径中的密钥 ID（{kid}）。
func pathKeyID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("kid"), 10, 64)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "无效的密钥 ID")
		return 0, false
	}
	return id, true
}

type createChannelKeyRequest struct {
	Name       string `json:"Name"`
	Credential string `json:"Credential"`
}

// HandleListKeys GET /api/v1/admin/channels/{id}/keys
// 聚合：keySvc.List（DB 全量）+ manager.KeyViews（运行时状态/凭据尾号）合并输出。
func (h *Handler) HandleListKeys(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(w, r)
	if !ok {
		return
	}
	if h.keySvc == nil {
		resp.OK(w, r, []viewChannelKey{})
		return
	}
	keys, err := h.keySvc.List(r.Context(), id)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	rtViews := make(map[int64]KeyRuntimeView)
	if h.mgr != nil {
		for _, v := range h.mgr.KeyViews(id) {
			rtViews[v.KeyID] = v
		}
	}
	list := make([]viewChannelKey, 0, len(keys))
	for i := range keys {
		var rv *KeyRuntimeView
		if v, ok := rtViews[keys[i].ID]; ok {
			rv = &v
		}
		list = append(list, toChannelKeyView(&keys[i], rv))
	}
	resp.OK(w, r, list)
}

// HandleCreateKey POST /api/v1/admin/channels/{id}/keys  {name,credential}
// 成功落库后经 mgr.UpsertKey 同步运行时，密钥立即参与路由候选。
func (h *Handler) HandleCreateKey(w http.ResponseWriter, r *http.Request) {
	channelID, ok := pathID(w, r)
	if !ok {
		return
	}
	var req createChannelKeyRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !h.keyDepsReady(w, r) {
		return
	}
	k, err := h.keySvc.Create(r.Context(), channelID, req.Name, req.Credential)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	if h.mgr != nil {
		h.mgr.UpsertKey(*k)
	}
	resp.OK(w, r, toChannelKeyView(k, h.keyRuntimeView(k.ID)))
}

// HandleListKeyEvents GET /api/v1/admin/channels/{id}/keys/{kid}/events
// 查询单个密钥的状态流转记录（channel_key_events，created_at 倒序，默认 50 条）。
func (h *Handler) HandleListKeyEvents(w http.ResponseWriter, r *http.Request) {
	channelID, ok := pathID(w, r)
	if !ok {
		return
	}
	kid, ok := pathKeyID(w, r)
	if !ok {
		return
	}
	if !h.keyDepsReady(w, r) {
		return
	}
	if !h.requireKeyOwnership(w, r, channelID, kid) {
		return
	}
	evs, err := h.keySvc.ListEvents(r.Context(), kid, 50)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, evs)
}

// HandleUpdateKey PUT /api/v1/admin/channels/{id}/keys/{kid}  {name,credential?}
// 落库成功后以最新记录重读并 UpsertKey 重建运行时。
func (h *Handler) HandleUpdateKey(w http.ResponseWriter, r *http.Request) {
	channelID, ok := pathID(w, r)
	if !ok {
		return
	}
	kid, ok := pathKeyID(w, r)
	if !ok {
		return
	}
	var req createChannelKeyRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !h.keyDepsReady(w, r) {
		return
	}
	if !h.requireKeyOwnership(w, r, channelID, kid) {
		return
	}
	k, err := h.keySvc.Update(r.Context(), kid, req.Name, req.Credential)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	if h.mgr != nil {
		h.mgr.UpsertKey(*k)
	}
	resp.OK(w, r, toChannelKeyView(k, h.keyRuntimeView(k.ID)))
}

// HandleDeleteKey DELETE /api/v1/admin/channels/{id}/keys/{kid}
// 软删成功后经 mgr.RemoveKey 立即摘除运行时，并联动清理该密钥全部存活会话
// （内存+DB：经注入的 SessionRegistry.KillByFilter(channel_key_id=keyID) 踢下线）。
func (h *Handler) HandleDeleteKey(w http.ResponseWriter, r *http.Request) {
	channelID, ok := pathID(w, r)
	if !ok {
		return
	}
	kid, ok := pathKeyID(w, r)
	if !ok {
		return
	}
	if !h.keyDepsReady(w, r) {
		return
	}
	if !h.requireKeyOwnership(w, r, channelID, kid) {
		return
	}
	if err := h.keySvc.Delete(r.Context(), kid); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	if h.mgr != nil {
		h.mgr.RemoveKey(kid)
	}
	if h.killKeySess != nil {
		h.killKeySess(kid)
	}
	resp.OK(w, r, map[string]any{"Affected": 1})
}

// HandleKeyState POST /api/v1/admin/channels/{id}/keys/{kid}/state  {action:normal|drain|disable|recover}
// KeyService.ForceState 落库 + 写 channel_key_events；mgr.ManualSetKeyState 同步内存运行时（解决审查 BLOCK3 的单点场景）。
func (h *Handler) HandleKeyState(w http.ResponseWriter, r *http.Request) {
	channelID, ok := pathID(w, r)
	if !ok {
		return
	}
	kid, ok := pathKeyID(w, r)
	if !ok {
		return
	}
	var req stateRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if !h.keyDepsReady(w, r) {
		return
	}
	if !h.requireKeyOwnership(w, r, channelID, kid) {
		return
	}
	k, err := h.keySvc.ForceState(r.Context(), kid, req.Action)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	// 内存同步：DB 已落库；密钥不在运行时（未装配/未装载）时忽略，留待 F1 全量装载。
	if h.mgr != nil {
		_ = h.mgr.ManualSetKeyState(kid, req.Action)
	}
	resp.OK(w, r, toChannelKeyView(k, h.keyRuntimeView(k.ID)))
}

// HandleProbeKey POST /api/v1/admin/channels/{id}/keys/{kid}/probe
// 手动触发该密钥一轮健康探测（同步）：结果按定时探测同规则驱动密钥状态机，
// 并落 probe_logs 与记账；返回 ProbeOutcome（探测后状态/耗时/错误）。
func (h *Handler) HandleProbeKey(w http.ResponseWriter, r *http.Request) {
	channelID, ok := pathID(w, r)
	if !ok {
		return
	}
	kid, ok := pathKeyID(w, r)
	if !ok {
		return
	}
	if !h.keyDepsReady(w, r) {
		return
	}
	if !h.requireKeyOwnership(w, r, channelID, kid) {
		return
	}
	if h.mgr == nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "渠道运行时未装配")
		return
	}
	out, err := h.mgr.ProbeKeyNow(kid)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, out)
}

// HandleProbeModel POST /api/v1/admin/channels/{id}/models/{mid}/probe
// 手动触发该内部模型一轮健康探测（同步）：经该渠道可用密钥对内部模型发起探测，
// 结果仅回喂模型状态机并落 probe_logs 与记账；返回 ProbeOutcome（探测后状态/耗时/错误）。
func (h *Handler) HandleProbeModel(w http.ResponseWriter, r *http.Request) {
	channelID, ok := pathID(w, r)
	if !ok {
		return
	}
	mid, ok := pathMid(r.PathValue("mid"))
	if !ok {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "无效的渠道内部模型 ID")
		return
	}
	if h.mgr == nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "渠道运行时未装配")
		return
	}
	out, err := h.mgr.ProbeModelNow(channelID, mid)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, out)
}
