// Package ratelimit 提供进程内登录防爆破限流（IP+账号双维度，单实例适用）。
package ratelimit

import (
	"errors"
	"sync"
	"time"
)

// 阈值配置。
const (
	ipUserMaxFailures = 5                // 同 (IP,username) 1 分钟内最大失败数
	ipUserWindow      = time.Minute      // (IP,username) 计数窗口
	userMaxFailures   = 10               // 同用户名 15 分钟内累计最大失败数
	userWindow        = 15 * time.Minute // 用户名计数窗口（超过该窗口无失败则计数清零）
	lockBaseDuration  = time.Minute      // 首次锁定（第 10 次失败）时长
	lockMaxDuration   = 60 * time.Minute // 锁定时长上限
)

// ErrRateLimited 表示触发限流/锁定。
var ErrRateLimited = errors.New("rate limited")

type counter struct {
	count int
	start time.Time
}

// Limiter 内存限流器。now 可注入便于测试。
type Limiter struct {
	mu       sync.Mutex
	ipUser   map[string]*counter
	users    map[string]*counter
	lockouts map[string]time.Time
	now      func() time.Time
}

// New 创建限流器。
func New() *Limiter {
	return &Limiter{
		ipUser:   map[string]*counter{},
		users:    map[string]*counter{},
		lockouts: map[string]time.Time{},
		now:      time.Now,
	}
}

// Allow 判断 (ip, username) 是否被放行：账号退避锁有效或 (IP,username) 短窗超限则拒绝。
func (l *Limiter) Allow(ip, username string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	if until, ok := l.lockouts[username]; ok && now.Before(until) {
		return ErrRateLimited
	}
	if c := l.ipUser[ip+"|"+username]; c != nil && now.Sub(c.start) < ipUserWindow && c.count >= ipUserMaxFailures {
		return ErrRateLimited
	}
	return nil
}

// RecordFailure 记录一次失败；账号维度计数达到阈值时按指数退避锁定：
// 第 10 次失败锁 1 分钟，其后每多 10 次失败翻倍，上限 60 分钟。
//
// 实际行为提示：由于 userWindow=15 分钟，且锁定期间 Allow 直接拒绝、RecordFailure
// 不会被调用，解锁后 15 分钟内未再累积失败即会清零计数，因此高等级退避（>2 倍）
// 在真实流量下很少达到——lockMaxDuration 属防御性上限，并非常态路径。
func (l *Limiter) RecordFailure(ip, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.bump(l.ipUser, ip+"|"+username, now, ipUserWindow)
	c := l.bump(l.users, username, now, userWindow)
	if c.count >= userMaxFailures {
		levels := c.count/userMaxFailures - 1
		d := lockBaseDuration << levels
		if d > lockMaxDuration {
			d = lockMaxDuration
		}
		l.lockouts[username] = now.Add(d)
	}
}

// RecordSuccess 登录成功：重置该 (IP,username) 与账号维度计数，并解除残留锁定。
func (l *Limiter) RecordSuccess(ip, username string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.ipUser, ip+"|"+username)
	delete(l.users, username)
	delete(l.lockouts, username)
}

func (l *Limiter) bump(m map[string]*counter, key string, now time.Time, win time.Duration) *counter {
	c, ok := m[key]
	if !ok || now.Sub(c.start) >= win {
		c = &counter{count: 0, start: now}
		m[key] = c
	}
	c.count++
	return c
}

// Cleanup 清理过期计数与已过期的锁定，防内存膨胀。
func (l *Limiter) Cleanup() {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for k, c := range l.ipUser {
		if now.Sub(c.start) >= ipUserWindow {
			delete(l.ipUser, k)
		}
	}
	for k, c := range l.users {
		if now.Sub(c.start) >= userWindow {
			delete(l.users, k)
		}
	}
	for k, until := range l.lockouts {
		if !now.Before(until) {
			delete(l.lockouts, k)
		}
	}
}
