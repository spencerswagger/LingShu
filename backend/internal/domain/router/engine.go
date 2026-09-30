// Package router 实现 LLM 网关的路由选择（双层模型）：对外模型 → 绑定该对外模型的
// 渠道内部模型 → 渠道选择（健康 + 标签/亲和/轮询）→ internal_model_id 发给上游。
//
// ## 路由决策（两层级联状态机重构后）
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
//  6. 按 priority 降序分层（高层优先，高层未耗尽不降层），同层渠道按 weight 加权随机决定
//     起始顺序并循环排列；并发会话上限由网关在会话创建时校验；
//  7. 候选为空 → ErrNoAvailable；
package router

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"sort"
	"time"

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
}

// ChannelRoute 单个候选渠道的路由信息，供网关在超限/429 时按序切换渠道（failover）。
type ChannelRoute struct {
	ChannelID       int64
	ChannelKeyID    int64 // 候选渠道密钥（B3 起候选为 (渠道,密钥,模型) 三元组）
	InternalModelID string
	ModelRowID      int64
	ExternalModelID int64
	Revision        int64
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

// affinityTTL 缺省会话保持时长（渠道 session_ttl_minutes 覆盖，TTL 常量便于测试引用）。
const affinityTTL = time.Hour

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

// intRanger 随机整数源（*rand.Rand 满足）。
type intRanger interface {
	Intn(n int) int
}

// Engine 是路由选择引擎。
type Engine struct {
	models extModelSource     // 对外模型查询
	cmSrc  channelModelSource // 渠道内部模型查询
	chMgr  *channel.Manager   // 渠道运行时快照（渠道级 + 模型级状态）
	sess   *SessionRegistry   // 会话注册表（亲和 + 存活会话计数）
	rng    intRanger          // 同层加权随机的随机源
}

// NewEngine 创建路由引擎。sess 为 nil 时使用默认会话注册表。
func NewEngine(models extModelSource, cm channelModelSource, chMgr *channel.Manager, sess *SessionRegistry) *Engine {
	if sess == nil {
		sess = NewSessionRegistry(nil)
	}
	return &Engine{
		models: models, cmSrc: cm, chMgr: chMgr, sess: sess,
		rng: rand.New(rand.NewSource(1)),
	}
}

// SetRand 注入随机源（测试用）。
func (e *Engine) SetRand(r intRanger) { e.rng = r }

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
	weight    int
	revision  int64
	chState   channel.State
	mState    channel.State
	keyState  channel.State
}

// Route 执行对外模型 → 绑定渠道内部模型 → 标签匹配 → 亲和 → 优先级分层 + 层内加权随机。
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
	}, nil
}

// RouteAll 返回全部可用候选渠道：
//   - 会话命中（含排空，禁用除外）排首；
//   - 其余为「渠道与模型都正常」的候选，按 priority 降序分层、同层按 weight 加权随机轮转。
//
// 供网关在超限/429 时按序切换渠道（failover）。
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
		})
	}
	return out, nil
}

// collectCandidates 解析对外模型并返回排序后的候选集合：
// 首位为会话命中候选（可排空，不可禁用），其余为双正常候选（按 ID 升序）。
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
	internalByChannel := make(map[int64][]internalBinding, len(cms))
	for _, cm := range cms {
		internalByChannel[cm.ChannelID] = append(internalByChannel[cm.ChannelID],
			internalBinding{internal: cm.InternalModelID, rowID: cm.ID})
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
	normal := make([]candidate, 0, len(cands))
	var sessionCand *candidate
	for i := range cands {
		c := cands[i]
		if spec.SessionKey != "" && sessionCand == nil {
			if e.sess.Hit(spec.SessionKey) {
				if s, ok := e.sess.Lookup(spec.SessionKey); ok &&
					s.ChannelKeyID == c.keyID && s.InternalModelID == c.internal {
					sessionCand = &cands[i]
					continue
				}
			}
		}
		if c.chState == channel.StateNormal && c.mState == channel.StateNormal && c.keyState == channel.StateNormal {
			normal = append(normal, c)
		}
	}
	if sessionCand != nil {
		return append([]candidate{*sessionCand}, normal...), ext.ID, nil
	}
	if len(normal) == 0 {
		return nil, 0, noAvailable()
	}
	return orderByLayerAndWeight(normal, e.rng), ext.ID, nil
}

// internalBinding 某渠道的一条内部模型绑定（同一对外模型下同渠道/密钥可有多个内部模型，各为独立候选）。
type internalBinding struct {
	internal string
	rowID    int64 // channel_models 行 ID（状态机回喂与模型级限流定位用）
}

// usableCandidates 返回「渠道非禁用 且 密钥非禁用」的候选，按内部模型行逐条展开
// （候选 = 每 (渠道,密钥,内部模型) 一条），含排空候选：会话命中时允许其续行；
// 普通选择由上层过滤为「渠道/模型/密钥都正常」。
// 某内部模型禁用/排空只跳过该行：同一渠道/密钥下的其他内部模型行仍正常展开候选。
// 展开顺序仅为收集顺序：渠道按快照遍历、同渠道按内部模型行 id 升序、各自按密钥轮询顺序，
// 最终顺序由 orderByLayerAndWeight 决定。
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
					weight:    v.Weight,
					revision:  v.UpdatedAt.UnixNano(),
					chState:   v.State,
					mState:    mState,
					keyState:  kr.Machine.State(),
				})
			}
		}
	}
	return cands
}

// orderByLayerAndWeight 按 priority 降序分层；每层内以渠道 weight 加权随机决定起始顺序，
// 返回重排后的候选（同一渠道的多个密钥/模型候选保持连续且内部次序不变）。
func orderByLayerAndWeight(cands []candidate, rng intRanger) []candidate {
	type group struct {
		chID   int64
		weight int
		items  []candidate
	}
	order := make([]*group, 0)
	byCh := make(map[int64]*group)
	for i := range cands {
		g := byCh[cands[i].id]
		if g == nil {
			g = &group{chID: cands[i].id, weight: cands[i].weight}
			byCh[cands[i].id] = g
			order = append(order, g)
		}
		g.items = append(g.items, cands[i])
	}

	layers := make([][]*group, 0)
	for _, g := range order {
		placed := false
		for li := range layers {
			if layers[li][0].items[0].priority == g.items[0].priority {
				layers[li] = append(layers[li], g)
				placed = true
				break
			}
		}
		if !placed {
			layers = append(layers, []*group{g})
		}
	}
	sort.Slice(layers, func(i, j int) bool {
		return layers[i][0].items[0].priority > layers[j][0].items[0].priority
	})

	out := make([]candidate, 0, len(cands))
	for _, layer := range layers {
		total := 0
		for _, g := range layer {
			w := g.weight
			if w < 1 {
				w = 1
			}
			total += w
		}
		start := 0
		if total > 0 && rng != nil {
			pos := rng.Intn(total)
			acc := 0
			for i, g := range layer {
				w := g.weight
				if w < 1 {
					w = 1
				}
				acc += w
				if pos < acc {
					start = i
					break
				}
			}
		}
		for k := 0; k < len(layer); k++ {
			g := layer[(start+k)%len(layer)]
			out = append(out, g.items...)
		}
	}
	return out
}
