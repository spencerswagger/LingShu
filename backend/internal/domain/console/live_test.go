package console

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/router"
)

// liveNow 实时区测试统一时钟：北京时间 2026-10-25 15:30。
func liveNow() time.Time { return time.Date(2026, 10, 25, 15, 30, 0, 0, cnLocation()) }

// liveSessionKeys 会话行的**完整**字段白名单。
// 任何新增字段都必须在这里显式登记，且不得承载会话内容（消息/提示词/模型输入输出/原始 session id）。
func liveSessionKeys() []string {
	return []string{
		"ID", "Name", "Model", "Inflight", "ContextUsed", "ContextLimit", "RoundTokens",
		"Calls", "TotalTokens", "WindowTokens", "TPM", "SpentCredits", "CreatedAt", "LastActive", "Spark",
	}
}

// liveUserKeys 用户行的完整字段白名单（只允许汇总吞吐与消耗，不含会话级字段）。
func liveUserKeys() []string {
	return []string{
		"ID", "Name", "Sessions", "ActiveSessions", "Calls", "Tokens", "TPM", "SpentCredits",
		"RangeTokens", "RangeCredits", "Spark", "Daily",
	}
}

// forbiddenLiveFields 实时区响应里绝不允许出现的字段名（会话内容面）。
func forbiddenLiveFields() []string {
	return []string{"SessionRaw", "Prompt", "Prompts", "Message", "Messages", "Input", "Output",
		"RequestBody", "ResponseBody", "Content", "Completion"}
}

func sortedKeys(m map[string]json.RawMessage) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func assertKeys(t *testing.T, label string, obj map[string]json.RawMessage, want []string) {
	t.Helper()
	got := sortedKeys(obj)
	sorted := append([]string{}, want...)
	sort.Strings(sorted)
	if strings.Join(got, ",") != strings.Join(sorted, ",") {
		t.Fatalf("%s 字段集不符：got %v want %v", label, got, sorted)
	}
}

// TestParseLiveParams 断言实时区参数：缺省值 + 越界收敛 + 非数字报错 + 范围参数。
func TestParseLiveParams(t *testing.T) {
	p, err := parseLiveParams(url.Values{}, liveNow())
	if err != nil {
		t.Fatalf("缺省解析失败: %v", err)
	}
	if p.WindowMinutes != 60 || p.SparkPoints != 12 || p.Limit != 20 || p.Window != 60*time.Minute {
		t.Fatalf("缺省值错: %+v", p)
	}
	// 在线用户范围默认近 24h（对齐页面默认预设）。
	if !p.To.Equal(liveNow()) || !p.From.Equal(liveNow().Add(-24*time.Hour)) {
		t.Fatalf("缺省范围应为近 24h: from=%v to=%v", p.From, p.To)
	}

	q := url.Values{"Window": {"99999"}, "SparkPoints": {"999"}, "Limit": {"9999"}}
	p, err = parseLiveParams(q, liveNow())
	if err != nil {
		t.Fatalf("越界值应被收敛而不是报错: %v", err)
	}
	if p.WindowMinutes != liveMaxWindowMinutes || p.SparkPoints != liveMaxSparkPoints || p.Limit != liveMaxLimit {
		t.Fatalf("上限收敛错: %+v", p)
	}

	q = url.Values{"Window": {"0"}, "SparkPoints": {"-5"}, "Limit": {"0"}}
	p, err = parseLiveParams(q, liveNow())
	if err != nil {
		t.Fatalf("下界值应被收敛: %v", err)
	}
	if p.WindowMinutes != 1 || p.SparkPoints != 1 || p.Limit != liveDefaultLimit {
		t.Fatalf("下限收敛错: %+v", p)
	}

	if _, err := parseLiveParams(url.Values{"Window": {"abc"}}, liveNow()); err == nil {
		t.Fatalf("非数字 Window 应报错")
	}

	// From/To：显式生效（含未编码 '+' 的宽松解析）、倒序报错、超 366 天报错、格式错报错。
	p, err = parseLiveParams(url.Values{
		"From": {"2026-09-25T00:00:00 08:00"}, "To": {"2026-10-25T00:00:00+08:00"},
	}, liveNow())
	if err != nil {
		t.Fatalf("范围解析失败: %v", err)
	}
	if p.To.Sub(p.From) != 30*24*time.Hour {
		t.Fatalf("范围应生效 30 天，got %v", p.To.Sub(p.From))
	}
	if _, err := parseLiveParams(url.Values{
		"From": {"2026-10-26T00:00:00+08:00"}, "To": {"2026-10-25T00:00:00+08:00"},
	}, liveNow()); err == nil {
		t.Fatalf("From 晚于 To 应报错")
	}
	if _, err := parseLiveParams(url.Values{
		"From": {"2024-01-01T00:00:00+08:00"}, "To": {"2026-10-25T00:00:00+08:00"},
	}, liveNow()); err == nil {
		t.Fatalf("超过 366 天应报错")
	}
	if _, err := parseLiveParams(url.Values{"From": {"2026/10/25"}}, liveNow()); err == nil {
		t.Fatalf("非法 From 格式应报错")
	}
}

// TestParseStatsOptions_Granularity 断言粒度判定：
// 近 24h 自动按小时、大范围自动按天、显式 hour 超 2 天报错、非法值报错。
func TestParseStatsOptions_Granularity(t *testing.T) {
	cases := []struct {
		name  string
		q     url.Values
		span  time.Duration
		want  string
		fails bool
	}{
		{"24h 自动按小时", url.Values{}, 24 * time.Hour, statsGranularityHour, false},
		{"26h 边界按小时", url.Values{}, 26 * time.Hour, statsGranularityHour, false},
		{"30 天按天", url.Values{}, 30 * 24 * time.Hour, statsGranularityDay, false},
		{"显式 auto + 7 天按天", url.Values{"Granularity": {"auto"}}, 7 * 24 * time.Hour, statsGranularityDay, false},
		{"显式 hour + 2 天", url.Values{"Granularity": {"hour"}}, 48 * time.Hour, statsGranularityHour, false},
		{"显式 hour + 3 天自动降级为天", url.Values{"Granularity": {"hour"}}, 72 * time.Hour, statsGranularityDay, false},
		{"显式 day + 24h", url.Values{"Granularity": {"day"}}, 24 * time.Hour, statsGranularityDay, false},
		{"非法粒度", url.Values{"Granularity": {"week"}}, 24 * time.Hour, "", true},
	}
	for _, c := range cases {
		opts, err := parseStatsOptions(c.q, c.span)
		if c.fails {
			if err == nil {
				t.Fatalf("%s：应报错", c.name)
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s：%v", c.name, err)
		}
		if opts.Granularity != c.want {
			t.Fatalf("%s：granularity=%q want %q", c.name, opts.Granularity, c.want)
		}
	}
}

// TestParseStatsOptions_ClampAndDefaults 断言 CompareDays/ModelLimit/ModelSort 的收敛与缺省。
func TestParseStatsOptions_ClampAndDefaults(t *testing.T) {
	opts, err := parseStatsOptions(url.Values{}, time.Hour)
	if err != nil {
		t.Fatalf("缺省解析失败: %v", err)
	}
	if opts.CompareDays != 0 || opts.ModelSort != "tokens" || opts.ModelLimit != statsDefaultModelLimit {
		t.Fatalf("缺省值错: %+v", opts)
	}

	opts, err = parseStatsOptions(url.Values{
		"CompareDays": {"99"}, "ModelLimit": {"10000"}, "ModelSort": {"tpm"},
	}, time.Hour)
	if err != nil {
		t.Fatalf("越界值应被收敛: %v", err)
	}
	if opts.CompareDays != 7 || opts.ModelLimit != 200 || opts.ModelSort != "tpm" {
		t.Fatalf("上限收敛错: %+v", opts)
	}

	opts, err = parseStatsOptions(url.Values{"CompareDays": {"-3"}, "ModelLimit": {"-1"}}, time.Hour)
	if err != nil {
		t.Fatalf("下界值应被收敛: %v", err)
	}
	if opts.CompareDays != 0 || opts.ModelLimit != statsDefaultModelLimit {
		t.Fatalf("下限收敛错: %+v", opts)
	}

	if _, err := parseStatsOptions(url.Values{"CompareDays": {"x"}}, time.Hour); err == nil {
		t.Fatalf("非数字 CompareDays 应报错")
	}

	// Window：缺省 exact（与前端标签一致），calendar 显式生效，非法值报错。
	if opts, err := parseStatsOptions(url.Values{}, time.Hour); err != nil || opts.Window != statsWindowExact {
		t.Fatalf("缺省应 exact: %+v %v", opts, err)
	}
	if opts, err := parseStatsOptions(url.Values{"Window": {"calendar"}}, time.Hour); err != nil || opts.Window != statsWindowCalendar {
		t.Fatalf("calendar 应生效: %+v %v", opts, err)
	}
	if _, err := parseStatsOptions(url.Values{"Window": {"rolling"}}, time.Hour); err == nil {
		t.Fatalf("非法 Window 应报错")
	}
}

// TestBuildLiveStats_NoSessions 断言无活跃会话时返回空数组（不是 null）且会话计数为 0。
// 注意：在线用户改为查 DB 后，即使没有会话也会查一次用户聚合（结果为空则不再查曲线/序列）。
func TestBuildLiveStats_NoSessions(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	reg := router.NewSessionRegistry(liveNow)

	mock.ExpectQuery(regexp.QuoteMeta(`count(DISTINCT session_id)`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "calls", "active_sessions",
			"win_tokens", "win_credits", "range_tokens", "range_credits"}))

	p := liveParams{WindowMinutes: 60, Window: 60 * time.Minute, SparkPoints: 12, Limit: 20,
		From: liveNow().Add(-24 * time.Hour), To: liveNow()}
	out, err := buildLiveStats(context.Background(), db, reg, liveNow(), nil, "global", p)
	if err != nil {
		t.Fatalf("buildLiveStats: %v", err)
	}
	if out.Sessions == nil || out.Users == nil {
		t.Fatalf("空集合必须是 []，不能是 null")
	}
	if len(out.Sessions) != 0 || len(out.Users) != 0 || out.SessionsTotal != 0 || out.UsersTotal != 0 {
		t.Fatalf("无会话时计数应为 0: %+v", out)
	}
	// 日活跃序列覆盖范围要回显（默认 24h = 2 个自然日：今天 + 昨天）。
	if out.DailyPoints != 2 || out.DailyFrom == "" || out.DailyTo == "" {
		t.Fatalf("日活跃序列覆盖回显错: %+v", out)
	}
	if out.RangeFrom == "" || out.RangeTo == "" || out.RangeTo != liveNow().Format(time.RFC3339) {
		t.Fatalf("范围回显错: %+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未满足的期望: %v", err)
	}
}

// TestBuildLiveStats_NoRegistry 断言未装配注册表时降级为空结果（不 panic）。
func TestBuildLiveStats_NoRegistry(t *testing.T) {
	out, err := buildLiveStats(context.Background(), nil, nil, liveNow(), nil, "global",
		liveParams{WindowMinutes: 60, Window: 60 * time.Minute, SparkPoints: 12, Limit: 20})
	if err != nil {
		t.Fatalf("buildLiveStats: %v", err)
	}
	if out.Sessions == nil || len(out.Sessions) != 0 {
		t.Fatalf("降级结果应为空切片: %+v", out.Sessions)
	}
}

// TestBuildLiveStats_SanitizedAndAggregates 断言：
//  1. 会话/用户行的字段集与白名单**完全一致**（字段层面杜绝内容泄露）；
//  2. 聚合值（上下文、本轮 Token、TPM、花费、曲线、日活跃序列）落到正确字段。
func TestBuildLiveStats_SanitizedAndAggregates(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	reg := router.NewSessionRegistry(liveNow)

	sess := &router.Session{
		SessionID:    "sess-1",
		UserID:       7,
		Model:        "gpt-4o",
		Name:         "写周报",
		SessionRaw:   "raw-should-not-leak",
		ChannelKeyID: 100,
		CreatedAt:    liveNow().Add(-2 * time.Hour),
		LastActive:   liveNow(),
		ExpireAt:     liveNow().Add(time.Hour),
	}
	if ok, _ := reg.Attach(sess, 100); !ok {
		t.Fatalf("attach 失败")
	}
	reg.BeginRequest("sess-1")

	// 1) 会话聚合：累计 2 次、累计 5 积分、累计 800 token；窗口内 600 token / 3.5 积分。
	mock.ExpectQuery(regexp.QuoteMeta(`GROUP BY session_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"session_id", "calls", "credits", "total_tokens", "win_tokens"}).
			AddRow("sess-1", int64(2), float64(5), int64(800), int64(600)))
	// 2) 最近请求：rn=2 更早（100 token / ctx 50），rn=1 最新（200 token / ctx 900）。
	mock.ExpectQuery(regexp.QuoteMeta(`row_number() OVER (PARTITION BY session_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"session_id", "rn", "tokens_total", "ctx_tokens"}).
			AddRow("sess-1", 2, int64(100), int64(50)).
			AddRow("sess-1", 1, int64(200), int64(900)))
	// 3) 用户范围聚合（在线用户全集的唯一来源）
	mock.ExpectQuery(regexp.QuoteMeta(`count(DISTINCT session_id)`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "calls", "active_sessions",
			"win_tokens", "win_credits", "range_tokens", "range_credits"}).
			AddRow(int64(7), int64(9), int64(3), int64(600), float64(3.5), int64(7000), float64(42)))
	// 4) 用户展示名
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, username, nickname FROM users`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username", "nickname"}).
			AddRow(int64(7), "dev", "开发者"))
	// 5) 用户逐分钟曲线（只给一分钟的数据，其余补 0）
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('minute'`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "minute", "tokens"}).
			AddRow(int64(7), liveNow().Truncate(time.Minute), int64(120)))
	// 6) 用户日活跃序列（只给最后一天的数据，其余补 0）
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('day'`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "day", "tokens"}).
			AddRow(int64(7), liveNow(), int64(7000)))

	p := liveParams{WindowMinutes: 60, Window: 60 * time.Minute, SparkPoints: 12, Limit: 20,
		From: liveNow().Add(-7 * 24 * time.Hour), To: liveNow()}
	out, err := buildLiveStats(context.Background(), db, reg, liveNow(), nil, "global", p)
	if err != nil {
		t.Fatalf("buildLiveStats: %v", err)
	}
	if len(out.Sessions) != 1 || len(out.Users) != 1 {
		t.Fatalf("行数错: sessions=%d users=%d", len(out.Sessions), len(out.Users))
	}
	row := out.Sessions[0]
	if row.ContextUsed != 900 || row.RoundTokens != 200 {
		t.Fatalf("上下文/本轮 Token 错: ctx=%d round=%d", row.ContextUsed, row.RoundTokens)
	}
	if row.Calls != 2 || row.SpentCredits != 5 || row.TotalTokens != 800 {
		t.Fatalf("累计值错: calls=%d spent=%v totalTokens=%d", row.Calls, row.SpentCredits, row.TotalTokens)
	}
	if row.WindowTokens != 600 || row.TPM != 10 { // 600/60min
		t.Fatalf("窗口吞吐错: tokens=%d tpm=%v", row.WindowTokens, row.TPM)
	}
	if len(row.Spark) != 2 || row.Spark[0] != 100 || row.Spark[1] != 200 {
		t.Fatalf("曲线应按时间正序: %v", row.Spark)
	}
	if row.ContextLimit != 0 {
		t.Fatalf("未配置上下文上限时应为 0，got %d", row.ContextLimit)
	}
	if row.CreatedAt == "" {
		t.Fatalf("会话创建时间应回填")
	}

	user := out.Users[0]
	if user.Name != "开发者" || user.Sessions != 1 || user.TPM != 10 || user.Tokens != 600 {
		t.Fatalf("在线用户汇总错: %+v", user)
	}
	if user.Calls != 9 || user.ActiveSessions != 3 || user.RangeTokens != 7000 || user.RangeCredits != 42 {
		t.Fatalf("在线用户范围聚合错: %+v", user)
	}
	if len(user.Spark) != 12 {
		t.Fatalf("用户曲线应补齐到 SparkPoints 长度，got %d", len(user.Spark))
	}
	last := user.Spark[len(user.Spark)-1]
	if last != 120 {
		t.Fatalf("最后一分钟（当前分钟）应含 120，got %v", user.Spark)
	}
	// 近 7 天 → 8 个自然日（含今天与起点当天），最后一天是今天。
	if len(user.Daily) != out.DailyPoints || out.DailyPoints != 8 {
		t.Fatalf("日活跃序列长度错: len=%d dailyPoints=%d", len(user.Daily), out.DailyPoints)
	}
	if user.Daily[len(user.Daily)-1] != 7000 || user.Daily[0] != 0 {
		t.Fatalf("日活跃序列补零/归位错: %v", user.Daily)
	}

	// 字段层面脱敏：JSON 里不允许出现任何内容面字段。
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded struct {
		Sessions []map[string]json.RawMessage `json:"Sessions"`
		Users    []map[string]json.RawMessage `json:"Users"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	assertKeys(t, "会话行", decoded.Sessions[0], liveSessionKeys())
	assertKeys(t, "用户行", decoded.Users[0], liveUserKeys())
	for _, f := range forbiddenLiveFields() {
		if strings.Contains(string(raw), `"`+f+`"`) {
			t.Fatalf("响应里出现了内容面字段 %q: %s", f, string(raw))
		}
	}
	if strings.Contains(string(raw), "raw-should-not-leak") {
		t.Fatalf("响应泄露了客户端原始 x-session-id")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未满足的期望: %v", err)
	}
}

// TestBuildLiveStats_SessionRecencyWindow 断言「我的会话」入选条件 = 请求中 ∥ 最近 5 分钟内有请求：
// 空闲但 4 分钟前发过请求的要保留，6 分钟前的要剔除，已关闭的剔除。
func TestBuildLiveStats_SessionRecencyWindow(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	cur := liveNow().Add(-6 * time.Minute)
	reg := router.NewSessionRegistry(func() time.Time { return cur })

	// 注册表会把 LastActive 写成「当下」，所以用可调时钟把三条会话分别放在 6 分钟前 / 4 分钟前 / 现在。
	attach := func(id string, userID int64) {
		if ok, _ := reg.Attach(&router.Session{
			SessionID: id, UserID: userID, Model: "gpt-4o",
			ExpireAt: liveNow().Add(time.Hour),
		}, 100); !ok {
			t.Fatalf("attach %s 失败", id)
		}
	}
	attach("idle-6min", 8) // 6 分钟前有过请求 → 应剔除
	cur = liveNow().Add(-4 * time.Minute)
	attach("idle-4min", 7) // 4 分钟前有过请求 → 保留
	cur = liveNow()
	attach("inflight", 7) // 正在请求 → 保留
	attach("closed", 8)   // 已关闭 → 剔除
	reg.BeginRequest("inflight")
	reg.Close("closed")

	mock.ExpectQuery(regexp.QuoteMeta(`GROUP BY session_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"session_id", "calls", "credits", "total_tokens", "win_tokens"}).
			AddRow("inflight", int64(1), float64(1), int64(100), int64(100)).
			AddRow("idle-4min", int64(1), float64(2), int64(200), int64(200)))
	mock.ExpectQuery(regexp.QuoteMeta(`row_number() OVER (PARTITION BY session_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"session_id", "rn", "tokens_total", "ctx_tokens"}).
			AddRow("inflight", 1, int64(100), int64(80)).
			AddRow("idle-4min", 1, int64(200), int64(160)))
	mock.ExpectQuery(regexp.QuoteMeta(`count(DISTINCT session_id)`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "calls", "active_sessions",
			"win_tokens", "win_credits", "range_tokens", "range_credits"}).
			AddRow(int64(7), int64(2), int64(2), int64(300), float64(3), int64(300), float64(3)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, username, nickname FROM users`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username", "nickname"}).AddRow(int64(7), "dev", ""))
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('minute'`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "minute", "tokens"}))
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('day'`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "day", "tokens"}))

	p := liveParams{WindowMinutes: 60, Window: 60 * time.Minute, SparkPoints: 12, Limit: 20,
		From: liveNow().Add(-24 * time.Hour), To: liveNow()}
	out, err := buildLiveStats(context.Background(), db, reg, liveNow(), nil, "global", p)
	if err != nil {
		t.Fatalf("buildLiveStats: %v", err)
	}
	if out.SessionsTotal != 2 || len(out.Sessions) != 2 {
		t.Fatalf("会话入选口径错: total=%d rows=%d", out.SessionsTotal, len(out.Sessions))
	}
	got := map[string]int{}
	for _, s := range out.Sessions {
		got[s.ID] = s.Inflight
	}
	if _, ok := got["inflight"]; !ok {
		t.Fatalf("请求中的会话应入选: %v", got)
	}
	if inflight, ok := got["idle-4min"]; !ok || inflight != 0 {
		t.Fatalf("5 分钟内活跃的空闲会话应入选且 Inflight=0: %v", got)
	}
	if _, ok := got["idle-6min"]; ok {
		t.Fatalf("超过 5 分钟的空闲会话不应入选: %v", got)
	}
	if _, ok := got["closed"]; ok {
		t.Fatalf("已关闭会话不应入选: %v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未满足的期望: %v", err)
	}
}

// TestBuildLiveStats_UsersFromDBRange 断言「在线用户」以 DB 范围为准：
// 会话为空也照样返回范围内有请求的用户，UsersTotal == 列表行数（同源），且补齐曲线与日活跃序列。
func TestBuildLiveStats_UsersFromDBRange(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	reg := router.NewSessionRegistry(liveNow) // 没有任何在用会话

	mock.ExpectQuery(regexp.QuoteMeta(`count(DISTINCT session_id)`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "calls", "active_sessions",
			"win_tokens", "win_credits", "range_tokens", "range_credits"}).
			AddRow(int64(7), int64(5), int64(2), int64(0), float64(0), int64(5000), float64(9)).
			AddRow(int64(9), int64(3), int64(1), int64(120), float64(1.5), int64(900), float64(2)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, username, nickname FROM users`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username", "nickname"}).
			AddRow(int64(7), "a", "阿七").
			AddRow(int64(9), "b", ""))
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('minute'`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "minute", "tokens"}).
			AddRow(int64(9), liveNow().Truncate(time.Minute), int64(120)))
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('day'`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "day", "tokens"}).
			AddRow(int64(7), liveNow(), int64(5000)))

	p := liveParams{WindowMinutes: 60, Window: 60 * time.Minute, SparkPoints: 12, Limit: 20,
		From: liveNow().Add(-30 * 24 * time.Hour), To: liveNow()}
	out, err := buildLiveStats(context.Background(), db, reg, liveNow(), nil, "global", p)
	if err != nil {
		t.Fatalf("buildLiveStats: %v", err)
	}
	if out.SessionsTotal != 0 || len(out.Sessions) != 0 {
		t.Fatalf("没有在用会话时应为空: %+v", out.Sessions)
	}
	if out.UsersTotal != 2 || len(out.Users) != 2 {
		t.Fatalf("在线用户应来自 DB 范围（总数必须等于列表行数）: total=%d rows=%d", out.UsersTotal, len(out.Users))
	}
	// 排序：范围 Token 高的在前（用户 7）。
	if out.Users[0].ID != "7" || out.Users[0].RangeTokens != 5000 || out.Users[0].Name != "阿七" {
		t.Fatalf("用户行错: %+v", out.Users[0])
	}
	if out.Users[0].Sessions != 0 {
		t.Fatalf("没有在用会话时 Sessions 应为 0，got %d", out.Users[0].Sessions)
	}
	if len(out.Users[0].Daily) != out.DailyPoints || out.DailyPoints != 31 {
		t.Fatalf("日活跃序列长度应覆盖 30 天范围: len=%d points=%d", len(out.Users[0].Daily), out.DailyPoints)
	}
	if out.Users[0].Daily[len(out.Users[0].Daily)-1] != 5000 {
		t.Fatalf("日活跃序列归位错: %v", out.Users[0].Daily)
	}
	if out.Users[1].Name != "b" || out.Users[1].TPM != 2 { // 120/60min
		t.Fatalf("昵称为空应回落到登录名，并算 TPM: %+v", out.Users[1])
	}
	if len(out.Users[1].Spark) != 12 || out.Users[1].Spark[len(out.Users[1].Spark)-1] != 120 {
		t.Fatalf("用户曲线错: %v", out.Users[1].Spark)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未满足的期望: %v", err)
	}
}

// TestBuildLiveStats_UsersTruncated 断言用户超上限时：UsersTotal 仍是全集行数（同源未截断），
// 列表截到上限并打 UsersTruncated。
func TestBuildLiveStats_UsersTruncated(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	reg := router.NewSessionRegistry(liveNow)

	rows := sqlmock.NewRows([]string{"user_id", "calls", "active_sessions",
		"win_tokens", "win_credits", "range_tokens", "range_credits"})
	for i := 1; i <= liveMaxUsers+1; i++ {
		rows.AddRow(int64(i), int64(1), int64(1), int64(10), float64(0.1), int64(10), float64(0.1))
	}
	mock.ExpectQuery(regexp.QuoteMeta(`count(DISTINCT session_id)`)).WillReturnRows(rows)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, username, nickname FROM users`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username", "nickname"}))
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('minute'`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "minute", "tokens"}))
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('day'`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "day", "tokens"}))

	p := liveParams{WindowMinutes: 60, Window: 60 * time.Minute, SparkPoints: 12, Limit: 20,
		From: liveNow().Add(-24 * time.Hour), To: liveNow()}
	out, err := buildLiveStats(context.Background(), db, reg, liveNow(), nil, "global", p)
	if err != nil {
		t.Fatalf("buildLiveStats: %v", err)
	}
	if out.UsersTotal != liveMaxUsers+1 || len(out.Users) != liveMaxUsers || !out.UsersTruncated {
		t.Fatalf("上限截断错: total=%d rows=%d truncated=%v", out.UsersTotal, len(out.Users), out.UsersTruncated)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未满足的期望: %v", err)
	}
}

// TestBuildLiveStats_DevScopeIsolation 断言开发端只看得到自己的会话与自己的用户汇总。
func TestBuildLiveStats_DevScopeIsolation(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	reg := router.NewSessionRegistry(liveNow)

	for _, s := range []struct {
		id     string
		userID int64
	}{{"mine", 7}, {"other", 8}} {
		if ok, _ := reg.Attach(&router.Session{
			SessionID: s.id, UserID: s.userID, Model: "gpt-4o",
			LastActive: liveNow(), ExpireAt: liveNow().Add(time.Hour),
		}, 100); !ok {
			t.Fatalf("attach %s 失败", s.id)
		}
		reg.BeginRequest(s.id)
	}

	mock.ExpectQuery(regexp.QuoteMeta(`GROUP BY session_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"session_id", "calls", "credits", "total_tokens", "win_tokens"}).
			AddRow("mine", int64(1), float64(1), int64(60), int64(60)))
	mock.ExpectQuery(regexp.QuoteMeta(`row_number() OVER (PARTITION BY session_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"session_id", "rn", "tokens_total", "ctx_tokens"}).
			AddRow("mine", 1, int64(60), int64(30)))
	mock.ExpectQuery(regexp.QuoteMeta(`count(DISTINCT session_id)`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "calls", "active_sessions",
			"win_tokens", "win_credits", "range_tokens", "range_credits"}).
			AddRow(int64(7), int64(1), int64(1), int64(60), float64(1), int64(60), float64(1)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, username, nickname FROM users`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username", "nickname"}).
			AddRow(int64(7), "dev", ""))
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('minute'`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "minute", "tokens"}))
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('day'`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "day", "tokens"}))

	uid := int64(7)
	p := liveParams{WindowMinutes: 60, Window: 60 * time.Minute, SparkPoints: 12, Limit: 20,
		From: liveNow().Add(-24 * time.Hour), To: liveNow()}
	out, err := buildLiveStats(context.Background(), db, reg, liveNow(), &uid, "self", p)
	if err != nil {
		t.Fatalf("buildLiveStats: %v", err)
	}
	if out.Scope != "self" || out.SessionsTotal != 1 || len(out.Sessions) != 1 {
		t.Fatalf("越权过滤失效: %+v", out)
	}
	if out.Sessions[0].ID != "mine" {
		t.Fatalf("返回了他人会话: %s", out.Sessions[0].ID)
	}
	if out.UsersTotal != 1 || out.Users[0].ID != "7" {
		t.Fatalf("用户汇总未按本人过滤: %+v", out.Users)
	}
	if out.Users[0].Name != "dev" {
		t.Fatalf("昵称为空时应回落到登录名，got %q", out.Users[0].Name)
	}
}

// TestHandleStatsLive_HTTP 断言两个端点都按 {Code,Data} 结构返回，且 admin/dev 的 scope 正确。
func TestHandleStatsLive_HTTP(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	admin := NewAdmin(db, nil, nil, nil)
	admin.now = liveNow
	admin.SetSessions(router.NewSessionRegistry(liveNow))

	emptyUsers := func() *sqlmock.Rows {
		return sqlmock.NewRows([]string{"user_id", "calls", "active_sessions",
			"win_tokens", "win_credits", "range_tokens", "range_credits"})
	}
	mock.ExpectQuery(regexp.QuoteMeta(`count(DISTINCT session_id)`)).WillReturnRows(emptyUsers())

	rec := httptest.NewRecorder()
	admin.HandleStatsLive(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/stats/live?Window=30", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("admin 状态码=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code int      `json:"Code"`
		Data liveResp `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Code != 0 || body.Data.Scope != "global" || body.Data.WindowMinutes != 30 {
		t.Fatalf("admin 响应错: %+v", body)
	}
	// From/To 缺省 = 近 24h，且要回显（前端据此打标签）。
	if body.Data.RangeTo != liveNow().Format(time.RFC3339) {
		t.Fatalf("admin 范围回显错: %+v", body.Data)
	}

	dev := NewDev(nil, func(context.Context) (int64, bool) { return 7, true })
	dev.now = liveNow
	dev.SetDB(db)
	dev.SetSessions(router.NewSessionRegistry(liveNow))
	mock.ExpectQuery(regexp.QuoteMeta(`count(DISTINCT session_id)`)).WillReturnRows(emptyUsers())
	rec = httptest.NewRecorder()
	dev.HandleStatsLive(rec, httptest.NewRequest(http.MethodGet, "/api/v1/dev/stats/live", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("dev 状态码=%d body=%s", rec.Code, rec.Body.String())
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if body.Data.Scope != "self" || body.Data.WindowMinutes != 60 {
		t.Fatalf("dev 响应错: %+v", body.Data)
	}

	// 非法参数 → 400
	rec = httptest.NewRecorder()
	admin.HandleStatsLive(rec, httptest.NewRequest(http.MethodGet, "/api/v1/admin/stats/live?Window=abc", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("非法 Window 应 400，got %d", rec.Code)
	}
	// 倒序范围 → 400
	rec = httptest.NewRecorder()
	admin.HandleStatsLive(rec, httptest.NewRequest(http.MethodGet,
		"/api/v1/admin/stats/live?From=2026-10-26T00:00:00%2B08:00&To=2026-10-25T00:00:00%2B08:00", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("倒序范围应 400，got %d", rec.Code)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未满足的期望: %v", err)
	}
}
