package reconcile

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/team/llmgateway/internal/db"
	"github.com/team/llmgateway/internal/domain/upstream"
)

// fakeAgg 是 WindowAggSource 的假实现（按窗口返回固定值）。
type fakeAgg struct {
	byStart map[time.Time]Agg
	err     error
	calls   int
}

func (f *fakeAgg) WindowAgg(_ context.Context, start, _ time.Time) (Agg, error) {
	f.calls++
	if f.err != nil {
		return Agg{}, f.err
	}
	return f.byStart[start], nil
}

// fakeCounts 记录 SaveCounts 的入参。
type fakeCounts struct {
	saved    []upstream.WindowCounts
	deferred int
}

func (f *fakeCounts) SaveCounts(_ context.Context, c upstream.WindowCounts) error {
	if err := c.Validate(); err != nil {
		return err
	}
	f.saved = append(f.saved, c)
	return nil
}

// fakeStates 返回预置的窗口状态。
type fakeStates map[time.Time]upstream.WindowStatus

func (f fakeStates) WindowStates(_ context.Context, from, to time.Time) (map[time.Time]upstream.WindowStatus, error) {
	out := map[time.Time]upstream.WindowStatus{}
	for k, v := range f {
		if !k.Before(from) && k.Before(to) {
			out[k] = v
		}
	}
	return out, nil
}

// TestDecide_Cases 覆盖判定表：对上 / 偏差 / 关闭账本 / 零分母。
func TestDecide_Cases(t *testing.T) {
	cases := []struct {
		name       string
		local, up  Agg
		prevStreak int16
		ledgerOn   bool
		wantStatus upstream.WindowStatus
		wantStreak int16
	}{
		{
			name: "完全对上", local: Agg{Count: 10, Tokens: 1000}, up: Agg{Count: 10, Tokens: 1000},
			ledgerOn: true, wantStatus: upstream.WindowOK, wantStreak: 0,
		},
		{
			name: "偏差 5% 超阈", local: Agg{Count: 10, Tokens: 1050}, up: Agg{Count: 10, Tokens: 1000},
			prevStreak: 1, ledgerOn: true, wantStatus: upstream.WindowAlarm, wantStreak: 2,
		},
		{
			name: "偏差 0.5% 在阈值内", local: Agg{Count: 10, Tokens: 1005}, up: Agg{Count: 10, Tokens: 1000},
			ledgerOn: true, wantStatus: upstream.WindowOK, wantStreak: 0,
		},
		{
			// 账本侧 0 token（本窗没有账本条目）不是「对上」：分母按 1 算，任何灵枢 token 都会超阈。
			name: "账本侧为空", local: Agg{Count: 3, Tokens: 300}, up: Agg{},
			ledgerOn: true, wantStatus: upstream.WindowAlarm, wantStreak: 1,
		},
		{
			name: "账本关闭", local: Agg{Count: 3, Tokens: 300}, up: Agg{},
			ledgerOn: false, wantStatus: upstream.WindowSkipped, wantStreak: 0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			status, streak, detail := decide(c.local, c.up, c.prevStreak, c.ledgerOn, DefaultTolerance)
			if status != c.wantStatus || streak != c.wantStreak {
				t.Fatalf("判定不符：status=%s streak=%d want %s/%d",
					status, streak, c.wantStatus, c.wantStreak)
			}
			// 计数必须进 detail 兜底（排障用），且灵枢侧数字不能丢。
			if detail["local_tokens"] != c.local.Tokens {
				t.Fatalf("detail 应带灵枢侧 token：%+v", detail)
			}
		})
	}
}

// TestReconciler_RunOnce_WritesCountsNotZero 是本切片的核心断言（D4）：
// 四个计数列必须被真实数字写入，不许恒 0；blocked 窗不许被覆盖。
func TestReconciler_RunOnce_WritesCountsNotZero(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 10, 0, 0, time.UTC) // 已结束窗口到 10:10
	end := now.Truncate(upstream.WindowSize)

	// 只关心最后一窗（10:09–10:10），其余窗口两侧都为空。
	local := &fakeAgg{byStart: map[time.Time]Agg{
		end.Add(-upstream.WindowSize): {Count: 7, Tokens: 700},
	}}
	up := &fakeAgg{byStart: map[time.Time]Agg{
		end.Add(-upstream.WindowSize): {Count: 7, Tokens: 700},
	}}
	counts := &fakeCounts{}
	states := fakeStates{end.Add(-3 * upstream.WindowSize): upstream.WindowBlocked}

	r := NewReconciler(Options{
		Local: local, Upstream: up, Counts: counts, States: states,
		LookbackWindowCount: 5, Now: func() time.Time { return now },
	})
	res, err := r.RunOnce(context.Background())
	if err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if res.Deferred != 1 {
		t.Fatalf("blocked 窗应被延后（不覆盖），got deferred=%d res=%+v", res.Deferred, res)
	}
	if len(counts.saved) != 4 {
		t.Fatalf("应写 4 个窗口（5 窗里 1 窗 blocked），got %d", len(counts.saved))
	}

	var last upstream.WindowCounts
	var sumLocalTokens int64
	for _, c := range counts.saved {
		if c.Start.Equal(end.Add(-upstream.WindowSize)) {
			last = c
		}
		sumLocalTokens += c.LocalTokens
		// 空窗落 0 是对的（本来就没有数据）；要挡的是「有数据的窗被写成 0」。
	}
	// 关键断言（D4）：注入的 700 token 必须原样落在计数列里 —— 恒 0 的计数列查不出漏账。
	if sumLocalTokens != 700 {
		t.Fatalf("灵枢侧 token 合计应为注入的 700（计数列必须被真实数字写入），got %d", sumLocalTokens)
	}
	if last.LocalCount != 7 || last.LocalTokens != 700 || last.UpstreamCount != 7 || last.UpstreamTokens != 700 {
		t.Fatalf("最后一窗计数不符：%+v", last)
	}
	if last.Status != upstream.WindowOK {
		t.Fatalf("两侧一致应为 ok，got %s", last.Status)
	}
}

// TestReconciler_RunOnce_LedgerDisabledSkips 断言账本关闭时状态为 skipped 且仍写灵枢侧计数。
func TestReconciler_RunOnce_LedgerDisabledSkips(t *testing.T) {
	now := time.Date(2026, 11, 15, 10, 10, 0, 0, time.UTC)
	end := now.Truncate(upstream.WindowSize)
	local := &fakeAgg{byStart: map[time.Time]Agg{
		end.Add(-upstream.WindowSize): {Count: 2, Tokens: 200},
	}}
	up := &fakeAgg{byStart: map[time.Time]Agg{}}
	counts := &fakeCounts{}
	r := NewReconciler(Options{
		Local: local, Upstream: up, Counts: counts,
		LedgerEnabled:       func() bool { return false },
		LookbackWindowCount: 1, Now: func() time.Time { return now },
	})
	if _, err := r.RunOnce(context.Background()); err != nil {
		t.Fatalf("对账失败: %v", err)
	}
	if len(counts.saved) != 1 {
		t.Fatalf("应写 1 个窗口，got %d", len(counts.saved))
	}
	got := counts.saved[0]
	if got.Status != upstream.WindowSkipped {
		t.Fatalf("账本关闭应为 skipped，got %s", got.Status)
	}
	if got.LocalTokens != 200 {
		t.Fatalf("skipped 窗也必须写灵枢侧计数，got %d", got.LocalTokens)
	}
	if up.calls != 0 {
		t.Fatalf("账本关闭时不该去查账本表，got %d 次", up.calls)
	}
}

// TestReconciler_RealSQL_WritesCounts 是真实库用例（真实迁移链 0001–0024）：
// 插一行账单 + 一行账本条目，跑真实 SQL 聚合与真实窗口存储，断言四个计数列落库非 0。
func TestReconciler_RealSQL_WritesCounts(t *testing.T) {
	conn := openMigratedTempDB(t)
	ctx := context.Background()

	win := time.Date(2026, 11, 15, 10, 0, 0, 0, time.UTC)
	winEnd := win.Add(upstream.WindowSize)

	// 先建用户（账单有 user_id 外键）。
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO users(id, username, password_hash, role) VALUES (1,'u1','x','DEVELOPER')`); err != nil {
		t.Fatalf("插入用户失败: %v", err)
	}
	// 账单行：cost_source 必须给 'none'（0020 的跨列 CHECK：无成本必须标明「没有成本口径」）。
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO billing_records(id, billing_id, user_id, pricing_mode, external_model_name,
		     internal_model_id, r_value, tokens, call_time, cost_source)
		 VALUES (1, 'b-1', 1, 'sale', 'm', 'im', 1,
		         '{"input":100,"cache_read":0,"output":20}'::jsonb, $1, 'none')`, win.Add(time.Second)); err != nil {
		t.Fatalf("插入账单失败: %v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO ledger_entries(id, ledger_id, provider, provider_account, model,
		     tokens_in, tokens_out, cost_credits, occurred_at)
		 VALUES (1, 'led-1', 'p', 'a', 'm', 100, 20, 1, $1)`, win.Add(2*time.Second)); err != nil {
		t.Fatalf("插入账本条目失败: %v", err)
	}

	wins := upstream.NewSQLWindowStore(conn)
	// 先建窗口行（对账是 UPDATE，不是插入）。
	if err := wins.Save(ctx, upstream.WindowRecord{
		Start: win, End: winEnd, Status: upstream.WindowOK, RecordedAt: winEnd,
	}); err != nil {
		t.Fatalf("写入窗口行失败: %v", err)
	}

	now := winEnd // 该窗已结束
	r := NewReconciler(Options{
		Local: NewLocalSource(conn), Upstream: NewUpstreamSource(conn),
		Counts: wins, States: wins, Streaks: wins,
		LookbackWindowCount: 1, Now: func() time.Time { return now },
	})
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatalf("对账失败: %v", err)
	}

	var (
		localCount, localTokens, upCount, upTokens int64
		status                                     string
	)
	if err := conn.QueryRowContext(ctx,
		`SELECT local_count, local_tokens, upstream_count, upstream_tokens, status
		   FROM reconcile_windows WHERE window_start = $1`, win).
		Scan(&localCount, &localTokens, &upCount, &upTokens, &status); err != nil {
		t.Fatalf("读取对账结果失败: %v", err)
	}
	if localCount != 1 || localTokens != 120 || upCount != 1 || upTokens != 120 {
		t.Fatalf("四个计数列必须落真实数字：local=%d/%d upstream=%d/%d",
			localCount, localTokens, upCount, upTokens)
	}
	if status != string(upstream.WindowOK) {
		t.Fatalf("两侧一致应为 ok，got %s", status)
	}

	// 幂等：再跑一轮结果不变（重算窗口不该叠加 alarm_streak 或换状态）。
	if _, err := r.RunOnce(ctx); err != nil {
		t.Fatalf("第二轮对账失败: %v", err)
	}
	var again int64
	if err := conn.QueryRowContext(ctx,
		`SELECT local_tokens FROM reconcile_windows WHERE window_start = $1`, win).Scan(&again); err != nil {
		t.Fatalf("第二轮读取失败: %v", err)
	}
	if again != 120 {
		t.Fatalf("重算应幂等，got local_tokens=%d", again)
	}
}

// TestSaveCounts_RefusesBlockedWindow 断言对账不许覆盖同步轮次持有的 blocked 行。
func TestSaveCounts_RefusesBlockedWindow(t *testing.T) {
	conn := openMigratedTempDB(t)
	ctx := context.Background()
	wins := upstream.NewSQLWindowStore(conn)

	win := time.Date(2026, 11, 15, 11, 0, 0, 0, time.UTC)
	if err := wins.Save(ctx, upstream.WindowRecord{
		Start: win, End: win.Add(upstream.WindowSize), Status: upstream.WindowBlocked,
		ScrollOffset: 4000, Reason: "单轮上限", RecordedAt: win,
	}); err != nil {
		t.Fatalf("写 blocked 行失败: %v", err)
	}

	err := wins.SaveCounts(ctx, upstream.WindowCounts{
		Start: win, End: win.Add(upstream.WindowSize), Status: upstream.WindowOK,
		LocalCount: 1, LocalTokens: 10, CheckedAt: win,
	})
	if err != upstream.ErrWindowNotReconcilable {
		t.Fatalf("覆盖 blocked 行应被拒绝，got %v", err)
	}
	// scroll_offset 不能被对账动过。
	pending, err := wins.PendingWindows(ctx, 5)
	if err != nil {
		t.Fatalf("读取待补拉失败: %v", err)
	}
	if len(pending) != 1 || pending[0].ScrollOffset != 4000 {
		t.Fatalf("blocked 行与断点应原样保留，got %+v", pending)
	}
}

// openMigratedTempDB 建临时库并跑**真实迁移链**（0001–0024），结束 dropdb。
// 未设置 LLMGATEWAY_TEST_PG_DSN 或账号无 CREATEDB 权限时跳过。
func openMigratedTempDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("LLMGATEWAY_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("未设置 LLMGATEWAY_TEST_PG_DSN，跳过真实库用例")
	}
	admin, err := db.Open(dsn)
	if err != nil {
		t.Fatalf("打开管理连接失败: %v", err)
	}
	t.Cleanup(func() { _ = admin.Close() })

	name := fmt.Sprintf("v2_reconcile_%d", time.Now().UnixNano())
	if _, err := admin.ExecContext(context.Background(), "CREATE DATABASE "+name); err != nil {
		t.Skipf("无法创建临时库（账号是否没有 CREATEDB 权限？）: %v", err)
	}
	t.Cleanup(func() {
		if a, derr := db.Open(dsn); derr == nil {
			_, _ = a.ExecContext(context.Background(), "DROP DATABASE IF EXISTS "+name+" WITH (FORCE)")
			_ = a.Close()
		}
	})

	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("解析 DSN 失败: %v", err)
	}
	u.Path = "/" + name
	target, err := db.Open(u.String())
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { _ = target.Close() })

	if _, err := db.Migrate(context.Background(), target); err != nil {
		t.Fatalf("在临时库应用迁移链（0001–0024）失败: %v", err)
	}
	return target
}
