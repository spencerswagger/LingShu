package channel

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/team/llmgateway/internal/pkg/cronx"
	"github.com/team/llmgateway/internal/pkg/crypto"
)

// DefaultRateLimit 返回渠道限流默认配置。
func DefaultRateLimit() RateLimitConfig {
	return RateLimitConfig{
		RPM:             1000,
		TPM:             1000000,
		BurstMultiplier: 1.2,
		OnExceed:        OnExceedQueue,
		QueueSize:       100,
		QueueTimeoutMS:  5000,
		MaxConcurrent:   16,
	}
}

// DefaultHealthProbe 返回健康探测默认配置（cron 调度 + 排空秒间隔）。
func DefaultHealthProbe() HealthProbeConfig {
	return HealthProbeConfig{
		Interval:             "0 * * * * *", // 默认每分钟
		DrainIntervalSeconds: 15,            // 排空态每 15 秒
		TimeoutMS:            15000,         // 探测超时默认 15 秒
		FailThreshold:        1,             // 一次失败即排空
		RecoveryThreshold:    2,             // 连续两次成功恢复
		ProbeModel:           "",
	}
}

// DefaultReliability 返回可靠性（真实调用滑动窗口）默认配置。
func DefaultReliability() ReliabilityConfig {
	return ReliabilityConfig{
		WindowSeconds:     60,
		MinSamples:        10,
		ErrorRatePct:      10,
		Rate429Pct:        20,
		P99LatencyMS:      5000,
		AuthFailThreshold: 3,
	}
}

// DefaultSessionTTLMinutes 会话存活时长默认值（分钟）。
const DefaultSessionTTLMinutes = 60

// normalizeRateLimit 以默认值兜底零值字段。
func normalizeRateLimit(r RateLimitConfig) RateLimitConfig {
	def := DefaultRateLimit()
	if r.RPM == 0 {
		r.RPM = def.RPM
	}
	if r.TPM == 0 {
		r.TPM = def.TPM
	}
	if r.BurstMultiplier == 0 {
		r.BurstMultiplier = def.BurstMultiplier
	}
	if r.OnExceed == "" {
		r.OnExceed = def.OnExceed
	}
	if r.QueueSize == 0 {
		r.QueueSize = def.QueueSize
	}
	if r.QueueTimeoutMS == 0 {
		r.QueueTimeoutMS = def.QueueTimeoutMS
	}
	if r.MaxConcurrent == 0 {
		r.MaxConcurrent = def.MaxConcurrent
	}
	return r
}

// normalizeHealthProbe 以默认值兜底零值字段。
func normalizeHealthProbe(h HealthProbeConfig) HealthProbeConfig {
	def := DefaultHealthProbe()
	if stringsBlank(h.Interval) {
		h.Interval = def.Interval
	}
	if h.DrainIntervalSeconds <= 0 {
		h.DrainIntervalSeconds = def.DrainIntervalSeconds
	}
	if h.TimeoutMS <= 0 {
		h.TimeoutMS = def.TimeoutMS
	}
	if h.FailThreshold <= 0 {
		h.FailThreshold = def.FailThreshold
	}
	if h.RecoveryThreshold <= 0 {
		h.RecoveryThreshold = def.RecoveryThreshold
	}
	return h
}

// normalizeReliability 以默认值兜底零值字段。
func normalizeReliability(r ReliabilityConfig) ReliabilityConfig {
	def := DefaultReliability()
	if r.WindowSeconds <= 0 {
		r.WindowSeconds = def.WindowSeconds
	}
	if r.MinSamples <= 0 {
		r.MinSamples = def.MinSamples
	}
	if r.ErrorRatePct <= 0 {
		r.ErrorRatePct = def.ErrorRatePct
	}
	if r.Rate429Pct <= 0 {
		r.Rate429Pct = def.Rate429Pct
	}
	if r.P99LatencyMS <= 0 {
		r.P99LatencyMS = def.P99LatencyMS
	}
	if r.AuthFailThreshold <= 0 {
		r.AuthFailThreshold = def.AuthFailThreshold
	}
	return r
}

func stringsBlank(s string) bool { return s == "" }

// RuntimeChannel 渠道配置模板运行时：静态信息 + 状态机 + 限流器（作为「渠道视图」保留，
// 供现有 gateway/router/console 消费方继续编译；渠道自身可用性只认三态 state）。
// Channel.State 以内存为准；渠道层不含凭据，凭据只在 KeyRuntime.CredentialPlain。
// Keys 为该渠道全部密钥运行动态视图引用（同一指针，B3 起消费方经 Keys 取密钥）。
type RuntimeChannel struct {
	Channel   Channel
	Machine   *Machine
	Limiter   *Limiter
	LastErr   string
	lastProbe time.Time
	Keys      []*KeyRuntime // 该渠道全部密钥运行时视图（同一指针，KeyRuntime 独有状态机/限流器）
}

// KeyRuntime 单个密钥的完整运行时（密钥=运行时实体，渠道=配置模板）。
// 每个密钥独立持有状态机 Machine 与限流窗口 Limiter；渠道配置经 Channel 只读引用。
type KeyRuntime struct {
	Key             ChannelKey
	Channel         *Channel // 共享渠道配置只读引用
	CredentialPlain string   // 解密后的明文凭据（SM4Decrypt 成功才有值；失败置空）
	Machine         *Machine // 密钥级状态机（阈值与现 buildRuntime 同源：渠道 health_probe/reliability）
	Limiter         *Limiter // 密钥级限流窗口（渠道 rate_limit 配置）
	// Reliability 密钥级可靠性窗口由 Machine 滑动窗口承载（窗口语义与阈值同源）。
	lastProbe time.Time
	CreatedAt time.Time
}

// KeyRuntimeView 是密钥运行时输出视图：凭据仅暴露尾号，绝不回传明文或密文。
type KeyRuntimeView struct {
	KeyID          int64  `json:"KeyID,string"`
	Name           string `json:"Name"`
	State          State  `json:"State"`
	LastErr        string `json:"LastErr,omitempty"`
	CredentialTail string `json:"CredentialTail,omitempty"` // 解密明文尾号（≤6 位整体脱敏），不含明文/密文
}

// ModelRuntime 内部模型运行时：与渠道运行时完全同构（状态机/限流器/探测），
// 仅配置缺省时以所属渠道配置兜底。
type ModelRuntime struct {
	Model     ChannelModel
	Machine   *Machine
	Limiter   *Limiter
	LastErr   string
	lastProbe time.Time
}

// ChannelView 是 API 输出结构：附当前状态/LastErr/探测时间及有效配置（凭据已收敛到 channel_keys）。
type ChannelView struct {
	ID            int64             `json:"ID,string"`
	Name          string            `json:"Name"`
	Protocol      string            `json:"Protocol"`
	BaseURL       string            `json:"BaseURL"`
	Tags          map[string]string `json:"Tags"`
	BoundTags     []TagRef          `json:"BoundTags"`
	Priority      int               `json:"Priority"`
	Weight        int               `json:"Weight"`
	State         State             `json:"State"`
	LastErr       string            `json:"LastErr,omitempty"`
	LastProbe     *time.Time        `json:"LastProbe,omitempty"`
	RateLimit     RateLimitConfig   `json:"RateLimit"`
	HealthProbe   HealthProbeConfig `json:"HealthProbe"`
	Reliability   ReliabilityConfig `json:"Reliability"`
	MaxSessions   int               `json:"MaxSessions"`
	SessionTTLMin int               `json:"SessionTTLMin"`
	CreatedAt     time.Time         `json:"CreatedAt"`
	UpdatedAt     time.Time         `json:"UpdatedAt"`
}

// ModelView 是内部模型运行时输出：状态/LastErr/探测时间 + 有效配置。
type ModelView struct {
	ID              int64             `json:"ID,string"`
	InternalModelID string            `json:"InternalModelID"`
	ExternalModelID int64             `json:"ExternalModelID,string"`
	State           State             `json:"State"`
	LastErr         string            `json:"LastErr,omitempty"`
	LastProbe       *time.Time        `json:"LastProbe,omitempty"`
	RateLimit       RateLimitConfig   `json:"RateLimit"`
	HealthProbe     HealthProbeConfig `json:"HealthProbe"`
	Reliability     ReliabilityConfig `json:"Reliability"`
	MaxSessions     int               `json:"MaxSessions"`
}

// ProbeRecorder 探测开销记账钩子（装配层注入：以系统身份记入 billing_records）。
type ProbeRecorder func(ctx context.Context, chID int64, modelID, target string, ok bool, errMsg string, usage ProbeUsage, durationMS int64) error

// Manager 渠道配置模板 + 密钥运行动态管理器：内存态 + 状态机 + 限流器 + 健康探测调度。
// rt = 渠道配置模板运行时（保留 Machine/Limiter 字段供现有消费方继续编译）；
// keys = 每个 channel_keys 一行一套独立运行时（状态机/限流器）；keyOfCh 为按渠道枚举索引。
type Manager struct {
	mu        sync.RWMutex
	rt        map[int64]*RuntimeChannel         // channelID → 渠道配置模板运行时
	mrt       map[int64]map[int64]*ModelRuntime // channelID → modelRowID → 模型运行时（模型保持渠道级，不复制到 KeyRuntime）
	keys      map[int64]*KeyRuntime             // keyID → 密钥运行时（每密钥独立 Machine/Limiter）
	keyOfCh   map[int64]map[int64]bool          // channelID → keyID 集合
	keyOrder  map[int64][]int64                 // channelID → 密钥路由轮询顺序（KeyOrderForRouting 旋转维护）
	boundTags map[int64][]TagRef                // channelID → 渠道已绑定标签（内存视图）
	store     *Store
	cmStore   *channelModelStore
	keyStore  *KeyStore // 密钥存储（List/ListAll）；nil 表示未装配（B2 起装配层注入）

	probeHTTP *http.Client
	sm4Key    []byte
	logger    *slog.Logger
	now       func() time.Time
	onProbe   ProbeRecorder

	cancelCtx context.Context
	cancel    context.CancelFunc
	wg        sync.WaitGroup

	probeCtx     context.Context // Start 时保存的探测上下文；运行期新增密钥据此启动探测循环
	probeRunning map[int64]bool  // 已启动探测循环的密钥 ID（避免重复 go）
}

// NewManager 创建双运行时管理器。keyStore 为可选变参：不传（或 nil）表示密钥运行时未装配，
// SyncFromDB/Upsert 将跳过密钥装载，现有调用方不受影响（B2 装配时传入）。
func NewManager(store *Store, cmStore *channelModelStore, sm4Key []byte, probeHTTP *http.Client, logger *slog.Logger, now func() time.Time, keyStore ...*KeyStore) *Manager {
	if probeHTTP == nil {
		probeHTTP = &http.Client{Timeout: 5 * time.Second}
	}
	if logger == nil {
		logger = slog.Default()
	}
	if now == nil {
		now = time.Now
	}
	var ks *KeyStore
	if len(keyStore) > 0 && keyStore[0] != nil {
		ks = keyStore[0]
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Manager{
		rt:           make(map[int64]*RuntimeChannel),
		mrt:          make(map[int64]map[int64]*ModelRuntime),
		keys:         make(map[int64]*KeyRuntime),
		keyOfCh:      make(map[int64]map[int64]bool),
		keyOrder:     make(map[int64][]int64),
		boundTags:    make(map[int64][]TagRef),
		probeRunning: make(map[int64]bool),
		store:        store,
		cmStore:      cmStore,
		keyStore:     ks,
		probeHTTP:    probeHTTP,
		sm4Key:       sm4Key,
		logger:       logger,
		now:          now,
		cancelCtx:    ctx,
		cancel:       cancel,
	}
}

// Stop 取消探测 goroutine 并等待退出。
func (m *Manager) Stop() {
	m.cancel()
	m.wg.Wait()
}

// SetProbeRecorder 注入探测开销记账钩子。
func (m *Manager) SetProbeRecorder(fn ProbeRecorder) { m.onProbe = fn }

// buildRuntime 由静态渠道构建渠道运行时：按配置建状态机与限流器。
// 渠道层不含凭据：密钥凭据解密见 buildKeyRuntime（KeyRuntime.CredentialPlain）。
func (m *Manager) buildRuntime(ch *Channel) *RuntimeChannel {
	rt := &RuntimeChannel{Channel: *ch}

	// 状态机配置 = 可靠性（窗口）+ 探测阈值（失败/恢复）
	hp := normalizeHealthProbe(ch.HealthProbe)
	rel := normalizeReliability(ch.Reliability)
	mcfg := DefaultMachineConfig()
	mcfg.ProbeFailThreshold = hp.FailThreshold
	mcfg.ProbeRecoveryThreshold = hp.RecoveryThreshold
	mcfg.WindowSeconds = rel.WindowSeconds
	mcfg.ErrorRatePct = rel.ErrorRatePct
	mcfg.Rate429Pct = rel.Rate429Pct
	mcfg.P99LatencyMS = rel.P99LatencyMS
	mcfg.MinSamples = rel.MinSamples
	mcfg.AuthFailThreshold = rel.AuthFailThreshold
	mach := NewMachine(mcfg)
	mach.SetClock(m.now)
	mach.transition(ch.State, m.now(), "")
	rt.Machine = mach

	rl := normalizeRateLimit(ch.RateLimit)
	rt.Limiter = NewLimiter(rl.RPM, rl.TPM, rl.BurstMultiplier)
	rt.Limiter.SetClock(m.now)
	return rt
}

// buildKeyRuntime 由密钥（+渠道模板）构建密钥运行时：
//   - 凭据 SM4 解密出明文（失败：CredentialPlain 置空 + LastErr 提示，沿用渠道凭据 lastErr 风格）；
//   - 独立 Machine（阈值与 buildRuntime 同源：渠道 health_probe/reliability）；
//   - 独立 Limiter（渠道 rate_limit 配置）。
func (m *Manager) buildKeyRuntime(ch *Channel, k ChannelKey) *KeyRuntime {
	kr := &KeyRuntime{Key: k, Channel: ch, CreatedAt: k.CreatedAt}
	if k.CredentialEnc == "" {
		kr.Key.LastErr = "密钥凭据为空，请先配置后再启用"
	} else if plain, err := crypto.SM4Decrypt(m.sm4Key, k.CredentialEnc); err == nil {
		kr.CredentialPlain = string(plain)
	} else {
		kr.CredentialPlain = ""
		kr.Key.LastErr = "密钥凭据解密失败，请检查 SM4 密钥配置"
		m.logger.Error("decrypt key credential failed", "key_id", k.ID, "err", kr.Key.LastErr)
	}

	// 状态机配置 = 可靠性（窗口）+ 探测阈值（失败/恢复），与 buildRuntime 完全同源。
	hp := normalizeHealthProbe(ch.HealthProbe)
	rel := normalizeReliability(ch.Reliability)
	mcfg := DefaultMachineConfig()
	mcfg.ProbeFailThreshold = hp.FailThreshold
	mcfg.ProbeRecoveryThreshold = hp.RecoveryThreshold
	mcfg.WindowSeconds = rel.WindowSeconds
	mcfg.ErrorRatePct = rel.ErrorRatePct
	mcfg.Rate429Pct = rel.Rate429Pct
	mcfg.P99LatencyMS = rel.P99LatencyMS
	mcfg.MinSamples = rel.MinSamples
	mcfg.AuthFailThreshold = rel.AuthFailThreshold
	mach := NewMachine(mcfg)
	mach.SetClock(m.now)
	mach.transition(k.State, m.now(), "")
	kr.Machine = mach

	rl := normalizeRateLimit(ch.RateLimit)
	kr.Limiter = NewLimiter(rl.RPM, rl.TPM, rl.BurstMultiplier)
	kr.Limiter.SetClock(m.now)
	return kr
}

// buildModelRuntime 由内部模型构建模型运行时；配置缺省项以所属渠道配置兜底。
func (m *Manager) buildModelRuntime(cm *ChannelModel, chRT *RuntimeChannel) *ModelRuntime {
	mr := &ModelRuntime{Model: *cm}

	// 限流：模型级 0 值 → 渠道级 → 默认
	effRL := normalizeRateLimit(mergeRateLimit(cm.RateLimit, chRT.Channel.RateLimit))
	mr.Limiter = NewLimiter(effRL.RPM, effRL.TPM, effRL.BurstMultiplier)
	mr.Limiter.SetClock(m.now)

	// 状态机：可靠性（模型→渠道→默认）+ 探测阈值（模型→渠道→默认）
	rel := mergeReliability(cm.Reliability, chRT.Channel.Reliability)
	hp := mergeHealthProbe(cm.HealthProbe, chRT.Channel.HealthProbe)
	mcfg := DefaultMachineConfig()
	mcfg.ProbeFailThreshold = hp.FailThreshold
	mcfg.ProbeRecoveryThreshold = hp.RecoveryThreshold
	mcfg.WindowSeconds = rel.WindowSeconds
	mcfg.ErrorRatePct = rel.ErrorRatePct
	mcfg.Rate429Pct = rel.Rate429Pct
	mcfg.P99LatencyMS = rel.P99LatencyMS
	mcfg.MinSamples = rel.MinSamples
	mcfg.AuthFailThreshold = rel.AuthFailThreshold
	mach := NewMachine(mcfg)
	mach.SetClock(m.now)
	mach.transition(cm.State, m.now(), "")
	mr.Machine = mach
	return mr
}

func mergeRateLimit(model, ch RateLimitConfig) RateLimitConfig {
	if model.RPM == 0 {
		model.RPM = ch.RPM
	}
	if model.TPM == 0 {
		model.TPM = ch.TPM
	}
	if model.BurstMultiplier == 0 {
		model.BurstMultiplier = ch.BurstMultiplier
	}
	if model.OnExceed == "" {
		model.OnExceed = ch.OnExceed
	}
	if model.QueueSize == 0 {
		model.QueueSize = ch.QueueSize
	}
	if model.QueueTimeoutMS == 0 {
		model.QueueTimeoutMS = ch.QueueTimeoutMS
	}
	if model.MaxConcurrent == 0 {
		model.MaxConcurrent = ch.MaxConcurrent
	}
	return model
}

func mergeReliability(model, ch ReliabilityConfig) ReliabilityConfig {
	if model.WindowSeconds == 0 {
		model.WindowSeconds = ch.WindowSeconds
	}
	if model.MinSamples == 0 {
		model.MinSamples = ch.MinSamples
	}
	if model.ErrorRatePct == 0 {
		model.ErrorRatePct = ch.ErrorRatePct
	}
	if model.Rate429Pct == 0 {
		model.Rate429Pct = ch.Rate429Pct
	}
	if model.P99LatencyMS == 0 {
		model.P99LatencyMS = ch.P99LatencyMS
	}
	if model.AuthFailThreshold == 0 {
		model.AuthFailThreshold = ch.AuthFailThreshold
	}
	return model
}

func mergeHealthProbe(model, ch HealthProbeConfig) HealthProbeConfig {
	if stringsBlank(model.Interval) {
		model.Interval = ch.Interval
	}
	if model.DrainIntervalSeconds == 0 {
		model.DrainIntervalSeconds = ch.DrainIntervalSeconds
	}
	if model.TimeoutMS == 0 {
		model.TimeoutMS = ch.TimeoutMS
	}
	if model.FailThreshold == 0 {
		model.FailThreshold = ch.FailThreshold
	}
	if model.RecoveryThreshold == 0 {
		model.RecoveryThreshold = ch.RecoveryThreshold
	}
	if stringsBlank(model.ProbeModel) {
		model.ProbeModel = ch.ProbeModel
	}
	return model
}

// SessionTTL 返回渠道会话存活时长（分钟），0 兜底默认。
func (m *Manager) SessionTTL(channelID int64) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if rt, ok := m.rt[channelID]; ok && rt.Channel.SessionTTLMinutes > 0 {
		return rt.Channel.SessionTTLMinutes
	}
	return DefaultSessionTTLMinutes
}

// MaxSessions 返回渠道并发会话数上限（0 兜底默认）。
func (m *Manager) MaxSessions(channelID int64) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if rt, ok := m.rt[channelID]; ok {
		rl := normalizeRateLimit(rt.Channel.RateLimit)
		return rl.MaxConcurrent
	}
	return DefaultRateLimit().MaxConcurrent
}

// SyncFromDB 启动时从库加载全部渠道（配置模板）与其内部模型、密钥运行时。
// 顺序：先装载渠道，再 keyStore.ListAll 装配全部 key→KeyRuntime（模型运行时重建逻辑保持现状）。
func (m *Manager) SyncFromDB(ctx context.Context) error {
	chs, err := m.store.List(ctx, "")
	if err != nil {
		return fmt.Errorf("sync channels from db: %w", err)
	}
	var allKeys []ChannelKey
	if m.keyStore != nil {
		allKeys, err = m.keyStore.ListAll(ctx)
		if err != nil {
			return fmt.Errorf("sync channel keys from db: %w", err)
		}
	}
	m.mu.Lock()
	for i := range chs {
		ch := chs[i]
		m.rt[ch.ID] = m.buildRuntime(&ch)
		if refs, rerr := m.store.ListTagRefsByChannel(ctx, ch.ID); rerr == nil {
			m.boundTags[ch.ID] = refs
		}
		if m.cmStore != nil {
			models, merr := m.cmStore.ListByChannel(ctx, ch.ID)
			if merr == nil {
				m.rebuildModelsLocked(ch.ID, models)
			}
		}
	}
	if m.keyStore != nil {
		m.rebuildKeysLocked(allKeys)
	}
	count := len(m.rt)
	m.mu.Unlock()
	m.logger.Info("synced channels from db", "count", count)
	return nil
}

// rebuildKeysLocked 重建全部密钥运行时（启动装载；调用方持有写锁或处于构建期）。
// 渠道配置模板不存在的孤儿密钥跳过。
func (m *Manager) rebuildKeysLocked(keys []ChannelKey) {
	next := make(map[int64]*KeyRuntime, len(keys))
	byCh := make(map[int64]map[int64]bool)
	for i := range keys {
		k := keys[i]
		chRT, ok := m.rt[k.ChannelID]
		if !ok {
			continue
		}
		kr := m.buildKeyRuntime(&chRT.Channel, k)
		next[k.ID] = kr
		if byCh[k.ChannelID] == nil {
			byCh[k.ChannelID] = make(map[int64]bool)
		}
		byCh[k.ChannelID][k.ID] = true
	}
	m.keys = next
	m.keyOfCh = byCh
	m.keyOrder = make(map[int64][]int64)
	m.syncAllKeysViewsLocked()
}

// rebuildChannelKeysLocked 重建某渠道全部密钥运行时（渠道更新触发）：
// 保留已存在密钥运行时的内存权威状态（配置保存不重置状态）；不在库中的密钥摘除运行时。
func (m *Manager) rebuildChannelKeysLocked(channelID int64, keys []ChannelKey) {
	chRT, ok := m.rt[channelID]
	if !ok {
		return
	}
	if m.keyOfCh[channelID] == nil {
		m.keyOfCh[channelID] = make(map[int64]bool)
	}
	keep := make(map[int64]bool, len(keys))
	for i := range keys {
		k := keys[i]
		keep[k.ID] = true
		kr := m.buildKeyRuntime(&chRT.Channel, k)
		if prev, ok := m.keys[k.ID]; ok {
			kr.Machine.transition(prev.Machine.State(), m.now(), prev.Machine.LastReason())
			kr.lastProbe = prev.lastProbe
			if kr.Key.LastErr == "" {
				kr.Key.LastErr = prev.Key.LastErr
			}
		}
		m.keys[k.ID] = kr
		m.keyOfCh[channelID][k.ID] = true
		m.startKeyProbeIfNewLocked(k.ID)
	}
	for keyID := range m.keyOfCh[channelID] {
		if !keep[keyID] {
			delete(m.keys, keyID)
			delete(m.keyOfCh[channelID], keyID)
			delete(m.probeRunning, keyID)
		}
	}
	m.keyOrder[channelID] = nil
	m.syncChannelKeysViewLocked(channelID)
}

// syncAllKeysViewsLocked 重建全部渠道运行时的 Keys 视图引用（同一指针）。
func (m *Manager) syncAllKeysViewsLocked() {
	for _, chRT := range m.rt {
		chRT.Keys = nil
		for keyID := range m.keyOfCh[chRT.Channel.ID] {
			chRT.Keys = append(chRT.Keys, m.keys[keyID])
		}
	}
}

// syncChannelKeysViewLocked 重建单渠道运行时的 Keys 视图引用（同一指针）。
func (m *Manager) syncChannelKeysViewLocked(channelID int64) {
	chRT, ok := m.rt[channelID]
	if !ok {
		return
	}
	chRT.Keys = chRT.Keys[:0]
	for keyID := range m.keyOfCh[channelID] {
		if kr, ok := m.keys[keyID]; ok {
			chRT.Keys = append(chRT.Keys, kr)
		}
	}
}

// rebuildModelsLocked 重建某渠道的全部模型运行时（调用方持有写锁或处于构建期）。
func (m *Manager) rebuildModelsLocked(channelID int64, models []ChannelModel) {
	chRT, ok := m.rt[channelID]
	if !ok {
		return
	}
	prev := m.mrt[channelID]
	next := make(map[int64]*ModelRuntime, len(models))
	for i := range models {
		cm := models[i]
		rc := cm
		if p, ok := prev[cm.ID]; ok {
			rc.State = p.Machine.State() // 保留内存权威状态（配置保存不重置模型状态）
		}
		mr := m.buildModelRuntime(&rc, chRT)
		if p, ok := prev[cm.ID]; ok {
			mr.LastErr = p.LastErr
			mr.lastProbe = p.lastProbe
		}
		next[cm.ID] = mr
	}
	m.mrt[channelID] = next
}

// Upsert 同步渠道（配置模板）到运行时：重建渠道运行时与该渠道全部模型运行时（渠道级兜底可能变化）。
// 同时重建该渠道全部 KeyRuntime（KeyStore.List）——密钥共享渠道配置，渠道更新后需以新配置刷新。
// 停用渠道（state=DISABLED）同样装载运行时，可用性只认三态 state，恢复由 ForceState 完成。
func (m *Manager) Upsert(ch *Channel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	prev := m.rt[ch.ID]
	var st State = ch.State
	if prev != nil {
		st = prev.Machine.State() // 保留内存权威状态
	}
	rclog := *ch
	rclog.State = st
	rt := m.buildRuntime(&rclog)
	if prev != nil {
		rt.Channel.CreatedAt = prev.Channel.CreatedAt
		rt.lastProbe = prev.lastProbe
		if rt.LastErr == "" {
			rt.LastErr = prev.LastErr
		}
	}
	m.rt[ch.ID] = rt
	if m.cmStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if models, err := m.cmStore.ListByChannel(ctx, ch.ID); err == nil {
			m.rebuildModelsLocked(ch.ID, models)
		}
	}
	if m.keyStore != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if keys, err := m.keyStore.List(ctx, ch.ID); err == nil {
			m.rebuildChannelKeysLocked(ch.ID, keys)
		}
	}
}

// UpsertKey 同步单个密钥到运行时（密钥 CRUD 后调用）：
// key 已存在则替换 Machine/Limiter 并保留内存权威状态；不存在则新建。
func (m *Manager) UpsertKey(k ChannelKey) {
	m.mu.Lock()
	defer m.mu.Unlock()
	chRT, ok := m.rt[k.ChannelID]
	if !ok {
		return // 渠道配置模板不存在（未装载/已删除），跳过
	}
	if m.keyOfCh[k.ChannelID] == nil {
		m.keyOfCh[k.ChannelID] = make(map[int64]bool)
	}
	prev := m.keys[k.ID]
	kr := m.buildKeyRuntime(&chRT.Channel, k)
	if prev != nil {
		kr.Machine.transition(prev.Machine.State(), m.now(), prev.Machine.LastReason())
		kr.lastProbe = prev.lastProbe
		if kr.Key.LastErr == "" {
			kr.Key.LastErr = prev.Key.LastErr
		}
	}
	m.keys[k.ID] = kr
	m.keyOfCh[k.ChannelID][k.ID] = true
	m.keyOrder[k.ChannelID] = nil
	m.syncChannelKeysViewLocked(k.ChannelID)
	m.startKeyProbeIfNewLocked(k.ID)
}

// RemoveKey 摘除密钥运行时（密钥软删后调用）：删除 keys/search 索引与渠道视图引用。
func (m *Manager) RemoveKey(keyID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kr, ok := m.keys[keyID]
	if !ok {
		return
	}
	chID := kr.Key.ChannelID
	delete(m.keys, keyID)
	if chSet, ok := m.keyOfCh[chID]; ok {
		delete(chSet, keyID)
	}
	delete(m.probeRunning, keyID)
	m.keyOrder[chID] = nil
	m.syncChannelKeysViewLocked(chID)
}

// KeyOrderForRouting 返回该渠道未删密钥的 id 列表，用于路由候选展开的轮询打散：
// 初始按密钥 id 升序，每次调用把首元素移到末尾（round-robin 游标），
// 保证同一渠道多个密钥分摊流量而非长期优先命中第一个。并发安全。
func (m *Manager) KeyOrderForRouting(channelID int64) []int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	order := m.keyOrder[channelID]
	if order == nil {
		ids := make([]int64, 0, len(m.keyOfCh[channelID]))
		for keyID := range m.keyOfCh[channelID] {
			ids = append(ids, keyID)
		}
		sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
		order = ids
		m.keyOrder[channelID] = order
	}
	if len(order) == 0 {
		return nil
	}
	out := append([]int64(nil), order...)
	m.keyOrder[channelID] = append(order[1:], order[0])
	return out
}

// UpsertModel 同步单个内部模型运行时（模型 CRUD 后调用）。
func (m *Manager) UpsertModel(cm *ChannelModel) {
	m.mu.Lock()
	defer m.mu.Unlock()
	chRT, ok := m.rt[cm.ChannelID]
	if !ok {
		// 渠道运行时不存在：移除模型运行时
		if mm, ok2 := m.mrt[cm.ChannelID]; ok2 {
			delete(mm, cm.ID)
		}
		return
	}
	prev := m.mrt[cm.ChannelID][cm.ID]
	rc := *cm
	rc.State = cm.State
	if prev != nil {
		rc.State = prev.Machine.State() // 保留内存权威状态
	}
	mr := m.buildModelRuntime(&rc, chRT)
	if prev != nil {
		mr.LastErr = prev.LastErr
		mr.lastProbe = prev.lastProbe
	}
	if m.mrt[cm.ChannelID] == nil {
		m.mrt[cm.ChannelID] = make(map[int64]*ModelRuntime)
	}
	m.mrt[cm.ChannelID][cm.ID] = mr
}

// DropModel 移除内部模型运行时（模型删除后调用）。
func (m *Manager) DropModel(channelID int64, modelRowID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mm, ok := m.mrt[channelID]; ok {
		delete(mm, modelRowID)
	}
}

// GetRuntime 返回渠道运行时；不存在返回 (nil, false)。
func (m *Manager) GetRuntime(id int64) (*RuntimeChannel, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	rt, ok := m.rt[id]
	return rt, ok
}

// GetModelRuntime 返回内部模型运行时；不存在返回 (nil, false)。
func (m *Manager) GetModelRuntime(channelID, modelRowID int64) (*ModelRuntime, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	mr, ok := m.mrt[channelID][modelRowID]
	return mr, ok
}

// GetKeyRuntime 返回密钥运行时（每个密钥独立 Machine/Limiter）；不存在返回 (nil, false)。
func (m *Manager) GetKeyRuntime(keyID int64) (*KeyRuntime, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	kr, ok := m.keys[keyID]
	return kr, ok
}

// KeyViews 返回某渠道全部密钥运行时视图（控制台/API 展示），按 keyID 升序。
// CredentialTail 仅暴露解密明文尾号，绝不回传明文/密文。
func (m *Manager) KeyViews(channelID int64) []KeyRuntimeView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	views := make([]KeyRuntimeView, 0, len(m.keyOfCh[channelID]))
	for keyID := range m.keyOfCh[channelID] {
		kr, ok := m.keys[keyID]
		if !ok {
			continue
		}
		views = append(views, KeyRuntimeView{
			KeyID:          kr.Key.ID,
			Name:           kr.Key.Name,
			State:          kr.Machine.State(),
			LastErr:        kr.Key.LastErr,
			CredentialTail: credentialTail(kr.CredentialPlain),
		})
	}
	sort.Slice(views, func(i, j int) bool { return views[i].KeyID < views[j].KeyID })
	return views
}

// KeyStateCounts 统计全部渠道密钥运行时状态分布（控制台运维统计用）。
func (m *Manager) KeyStateCounts() map[State]int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[State]int{}
	for _, kr := range m.keys {
		out[kr.Machine.State()]++
	}
	return out
}

// ModelStateCounts 统计全部渠道内部模型运行时状态分布（控制台运维统计用）。
func (m *Manager) ModelStateCounts() map[State]int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := map[State]int{}
	for _, models := range m.mrt {
		for _, mr := range models {
			out[mr.Machine.State()]++
		}
	}
	return out
}

// credentialTail 返回凭据明文末 6 位；明文不存在（解密失败/为空）返回空串；
// 明文 ≤6 位时整体脱敏为 ******，防止短凭据整体泄漏。
func credentialTail(plain string) string {
	if plain == "" {
		return ""
	}
	if len(plain) <= 6 {
		return "******"
	}
	return plain[len(plain)-6:]
}

// KeyMaxSessions 返回单个密钥的并发会话数上限（读所属渠道 rate_limit.max_concurrent，0 兜底默认）。
// 密钥共享渠道限流配置模板，各密钥独立计数。
// 注：Go 方法名不可重载，现有 MaxSessions(channelID) 保留供 gateway 等旧消费方，
// 密钥级并发上限由本方法承接（B3 起路由切键后消费）。
func (m *Manager) KeyMaxSessions(keyID int64) int {
	m.mu.RLock()
	defer m.mu.RUnlock()
	kr, ok := m.keys[keyID]
	if !ok {
		return DefaultRateLimit().MaxConcurrent
	}
	return normalizeRateLimit(kr.Channel.RateLimit).MaxConcurrent
}

// ModelViews 返回某渠道全部模型运行时视图（控制台展示）。
func (m *Manager) ModelViews(channelID int64) []ModelView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	views := make([]ModelView, 0, len(m.mrt[channelID]))
	for _, mr := range m.mrt[channelID] {
		var lp *time.Time
		if !mr.lastProbe.IsZero() {
			t := mr.lastProbe
			lp = &t
		}
		views = append(views, ModelView{
			ID:              mr.Model.ID,
			InternalModelID: mr.Model.InternalModelID,
			ExternalModelID: mr.Model.ExternalModelID,
			State:           mr.Machine.State(),
			LastErr:         mr.LastErr,
			LastProbe:       lp,
			RateLimit:       normalizeRateLimit(mergeRateLimit(mr.Model.RateLimit, m.rt[channelID].Channel.RateLimit)),
			HealthProbe:     normalizeHealthProbe(mergeHealthProbe(mr.Model.HealthProbe, m.rt[channelID].Channel.HealthProbe)),
			Reliability:     normalizeReliability(mergeReliability(mr.Model.Reliability, m.rt[channelID].Channel.Reliability)),
			MaxSessions:     normalizeRateLimit(mergeRateLimit(mr.Model.RateLimit, m.rt[channelID].Channel.RateLimit)).MaxConcurrent,
		})
	}
	return views
}

// Snapshot 返回控制台渠道视图（当前状态 + LastErr + up/down）。
func (m *Manager) Snapshot() []ChannelView {
	m.mu.RLock()
	defer m.mu.RUnlock()
	views := make([]ChannelView, 0, len(m.rt))
	for _, rt := range m.rt {
		c := rt.Channel
		var lp *time.Time
		if !rt.lastProbe.IsZero() {
			t := rt.lastProbe
			lp = &t
		}
		rl := normalizeRateLimit(c.RateLimit)
		srcRefs := m.boundTags[c.ID]
		btCp := make([]TagRef, len(srcRefs))
		copy(btCp, srcRefs)
		views = append(views, ChannelView{
			ID:            c.ID,
			Name:          c.Name,
			Protocol:      c.Protocol,
			BaseURL:       c.BaseURL,
			Tags:          c.Tags,
			BoundTags:     btCp,
			Priority:      c.Priority,
			Weight:        c.Weight,
			State:         rt.Machine.State(),
			LastErr:       rt.LastErr,
			LastProbe:     lp,
			RateLimit:     rl,
			HealthProbe:   normalizeHealthProbe(c.HealthProbe),
			Reliability:   normalizeReliability(c.Reliability),
			MaxSessions:   rl.MaxConcurrent,
			SessionTTLMin: c.SessionTTLMinutes,
			CreatedAt:     c.CreatedAt,
			UpdatedAt:     c.UpdatedAt,
		})
	}
	return views
}

// SetBoundTags 设置某渠道已绑定标签的内存视图（CRUD/SyncFromDB 后调用）。
func (m *Manager) SetBoundTags(channelID int64, refs []TagRef) {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := make([]TagRef, 0, len(refs))
	for _, r := range refs {
		kv := make(map[string]string, len(r.KV))
		for k, v := range r.KV {
			kv[k] = v
		}
		cp = append(cp, TagRef{ID: r.ID, Name: r.Name, KV: kv})
	}
	m.boundTags[channelID] = cp
}

// BoundTags 返回某渠道绑定标签快照。
func (m *Manager) BoundTags(channelID int64) []TagRef {
	m.mu.RLock()
	defer m.mu.RUnlock()
	src := m.boundTags[channelID]
	cp := make([]TagRef, 0, len(src))
	for _, r := range src {
		cp = append(cp, r)
	}
	return cp
}

// Drop 删除运行时条目（渠道删除后调用）：同时摘除该渠道全部密钥运行时。
func (m *Manager) Drop(id int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rt, id)
	delete(m.mrt, id)
	for keyID := range m.keyOfCh[id] {
		delete(m.keys, keyID)
		delete(m.probeRunning, keyID)
	}
	delete(m.keyOfCh, id)
	delete(m.keyOrder, id)
}

// persistChannelTransition 锁外写库：记录事件 + 更新渠道状态。
func (m *Manager) persistChannelTransition(id int64, from, to State, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.store.InsertEvent(ctx, &ChannelEvent{
		ChannelID: id, FromState: from, ToState: to, Reason: reason,
	}); err != nil {
		m.logger.Error("insert channel event failed", "channel_id", id, "err", err)
	}
	if err := m.store.UpdateState(ctx, id, to); err != nil && !errors.Is(err, sql.ErrNoRows) {
		m.logger.Error("update channel state failed", "channel_id", id, "err", err)
	}
}

// persistKeyTransition 锁外写库：记录密钥状态事件 + 更新 channel_keys.state（keyStore 未装配时跳过）。
// 密钥自动禁用（401/403）等由探测驱动的状态流转经此持久化，重启后仍生效。
func (m *Manager) persistKeyTransition(keyID int64, from, to State, reason string) {
	if m.keyStore == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.keyStore.InsertEvent(ctx, keyID, from, to, reason); err != nil {
		m.logger.Error("insert channel key event failed", "key_id", keyID, "err", err)
	}
	if err := m.keyStore.SetState(ctx, keyID, to, ""); err != nil {
		m.logger.Error("update channel key state failed", "key_id", keyID, "err", err)
	}
}

// persistModelTransition 锁外写库：记录模型事件 + 更新模型状态。
func (m *Manager) persistModelTransition(channelID int64, modelID string, from, to State, reason string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := m.cmStore.InsertModelEvent(ctx, &ChannelModelEvent{
		ChannelID: channelID, ModelID: modelID, FromState: from, ToState: to, Reason: reason,
	}); err != nil {
		m.logger.Error("insert model event failed", "channel_id", channelID, "model", modelID, "err", err)
	}
	if err := m.cmStore.UpdateModelState(ctx, channelID, modelID, to); err != nil && !errors.Is(err, sql.ErrNoRows) {
		m.logger.Error("update model state failed", "channel_id", channelID, "model", modelID, "err", err)
	}
}

// pendingTransition 一次状态流转（锁内捕获，锁外持久化）。
type pendingTransition struct {
	from, to State
	reason   string
}

// feedAndTransit 注入一次反馈并驱动状态机，捕获两层状态变化，锁外写事件与持久化。
// 密钥为运行时实体：真实调用同时回喂密钥状态机（keyID）与内部模型状态机（modelRowID）。
// 渠道为配置模板（批量操作层），不再由真实调用驱动渠道级状态机。
func (m *Manager) feedAndTransit(keyID int64, modelRowID int64, fb Feedback) {
	var keyTr, mTr *pendingTransition
	var channelID int64
	m.mu.Lock()
	if kr, ok := m.keys[keyID]; ok {
		channelID = kr.Key.ChannelID
		prev := kr.Machine.State()
		kr.Machine.Feed(fb)
		if cur := kr.Machine.State(); cur != prev {
			keyTr = &pendingTransition{from: prev, to: cur, reason: kr.Machine.LastReason()}
		}
	}
	if modelRowID > 0 {
		if mr, ok := m.mrt[channelID][modelRowID]; ok {
			prev := mr.Machine.State()
			mr.Machine.Feed(fb)
			if cur := mr.Machine.State(); cur != prev {
				mTr = &pendingTransition{from: prev, to: cur, reason: mr.Machine.LastReason()}
			}
		}
	}
	m.mu.Unlock()

	if keyTr != nil {
		m.persistKeyTransition(keyID, keyTr.from, keyTr.to, keyTr.reason)
		m.logger.Info("channel key state transition", "key_id", keyID,
			"from", keyTr.from, "to", keyTr.to, "reason", keyTr.reason)
	}
	if mTr != nil && modelRowID > 0 && channelID != 0 {
		if mr, ok := m.GetModelRuntime(channelID, modelRowID); ok {
			m.persistModelTransition(channelID, mr.Model.InternalModelID, mTr.from, mTr.to, mTr.reason)
			m.logger.Info("model state transition", "channel_id", channelID,
				"model", mr.Model.InternalModelID, "from", mTr.from, "to", mTr.to, "reason", mTr.reason)
		}
	}
}

// FeedResult 供网关调用后回喂结果：一次真实调用同时驱动密钥与内部模型状态机。
// 首参为 channel_key_id（密钥=运行时实体），模型为渠道配置实体的模型级回喂（双回喂保持）。
func (m *Manager) FeedResult(keyID int64, modelRowID int64, fb Feedback) {
	m.feedAndTransit(keyID, modelRowID, fb)
}

// ManualSetState 手动设置渠道状态（正常/排空/禁用），写事件并持久化。
func (m *Manager) ManualSetState(id int64, st State) error {
	m.mu.Lock()
	rt, ok := m.rt[id]
	if !ok {
		m.mu.Unlock()
		return sql.ErrNoRows
	}
	prev := rt.Machine.State()
	switch st {
	case StateNormal:
		rt.Machine.ForceNormal(m.now())
	case StateDrain:
		rt.Machine.ForceDrain(m.now())
	case StateDisabled:
		rt.Machine.ForceDisable(m.now())
	default:
		m.mu.Unlock()
		return fmt.Errorf("invalid state %q", st)
	}
	cur := rt.Machine.State()
	m.mu.Unlock()

	if prev != cur {
		reason := rt.Machine.LastReason()
		m.persistChannelTransition(id, prev, cur, reason)
	}
	return nil
}

// ManualSetModelState 手动设置内部模型状态，写事件并持久化。
func (m *Manager) ManualSetModelState(channelID, modelRowID int64, st State) error {
	m.mu.Lock()
	mr, ok := m.mrt[channelID][modelRowID]
	if !ok {
		m.mu.Unlock()
		return sql.ErrNoRows
	}
	prev := mr.Machine.State()
	switch st {
	case StateNormal:
		mr.Machine.ForceNormal(m.now())
	case StateDrain:
		mr.Machine.ForceDrain(m.now())
	case StateDisabled:
		mr.Machine.ForceDisable(m.now())
	default:
		m.mu.Unlock()
		return fmt.Errorf("invalid state %q", st)
	}
	cur := mr.Machine.State()
	m.mu.Unlock()

	if prev != cur {
		m.persistModelTransition(channelID, mr.Model.InternalModelID, prev, cur, mr.Machine.LastReason())
	}
	return nil
}

// ManualSetKeyState 手动切换单个密钥状态（action ∈ normal/drain/disable/recover），
// 仅同步内存状态机（ForceNormal/ForceDrain/ForceDisable，原因经 Machine 的 manual_* 记录）。
// DB 落库与 channel_key_events 由 A4 KeyService.ForceState 负责，此处不写库避免双写。
func (m *Manager) ManualSetKeyState(keyID int64, action string) error {
	st, ok := keyActionState[action]
	if !ok {
		return fmt.Errorf("invalid action %q", action)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	kr, ok := m.keys[keyID]
	if !ok {
		return sql.ErrNoRows
	}
	switch st {
	case StateNormal:
		kr.Machine.ForceNormal(m.now())
	case StateDrain:
		kr.Machine.ForceDrain(m.now())
	case StateDisabled:
		kr.Machine.ForceDisable(m.now())
	}
	return nil
}

// ForceChannelStateBatch 渠道级批量操作：将某渠道全部密钥状态机切到同一目标状态。
// 分工说明：DB 批量落库（逐密钥 SetState + channel_key_events + 渠道级 channel_events）
// 已在 A4 Service.ForceState 完成；本方法只负责内存 StateMachine 同步，避免双写冲突。
func (m *Manager) ForceChannelStateBatch(channelID int64, action string) {
	m.mu.RLock()
	ids := make([]int64, 0, len(m.keyOfCh[channelID]))
	for keyID := range m.keyOfCh[channelID] {
		ids = append(ids, keyID)
	}
	m.mu.RUnlock()
	for _, keyID := range ids {
		if err := m.ManualSetKeyState(keyID, action); err != nil {
			m.logger.Warn("force channel key state failed",
				"channel_id", channelID, "key_id", keyID, "action", action, "error", err)
		}
	}
}

// Start 启动健康探测 goroutine：每个密钥一个独立循环（密钥=运行时实体，调度/状态/记录均按密钥）。
// keyStore 未装配（装配层暂未注入）时无密钥运行时，探测空转并打印日志。
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	m.probeCtx = ctx
	if m.probeRunning == nil {
		m.probeRunning = make(map[int64]bool)
	}
	assembled := m.keyStore != nil
	m.mu.Unlock()

	if !assembled {
		m.logger.Warn("channel keyStore not assembled, per-key health probe scheduling skipped")
	}
	m.mu.Lock()
	for keyID := range m.keys {
		if m.probeRunning[keyID] {
			continue
		}
		m.probeRunning[keyID] = true
		m.wg.Add(1)
		go func(id int64, c context.Context) {
			defer m.wg.Done()
			m.keyProbeLoop(c, id)
		}(keyID, ctx)
	}
	m.mu.Unlock()
}

// startKeyProbeIfNewLocked 为运行期新增/装载的密钥启动探测循环（调用方需持有 m.mu）：
// probeCtx 为空（Start 未执行）或该密钥已有循环时跳过；RemoveKey 摘除时循环随 GetKeyRuntime 丢失退出。
func (m *Manager) startKeyProbeIfNewLocked(keyID int64) {
	if m.probeCtx == nil || m.probeRunning[keyID] {
		return
	}
	if m.probeRunning == nil {
		m.probeRunning = make(map[int64]bool)
	}
	m.probeRunning[keyID] = true
	m.wg.Add(1)
	go func(id int64, c context.Context) {
		defer m.wg.Done()
		m.keyProbeLoop(c, id)
	}(keyID, m.probeCtx)
}

// cronSpec 解析 cron 表达式（失败返回 nil，由调用方回退）。
func cronSpec(expr string) *cronx.Spec {
	s, err := cronx.Parse(expr)
	if err != nil {
		return nil
	}
	return s
}
