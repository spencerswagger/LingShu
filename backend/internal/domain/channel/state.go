package channel

import (
	"sort"
	"time"
)

// 状态流转原因，供写 channel_events / channel_model_events 时使用。
const (
	reasonErrorRate     = "error_rate_exceeded"
	reason429           = "http_429_exceeded"
	reasonLatency       = "p99_latency_exceeded"
	reasonProbeFail     = "probe_failed"
	reasonProbeRecover  = "probe_recovered"
	reasonAuthFailure   = "auth_failure"
	reasonManualNormal  = "manual_normal"
	reasonManualDrain   = "manual_drain"
	reasonManualDisable = "manual_disable"
)

// Feedback 一次真实调用或健康探测的反馈，供状态机评估。
type Feedback struct {
	IsSuccess     bool      // 调用/探测成功
	IsTimeout     bool      // 超时
	Is429         bool      // HTTP 429（被上游限流）
	IsAuthFailure bool      // 401/403 认证失败（连续达阈值 → 禁用）
	LatencyMS     int64     // 成功样本耗时
	HasProbe      bool      // 是否为健康探测反馈（区别于真实调用）
	Now           time.Time // 事件时间；为零则用机器时钟
}

// IsProbe 是否为健康探测反馈，供连续探测阈值判定。
func (f Feedback) IsProbe() bool { return f.HasProbe }

// MachineConfig 状态机阈值配置；字段为 0 时用 DefaultMachineConfig 兜底。
// 排空没有超时自动禁用：禁用仅由认证失败达阈值或手动进入。
type MachineConfig struct {
	WindowSeconds          int     // 滑动窗口长度，默认 60
	ErrorRatePct           float64 // 窗口内 (failure+timeout)/total 阈值，默认 10
	P99LatencyMS           int64   // 成功样本 P99 阈值，默认 5000
	Rate429Pct             float64 // 窗口内 429 占比阈值，默认 20
	ProbeFailThreshold     int     // 连续探活失败阈值 → DRAIN，默认 1
	ProbeRecoveryThreshold int     // 连续探活成功阈值 → NORMAL，默认 2
	MinSamples             int     // 窗口指标生效所需的最小样本数，默认 10，防止少样本误判
	AuthFailThreshold      int     // 连续真实调用鉴权失败(401/403)阈值 → DISABLED，默认 3
}

// DefaultMachineConfig 返回状态机默认阈值。
func DefaultMachineConfig() MachineConfig {
	return MachineConfig{
		WindowSeconds:          60,
		ErrorRatePct:           10,
		P99LatencyMS:           5000,
		Rate429Pct:             20,
		ProbeFailThreshold:     1,
		ProbeRecoveryThreshold: 2,
		MinSamples:             10,
		AuthFailThreshold:      3,
	}
}

// Machine 三态状态机（正常/排空/禁用），纯逻辑可注入时钟。
//
// 流转规则（渠道与内部模型完全同构）：
//   - 可靠性窗口超限（样本达标）→ DRAIN
//   - 连续探活失败 ≥ ProbeFailThreshold → DRAIN
//   - 排空恢复：仅由健康探测驱动，连续探活成功 ≥ ProbeRecoveryThreshold → NORMAL
//   - 真实调用连续鉴权失败(401/403) ≥ AuthFailThreshold → DISABLED；
//     健康探测的 401/403 视为确定性检查，一次即 DISABLED
//   - DISABLED 为终态，仅手动退出；无排空超时自动禁用
type Machine struct {
	cfg             MachineConfig
	now             func() time.Time
	state           State
	lastReason      string
	failures        []time.Time // 非超时/非429 的失败
	timeouts        []time.Time
	rate429         []time.Time
	total           []time.Time
	successLats     []int64 // 窗口内成功样本耗时
	probeFails      int     // 连续探活失败
	probeOKs        int     // 连续探活成功
	authFails       int     // 连续（真实调用）鉴权失败
	drainingReasons []string
}

// NewMachine 创建以 NORMAL 为初态的状态机。
func NewMachine(cfg MachineConfig) *Machine {
	if cfg == (MachineConfig{}) {
		cfg = DefaultMachineConfig()
	} else {
		def := DefaultMachineConfig()
		if cfg.WindowSeconds == 0 {
			cfg.WindowSeconds = def.WindowSeconds
		}
		if cfg.ErrorRatePct == 0 {
			cfg.ErrorRatePct = def.ErrorRatePct
		}
		if cfg.P99LatencyMS == 0 {
			cfg.P99LatencyMS = def.P99LatencyMS
		}
		if cfg.Rate429Pct == 0 {
			cfg.Rate429Pct = def.Rate429Pct
		}
		if cfg.ProbeFailThreshold == 0 {
			cfg.ProbeFailThreshold = def.ProbeFailThreshold
		}
		if cfg.ProbeRecoveryThreshold == 0 {
			cfg.ProbeRecoveryThreshold = def.ProbeRecoveryThreshold
		}
		if cfg.MinSamples == 0 {
			cfg.MinSamples = def.MinSamples
		}
		if cfg.AuthFailThreshold == 0 {
			cfg.AuthFailThreshold = def.AuthFailThreshold
		}
	}
	return &Machine{cfg: cfg, now: time.Now, state: StateNormal}
}

// SetClock 注入时钟函数，便于测试中伪造时间保证确定性。nil 表示恢复系统时钟。
func (m *Machine) SetClock(fn func() time.Time) {
	if fn == nil {
		fn = time.Now
	}
	m.now = fn
}

// State 返回当前状态。
func (m *Machine) State() State { return m.state }

// LastReason 返回最近一次状态流转的原因；未发生流转则为空串。
func (m *Machine) LastReason() string { return m.lastReason }

// Feed 注入一次反馈并返回评估后的状态。Manager 通过比较前后状态判断是否流转并写事件。
func (m *Machine) Feed(f Feedback) State {
	now := f.Now
	if now.IsZero() {
		now = m.now()
	}
	m.prune(now)

	// 认证失败：真实调用按「连续失败达阈值」才熔断，避免单次抖动误杀健康渠道
	//（网关会先把该次请求转移到下一候选）；健康探测是确定性检查，一次即熔断。
	if f.IsAuthFailure {
		if f.IsProbe() {
			m.transition(StateDisabled, now, reasonAuthFailure)
			return m.state
		}
		m.authFails++
		if m.authFails >= m.cfg.AuthFailThreshold {
			m.transition(StateDisabled, now, reasonAuthFailure)
		}
		return m.state
	}
	// 非鉴权失败 → 打断「连续鉴权失败」计数（连续 = 中间没有其它结果）。
	m.authFails = 0

	if !f.IsProbe() {
		m.total = append(m.total, now)
		switch {
		case f.IsTimeout:
			m.timeouts = append(m.timeouts, now)
		case f.Is429:
			m.rate429 = append(m.rate429, now)
		case f.IsSuccess:
			m.successLats = append(m.successLats, f.LatencyMS)
		default:
			m.failures = append(m.failures, now)
		}
	} else if f.IsSuccess {
		m.probeOKs++
		m.probeFails = 0
	} else {
		m.probeFails++
		m.probeOKs = 0
	}

	m.evaluate(now)
	return m.state
}

// evaluate 根据窗口指标与当前状态计算目标状态并执行合法流转。
// 窗口类指标（错误率/429/P99）仅在样本量达到 MinSamples 时判定，防止少样本瞬时误判。
func (m *Machine) evaluate(now time.Time) {
	sampleOK := len(m.total) >= m.cfg.MinSamples
	switch {
	case m.state == StateDisabled:
		return // 终态，仅手动恢复
	case m.probeFails >= m.cfg.ProbeFailThreshold:
		m.ensureDrain(now, reasonProbeFail)
	case m.state == StateDrain && m.probeOKs >= m.cfg.ProbeRecoveryThreshold:
		// 排空恢复仅由健康探测驱动：真实调用不参与恢复判定。
		m.drainingReasons = nil
		m.probeOKs = 0
		m.transition(StateNormal, now, reasonProbeRecover)
	case sampleOK && m.errorRate() > m.cfg.ErrorRatePct:
		m.ensureDrain(now, reasonErrorRate)
	case sampleOK && m.rate429Pct() > m.cfg.Rate429Pct:
		m.ensureDrain(now, reason429)
	case sampleOK && m.p99SuccessLatency() > m.cfg.P99LatencyMS:
		m.ensureDrain(now, reasonLatency)
	}
}

// ensureDrain 进入/维持排空态，记录触发原因（诊断用）。
func (m *Machine) ensureDrain(now time.Time, reason string) {
	if !containsString(m.drainingReasons, reason) {
		m.drainingReasons = append(m.drainingReasons, reason)
	}
	m.transition(StateDrain, now, reason)
}

// transition 在状态实际变化时更新 state 与 lastReason。
func (m *Machine) transition(next State, now time.Time, reason string) {
	if m.state != next {
		m.state = next
		m.lastReason = reason
	}
}

// ForceNormal 手动置正常：清空排水原因、探测计数与连续鉴权失败计数。
func (m *Machine) ForceNormal(now time.Time) State {
	m.drainingReasons = nil
	m.probeOKs = 0
	m.probeFails = 0
	m.authFails = 0
	m.transition(StateNormal, now, reasonManualNormal)
	return m.state
}

// ForceDrain 手动置排空。
func (m *Machine) ForceDrain(now time.Time) State {
	m.transition(StateDrain, now, reasonManualDrain)
	return m.state
}

// ForceDisable 手动置禁用：任意状态 → DISABLED（终态）。
func (m *Machine) ForceDisable(now time.Time) State {
	m.transition(StateDisabled, now, reasonManualDisable)
	return m.state
}

// DrainReasons 返回当前排空触发原因列表（诊断用）。
func (m *Machine) DrainReasons() []string { return m.drainingReasons }

// prune 移除滑动窗口外的请求记录（时间戳 + 成功样本）。
func (m *Machine) prune(now time.Time) {
	cutoff := now.Add(-time.Duration(m.cfg.WindowSeconds) * time.Second)
	m.failures = trimBefore(m.failures, cutoff)
	m.timeouts = trimBefore(m.timeouts, cutoff)
	m.rate429 = trimBefore(m.rate429, cutoff)
	m.total = trimBefore(m.total, cutoff)
	m.successLats = trimOldLatencies(m.successLats, cutoff, m.total)
}

// errorRate 窗口内 (failure+timeout)/total 的百分比。
func (m *Machine) errorRate() float64 {
	if len(m.total) == 0 {
		return 0
	}
	err := float64(len(m.failures)+len(m.timeouts)) / float64(len(m.total)) * 100
	return err
}

// rate429Pct 窗口内 429 占比百分比。
func (m *Machine) rate429Pct() float64 {
	if len(m.total) == 0 {
		return 0
	}
	return float64(len(m.rate429)) / float64(len(m.total)) * 100
}

// p99SuccessLatency 窗口内成功样本耗时的 P99；样本数不足时保守估计放大。
func (m *Machine) p99SuccessLatency() int64 {
	n := len(m.successLats)
	if n == 0 {
		return 0
	}
	idx := int(float64(n)*0.99) + 1
	if idx > n {
		idx = n
	}
	cp := append([]int64(nil), m.successLats...)
	sort.Slice(cp, func(i, j int) bool { return cp[i] < cp[j] })
	return cp[idx-1]
}

func trimBefore(times []time.Time, cutoff time.Time) []time.Time {
	i := 0
	for i < len(times) && times[i].Before(cutoff) {
		i++
	}
	out := make([]time.Time, 0, len(times)-i)
	return append(out, times[i:]...)
}

// trimOldLatencies 仅保留与窗口内请求时间对应的成功样本，防止旧样本干扰 P99。
// 简化实现：成功样本与请求一一对应，直接按 count(total) 截取最近样本。
func trimOldLatencies(lats []int64, cutoff time.Time, total []time.Time) []int64 {
	if len(lats) <= len(total) {
		return lats
	}
	return lats[len(lats)-len(total):]
}

func containsString(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}
