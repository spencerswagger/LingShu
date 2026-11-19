package upstream

// 账本链路可观测性：不允许「端点已配、每轮 0 行、日志全绿」这种静默。
//
// 背景（P0）：旧闸门依赖 upstream.base_url（默认空串、无管理端写入口）→ 同步轮恒停摆，
// ledger_entries 永远空、账单不出，但日志里只有一行启动 WARN 甚至什么都没有。修复后端点配了就拉，
// 但如果「配了端点却始终 0 行」，仍然可能是端点不通 / 账本没数据 / 鉴权错——必须有一条显式告警。
//
// EmptyRoundWarner 聚合「连续空轮」：只有连续 threshold 轮一条都没拉到（且期间没有报错）才提示一次，
// 并带上最近一次 HTTP 状态或错误，避免把「正常无新数据」也刷成告警风暴。

// defaultEmptyRoundThreshold 是「连续空轮」默认阈值（与派工口径一致：连续 5 轮）。
const defaultEmptyRoundThreshold = 5

// EmptyRoundWarner 统计连续空轮并在达到阈值时提示一次。
//
// 用法（同步轮每秒/每分钟一跳，串行调用，无需加锁）：
//
//	w := upstream.NewEmptyRoundWarner(5)
//	res, err := rounder.RunOnce(ctx)
//	if w.Observe(res.Empty, res.LastHTTPStatus, err) {
//	    log.Warn("账本疑似未通/无新行", "streak", w.Streak(),
//	        "last_http_status", w.LastStatus(), "last_err", errText(w.LastError()))
//	}
type EmptyRoundWarner struct {
	threshold  int
	streak     int
	lastStatus int
	lastErr    error
	warned     bool
}

// NewEmptyRoundWarner 创建告警器；threshold <= 0 时用默认 5。
func NewEmptyRoundWarner(threshold int) *EmptyRoundWarner {
	if threshold <= 0 {
		threshold = defaultEmptyRoundThreshold
	}
	return &EmptyRoundWarner{threshold: threshold}
}

// Observe 记录一轮结果，返回 true 表示本轮应打「疑似未通/无新行」告警（只提示一次）。
//
// 规则：
//   - 本轮报错：清零连击、记下错误（下一轮空轮告警可带上）、重置告警标记；
//   - 本轮非空（拉到数据）：清零连击、清掉上次错误、重置告警标记；
//   - 本轮空且无错：连击 +1；达到阈值且尚未提示过 → 提示一次。
//
// httpStatus 只在本轮是有效 HTTP 响应时才有意义（0 = 未知，沿用上一次已知值）。
func (w *EmptyRoundWarner) Observe(empty bool, httpStatus int, err error) bool {
	if w == nil {
		return false
	}
	if httpStatus > 0 {
		w.lastStatus = httpStatus
	}
	if err != nil {
		w.streak = 0
		w.lastErr = err
		w.warned = false
		return false
	}
	if !empty {
		w.streak = 0
		w.lastErr = nil
		w.warned = false
		return false
	}
	w.streak++
	if w.streak >= w.threshold && !w.warned {
		w.warned = true
		return true
	}
	return false
}

// Streak 返回当前连续空轮数。
func (w *EmptyRoundWarner) Streak() int {
	if w == nil {
		return 0
	}
	return w.streak
}

// LastStatus 返回最近一次已知的 HTTP 状态（0 = 未知）。
func (w *EmptyRoundWarner) LastStatus() int {
	if w == nil {
		return 0
	}
	return w.lastStatus
}

// LastError 返回最近一次错误（可能为 nil）。
func (w *EmptyRoundWarner) LastError() error {
	if w == nil {
		return nil
	}
	return w.lastErr
}
