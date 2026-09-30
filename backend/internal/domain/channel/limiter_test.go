package channel

import (
	"testing"
	"time"
)

func TestLimiter_AllowUntilLimit(t *testing.T) {
	l := NewLimiter(5, 1_000_000, 1.0) // RPM=5，burst=1.0
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.SetClock(func() time.Time { return base })

	for i := 0; i < 5; i++ {
		ok, _ := l.Allow(base.Add(time.Duration(i)*time.Second), 1)
		if !ok {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	// 第 6 个在限额内被拒绝（rpm 达 5/5）。
	if ok, wait := l.Allow(base.Add(6*time.Second), 1); ok {
		t.Fatal("6th request should be rejected")
	} else if wait <= 0 {
		t.Fatalf("expected wait > 0 on reject, got %v", wait)
	}
}

func TestLimiter_WindowSliding_Recovers(t *testing.T) {
	l := NewLimiter(3, 1_000_000, 1.0)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.SetClock(func() time.Time { return base })

	for i := 0; i < 3; i++ {
		if ok, _ := l.Allow(base.Add(time.Duration(i)*time.Second), 1); !ok {
			t.Fatalf("request %d should be allowed", i+1)
		}
	}
	if ok, _ := l.Allow(base.Add(4*time.Second), 1); ok {
		t.Fatal("4th request should be rejected before window slides")
	}
	// 推进超出一个窗口 → 旧记录被清除，重新放行。
	if ok, _ := l.Allow(base.Add(61*time.Second), 1); !ok {
		t.Fatal("request after window slide should be allowed")
	}
}

func TestLimiter_BurstAllowsOvershoot(t *testing.T) {
	// RPM=10，burst=1.2 → 瞬时上限 12。
	l := NewLimiter(10, 1_000_000, 1.2)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.SetClock(func() time.Time { return base })

	for i := 0; i < 12; i++ {
		if ok, _ := l.Allow(base.Add(time.Duration(i)*time.Second), 1); !ok {
			t.Fatalf("burst request %d should be allowed", i+1)
		}
	}
	if ok, _ := l.Allow(base.Add(13*time.Second), 1); ok {
		t.Fatal("13th request must be rejected (over burst limit)")
	}
}

func TestLimiter_TPMIndependentOfRPM(t *testing.T) {
	// RPM 很大、TPM=1000：单个 600-token 请求受 TPM 约束。
	l := NewLimiter(10_000, 1000, 1.0)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l.SetClock(func() time.Time { return base })

	if ok, _ := l.Allow(base, 600); !ok {
		t.Fatal("first 600-token request should be allowed")
	}
	// RPM 仍充足，但 TPM 累计 1200 > 1000 → 拒绝。
	if ok, _ := l.Allow(base.Add(time.Second), 600); ok {
		t.Fatal("second 600-token request must be rejected by TPM despite rpm headroom")
	}
}

func TestLimiter_TokenLimit(t *testing.T) {
	l := NewLimiter(1000, 5000, 2.0)
	if got := l.TokenLimit(); got != 10_000 {
		t.Fatalf("expected TokenLimit 10000, got %d", got)
	}
}
