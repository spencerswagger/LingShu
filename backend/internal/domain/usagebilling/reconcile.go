// reconcile.go 是 60 秒对账：灵枢账单（旁路 usage）vs 代理侧账本，**只告警、绝不回写账单**。
//
// 只比 token 三段与请求笔数，不比金额：代理侧按美元牌价、灵枢按人民币倍率，价源与币种都不同，
// 比钱必然误报（契约 §3.6）。
//
// 阈值（契约 §3.6 + §7 补丁）：
//   - 分组：笔数 ≥5 且任一段相对偏差 >5% → alarm；
//   - 窗口整体：按 token 加权偏差 >2% 且总量 ≥10000 → alarm；
//   - 相对偏差分母以**代理侧为基准**：|ls-magpie|/magpie；magpie=0 且 ls>0 → 该段直接命中；
//     两边同为 0 → 跳过（n/a）；
//   - 笔数不一致只进 detail，不触发 alarm（笔数仅作分组样本门槛）；
//   - 连续 3 窗 alarm → 升级（写审计 + ERROR 日志），仍不回写账单。
package usagebilling

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/pkg/idgen"
)

// 对账常量集中在此（口径改动只改这里）。
const (
	// ReconcileInterval 是窗口长度与调度周期。
	ReconcileInterval = 60 * time.Second
	// GroupThresholdPct 是分组偏差阈值（%）。
	GroupThresholdPct = 5.0
	// GroupMinRequests 是分组样本门槛（笔数）。
	GroupMinRequests = 5
	// WindowThresholdPct 是窗口整体加权偏差阈值（%）。
	WindowThresholdPct = 2.0
	// WindowMinTokens 是窗口整体判定的最小样本量（token）。
	WindowMinTokens = 10000
	// EscalateStreak 是连续 alarm 窗数达到即升级。
	EscalateStreak = 3
)

// Group 是对账维度：callerKey + 模型，附带三段 token 与笔数。
type Group struct {
	CallerKey string `json:"caller_key"`
	Model     string `json:"model"`
	Requests  int64  `json:"requests"`
	Input     int64  `json:"input"`
	CacheRead int64  `json:"cache_read"`
	Output    int64  `json:"output"`
	// Estimated 标记该组来自估算账单（灵枢侧专有：这部分不计入主口径的 token 对比）。
	Estimated bool `json:"estimated,omitempty"`
}

// Tokens 返回三段之和（窗口整体判定的权重）。
func (g Group) Tokens() int64 { return g.Input + g.CacheRead + g.Output }

// groupKey 是分组键（callerKey + 模型）。
func (g Group) groupKey() string { return g.CallerKey + "\x00" + g.Model }

// LedgerClient 拉取代理侧账本。第二个返回值是字段可见性报告（契约 §8.2 降级表）：
// 必需项缺失或分页只支持 offset → 该窗口不可对账（标 blocked: pending POC）。
type LedgerClient interface {
	FetchWindow(ctx context.Context, from, to time.Time, callerKeys []string) ([]Group, LedgerReport, error)
}

// ReconcileStore 是对账落库依赖。
type ReconcileStore interface {
	// LocalGroups 取窗口内灵枢侧账单聚合（把 estimated 单单独成组）。
	LocalGroups(ctx context.Context, from, to time.Time) ([]Group, error)
	// CallerKeys 返回需要比对的 callerKey 全集（magpie_keys ACTIVE）。
	CallerKeys(ctx context.Context) ([]string, error)
	// PreviousStreak 取上一窗的 alarm 连续数。
	PreviousStreak(ctx context.Context) (int, error)
	// SaveWindow 幂等写入窗口结果（UNIQUE(window_start,window_end) + ON CONFLICT DO UPDATE）。
	SaveWindow(ctx context.Context, w Window) error
}

// Auditor 写审计（升级时用；nil = 只打日志）。
type Auditor interface {
	InsertReconcileAlarm(ctx context.Context, detail map[string]any) error
}

// Window 是一个对账窗口的结果行（对应 reconcile_windows）。
type Window struct {
	WindowStart time.Time
	WindowEnd   time.Time
	ReqCountLS  int64
	ReqCountMag int64
	TokensLS    Group
	TokensMag   Group
	// DevPct 是窗口整体加权偏差（%）；无法计算时为 nil。
	DevPct      *float64
	Status      string // ok / alarm / error / skipped
	AlarmStreak int
	Detail      map[string]any
}

// Reconciler 周期性对账。
type Reconciler struct {
	store  ReconcileStore
	ledger LedgerClient
	audit  Auditor
	logger *slog.Logger
	now    func() time.Time
	// interval 允许测试缩短。
	interval time.Duration
	// lock 是可选的多副本互斥（拿不到锁就跳过本窗）。
	lock func(ctx context.Context) (release func(), ok bool)
}

// NewReconciler 装配对账器。ledger 为 nil 时窗口记 skipped（未接入账本不误报）。
func NewReconciler(store ReconcileStore, ledger LedgerClient, audit Auditor, logger *slog.Logger) *Reconciler {
	if logger == nil {
		logger = slog.Default()
	}
	return &Reconciler{store: store, ledger: ledger, audit: audit, logger: logger, now: time.Now, interval: ReconcileInterval}
}

// SetInterval 调整调度周期（测试用）。
func (r *Reconciler) SetInterval(d time.Duration) { r.interval = d }

// SetLock 注入多副本互斥（通常为 SQLReconcileStore.TryAdvisoryLock）。
func (r *Reconciler) SetLock(fn func(ctx context.Context) (release func(), ok bool)) { r.lock = fn }

// Run 每 interval 跑一次，直到 ctx 结束。
func (r *Reconciler) Run(ctx context.Context) {
	r.RunOnce(ctx)
	ticker := time.NewTicker(r.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.RunOnce(ctx)
		}
	}
}

// RunOnce 跑一个窗口：[now-interval, now)。
func (r *Reconciler) RunOnce(ctx context.Context) {
	if r.lock != nil {
		release, ok := r.lock(ctx)
		if !ok {
			return // 其他副本正在跑本窗
		}
		defer release()
	}
	if r.ledger == nil {
		r.saveSkipped(ctx, "ledger 未装配")
		return
	}
	end := r.now()
	start := end.Add(-r.interval)
	w, err := r.buildWindow(ctx, start, end)
	if err != nil {
		// 拉取失败：记 error + 60s 退避语义（下一窗自然重试），绝不改动账单。
		r.logger.ErrorContext(ctx, "对账失败", "window_start", start, "err", err.Error())
		w = Window{WindowStart: start, WindowEnd: end, Status: "error",
			Detail: map[string]any{"error": err.Error()}}
	}
	if err := r.store.SaveWindow(ctx, w); err != nil {
		r.logger.ErrorContext(ctx, "对账结果落库失败", "err", err.Error())
		return
	}
	if w.Status == "alarm" {
		r.logger.WarnContext(ctx, "对账偏差超阈值", "window_end", end,
			"dev_pct", w.DevPct, "streak", w.AlarmStreak)
		if w.AlarmStreak >= EscalateStreak {
			r.logger.ErrorContext(ctx, "对账偏差连续超阈值，已升级", "window_end", end, "streak", w.AlarmStreak)
			if r.audit != nil {
				if err := r.audit.InsertReconcileAlarm(ctx, w.Detail); err != nil {
					r.logger.ErrorContext(ctx, "对账升级审计写入失败", "err", err.Error())
				}
			}
		}
	}
}

// buildWindow 汇总两侧数据并判定。
func (r *Reconciler) buildWindow(ctx context.Context, start, end time.Time) (Window, error) {
	keys, err := r.store.CallerKeys(ctx)
	if err != nil {
		return Window{}, fmt.Errorf("取 callerKey 全集: %w", err)
	}
	local, err := r.store.LocalGroups(ctx, start, end)
	if err != nil {
		return Window{}, fmt.Errorf("取灵枢账单聚合: %w", err)
	}
	var magpie []Group
	report := LedgerReport{Paging: "none"}
	if len(keys) > 0 {
		magpie, report, err = r.ledger.FetchWindow(ctx, start, end, keys)
		if err != nil {
			return Window{}, fmt.Errorf("拉代理侧账本: %w", err)
		}
	}
	prev, err := r.store.PreviousStreak(ctx)
	if err != nil {
		prev = 0 // 读不到上一窗按 0 处理（不阻断本窗）
	}
	w := compareWindow(start, end, local, magpie, prev)

	// 契约 §8.2：字段不全（或只有 offset 分页）→ 该窗口不可对账。
	// 本地比对结果只作为「参考」留在 detail，状态既不算通过也不算不通过，且不参与连续告警升级。
	if reasons := report.BlockedReasons(); len(reasons) > 0 {
		w.Detail["local_comparison"] = map[string]any{
			"status":           w.Status,
			"dev_pct":          w.DevPct,
			"alarm_groups":     w.Detail["alarm_groups"],
			"token_sum_ls":     w.Detail["token_sum_ls"],
			"token_sum_magpie": w.Detail["token_sum_magpie"],
		}
		w.Detail["blocked"] = BlockedPendingPOC
		w.Detail["blocked_reasons"] = reasons
		w.Status = "blocked"
		w.AlarmStreak = 0
	}
	w.Detail["ledger_report"] = report
	return w, nil
}

// saveSkipped 在账本未装配时留一条 skipped 记录（可观测「没在对账」）。
//
// 与 blocked 的区别：skipped = 我们自己没接上账本；blocked = 接上了但字段不全
// （端到端未验，pending POC），两者都不改任何账单。
func (r *Reconciler) saveSkipped(ctx context.Context, reason string) {
	end := r.now()
	start := end.Add(-r.interval)
	w := Window{WindowStart: start, WindowEnd: end, Status: "skipped",
		Detail: map[string]any{"reason": reason}}
	if err := r.store.SaveWindow(ctx, w); err != nil {
		r.logger.ErrorContext(ctx, "对账 skipped 落库失败", "err", err.Error())
	}
}

// compareWindow 是纯函数：两侧聚合 → 窗口结果（便于单测直接覆盖判定口径）。
func compareWindow(start, end time.Time, local, magpie []Group, prevStreak int) Window {
	localMap := indexGroups(local)
	magpieMap := indexGroups(magpie)

	w := Window{WindowStart: start, WindowEnd: end, Detail: map[string]any{}}

	// 主口径只比 estimated=false 的组（契约：estimated 单列一组，不计入主口径）。
	estimatedGroups := 0
	localMain := make(map[string]Group)
	for k, g := range localMap {
		if g.Estimated {
			estimatedGroups++
			continue
		}
		localMain[k] = g
	}

	keys := make([]string, 0, len(localMain)+len(magpieMap))
	seen := map[string]bool{}
	for k := range localMain {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	for k := range magpieMap {
		if !seen[k] {
			seen[k] = true
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)

	var (
		alarmGroups []map[string]any
		sumLSTokens int64
		sumMagToks  int64
		sumAbsDelta int64
	)
	for _, k := range keys {
		ls := localMain[k]
		mg := magpieMap[k]
		w.ReqCountLS += ls.Requests
		w.ReqCountMag += mg.Requests
		sumLSTokens += ls.Tokens()
		sumMagToks += mg.Tokens()
		sumAbsDelta += absInt64(ls.Input-mg.Input) + absInt64(ls.CacheRead-mg.CacheRead) + absInt64(ls.Output-mg.Output)

		// 只有两侧都有账单的分组才谈得上比对：单侧缺账（含灵枢只有估算单）
		// 记进 absent_in_* 供人工定位，不做偏差报警。
		if ls.Requests == 0 || mg.Requests == 0 {
			continue
		}
		// 分组判定：样本门槛 + 任一段相对偏差 > 5%（分母=代理侧）。
		if mg.Requests < GroupMinRequests {
			continue
		}
		seg, hit := segmentAlarm(ls, mg)
		if hit {
			alarmGroups = append(alarmGroups, map[string]any{
				"caller_key": mg.CallerKey, "model": mg.Model,
				"ls":      map[string]int64{"input": ls.Input, "cache_read": ls.CacheRead, "output": ls.Output, "requests": ls.Requests},
				"magpie":  map[string]int64{"input": mg.Input, "cache_read": mg.CacheRead, "output": mg.Output, "requests": mg.Requests},
				"segment": seg,
			})
		}
	}
	w.TokensLS = Group{Input: sumSeg(localMain, "input"), CacheRead: sumSeg(localMain, "cache_read"), Output: sumSeg(localMain, "output")}
	w.TokensMag = Group{Input: sumSeg(magpieMap, "input"), CacheRead: sumSeg(magpieMap, "cache_read"), Output: sumSeg(magpieMap, "output")}

	// 窗口整体加权偏差（%）：Σ|ls-magpie| / Σmagpie；分母为 0 时无从比较。
	if sumMagToks > 0 {
		pct := float64(sumAbsDelta) / float64(sumMagToks) * 100
		w.DevPct = &pct
	}

	windowAlarm := w.DevPct != nil && *w.DevPct > WindowThresholdPct && sumMagToks >= WindowMinTokens
	if len(alarmGroups) > 0 || windowAlarm {
		w.Status = "alarm"
		w.AlarmStreak = prevStreak + 1
	} else {
		w.Status = "ok"
		w.AlarmStreak = 0
	}

	w.Detail = map[string]any{
		"thresholds": map[string]any{
			"group_pct": GroupThresholdPct, "group_min_requests": GroupMinRequests,
			"window_pct": WindowThresholdPct, "window_min_tokens": WindowMinTokens,
			"escalate_streak": EscalateStreak,
		},
		"window_alarm":       windowAlarm,
		"alarm_groups":       alarmGroups,
		"token_sum_magpie":   sumMagToks,
		"token_sum_ls":       sumLSTokens,
		"absent_in_magpie":   absentGroups(localMain, magpieMap),
		"absent_in_ls":       absentGroups(magpieMap, localMain),
		"estimated_groups":   estimatedGroups,
		"req_count_mismatch": w.ReqCountLS != w.ReqCountMag,
	}
	return w
}

// segmentAlarm 判定单个分组是否有任一段超阈值，返回命中的段名。
//
// 口径：分母是代理侧；magpie=0 且 ls>0 → 直接命中；两边同为 0 → 该段跳过。
func segmentAlarm(ls, mg Group) (string, bool) {
	type seg struct {
		name       string
		ls, magpie int64
	}
	for _, s := range []seg{
		{"input", ls.Input, mg.Input},
		{"cache_read", ls.CacheRead, mg.CacheRead},
		{"output", ls.Output, mg.Output},
	} {
		if s.magpie == 0 {
			if s.ls > 0 {
				return s.name, true
			}
			continue
		}
		if math.Abs(float64(s.ls-s.magpie))/float64(s.magpie)*100 > GroupThresholdPct {
			return s.name, true
		}
	}
	return "", false
}

func indexGroups(gs []Group) map[string]Group {
	out := make(map[string]Group, len(gs))
	for _, g := range gs {
		key := g.groupKey()
		cur := out[key]
		cur.CallerKey, cur.Model = g.CallerKey, g.Model
		cur.Requests += g.Requests
		cur.Input += g.Input
		cur.CacheRead += g.CacheRead
		cur.Output += g.Output
		cur.Estimated = cur.Estimated || g.Estimated
		out[key] = cur
	}
	return out
}

func sumSeg(m map[string]Group, seg string) int64 {
	var n int64
	for _, g := range m {
		switch seg {
		case "input":
			n += g.Input
		case "cache_read":
			n += g.CacheRead
		case "output":
			n += g.Output
		}
	}
	return n
}

// absentGroups 返回「只在 other 侧出现」的分组名（便于定位缺账/多账）。
func absentGroups(have map[string]Group, other map[string]Group) []string {
	var out []string
	for k, g := range have {
		if _, ok := other[k]; !ok && g.Requests > 0 {
			out = append(out, g.CallerKey+"|"+g.Model)
		}
	}
	sort.Strings(out)
	return out
}

func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

// ===== SQL 实现 =====

// SQLReconcileStore 用 PostgreSQL 实现 ReconcileStore。
type SQLReconcileStore struct{ db *sql.DB }

// NewSQLReconcileStore 装配对账落库。
func NewSQLReconcileStore(db *sql.DB) *SQLReconcileStore { return &SQLReconcileStore{db: db} }

// LocalGroups 按 (callerKey, 代理模型) 聚合窗口内灵枢账单；estimated 单单独成组。
//
// 只统计 completed（失败单没有 token，也不该参与 usage 对账）。
func (s *SQLReconcileStore) LocalGroups(ctx context.Context, from, to time.Time) ([]Group, error) {
	const q = `SELECT magpie_key_name, internal_model_id, estimated,
		count(*),
		COALESCE(SUM((tokens->>'input')::bigint), 0),
		COALESCE(SUM((tokens->>'cache_read')::bigint), 0),
		COALESCE(SUM((tokens->>'output')::bigint), 0)
		FROM billing_records
		WHERE magpie_key_name IS NOT NULL AND status = 'completed'
		  AND call_time >= $1 AND call_time < $2
		GROUP BY 1, 2, 3`
	rows, err := s.db.QueryContext(ctx, q, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.CallerKey, &g.Model, &g.Estimated, &g.Requests, &g.Input, &g.CacheRead, &g.Output); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CallerKeys 返回 magpie_keys 里 ACTIVE 的 callerKey 名单。
func (s *SQLReconcileStore) CallerKeys(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key_name FROM magpie_keys WHERE state = 'ACTIVE' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// PreviousStreak 取最近一窗的 alarm_streak（无历史返回 0）。
func (s *SQLReconcileStore) PreviousStreak(ctx context.Context) (int, error) {
	var streak int
	err := s.db.QueryRowContext(ctx,
		`SELECT alarm_streak FROM reconcile_windows ORDER BY window_end DESC LIMIT 1`).Scan(&streak)
	if err != nil {
		if err == sql.ErrNoRows {
			return 0, nil
		}
		return 0, err
	}
	return streak, nil
}

// SaveWindow 幂等写入窗口结果（同窗口重复跑只更新）。
func (s *SQLReconcileStore) SaveWindow(ctx context.Context, w Window) error {
	tokensLS, err := json.Marshal(w.TokensLS)
	if err != nil {
		return err
	}
	tokensMag, err := json.Marshal(w.TokensMag)
	if err != nil {
		return err
	}
	detail, err := json.Marshal(w.Detail)
	if err != nil {
		return err
	}
	const q = `INSERT INTO reconcile_windows(
		id, window_start, window_end, req_count_ls, req_count_magpie, tokens_ls, tokens_magpie,
		dev_pct, status, alarm_streak, detail)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (window_start, window_end) DO UPDATE SET
		req_count_ls = EXCLUDED.req_count_ls, req_count_magpie = EXCLUDED.req_count_magpie,
		tokens_ls = EXCLUDED.tokens_ls, tokens_magpie = EXCLUDED.tokens_magpie,
		dev_pct = EXCLUDED.dev_pct, status = EXCLUDED.status,
		alarm_streak = EXCLUDED.alarm_streak, detail = EXCLUDED.detail, pulled_at = now()`
	var devPct any
	if w.DevPct != nil {
		devPct = *w.DevPct
	}
	_, err = s.db.ExecContext(ctx, q, idgen.New(), w.WindowStart, w.WindowEnd, w.ReqCountLS, w.ReqCountMag,
		tokensLS, tokensMag, devPct, w.Status, w.AlarmStreak, detail)
	return err
}

// TryAdvisoryLock 多副本只让一个实例跑对账（拿不到锁就跳过本窗）。
func (s *SQLReconcileStore) TryAdvisoryLock(ctx context.Context) (release func(), ok bool) {
	const lockKey = int64(0x4c5352434f4e) // "LSRCON"
	var got bool
	if err := s.db.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, lockKey).Scan(&got); err != nil || !got {
		return func() {}, false
	}
	return func() {
		_, _ = s.db.ExecContext(context.WithoutCancel(ctx), `SELECT pg_advisory_unlock($1)`, lockKey)
	}, true
}

// ===== 审计实现 =====

// AuditSink 把对账升级写进 audit_logs（user_id/username 为空表示系统动作）。
type AuditSink struct{ DB *sql.DB }

// InsertReconcileAlarm 写一条系统审计（不改写任何账单）。
func (a *AuditSink) InsertReconcileAlarm(ctx context.Context, detail map[string]any) error {
	raw, err := json.Marshal(detail)
	if err != nil {
		return err
	}
	_, err = a.DB.ExecContext(ctx,
		`INSERT INTO audit_logs(id, user_id, username, action, target_type, detail)
		 VALUES ($1, NULL, 'system', 'reconcile_alarm_escalated', 'reconcile_window', $2)`,
		idgen.New(), raw)
	return err
}

// String 便于日志（精简成一行）。
func (w Window) String() string {
	var b strings.Builder
	fmt.Fprintf(&b, "window=%s..%s status=%s streak=%d req=%d/%d",
		w.WindowStart.Format(time.RFC3339), w.WindowEnd.Format(time.RFC3339), w.Status, w.AlarmStreak,
		w.ReqCountLS, w.ReqCountMag)
	if w.DevPct != nil {
		fmt.Fprintf(&b, " dev=%.2f%%", *w.DevPct)
	}
	return b.String()
}
