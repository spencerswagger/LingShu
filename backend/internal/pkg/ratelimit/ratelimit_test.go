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
	l.now = func() time.Time { return time.Unix(1000+15*60+1, 0) }
	if err := l.Allow("8.8.8.8", "carl"); err != nil {
		t.Fatalf("lock should expire after 15min, got %v", err)
	}
}
