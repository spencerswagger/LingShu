package channel

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/pkg/crypto"
	"github.com/team/llmgateway/internal/pkg/errbody"
	"github.com/team/llmgateway/internal/testutil/randbody"
)

// 本文件钉住「探活链路的上游原文可见性」——本任务三个运营落点里的两个：
//
//	① 探活单次结果（POST .../keys/{kid}/probe、.../models/{mid}/probe）→ ProbeOutcome.ErrorBody；
//	② 探活历史（GET .../probe-logs）→ ProbeLog.ErrorBody（probe_logs.error_body）。
//
// 断言口径只有一条：随机内容**逐字节**出现在运营可见处。
// 不挑字段、不比对片段——否则实现里「按内容白名单/字段裁剪/文案映射」的缺陷照样能过。
// 旧实现（非 2xx 从不读 resp.Body）在这套用例下必然全红。

// argString 捕获 sqlmock 实际传入的字符串参数（用来断言 probe_logs.error_body 到底存了什么）。
type argString struct {
	got string
	set bool
}

// Match 实现 sqlmock.Argument：只在值是字符串时捕获并放行（类型不符即判失败）。
func (a *argString) Match(v driver.Value) bool {
	s, ok := v.(string)
	if !ok {
		return false
	}
	a.got, a.set = s, true
	return true
}

// probeBodyCase 一种上游错误响应（形状 + 随机内容）。
type probeBodyCase struct {
	name   string
	status int
	body   string
}

// randomProbeCases 生成探活用例集：≥10 组随机值 + 8 类形状各一组（内容每轮不同）。
func randomProbeCases(rnd *rand.Rand) []probeBodyCase {
	cases := make([]probeBodyCase, 0, 20)
	add := func(name string, status int, body string) {
		cases = append(cases, probeBodyCase{
			name: fmt.Sprintf("%s#%s", name, randbody.Digest(body)), status: status, body: body,
		})
	}

	// ①-⑩ 十组不同随机值（形状轮换）。
	statuses := []int{http.StatusBadRequest, http.StatusUnprocessableEntity, http.StatusConflict,
		http.StatusInternalServerError, http.StatusBadGateway, http.StatusNotFound,
		http.StatusTeapot, http.StatusServiceUnavailable, http.StatusPaymentRequired, http.StatusBadGateway}
	for i := 0; i < 10; i++ {
		status := statuses[i%len(statuses)]
		switch i % 5 {
		case 0:
			add(fmt.Sprintf("随机OpenAI形状-%d", i), status, randbody.JSONText(randbody.OpenAIError(rnd)))
		case 1:
			add(fmt.Sprintf("随机深嵌套JSON-%d", i), status, randbody.JSONText(randbody.Doc(rnd, 4)))
		case 2:
			add(fmt.Sprintf("随机平铺JSON-%d", i), status,
				randbody.JSONText(map[string]any{randbody.Word(rnd): randbody.Text(rnd, 5, 80)}))
		case 3:
			add(fmt.Sprintf("随机纯文本-%d", i), status, "probe rejected: "+randbody.Text(rnd, 20, 300))
		case 4:
			add(fmt.Sprintf("随机HTML-%d", i), status, randbody.HTML(rnd))
		}
	}

	// ⑪-⑱ 八类形状各一组。
	add("形状-OpenAI错误对象", http.StatusBadRequest, randbody.JSONText(randbody.OpenAIError(rnd)))
	add("形状-深嵌套JSON", http.StatusUnprocessableEntity, randbody.JSONText(randbody.Doc(rnd, 5)))
	add("形状-平铺JSON", http.StatusConflict,
		randbody.JSONText(map[string]any{randbody.Word(rnd): randbody.Text(rnd, 1, 50)}))
	add("形状-顶层数组", http.StatusBadGateway, randbody.JSONText([]any{randbody.Doc(rnd, 2), randbody.Text(rnd, 3, 20)}))
	// 结尾刻意带换行：首尾空白也是原文的一部分，不能被 TrimSpace 静默吃掉。
	add("形状-纯文本(带首尾空白)", http.StatusBadRequest, "\n upstream says: "+randbody.Text(rnd, 10, 200)+"\n")
	add("形状-HTML错误页", http.StatusInternalServerError, randbody.HTML(rnd))
	add("形状-空body", http.StatusBadRequest, "")
	add("形状-超长", http.StatusBadRequest,
		randbody.JSONText(map[string]any{"error": map[string]any{
			"message":          randbody.Text(rnd, 40000, 60000),
			randbody.Word(rnd): randbody.Doc(rnd, 2),
		}}))
	return cases
}

// assertProbeBodyVisible 断言运营可见的探活原文与上游原文**逐字节**一致（超长则前缀 + 显式标注）。
func assertProbeBodyVisible(t *testing.T, what, got, upstream, cred string) {
	t.Helper()
	want := errbody.MaskSecrets(upstream, cred)
	if len(want) <= probeErrBodyMaxBytes {
		if got != want {
			t.Fatalf("%s 必须是上游原文（逐字节）\nwant(len=%d)=%q\ngot (len=%d)=%q",
				what, len(want), headBytes(want, 300), len(got), headBytes(got, 300))
		}
		return
	}
	idx := strings.Index(got, "\n…（已截断")
	if idx < 0 {
		t.Fatalf("%s 超长必须带显式截断标注（同字段），got=%q", what, headBytes(got, 300))
	}
	if kept := got[:idx]; !strings.HasPrefix(want, kept) {
		t.Fatalf("%s 截断前缀必须与上游原文逐字节一致（kept=%d 字节）", what, len(kept))
	}
	if !strings.Contains(got[idx:], "字节") {
		t.Fatalf("%s 截断标注必须说明原始规模，got=%q", what, got[idx:])
	}
}

// headBytes 取前 n 字节（失败信息展示用）。
func headBytes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// newProbeBodyServer 起一个固定返回 status + body 的上游。
func newProbeBodyServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(status)
		if body != "" {
			_, _ = w.Write([]byte(body))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestProbeModelNow_ExposesUpstreamRawBody 手动模型探测：响应里必须能拿到上游原文，
// 且 probe_logs.error_body 存的就是同一份原文。
func TestProbeModelNow_ExposesUpstreamRawBody(t *testing.T) {
	seed := time.Now().UnixNano()
	rnd := rand.New(rand.NewSource(seed))
	t.Logf("随机种子 seed=%d（失败可用它复现）", seed)

	for _, tc := range randomProbeCases(rnd) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := newProbeBodyServer(t, tc.status, tc.body)
			mgr, mock := newKeyProbeTestManager(t, srv.URL, HealthProbeConfig{FailThreshold: 2, TimeoutMS: 2000},
				[]ChannelModel{{ID: 21, InternalModelID: "m1", ExternalModelID: 1, State: StateNormal}})
			addKeyRuntime(t, mgr, 11, testKeyACred, StateNormal)

			var stored argString
			mock.ExpectExec(regexp.QuoteMeta(insertProbeLogSQL)).
				WithArgs(sqlmock.AnyArg(), int64(11), "m1", "model", srv.URL+"/chat/completions",
					false, sqlmock.AnyArg(), tc.status, &stored, 0, 0, 0, 0, sqlmock.AnyArg()).
				WillReturnResult(sqlmock.NewResult(1, 1))

			out, err := mgr.ProbeModelNow(1, 21)
			if err != nil {
				t.Fatalf("ProbeModelNow: %v", err)
			}
			if out.OK {
				t.Fatalf("上游 %d 不应判定探测成功: %+v", tc.status, out)
			}
			if out.UpstreamStatus != tc.status {
				t.Fatalf("ProbeOutcome.UpstreamStatus want %d, got %d", tc.status, out.UpstreamStatus)
			}
			assertProbeBodyVisible(t, "ProbeOutcome.ErrorBody", out.ErrorBody, tc.body, testKeyACred)

			if err := mock.ExpectationsWereMet(); err != nil {
				t.Fatalf("probe_logs 落库: %v", err)
			}
			if !stored.set {
				t.Fatal("probe_logs.error_body 未写入（运营事后翻不到原文）")
			}
			if stored.got != out.ErrorBody {
				t.Fatalf("probe_logs.error_body 必须与返回给运营的原文一致\nresp=%q\nlogs=%q",
					headBytes(out.ErrorBody, 200), headBytes(stored.got, 200))
			}
		})
	}
}

// TestProbeKeyNow_ExposesUpstreamRawBody 手动密钥探测（运营新建密钥后点的第一个按钮）同样要带原文。
func TestProbeKeyNow_ExposesUpstreamRawBody(t *testing.T) {
	seed := time.Now().UnixNano() + 1
	rnd := rand.New(rand.NewSource(seed))
	t.Logf("随机种子 seed=%d（失败可用它复现）", seed)

	for _, tc := range randomProbeCases(rnd) {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			srv := newProbeBodyServer(t, tc.status, tc.body)
			mgr, mock := newKeyProbeTestManager(t, srv.URL, HealthProbeConfig{FailThreshold: 2, TimeoutMS: 2000},
				[]ChannelModel{{ID: 21, InternalModelID: "m1", ExternalModelID: 1, State: StateNormal}})
			addKeyRuntime(t, mgr, 11, testKeyACred, StateNormal)

			mock.ExpectExec(regexp.QuoteMeta(insertProbeLogSQL)).
				WithArgs(sqlmock.AnyArg(), int64(11), "m1", "model", srv.URL+"/chat/completions",
					false, sqlmock.AnyArg(), tc.status, sqlmock.AnyArg(), 0, 0, 0, 0, sqlmock.AnyArg()).
				WillReturnResult(sqlmock.NewResult(1, 1))

			out, err := mgr.ProbeKeyNow(11)
			if err != nil {
				t.Fatalf("ProbeKeyNow: %v", err)
			}
			if out.OK {
				t.Fatalf("上游 %d 不应判定探测成功: %+v", tc.status, out)
			}
			if out.UpstreamStatus != tc.status {
				t.Fatalf("ProbeOutcome.UpstreamStatus want %d, got %d", tc.status, out.UpstreamStatus)
			}
			assertProbeBodyVisible(t, "ProbeOutcome.ErrorBody", out.ErrorBody, tc.body, testKeyACred)
			if out.Error == "" {
				t.Fatal("Error 仍要有可读短文案（载体是 ErrorBody，不是把短文案去掉）")
			}
		})
	}
}

// TestProbeErrorBody_MasksOnlyExactCredential 只遮蔽**本次实际使用的凭据原值**：
// 上游把收到的 key 回显在报文里时不能落盘，但同一报文里的其它内容（含长随机串/request-id/
// 模型名/URL query）必须逐字节保留——历史「按样式清洗」正是把后者一并抹掉的元凶。
func TestProbeErrorBody_MasksOnlyExactCredential(t *testing.T) {
	rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
	// 只用字母/数字：断言要按**字面量**比对，混入引号会让 JSON 转义参与进来，反而干扰判据。
	reqID := strings.ToUpper(randbody.Word(rnd)) + "-" + randbody.Word(rnd)
	modelName := randbody.Word(rnd) + "-" + randbody.Word(rnd)
	body := fmt.Sprintf(`{"error":{"message":"invalid api key %s","request_id":%q,"model":%q,`+
		`"url":"https://api.example.com/v1/x?api-version=2024-10-01"}}`,
		testKeyACred, reqID, modelName)

	srv := newProbeBodyServer(t, http.StatusUnauthorized, body)
	mgr, mock := newKeyProbeTestManager(t, srv.URL, HealthProbeConfig{FailThreshold: 2, TimeoutMS: 2000},
		[]ChannelModel{{ID: 21, InternalModelID: "m1", ExternalModelID: 1, State: StateNormal}})
	addKeyRuntime(t, mgr, 11, testKeyACred, StateNormal)
	mock.ExpectExec(regexp.QuoteMeta(insertProbeLogSQL)).WillReturnResult(sqlmock.NewResult(1, 1))

	out, err := mgr.ProbeModelNow(1, 21)
	if err != nil {
		t.Fatalf("ProbeModelNow: %v", err)
	}
	if strings.Contains(out.ErrorBody, testKeyACred) {
		t.Fatalf("凭据不得回显: %q", out.ErrorBody)
	}
	// 遮蔽只发生在凭据那一段：其余字节逐字节保留。
	want := strings.ReplaceAll(body, testKeyACred, "***")
	if out.ErrorBody != want {
		t.Fatalf("除凭据外必须逐字节保留原文\nwant=%q\ngot =%q", want, out.ErrorBody)
	}
	for _, keep := range []string{reqID, modelName, "api-version=2024-10-01"} {
		if !strings.Contains(out.ErrorBody, keep) {
			t.Fatalf("不得按样式/白名单清洗：%q 被抹掉了（got=%q）", keep, out.ErrorBody)
		}
	}
}

// newProbeHandler 装配 Handler + Manager + KeyService（同一 sqlmock db），渠道 base_url 指向 upstreamURL。
func newProbeHandler(t *testing.T, upstreamURL string) (*Handler, *Manager, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	nowFn := func() time.Time { return now }
	ch := &Channel{
		ID: 1, Name: "c1", Protocol: ProtocolOpenAICompat, BaseURL: upstreamURL,
		RateLimit:   RateLimitConfig{MaxConcurrent: 8},
		HealthProbe: HealthProbeConfig{FailThreshold: 2, TimeoutMS: 2000},
		Reliability: ReliabilityConfig{WindowSeconds: 60, MinSamples: 10},
		CreatedAt:   now, UpdatedAt: now,
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE deleted_at IS NULL ORDER BY priority DESC, id ASC`)).
		WillReturnRows(channelRow(ch))

	enc, err := crypto.SM4Encrypt(testSM4Key, []byte(testKeyACred))
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE deleted_at IS NULL ORDER BY channel_id, id`)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "channel_id", "name", "credential_enc", "state", "last_err", "deleted_at", "created_at", "updated_at"}).
			AddRow(int64(11), int64(1), "k1", enc, string(StateNormal), "", nil, now, now))

	mgr := NewManager(NewStore(db), NewChannelModelStore(db), testSM4Key, &http.Client{Timeout: 3 * time.Second}, nil, nowFn, NewKeyStore(db))
	if err := mgr.SyncFromDB(context.Background()); err != nil {
		t.Fatalf("SyncFromDB: %v", err)
	}
	cm := &ChannelModel{ID: 21, ChannelID: 1, InternalModelID: "m1", ExternalModelID: 1, State: StateNormal}
	mgr.mu.Lock()
	mgr.mrt[1] = map[int64]*ModelRuntime{21: mgr.buildModelRuntime(cm, mgr.rt[1])}
	mgr.mu.Unlock()

	keySvc := NewKeyService(NewKeyStore(db), testSM4Key)
	h := NewHandler(nil)
	h.SetKeyDeps(keySvc, mgr)
	return h, mgr, mock
}

// TestHandleProbeEndpoints_ExposeUpstreamRawBody HTTP 级：两个探活端点返回体里逐字节带上游原文
// （走真实 handler → resp.Write 序列化链路，避免只测内部函数漏掉「响应组装把原文转义/裁剪」）。
func TestHandleProbeEndpoints_ExposeUpstreamRawBody(t *testing.T) {
	seed := time.Now().UnixNano() + 2
	rnd := rand.New(rand.NewSource(seed))
	t.Logf("随机种子 seed=%d", seed)

	type probeResp struct {
		Code int          `json:"Code"`
		Data ProbeOutcome `json:"Data"`
	}
	for i := 0; i < 3; i++ {
		status := []int{http.StatusBadRequest, http.StatusInternalServerError, http.StatusUnprocessableEntity}[i]
		body := randbody.HTML(rnd)
		if i == 1 {
			body = randbody.JSONText(randbody.OpenAIError(rnd))
		}
		srv := newProbeBodyServer(t, status, body)

		t.Run(fmt.Sprintf("模型探测-%d", i), func(t *testing.T) {
			h, _, mock := newProbeHandler(t, srv.URL)
			mock.ExpectExec(regexp.QuoteMeta(insertProbeLogSQL)).WillReturnResult(sqlmock.NewResult(1, 1))

			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/channels/1/models/21/probe", nil)
			req.SetPathValue("id", "1")
			req.SetPathValue("mid", "21")
			rec := httptest.NewRecorder()
			h.HandleProbeModel(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
			}
			var got probeResp
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v (%s)", err, rec.Body.String())
			}
			if got.Data.UpstreamStatus != status {
				t.Fatalf("UpstreamStatus want %d, got %d", status, got.Data.UpstreamStatus)
			}
			assertProbeBodyVisible(t, "探活响应 Data.ErrorBody", got.Data.ErrorBody, body, testKeyACred)
		})

		t.Run(fmt.Sprintf("密钥探测-%d", i), func(t *testing.T) {
			h, _, mock := newProbeHandler(t, srv.URL)
			mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE id=$1 AND deleted_at IS NULL`)).
				WithArgs(int64(11)).
				WillReturnRows(channelKeyRow(&ChannelKey{ID: 11, ChannelID: 1, Name: "k1", State: StateNormal, CreatedAt: time.Now(), UpdatedAt: time.Now()}))
			mock.ExpectExec(regexp.QuoteMeta(insertProbeLogSQL)).WillReturnResult(sqlmock.NewResult(1, 1))

			req := httptest.NewRequest(http.MethodPost, "/api/v1/admin/channels/1/keys/11/probe", nil)
			req.SetPathValue("id", "1")
			req.SetPathValue("kid", "11")
			rec := httptest.NewRecorder()
			h.HandleProbeKey(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
			}
			var got probeResp
			if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
				t.Fatalf("decode: %v (%s)", err, rec.Body.String())
			}
			if got.Data.UpstreamStatus != status {
				t.Fatalf("UpstreamStatus want %d, got %d", status, got.Data.UpstreamStatus)
			}
			assertProbeBodyVisible(t, "探活响应 Data.ErrorBody", got.Data.ErrorBody, body, testKeyACred)
		})
	}
}

// TestHandleListProbeLogs_ReturnsStoredRawBody HTTP 级：探活历史接口把 probe_logs.error_body 原文原样返回。
func TestHandleListProbeLogs_ReturnsStoredRawBody(t *testing.T) {
	rnd := rand.New(rand.NewSource(time.Now().UnixNano() + 3))
	body := randbody.JSONText(randbody.OpenAIError(rnd))

	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewService(NewStore(db))
	svc.SetChannelModelStore(NewChannelModelStore(db))
	svc.SetKeyService(NewKeyService(NewKeyStore(db), testSM4Key))
	h := NewHandler(svc)

	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE id = $1 AND deleted_at IS NULL`)).
		WithArgs(int64(1)).
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "ch", Protocol: ProtocolOpenAICompat,
			BaseURL: "https://a.example.com", CreatedAt: now, UpdatedAt: now}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE channel_id=$1 AND deleted_at IS NULL ORDER BY id`)).
		WithArgs(int64(1)).
		WillReturnRows(channelKeyRow(&ChannelKey{ID: 11, ChannelID: 1, Name: "k1", State: StateNormal,
			CreatedAt: now, UpdatedAt: now}))

	probeLogsSQL := `SELECT id, channel_key_id, model_id, level, target, ok, error, upstream_status, error_body, input_tokens, output_tokens, cached_tokens, total_tokens, duration_ms, created_at
		 FROM probe_logs WHERE channel_key_id = ANY($1::bigint[]) ORDER BY id DESC LIMIT $2`
	plCols := []string{"id", "channel_key_id", "model_id", "level", "target", "ok", "error",
		"upstream_status", "error_body", "input_tokens", "output_tokens", "cached_tokens", "total_tokens",
		"duration_ms", "created_at"}
	mock.ExpectQuery(regexp.QuoteMeta(probeLogsSQL)).
		WithArgs("{11}", 100).
		WillReturnRows(sqlmock.NewRows(plCols).
			AddRow(int64(1), int64(11), "m1", "model", "https://a.example.com/chat/completions",
				false, "probe http status: 400", 400, body, 0, 0, 0, 0, 12, now))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/channels/1/probe-logs", nil)
	req.SetPathValue("id", "1")
	rec := httptest.NewRecorder()
	h.HandleListProbeLogs(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Data []ProbeLog `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, rec.Body.String())
	}
	if len(got.Data) != 1 {
		t.Fatalf("want 1 条探活历史, got %d", len(got.Data))
	}
	if got.Data[0].ErrorBody != body {
		t.Fatalf("探活历史必须原样返回上游原文\nwant=%q\ngot =%q", body, got.Data[0].ErrorBody)
	}
	if got.Data[0].UpstreamStatus != http.StatusBadRequest {
		t.Fatalf("探活历史必须带上游状态码, got %d", got.Data[0].UpstreamStatus)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestHandleListProbeLogs_BoundsPayload 探活列表必须收口体积：
// 历史实现允许 Limit=500 且每行 error_body 原样 32KB → 单次响应可达十几兆。
// 现在①行数上限 100（Limit=500 也只取 100）②每行原文按 4KB 预览 + 同字段标注（完整原文仍在 probe_logs.error_body）。
func TestHandleListProbeLogs_BoundsPayload(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	svc := NewService(NewStore(db))
	svc.SetChannelModelStore(NewChannelModelStore(db))
	svc.SetKeyService(NewKeyService(NewKeyStore(db), testSM4Key))
	h := NewHandler(svc)

	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelCols + ` FROM channels WHERE id = $1 AND deleted_at IS NULL`)).
		WithArgs(int64(1)).
		WillReturnRows(channelRow(&Channel{ID: 1, Name: "ch", Protocol: ProtocolOpenAICompat,
			BaseURL: "https://a.example.com", CreatedAt: now, UpdatedAt: now}))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + channelKeyCols + ` FROM channel_keys WHERE channel_id=$1 AND deleted_at IS NULL ORDER BY id`)).
		WithArgs(int64(1)).
		WillReturnRows(channelKeyRow(&ChannelKey{ID: 11, ChannelID: 1, Name: "k1", State: StateNormal,
			CreatedAt: now, UpdatedAt: now}))

	probeLogsSQL := `SELECT id, channel_key_id, model_id, level, target, ok, error, upstream_status, error_body, input_tokens, output_tokens, cached_tokens, total_tokens, duration_ms, created_at
		 FROM probe_logs WHERE channel_key_id = ANY($1::bigint[]) ORDER BY id DESC LIMIT $2`
	plCols := []string{"id", "channel_key_id", "model_id", "level", "target", "ok", "error",
		"upstream_status", "error_body", "input_tokens", "output_tokens", "cached_tokens", "total_tokens",
		"duration_ms", "created_at"}
	hugeBody := strings.Repeat("x", 30<<10)
	// Limit=500 必须被收敛到 100（否则 500 行 × 32KB ≈ 16MB）。
	mock.ExpectQuery(regexp.QuoteMeta(probeLogsSQL)).
		WithArgs("{11}", 100).
		WillReturnRows(sqlmock.NewRows(plCols).
			AddRow(int64(1), int64(11), "m1", "model", "https://a.example.com/chat/completions",
				false, "probe http status: 400", 400, hugeBody, 0, 0, 0, 0, 12, now))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/admin/channels/1/probe-logs?Limit=500", nil)
	req.SetPathValue("id", "1")
	rec := httptest.NewRecorder()
	h.HandleListProbeLogs(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("want 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var got struct {
		Data []ProbeLog `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Data) != 1 {
		t.Fatalf("want 1 行, got %d", len(got.Data))
	}
	if n := len(got.Data[0].ErrorBody); n > probeListBodyMaxBytes {
		t.Fatalf("列表里每行原文必须 ≤ %d 字节, got %d", probeListBodyMaxBytes, n)
	}
	if !strings.Contains(got.Data[0].ErrorBody, "已截断") {
		t.Fatalf("截断必须在同一字段标注, got %q", got.Data[0].ErrorBody[len(got.Data[0].ErrorBody)-80:])
	}
	if len(rec.Body.Bytes()) > 64<<10 {
		t.Fatalf("单次列表响应必须收口到 64KB 量级, got %d 字节", len(rec.Body.Bytes()))
	}
}
