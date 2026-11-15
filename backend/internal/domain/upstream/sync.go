package upstream

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/team/llmgateway/internal/pkg/backoff"
	"github.com/team/llmgateway/internal/pkg/logger"
)

// DefaultInterval 是同步器默认周期（启动即跑一轮，之后「跑完再等」）。
const DefaultInterval = 60 * time.Second

// Step 是一次可重试的同步动作（协议无关：拉取快照 → 落库/替换内存镜像）。
//
// 实现方不要再自己做重试：重试节奏由 Syncer 统一按 5s/20s/60s（最多 3 次）执行，
// 否则一次步骤里会嵌套两套退避，实际间隔与验收口径对不上。
type Step interface {
	// Name 是步骤名（日志与错误文案里的定位标识，同一次装配内必须唯一）。
	Name() string
	// Run 执行一次拉取/落库；返回错误即视为该次尝试失败（由 Syncer 决定是否重试）。
	Run(ctx context.Context) error
}

// StepFunc 把函数适配成 Step。
type StepFunc struct {
	N string
	F func(ctx context.Context) error
}

// Name 实现 Step。
func (s StepFunc) Name() string { return s.N }

// Run 实现 Step。
func (s StepFunc) Run(ctx context.Context) error {
	if s.F == nil {
		return fmt.Errorf("步骤 %s 未实现", s.N)
	}
	return s.F(ctx)
}

// SkipError 表示「本轮不同步」，不是故障：降级开关打开、基址未配置、上一轮仍在运行等。
//
// 调用方必须把它记 INFO 而不是打红，并且**不得据此清空已有快照**（沿用上期数据）。
type SkipError struct {
	Op     string
	Reason string
}

func (e *SkipError) Error() string {
	if e == nil {
		return "跳过"
	}
	return fmt.Sprintf("%s 本轮跳过：%s", e.Op, e.Reason)
}

// IsSkip 判断错误是否为「本轮跳过」（用于日志分级，不影响是否需要重试）。
func IsSkip(err error) bool {
	var s *SkipError
	return errors.As(err, &s)
}

// ErrBusy 是严格串行的体现：上一轮还没跑完时，新的触发直接跳过而不是排队/并发。
var ErrBusy = &SkipError{Op: "上游同步", Reason: "上一轮仍在运行（严格串行，不并发）"}

// SyncerOptions 是 NewSyncer 的入参（零值可用：默认 5s/20s/60s 退避 + 真实等待 + 默认 logger）。
type SyncerOptions struct {
	// Policy 是重试节奏；零值表示不重试，默认用 backoff.Default()。
	Policy backoff.Policy
	// Sleep 是等待能力（测试注入假时钟；nil 用 backoff.SleepContext）。
	Sleep backoff.SleepFunc
	// Logger 为 nil 时用默认 logger。
	Logger *slog.Logger
	// Gate 可选：返回 *SkipError 时本轮整体跳过（配置/降级开关的接入点，见 ConfigGate）。
	// 返回其它错误视为真故障（不重试，直接返回给调用方）。
	Gate func(ctx context.Context) error
}

// Syncer 严格串行执行注册的步骤。
//
// 串行有两层保证，缺一不可：
//  1. 单轮内部按注册顺序逐个 await（同 goroutine，天然不并发）；
//  2. 重复触发（手动触发撞上定时轮次）由 running 标记直接拒绝（ErrBusy），
//     不排队、不并发——排队会让积压的轮次在网络恢复后一起打上游。
type Syncer struct {
	policy backoff.Policy
	sleep  backoff.SleepFunc
	log    *slog.Logger
	gate   func(ctx context.Context) error

	mu    sync.RWMutex
	steps []Step

	running atomic.Bool
}

// NewSyncer 创建同步器。
func NewSyncer(opts SyncerOptions) *Syncer {
	p := opts.Policy
	if p.MaxRetries == 0 && len(p.Delays) == 0 {
		p = backoff.Default()
	}
	log := opts.Logger
	if log == nil {
		log = logger.Fallback()
	}
	sleep := opts.Sleep
	if sleep == nil {
		sleep = backoff.SleepContext
	}
	return &Syncer{policy: p, sleep: sleep, log: log, gate: opts.Gate}
}

// Register 追加一个步骤（顺序即执行顺序）；名字为空或重复直接报错，避免日志里两个步骤同名。
func (s *Syncer) Register(step Step) error {
	if step == nil {
		return errors.New("upstream: 步骤不能为空")
	}
	name := step.Name()
	if name == "" {
		return errors.New("upstream: 步骤名不能为空")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, existing := range s.steps {
		if existing.Name() == name {
			return fmt.Errorf("upstream: 步骤名重复 %q", name)
		}
	}
	s.steps = append(s.steps, step)
	return nil
}

// Steps 返回已注册步骤名（按执行顺序），供装配自检与日志。
func (s *Syncer) Steps() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]string, 0, len(s.steps))
	for _, step := range s.steps {
		out = append(out, step.Name())
	}
	return out
}

// RunOnce 串行执行一遍全部步骤，返回本轮所有失败（errors.Join）。
//
// 口径：
//   - 每个步骤独立重试（一个步骤耗尽不影响后续步骤，避免一个接口故障拖死整轮）；
//   - 每次尝试都按 backoff 的统一节奏，耗尽后记 ERROR（步骤名/实际尝试次数/退避序列/原始错误）；
//   - 闸门返回 *SkipError 时本轮**不执行任何步骤**，原样返回该跳过原因。
func (s *Syncer) RunOnce(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if s.gate != nil {
		if err := s.gate(ctx); err != nil {
			return err
		}
	}
	if !s.running.CompareAndSwap(false, true) {
		return ErrBusy
	}
	defer s.running.Store(false)

	steps := s.snapshot()
	var errs []error
	for _, step := range steps {
		if err := ctx.Err(); err != nil {
			errs = append(errs, fmt.Errorf("上游同步中止（%s 未执行）：%w", step.Name(), err))
			break
		}
		name := step.Name()
		started := time.Now()
		err := backoff.Do(ctx, name, s.policy, s.sleep, func(ctx context.Context, attempt int) error {
			if attempt > 1 {
				s.log.Warn("上游同步重试", "step", name, "attempt", attempt)
			}
			return step.Run(ctx)
		})
		switch {
		case err == nil:
			s.log.Info("上游同步完成", "step", name, "cost_ms", time.Since(started).Milliseconds())
		case IsSkip(err):
			s.log.Info("上游同步跳过", "step", name, "reason", err.Error())
		default:
			var ex *backoff.ExhaustedError
			if errors.As(err, &ex) {
				// 可诊断：步骤名 + 实际调用次数 + 重试次数 + 退避序列 + 原始错误（逐字保留）。
				s.log.Error("上游同步放弃",
					"step", name,
					"attempts", ex.Attempts,
					"retries", ex.Retries,
					"backoff", ex.Delays,
					"err", ex.Last)
			} else {
				s.log.Error("上游同步失败", "step", name, "err", err)
			}
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Start 启动同步循环（阻塞到 ctx 结束）：启动即跑一轮，之后**跑完再等** interval。
//
// 「跑完再等」而不是「固定 tick」是刻意的：一轮耗时超过 interval 时不会堆积下一轮，
// 这是严格串行的外层保证（ticker 会补发 tick，等于偷偷并发）。
func (s *Syncer) Start(ctx context.Context, interval time.Duration) {
	if ctx == nil {
		ctx = context.Background()
	}
	if interval <= 0 {
		interval = DefaultInterval
	}
	for {
		if err := s.RunOnce(ctx); err != nil && !IsSkip(err) && !errors.Is(err, context.Canceled) {
			s.log.Error("上游同步轮次失败", "err", err)
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
	}
}

// snapshot 取一份步骤快照，注册与执行并发时不持有锁跑网络调用。
func (s *Syncer) snapshot() []Step {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]Step(nil), s.steps...)
}
