package ratelimit

import (
	"testing"
	"time"
)

func TestLimiter_IPUserWindow(t *testing.T) {
	l := New()
	ip := "1.2.3.4"
	u := "alice"
	// 4 次失败放行，第 5 次触发 IP+账号维度限流
	for i := 0; i < 4; i++ {
		l.RecordFailure(ip, u)
	}
	if err := l.Allow(ip, u); err != nil {
		t.Fatalf("expected allowed after 4 failures, got %v", err)
	}
	l.RecordFailure(ip, u)
	if err := l.Allow(ip, u); err == nil {
		t.Fatal("expected rate limited after 5 failures")
	}
}

func TestLimiter_AccountLockout(t *testing.T) {
	l := New()
	u := "victim"
	// 10 次失败（不同 IP 触发）→ 账号锁定
	for i := 0; i < 10; i++ {
		l.RecordFailure("10.0.0."+string(rune('0'+i%10)), u)
	}
	if err := l.Allow("9.9.9.9", u); err == nil {
		t.Fatal("expected account locked")
	}
}

func TestLimiter_SuccessResets(t *testing.T) {
	l := New()
	l.RecordFailure("1.1.1.1", "bob")
	l.RecordSuccess("1.1.1.1", "bob")
	if err := l.Allow("1.1.1.1", "bob"); err != nil {
		t.Fatalf("success should reset, got %v", err)
	}
}

func TestLimiter_LockExpiry(t *testing.T) {
	l := New()
	l.now = func() time.Time { return time.Unix(1000, 0) }
	for i := 0; i < 10; i++ {
		l.RecordFailure("8.8.8.8", "carl")
	}
	if err := l.Allow("8.8.8.8", "carl"); err == nil {
		t.Fatal("expected locked")
	}
	// 第 10 次失败按退避锁 1 分钟
	l.now = func() time.Time { return time.Unix(1000+60+1, 0) }
	if err := l.Allow("8.8.8.8", "carl"); err != nil {
		t.Fatalf("lock should expire after base duration, got %v", err)
	}
}

func TestLimiter_BackoffDoubles(t *testing.T) {
	l := New()
	l.now = func() time.Time { return time.Unix(1000, 0) }
	u := "backoff-user"
	// 第 10 次失败 → 锁 1 分钟
	for i := 0; i < 10; i++ {
		l.RecordFailure("10.0.0."+string(rune('0'+i%10)), u)
	}
	l.now = func() time.Time { return time.Unix(1000+60+1, 0) } // 1 分钟后
	if err := l.Allow("10.0.0.9", u); err != nil {
		t.Fatalf("expected unlocked after 1min, got %v", err)
	}
	// 第 20 次失败（窗口内累计）→ 锁 2 分钟
	for i := 10; i < 20; i++ {
		l.RecordFailure("10.0.0."+string(rune('0'+i%10)), u)
	}
	l.now = func() time.Time { return time.Unix(1000+60+1+60+1, 0) } // 再过 1 分钟（不足 2 分钟）
	if err := l.Allow("10.0.0.9", u); err == nil {
		t.Fatal("expected still locked (2min backoff)")
	}
	l.now = func() time.Time { return time.Unix(1000+60+1+120+1, 0) } // 2 分钟过后
	if err := l.Allow("10.0.0.9", u); err != nil {
		t.Fatalf("expected unlocked after 2min backoff, got %v", err)
	}
}

func TestLimiter_SuccessClearsLockout(t *testing.T) {
	l := New()
	l.now = func() time.Time { return time.Unix(1000, 0) }
	u := "lucky"
	for i := 0; i < 10; i++ {
		l.RecordFailure("10.0.0."+string(rune('0'+i%10)), u)
	}
	if err := l.Allow("10.0.0.9", u); err == nil {
		t.Fatal("expected locked")
	}
	l.RecordSuccess("10.0.0.9", u)
	if err := l.Allow("10.0.0.9", u); err != nil {
		t.Fatalf("success should clear account lockout, got %v", err)
	}
	// 账号维度计数也应归零：再错 9 次不应触发锁定
	for i := 0; i < 9; i++ {
		l.RecordFailure("10.0.0."+string(rune('0'+i%10)), u)
	}
	if err := l.Allow("10.0.0.9", u); err != nil {
		t.Fatalf("account counter should be reset after success, got %v", err)
	}
}
