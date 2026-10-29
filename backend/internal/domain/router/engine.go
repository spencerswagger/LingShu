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
//  3. 候选过滤（两层级联）：密钥.state != DISABLED、且该内部模型行.state != DISABLED
//     ——渠道无独立状态，可用性由密钥与内部模型决定；
//     禁用/排空某内部模型只跳过该行，同渠道/密钥的其他内部模型仍候选；
//  4. 会话命中（SessionRegistry）：命中的 (渠道,密钥,内部模型) 即使排空也放行并排首位；
//  5. 新会话只在「密钥、内部模型都为正常」的候选中选择；空标签过滤同理；
//  6. 新会话候选排序：priority 降序（高优先级层永远优先）→ 当前成本升序
//     （同优先级内最便宜优先）→ 存活会话数升序（成本并列时更少者优先）→
//     渠道/模型行/密钥 ID 升序兜底；并发会话上限由网关在会话创建时校验；
//  7. 候选为空 → ErrNoAvailable。
//
// 「当前成本」= 样值用量代入 channel_models.cost_rates 求和 × 当前时段系数 × 上下文分档系数：
// 样值默认来自全局配置 routing.cost_sample，RouteSpec.Sample 非空时以本轮样值覆盖；
// 时段系数取候选模型级 time_config，为空回落全局 billing.time_config；
// 上下文分档系数按 input+cache_read 落在分档上取候选模型级 context_tiers，为空回落全局 billing.context_tiers。
// cost_rates 缺失/三段全 0 视为「成本未知」，排在有有效成本候选之后。
// 渠道 weight 已废弃，不再参与任何路由选择。
package router

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
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
	// Sample 本轮请求的三段 token 样值（逐请求显式样值兜底入口，成本比较用）：非 nil 时用于成本比较
	// （覆盖全局 routing.cost_sample）。语义与会话级 SessionTokens 区分：Sample 服务于「默认成本排序 /
	// 起点择优」的样值口径，而 SessionTokens 是「会话成本路由」的会话级明细（见下）。
	// nil=回落全局样值；&billing.Usage{}（全零）为显式零样值，会关闭成本择优。
	// 注意：Sample 同时参与「默认成本排序」（Enabled=false 亦生效），改动它会改变既有排序。
	// 生产侧网关不写 Sample（改用 SessionTokens）；本字段保留为逐请求显式样值的兜底注入入口（测试/特殊调用方用）。
	Sample *billing.Usage
	// SessionTokens 本轮会话三段 token 明细（输入/缓存读/输出），仅供「会话成本路由」在
	// SessionCostConfig.Enabled=true 且为起点时择优使用；与 Sample 刻意分离：即便网关每轮组装本轮样值，
	// Enabled=false 时也不影响默认成本排序（零行为变化）。
	// nil（或未设置）=回落 Sample / 全局 routing.cost_sample。
	SessionTokens *billing.Usage
	// SessionTurn 会话位置 k（本会话已完成轮数；0=起点/未知）。
	// 仅用于决策产物的观测（事件 Turn 可回放）与「若改用经验式」时的输入；**不参与剩余轮数计算**
	//（需求已定剩余轮数为固定值，见 SessionCostConfig.RemainingA/B）。网关经 SessionRegistry.Turns 组装。
	SessionTurn int
	// Now 本次路由时刻（零值=回落引擎时钟 e.now()）；供时段系数与决策轨迹求值。
	Now time.Time
}

// RouteResult 描述一次路由结果。
type RouteResult struct {
	ChannelID       int64
	ChannelKeyID    int64 // 选中的渠道密钥（B3 起路由以密钥为运行时实体）
	InternalModelID string
	ModelRowID      int64 // channel_models 行 ID（状态机回喂与模型级限流定位用）
	ExternalModelID int64 // 命中对外模型的 ID（计费/展示用）
	// AutoModelName 请求的 Auto 名（命中 Auto 时非空；非 Auto 请求为空）。
	// 仅用于账单/调用日志留痕；external_model_name 仍记实际命中的对外模型名。
	AutoModelName string
	Revision      int64 // 选中渠道的变更代次（基于 Channel.UpdatedAt），用于亲和失效判断
	Fallback      bool
	// 成本标量 / 时段系数（OQ-3：仅用于决策轨迹排查，不参与计费）。
	// Cost = 样值用量 × 三档成本单价求和 × 时段系数 × 上下文分档系数。
	Cost      float64
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
	// Cost = 样值用量 × 三档成本单价求和 × 时段系数 × 上下文分档系数。
	Cost      float64
	CostKnown bool
	TimeCoeff float64
	// Switch 非 nil 表示本候选是「会话中途切换」的决策目标（含完整决策产物）。
	// 引擎只做决策、**不迁移会话**：由网关在本候选通过可用性/限流校验后调用
	// Engine.ConfirmSessionSwitch 完成迁移并补发决策产物事件，保证会话归属与真实落点一致。
	// 原归属渠道由 Decision.FromChannelKeyID 承载，不再单列重复字段。
	Switch          *SessionCostDecision
	CrossCacheRead  float64 // 本次切换的临界缓存读（Switch 非 nil 时有效）
	CrossCacheKnown bool
}

// 路由层排除原因（稳定枚举字符串：供决策轨迹展示与运维动作映射，勿随文案改动）。
const (
	ExcludeNoBinding   = "no_binding"   // 对外模型未绑定任何渠道内部模型
	ExcludeTagNoRoute  = "tag_no_route" // 标签过滤后无候选
	ExcludeDrained     = "drained"      // 排空候选被剔出新会话候选集
	ExcludeNoCandidate = "no_candidate" // 无可用候选（密钥/内部模型均不可用）
)

// ExcludedCandidate 描述一个在路由层被排除、未进入候选的项（决策轨迹用）。
// 对「无绑定 / 标签无路由 / 无候选」这类没有具体候选身份的排除点，仅 Reason 有效，
// 身份字段为零值；「排空」则按候选逐条记录，使轨迹可见（不再静默无记录）。
type ExcludedCandidate struct {
	ChannelID       int64
	ChannelName     string
	ChannelKeyID    int64
	InternalModelID string
	Reason          string
}

// RouteAllResult 是 RouteAll 的完整结果：候选序列 + 被排除候选。
// Excluded 为向后兼容的新增可空字段（无排除时为 nil）；既有候选序列语义不变。
type RouteAllResult struct {
	Routes   []*ChannelRoute
	Excluded []ExcludedCandidate
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
	// SampleProfile 返回样值补齐比例（routing.sample_profile）：当 RouteSpec 携带的样值
	// 只有「总输入」而缓存读/输出未知时，按该比例拆成三段（缓存读/纯输入/输出）。
	SampleProfile() (billing.SampleProfile, error)
	// TimeConfig 返回全局时段配置（billing.time_config），供候选模型级 time_config 为空时回落。
	TimeConfig() (billing.TimeCoeffConfig, error)
	// ContextTiers 返回全局上下文分档（billing.context_tiers），供候选模型级 context_tiers 为空时回落。
	ContextTiers() ([]billing.TierRule, error)
}

// defaultCostSample 内置默认样值用量（与 seeding 默认一致：输入 10000、缓存读 9000、输出 1000）。
// 仅当未注入 CostConfig 或读取失败时兜底。
var defaultCostSample = billing.Usage{Input: 10000, CacheRead: 9000, Output: 1000}

// Engine 是路由选择引擎。
type Engine struct {
	models  extModelSource     // 对外模型查询
	cmSrc   channelModelSource // 渠道内部模型查询
	chMgr   *channel.Manager   // 渠道运行时快照（渠道级 + 模型级状态）
	sess    *SessionRegistry   // 会话注册表（亲和 + 存活会话计数）
	costCfg CostConfig         // 全局成本比较配置（样值 + 全局时段）；nil 时用内置默认
	now     func() time.Time   // 路由时刻（时段系数求值用）
	// costSink 会话成本决策产物落库 sink；nil 时决策后 no-op（不阻断路由）。
	costSink CostEventSink
	// sessionCost 会话成本路由配置提供者（起点择优 / 中途门控开关）；nil=缺省安全态（Enabled=false）。
	sessionCost *SessionCostProvider
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

// SetSessionCostProvider 注入会话成本路由配置提供者（装配层调用）。
// nil=缺省安全态（Enabled=false）：起点择优不生效，路由完全保持现状（会话亲和优先 / 原排序），渠道不变。
func (e *Engine) SetSessionCostProvider(p *SessionCostProvider) { e.sessionCost = p }

// sessionCostConfig 读取会话成本路由配置；**未注入提供者时回退故障安全态（三态全关）**。
//
// 注意与「产品默认」的区别：产品默认（enabled/mid_switch=true）只在配置存在且能解析、只是少写了字段时生效。
// 装配层漏注入提供者属**装配故障**，此时必须 fail-closed（渠道保持现状），不能凭产品默认静默打开
// 起点择优与中途切换——否则一次装配遗漏就会让所有会话开始换渠道，且没有任何开关痕迹。
func (e *Engine) sessionCostConfig() SessionCostConfig {
	if e.sessionCost == nil {
		return safeSessionCostConfig()
	}
	return e.sessionCost.Config()
}

// SetCostEventSink 注入会话成本决策产物 sink（装配层调用）。
// nil 表示不落库：决策后 EmitSessionCostEvent 为 no-op，不阻断路由。
func (e *Engine) SetCostEventSink(sink CostEventSink) { e.costSink = sink }

// EmitSessionCostEvent 在会话成本决策产生后，经注入的 sink 提交决策产物（落库 / 审计留痕）。
// sink 未注入时 no-op（返回 nil），绝不 panic、不阻断路由；sink 提交错误原样返回供调用方按需记录。
func (e *Engine) EmitSessionCostEvent(sessionID string, turn int, decision SessionCostDecision) error {
	return e.emitCostEvent(SessionCostEvent{SessionID: sessionID, Turn: turn, Decision: decision})
}

// emitCostEvent 经注入的 sink 提交一条完整会话成本决策产物事件（含决策与临界缓存读等上下文）。
// sink 未注入时 no-op（返回 nil），绝不 panic、不阻断路由；sink 提交错误原样返回供调用方按需记录。
func (e *Engine) emitCostEvent(ev SessionCostEvent) error {
	if e.costSink == nil {
		return nil
	}
	return e.costSink.Submit(ev)
}

// SetClock 注入时钟（测试用；nil 恢复 time.Now）。
func (e *Engine) SetClock(now func() time.Time) {
	if now == nil {
		now = time.Now
	}
	e.now = now
}

// Sessions 暴露会话注册表（网关创建/续期会话、并发校验、启动清扫用）。
func (e *Engine) Sessions() *SessionRegistry { return e.sess }

// ActiveKeySessions 返回某渠道密钥当前存活会话数（实时读取会话注册表）。
// 供控制台「服务状态」页按密钥展示真实会话数（并发上限见 channel.KeyDetail.MaxSessions）；
// 未装配会话注册表时返回 0。
func (e *Engine) ActiveKeySessions(keyID int64) int {
	if e.sess == nil {
		return 0
	}
	return e.sess.CountActive(keyID)
}

// CostSample 返回成本比较所用样值用量（routing.cost_sample）；未注入 CostConfig 或读取失败时
// 返回内置默认样值。控制台「服务状态」页成本列折叠区间与该样值同源，保证与决策轨迹成本可对比。
func (e *Engine) CostSample() billing.Usage {
	if e.costCfg != nil {
		if s, err := e.costCfg.SampleUsage(); err == nil {
			return s
		}
	}
	return defaultCostSample
}

// candidate 是路由候选的运行时视图（候选 = (渠道, 密钥, 模型) 三元组）。
type candidate struct {
	id           int64
	chName       string // 渠道名（决策轨迹排除项展示用）
	keyID        int64
	modelRow     int64
	internal     string
	boundTags    []channel.TagRef
	priority     int
	revision     int64
	costRates    billing.Rates            // 该内部模型行的成本单价（缺失=成本未知）
	timeCfg      *billing.TimeCoeffConfig // 模型级时段系数（nil=回落全局）
	contextTiers []billing.TierRule       // 模型级上下文分档（空=回落全局）
	// 择序时回填的成本标量 / 时段系数（OQ-3：随候选携带至决策轨迹）。
	cost      float64
	costKnown bool
	timeCoeff float64
	mState    channel.State
	keyState  channel.State
	// 会话中途切换：决定切换时回填到目标候选上，由 RouteAllWithExcluded 转交网关执行迁移。
	switchDecision      *SessionCostDecision
	crossCacheRead      float64
	crossCacheReadKnown bool
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
//   - 其余为「密钥与模型都正常」的候选，按 priority 降序 → 成本升序 → 存活会话数升序 → 稳定 id 排序。
func (e *Engine) RouteAll(ctx context.Context, spec RouteSpec) ([]*ChannelRoute, error) {
	res, err := e.RouteAllWithExcluded(ctx, spec)
	if err != nil {
		return nil, err
	}
	return res.Routes, nil
}

// RouteAllWithExcluded 与 RouteAll 同语义，但额外返回路由层被排除的候选（决策轨迹用）。
// 即便路由失败（无可用候选等），也会返回已收集的排除项，使「仅因排空而无候选」留下轨迹。
func (e *Engine) RouteAllWithExcluded(ctx context.Context, spec RouteSpec) (*RouteAllResult, error) {
	cands, extID, excluded, err := e.collectCandidates(ctx, spec)
	if err != nil {
		return &RouteAllResult{Excluded: excluded}, err
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
			Switch:          c.switchDecision,
			CrossCacheRead:  c.crossCacheRead,
			CrossCacheKnown: c.crossCacheReadKnown,
		})
	}
	return &RouteAllResult{Routes: out, Excluded: excluded}, nil
}

// collectCandidates 解析对外模型并返回排序后的候选集合：
// 首位为会话命中候选（可排空，不可禁用），其余为双正常候选（按成本最优排序）。
// 同时返回路由层被排除的候选（决策轨迹用；路由失败时也返回已收集者）。
func (e *Engine) collectCandidates(ctx context.Context, spec RouteSpec) ([]candidate, int64, []ExcludedCandidate, error) {
	ext, err := e.models.GetByExternalName(ctx, spec.ExternalModel)
	if err != nil {
		if errors.Is(err, model.ErrNotFound) {
			return nil, 0, nil, modelNotFound(spec.ExternalModel)
		}
		return nil, 0, nil, fmt.Errorf("resolve external model: %w", err)
	}

	cms, err := e.cmSrc.ListByExternal(ctx, ext.ID)
	if err != nil {
		return nil, 0, nil, fmt.Errorf("list bound channel models: %w", err)
	}
	if len(cms) == 0 {
		// 对外模型未绑定任何渠道内部模型 → 记录排除项（可定位到「无绑定」）。
		return nil, 0, []ExcludedCandidate{{Reason: ExcludeNoBinding}}, noAvailable()
	}
	// 每行绑定记录都是独立候选：同渠道/密钥下多个内部模型各自展开（若按行展开前先归并，
	// 会导致某个内部模型禁用时渠道整体被跳过，或退而用同渠道另一行顶替，语义都会错乱；
	// 禁用/排空某内部模型应只跳过该行）。ListByExternal 已按 channel_id,id 排序。
	// 同时把该行的成本单价与模型级时段配置一并随身携带（成本比较用，无需二次查询）。
	internalByChannel := make(map[int64][]internalBinding, len(cms))
	for _, cm := range cms {
		internalByChannel[cm.ChannelID] = append(internalByChannel[cm.ChannelID],
			internalBinding{internal: cm.InternalModelID, rowID: cm.ID,
				costRates: cm.CostRates, timeCfg: cm.TimeConfig, contextTiers: cm.ContextTiers})
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
			// 标签过滤后为空 → 记录排除项（可定位到「标签无路由」）。
			return nil, 0, []ExcludedCandidate{{Reason: ExcludeTagNoRoute}}, noRoute()
		}
	}
	if len(cands) == 0 {
		// 无可用候选（密钥/内部模型均不可用）→ 记录排除项。
		return nil, 0, []ExcludedCandidate{{Reason: ExcludeNoCandidate}}, noAvailable()
	}

	// 会话命中：命中的 (密钥,模型) 即使排空也放行（禁用已被 usable 排除），并排首位。
	// 命中判定为密钥级：在候选集内找 ChannelKeyID+InternalModelID 精确匹配的候选移到首位，
	// 避免同渠道多密钥共享会话时轮询切到非归属密钥。
	now := e.routeNow(spec)
	sample, globalTC := e.costContext()
	if spec.Sample != nil {
		sample = *spec.Sample // 本轮样值覆盖全局 routing.cost_sample
		if *spec.Sample == (billing.Usage{}) {
			slog.Default().Warn("路由收到本轮全零样值，成本择优将退化为会话数/ID 兜底；如非本意请改传 nil 以回落全局 routing.cost_sample")
		}
	}
	globalTiers := e.contextTiers()
	normal := make([]candidate, 0, len(cands))
	var excluded []ExcludedCandidate
	var sessionCand *candidate
	for i := range cands {
		c := cands[i]
		if spec.SessionKey != "" && sessionCand == nil {
			if e.sess.Hit(spec.SessionKey) {
				if s, ok := e.sess.Lookup(spec.SessionKey); ok &&
					s.ChannelKeyID == c.keyID && s.InternalModelID == c.internal {
					e.fillCost(&cands[i], sample, globalTC, globalTiers, now)
					sessionCand = &cands[i]
					continue
				}
			}
		}
		if c.mState == channel.StateNormal && c.keyState == channel.StateNormal {
			normal = append(normal, c)
		} else {
			// 排空（非会话命中）候选被剔出新会话候选集 → 记录排除项（不再静默丢失）。
			excluded = append(excluded, ExcludedCandidate{
				ChannelID:       c.id,
				ChannelName:     c.chName,
				ChannelKeyID:    c.keyID,
				InternalModelID: c.internal,
				Reason:          ExcludeDrained,
			})
		}
	}
	// 会话成本路由——起点择优（Step 10）：
	//   - 起点 = 会话注册表无活跃亲和（Hit 未命中；已关闭/过期的历史残留行不构成亲和，起点无旧缓存）；
	//   - 仅当 SessionCostConfig.Enabled=true 且为起点时生效：按「本轮花费最小」（起点重灌代价=0）择优；
	//   - 此分支刻意不经会话亲和置顶：起点语义下忽略亲和（即便存在历史亲和残留），一律按成本择优；
	//   - Enabled=false 或非起点 → 完全保持现状（亲和优先 + 原排序），渠道不变。
	// 样值来源：优先 RouteSpec.SessionTokens（本轮可得三段），其次 RouteSpec.Sample，最后回落全局 sample。
	cfg := e.sessionCostConfig()
	hit := e.sess.Hit(spec.SessionKey)
	// 会话成本路由样值来源：优先 RouteSpec.SessionTokens（本轮可得），其次 RouteSpec.Sample，
	// 最后回落全局 sample；起点择优与中途切换/影子记录共用同一样值口径。
	// 路由时刻通常只能拿到「总输入」（缓存读/输出需上游响应后才知），故按 routing.sample_profile
	// 的比例把「只有总输入」的样值拆成三段（缓存读/纯输入/输出），避免退化为「只按输入价择优」。
	sessionSample := sample
	if spec.SessionTokens != nil {
		sessionSample = e.expandSample(*spec.SessionTokens)
	} else if spec.Sample != nil {
		sessionSample = e.expandSample(*spec.Sample)
	}
	if cfg.Enabled && !hit {
		picked := e.orderStartPick(normal, now, sessionSample, globalTC, globalTiers, cfg, spec.SessionKey, spec.SessionTurn)
		if len(picked) == 0 {
			return nil, 0, excluded, noAvailable()
		}
		return picked, ext.ID, excluded, nil
	}
	// 会话成本路由——中途切换（Step 13，P2 子开关 MidSwitch）：
	//   - 中途 = 会话活跃亲和命中（Hit）且归属渠道在候选集内（sessionCand 非空），非起点；
	//   - 仅当 Enabled 与 MidSwitch 同时为 true 时才评估切换；否则保持会话亲和、渠道不变（零变化）；
	//   - midSwitch 只产出「切换到哪个候选」的决策（目标候选携带决策产物），**不在路由阶段迁移会话**；
	//     迁移与事件由网关在目标候选通过可用性/限流校验后经 ConfirmSessionSwitch 完成（F1）。
	//   - 影子观测（P0，需 cfg.ShadowEnabled=true，默认关闭）：未启用切换但开启影子时，记录
	//     「若启用则会切换」的假设节省事件（emitShadowDecision），渠道保持现状、不迁移、不写会话。
	if cfg.Enabled && cfg.MidSwitch && hit && sessionCand != nil {
		if switched, ok := e.midSwitch(sessionCand, normal, now, sessionSample, globalTC, globalTiers, cfg, spec.SessionKey, spec.SessionTurn); ok {
			return switched, ext.ID, excluded, nil
		}
	} else if cfg.ShadowEnabled && !(cfg.Enabled && cfg.MidSwitch) && e.sessionCost != nil && hit && sessionCand != nil {
		e.emitShadowDecision(sessionCand, normal, now, sessionSample, globalTC, globalTiers, cfg, spec.SessionKey, spec.SessionTurn)
	}
	ordered := e.orderCandidates(normal, now, sample, globalTC, globalTiers)
	if sessionCand != nil {
		return append([]candidate{*sessionCand}, ordered...), ext.ID, excluded, nil
	}
	if len(ordered) == 0 {
		// 仅因排空而无候选：excluded 已携带排空项，随结果一并返回。
		return nil, 0, excluded, noAvailable()
	}
	return ordered, ext.ID, excluded, nil
}

// internalBinding 某渠道的一条内部模型绑定（同一对外模型下同渠道/密钥可有多个内部模型，各为独立候选）。
type internalBinding struct {
	internal     string
	rowID        int64                    // channel_models 行 ID（状态机回喂与模型级限流定位用）
	costRates    billing.Rates            // 该行成本单价
	timeCfg      *billing.TimeCoeffConfig // 该行模型级时段配置
	contextTiers []billing.TierRule       // 该行模型级上下文分档
}

// usableCandidates 返回「密钥非禁用」的候选，按内部模型行逐条展开
// （候选 = 每 (渠道,密钥,内部模型) 一条），含排空候选：会话命中时允许其续行；
// 普通选择由上层过滤为「模型/密钥都正常」。
// 某内部模型禁用/排空只跳过该行：同一渠道/密钥下的其他内部模型行仍正常展开候选。
// 展开顺序仅为收集顺序：渠道按快照遍历、同渠道按内部模型行 id 升序、各自按密钥 id 升序，
// 最终顺序由 orderCandidates 决定。
//
// 快照一致性（显式成文，不默认；P3「非原子快照」的既定取舍）：
// 本方法对 channel.Manager 的读取为多次独立取锁——Snapshot()（渠道元信息/绑定标签）、
// ModelViews()（内部模型状态）、KeyOrderForRouting()/GetKeyRuntime()（密钥状态）——
// 因此同一次路由可能取自略有时序差的多个视图（TOCTOU）。这里显式接受该「最终一致」语义：
//   - 各视图自身都是一致快照（各自在 Manager.mu 读锁内采集），不存在撕裂读；
//   - 跨视图错位仅在「路由进行中恰好发生配置/状态变更」时出现，其结果仍是一个当时合法的候选
//     集合（可能少/多包含一个刚变更的行），下一次路由即自愈，不产生错误渠道绑定；
//   - 故不引入跨视图单锁快照，保持改动外科手术式（避免触及 NewEngine 签名与全部路由用例）。
//
// 若将来要求「单次路由内各视图严格同一时刻」，应新增 Manager 单锁快照方法收口此处。
func (e *Engine) usableCandidates(internalByChannel map[int64][]internalBinding) []candidate {
	cands := make([]candidate, 0, 16)
	for _, v := range e.chMgr.Snapshot() {
		// 协议可用性由网关按「入站协议 ∈ 模型可达 protocols」判定（AC-4/AC-18）；
		// 路由层不做协议过滤；渠道无独立状态，可用性由密钥/内部模型三态决定。
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
			// 密钥级展开：同渠道多个可用密钥按密钥 id 升序平铺（KeyOrderForRouting 不轮转），state != DISABLED 才候选
			for _, keyID := range e.chMgr.KeyOrderForRouting(v.ID) {
				kr, ok := e.chMgr.GetKeyRuntime(keyID)
				if !ok || kr.Machine.State() == channel.StateDisabled {
					continue
				}
				cands = append(cands, candidate{
					id:           v.ID,
					chName:       v.Name,
					keyID:        keyID,
					modelRow:     modelRowID,
					internal:     b.internal,
					boundTags:    v.BoundTags,
					priority:     v.Priority,
					revision:     v.UpdatedAt.UnixNano(),
					costRates:    b.costRates,
					timeCfg:      b.timeCfg,
					contextTiers: b.contextTiers,
					mState:       mState,
					keyState:     kr.Machine.State(),
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
// 成本标量 = 样值用量代入 cost_rates 求和 × 当前时段系数 × 上下文分档系数。
// 样值、全局时段与全局分档由 collectCandidates 只读一次并传入，逐候选仅做纯计算，不产生额外 IO。
// globalTiers 为全局上下文分档（collectCandidates 传入本轮读到的全局分档）；为空时按无分档（系数 1）处理。
func (e *Engine) orderCandidates(cands []candidate, now time.Time, sample billing.Usage, globalTC *billing.TimeCoeffConfig, globalTiers []billing.TierRule) []candidate {
	if len(cands) == 0 {
		return cands
	}
	scored := make([]scoredCandidate, len(cands))
	for i := range cands {
		c := cands[i]
		e.fillCost(&c, sample, globalTC, globalTiers, now)
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

// routeNow 求本次路由时刻：RouteSpec.Now 非零时优先（网关组装），否则回落引擎时钟 e.now()。
func (e *Engine) routeNow(spec RouteSpec) time.Time {
	if !spec.Now.IsZero() {
		return spec.Now
	}
	return e.now()
}

// startPick 起点择优的中间态：附决策产物（含本轮花费 TurnCostNew）、成本有效性与会话负载。
type startPick struct {
	c        candidate
	d        SessionCostDecision
	known    bool
	load     int
	pickCost float64
}

// orderStartPick 起点择优：按「本轮花费最小」重排候选并返回（首位即选中渠道）。
//
// 语义（与 session_cost.go 的 decideSessionCost 一致）：
//   - 起点 cur=nil：无旧渠道可比较，重灌代价=0、回本门控不介入，priority 硬分层内按 argmin(本轮花费) 择优；
//   - 本轮花费 = perTurnCost(候选 ChannelCost, sessionSample)：三段成本单价 × 时段系数 × 上下文分档系数；
//   - priority 为硬分层（与默认排序同语义）：高优先级候选永远优先，成本只在同优先级层内比较；
//   - 成本未知（costRates 缺失/全 0）视为最贵，排在有有效成本候选之后（与默认排序口径一致）；
//   - 起点忽视会话亲和：本函数只作用于「双正常」候选（sessionCand 已被排除在外，不会被置顶）；
//   - 排序：priority 降序（硬分层）→ 有有效成本者优先 → 本轮花费升序 → 存活会话数升序 → 稳定 id（渠道/模型行/密钥）兜底，保证确定（无随机）。
//   - 若注入了 costSink，则把选中候选的决策产物（起点）提交落库；sink 未注入或提交失败仅记日志、不阻断路由。
//
// globalTiers 为全局上下文分档（为空回落无分档系数 1）；now 为本次路由时刻。逐候选仅做纯计算，不产生额外 IO。
func (e *Engine) orderStartPick(cands []candidate, now time.Time, sessionSample billing.Usage, globalTC *billing.TimeCoeffConfig, globalTiers []billing.TierRule, cfg SessionCostConfig, sessionID string, turn int) []candidate {
	if len(cands) == 0 {
		return cands
	}
	picks := make([]startPick, len(cands))
	for i := range cands {
		c := cands[i]
		tc, ctx := resolveCostCoeffs(c.timeCfg, globalTC, c.contextTiers, globalTiers, now, sessionSample)
		cost, known := costScalar(sessionSample, c.costRates, tc, ctx)
		c.timeCoeff = tc
		c.cost = cost
		c.costKnown = known
		cc := ChannelCost{
			Name:      c.internal,
			Input:     c.costRates["input"],
			CacheRead: c.costRates["cache_read"],
			Output:    c.costRates["output"],
			TimeCoeff: tc,
			CtxCoeff:  ctx,
		}
		d := decideSessionCost(cfg, nil, cc, sessionSample.CacheRead, sessionSample.Input, sessionSample.Output, turn)
		d.ToChannelKeyID = c.keyID
		picks[i] = startPick{c: c, d: d, known: known, load: e.sess.CountActive(c.keyID), pickCost: d.TurnCostNew}
	}
	sort.SliceStable(picks, func(i, j int) bool {
		a, b := picks[i], picks[j]
		// priority 是硬分层：高优先级候选永远优先于低优先级候选（与默认排序同语义，成本只在同层内比较）。
		if a.c.priority != b.c.priority {
			return a.c.priority > b.c.priority
		}
		if a.known != b.known {
			return a.known // 有有效成本者优先（缺价视为最贵）
		}
		if a.known && a.pickCost != b.pickCost {
			return a.pickCost < b.pickCost // 同优先级内：本轮花费最小者优先
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
	out := make([]candidate, len(picks))
	for i := range picks {
		out[i] = picks[i].c
	}
	if e.costSink != nil && sessionID != "" {
		if err := e.EmitSessionCostEvent(sessionID, turn, picks[0].d); err != nil {
			slog.Default().Warn("会话成本决策产物提交失败", "session_id", sessionID, "turn", turn, "err", err)
		}
	}
	return out
}

// midSwitch 评估「会话中途切换」（Step 13）。
//
//   - cur：当前会话归属渠道候选（会话活跃亲和命中，非起点）；
//   - alts：其余「密钥与模型都正常」的候选（不含 cur），作为可切换目标；
//   - 返回 (重排后的候选序列, true) 表示已**决策**切换；返回 (nil, false) 表示保持亲和（渠道不变）。
//
// 重要：本函数只做决策，**不迁移会话、不发事件**。原因（F1）：路由阶段尚不知道目标候选能否真正
// 承载本次请求（随后还要过密钥/模型状态复查、模型级与密钥级限流、并发会话上限）。若在此处迁移，
// 一旦目标候选被限流/跳过，会话归属会偏置到从未服务过的渠道，且并发计数错位。
// 故决策产物挂在目标候选上（candidate.switchDecision，原归属渠道见 Decision.FromChannelKeyID），
// 由网关在目标候选通过全部校验后调用 Engine.ConfirmSessionSwitch 完成迁移 + 补发事件。
//
// 保守计价（D6）：一律按最坏情况计入一次性重灌代价，不检测 provider 缓存失效（压缩/TTL 空闲过期网关不可感知）；
// 实现上直接复用纯函数 decideSessionCost（其重灌代价与回本门控即保守口径）。
//
// 抖动抑制（P2，参数全部取自 SessionCostConfig，见 cost_config.go 字段注释）：
//   - MinRemaining：最小剩余轮数门槛——经验式剩余轮数低于该门槛时不切（剩余太少，切换无意义）；
//   - CooldownSec：切换冷却——距本会话上次切换（Session.LastSwitchAt）不足冷却时长则跳过；
//   - MinImprove：滞回阈值——仅当新渠道本轮成本相对当前渠道的改善超过该比例才切，避免成本接近时来回横跳。
func (e *Engine) midSwitch(cur *candidate, alts []candidate, now time.Time, sample billing.Usage, globalTC *billing.TimeCoeffConfig, globalTiers []billing.TierRule, cfg SessionCostConfig, sessionID string, turn int) ([]candidate, bool) {
	if cur == nil || len(alts) == 0 {
		return nil, false
	}
	// 抖动抑制（门槛）：剩余轮数低于 MinRemaining 直接保持亲和。
	if remainingTurns(turn, cfg) < cfg.MinRemaining {
		return nil, false
	}
	// 抖动抑制（冷却）：距上次切换未满 CooldownSec 则跳过。
	if cfg.CooldownSec > 0 {
		if t, ok := e.sess.LastSwitchAt(sessionID); ok && !t.IsZero() &&
			now.Sub(t) < time.Duration(cfg.CooldownSec)*time.Second {
			return nil, false
		}
	}

	curTC, curCtx := resolveCostCoeffs(cur.timeCfg, globalTC, cur.contextTiers, globalTiers, now, sample)
	curCC := channelCost(cur, curTC, curCtx)

	bestIdx := -1
	var bestD SessionCostDecision
	var bestCC ChannelCost
	var bestTC, bestCtx float64
	for i := range alts {
		c := alts[i]
		if !hasValidCost(c.costRates) {
			continue // 成本未知（缺价/全 0）不作切换目标（保守）
		}
		tc, ctx := resolveCostCoeffs(c.timeCfg, globalTC, c.contextTiers, globalTiers, now, sample)
		cc := channelCost(&alts[i], tc, ctx)
		d := decideSessionCost(cfg, &curCC, cc, sample.CacheRead, sample.Input, sample.Output, turn)
		if !d.Switch {
			continue // 回本门控未通过（剩余轮数不足，或分母≤0 永不回本）
		}
		if !hysteresisAllows(d.TurnCostOld, d.TurnCostNew, cfg.MinImprove) {
			continue // 滞回：相对改善未超阈值
		}
		if bestIdx == -1 || d.TurnCostNew < bestD.TurnCostNew {
			bestIdx, bestD, bestCC = i, d, cc
			bestTC, bestCtx = tc, ctx
		}
	}
	if bestIdx == -1 {
		return nil, false
	}

	// 回填成本标量 / 时段系数（F4）：目标候选会成为 routes[0]，其 cost/timeCoeff 会进入
	// 调用日志的决策轨迹；不回填会显示「成本 0 / 未知」，与「选中更便宜渠道」自相矛盾。
	target := alts[bestIdx]
	target.timeCoeff = bestTC
	if cost, known := costScalar(sample, target.costRates, bestTC, bestCtx); known {
		target.cost = cost
		target.costKnown = true
	} else {
		target.cost = 0
		target.costKnown = false
	}

	bestD.FromChannelKeyID = cur.keyID
	bestD.ToChannelKeyID = target.keyID
	// 决策轨迹附带本次的临界缓存读（crossCacheRead）：旧渠道 vs 新渠道每轮成本打平的缓存读量，
	// 供控制台「会话成本路由」面板按会话聚合分位/直方（未知=两渠道缓存读价相等，则标记 Known=false）。
	target.crossCacheRead, target.crossCacheReadKnown = crossCacheRead(curCC, bestCC, sample.Input, sample.Output)
	// 复制一份决策产物挂在候选上（避免多个候选共享同一指针被后续改写）。
	d := bestD
	target.switchDecision = &d

	// 目标渠道优先；原归属渠道作为**首选回退**紧随其后（它仍是健康的、且已建立缓存），
	// 最后是其余候选按既有择序规则续排。
	//
	// 为什么必须保留原归属渠道（F1）：引擎只做决策、不迁移会话；真正的迁移发生在网关
	// （ConfirmSessionSwitch），若目标密钥并发会话已满而迁移失败，网关会跳过该候选。
	// 此时若候选序列里没有原渠道，请求就只能落到其它渠道或 503——既丢掉了「保持亲和」的
	// 兜底，也与「迁移失败即保持原渠道」的意图不符。
	out := make([]candidate, 0, len(alts)+1)
	out = append(out, target, *cur)
	rest := make([]candidate, 0, len(alts)-1)
	for i := range alts {
		if i != bestIdx {
			rest = append(rest, alts[i])
		}
	}
	out = append(out, e.orderCandidates(rest, now, sample, globalTC, globalTiers)...)
	return out, true
}

// ConfirmSessionSwitch 在网关确认「会话中途切换」的目标候选真正被采用（已通过可用性/限流校验）后调用：
//   - 经 SessionRegistry.Migrate 把会话归属迁到目标候选（并发上限校验 + 滑动 TTL，失败原子回退）；
//   - 迁移成功后补发一条决策产物事件（记录「确实发生」的切换），避免为未采用的候选留假事件。
//
// 返回 false 表示迁移失败（目标密钥并发会话数已达上限）——调用方应保持原渠道并继续下一候选。
// ttl ≤ 0 时只刷新活动时间、不延长 ExpireAt（Migrate 语义）。
func (e *Engine) ConfirmSessionSwitch(sessionID string, cr *ChannelRoute, turn int, ttl time.Duration) bool {
	if sessionID == "" || cr == nil || cr.Switch == nil {
		return false
	}
	if !e.sess.Migrate(sessionID, cr.ChannelKeyID, cr.InternalModelID, e.keyMaxSessions(cr.ChannelKeyID), ttl) {
		return false
	}
	ev := SessionCostEvent{SessionID: sessionID, Turn: turn, Decision: *cr.Switch,
		CrossCacheRead: cr.CrossCacheRead, CrossCacheReadKnown: cr.CrossCacheKnown}
	if err := e.emitCostEvent(ev); err != nil {
		slog.Default().Warn("会话成本决策产物提交失败（中途切换）", "session_id", sessionID, "turn", turn, "err", err)
	}
	return true
}

// emitShadowDecision 记录一条「影子假设」事件（Step 6 / FIX-A）：当会话成本路由未启用切换
// （!cfg.Enabled 或 !cfg.MidSwitch）但存在会话成本 provider 时由 collectCandidates 调用，
// 对「若启用中途切换则会切换」的最优候选计算 decideSessionCost，经 shadowDecision 标记为
// 影子/未切换后经既有 emitCostEvent 提交一条观测事件；**不改变最终选中渠道**（调用方继续按
// 下方「亲和优先」原语义路由，且不执行会话迁移）——保证 Enabled=false 时对外路由行为零变化。
//
// 选靶口径与 midSwitch 一致：在「命中回本门控 + 滞回阈值」的候选里取本轮成本最低者；
// 若没有任何候选「若启用则会切换」，则不产生影子记录（无假设节省可记）。
func (e *Engine) emitShadowDecision(cur *candidate, alts []candidate, now time.Time, sample billing.Usage, globalTC *billing.TimeCoeffConfig, globalTiers []billing.TierRule, cfg SessionCostConfig, sessionID string, turn int) {
	if cur == nil || len(alts) == 0 || sessionID == "" || e.costSink == nil {
		return
	}
	curTC, curCtx := resolveCostCoeffs(cur.timeCfg, globalTC, cur.contextTiers, globalTiers, now, sample)
	curCC := channelCost(cur, curTC, curCtx)

	bestIdx := -1
	var bestD SessionCostDecision
	for i := range alts {
		c := alts[i]
		if !hasValidCost(c.costRates) {
			continue // 成本未知（缺价/全 0）不作切换目标（保守，与 midSwitch 一致）
		}
		tc, ctx := resolveCostCoeffs(c.timeCfg, globalTC, c.contextTiers, globalTiers, now, sample)
		cc := channelCost(&alts[i], tc, ctx)
		d := decideSessionCost(cfg, &curCC, cc, sample.CacheRead, sample.Input, sample.Output, turn)
		if !d.Switch {
			continue // 若启用也不会切换（回本门控未通过）→ 无假设节省可记
		}
		if !hysteresisAllows(d.TurnCostOld, d.TurnCostNew, cfg.MinImprove) {
			continue // 滞回：相对改善未超阈值
		}
		if bestIdx == -1 || d.TurnCostNew < bestD.TurnCostNew {
			bestIdx, bestD = i, d
		}
	}
	if bestIdx == -1 {
		return // 若启用也不会切换 → 不产生影子记录
	}
	best := alts[bestIdx]
	bestD.FromChannelKeyID = cur.keyID
	bestD.ToChannelKeyID = best.keyID
	shadow := shadowDecision(cfg, bestD)
	if shadow.Switch {
		// cfg.Enabled=true 但 MidSwitch=false：shadowDecision 只门控 Enabled，此处原样返回；
		// 显式标记为影子/未切换，保证事件语义与「渠道实际未变」一致。
		shadow.Switch = false
		shadow.Reason = "影子模式(中途切换未启用，仅记录假设): " + shadow.Reason
	}
	ev := SessionCostEvent{SessionID: sessionID, Turn: turn, Decision: shadow, Shadow: true}
	if err := e.emitCostEvent(ev); err != nil {
		slog.Default().Warn("会话成本影子决策产物提交失败", "session_id", sessionID, "turn", turn, "err", err)
	}
}

// channelCost 由候选构造其成本计价视图（三档成本单价 + 时段系数 + 上下文分档系数）。
func channelCost(c *candidate, timeCoeff, ctxCoeff float64) ChannelCost {
	return ChannelCost{
		Name:      c.internal,
		Input:     c.costRates["input"],
		CacheRead: c.costRates["cache_read"],
		Output:    c.costRates["output"],
		TimeCoeff: timeCoeff,
		CtxCoeff:  ctxCoeff,
	}
}

// hysteresisAllows 滞回判据：仅当新渠道本轮成本较当前渠道的「相对改善」超过阈值 minImprove 才允许切换。
// minImprove ≤ 0 时退化为「新渠道必须严格更便宜」（仍受 decideSessionCost 的回本门控约束）。
func hysteresisAllows(curTurnCost, newTurnCost, minImprove float64) bool {
	if !(newTurnCost < curTurnCost) {
		return false
	}
	if minImprove <= 0 {
		return true
	}
	return (curTurnCost - newTurnCost) > minImprove*curTurnCost
}

// keyMaxSessions 返回目标密钥的并发会话数上限（读所属渠道 rate_limit.max_concurrent，0 兜底默认）；
// 未装配渠道运行时（chMgr=nil）时返回 0（Migrate 按不限处理）。供中途切换迁移时校验并发上限。
func (e *Engine) keyMaxSessions(keyID int64) int {
	if e.chMgr == nil {
		return 0
	}
	return e.chMgr.KeyMaxSessions(keyID)
}

// fillCost 计算候选当前的时段系数、上下文分档系数与成本标量，并回填到候选字段
// （OQ-3：随候选携带至网关决策轨迹，便于排查「为何选中该渠道」）。
// globalTiers 为全局上下文分档（collectCandidates 传入本轮全局分档）；为空时按无分档（系数 1）处理。
func (e *Engine) fillCost(c *candidate, sample billing.Usage, globalTC *billing.TimeCoeffConfig, globalTiers []billing.TierRule, now time.Time) {
	tc, ctxCoeff := resolveCostCoeffs(c.timeCfg, globalTC, c.contextTiers, globalTiers, now, sample)
	cost, known := costScalar(sample, c.costRates, tc, ctxCoeff)
	c.timeCoeff = tc
	c.cost = cost
	c.costKnown = known
}

// expandSample 把「只有总输入」的样值补齐为三段（缓存读 / 纯输入 / 输出）。
//
// 背景：路由时刻网关只能可靠得到「总输入 token」（由请求体估算），缓存读与输出要等上游响应后
// 才知。若直接把「总输入」塞进 Input 段参与比较，成本标量退化为「只按输入价择优」，
// 与「缓存读主导」的实际成本结构相悖。故按全局 routing.sample_profile 的比例拆分：
//
//	总输入 P（= 纯输入 + 缓存读）：纯输入 = P / (1 + cache_read_ratio)
//	                             缓存读  = P − 纯输入
//	                             输出    = 纯输入 × output_ratio
//
// 仅在「Input > 0 且 CacheRead == 0 且 Output == 0」时补齐（即确为「只有总输入」的样值）；
// 其余情况（已含缓存读/输出，或全零显式样值）原样返回，保持既有语义不变。
// 比例读取失败/未注入时原样返回（不猜测，保持改动前行为）。
func (e *Engine) expandSample(u billing.Usage) billing.Usage {
	if u.Input <= 0 || u.CacheRead != 0 || u.Output != 0 || e.costCfg == nil {
		return u
	}
	prof, err := e.costCfg.SampleProfile()
	if err != nil {
		slog.Default().Warn("路由样值补齐比例读取失败，仅按纯输入样值比较", "err", err)
		return u
	}
	ratio := prof.CacheReadRatio
	if ratio < 0 || math.IsNaN(ratio) || math.IsInf(ratio, 0) {
		ratio = 0
	}
	total := float64(u.Input)
	pureInput := total / (1 + ratio)
	// 先定纯输入（四舍五入），再由「总输入 − 纯输入」得缓存读：
	// 保证 Input + CacheRead 恒等于原始总输入（不因两处独立取整出现 ±1 漂移，
	// 否则分档落档依据（Input+CacheRead）会与请求实际规模不一致）。
	pi := int64(math.Round(pureInput))
	if pi < 0 {
		pi = 0
	}
	if pi > u.Input {
		pi = u.Input
	}
	cacheRead := u.Input - pi
	out := 0.0
	if prof.OutputRatio > 0 && !math.IsNaN(prof.OutputRatio) && !math.IsInf(prof.OutputRatio, 0) {
		out = float64(pi) * prof.OutputRatio
	}
	return billing.Usage{
		Input:     pi,
		CacheRead: cacheRead,
		Output:    int64(math.Round(out)),
	}
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

// contextTiers 读取一次全局上下文分档（billing.context_tiers），供本轮候选比较复用；
// 未注入配置或读取失败时返回 nil（视为无分档，系数 1）并记录日志（不阻断路由）。
func (e *Engine) contextTiers() []billing.TierRule {
	if e.costCfg == nil {
		return nil
	}
	tiers, err := e.costCfg.ContextTiers()
	if err != nil {
		slog.Default().Warn("全局上下文分档读取失败，分档系数按 1 处理", "err", err)
		return nil
	}
	return tiers
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

// resolveCtxCoeff 求候选当前上下文分档系数：tiers 取模型级（len>0）否则全局；均为空则 1；
// 分档口径为 ContextTierCoeff(sample.Input+sample.CacheRead, tiers)（含缓存读，与计费一致）；
// 解析失败回落 1 并记录日志（不阻断路由）。
func resolveCtxCoeff(modelTiers, globalTiers []billing.TierRule, sample billing.Usage) float64 {
	tiers := modelTiers
	if len(tiers) == 0 {
		tiers = globalTiers
	}
	if len(tiers) == 0 {
		return 1
	}
	v, err := billing.ContextTierCoeff(sample.Input+sample.CacheRead, tiers)
	if err != nil {
		slog.Default().Warn("上下文分档系数解析失败，按 1 处理", "err", err)
		return 1
	}
	return v
}

// resolveCostCoeffs 一次性求候选的时段系数与上下文分档系数，作为「决策」与「成本比较」的唯一致数来源：
//   - 时段：模型级 time_config 优先，为空回落全局，均无则 1（CoeffFor(now)）；
//   - 分档：模型级 context_tiers（len>0）优先，为空回落全局，均空则 1，
//     分档口径含缓存读 ContextTierCoeff(sample.Input+sample.CacheRead, tiers)（与计费一致）。
//
// 默认排序/成本比较（fillCost→costScalar）、起点择优（orderStartPick）与中途切换（midSwitch）
// 统一从本函数取系数，保证「决策用的 timeCoeff/ctxCoeff」与「成本比较用的 timeCoeff/ctxCoeff」
// 乃至 billing.ComputeCredits 的计费口径同源一致，不会出现一处含缓存读、一处不含的漂移。
func resolveCostCoeffs(modelTC, globalTC *billing.TimeCoeffConfig, modelTiers, globalTiers []billing.TierRule, now time.Time, sample billing.Usage) (timeCoeff, ctxCoeff float64) {
	return resolveTimeCoeff(modelTC, globalTC, now), resolveCtxCoeff(modelTiers, globalTiers, sample)
}

// costScalar 计算候选成本标量 = sumRates(sample, rates) × 时段系数 × 上下文分档系数。
// 返回 (标量, 是否有效)；cost_rates 缺失/全 0、或单价表不合法（缺键/负值）时返回 (0, false)，
// 由调用方按「成本未知（最贵）」处理。
func costScalar(sample billing.Usage, rates billing.Rates, timeCoeff, ctxCoeff float64) (float64, bool) {
	if !hasValidCost(rates) {
		return 0, false
	}
	total, err := billing.RawTotal(sample, rates, timeCoeff, ctxCoeff)
	if err != nil {
		return 0, false
	}
	return total, true
}

// hasValidCost 判断成本单价表是否具备有效值：至少一段非 0（全 0/空视为成本未知）。
// 口径说明（FR-5）：仅当三段全为 0 或缺失才算「成本未知」；若某候选仅 cache_read 有单价
// 而样值 cache_read=0，其标量为 0 且仍视为有效（按最便宜参与比较），这是既定样值口径。
func hasValidCost(rates billing.Rates) bool {
	for _, v := range rates {
		if v != 0 {
			return true
		}
	}
	return false
}
