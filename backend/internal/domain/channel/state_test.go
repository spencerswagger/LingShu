package channel

import (
	"testing"
	"time"
)

// makeFeed 构造一次真实调用反馈（默认成功、低耗时）。
func callFeedback(t time.Time, success bool) Feedback {
	return Feedback{IsSuccess: success, Now: t}
}

func TestMachine_ErrorRate_exceeds_goDrain(t *testing.T) {
	m := NewMachine(DefaultMachineConfig())
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.SetClock(func() time.Time { return base })

	// 窗口（60s）内 15 失败 + 85 成功 = 15% 错误率 > 10%。
	for i := 0; i < 15; i++ {
		m.Feed(callFeedback(base.Add(time.Duration(i)*500*time.Millisecond), false))
	}
	for i := 15; i < 100; i++ {
		m.Feed(callFeedback(base.Add(time.Duration(i)*500*time.Millisecond), true))
	}

	if got := m.State(); got != StateDrain {
		t.Fatalf("expected DRAIN_ONLY, got %s (reason=%s)", got, m.LastReason())
	}
}

func TestMachine_ProbeRecovery_DrainToHealthy(t *testing.T) {
	m := NewMachine(DefaultMachineConfig())
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.SetClock(func() time.Time { return base })

	// 进入 DRAIN。
	for i := 0; i < 15; i++ {
		m.Feed(callFeedback(base.Add(time.Duration(i)*200*time.Millisecond), false))
	}
	if m.State() != StateDrain {
		t.Fatalf("expected DRAIN_ONLY before recovery, got %s", m.State())
	}

	// 连续 5 次探活成功 → HEALTHY。
	for i := 0; i < 5; i++ {
		m.Feed(Feedback{IsSuccess: true, HasProbe: true, Now: base.Add(time.Duration(100+i) * time.Second)})
	}
	if got := m.State(); got != StateNormal {
		t.Fatalf("expected HEALTHY after probe recovery, got %s", got)
	}
}

func TestMachine_429Storm_goDrain(t *testing.T) {
	m := NewMachine(DefaultMachineConfig())
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.SetClock(func() time.Time { return base })

	// 7 成功 + 3 个 429 → 429 占比 30% > 20%。
	for i := 0; i < 7; i++ {
		m.Feed(callFeedback(base.Add(time.Duration(i)*100*time.Millisecond), true))
	}
	for i := 7; i < 10; i++ {
		m.Feed(Feedback{Is429: true, Now: base.Add(time.Duration(i) * 100 * time.Millisecond)})
	}

	if got := m.State(); got != StateDrain {
		t.Fatalf("expected DRAIN_ONLY from 429 storm, got %s", got)
	}
}

func TestMachine_P99Latency_exceed_goDrain(t *testing.T) {
	cfg := DefaultMachineConfig()
	cfg.P99LatencyMS = 5000 // 显式低阈值验证「P99 超限 → DRAIN」机制（默认阈值已提至 120000ms）
	m := NewMachine(cfg)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.SetClock(func() time.Time { return base })

	// 100 个成功样本（50s 内、同一 60s 窗口）：99 个耗时 6000ms（>5000 P99 阈值），1 个 100ms。
	for i := 0; i < 99; i++ {
		m.Feed(Feedback{IsSuccess: true, LatencyMS: 6000, Now: base.Add(time.Duration(i) * 500 * time.Millisecond)})
	}
	m.Feed(Feedback{IsSuccess: true, LatencyMS: 100, Now: base.Add(99 * 500 * time.Millisecond)})

	if got := m.State(); got != StateDrain {
		t.Fatalf("expected DRAIN_ONLY from p99 latency, got %s", got)
	}
}

func TestMachine_Drain_NoAutoDisable(t *testing.T) {
	m := NewMachine(DefaultMachineConfig())
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.SetClock(func() time.Time { return base })

	// 进入 DRAIN。
	for i := 0; i < 15; i++ {
		m.Feed(callFeedback(base.Add(time.Duration(i)*200*time.Millisecond), false))
	}
	if m.State() != StateDrain {
		t.Fatalf("expected DRAIN, got %s", m.State())
	}

	// 长时间排空后仍提交失败：绝不自动转 DISABLED（禁用仅由 401/403 或手动进入）。
	m.Feed(callFeedback(base.Add(31*time.Minute), false))
	if got := m.State(); got != StateDrain {
		t.Fatalf("expected DRAIN (no auto disable), got %s (reason=%s)", got, m.LastReason())
	}
}

func TestMachine_ManualStateTransitions(t *testing.T) {
	m := NewMachine(DefaultMachineConfig())
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.SetClock(func() time.Time { return base })

	if got := m.ForceDrain(base); got != StateDrain {
		t.Fatal("ForceDrain should set DRAIN")
	}
	if got := m.ForceDisable(base); got != StateDisabled {
		t.Fatal("ForceDisable should set DISABLED")
	}
	if got := m.ForceNormal(base); got != StateNormal {
		t.Fatal("ForceNormal should set NORMAL")
	}
}

func TestMachine_AuthFailure_goUnavailable(t *testing.T) {
	m := NewMachine(DefaultMachineConfig()) // AuthFailThreshold 默认 3
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.SetClock(func() time.Time { return base })

	// 真实调用的 401/403 需连续达阈值才熔断，避免单次抖动误杀健康渠道
	//（网关会先转移到下一候选）。
	for i := 1; i < 3; i++ {
		m.Feed(Feedback{IsAuthFailure: true, Now: base})
		if got := m.State(); got != StateNormal {
			t.Fatalf("第 %d 次鉴权失败未达阈值应保持 NORMAL, got %s", i, got)
		}
	}
	m.Feed(Feedback{IsAuthFailure: true, Now: base})
	if got := m.State(); got != StateDisabled {
		t.Fatalf("连续达阈值应 DISABLED, got %s", got)
	}
	if got := m.LastReason(); got != reasonAuthFailure {
		t.Fatalf("reason want %q, got %q", reasonAuthFailure, got)
	}

	// DISABLED 为终态：后续成功不再改变。
	m.Feed(callFeedback(base.Add(time.Second), true))
	if got := m.State(); got != StateDisabled {
		t.Fatalf("DISABLED must be terminal, got %s", got)
	}
}

// 真实调用的连续鉴权失败计数会被「非鉴权失败」打断；探测的 401/403 一次即熔断。
func TestMachine_AuthFailure_ConsecutiveAndProbe(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	m := NewMachine(DefaultMachineConfig())
	m.SetClock(func() time.Time { return base })
	m.Feed(Feedback{IsAuthFailure: true, Now: base})
	m.Feed(Feedback{Now: base}) // 非鉴权失败 → 打断连续计数
	m.Feed(Feedback{IsAuthFailure: true, Now: base})
	m.Feed(Feedback{IsAuthFailure: true, Now: base})
	if got := m.State(); got != StateNormal {
		t.Fatalf("连续计数被打断后不应熔断, got %s", got)
	}
	m.Feed(Feedback{IsAuthFailure: true, Now: base})
	if got := m.State(); got != StateDisabled {
		t.Fatalf("重新连续三次应熔断, got %s", got)
	}

	// 健康探测是确定性检查：一次 401 即熔断。
	mp := NewMachine(DefaultMachineConfig())
	mp.SetClock(func() time.Time { return base })
	mp.Feed(Feedback{IsAuthFailure: true, HasProbe: true, Now: base})
	if got := mp.State(); got != StateDisabled {
		t.Fatalf("探测 401 应直接熔断, got %s", got)
	}
}

func TestMachine_ForceNormal_FromDisabled(t *testing.T) {
	m := NewMachine(DefaultMachineConfig())
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.SetClock(func() time.Time { return base })

	for i := 0; i < 3; i++ {
		m.Feed(Feedback{IsAuthFailure: true, Now: base})
	}
	if m.State() != StateDisabled {
		t.Fatalf("setup: expected DISABLED, got %s", m.State())
	}

	m.ForceNormal(base.Add(time.Minute))
	if got := m.State(); got != StateNormal {
		t.Fatalf("expected NORMAL after manual recover, got %s", got)
	}
	// ForceNormal 清空连续鉴权失败计数：恢复后需重新累计。
	m.Feed(Feedback{IsAuthFailure: true, Now: base})
	if got := m.State(); got != StateNormal {
		t.Fatalf("恢复后计数应清零, got %s", got)
	}
}

func TestMachine_ForceDisable_Manual(t *testing.T) {
	m := NewMachine(DefaultMachineConfig())
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	m.SetClock(func() time.Time { return base })

	m.ForceDisable(base)
	if got := m.State(); got != StateDisabled {
		t.Fatalf("expected DISABLED after manual disable, got %s", got)
	}
	if got := m.LastReason(); got != reasonManualDisable {
		t.Fatalf("expected reason %s, got %s", reasonManualDisable, got)
	}
}

func TestMachine_ValidState(t *testing.T) {
	if !StateNormal.Valid() || !StateDrain.Valid() || !StateDisabled.Valid() {
		t.Fatal("all three states must be valid")
	}
	if State("BOGUS").Valid() {
		t.Fatal("bogus state must be invalid")
	}
}
