// Package router 实现 LLM 网关的路由选择（双层模型）：对外模型 → 绑定该对外模型的
// 渠道内部模型 → 渠道选择（健康 + 标签/亲和/成本最优）→ internal_model_id 发给上游。
//
// ## 路由决策（成本最优重构后）
//
// 0003 迁移后模型拆为两层：external_models（对外名 + 售价）与 channel_models
// （渠道 + 内部模型ID + 成本 + 绑定某一对外模型）。路由以对外模型为入口，
// 只把「实际绑定了该对外模型」的渠道作为候选：
//
//  1. model.Service.GetByExternalName 取启用对外模型；
//  2. channel.Store.ListByExternal(ext.ID) 取绑定该对外模型的全部非禁用 channel_models；
//     同一对外模型在同一渠道/密钥下可绑定多个内部模型，每行都是独立候选；
//  3. 候选过滤（两层级联）：渠道.state != DISABLED、密钥.state != DISABLED、
//     且该内部模型行.state != DISABLED——禁用/排空某内部模型只跳过该行，
//     同渠道/密钥的其他内部模型仍候选；
//  4. 会话命中（SessionRegistry）：命中的 (渠道,密钥,内部模型) 即使排空也放行并排首位；
//  5. 新会话只在「渠道、密钥、内部模型都为正常」的候选中选择；空标签过滤同理；
//  6. 新会话候选排序：priority 降序（高优先级层永远优先）→ 当前成本升序
//     （同优先级内最便宜优先）→ 存活会话数升序（成本并列时更少者优先）→
//     渠道/模型行/密钥 ID 升序兜底；并发会话上限由网关在会话创建时校验；
//  7. 候选为空 → ErrNoAvailable。
//
// 「当前成本」= 样值用量代入 channel_models.cost_rates 求和 × 当前时段系数：
// 样值来自全局配置 routing.cost_sample；时段系数取候选模型级 time_config，
// 为空回落全局 billing.time_config；上下文分档系数恒按 1（不参与比较）。
// cost_rates 缺失/五段全 0 视为「成本未知」，排在有有效成本候选之后。
// 渠道 weight 已废弃，不再参与任何路由选择。
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/channel"
	"github.com/team/llmgateway/internal/domain/model"
	"github.com/team/llmgateway/internal/domain/tag"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// RouteSpec 描述一次路由请求。
type RouteSpec struct {
	ExternalModel string            // 对外模型名
	TagKV         map[string]string // 已解析的令牌标签 KV；无标签时 nil
	TagID         *int64            // 用于日志
	SessionKey    string            // token_id + model + x-session-id；空则不启用亲和
}

// RouteResult 描述一次路由结果。
type RouteResult struct {
	ChannelID       int64
	ChannelKeyID    int64 // 选中的渠道密钥（B3 起路由以密钥为运行时实体）
	InternalModelID string
	ModelRowID      int64 // channel_models 行 ID（状态机回喂与模型级限流定位用）
	ExternalModelID int64 // 命中对外模型的 ID（计费/展示用）
	Revision        int64 // 选中渠道的变更代次（基于 Channel.UpdatedAt），用于亲和失效判断
	Fallback        bool
	// 成本标量 / 时段系数（OQ-3：仅用于决策轨迹排查，不参与计费）。
	Cost      float64 // 选中候选的成本标量（样值用量 × 单价 × 时段系数）
	CostKnown bool    // 成本是否有效（false=单价缺失/全 0，视为最贵）
	TimeCoeff float64 // 选中候选的时段系数（模型级优先，回落全局/1）
}

// ChannelRoute 单个候选渠道的路由信息，供网关在超限/429 时按序切换渠道（failover）。
type ChannelRoute struct {
	ChannelID       int64
	ChannelKeyID    int64 // 候选渠道密钥（B3 起候选为 (渠道,密钥,模型) 三元组）
	InternalModelID string
	ModelRowID      int64
	ExternalModelID int64
	Revision        int64
	// 成本标量 / 时段系数（OQ-3：仅用于决策轨迹排查，不参与计费）。
	Cost      float64
	CostKnown bool
	TimeCoeff float64
}

// RouteErrorKind 区分不同路由失败原因。
type RouteErrorKind int

const (
	KindModelNotFound RouteErrorKind = iota + 1
	KindNoRoute
	KindNoAvailable
)

// RouteError 是路由失败错误，携带 HTTP 状态码与业务码，供网关返回统一响应。
type RouteError struct {
	Kind       RouteErrorKind
	HTTPStatus int
	Code       int
	Message    string
}

func (e *RouteError) Error() string { return e.Message }

// AsRouteError 从错误中提取 *RouteError；非路由错误返回 (nil, false)。
func AsRouteError(err error) (*RouteError, bool) {
	var re *RouteError
	if errors.As(err, &re) {
		return re, true
	}
	return nil, false
}

func modelNotFound(name string) *RouteError {
	return &RouteError{Kind: KindModelNotFound, HTTPStatus: http.StatusNotFound,
		Code: resp.CodeNotFound, Message: fmt.Sprintf("模型 %s 未配置或已停用", name)}
}

func noRoute() *RouteError {
	return &RouteError{Kind: KindNoRoute, HTTPStatus: http.StatusServiceUnavailable,
		Code: resp.CodeNoRoute, Message: "当前请求的语义标签无可用的服务渠道"}
}

func noAvailable() *RouteError {
	return &RouteError{Kind: KindNoAvailable, HTTPStatus: http.StatusServiceUnavailable,
		Code: resp.CodeNoRoute, Message: "无可用渠道，请稍后重试"}
}

// extModelSource 对外模型查询（model.Service 满足）。
type extModelSource interface {
	GetByExternalName(ctx context.Context, externalName string) (*model.ExternalModel, error)
}

// channelModelSource 渠道内部模型查询（channel 包 channelModelStore 满足）。
type channelModelSource interface {
	// ListByExternal 返回绑定某对外模型的非禁用渠道内部模型列表；同一渠道/密钥可有多行，
	// 每行是独立候选（某行禁用只跳过该行，同渠道其他内部模型行仍候选）。
	ListByExternal(ctx context.Context, externalModelID int64) ([]channel.ChannelModel, error)
}

// CostConfig 提供会话创建时成本比较所需的全局配置（装配层注入；router 不直接读库）。
// 读取失败由实现返回 error，路由层按内置默认样值 / 系数 1 降级并记录日志，不阻断路由。
type CostConfig interface {
	// SampleUsage 返回成本比较用的样值用量（routing.cost_sample）。
	SampleUsage() (billing.Usage, error)
	// TimeConfig 返回全局时段配置（billing.time_config），供候选模型级 time_config 为空时回落。
	TimeConfig() (billing.TimeCoeffConfig, error)
}

// defaultCostSample 内置默认样值用量（与 seeding 默认一致：输入 10000、缓存读 9000、
// 输出 1000、推理 500、缓存写 0）。仅当未注入 CostConfig 或读取失败时兜底。
var defaultCostSample = billing.Usage{Input: 10000, CacheRead: 9000, Output: 1000, Reasoning: 500}

// Engine 是路由选择引擎。
type Engine struct {
	models  extModelSource     // 对外模型查询
	cmSrc   channelModelSource // 渠道内部模型查询
	chMgr   *channel.Manager   // 渠道运行时快照（渠道级 + 模型级状态）
	sess    *SessionRegistry   // 会话注册表（亲和 + 存活会话计数）
	costCfg CostConfig         // 全局成本比较配置（样值 + 全局时段）；nil 时用内置默认
	now     func() time.Time   // 路由时刻（时段系数求值用）
}

// NewEngine 创建路由引擎。sess 为 nil 时使用默认会话注册表。
func NewEngine(models extModelSource, cm channelModelSource, chMgr *channel.Manager, sess *SessionRegistry) *Engine {
	if sess == nil {
		sess = NewSessionRegistry(nil)
	}
	return &Engine{
		models: models, cmSrc: cm, chMgr: chMgr, sess: sess,
		now: time.Now,
	}
}

// SetCostConfig 注入全局成本比较配置（装配层调用；nil 表示使用内置默认样值、时段系数回落 1）。
func (e *Engine) SetCostConfig(cfg CostConfig) { e.costCfg = cfg }

// SetClock 注入时钟（测试用；nil 恢复 time.Now）。
func (e *Engine) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	e.now = now
}

// Sessions 暴露会话注册表（网关创建/续期会话、并发校验、启动清扫用）。
func (e *Engine) Sessions() *SessionRegistry { return e.sess }

// candidate 是路由候选的运行时视图（候选 = (渠道, 密钥, 模型) 三元组）。
type candidate struct {
	id        int64
	keyID     int64
	modelRow  int64
	internal  string
	boundTags []channel.TagRef
	priority  int
	revision  int64
	costRates billing.Rates            // 该内部模型行的成本单价（缺失=成本未知）
	timeCfg   *billing.TimeCoeffConfig // 模型级时段系数（nil=回落全局）
	// 择序时回填的成本标量 / 时段系数（OQ-3：随候选携带至决策轨迹）。
	cost      float64
	costKnown bool
	timeCoeff float64
	chState   channel.State
	mState    channel.State
	keyState  channel.State
}

// Route 执行对外模型 → 绑定渠道内部模型 → 标签匹配 → 亲和 → 成本最优排序，返回首个候选。
//
// 渠道透明：错误仅返回业务错误，不暴露渠道名；结果对调用方不区分渠道身份详情。
func (e *Engine) Route(ctx context.Context, spec RouteSpec) (*RouteResult, error) {
	routes, err := e.RouteAll(ctx, spec)
	if err != nil {
		return nil, err
	}
	cr := routes[0]
	return &RouteResult{
		ChannelID:       cr.ChannelID,
		ChannelKeyID:    cr.ChannelKeyID,
		InternalModelID: cr.InternalModelID,
		ModelRowID:      cr.ModelRowID,
		ExternalModelID: cr.ExternalModelID,
		Revision:        cr.Revision,
		Cost:            cr.Cost,
		CostKnown:       cr.CostKnown,
		TimeCoeff:       cr.TimeCoeff,
	}, nil
}

// RouteAll 返回全部可用候选渠道（网关据此在超限/429/失败时按序 failover）：
//   - 会话命中（含排空，禁用除外）排首；
//   - 其余为「渠道与模型都正常」的候选，按 priority 降序 → 成本升序 → 存活会话数升序 → 稳定 id 排序。
func (e *Engine) RouteAll(ctx context.Context, spec RouteSpec) ([]*ChannelRoute, error) {
	cands, extID, err := e.collectCandidates(ctx, spec)
	if err != nil {
		return nil, err
	}
	out := make([]*ChannelRoute, 0, len(cands))
	for _, c := range cands {
		out = append(out, &ChannelRoute{
			ChannelID:       c.id,
			ChannelKeyID:    c.keyID,
			InternalModelID: c.internal,
			ModelRowID:      c.modelRow,
			ExternalModelID: extID,
			Revision:        c.revision,
			Cost:            c.cost,
			CostKnown:       c.costKnown,
			TimeCoeff:       c.timeCoeff,
		})
	}
	return out, nil
}

// collectCandidates 解析对外模型并返回排序后的候选集合：
// 首位为会话命中候选（可排空，不可禁用），其余为双正常候选（按成本最优排序）。
func (e *Engine) collectCandidates(ctx context.Context, spec RouteSpec) ([]candidate, int64, error) {
	ext, err := e.models.GetByExternalName(ctx, spec.ExternalModel)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, 0, modelNotFound(spec.ExternalModel)
		}
		return nil, 0, fmt.Errorf("resolve external model: %w", err)
	}

	cms, err := e.cmSrc.ListByExternal(ctx, ext.ID)
	if err != nil {
		return nil, 0, fmt.Errorf("list bound channel models: %w", err)
	}
	if len(cms) == 0 {
		return nil, 0, noAvailable()
	}
	// 每行绑定记录都是独立候选：同渠道/密钥下多个内部模型各自展开（若按行展开前先归并，
	// 会导致某个内部模型禁用时渠道整体被跳过，或退而用同渠道另一行顶替，语义都会错乱；
	// 禁用/排空某内部模型应只跳过该行）。ListByExternal 已按 channel_id,id 排序。
	// 同时把该行的成本单价与模型级时段配置一并随身携带（成本比较用，无需二次查询）。
	internalByChannel := make(map[int64][]internalBinding, len(cms))
	for _, cm := range cms {
		internalByChannel[cm.ChannelID] = append(internalByChannel[cm.ChannelID],
			internalBinding{internal: cm.InternalModelID, rowID: cm.ID,
				costRates: cm.CostRates, timeCfg: cm.TimeConfig})
	}

	cands := e.usableCandidates(internalByChannel)
	if spec.TagKV != nil && len(spec.TagKV) > 0 {
		filtered := cands[:0]
		for _, c := range cands {
			for _, bt := range c.boundTags {
				if tag.StrictContains(bt.KV, spec.TagKV) {
					filtered = append(filtered, c)
					break
				}
			}
		}
		cands = filtered
		if len(cands) == 0 {
			return nil, 0, noRoute()
		}
	}
	if len(cands) == 0 {
		return nil, 0, noAvailable()
	}

	// 会话命中：命中的 (密钥,模型) 即使排空也放行（禁用已被 usable 排除），并排首位。
	// 命中判定为密钥级：在候选集内找 ChannelKeyID+InternalModelID 精确匹配的候选移到首位，
	// 避免同渠道多密钥共享会话时轮询切到非归属密钥。
	now := e.now()
	sample, globalTC := e.costContext()
	normal := make([]candidate, 0, len(cands))
	var sessionCand *candidate
	for i := range cands {
		c := cands[i]
		if spec.SessionKey != "" && sessionCand == nil {
			if e.sess.Hit(spec.SessionKey) {
				if s, ok := e.sess.Lookup(spec.SessionKey); ok &&
					s.ChannelKeyID == c.keyID && s.InternalModelID == c.internal {
					e.fillCost(&cands[i], sample, globalTC, now)
					sessionCand = &cands[i]
					continue
				}
			}
		}
		if c.chState == channel.StateNormal && c.mState == channel.StateNormal && c.keyState == channel.StateNormal {
			normal = append(normal, c)
		}
	}
	ordered := e.orderCandidates(normal, now, sample, globalTC)
	if sessionCand != nil {
		return append([]candidate{*sessionCand}, ordered...), ext.ID, nil
	}
	if len(ordered) == 0 {
		return nil, 0, noAvailable()
	}
	return ordered, ext.ID, nil
}

// internalBinding 某渠道的一条内部模型绑定（同一对外模型下同渠道/密钥可有多个内部模型，各为独立候选）。
type internalBinding struct {
	internal  string
	rowID     int64                    // channel_models 行 ID（状态机回喂与模型级限流定位用）
	costRates billing.Rates            // 该行成本单价
	timeCfg   *billing.TimeCoeffConfig // 该行模型级时段配置
}

// usableCandidates 返回「渠道非禁用 且 密钥非禁用」的候选，按内部模型行逐条展开
// （候选 = 每 (渠道,密钥,内部模型) 一条），含排空候选：会话命中时允许其续行；
// 普通选择由上层过滤为「渠道/模型/密钥都正常」。
// 某内部模型禁用/排空只跳过该行：同一渠道/密钥下的其他内部模型行仍正常展开候选。
// 展开顺序仅为收集顺序：渠道按快照遍历、同渠道按内部模型行 id 升序、各自按密钥轮询顺序，
// 最终顺序由 orderCandidates 决定。
func (e *Engine) usableCandidates(internalByChannel map[int64][]internalBinding) []candidate {
	cands := make([]candidate, 0, 16)
	for _, v := range e.chMgr.Snapshot() {
		if v.State == channel.StateDisabled || v.Protocol != channel.ProtocolOpenAICompat {
			continue
		}
		binds, ok := internalByChannel[v.ID]
		if !ok {
			continue // 该渠道未绑定当前对外模型 → 不候选
		}
		for _, b := range binds {
			// 模型级状态：仅当该内部模型行在运行时可用（非禁用）才展开；禁用只跳过这一行。
			var modelRowID int64
			var mState channel.State
			foundModel := false
			for _, mv := range e.chMgr.ModelViews(v.ID) {
				if mv.InternalModelID != b.internal || mv.State == channel.StateDisabled {
					continue
				}
				modelRowID = mv.ID
				mState = mv.State
				foundModel = true
				break
			}
			if !foundModel {
				continue
			}
			// 密钥级展开：同渠道多个可用密钥按路由轮询顺序平铺打散，state != DISABLED 才候选
			for _, keyID := range e.chMgr.KeyOrderForRouting(v.ID) {
				kr, ok := e.chMgr.GetKeyRuntime(keyID)
				if !ok || kr.Machine.State() == channel.StateDisabled {
					continue
				}
				cands = append(cands, candidate{
					id:        v.ID,
					keyID:     keyID,
					modelRow:  modelRowID,
					internal:  b.internal,
					boundTags: v.BoundTags,
					priority:  v.Priority,
					revision:  v.UpdatedAt.UnixNano(),
					costRates: b.costRates,
					timeCfg:   b.timeCfg,
					chState:   v.State,
					mState:    mState,
					keyState:  kr.Machine.State(),
				})
			}
		}
	}
	return cands
}

// scoredCandidate 候选择序的中间态：附成本标量（是否有效）与存活会话数。
type scoredCandidate struct {
	c     candidate
	cost  float64
	known bool // 成本是否有效（false=缺失/全 0，视为最贵）
	load  int  // 该候选密钥的存活会话数
}

// orderCandidates 按「priority 降序 → 成本升序 → 存活会话数升序 → 稳定 id 升序」重排候选：
//   - priority 为硬分层：高优先级候选永远优先于低优先级候选（即便后者更便宜）；
//   - 同优先级内取当前成本最低者；成本缺失/全 0 的候选视为最贵，排在有有效成本候选之后；
//   - 成本并列（或均缺失）时存活会话数更少者优先；
//   - 再并列按 渠道 ID、模型行 ID、密钥 ID 升序兜底，保证结果确定（无随机）。
//
// 成本标量 = 样值用量代入 cost_rates 求和 × 当前时段系数（分档系数恒为 1）。
// 全局样值与全局时段由 collectCandidates 只读一次并传入，逐候选仅做纯计算，不产生额外 IO。
func (e *Engine) orderCandidates(cands []candidate, now time.Time, sample billing.Usage, globalTC *billing.TimeCoeffConfig) []candidate {
	if len(cands) == 0 {
		return cands
	}
	scored := make([]scoredCandidate, len(cands))
	for i := range cands {
		c := cands[i]
		e.fillCost(&c, sample, globalTC, now)
		scored[i] = scoredCandidate{c: c, cost: c.cost, known: c.costKnown, load: e.sess.CountActive(c.keyID)}
	}
	sort.SliceStable(scored, func(i, j int) bool {
		a, b := scored[i], scored[j]
		if a.c.priority != b.c.priority {
			return a.c.priority > b.c.priority
		}
		if a.known != b.known {
			return a.known // 有有效成本者优先（缺价视为最贵）
		}
		if a.known && a.cost != b.cost {
			return a.cost < b.cost
		}
		if a.load != b.load {
			return a.load < b.load
		}
		if a.c.id != b.c.id {
			return a.c.id < b.c.id
		}
		if a.c.modelRow != b.c.modelRow {
			return a.c.modelRow < b.c.modelRow
		}
		return a.c.keyID < b.c.keyID
	})
	out := make([]candidate, len(scored))
	for i := range scored {
		out[i] = scored[i].c
	}
	return out
}

// fillCost 计算候选当前的时段系数与成本标量，并回填到候选字段
// （OQ-3：随候选携带至网关决策轨迹，便于排查「为何选中该渠道」）。
func (e *Engine) fillCost(c *candidate, sample billing.Usage, globalTC *billing.TimeCoeffConfig, now time.Time) {
	tc := resolveTimeCoeff(c.timeCfg, globalTC, now)
	cost, known := costScalar(sample, c.costRates, tc)
	c.timeCoeff = tc
	c.cost = cost
	c.costKnown = known
}

// costContext 读取一次全局成本参数（样值 + 全局时段），供本轮候选比较复用；
// 未注入配置或读取失败时按内置默认样值 / 无全局时段降级并记录日志（不阻断路由）。
func (e *Engine) costContext() (billing.Usage, *billing.TimeCoeffConfig) {
	sample := defaultCostSample
	var globalTC *billing.TimeCoeffConfig
	if e.costCfg == nil {
		return sample, nil
	}
	if s, err := e.costCfg.SampleUsage(); err == nil {
		sample = s
	} else {
		slog.Default().Warn("路由成本样值读取失败，使用内置默认值", "err", err)
	}
	if tc, err := e.costCfg.TimeConfig(); err == nil {
		globalTC = &tc
	} else {
		slog.Default().Warn("全局时段配置读取失败，时段系数按 1 处理", "err", err)
	}
	return sample, globalTC
}

// resolveTimeCoeff 求候选当前时段系数：模型级 time_config 优先，为空回落全局；均无则 1；
// 解析失败回落 1 并记录日志（不阻断路由）。
func resolveTimeCoeff(modelTC, globalTC *billing.TimeCoeffConfig, now time.Time) float64 {
	cfg := modelTC
	if cfg == nil {
		cfg = globalTC
	}
	if cfg == nil {
		return 1
	}
	v, err := cfg.CoeffFor(now)
	if err != nil {
		slog.Default().Warn("时段系数解析失败，按 1 处理", "err", err)
		return 1
	}
	return v
}

// costScalar 计算候选成本标量 = sumRates(sample, rates) × 时段系数（分档系数恒为 1）。
// 返回 (标量, 是否有效)；cost_rates 缺失/全 0、或单价表不合法（缺键/负值）时返回 (0, false)，
// 由调用方按「成本未知（最贵）」处理。
func costScalar(sample billing.Usage, rates billing.Rates, timeCoeff float64) (float64, bool) {
	if !hasValidCost(rates) {
		return 0, false
	}
	total, err := billing.RawTotal(sample, rates, timeCoeff, 1)
	if err != nil {
		return 0, false
	}
	return total, true
}

// hasValidCost 判断成本单价表是否具备有效值：至少一段非 0（全 0/空视为成本未知）。
// 口径说明（FR-5）：仅当五段全为 0 或缺失才算「成本未知」；若某候选仅 cache_write 有单价
// 而样值 cache_write=0，其标量为 0 且仍视为有效（按最便宜参与比较），这是既定样值口径。
func hasValidCost(rates billing.Rates) bool {
	for _, v := range rates {
		if v != 0 {
			return true
		}
	}
	return false
}
