package router

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/asyncwrite"
)

// 会话成本决策产物落库（Step 7）：把「选中渠道 / 本轮花费 / 重灌代价 / 回本轮数 / 决策原因」
// 等决策产物经既有 asyncwrite 写后异步持久化，供账实校准与决策轨迹排查。
//
// 安全态：KindSessionCostEvent 默认不在 AsyncKinds 中 → 未显式登记时同步执行（不异步），
// 不改变任何既有默认行为；只有装配层显式登记后才走异步。

// CostEventSink 接收会话成本决策产物并负责落库（异步或同步由实现决定）。
// 引擎未注入（nil）时决策后保持 no-op，绝不阻断路由。
type CostEventSink interface {
	Submit(ev SessionCostEvent) error
}

// SessionCostEvent 是一条会话成本决策产物事件（决策轨迹 / 审计留痕）。
// 其 JSON 序列化即 asyncwrite 任务的 Payload；Seq 与 SessionID 组成幂等键。
type SessionCostEvent struct {
	SessionID string `json:"session_id"`
	Turn      int    `json:"turn"`
	// Seq 是本次发射的单调唯一序号（由 sink 在 Submit 时分配，随 Payload 落 WAL）。
	// 仅用于构成 asyncwrite 幂等键：Turn 现由会话轮次（SessionRegistry.Turns）驱动、可重复取值
	// （如影子记录与真实切换可能同轮次），故不能单独作为幂等键；
	// 以 Seq 保证「每一次独立决策」唯一，避免同轮次多次决策撞键被去重丢弃；
	// 同一条记录崩溃重放时复用同一 Seq，保持「同一次发射」幂等。
	Seq      uint64              `json:"seq"`
	Decision SessionCostDecision `json:"decision"`
	// Shadow 标记本条为「影子假设记录」（未启用切换时仍产出「若启用则会切换」的假设节省记录）：
	// true 表示渠道实际未变、仅记录假设；此时 Decision.Switch 恒为 false，Decision.Reason 带影子前缀。
	Shadow bool `json:"shadow"`
	// CrossCacheRead 是本次决策的「临界缓存读」：旧渠道缓存读 vs 新渠道缓存读在给定用量下
	// 每轮成本打平的缓存读 token 量（由 crossCacheRead 求得，来源=模型成本×真实 token）。
	// CrossCacheReadKnown=false 表示无解（两渠道缓存读价相等）或起点择优（无旧渠道可比），此时值无意义。
	CrossCacheRead      float64 `json:"cross_cache_read"`
	CrossCacheReadKnown bool    `json:"cross_cache_read_known"`
}

// asyncCostEventSink 是 CostEventSink 的 asyncwrite 实现：决策产物经写后异步落库。
//
// 保序与幂等（不允许隐性乱序 / 隐性重复）：
//   - Key = session_id：同一会话的全部事件被哈希到同一分片串行执行，保证同会话决策产物保序；
//   - IdempotencyKey = session_id + ":" + Seq：Seq 每次发射单调唯一，保证「每一次独立决策」各自留痕
//     （Turn 来自会话轮次、可能在同一轮次内产生多条独立决策，不足以区分），同一条记录重放时复用同一键（进程内去重）；
//   - Payload 编码完整决策产物，崩溃重启后由 KindSessionCostEvent 重放函数重建副作用；
//   - 失败任务保留 WAL 记录待重启重放（不静默丢失决策轨迹）。
type asyncCostEventSink struct {
	w *asyncwrite.Writer
	// persist 是可选落地回调：事件成功执行时调用（默认 nil = 仅经 asyncwrite 入队 / WAL 留痕，
	// 供尚未接线落地存储的阶段使用）。作为实例字段而非包级变量，避免并行测试共享可变状态
	// （与 asyncwrite.fsyncCounter、channel.droppedReplays 的测试范式一致）。
	persist func(ctx context.Context, ev SessionCostEvent) error
	// seq 是发射序号计数器（提交时 Add(1) 取号）；构造时以 UnixNano 为基准，避免进程重启后
	// 新序号与崩溃前 WAL 遗留记录的序号相撞而被误去重。
	seq atomic.Uint64
}

// 编译期断言：asyncCostEventSink 满足 CostEventSink。
var _ CostEventSink = (*asyncCostEventSink)(nil)

// NewAsyncCostEventSink 创建会话成本决策产物的异步 sink，并注册 KindSessionCostEvent 的重放函数。
// w 为 nil 时各提交方法同步直调落地回调（安全兜底）。
//
// 可选透传 stores[0]：把每条事件的执行体（persist 回调）接到该有界进程内存储（单实例假设，
// 不引入 Redis / 同步往返）。控制台「会话成本路由」面板的 /events 与 /distribution 即读该存储。
// 未传 store 时行为与既有完全一致（仅经 asyncwrite 入队 / WAL 留痕，persist 为 nil）。
func NewAsyncCostEventSink(w *asyncwrite.Writer, stores ...*SessionCostEventStore) CostEventSink {
	s := &asyncCostEventSink{w: w}
	s.seq.Store(uint64(time.Now().UnixNano()))
	if len(stores) > 0 && stores[0] != nil {
		store := stores[0]
		s.persist = func(_ context.Context, ev SessionCostEvent) error {
			store.Append(ev)
			return nil
		}
	}
	if w != nil {
		w.SetReplayFunc(asyncwrite.KindSessionCostEvent, s.replay)
	}
	return s
}

// Submit 提交一条会话成本决策产物：Key=session_id 保序，IdempotencyKey=session_id:Seq 幂等。
// Seq 由本 sink 在提交时分配（每次发射单调唯一），确保同一会话内「每一次独立决策」各自留痕：
// Turn 现由会话轮次驱动、可在同一轮次内产生多条独立决策（如影子记录、来回切换），
// 若用 session:turn（或再叠加 from>to 转移）会撞键而被 asyncwrite 静默去重丢弃；Seq 随 Payload 落 WAL，
// 同一条记录崩溃重放时仍复用同一键，保持「同一次发射」幂等。
func (s *asyncCostEventSink) Submit(ev SessionCostEvent) error {
	ev.Seq = s.nextSeq()
	if s.w == nil {
		return s.persistEvent(context.Background(), ev)
	}
	payload, err := marshalEventForJSON(ev)
	if err != nil {
		return err
	}
	return s.w.Submit(asyncwrite.Job{
		Kind:           asyncwrite.KindSessionCostEvent,
		Key:            ev.SessionID,
		IdempotencyKey: idempotencyKey(ev),
		Payload:        payload,
		Exec: func(c context.Context) error {
			return s.persistEvent(c, ev)
		},
	})
}

// idempotencyKey 由 SessionID 与每次发射唯一的 Seq 组成（语义见 Submit 注释）。
func idempotencyKey(ev SessionCostEvent) string {
	return ev.SessionID + ":" + strconv.FormatUint(ev.Seq, 10)
}

// nextSeq 返回本 sink 内单调唯一的发射序号（构造时已以 UnixNano 为基准，见字段注释）。
func (s *asyncCostEventSink) nextSeq() uint64 { return s.seq.Add(1) }

// sessionCostEventWire 是 SessionCostEvent 的 WAL 线格式：在原有字段之外额外记录
// 「非有限浮点（NaN/±Inf）」的字段名。
// 原因：encoding/json 无法编码 NaN/Inf（报 "unsupported value: NaN"），会导致整条事件提交失败、
// 中途切换事件被静默丢弃，故编码前须把这些值规整为 0；但仅归零会让「未知」在崩溃重启重放后
// 退化成 0（前端把 NaN 渲染为 null=未知，0 会被当成真实值）。故用 __non_finite 记录字段名，
// 重放时据此还原为 NaN，保证 WAL 往返不改变语义。
type sessionCostEventWire struct {
	SessionCostEvent
	NonFinite []string `json:"__non_finite,omitempty"`
}

// 线格式中「非有限浮点」字段标识（与 Payload 的 JSON 键一致）。
const (
	fieldCrossCacheRead = "cross_cache_read"
	fieldTurnCostOld    = "TurnCostOld"
	fieldTurnCostNew    = "TurnCostNew"
	fieldRechargeCost   = "RechargeCost"
	fieldPaybackTurns   = "PaybackTurns"
	fieldPaybackLimit   = "PaybackLimit"
)

// isNonFiniteFloat 判定 NaN/±Inf。
func isNonFiniteFloat(v float64) bool { return math.IsNaN(v) || math.IsInf(v, 0) }

// marshalEventForJSON 把决策产物编码为 asyncwrite 的 Payload：非有限浮点先归零再序列化，
// 并在 __non_finite 记录其字段名，供 unmarshalEventFromJSON 还原为 NaN。
// 仅作用于「序列化副本」，不修改传入的 ev（运行时/内存事件存储仍保留 NaN）。
func marshalEventForJSON(ev SessionCostEvent) ([]byte, error) {
	w := sessionCostEventWire{SessionCostEvent: ev}
	mark := func(name string, v *float64) {
		if isNonFiniteFloat(*v) {
			*v = 0
			w.NonFinite = append(w.NonFinite, name)
		}
	}
	mark(fieldCrossCacheRead, &w.CrossCacheRead)
	mark(fieldTurnCostOld, &w.Decision.TurnCostOld)
	mark(fieldTurnCostNew, &w.Decision.TurnCostNew)
	mark(fieldRechargeCost, &w.Decision.RechargeCost)
	mark(fieldPaybackTurns, &w.Decision.PaybackTurns)
	mark(fieldPaybackLimit, &w.Decision.PaybackLimit)
	return json.Marshal(w)
}

// unmarshalEventFromJSON 是 marshalEventForJSON 的逆：解码后按 __non_finite 还原为 NaN。
// 兼容旧格式（无 __non_finite）：此时非有限值退化为 0，与历史行为一致。
func unmarshalEventFromJSON(payload []byte) (SessionCostEvent, error) {
	var w sessionCostEventWire
	if err := json.Unmarshal(payload, &w); err != nil {
		return SessionCostEvent{}, fmt.Errorf("decode session cost event payload: %w", err)
	}
	ev := w.SessionCostEvent
	nan := math.NaN()
	for _, name := range w.NonFinite {
		switch name {
		case fieldCrossCacheRead:
			ev.CrossCacheRead = nan
		case fieldTurnCostOld:
			ev.Decision.TurnCostOld = nan
		case fieldTurnCostNew:
			ev.Decision.TurnCostNew = nan
		case fieldRechargeCost:
			ev.Decision.RechargeCost = nan
		case fieldPaybackTurns:
			ev.Decision.PaybackTurns = nan
		case fieldPaybackLimit:
			ev.Decision.PaybackLimit = nan
		}
	}
	return ev, nil
}

// persistEvent 调用可选的落地回调；未设置时 no-op（该阶段仅经 asyncwrite 入队 / WAL 留痕）。
func (s *asyncCostEventSink) persistEvent(ctx context.Context, ev SessionCostEvent) error {
	if s.persist == nil {
		return nil
	}
	return s.persist(ctx, ev)
}

// replay 崩溃恢复重放：解码决策产物并交落地回调重建副作用（无回调时 no-op 幂等）。
func (s *asyncCostEventSink) replay(ctx context.Context, _ string, payload []byte) error {
	ev, err := unmarshalEventFromJSON(payload)
	if err != nil {
		return err
	}
	return s.persistEvent(ctx, ev)
}

// defaultSessionCostEventCap 是会话成本决策产物进程内存储的默认容量（有界环形，防无界增长）。
const defaultSessionCostEventCap = 1000

// SessionCostEventStore 是会话成本决策产物（KindSessionCostEvent）的有界进程内落地存储。
//
// 设计（单实例假设，符合「不引入 Redis / 同步往返」）：
//   - 有界环形缓冲：容量固定，写满后覆盖最旧事件，保证长跑进程内存有界（不无界增长）；
//   - 并发安全：读（Snapshot/Len）与写（Append）均持互斥锁，供 asyncwrite 工作协程并发落地、
//     控制台并发读取（-race 安全）；
//   - 只读快照：Snapshot 返回副本并按写入顺序（旧→新）展开，调用方不得修改共享底层数组。
//
// 定位：进程内「决策轨迹」观测缓存（重启即清零，不承诺持久化——持久化由 asyncwrite WAL + 重放兜底）；
// 故容量溢出仅意味着最旧样本被淘汰，不影响路由与计费。
type SessionCostEventStore struct {
	mu   sync.Mutex
	cap  int
	buf  []SessionCostEvent
	next int // 环形写指针：写满后指向最旧（将被覆盖）的位置
}

// NewSessionCostEventStore 创建有界事件存储；capacity ≤ 0 时使用默认容量。
func NewSessionCostEventStore(capacity int) *SessionCostEventStore {
	if capacity <= 0 {
		capacity = defaultSessionCostEventCap
	}
	return &SessionCostEventStore{cap: capacity}
}

// Append 追写一条事件（并发安全）：未满则尾插；已满则覆盖最旧（环形）。
func (s *SessionCostEventStore) Append(ev SessionCostEvent) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cap <= 0 {
		s.cap = defaultSessionCostEventCap
	}
	if len(s.buf) < s.cap {
		s.buf = append(s.buf, ev)
		return
	}
	s.buf[s.next] = ev
	s.next = (s.next + 1) % s.cap
}

// Len 返回当前存活事件数。
func (s *SessionCostEventStore) Len() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.buf)
}

// Snapshot 返回全部事件副本，按写入顺序（旧→新）展开；空存储返回非 nil 空切片。
func (s *SessionCostEventStore) Snapshot() []SessionCostEvent {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	n := len(s.buf)
	if n == 0 {
		return []SessionCostEvent{}
	}
	out := make([]SessionCostEvent, 0, n)
	if s.cap <= 0 || n < s.cap {
		return append(out, s.buf...)
	}
	out = append(out, s.buf[s.next:]...)
	out = append(out, s.buf[:s.next]...)
	return out
}

// ResolveTimeCoeff 导出路由侧的时段系数解析口径（模型级 time_config 优先，为空回落全局，均无则 1），
// 供控制台「会话成本路由」面板与路由决策保持同源（避免两处口径漂移）。
func ResolveTimeCoeff(modelTC, globalTC *billing.TimeCoeffConfig, now time.Time) float64 {
	return resolveTimeCoeff(modelTC, globalTC, now)
}

// ResolveCtxCoeff 导出路由侧的上下文分档系数解析口径（模型级 context_tiers 优先，为空回落全局，
// 均空则 1；口径含缓存读 Input+CacheRead），供控制台面板与路由决策同源。
func ResolveCtxCoeff(modelTiers, globalTiers []billing.TierRule, sample billing.Usage) float64 {
	return resolveCtxCoeff(modelTiers, globalTiers, sample)
}
