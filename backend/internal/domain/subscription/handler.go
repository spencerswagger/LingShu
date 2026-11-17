package subscription

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/team/llmgateway/internal/domain/amortize"
	"github.com/team/llmgateway/internal/domain/audit"
	"github.com/team/llmgateway/internal/pkg/resp"
)

const (
	// amortizeMinInterval 是手工重算的最小间隔（标准 §6.3：建议 1 次/10 秒，返回 429）。
	// 只限制手工入口，不影响 5 分钟的定时轮。
	amortizeMinInterval = 10 * time.Second

	// 录入请求体上限：字段都很小，64KiB 已远超正常输入（备注 500 字）。
	createBodyLimit = 64 << 10
)

// Amortizer 是手工重算依赖的能力（*amortize.Allocator 满足）。
type Amortizer interface {
	RunOnce(ctx context.Context) (amortize.Result, error)
}

// Handler 是订阅的两个管理端接口：
//
//	S2 POST /api/v1/admin/subscriptions               录入（自然键 + 单位积分 + 派生判定）
//	S6 POST /api/v1/admin/subscriptions/amortize/run  手工重算一轮（单实例限流 1 次/10 秒）
//
// 全部挂在 /api/v1/admin 下：成本侧数据只给 ADMIN（开发者端零入口，标准 §6.3）。
type Handler struct {
	svc       *Service
	amortizer Amortizer
	audit     AuditSink
	log       *slog.Logger
	now       func() time.Time
	limit     amortizeLimit
}

// NewHandler 创建订阅处理器。
func NewHandler(svc *Service, amortizer Amortizer, auditSink AuditSink, logger *slog.Logger) *Handler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Handler{svc: svc, amortizer: amortizer, audit: auditSink, log: logger, now: time.Now}
}

// HandleCreate 处理 S2。
func (h *Handler) HandleCreate(w http.ResponseWriter, r *http.Request) {
	body := http.MaxBytesReader(w, r.Body, createBodyLimit)
	var in Input
	if err := json.NewDecoder(body).Decode(&in); err != nil {
		resp.ErrReason(w, r, http.StatusBadRequest, resp.CodeBadRequest, ReasonInvalidField,
			"请求体不是合法 JSON："+err.Error(), nil)
		return
	}
	view, berr := h.svc.Create(r.Context(), in)
	if berr != nil {
		data := berr.Data
		if data == nil && berr.Field != "" {
			data = map[string]any{"Field": berr.Field}
		}
		resp.ErrReason(w, r, berr.Status, berr.Code, berr.Reason, berr.Message, data)
		return
	}
	resp.Created(w, r, view)
}

// HandleList 处理 S1：列表 + 派生状态 + 本期已摊/未摊出。
func (h *Handler) HandleList(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.svc == nil {
		resp.Err(w, r, http.StatusServiceUnavailable, resp.CodeInternalError, "订阅服务未装配")
		return
	}
	out, berr := h.svc.List(r.Context(), parseFilter(r.URL.Query()))
	if berr != nil {
		resp.ErrReason(w, r, berr.Status, berr.Code, berr.Reason, berr.Message, berr.Data)
		return
	}
	resp.OK(w, r, out)
}

// HandleFormOptions 处理 S7：录入页候选（provider 只读、账号软候选、汇率/R 可用性）。
func (h *Handler) HandleFormOptions(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.svc == nil {
		resp.Err(w, r, http.StatusServiceUnavailable, resp.CodeInternalError, "订阅服务未装配")
		return
	}
	out, berr := h.svc.FormOptions(r.Context())
	if berr != nil {
		resp.ErrReason(w, r, berr.Status, berr.Code, berr.Reason, berr.Message, berr.Data)
		return
	}
	resp.OK(w, r, out)
}

// HandleAmortizeRun 处理 S6：手工重算一轮，复用 amortize.Allocator.RunOnce（不另写一套分摊逻辑）。
func (h *Handler) HandleAmortizeRun(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.amortizer == nil {
		resp.Err(w, r, http.StatusServiceUnavailable, resp.CodeInternalError, "分摊服务未装配")
		return
	}
	now := h.now()
	if ok, wait := h.limit.allow(now); !ok {
		secs := int(wait.Seconds())
		if secs < 1 {
			secs = 1
		}
		w.Header().Set("Retry-After", strconv.Itoa(secs))
		resp.ErrReason(w, r, http.StatusTooManyRequests, resp.CodeRateLimited, ReasonRateLimited,
			"手工重算过于频繁，请 "+strconv.Itoa(secs)+" 秒后再试（定时轮不受影响）", nil)
		return
	}

	res, err := h.amortizer.RunOnce(r.Context())
	h.auditRun(r.Context(), res, err)
	if err != nil {
		// 已发生的部分写入会在下一轮被 SET 重算收敛（幂等），所以这里只报错不补偿。
		resp.ErrReason(w, r, http.StatusInternalServerError, resp.CodeInternalError, ReasonInternal,
			"手工重算失败（已写入的部分下轮会重算收敛）："+err.Error(), nil)
		return
	}
	resp.OK(w, r, res)
}

func (h *Handler) auditRun(ctx context.Context, res amortize.Result, runErr error) {
	if h.audit == nil {
		return
	}
	detail := map[string]any{
		"subscriptions": res.Subscriptions,
		"skipped":       res.Skipped,
		"updated":       res.Updated,
		"allocated":     res.Allocated,
		"unallocated":   res.Unallocated,
		"ok":            runErr == nil,
	}
	if runErr != nil {
		detail["error"] = runErr.Error()
	}
	if err := h.audit.Insert(ctx, audit.Entry{
		Action:     "subscription.amortize.run",
		TargetType: "subscription",
		TargetID:   "amortize",
		Detail:     detail,
	}); err != nil {
		h.log.Error("手工重算审计写入失败", "err", err)
	}
}

// amortizeLimit 是单实例的手工重算限流（进程内即可：本接口防的是运营连点，
// 不是分布式配额；多副本时每副本各 1 次/10 秒仍然远低于打爆数据库的量级）。
type amortizeLimit struct {
	mu       sync.Mutex
	last     time.Time
	interval time.Duration
}

// allow 返回是否放行与需要等待的时长。
func (l *amortizeLimit) allow(now time.Time) (bool, time.Duration) {
	interval := l.interval
	if interval <= 0 {
		interval = amortizeMinInterval
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() {
		if elapsed := now.Sub(l.last); elapsed < interval {
			return false, interval - elapsed
		}
	}
	l.last = now
	return true, 0
}
