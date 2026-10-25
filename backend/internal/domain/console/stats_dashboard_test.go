package console

import (
	"context"
	"encoding/json"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/domain/billing"
)

// TestParseStatsRange_KeepsExactForHourBuckets 断言：
// 整天归一的 From/To 保持不变（既有口径），同时保留请求原始区间给小时桶用。
// 「近 24h」若只按归一区间切桶，会被拉长到最多 48 小时、多出半天空桶。
func TestParseStatsRange_KeepsExactForHourBuckets(t *testing.T) {
	loc := cnLocation()
	now := time.Date(2026, 10, 25, 14, 37, 22, 0, loc)
	q := url.Values{
		"From": {now.Add(-24 * time.Hour).Format(time.RFC3339)},
		"To":   {now.Format(time.RFC3339)},
	}
	rg, err := parseStatsRange(q, now)
	if err != nil {
		t.Fatalf("parseStatsRange: %v", err)
	}
	if !rg.ExactFrom.Equal(now.Add(-24*time.Hour)) || !rg.ExactTo.Equal(now) {
		t.Fatalf("原始区间被改写: %v ~ %v", rg.ExactFrom, rg.ExactTo)
	}
	// 归一：起点当天 00:00、终点当天 23:59:59（既有语义不变）。
	wantFrom := time.Date(2026, 10, 24, 0, 0, 0, 0, loc)
	wantTo := time.Date(2026, 10, 25, 23, 59, 59, 0, loc)
	if !rg.From.Equal(wantFrom) || !rg.To.Equal(wantTo) {
		t.Fatalf("归一区间错: %v ~ %v，want %v ~ %v", rg.From, rg.To, wantFrom, wantTo)
	}
	// 跨时区提交（UTC）应落到同一个北京时间日界。
	q = url.Values{"From": {"2026-10-24T18:00:00Z"}, "To": {"2026-10-25T18:00:00Z"}}
	rg, err = parseStatsRange(q, now)
	if err != nil {
		t.Fatalf("parseStatsRange(UTC): %v", err)
	}
	if !rg.From.Equal(time.Date(2026, 10, 25, 0, 0, 0, 0, loc)) {
		t.Fatalf("UTC 2026-10-24T18:00Z 应归到北京 10-25 日界，got %v", rg.From)
	}

	// 既有签名保持可用（老调用方口径不变）。
	from, to, err := statsRangeFromQuery(q, now)
	if err != nil || !from.Equal(rg.From) || !to.Equal(rg.To) {
		t.Fatalf("statsRangeFromQuery 行为漂移: %v %v %v", from, to, err)
	}
}

// TestHandleStatsDashboard_HourlyComparePlaza 断言一次「近 24h」请求的完整装配：
// 老字段（Daily/KPI/各 Top）语义不变，同时给出小时桶、四日同期对比、模型广场。
func TestHandleStatsDashboard_HourlyComparePlaza(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := billing.NewSqlStore(db)

	// 1) 日界口径聚合（Usage 块）
	mock.ExpectQuery(regexp.QuoteMeta(`COALESCE(sum((first_token_ms IS NOT NULL)::int),0)`)).
		WillReturnRows(sqlmock.NewRows([]string{
			"input", "output", "cache_read", "credits", "calls", "failed",
			"sumdur", "sumfirst", "okdur", "okfirst", "busyreq", "busytok", "busydur",
		}).AddRow(int64(100), int64(50), int64(20), float64(12.5), int64(10), int64(1),
			int64(180000), int64(400), int64(10), int64(10), int64(9), int64(160), int64(120000)))
	// 2) 按天（老字段必须保持）
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('day', call_time AT TIME ZONE 'Asia/Shanghai')::date`)).
		WillReturnRows(sqlmock.NewRows([]string{"d", "calls", "failed", "credits", "tokens", "duration"}).
			AddRow(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), int64(10), int64(1), float64(12.5), int64(170), int64(2000)))
	// 3-6) 各维度 Top
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT external_model_name,`)).
		WillReturnRows(sqlmock.NewRows([]string{"model", "calls", "failed", "credits", "tokens"}).
			AddRow("gpt-4o", int64(10), int64(1), float64(12.5), int64(170)))
	mock.ExpectQuery(regexp.QuoteMeta(`JOIN channel_keys ck ON ck.id = br.channel_key_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"key", "channel", "calls", "failed", "credits", "tokens"}).
			AddRow("主密钥", "openai", int64(10), int64(1), float64(12.5), int64(170)))
	mock.ExpectQuery(regexp.QuoteMeta(`JOIN users u ON u.id = br.user_id`)).
		WillReturnRows(sqlmock.NewRows([]string{"username", "nickname", "calls", "failed", "credits", "tokens"}).
			AddRow("dev", "开发者", int64(10), int64(1), float64(12.5), int64(170)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT pricing_mode,`)).
		WillReturnRows(sqlmock.NewRows([]string{"mode", "calls", "failed", "credits", "tokens"}).
			AddRow("sale", int64(10), int64(1), float64(12.5), int64(170)))
	// 7-9) 错误 / 分桶 / 分位
	mock.ExpectQuery(regexp.QuoteMeta(`'(未记录)'`)).
		WillReturnRows(sqlmock.NewRows([]string{"msg", "c"}).AddRow("超时", int64(1)))
	mock.ExpectQuery(regexp.QuoteMeta(`'<100ms'`)).
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "n"}).AddRow("1-3s", int64(10)))
	mock.ExpectQuery(regexp.QuoteMeta(`percentile_disc(0.5)`)).
		WillReturnRows(sqlmock.NewRows([]string{"p50", "p90", "p95"}).AddRow(float64(200), float64(400), float64(500)))
	// 1b) KPI 窗口口径（严格按请求区间，同一份 SQL 的第二跑）
	mock.ExpectQuery(regexp.QuoteMeta(`COALESCE(sum((first_token_ms IS NOT NULL)::int),0)`)).
		WillReturnRows(sqlmock.NewRows([]string{
			"input", "output", "cache_read", "credits", "calls", "failed",
			"sumdur", "sumfirst", "okdur", "okfirst", "busyreq", "busytok", "busydur",
		}).AddRow(int64(200), int64(50), int64(20), float64(20.5), int64(12), int64(2),
			int64(200000), int64(500), int64(12), int64(12), int64(11), int64(200), int64(100000)))
	// 10) 上期总量（环比）
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*),`)).
		WillReturnRows(sqlmock.NewRows([]string{"calls", "credits", "tokens"}).
			AddRow(int64(8), float64(10), int64(150)))
	// 11) 小时桶（近 24h 自动按小时）
	mock.ExpectQuery(regexp.QuoteMeta(`date_trunc('hour', call_time AT TIME ZONE 'Asia/Shanghai')`)).
		WillReturnRows(sqlmock.NewRows([]string{"bucket", "calls", "failed", "credits", "tokens", "duration"}).
			AddRow(time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC), int64(4), int64(0), float64(5), int64(60), int64(800)).
			AddRow(time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC), int64(6), int64(1), float64(7.5), int64(110), int64(1200)))
	// 12) 四日同期对比（CompareDays=4）
	mock.ExpectQuery(regexp.QuoteMeta(`EXTRACT(hour FROM call_time AT TIME ZONE 'Asia/Shanghai')::int`)).
		WillReturnRows(sqlmock.NewRows([]string{"day", "hour", "calls", "failed", "credits", "tokens", "duration"}).
			AddRow(time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC), 9, int64(4), int64(0), float64(5), int64(60), int64(800)).
			AddRow(time.Date(2026, 10, 24, 0, 0, 0, 0, time.UTC), 9, int64(3), int64(0), float64(4), int64(50), int64(700)))
	// 13) 模型广场（按实体归属）
	mock.ExpectQuery(regexp.QuoteMeta(`LEFT JOIN agg a ON a.ext_id = em.id`)).
		WillReturnRows(sqlmock.NewRows([]string{"model", "enabled", "calls", "failed", "credits", "tokens",
			"busy_calls", "busy_tokens", "busy_ms", "dur_sum", "dur_n", "first_sum", "first_n"}).
			AddRow("gpt-4o", true, int64(10), int64(1), float64(12.5), int64(170), int64(9), int64(160), int64(120000), int64(18000), int64(10), int64(400), int64(10)).
			AddRow("idle-model", true, int64(0), int64(0), float64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0), int64(0)))
	// 14) 目录总数 / 活跃数 / 未匹配用量
	mock.ExpectQuery(regexp.QuoteMeta(`FROM agg a`)).
		WillReturnRows(sqlmock.NewRows([]string{"total", "active", "um_calls", "um_failed", "um_credits", "um_tokens",
			"um_busy_calls", "um_busy_tokens", "um_busy_ms", "um_dur_sum", "um_dur_n", "um_first_sum", "um_first_n"}).
			AddRow(int64(2), int64(1), int64(0), int64(0), float64(0), float64(0),
				int64(0), float64(0), int64(0), int64(0), int64(0), int64(0), int64(0)))

	q := url.Values{
		"From":        {"2026-10-25T02:00:00+08:00"},
		"To":          {"2026-10-26T02:00:00+08:00"},
		"CompareDays": {"4"},
	}
	rg, err := parseStatsRange(q, liveNow())
	if err != nil {
		t.Fatalf("parseStatsRange: %v", err)
	}
	opts, err := parseStatsOptions(q, rg.ExactTo.Sub(rg.ExactFrom))
	if err != nil {
		t.Fatalf("parseStatsOptions: %v", err)
	}
	out, err := buildStatsUsage(context.Background(), store, rg, nil, "global", opts)
	if err != nil {
		t.Fatalf("buildStatsUsage: %v", err)
	}
	// 按「{Code,Data}」外壳序列化（与 HTTP 响应同形），便于按 JSON 字段名断言。
	raw, err := json.Marshal(map[string]any{"Code": 0, "Data": out})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var body struct {
		Code int `json:"Code"`
		Data struct {
			Scope string `json:"Scope"`
			KPI   struct {
				TotalRequests int64   `json:"TotalRequests"`
				TotalTokens   int64   `json:"TotalTokens"`
				TPM           float64 `json:"TPM"`
				WindowFrom    string  `json:"WindowFrom"`
				WindowTo      string  `json:"WindowTo"`
				WindowMode    string  `json:"WindowMode"`
			} `json:"KPI"`
			Usage struct {
				Granularity string `json:"Granularity"`
				Total       struct {
					Input     int64 `json:"Input"`
					Output    int64 `json:"Output"`
					CacheRead int64 `json:"CacheRead"`
				} `json:"Total"`
				TotalCall int64 `json:"TotalCall"`
				Daily     []struct {
					Date string `json:"Date"`
				} `json:"Daily"`
				Hourly []struct {
					Hour      string `json:"Hour"`
					HourOfDay int    `json:"HourOfDay"`
					Tokens    int64  `json:"Tokens"`
				} `json:"Hourly"`
				Compare []struct {
					Day    string `json:"Day"`
					Label  string `json:"Label"`
					Offset int    `json:"Offset"`
					Points []struct {
						HourOfDay int `json:"HourOfDay"`
					} `json:"Points"`
				} `json:"Compare"`
			} `json:"Usage"`
			ModelPlaza struct {
				Sort      string `json:"Sort"`
				Total     int    `json:"Total"`
				Active    int    `json:"Active"`
				Truncated bool   `json:"Truncated"`
				List      []struct {
					Model  string  `json:"Model"`
					Active bool    `json:"Active"`
					TPM    float64 `json:"TPM"`
				} `json:"List"`
			} `json:"ModelPlaza"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, string(raw))
	}

	// KPI 走 **KPI 窗口**（严格按请求区间）：calls=12 / tokens=200+20+50=270。
	if body.Data.Scope != "global" || body.Data.KPI.TotalRequests != 12 || body.Data.KPI.TotalTokens != 270 {
		t.Fatalf("KPI 应取 KPI 窗口口径: %+v", body.Data.KPI)
	}
	if body.Data.KPI.WindowMode != "exact" ||
		body.Data.KPI.WindowFrom != "2026-10-25T02:00:00+08:00" ||
		body.Data.KPI.WindowTo != "2026-10-26T02:00:00+08:00" {
		t.Fatalf("KPI 窗口元信息错: %+v", body.Data.KPI)
	}
	// Daily 仍是按天、且按归一区间补零（10-25 有数据 + 10-26 补零），老口径不动。
	// Usage 块仍是日界归一口径（calls=10 / tokens=170），与 KPI 窗口刻意不同。
	if body.Data.Usage.TotalCall != 10 ||
		body.Data.Usage.Total.Input+body.Data.Usage.Total.Output+body.Data.Usage.Total.CacheRead != 170 {
		t.Fatalf("Usage 应保持日界归一口径: %+v", body.Data.Usage)
	}
	if len(body.Data.Usage.Daily) != 2 || body.Data.Usage.Daily[0].Date != "2026-10-25" ||
		body.Data.Usage.Daily[1].Date != "2026-10-26" {
		t.Fatalf("Daily 应保持按天口径，got %+v", body.Data.Usage.Daily)
	}
	// 新字段：小时桶 + 四日对比 + 模型广场
	if body.Data.Usage.Granularity != "hour" {
		t.Fatalf("Granularity=%q, want hour", body.Data.Usage.Granularity)
	}
	if len(body.Data.Usage.Hourly) != 2 || body.Data.Usage.Hourly[0].Hour != "2026-10-25T09:00" ||
		body.Data.Usage.Hourly[1].HourOfDay != 10 {
		t.Fatalf("小时桶错: %+v", body.Data.Usage.Hourly)
	}
	if len(body.Data.Usage.Compare) != 4 || body.Data.Usage.Compare[0].Label != "今天" ||
		body.Data.Usage.Compare[0].Offset != 0 || body.Data.Usage.Compare[0].Day != "2026-10-26" {
		t.Fatalf("同期对比序列错: %+v", body.Data.Usage.Compare)
	}
	// 今天（10-26）与大前天（10-23）都没有数据：Points 必须为空，不能补 0。
	if len(body.Data.Usage.Compare[0].Points) != 0 || len(body.Data.Usage.Compare[3].Points) != 0 {
		t.Fatalf("无数据序列 Points 必须为空（不补 0）: 今天=%+v 大前天=%+v",
			body.Data.Usage.Compare[0], body.Data.Usage.Compare[3])
	}
	// 昨天（10-25）与前天（10-24）各有 09 点一个桶，同小时对齐。
	if len(body.Data.Usage.Compare[1].Points) != 1 || body.Data.Usage.Compare[1].Points[0].HourOfDay != 9 ||
		len(body.Data.Usage.Compare[2].Points) != 1 || body.Data.Usage.Compare[2].Points[0].HourOfDay != 9 {
		t.Fatalf("同期对齐错: %+v", body.Data.Usage.Compare)
	}
	if len(body.Data.ModelPlaza.List) != 2 || body.Data.ModelPlaza.Sort != "tokens" {
		t.Fatalf("模型广场错: %+v", body.Data.ModelPlaza)
	}
	if body.Data.ModelPlaza.List[1].Active {
		t.Fatalf("零用量模型应标记为不活跃（前端折叠用）")
	}
	if body.Data.ModelPlaza.Total != 2 || body.Data.ModelPlaza.Active != 1 {
		t.Fatalf("目录统计错: %+v", body.Data.ModelPlaza)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("未满足的期望: %v", err)
	}
}
