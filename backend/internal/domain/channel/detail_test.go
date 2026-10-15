package channel

import (
	"testing"
	"time"
)

// newDetailTestManager 构造仅内存态的 Manager（store/cmStore/keyStore 全 nil），供 RuntimeDetails 聚合测试。
func newDetailTestManager(nowFn func() time.Time) *Manager {
	return NewManager(nil, nil, nil, nil, nil, nowFn)
}

func newDetailTestMachine(nowFn func() time.Time) *Machine {
	m := NewMachine(MachineConfig{})
	m.SetClock(nowFn)
	return m
}

func newDetailTestLimiter(rpm, tpm int, burst float64, nowFn func() time.Time) *Limiter {
	l := NewLimiter(rpm, tpm, burst)
	l.SetClock(nowFn)
	return l
}

// 三层 Metrics 与窗口用量应与注入值一致。
func TestRuntimeDetails_AggregatesLayers(t *testing.T) {
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return base }
	mgr := newDetailTestManager(nowFn)

	ch := Channel{
		ID: 1, Name: "c1", Protocol: ProtocolOpenAICompat, BaseURL: "https://api.example.com",
		State:       StateNormal,
		RateLimit:   RateLimitConfig{RPM: 100, TPM: 1000, BurstMultiplier: 1.0, MaxConcurrent: 8},
		HealthProbe: HealthProbeConfig{FailThreshold: 1, RecoveryThreshold: 2},
		Reliability: ReliabilityConfig{WindowSeconds: 60, MinSamples: 10},
	}
	chMachine := newDetailTestMachine(nowFn)
	chMachine.Feed(Feedback{IsSuccess: true, LatencyMS: 100, Now: base})
	chMachine.Feed(Feedback{IsSuccess: true, LatencyMS: 200, Now: base})
	chMachine.Feed(Feedback{IsSuccess: true, LatencyMS: 300, Now: base})
	chMachine.Feed(Feedback{Now: base})                  // 非超时/非429 失败
	chMachine.Feed(Feedback{IsTimeout: true, Now: base}) // 超时
	chLimiter := newDetailTestLimiter(100, 1000, 1.0, nowFn)
	chLimiter.Record(base, 10)
	chLimiter.Record(base, 20)
	rt := &RuntimeChannel{Channel: ch, Machine: chMachine, Limiter: chLimiter}

	cm := ChannelModel{ID: 7, ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2, State: StateNormal}
	mMachine := newDetailTestMachine(nowFn)
	mMachine.Feed(Feedback{IsSuccess: true, LatencyMS: 50, Now: base})
	mLimiter := newDetailTestLimiter(50, 500, 1.0, nowFn)
	mLimiter.Record(base, 5)
	mr := &ModelRuntime{Model: cm, Machine: mMachine, Limiter: mLimiter}

	k := ChannelKey{ID: 11, ChannelID: 1, Name: "k1", State: StateNormal}
	kMachine := newDetailTestMachine(nowFn)
	kMachine.Feed(Feedback{IsSuccess: true, LatencyMS: 30, Now: base})
	kLimiter := newDetailTestLimiter(30, 300, 1.0, nowFn)
	kLimiter.Record(base, 3)
	kr := &KeyRuntime{Key: k, Channel: &ch, Machine: kMachine, Limiter: kLimiter, lastProbe: base, CreatedAt: base}

	mgr.rt[1] = rt
	mgr.mrt[1] = map[int64]*ModelRuntime{7: mr}
	mgr.keys[11] = kr
	mgr.keyOfCh[1] = map[int64]bool{11: true}

	got := mgr.RuntimeDetails()
	if len(got) != 1 {
		t.Fatalf("want 1 channel detail, got %d", len(got))
	}
	cd := got[0]
	if cd.View.ID != 1 || cd.View.Name != "c1" || cd.View.State != StateNormal {
		t.Fatalf("channel view mismatch: %+v", cd.View)
	}
	if cd.View.MaxSessions != 8 {
		t.Fatalf("channel MaxSessions want 8, got %d", cd.View.MaxSessions)
	}
	// 渠道指标：total=5 failures=1 timeouts=1 rate429=0 success=3
	if cd.Metrics.Total != 5 || cd.Metrics.Failures != 1 || cd.Metrics.Timeouts != 1 ||
		cd.Metrics.Rate429 != 0 || cd.Metrics.SuccessSamples != 3 {
		t.Fatalf("channel metrics mismatch: %+v", cd.Metrics)
	}
	if cd.Metrics.ErrorRatePct != 40 || cd.Metrics.Rate429Pct != 0 || cd.Metrics.P99LatencyMS != 300 {
		t.Fatalf("channel metric rates mismatch: %+v", cd.Metrics)
	}
	if cd.Metrics.WindowSeconds != 60 || cd.Metrics.MinSamples != 10 || cd.Metrics.SampleOK {
		t.Fatalf("channel window config mismatch: %+v", cd.Metrics)
	}
	if cd.RPM != 2 || cd.TPM != 30 || cd.RPMLimit != 100 || cd.TPMLimit != 1000 {
		t.Fatalf("channel usage mismatch: rpm=%d tpm=%d rpmLimit=%d tpmLimit=%d", cd.RPM, cd.TPM, cd.RPMLimit, cd.TPMLimit)
	}

	if len(cd.Models) != 1 {
		t.Fatalf("want 1 model, got %d", len(cd.Models))
	}
	md := cd.Models[0]
	if md.View.ID != 7 || md.View.InternalModelID != "qwen-max" || md.View.State != StateNormal {
		t.Fatalf("model view mismatch: %+v", md.View)
	}
	if md.Metrics.Total != 1 || md.Metrics.SuccessSamples != 1 {
		t.Fatalf("model metrics mismatch: %+v", md.Metrics)
	}
	if md.RPM != 1 || md.TPM != 5 || md.RPMLimit != 50 || md.TPMLimit != 500 {
		t.Fatalf("model usage mismatch: rpm=%d tpm=%d rpmLimit=%d tpmLimit=%d", md.RPM, md.TPM, md.RPMLimit, md.TPMLimit)
	}

	if len(cd.Keys) != 1 {
		t.Fatalf("want 1 key, got %d", len(cd.Keys))
	}
	kd := cd.Keys[0]
	if kd.View.KeyID != 11 || kd.View.State != StateNormal {
		t.Fatalf("key view mismatch: %+v", kd.View)
	}
	if kd.Metrics.Total != 1 || kd.Metrics.SuccessSamples != 1 {
		t.Fatalf("key metrics mismatch: %+v", kd.Metrics)
	}
	if kd.RPM != 1 || kd.TPM != 3 || kd.RPMLimit != 30 || kd.TPMLimit != 300 {
		t.Fatalf("key usage mismatch: rpm=%d tpm=%d rpmLimit=%d tpmLimit=%d", kd.RPM, kd.TPM, kd.RPMLimit, kd.TPMLimit)
	}
	if kd.MaxSessions != 8 {
		t.Fatalf("key MaxSessions want 8, got %d", kd.MaxSessions)
	}
	if kd.View.LastProbe == nil || !kd.View.LastProbe.Equal(base) {
		t.Fatalf("key LastProbe want %v, got %v", base, kd.View.LastProbe)
	}
}

// 排空态实体仍被导出（不被过滤），且带排空原因。
func TestRuntimeDetails_DrainedIncluded(t *testing.T) {
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return base }
	mgr := newDetailTestManager(nowFn)

	ch := Channel{ID: 1, Name: "c1", State: StateNormal,
		RateLimit: RateLimitConfig{RPM: 100, TPM: 1000, BurstMultiplier: 1.0, MaxConcurrent: 8}}
	chMachine := newDetailTestMachine(nowFn)
	rt := &RuntimeChannel{Channel: ch, Machine: chMachine, Limiter: newDetailTestLimiter(100, 1000, 1.0, nowFn)}

	cm := ChannelModel{ID: 7, ChannelID: 1, InternalModelID: "qwen-max", ExternalModelID: 2, State: StateNormal}
	mMachine := newDetailTestMachine(nowFn)
	mr := &ModelRuntime{Model: cm, Machine: mMachine, Limiter: newDetailTestLimiter(50, 500, 1.0, nowFn)}

	k := ChannelKey{ID: 11, ChannelID: 1, Name: "k1", State: StateNormal}
	kMachine := newDetailTestMachine(nowFn)
	kr := &KeyRuntime{Key: k, Channel: &ch, Machine: kMachine, Limiter: newDetailTestLimiter(30, 300, 1.0, nowFn)}

	// 自然排空（错误率超阈值且样本达标）记录排空原因；随后显式 ForceDrain 确认状态为 DRAIN。
	drain := func(mach *Machine) {
		for i := 0; i < 10; i++ {
			mach.Feed(Feedback{Now: base})
		}
		if mach.State() != StateDrain {
			t.Fatalf("setup: machine should be DRAIN after error rate exceeded, got %s", mach.State())
		}
		if len(mach.DrainReasons()) == 0 {
			t.Fatal("setup: drain reasons should be non-empty")
		}
		mach.ForceDrain(base)
	}
	drain(chMachine)
	drain(mMachine)
	drain(kMachine)

	mgr.rt[1] = rt
	mgr.mrt[1] = map[int64]*ModelRuntime{7: mr}
	mgr.keys[11] = kr
	mgr.keyOfCh[1] = map[int64]bool{11: true}

	got := mgr.RuntimeDetails()
	if len(got) != 1 {
		t.Fatalf("drained channel must still be exported, got %d", len(got))
	}
	cd := got[0]
	if cd.View.State != StateDrain || len(cd.Metrics.DrainingReasons) == 0 {
		t.Fatalf("channel should be DRAIN with reasons: state=%s reasons=%v", cd.View.State, cd.Metrics.DrainingReasons)
	}
	if len(cd.Models) != 1 || cd.Models[0].View.State != StateDrain || len(cd.Models[0].Metrics.DrainingReasons) == 0 {
		t.Fatalf("model should be DRAIN with reasons: %+v", cd.Models)
	}
	if len(cd.Keys) != 1 || cd.Keys[0].View.State != StateDrain || len(cd.Keys[0].Metrics.DrainingReasons) == 0 {
		t.Fatalf("key should be DRAIN with reasons: %+v", cd.Keys)
	}
	if len(cd.Keys[0].View.DrainReasons) == 0 {
		t.Fatalf("key view DrainReasons should be non-empty: %+v", cd.Keys[0].View)
	}
}

// 渠道/模型/密钥各层按 ID 升序；空渠道返回非 nil 空切片。
func TestRuntimeDetails_OrderAndEmpty(t *testing.T) {
	base := time.Date(2026, 10, 3, 12, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return base }
	mgr := newDetailTestManager(nowFn)

	buildChannel := func(chID int64) *RuntimeChannel {
		ch := Channel{ID: chID, Name: "c", State: StateNormal,
			RateLimit: RateLimitConfig{RPM: 100, TPM: 1000, BurstMultiplier: 1.0, MaxConcurrent: 4}}
		return &RuntimeChannel{Channel: ch, Machine: newDetailTestMachine(nowFn), Limiter: newDetailTestLimiter(100, 1000, 1.0, nowFn)}
	}
	mgr.rt[3] = buildChannel(3) // 空模型/空密钥渠道
	mgr.rt[1] = buildChannel(1)
	mgr.mrt[1] = map[int64]*ModelRuntime{
		30: {Model: ChannelModel{ID: 30, ChannelID: 1, InternalModelID: "m30"}, Machine: newDetailTestMachine(nowFn), Limiter: newDetailTestLimiter(10, 100, 1.0, nowFn)},
		10: {Model: ChannelModel{ID: 10, ChannelID: 1, InternalModelID: "m10"}, Machine: newDetailTestMachine(nowFn), Limiter: newDetailTestLimiter(10, 100, 1.0, nowFn)},
	}
	ch1 := mgr.rt[1].Channel
	keyAt := func(keyID int64) *KeyRuntime {
		return &KeyRuntime{
			Key:     ChannelKey{ID: keyID, ChannelID: 1, Name: "k"},
			Channel: &ch1, Machine: newDetailTestMachine(nowFn), Limiter: newDetailTestLimiter(10, 100, 1.0, nowFn),
		}
	}
	mgr.keys[40] = keyAt(40)
	mgr.keys[20] = keyAt(20)
	mgr.keyOfCh[1] = map[int64]bool{40: true, 20: true}
	mgr.keyOfCh[3] = map[int64]bool{}

	got := mgr.RuntimeDetails()
	if len(got) != 2 {
		t.Fatalf("want 2 channels, got %d", len(got))
	}
	if got[0].View.ID != 1 || got[1].View.ID != 3 {
		t.Fatalf("channels must be ID ascending, got %d,%d", got[0].View.ID, got[1].View.ID)
	}
	if len(got[0].Models) != 2 || got[0].Models[0].View.ID != 10 || got[0].Models[1].View.ID != 30 {
		t.Fatalf("models must be ID ascending: %+v", got[0].Models)
	}
	if len(got[0].Keys) != 2 || got[0].Keys[0].View.KeyID != 20 || got[0].Keys[1].View.KeyID != 40 {
		t.Fatalf("keys must be ID ascending: %+v", got[0].Keys)
	}
	if got[1].Models == nil || len(got[1].Models) != 0 || got[1].Keys == nil || len(got[1].Keys) != 0 {
		t.Fatalf("empty channel should expose non-nil empty slices: models=%v keys=%v", got[1].Models, got[1].Keys)
	}
}
