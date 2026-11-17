package upstream

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// realFeedPayload 是代理侧 `GET /v1/magpie/usage/requests?cursor=start` 的真实形状样本
// （字段取自 yetone/magpie 的 usageapi.FeedRow；只保留灵枢用到的字段 + 几个干扰字段）。
const realFeedPayload = `{
  "rows": [
    {
      "id": "u_2f1c0c9e8a7b4d31",
      "seq": 1042,
      "origin": "gateway",
      "original_ids": {
        "request_id": "req_abc",
        "response_id": "resp_x",
        "session_id": "sess_1"
      },
      "t": "2026-11-17T10:00:00.123456+08:00",
      "agent": "claude-code",
      "provider": "anthropic",
      "host": "api.anthropic.com",
      "providerKeyName": "team-key",
      "providerAccount": "alice@example.com",
      "callerKeyId": "ck_1",
      "callerKeyName": "tenant-key",
      "model": "claude-sonnet-4-20250514",
      "req": "claude-sonnet-4",
      "served": "claude-sonnet-4-20250514",
      "upstream": "anthropic",
      "in": 1200,
      "out": 345,
      "cache_read": 800,
      "cache_write": 0,
      "cache_write_1h": 0,
      "cache_unknown": 0,
      "reasoning": 0,
      "ms": 1234,
      "ttft_ms": 210,
      "status": 200,
      "err": "",
      "err_type": "",
      "rid": "req_abc",
      "session": "sess_1",
      "handed_on": false,
      "rejected": false,
      "cost": 0.0042,
      "priced": true,
      "credits": 12.5
    },
    {
      "id": "u_handed_on",
      "seq": 1043,
      "t": "2026-11-17T10:00:01+08:00",
      "provider": "anthropic",
      "providerAccount": "alice@example.com",
      "model": "claude-sonnet-4",
      "in": 10,
      "out": 0,
      "status": 200,
      "handed_on": true,
      "cost": 0,
      "priced": false
    },
    {
      "id": "",
      "seq": 1044,
      "t": "2026-11-17T10:00:02+08:00",
      "provider": "anthropic",
      "model": "claude-sonnet-4",
      "in": 10,
      "out": 0,
      "status": 200
    },
    {
      "id": "u_no_time",
      "seq": 1045,
      "provider": "anthropic",
      "model": "claude-sonnet-4",
      "in": 10,
      "out": 0,
      "status": 200
    }
  ],
  "deleted": [
    {"id": "u_old", "seq": 900}
  ],
  "next_cursor": "cur_abc",
  "has_more": true
}`

func newFeedTestServer(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

// TestMagpieLedgerSource_FetchFeed_RealSample 用代理侧真实报文样本断言映射口径：
// rows（不是 items）、id 幂等键、providerAccount（不是 callerKeyName）、in/out、t、cost。
func TestMagpieLedgerSource_FetchFeed_RealSample(t *testing.T) {
	var gotCursor, gotLimit string
	srv := newFeedTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != LedgerPath {
			t.Errorf("path = %q, want %q", r.URL.Path, LedgerPath)
		}
		if auth := r.Header.Get("Authorization"); auth != "Bearer admin-key" {
			t.Errorf("Authorization = %q", auth)
		}
		gotCursor = r.URL.Query().Get("cursor")
		gotLimit = r.URL.Query().Get("limit")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(realFeedPayload))
	})

	src := NewMagpieLedgerSource(srv.URL, "admin-key", "", nil, func(usd float64) (float64, error) {
		return usd * 100, nil // 只为断言换算被调用（真实装配是 USD×cny_rate×1e6÷R）
	})
	page, err := src.FetchFeed(context.Background(), "", 200)
	if err != nil {
		t.Fatalf("FetchFeed: %v", err)
	}
	if gotCursor != MagpieCursorStart {
		t.Fatalf("空 cursor 应发 start，实际 %q", gotCursor)
	}
	if gotLimit != "200" {
		t.Fatalf("limit 应透传 200，实际 %q", gotLimit)
	}
	if len(page.Entries) != 1 {
		t.Fatalf("只应映射 1 条（handed_on/无 id/无 t 各跳过），实际 %d", len(page.Entries))
	}
	e := page.Entries[0]
	if e.LedgerID != "u_2f1c0c9e8a7b4d31" {
		t.Fatalf("幂等键应取 id（不是 original_ids.request_id），实际 %q", e.LedgerID)
	}
	if e.Provider != "anthropic" || e.ProviderAccount != "alice@example.com" {
		t.Fatalf("维度映射错误：provider=%q account=%q（不能用 callerKeyName）", e.Provider, e.ProviderAccount)
	}
	if e.Model != "claude-sonnet-4-20250514" || e.TokensIn != 1200 || e.TokensOut != 345 {
		t.Fatalf("模型/token 映射错误：%+v", e)
	}
	if e.CostCredits != 0.42 {
		t.Fatalf("cost 应经换算（0.0042×100=0.42），实际 %v", e.CostCredits)
	}
	wantTime := time.Date(2026, 11, 17, 2, 0, 0, 123456000, time.UTC)
	if !e.OccurredAt.Equal(wantTime) {
		t.Fatalf("t 解析错误：got %s want %s", e.OccurredAt, wantTime)
	}
	if page.Skipped != 3 {
		t.Fatalf("应记录跳过 3 行（handed_on/无 id/无 t），实际 %d", page.Skipped)
	}
	if len(page.Deleted) != 1 || page.Deleted[0] != "u_old" {
		t.Fatalf("deleted 解析错误：%v", page.Deleted)
	}
	if page.NextCursor != "cur_abc" || !page.HasMore {
		t.Fatalf("游标/是否还有下一页解析错误：%q %v", page.NextCursor, page.HasMore)
	}
}

// TestMagpieLedgerSource_ClampsLimit 断言 limit 上限 500（代理侧超过会 400）。
func TestMagpieLedgerSource_ClampsLimit(t *testing.T) {
	var got string
	srv := newFeedTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("limit")
		_, _ = w.Write([]byte(`{"rows":[],"next_cursor":"","has_more":false}`))
	})
	src := NewMagpieLedgerSource(srv.URL, "", "", nil, nil)
	if _, err := src.FetchFeed(context.Background(), "c1", 5000); err != nil {
		t.Fatalf("FetchFeed: %v", err)
	}
	if got != "500" {
		t.Fatalf("limit 应被夹到 500，实际 %q", got)
	}
}

// TestMagpieLedgerSource_CursorErrors 断言 410/400 cursor_* 被识别为「必须从 start 重放」，
// 其它 4xx 不重放（避免把鉴权/参数错误变成无限重放）。
func TestMagpieLedgerSource_CursorErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		reset  bool
	}{
		{"410_cursor_reset", 410, `{"error":{"code":"cursor_reset"}}`, true},
		{"410_cursor_expired", 410, `{"code":"cursor_expired"}`, true},
		{"400_cursor_invalid", 400, `{"error":"cursor_invalid"}`, true},
		{"400_cursor_filter_changed", 400, `{"error":{"code":"cursor_filter_changed"}}`, true},
		{"400_bad_request", 400, `{"error":{"code":"invalid_request_error"}}`, false},
		{"401_unauthorized", 401, `{"error":"unauthorized"}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := newFeedTestServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			src := NewMagpieLedgerSource(srv.URL, "k", "", nil, nil)
			_, err := src.FetchFeed(context.Background(), "c1", 100)
			if err == nil {
				t.Fatalf("应返回错误")
			}
			var ce *MagpieCursorError
			if errors.As(err, &ce) != tc.reset {
				t.Fatalf("errors.As(MagpieCursorError)=%v, want %v (err=%v)", errors.As(err, &ce), tc.reset, err)
			}
			if tc.reset && !ce.Reset() {
				t.Fatalf("cursor 错误应判定为 Reset")
			}
		})
	}
}

// TestMagpieLedgerSource_RejectsLegacyShape 断言旧形状（items/data）会显式报错，
// 不能静默返回 0 行 —— 那正是 F9 要防的「空轮假绿」。
func TestMagpieLedgerSource_RejectsLegacyShape(t *testing.T) {
	srv := newFeedTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"items":[],"next_cursor":"x"}`))
	})
	src := NewMagpieLedgerSource(srv.URL, "", "", nil, nil)
	if _, err := src.FetchFeed(context.Background(), MagpieCursorStart, 100); err == nil {
		t.Fatalf("缺少 rows 必须报错")
	} else if !strings.Contains(err.Error(), "rows") {
		t.Fatalf("错误应点明缺少 rows，实际 %v", err)
	}
}

// TestMagpieLedgerSource_PricedFalseIgnoresCost 断言 priced=false 的行成本记 0，
// 且**不调用**换算函数（代理侧没算价时 cost 字段无意义）。
func TestMagpieLedgerSource_PricedFalseIgnoresCost(t *testing.T) {
	srv := newFeedTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"rows":[{
			"id":"u_unpriced","t":"2026-11-17T10:00:00Z","provider":"openai",
			"providerAccount":"acct","model":"gpt-4o","in":1,"out":2,
			"cost":9.99,"priced":false
		}],"has_more":false}`))
	})
	called := false
	src := NewMagpieLedgerSource(srv.URL, "", "", nil, func(usd float64) (float64, error) {
		called = true
		return usd * 100, nil
	})
	page, err := src.FetchFeed(context.Background(), "c", 10)
	if err != nil {
		t.Fatalf("FetchFeed: %v", err)
	}
	if called {
		t.Fatalf("priced=false 不应调用成本换算")
	}
	if len(page.Entries) != 1 || page.Entries[0].CostCredits != 0 {
		t.Fatalf("priced=false 成本应为 0，实际 %+v", page.Entries)
	}
}

// TestMagpieLedgerSource_ConversionErrorSurfaces 断言换算失败整页报错（不静默写 0 成本）。
func TestMagpieLedgerSource_ConversionErrorSurfaces(t *testing.T) {
	srv := newFeedTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"rows":[{
			"id":"u_1","t":"2026-11-17T10:00:00Z","provider":"openai","model":"gpt-4o",
			"in":1,"out":2,"cost":1.5,"priced":true
		}],"has_more":false}`))
	})
	src := NewMagpieLedgerSource(srv.URL, "", "", nil, func(float64) (float64, error) {
		return 0, errors.New("billing.cny_rate 缺失")
	})
	if _, err := src.FetchFeed(context.Background(), "c", 10); err == nil {
		t.Fatalf("换算失败必须报错")
	}
}

// TestParseMagpieFeed_Defaults 断言缺省值：model 缺失回落 served；providerAccount 缺失空串
// （绝不回落到 callerKeyName）；负 token 夹到 0。
func TestParseMagpieFeed_Defaults(t *testing.T) {
	payload := `{"rows":[{"id":"u_1","t":"2026-11-17T10:00:00Z",
		"provider":"openai","model":"","served":"gpt-4o-served","callerKeyName":"tenant-key",
		"in":-5,"out":-1,"priced":false}],"has_more":false}`
	page, err := parseMagpieFeed([]byte(payload), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := page.Entries[0]
	if e.Model != "gpt-4o-served" {
		t.Fatalf("model 缺失应回落 served，实际 %q", e.Model)
	}
	if e.ProviderAccount != "" {
		t.Fatalf("providerAccount 缺失应为空，实际 %q（不能用 callerKeyName 兜底）", e.ProviderAccount)
	}
	if e.TokensIn != 0 || e.TokensOut != 0 {
		t.Fatalf("负 token 应夹到 0，实际 in=%d out=%d", e.TokensIn, e.TokensOut)
	}
}

// TestMagpieCursorError_JSONShape 确保错误体三种形状都能取到 code（代理侧版本差异）。
func TestMagpieCursorError_JSONShape(t *testing.T) {
	for _, body := range []string{
		`{"code":"cursor_reset"}`,
		`{"error":"cursor_reset"}`,
		`{"error":{"code":"cursor_reset"}}`,
		`{"error":{"type":"cursor_reset"}}`,
	} {
		ce := &MagpieCursorError{StatusCode: 410, Code: magpieErrorCode([]byte(body))}
		if !ce.Reset() {
			t.Fatalf("应识别为可重放：%s", body)
		}
	}
}

// TestParseMagpieFeed_CanceledAndRejectedIncluded 断言 499（客户端取消）与 rejected
// 都不能被账本适配层吞掉：它们是**发生过的请求**，漏了会让灵枢少一笔（对账单侧缺账）。
func TestParseMagpieFeed_CanceledAndRejectedIncluded(t *testing.T) {
	payload := `{"rows":[
		{"id":"u_499","t":"2026-11-17T10:00:00Z","provider":"openai","providerAccount":"acct",
		 "model":"gpt-4o","in":0,"out":0,"status":499,"err_type":"agent_canceled","priced":false},
		{"id":"u_rejected","t":"2026-11-17T10:00:01Z","provider":"openai","providerAccount":"acct",
		 "model":"gpt-4o","in":0,"out":0,"status":400,"rejected":true,"priced":false}
	],"has_more":false}`
	page, err := parseMagpieFeed([]byte(payload), nil)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(page.Entries) != 2 || page.Skipped != 0 {
		t.Fatalf("499/rejected 都应保留，got entries=%d skipped=%d", len(page.Entries), page.Skipped)
	}
	if page.Entries[0].LedgerID != "u_499" || page.Entries[1].LedgerID != "u_rejected" {
		t.Fatalf("顺序/幂等键错误：%+v", page.Entries)
	}
}
