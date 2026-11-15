package usagebilling

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// 代理侧外层三种形状都要能解析（字段名未联调确认，adapter 需容错）。
func TestParseLedgerBody_Shapes(t *testing.T) {
	cases := map[string]string{
		"items": `{"items":[{"caller_key_name":"k1","model":"m1","requests":2,"input_tokens":10,"cache_read_input_tokens":1,"output_tokens":5}]}`,
		"data":  `{"data":[{"callerKey":"k1","magpie_model":"m1","count":2,"prompt_tokens":10,"cached_tokens":1,"completion_tokens":5}]}`,
		"array": `[{"key":"k1","model":"m1","tokens":{"input":10,"cache_read":1,"output":5}},{"key":"k1","model":"m1","tokens":{"input":10,"cache_read":1,"output":5}}]`,
	}
	for name, body := range cases {
		gs, _, err := parseLedgerBody([]byte(body))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var tot Group
		for _, g := range gs {
			tot.Input += g.Input
			tot.CacheRead += g.CacheRead
			tot.Output += g.Output
			tot.Requests += g.Requests
			if g.CallerKey != "k1" || g.Model != "m1" {
				t.Fatalf("%s: 分组键=%q/%q", name, g.CallerKey, g.Model)
			}
		}
		want := []int64{10, 1, 5}
		if name == "array" {
			want = []int64{20, 2, 10}
		}
		if tot.Input != want[0] || tot.CacheRead != want[1] || tot.Output != want[2] {
			t.Fatalf("%s: tokens=%+v", name, tot)
		}
	}
}

// 聚合形状的 requests/count 必须计入笔数：忽略它会让两臂都恒定 Requests=1 →
// 分组样本门槛（≥5 笔）永不满足、req_count_mismatch 恒真（复审 P2）。
func TestParseLedgerBody_AggregateRequestCounts(t *testing.T) {
	body := `{"items":[
		{"caller_key_name":"k1","model":"m1","requests":7,"input_tokens":70,"output_tokens":7},
		{"caller_key_name":"k1","model":"m1","count":4,"input_tokens":40,"output_tokens":4},
		{"caller_key_name":"k2","model":"m2","input_tokens":10,"output_tokens":1}]}`
	gs, _, err := parseLedgerBody([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	got := map[string]int64{}
	for _, g := range gs {
		got[g.CallerKey] = g.Requests
	}
	if got["k1"] != 11 {
		t.Fatalf("聚合形状笔数应累加为 11（7+4），实际 %d", got["k1"])
	}
	if got["k2"] != 1 {
		t.Fatalf("逐笔形状仍按 1 笔，实际 %d", got["k2"])
	}
}

// 聚合形状 + 分组判定端到端：笔数达标后，偏差组必须能报警（旧实现样本门槛永远不满足）。
func TestCompareWindow_AggregateRequestGateWorks(t *testing.T) {
	gs, _, err := parseLedgerBody([]byte(
		`{"items":[{"caller_key_name":"k1","model":"m1","requests":7,"input_tokens":100,"output_tokens":0}]}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	local := []Group{g("k1", "m1", 7, 200, 0, 0)}
	w := compareWindow(wStart, wEnd, local, gs, 0)
	if w.Status != "alarm" {
		t.Fatalf("聚合笔数 7 ≥ 门槛 5 时应按偏差报警: %+v", w.Detail)
	}
	groups, _ := w.Detail["alarm_groups"].([]map[string]any)
	if len(groups) != 1 {
		t.Fatalf("命中组=%+v", groups)
	}
	if w.Detail["req_count_mismatch"] != false {
		t.Fatalf("两侧都是 7 笔时不该标记 mismatch: %+v", w.Detail["req_count_mismatch"])
	}
}

// 未知形状必须报错（而不是静默当成 0，让整窗假绿）。
func TestParseLedgerBody_UnknownShapeErrors(t *testing.T) {
	if _, _, err := parseLedgerBody([]byte(`{"foo":"bar"}`)); err == nil {
		t.Fatalf("未知形状应报错")
	}
}

// 分批：>50 个 callerKey 要拆多次请求（URL 长度与代理侧上限）。
func TestLedgerHTTP_BatchesByCallerKey(t *testing.T) {
	var batches []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != LedgerPath {
			t.Errorf("path=%s", got)
		}
		keys := r.URL.Query().Get("callerKey")
		batches = append(batches, len(splitKeys(keys)))
		if r.Header.Get("Authorization") != "Bearer admin-key" {
			t.Errorf("鉴权头=%q", r.Header.Get("Authorization"))
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()

	keys := make([]string, 0, 120)
	for i := 0; i < 120; i++ {
		keys = append(keys, "k")
	}
	l := NewLedgerHTTP(srv.URL, "admin-key", "", srv.Client())
	if _, _, err := l.FetchWindow(t.Context(), time.Now().Add(-time.Minute), time.Now(), keys); err != nil {
		t.Fatalf("FetchWindow: %v", err)
	}
	if len(batches) != 3 || batches[0] != 50 || batches[1] != 50 || batches[2] != 20 {
		t.Fatalf("分批=%v", batches)
	}
}

func splitKeys(s string) []string {
	if s == "" {
		return nil
	}
	out := []string{}
	cur := ""
	for _, c := range s {
		if c == ',' {
			out = append(out, cur)
			cur = ""
			continue
		}
		cur += string(c)
	}
	return append(out, cur)
}

// 非 2xx 必须返回 error（窗口记 error 并下窗重试，而不是拿空数据判 ok）。
func TestLedgerHTTP_Non2xxErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`oops`))
	}))
	defer srv.Close()
	l := NewLedgerHTTP(srv.URL, "k", "", srv.Client())
	if _, _, err := l.FetchWindow(t.Context(), time.Now(), time.Now(), []string{"k1"}); err == nil {
		t.Fatalf("5xx 应返回 error")
	}
}
