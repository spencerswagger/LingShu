// Package backoff 是灵枢 V2「上游调用重试节奏」的唯一实现。
//
// 口径（阶段一已批准）：退避序列固定 **5s / 20s / 60s**，重试最多 **3 次**，之后放弃并
// 抛出可诊断的错误。其它包不得自行硬编码这几个数字——验收里「注入 429 与超时，重试
// 序列恰为 5/20/60s 且重试 ≤3 次」这条要随实现走，散落多份就会漂移。
//
// 语义边界（容易搞混，写清楚这里）：
//   - 首次尝试**立即执行**，不等待；
//   - 「重试 3 次」= 首次 + 3 次重试 = 最多 4 次调用，退避依次 5s、20s、60s；
//   - ctx 取消/超时后**不再退避**（等待被中断即返回，不把 60s 睡完再说）。
package backoff

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// DefaultMaxRetries 是默认重试次数（不含首次尝试）。
const DefaultMaxRetries = 3

// DefaultRetryDelays 返回默认退避序列（5s / 20s / 60s）的副本。
// 返回副本而非共享切片：调用方改自己的策略不会污染全局默认。
func DefaultRetryDelays() []time.Duration {
	return []time.Duration{5 * time.Second, 20 * time.Second, 60 * time.Second}
}

// Default 返回标准重试策略（5s/20s/60s，最多重试 3 次）。
func Default() Policy {
	return Policy{Delays: DefaultRetryDelays(), MaxRetries: DefaultMaxRetries}
}

// Policy 是重试节奏：首次尝试后按 Delays 逐次退避，最多 MaxRetries 次重试。
//
// 零值 Policy{} 表示「不重试」（只调用一次）；需要标准节奏请用 Default()。
// 这个约定是刻意的：零值静默变成重试 4 次会让「本想只试一次」的调用方多打上游。
type Policy struct {
	// Delays 是第 1..N 次重试前的等待时长（非正值项会被跳过）。
	Delays []time.Duration
	// MaxRetries 是重试次数上限（不含首次尝试）。
	MaxRetries int
}

// Attempts 返回该策略下最多调用次数（首次 + 重试）。
func (p Policy) Attempts() int {
	p = p.normalized()
	return p.MaxRetries + 1
}

// DelayFor 返回第 retry 次重试（从 1 开始）前的等待时长；超出该策略的重试次数返回 false。
func (p Policy) DelayFor(retry int) (time.Duration, bool) {
	p = p.normalized()
	if retry < 1 || retry > p.MaxRetries {
		return 0, false
	}
	return p.Delays[retry-1], true
}

// DelaysText 把退避序列渲染成日志/错误文案（如 "5s/20s/60s"）。
// 整秒值统一按秒渲染——time.Duration 自带 String() 会把 60s 写成 "1m0s"，
// 与验收口径的写法对不上，排障时要多一次脑内换算。
func (p Policy) DelaysText() string {
	p = p.normalized()
	if len(p.Delays) == 0 {
		return "无"
	}
	parts := make([]string, 0, len(p.Delays))
	for _, d := range p.Delays {
		if d%time.Second == 0 {
			parts = append(parts, fmt.Sprintf("%ds", int64(d/time.Second)))
			continue
		}
		parts = append(parts, d.String())
	}
	return strings.Join(parts, "/")
}

// normalized 收敛非法配置：重试次数不超过可用退避项数（多出的重试没有等待时长，没有意义），
// 并丢弃非正值的等待项（<=0 等于「不退避」，会让重试变成对上游的连打）。
func (p Policy) normalized() Policy {
	delays := make([]time.Duration, 0, len(p.Delays))
	for _, d := range p.Delays {
		if d > 0 {
			delays = append(delays, d)
		}
	}
	p.Delays = delays
	if p.MaxRetries < 0 {
		p.MaxRetries = 0
	}
	if p.MaxRetries > len(delays) {
		p.MaxRetries = len(delays)
	}
	return p
}

// SleepFunc 是等待能力（测试注入假时钟，生产用 SleepContext）。
type SleepFunc func(ctx context.Context, d time.Duration) error

// SleepContext 在 ctx 取消时立即返回 ctx.Err()，否则睡满 d。
func SleepContext(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ExhaustedError 表示重试次数用尽后仍然失败（**不吞错**：Last 原样保留，可用 errors.Is/As 追踪）。
type ExhaustedError struct {
	// Op 是操作名（同步器里是步骤名），用于一眼定位「哪一步放弃了」。
	Op string
	// Attempts 是实际调用次数（含首次）。
	Attempts int
	// Retries 是实际用掉的重试次数（Attempts-1，除非 ctx 中断）。
	Retries int
	// Delays 是本次用到的退避序列。
	Delays []time.Duration
	// Last 是最后一次失败原因（原始错误，逐字保留）。
	Last error
}

func (e *ExhaustedError) Error() string {
	if e == nil {
		return "backoff: 重试耗尽"
	}
	return fmt.Sprintf("%s 重试耗尽：共调用 %d 次（重试 %d 次，退避 %s），最后错误：%v",
		e.Op, e.Attempts, e.Retries, Policy{Delays: e.Delays}.DelaysText(), e.Last)
}

// Unwrap 让 errors.Is/errors.As 能穿透到原始错误。
func (e *ExhaustedError) Unwrap() error { return e.Last }

// CanceledError 表示等待期间 ctx 被取消/超时（不再继续重试）。
type CanceledError struct {
	Op       string
	Attempts int
	Last     error
	Cause    error
}

func (e *CanceledError) Error() string {
	if e == nil {
		return "backoff: 已取消"
	}
	msg := fmt.Sprintf("%s 重试被中断（已调用 %d 次）：%v", e.Op, e.Attempts, e.Cause)
	if e.Last != nil {
		msg += fmt.Sprintf("；最后错误：%v", e.Last)
	}
	return msg
}

// Unwrap 同时暴露中断原因与最后一次业务错误：调用方既能 errors.Is(err, context.Canceled)，
// 也能 errors.Is(err, 具体业务错)。
func (e *CanceledError) Unwrap() []error {
	var out []error
	if e.Cause != nil {
		out = append(out, e.Cause)
	}
	if e.Last != nil {
		out = append(out, e.Last)
	}
	return out
}

// StopError 表示「fn 判定这是终止态，不要再重试」。
//
// 用途：把「本轮跳过」「配置缺失」这类**非故障**结论从重试里摘出来——否则它会按 5s/20s/60s
// 空转三次（一轮 85s），最后被包成 ExhaustedError，调用方再靠 errors.As 穿透才知道其实是跳过。
// Do 见到它立即把内部原始错误原样返回（不包 ExhaustedError、不再等待）。
type StopError struct {
	// Err 是真实的终止原因（逐字保留，可用 errors.Is/As 追踪）。
	Err error
}

func (e *StopError) Error() string {
	if e == nil || e.Err == nil {
		return "backoff: 终止"
	}
	return e.Err.Error()
}

// Unwrap 让 errors.Is/As 穿透到原始错误。
func (e *StopError) Unwrap() error { return e.Err }

// Stop 把 err 标记为终止态（err 为 nil 时原样返回 nil）。
func Stop(err error) error {
	if err == nil {
		return nil
	}
	return &StopError{Err: err}
}

// IsStop 判断错误是否被打上终止态标记。
func IsStop(err error) bool {
	var s *StopError
	return errors.As(err, &s)
}

// Do 以 p 的节奏执行 fn，返回 nil 或最后失败原因。
//
//   - fn 的 attempt 从 1 开始（首次尝试 = 1），便于日志区分「第一次就成功」与「重试成功」；
//   - 失败按 p 退避重试，重试次数用尽返回 *ExhaustedError（重试次数为 0 时直接返回原始错误）；
//   - ctx 在等待期间结束时返回 *CanceledError（不吞原始错误，也不再继续）。
func Do(ctx context.Context, op string, p Policy, sleep SleepFunc, fn func(ctx context.Context, attempt int) error) error {
	if fn == nil {
		return errors.New("backoff: fn 不能为空")
	}
	if sleep == nil {
		sleep = SleepContext
	}
	p = p.normalized()
	if p.MaxRetries == 0 {
		// 不重试的路径保持错误原样返回，不包一层「重试耗尽」噪声。
		return fn(ctx, 1)
	}

	attempt := 0
	var last error
	for {
		attempt++
		last = fn(ctx, attempt)
		if last == nil {
			return nil
		}
		// 终止态（如「本轮跳过」）：不等待、不重试、不包 ExhaustedError，原样返回原因。
		var stop *StopError
		if errors.As(last, &stop) {
			return stop.Err
		}
		if ctx.Err() != nil {
			return &CanceledError{Op: op, Attempts: attempt, Last: last, Cause: ctx.Err()}
		}
		if attempt > p.MaxRetries {
			break
		}
		d, ok := p.DelayFor(attempt)
		if !ok {
			break
		}
		if err := sleep(ctx, d); err != nil {
			return &CanceledError{Op: op, Attempts: attempt, Last: last, Cause: err}
		}
	}
	return &ExhaustedError{
		Op:       op,
		Attempts: attempt,
		Retries:  attempt - 1,
		Delays:   append([]time.Duration(nil), p.Delays...),
		Last:     last,
	}
}
