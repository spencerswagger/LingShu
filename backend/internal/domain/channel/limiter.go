package channel

import (
	"sync"
	"time"
)

// OnExceed 模式：超出限额时的处置。
const (
	OnExceedQueue  = "QUEUE"  // 排队等待，超过超时拒 429
	OnExceedReject = "REJECT" // 直接拒 429
)

// limRec 一条已放行请求的窗口记录。
type limRec struct {
	ts     time.Time
	tokens int
}

// Limiter 内存滑动窗口限流器。
//
// RPM=每分钟允许请求数，TPM=每分钟允许 token 数，burst 为瞬时超发系数；
// 以 burst 放大后的额度（rpmLimit*burst、tpmLimit*burst）作为放行上限。
// 窗口内维护多条记录，rpm=len(窗口记录)，tpm=SUM(记录 tokens)。
// 并发会话数不在此处限制：由 SessionRegistry（存活会话）负责。
type Limiter struct {
	mu       sync.Mutex
	window   time.Duration
	rpmLimit int
	tpmLimit int
	burst    float64
	recs     []limRec
	now      func() time.Time
}

// NewLimiter 创建限流器。rpm/tpm <=0 表示不受该维度限制。
func NewLimiter(rpmLimit, tpmLimit int, burst float64) *Limiter {
	if burst <= 0 {
		burst = 1
	}
	return &Limiter{
		window:   time.Minute,
		rpmLimit: rpmLimit,
		tpmLimit: tpmLimit,
		burst:    burst,
		now:      time.Now,
	}
}

// SetClock 注入时钟函数便于测试。
func (l *Limiter) SetClock(fn func() time.Time) {
	if fn == nil {
		fn = time.Now
	}
	l.now = fn
}

// rpmBurst tpmBurst 返回放大后的额度。
func (l *Limiter) rpmBurst() int {
	if l.rpmLimit <= 0 {
		return 1 << 30
	}
	return int(float64(l.rpmLimit) * l.burst)
}
func (l *Limiter) tpmBurst() int64 {
	if l.tpmLimit <= 0 {
		return 1 << 40
	}
	return int64(float64(l.tpmLimit) * l.burst)
}

// TokenLimit 返回 TPM 放大后的额度，供上层查询。
func (l *Limiter) TokenLimit() int64 { return l.tpmBurst() }

// prune 移除窗口外记录。
func (l *Limiter) prune(now time.Time) {
	cutoff := now.Add(-l.window)
	i := 0
	for i < len(l.recs) && l.recs[i].ts.Before(cutoff) {
		i++
	}
	if i > 0 {
		l.recs = append(l.recs[:0], l.recs[i:]...)
	}
}

// counts 统计窗口内 rpm 与 tpm。
func (l *Limiter) counts() (int, int64) {
	var rpm int
	var tpm int64
	for _, r := range l.recs {
		rpm++
		tpm += int64(r.tokens)
	}
	return rpm, tpm
}

// Allow 判定一次放行：
//   - 放行：返回 (true, 0) 并记录本次成本（1 请求 + tokens）。
//   - 拒绝：返回 (false, wait)。wait>0 表示排队到该时长后才能放行；wait==0 表示拒绝。
//
// 调用方依据 on_exceed 语义：QUEUE 时若 wait<=queue_timeout 可等待重试，超时拒 429；
// REJECT 时直接返回 429。
func (l *Limiter) Allow(now time.Time, tokens int) (ok bool, wait time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.now == nil {
		l.now = time.Now
	}
	if now.IsZero() {
		now = l.now()
	}
	l.prune(now)

	rpm, tpm := l.counts()
	reserved := int64(1) // 每个请求占用 1 RPM；token 成本单独计算
	if int64(rpm)+reserved <= int64(l.rpmBurst()) && tpm+int64(tokens) <= l.tpmBurst() {
		l.recs = append(l.recs, limRec{ts: now, tokens: tokens})
		return true, 0
	}

	// 计算等待时长：找到最早能释放出容量的时间点。
	wait = l.waitForCapacity(now, int64(tokens), reserved)
	return false, wait
}

// waitForCapacity 返回最早的可放行时间 - now。无可行点时返回 window（最坏等待）。
func (l *Limiter) waitForCapacity(now time.Time, tokens, reservedToken int64) time.Duration {
	best := now.Add(l.window) // 最坏：等一个完整窗口
	found := false
	for _, r := range l.recs {
		release := r.ts.Add(l.window) // 该记录释放的时刻
		var rpm int
		var tpm int64
		for _, o := range l.recs {
			if o.ts.Add(l.window).After(release) {
				rpm++
				tpm += int64(o.tokens)
			}
		}
		if int64(rpm)+reservedToken <= int64(l.rpmBurst()) && tpm+int64(tokens) <= l.tpmBurst() {
			if release.Before(best) {
				best = release
				found = true
			}
		}
	}
	if !found {
		best = now.Add(l.window)
	}
	win := best.Sub(now)
	if win < 0 {
		win = 0
	}
	return win
}

// Record 直接记录一次已放行的调用成本（供网关在其他路径已放行时手动记账）。
func (l *Limiter) Record(now time.Time, tokens int) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if now.IsZero() {
		if l.now == nil {
			l.now = time.Now
		}
		now = l.now()
	}
	l.prune(now)
	l.recs = append(l.recs, limRec{ts: now, tokens: tokens})
}
