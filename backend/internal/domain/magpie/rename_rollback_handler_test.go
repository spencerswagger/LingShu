// rename_rollback_handler_test.go 锁 M2 端点行为（契约定稿 §1.2 + 终表 §3.5）：
//
//	POST /api/v1/admin/tokens/magpie-keys/rename          —— dry-run 默认开启，?dry_run=false 才执行；
//	                                                         消歧仍撞 → 409（挂起人工，明细在 Failed）。
//	POST /api/v1/admin/tokens/magpie-keys/rename-rollback —— dry-run 默认开启；执行=按 legacy_key_name
//	                                                         复原旧名（三步镜像、幂等可重跑）。
package magpie

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// newKeyAdminForTest 装配带 fake store 与 fake 同步器的处理器。
func newKeyAdminForTest(t *testing.T, fk *fakeKeyStore, sync *CallerKeySync) (*KeyAdminHandler, http.Handler) {
	t.Helper()
	h := NewKeyAdminHandler(fk, testCredKey, "")
	if sync != nil {
		h.SetAutoSync(func(ctx context.Context, tokenID int64) (string, error) { return "s", nil }, sync)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/admin/tokens/magpie-keys/rename-rollback", h.HandleRenameRollback)
	return h, mux
}

func postJSON(t *testing.T, h http.Handler, path string, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(http.MethodPost, path, rdr)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func decBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var m struct {
		Data map[string]any `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应不是 JSON：%v", err)
	}
	if m.Data == nil {
		t.Fatalf("响应缺 Data 段：%s", rec.Body.String())
	}
	return m.Data
}

// TestRenameRollback_DryRunDefault 只列目标不改状态（默认 dry-run）。
func TestRenameRollback_DryRunDefault(t *testing.T) {
	ctx := context.Background()
	fk := newFakeKeyStore()
	// 预置一个「已改名」行：新名 zhangsan-bot，legacy=ls-token-7。
	if _, err := fk.Provision(ctx, 7, "zhangsan-bot", "cipher"); err != nil {
		t.Fatal(err)
	}
	_, _ = fk.RenameAndActivate(ctx, 7, "ls-token-7", "zhangsan-bot", "cipher")
	_, mux := newKeyAdminForTest(t, fk, nil) // 无 sync：dry-run 不应需要

	rec := postJSON(t, mux, "/api/v1/admin/tokens/magpie-keys/rename-rollback", "")
	m := decBody(t, rec)
	if m["DryRun"] != true {
		t.Fatalf("dry-run 应默认开启：%v", m)
	}
	targets, _ := m["Targets"].([]any)
	if len(targets) != 1 {
		t.Fatalf("目标数不符：%v", m["Targets"])
	}
	tg, _ := targets[0].(map[string]any)
	if tg["CurrentName"] != "zhangsan-bot" || tg["LegacyName"] != "ls-token-7" {
		t.Fatalf("目标行不符：%v", tg)
	}
	if m["Restored"] != float64(0) {
		t.Fatalf("dry-run 不应执行：%v", m["Restored"])
	}
	// 状态未变。
	k, _ := fk.ByTokenID(ctx, 7)
	if k.KeyName != "zhangsan-bot" || k.LegacyKeyName != "ls-token-7" {
		t.Fatalf("dry-run 不应改动状态：%+v", k)
	}
}

// TestRenameRollback_Execute ?dry_run=false 执行：旧名复原 + legacy 清空。
func TestRenameRollback_Execute(t *testing.T) {
	ctx := context.Background()
	srv, _ := fakeMagpiePage(t, syncTestPageKey)
	fk := newFakeKeyStore()
	cipher, _ := encryptForTest("secret-xyz")
	fk.Provision(ctx, 7, "zhangsan-bot", cipher)
	_, _ = fk.RenameAndActivate(ctx, 7, "ls-token-7", "zhangsan-bot", cipher)
	sync := newSyncForTest(t, srv.URL, fk)
	_, mux := newKeyAdminForTest(t, fk, sync)

	rec := postJSON(t, mux, "/api/v1/admin/tokens/magpie-keys/rename-rollback?dry_run=false", "")
	m := decBody(t, rec)
	if m["DryRun"] != false || m["Restored"] != float64(1) {
		t.Fatalf("执行结果不符：%v", m)
	}
	k, _ := fk.ByTokenID(ctx, 7)
	if k.KeyName != "ls-token-7" || k.LegacyKeyName != "" {
		t.Fatalf("执行后终态不符：%+v", k)
	}
}

// TestRenameRollback_BodyExplicitFalse 请求体 {"DryRun":false} 也可执行（显式指针区分未传）。
func TestRenameRollback_BodyExplicitFalse(t *testing.T) {
	ctx := context.Background()
	srv, _ := fakeMagpiePage(t, syncTestPageKey)
	fk := newFakeKeyStore()
	cipher, _ := encryptForTest("secret-xyz")
	fk.Provision(ctx, 7, "zhangsan-bot", cipher)
	_, _ = fk.RenameAndActivate(ctx, 7, "ls-token-7", "zhangsan-bot", cipher)
	sync := newSyncForTest(t, srv.URL, fk)
	_, mux := newKeyAdminForTest(t, fk, sync)

	rec := postJSON(t, mux, "/api/v1/admin/tokens/magpie-keys/rename-rollback", `{"DryRun":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("body 显式 false 应执行：%d %s", rec.Code, rec.Body.String())
	}
	k, _ := fk.ByTokenID(ctx, 7)
	if k.KeyName != "ls-token-7" {
		t.Fatalf("执行后 key_name 应为旧名：%+v", k)
	}
}

// TestRenameRollback_ExecuteNeedsSync 执行但未装配 sync → 503（不是 404）。
func TestRenameRollback_ExecuteNeedsSync(t *testing.T) {
	fk := newFakeKeyStore()
	fk.Provision(context.Background(), 7, "zhangsan-bot", "c")
	_, mux := newKeyAdminForTest(t, fk, nil)
	rec := postJSON(t, mux, "/api/v1/admin/tokens/magpie-keys/rename-rollback?dry_run=false", "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("缺 sync 执行应 503：%d", rec.Code)
	}
}

// TestPageAdminRetryOn429 doJSON 对 429/5xx 按退避序列重试，2xx 后成功；
// 非重试码（400）不重试直接失败。
func TestPageAdminRetryOn429(t *testing.T) {
	// 缩短退避，避免测试等待真实 5s/20s。
	orig := pageRetryBackoff
	pageRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { pageRetryBackoff = orig })

	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" { // login
			w.WriteHeader(http.StatusOK)
			return
		}
		n++
		if n <= 2 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":"rate limited"}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	a := NewPageAdmin(srv.URL, "k", nil)
	var out map[string]any
	if err := a.doJSON(context.Background(), http.MethodGet, "/api/x", nil, &out); err != nil {
		t.Fatalf("两次 429 后应重试成功：%v", err)
	}
	if out["ok"] != true || n != 3 {
		t.Fatalf("重试次数/结果不符：n=%d out=%v", n, out)
	}
}

// TestPageAdminRetryExhausted 退避 3 次用尽仍 429 → 报错（不无限重试）。
func TestPageAdminRetryExhausted(t *testing.T) {
	orig := pageRetryBackoff
	pageRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { pageRetryBackoff = orig })

	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		n++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"rate limited"}`))
	}))
	defer srv.Close()

	a := NewPageAdmin(srv.URL, "k", nil)
	err := a.doJSON(context.Background(), http.MethodGet, "/api/x", nil, nil)
	if err == nil {
		t.Fatal("重试用尽应报错")
	}
	if n != 4 { // 首次 + 3 次重试
		t.Fatalf("总请求数应为 4：%d", n)
	}
}

// TestPageAdminNoRetryOn4xx 400 不在退避名单（只有 429 与 5xx），一次即失败。
func TestPageAdminNoRetryOn4xx(t *testing.T) {
	orig := pageRetryBackoff
	pageRetryBackoff = []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond}
	t.Cleanup(func() { pageRetryBackoff = orig })

	n := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		n++
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	a := NewPageAdmin(srv.URL, "k", nil)
	if err := a.doJSON(context.Background(), http.MethodGet, "/api/x", nil, nil); err == nil {
		t.Fatal("400 应直接失败")
	}
	if n != 1 {
		t.Fatalf("400 不应重试：%d", n)
	}
}

// TestPageAdminRetryContextCancel 退避等待期间 ctx 取消 → 立即中止。
func TestPageAdminRetryContextCancel(t *testing.T) {
	orig := pageRetryBackoff
	pageRetryBackoff = []time.Duration{50 * time.Millisecond, 50 * time.Millisecond, 50 * time.Millisecond}
	t.Cleanup(func() { pageRetryBackoff = orig })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	a := NewPageAdmin(srv.URL, "k", nil)
	if err := a.doJSON(ctx, http.MethodGet, "/api/x", nil, nil); err == nil {
		t.Fatal("ctx 取消应报错")
	}
}

// TestEnsureNameUniqueTriesLimit 消歧候选序列恰为 -2…-9（maxTries=8），不含 -10+。
func TestEnsureNameUniqueTriesLimit(t *testing.T) {
	s := newSyncForTest(t, mustFakePage(t), newFakeKeyStore())
	fk := s.keys.(*fakeKeyStore)
	// 占掉基础名与 -2 到 -9（8 个候选全满）→ 耗尽报 ErrNameUnavailable。
	fk.occupied["zhangsan-bot"] = true
	for i := 2; i <= 9; i++ {
		fk.occupied[fmt.Sprintf("zhangsan-bot-%d", i)] = true
	}
	got, err := s.EnsureNameUnique(context.Background(), 12, 7, "zhangsan", "bot")
	if got != "" || !errors.Is(err, ErrNameUnavailable) {
		t.Fatalf("8 候选全撞应 ErrNameUnavailable：got=%q err=%v", got, err)
	}
	// 放开 -9 → 得到 -9（最后一个候选也可用）。
	delete(fk.occupied, "zhangsan-bot-9")
	got, err = s.EnsureNameUnique(context.Background(), 12, 7, "zhangsan", "bot")
	if err != nil || got != "zhangsan-bot-9" {
		t.Fatalf("放开最后候选应可用：%q err=%v", got, err)
	}
}
