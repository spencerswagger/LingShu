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
		"Calls", "WindowTokens", "TPM", "SpentCredits", "LastActive", "Spark",
	}
}

// liveUserKeys 用户行的完整字段白名单（只允许汇总吞吐与消耗，不含会话级字段）。
func liveUserKeys() []string {
	return []string{"ID", "Name", "Sessions", "Tokens", "TPM", "SpentCredits", "Spark"}
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

// TestParseLiveParams 断言实时区参数：缺省值 + 越界收敛 + 非数字报错。
func TestParseLiveParams(t *testing.T) {
	p, err := parseLiveParams(url.Values{})
	if err != nil {
		t.Fatalf("缺省解析失败: %v", err)
	}
	if p.WindowMinutes != 60 || p.SparkPoints != 12 || p.Limit != 20 || p.Window != 60*time.Minute {
		t.Fatalf("缺省值错: %+v", p)
	}

	q := url.Values{"Window": {"99999"}, "SparkPoints": {"999"}, "Limit": {"9999"}}
	p, err = parseLiveParams(q)
	if err != nil {
		t.Fatalf("越界值应被收敛而不是报错: %v", err)
	}
	if p.WindowMinutes != liveMaxWindowMinutes || p.SparkPoints != liveMaxSparkPoints || p.Limit != liveMaxLimit {
		t.Fatalf("上限收敛错: %+v", p)
	}

	q = url.Values{"Window": {"0"}, "SparkPoints": {"-5"}, "Limit": {"0"}}
	p, err = parseLiveParams(q)
	if err != nil {
		t.Fatalf("下界值应被收敛: %v", err)
	}
	if p.WindowMinutes != 1 || p.SparkPoints != 1 || p.Limit != liveDefaultLimit {
		t.Fatalf("下限收敛错: %+v", p)
	}

	if _, err := parseLiveParams(url.Values{"Window": {"abc"}}); err == nil {
		t.Fatalf("非数字 Window 应报错")
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
		{"显式 hour + 3 天报错", url.Values{"Granularity": {"hour"}}, 72 * time.Hour, "", true},
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
}

// TestBuildLiveStats_NoSessions 断言无在用会话时返回空数组（不是 null）且计数为 0。
func TestBuildLiveStats_NoSessions(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	reg := router.NewSessionRegistry(liveNow)

	out, err := buildLiveStats(context.Background(), db, reg, liveNow(), nil, "global",
		liveParams{WindowMinutes: 60, Window: 60 * time.Minute, SparkPoints: 12, Limit: 20})
	if err != nil {
		t.Fatalf("buildLiveStats: %v", err)
	}
	if out.Sessions == nil || out.Users == nil {
		t.Fatalf("空集合必须是 []，不能是 null")
	}
	if len(out.Sessions) != 0 || len(out.Users) != 0 || out.SessionsTotal != 0 || out.UsersTotal != 0 {
		t.Fatalf("无会话时计数应为 0: %+v", out)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("无会话时不应查询 DB: %v", err)
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
//  2. 聚合值（上下文、本轮 Token、TPM、花费、曲线）落到正确字段。
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
		LastActive:   liveNow(),
		ExpireAt:     liveNow().Add(time.Hour),
	}
	if ok, _ := reg.Attach(sess, 100); !ok {
		t.Fatalf("attach 失败")
	}
	reg.BeginRequest("sess-1")

	// 1) 会话聚合：累计 2 次、累计 5 积分；窗口内 600 token / 3.5 积分。
	mock.ExpectQuery(regexp.QuoteMeta(`GROUP BY session_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"session_id", "calls", "credits", "win_tokens"}).
			AddRow("sess-1", int64(2), float64(5), int64(600)))
	// 2) 最近请求：rn=2 更早（100 token / ctx 50），rn=1 最新（200 token / ctx 900）。
	mock.ExpectQuery(regexp.QuoteMeta(`row_number() OVER (PARTITION BY session_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"session_id", "rn", "tokens_total", "ctx_tokens"}).
			AddRow("sess-1", 2, int64(100), int64(50)).
			AddRow("sess-1", 1, int64(200), int64(900)))
	// 3) 用户窗口聚合
	mock.ExpectQuery(regexp.QuoteMeta(`GROUP BY user_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "win_tokens", "win_credits"}).
			AddRow(int64(7), int64(600), float64(3.5)))
	// 4) 用户逐分钟曲线（只给一分钟的数据，其余补 0）
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('minute'`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "minute", "tokens"}).
			AddRow(int64(7), liveNow().Truncate(time.Minute), int64(120)))
	// 5) 用户展示名
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, username, nickname FROM users`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username", "nickname"}).
			AddRow(int64(7), "dev", "开发者"))

	out, err := buildLiveStats(context.Background(), db, reg, liveNow(), nil, "global",
		liveParams{WindowMinutes: 60, Window: 60 * time.Minute, SparkPoints: 12, Limit: 20})
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
	if row.Calls != 2 || row.SpentCredits != 5 {
		t.Fatalf("累计值错: calls=%d spent=%v", row.Calls, row.SpentCredits)
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

	user := out.Users[0]
	if user.Name != "开发者" || user.Sessions != 1 || user.TPM != 10 || user.Tokens != 600 {
		t.Fatalf("在线用户汇总错: %+v", user)
	}
	if len(user.Spark) != 12 {
		t.Fatalf("用户曲线应补齐到 SparkPoints 长度，got %d", len(user.Spark))
	}
	last := user.Spark[len(user.Spark)-1]
	if last != 120 {
		t.Fatalf("最后一分钟（当前分钟）应含 120，got %v", user.Spark)
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
		WillReturnRows(sqlmock.NewRows([]string{"session_id", "calls", "credits", "win_tokens"}).
			AddRow("mine", int64(1), float64(1), int64(60)))
	mock.ExpectQuery(regexp.QuoteMeta(`row_number() OVER (PARTITION BY session_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"session_id", "rn", "tokens_total", "ctx_tokens"}).
			AddRow("mine", 1, int64(60), int64(30)))
	mock.ExpectQuery(regexp.QuoteMeta(`GROUP BY user_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "win_tokens", "win_credits"}).
			AddRow(int64(7), int64(60), float64(1)))
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('minute'`)).
		WillReturnRows(sqlmock.NewRows([]string{"user_id", "minute", "tokens"}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, username, nickname FROM users`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "username", "nickname"}).
			AddRow(int64(7), "dev", ""))

	uid := int64(7)
	out, err := buildLiveStats(context.Background(), db, reg, liveNow(), &uid, "self",
		liveParams{WindowMinutes: 60, Window: 60 * time.Minute, SparkPoints: 12, Limit: 20})
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

	dev := NewDev(nil, func(context.Context) (int64, bool) { return 7, true })
	dev.now = liveNow
	dev.SetDB(db)
	dev.SetSessions(router.NewSessionRegistry(liveNow))
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
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未满足的期望: %v", err)
	}
}
