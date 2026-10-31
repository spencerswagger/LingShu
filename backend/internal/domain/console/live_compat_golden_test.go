package console

// live_compat_golden_test.go 向后兼容的**硬证据**（对齐 PR #65 的做法）：
// 「实时区」的**旧字段**必须与 `origin/main`（#66 合入前）逐字节一致。
//
// 口径背景：spencer 拍板「旧字段语义不变，新口径单列」——
//   - `UsersTotal` / `UsersTruncated` / `Users[]` / `SessionsTotal` / `Truncated` 属旧字段；
//   - 范围内活跃用户改由新字段 `RangeUsersTotal` / `RangeUsers[]` 承载。
//
// golden 文件是在**改动前**（`6392a8c`，即 #66 合入前）的代码上跑同一组 fixture 直接落盘的，
// 不是手写期望值。生成方式：`LIVE_GOLDEN=write go test ./internal/domain/console -run TestLiveCompatGolden`。
//
// 比对粒度说明：`Sessions[]` 的**元素**在 #66 起多了 `TotalTokens`/`CreatedAt`（纯新增、spencer 已接受），
// 因此 `Sessions` 不整体比字节，而是投影到 #66 之前的字段集再逐字节比——新增字段不影响结论，
// 旧字段一旦改值/改名/改序就会失败。

import (
	"database/sql/driver"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/router"
)

// liveSessionItemMain 与 `origin/main`（#66 合入前）的 `liveSessionItem` 字段集与**声明顺序**完全一致。
// 用它把两个版本的响应投影到同一形状，再逐字节比对。
type liveSessionItemMain struct {
	ID           string  `json:"ID"`
	Name         string  `json:"Name"`
	Model        string  `json:"Model"`
	Inflight     int     `json:"Inflight"`
	ContextUsed  int64   `json:"ContextUsed"`
	ContextLimit int64   `json:"ContextLimit"`
	RoundTokens  int64   `json:"RoundTokens"`
	Calls        int64   `json:"Calls"`
	WindowTokens int64   `json:"WindowTokens"`
	TPM          float64 `json:"TPM"`
	SpentCredits float64 `json:"SpentCredits"`
	LastActive   string  `json:"LastActive"`
	Spark        []int64 `json:"Spark"`
}

// liveCompatProjection 旧字段的投影：值保持原始 JSON 字节，顺序固定。
// Sessions 用投影后的结构重新序列化（消除纯新增字段的影响）。
type liveCompatProjection struct {
	Scope          json.RawMessage       `json:"Scope,omitempty"`
	SampledAt      json.RawMessage       `json:"SampledAt,omitempty"`
	WindowMinutes  json.RawMessage       `json:"WindowMinutes,omitempty"`
	SparkPoints    json.RawMessage       `json:"SparkPoints,omitempty"`
	SessionsTotal  json.RawMessage       `json:"SessionsTotal,omitempty"`
	UsersTotal     json.RawMessage       `json:"UsersTotal,omitempty"`
	Truncated      json.RawMessage       `json:"Truncated,omitempty"`
	UsersTruncated json.RawMessage       `json:"UsersTruncated,omitempty"`
	Sessions       []liveSessionItemMain `json:"Sessions,omitempty"`
	Users          json.RawMessage       `json:"Users,omitempty"`
}

// projectLiveCompat 从响应体提取旧字段投影（新字段一律不参与）。
func projectLiveCompat(t *testing.T, body []byte) liveCompatProjection {
	t.Helper()
	var envelope struct {
		Data json.RawMessage `json:"Data"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("解析信封失败: %v", err)
	}
	var data struct {
		Scope          json.RawMessage `json:"Scope"`
		SampledAt      json.RawMessage `json:"SampledAt"`
		WindowMinutes  json.RawMessage `json:"WindowMinutes"`
		SparkPoints    json.RawMessage `json:"SparkPoints"`
		SessionsTotal  json.RawMessage `json:"SessionsTotal"`
		UsersTotal     json.RawMessage `json:"UsersTotal"`
		Truncated      json.RawMessage `json:"Truncated"`
		UsersTruncated json.RawMessage `json:"UsersTruncated"`
		Sessions       json.RawMessage `json:"Sessions"`
		Users          json.RawMessage `json:"Users"`
	}
	if err := json.Unmarshal(envelope.Data, &data); err != nil {
		t.Fatalf("解析 Data 失败: %v", err)
	}
	var sessions []liveSessionItemMain
	if len(data.Sessions) > 0 {
		if err := json.Unmarshal(data.Sessions, &sessions); err != nil {
			t.Fatalf("解析 Sessions 失败: %v", err)
		}
	}
	if sessions == nil {
		sessions = []liveSessionItemMain{}
	}
	return liveCompatProjection{
		Scope: data.Scope, SampledAt: data.SampledAt,
		WindowMinutes: data.WindowMinutes, SparkPoints: data.SparkPoints,
		SessionsTotal: data.SessionsTotal, UsersTotal: data.UsersTotal,
		Truncated: data.Truncated, UsersTruncated: data.UsersTruncated,
		Sessions: sessions, Users: data.Users,
	}
}

// liveCompatFixture 三组场景共用一套可复现的时间与注册表。
type liveCompatFixture struct {
	name     string
	sessions []*router.Session
	inflight []string
	// SQL 返回行
	// sessionAggRowsLegacy 是 #66 之前的会话聚合列形状（4 列）；sessionAggRows 是当前形状（5 列，
	// 多一个 total_tokens 供新增字段使用）。两者在**旧字段**上给同一组值，保证 golden 可跨版本比对。
	sessionAggRowsLegacy [][]driver.Value
	sessionAggRows       [][]driver.Value
	recentRows           [][]driver.Value
	userAggRows          [][]driver.Value
	sparkRows            [][]driver.Value
	userNameRows         [][]driver.Value
	rangeAggRows         [][]driver.Value
	rangeDailyRows       [][]driver.Value
}

// newRows 按列名 + 二维值构造 sqlmock 行集（AddRow 是变参，不能直接展开 [][]driver.Value）。
func newRows(cols []string, data [][]driver.Value) *sqlmock.Rows {
	rows := sqlmock.NewRows(cols)
	for _, r := range data {
		rows.AddRow(r...)
	}
	return rows
}

// expectLiveCompatSQL 按实现顺序登记查询期望（只用正则，不带参数断言，兼容两个版本）。
//
// 顺序：#66 之前的旧路径（会话聚合 → 最近请求 → 会话作用域用户聚合 → 用户分钟曲线 → 名称），
// 未请求中会话时旧路径一个查询都不发；比较模式下再追加新口径（范围聚合 → 名称 → 分钟曲线 → 日序列）。
func expectLiveCompatSQL(t *testing.T, mock sqlmock.Sqlmock, f liveCompatFixture, gen bool) {
	t.Helper()
	if len(f.inflight) > 0 {
		if gen {
			mock.ExpectQuery(regexp.QuoteMeta(`GROUP BY session_id`)).
				WillReturnRows(newRows([]string{"session_id", "calls", "credits", "win_tokens"}, f.sessionAggRowsLegacy))
		} else {
			mock.ExpectQuery(regexp.QuoteMeta(`GROUP BY session_id`)).
				WillReturnRows(newRows([]string{"session_id", "calls", "credits", "total_tokens", "win_tokens"}, f.sessionAggRows))
		}
		mock.ExpectQuery(regexp.QuoteMeta(`row_number() OVER (PARTITION BY session_id`)).
			WillReturnRows(newRows([]string{"session_id", "rn", "tokens_total", "ctx_tokens"}, f.recentRows))
		// 会话作用域的用户聚合（#66 之前的实现带 session_id 过滤；新实现恢复此形状给旧字段用）。
		mock.ExpectQuery(regexp.QuoteMeta(`AND session_id = ANY(string_to_array($2, ','))`)).
			WillReturnRows(newRows([]string{"user_id", "tokens", "credits"}, f.userAggRows))
		mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('minute'`)).
			WillReturnRows(newRows([]string{"user_id", "minute", "tokens"}, f.sparkRows))
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, username, nickname FROM users`)).
			WillReturnRows(newRows([]string{"id", "username", "nickname"}, f.userNameRows))
	}
	if gen {
		return
	}
	// 新口径：范围内用户聚合（不带 session 作用域）+ 名称 + 分钟曲线 + 日序列
	mock.ExpectQuery(regexp.QuoteMeta(`count(DISTINCT session_id)`)).
		WillReturnRows(newRows([]string{"user_id", "calls", "active_sessions",
			"win_tokens", "win_credits", "range_tokens", "range_credits"}, f.rangeAggRows))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT id, username, nickname FROM users`)).
		WillReturnRows(newRows([]string{"id", "username", "nickname"}, f.userNameRows))
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('minute'`)).
		WillReturnRows(newRows([]string{"user_id", "minute", "tokens"}, f.sparkRows))
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('day'`)).
		WillReturnRows(newRows([]string{"user_id", "day", "tokens"}, f.rangeDailyRows))
}

// TestLiveCompatGolden 旧字段逐字节回归：三组场景（三个请求中会话 / 一个在途+两个刚结束 / 全空闲）
// 的 `SessionsTotal` / `UsersTotal` / `Truncated` / `UsersTruncated` / `Users` 与
// `Sessions`（投影到旧字段集）都必须与 #66 合入前完全一致。
func TestLiveCompatGolden(t *testing.T) {
	write := os.Getenv("LIVE_GOLDEN") == "write"
	dir := "testdata"

	fixtures := []liveCompatFixture{
		{
			name:                 "three_inflight_one_user",
			inflight:             []string{"s1", "s2", "s3"},
			sessionAggRowsLegacy: [][]driver.Value{{"s1", int64(2), float64(1.5), int64(900)}, {"s2", int64(1), float64(0.5), int64(300)}, {"s3", int64(1), float64(0.25), int64(150)}},
			sessionAggRows:       [][]driver.Value{{"s1", int64(2), float64(1.5), int64(2000), int64(900)}, {"s2", int64(1), float64(0.5), int64(600), int64(300)}, {"s3", int64(1), float64(0.25), int64(300), int64(150)}},
			// 旧口径：只汇总「请求中会话」的窗口用量（5550 → 900+300+150 之外的历史不计）
			recentRows: [][]driver.Value{
				{"s1", 1, int64(900), int64(800)}, {"s2", 1, int64(300), int64(280)}, {"s3", 1, int64(150), int64(120)},
			},
			userAggRows:  [][]driver.Value{{int64(7), int64(1350), float64(2.25)}},
			sparkRows:    [][]driver.Value{},
			userNameRows: [][]driver.Value{{int64(7), "dev", ""}},
			rangeAggRows: [][]driver.Value{{int64(7), int64(7), int64(3), int64(1350), float64(2.25), int64(5550), float64(9)}},
			rangeDailyRows: [][]driver.Value{
				{int64(7), liveNow(), int64(5550)},
			},
		},
		{
			name:                 "one_inflight_two_exited",
			inflight:             []string{"s1"},
			sessionAggRowsLegacy: [][]driver.Value{{"s1", int64(3), float64(2), int64(1200)}},
			sessionAggRows:       [][]driver.Value{{"s1", int64(3), float64(2), int64(3600), int64(1200)}},
			recentRows:           [][]driver.Value{{"s1", 1, int64(1200), int64(1000)}},
			userAggRows:          [][]driver.Value{{int64(7), int64(1200), float64(2)}},
			sparkRows:            [][]driver.Value{},
			userNameRows:         [][]driver.Value{{int64(7), "dev", ""}},
			rangeAggRows:         [][]driver.Value{{int64(7), int64(7), int64(3), int64(1200), float64(2), int64(5550), float64(9)}},
			rangeDailyRows:       [][]driver.Value{{int64(7), liveNow(), int64(5550)}},
		},
		{
			name:           "all_idle_no_user",
			inflight:       nil,
			sessionAggRows: [][]driver.Value{},
			recentRows:     [][]driver.Value{},
			userAggRows:    [][]driver.Value{},
			sparkRows:      [][]driver.Value{},
			userNameRows:   [][]driver.Value{},
			rangeAggRows:   [][]driver.Value{{int64(7), int64(4), int64(2), int64(0), float64(0), int64(4000), float64(7)}},
			rangeDailyRows: [][]driver.Value{{int64(7), liveNow(), int64(4000)}},
		},
	}

	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			db, mock, err := sqlmock.New()
			if err != nil {
				t.Fatalf("sqlmock: %v", err)
			}
			defer db.Close()

			cur := liveNow().Add(-3 * time.Minute)
			reg := router.NewSessionRegistry(func() time.Time { return cur })
			// 先建「刚结束请求」的会话（LastActive 落在淡出窗口内），再建请求中的会话。
			for _, id := range []string{"x1", "x2"} {
				if ok, _ := reg.Attach(&router.Session{
					SessionID: id, UserID: 7, Model: "gpt-4o", ExpireAt: liveNow().Add(time.Hour),
				}, 100); !ok {
					t.Fatalf("attach %s 失败", id)
				}
			}
			cur = liveNow()
			for _, id := range f.inflight {
				if ok, _ := reg.Attach(&router.Session{
					SessionID: id, UserID: 7, Model: "gpt-4o", ExpireAt: liveNow().Add(time.Hour),
				}, 100); !ok {
					t.Fatalf("attach %s 失败", id)
				}
				reg.BeginRequest(id)
			}

			expectLiveCompatSQL(t, mock, f, write)

			h := NewAdmin(db, nil, nil, nil)
			h.now = liveNow
			h.SetSessions(reg)
			rec := httptest.NewRecorder()
			h.HandleStatsLive(rec, httptest.NewRequest(http.MethodGet,
				"/api/v1/admin/stats/live?Window=60&SparkPoints=12&Limit=20", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}

			got := projectLiveCompat(t, rec.Body.Bytes())
			gotRaw, err := json.Marshal(got)
			if err != nil {
				t.Fatalf("marshal projection: %v", err)
			}

			path := filepath.Join(dir, "live_compat_"+f.name+".golden.json")
			if write {
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(path, gotRaw, 0o644); err != nil {
					t.Fatalf("write golden: %v", err)
				}
				return
			}
			want, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("读取 golden %s 失败: %v", path, err)
			}
			if string(gotRaw) != string(want) {
				t.Fatalf("旧字段与 #66 合入前不再逐字节一致（%s）\n got: %s\nwant: %s", f.name, gotRaw, want)
			}
		})
	}
}
