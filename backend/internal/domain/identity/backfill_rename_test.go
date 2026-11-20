// backfill_rename_test.go 锁存量改名回填的三条硬口径（服务层，sqlmock 供数）：
//  1. 改名资格是确定性映射（当前名 == 旧自动名 ls-token-<id>），不猜名、不按样本特判；
//  2. dry-run 只判定不执行（无代理请求、无写库）；
//  3. 手工绑定名 / 已是新名 / 无密文 各归各的 skip 桶，不混、不产生重复数据。
package identity

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/pkg/crypto"
)

// fakeSyncer 记录 Ensure/Reactivate/ProbeName 调用（回填执行断言），可注入失败。
type fakeSyncer struct {
	mu             sync.Mutex
	calls          []string // "uid:tokid:disp:username"（Ensure）/ "R uid:tokid:disp"（Reactivate）
	fail           map[int64]error
	probeTaken     map[int64]error // ProbeName 对这些令牌返回注入错误（dry-run 探 Conflict）
	EnsureDisabled bool            // true 时 Ensure 返回 ErrCallerKeyDisabled（模拟自动路径守卫）
}

func (f *fakeSyncer) Ensure(_ context.Context, userID int64, tok CallerKeyToken, username, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.EnsureDisabled {
		return ErrCallerKeyDisabled
	}
	if err, ok := f.fail[tok.ID]; ok {
		return err
	}
	f.calls = append(f.calls, fmt.Sprintf("%d:%d:%s:%s", userID, tok.ID, tok.DisplayName, username))
	return nil
}

func (f *fakeSyncer) Remove(context.Context, int64) error { return nil }

// ProbeName dry-run 冲突预演：默认按「令牌名转写」给出拟分配名；
// probeTaken 里的令牌返回 ErrCallerKeyNameTaken（探出 Conflict 的断言源）。
func (f *fakeSyncer) ProbeName(_ context.Context, userID int64, tok CallerKeyToken, username string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.probeTaken[tok.ID]; ok {
		return "", err
	}
	return fmt.Sprintf("%d:%d:%s:%s", userID, tok.ID, tok.DisplayName, username), nil
}

// Reactivate 显式启用路径（Toggle 启用侧）；回填用例里不应被调用（停用行走分桶）。
func (f *fakeSyncer) Reactivate(_ context.Context, userID int64, tok CallerKeyToken, _ string, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf("R%d:%d:%s", userID, tok.ID, tok.DisplayName))
	return nil
}

const listTokensPageSQL = `SELECT ` + tokenInfoCols + ` FROM tokens t JOIN users u ON u.id = t.user_id
		 WHERE t.deleted_at IS NULL AND u.deleted_at IS NULL
		   AND ($1::bigint IS NULL OR t.user_id = $1) AND ($2 = '' OR t.status = $2)
		 ORDER BY t.id DESC LIMIT $3 OFFSET $4`

func newBackfillSvc(t *testing.T, ids []int64, names map[int64]string, secrets map[int64]string) (*TokenService, sqlmock.Sqlmock, *fakeSyncer) {
	t.Helper()
	var err2 error
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	// count(*) 一页断言（WHERE 子句完整形状）。
	mock.ExpectQuery(`SELECT count\(\*\) FROM tokens t JOIN users u ON u.id = t.user_id WHERE t.deleted_at IS NULL AND u.deleted_at IS NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(len(ids))))
	rows := sqlmock.NewRows([]string{"t.id", "t.token_hash", "t.token_display", "t.user_id", "t.display_name",
		"t.expires_at", "t.last_used_at", "t.status", "t.created_at", "u.username", "u.nickname"})
	for _, id := range ids {
		uid := int64(100 + id%3)
		rows.AddRow(id, fmt.Sprintf("hash-%d", id), fmt.Sprintf("disp-%d", id), uid,
			fmt.Sprintf("name-%d", id), nil, nil, "ACTIVE", time.Now(), fmt.Sprintf("user-%d", uid), "")
	}
	mock.ExpectQuery(regexp.QuoteMeta(listTokensPageSQL)).WithArgs(nil, "", 50, 0).
		WillReturnRows(rows)

	// 每个令牌的密文查询（Secret 读取）：密文必须是真实 SM4（Secret 走 SM4Decrypt）。
	for _, id := range ids {
		cipher := ""
		if secrets[id] != "" {
			cipher, err2 = crypto.SM4Encrypt([]byte("0123456789abcdef"), []byte(secrets[id]))
			if err2 != nil {
				t.Fatalf("sm4: %v", err2)
			}
		}
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(secret_cipher, '') FROM tokens WHERE id = $1 AND deleted_at IS NULL`)).
			WithArgs(id).
			WillReturnRows(sqlmock.NewRows([]string{"cipher"}).AddRow(cipher))
	}

	svc := NewTokenService(NewTokenStore(db))
	svc.SetSecretKey([]byte("0123456789abcdef")) // Secret 需要 SM4 密钥解密密文
	fs := &fakeSyncer{fail: map[int64]error{}, probeTaken: map[int64]error{}}
	svc.SetCallerKeySyncer(fs)
	svc.SetCallerKeyNameOf(func(ctx context.Context, tokenID int64) string { return names[tokenID] })
	svc.SetCallerWantNameOf(func(ctx context.Context, userID, tokenID int64, displayName string) string {
		return fmt.Sprintf("want-u%d-t%d", userID, tokenID)
	})
	return svc, mock, fs
}

// newBackfillSvcStates 同 newBackfillSvc，额外注入映射行状态（disabled 分桶用例）。
func newBackfillSvcStates(t *testing.T, ids []int64, names, secrets, states map[int64]string) (*TokenService, sqlmock.Sqlmock, *fakeSyncer) {
	t.Helper()
	var err2 error
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	mock.ExpectQuery(`SELECT count\(\*\) FROM tokens t JOIN users u ON u.id = t.user_id WHERE t.deleted_at IS NULL AND u.deleted_at IS NULL`).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(len(ids))))
	rows := sqlmock.NewRows([]string{"t.id", "t.token_hash", "t.token_display", "t.user_id", "t.display_name",
		"t.expires_at", "t.last_used_at", "t.status", "t.created_at", "u.username", "u.nickname"})
	for _, id := range ids {
		uid := int64(100 + id%3)
		rows.AddRow(id, fmt.Sprintf("hash-%d", id), fmt.Sprintf("disp-%d", id), uid,
			fmt.Sprintf("name-%d", id), nil, nil, "ACTIVE", time.Now(), fmt.Sprintf("user-%d", uid), "")
	}
	mock.ExpectQuery(regexp.QuoteMeta(listTokensPageSQL)).WithArgs(nil, "", 50, 0).
		WillReturnRows(rows)
	for _, id := range ids {
		if states[id] == "DISABLED" {
			continue // 停用行走分桶：不查密文（省一次往返，行为与实现一致）
		}
		cipher := ""
		if secrets[id] != "" {
			cipher, err2 = crypto.SM4Encrypt([]byte("0123456789abcdef"), []byte(secrets[id]))
			if err2 != nil {
				t.Fatalf("sm4: %v", err2)
			}
		}
		mock.ExpectQuery(regexp.QuoteMeta(`SELECT COALESCE(secret_cipher, '') FROM tokens WHERE id = $1 AND deleted_at IS NULL`)).
			WithArgs(id).
			WillReturnRows(sqlmock.NewRows([]string{"cipher"}).AddRow(cipher))
	}

	svc := NewTokenService(NewTokenStore(db))
	svc.SetSecretKey([]byte("0123456789abcdef"))
	fs := &fakeSyncer{fail: map[int64]error{}, probeTaken: map[int64]error{}}
	svc.SetCallerKeySyncer(fs)
	svc.SetCallerKeyNameOf(func(ctx context.Context, tokenID int64) string { return names[tokenID] })
	svc.SetCallerKeyStateOf(func(ctx context.Context, tokenID int64) string { return states[tokenID] })
	svc.SetCallerWantNameOf(func(ctx context.Context, userID, tokenID int64, displayName string) string {
		return fmt.Sprintf("want-u%d-t%d", userID, tokenID)
	})
	return svc, mock, fs
}

// TestBackfill_DryRunBuckets dry-run 只分桶不执行：四类令牌各归各桶，Ensure 零调用。
func TestBackfill_DryRunBuckets(t *testing.T) {
	ids := []int64{1, 2, 3, 4} // 1=改名 2=已是新名 3=手工名 4=无密文
	names := map[int64]string{
		1: "ls-token-1",     // 旧自动名 → renamed
		2: "want-u102-t2",   // 已是新口径（uid=102）→ already-bound
		3: "ops-manual-key", // 手工绑定 → manual-named
	}
	secrets := map[int64]string{1: "s1", 2: "s2", 3: "s3"} // 4 无密文
	svc, mock, fs := newBackfillSvc(t, ids, names, secrets)

	created, renamed, skipped, conflicts, failed, err := svc.BackfillCallerKeys(context.Background(), 0, true)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if created != 0 || renamed != 0 {
		t.Fatalf("dry-run 不得执行：created=%d renamed=%d", created, renamed)
	}
	if len(fs.calls) != 0 {
		t.Fatalf("dry-run 不得触发代理请求：%v", fs.calls)
	}
	if skipped["renamed"] != 1 || skipped["already-bound"] != 1 || skipped["manual-named"] != 1 || skipped["no-cipher"] != 1 {
		t.Fatalf("分桶不符：%v (failed=%v)", skipped, failed)
	}
	if len(conflicts) != 0 {
		t.Fatalf("无注入撞名时 dry-run 不应探出冲突：%v", conflicts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestBackfill_ExecuteRenamesLegacyOnly 执行态：旧自动名与手工名**都会**进 Ensure
// （旧自动名 → 消歧改名；手工名 → 名字不动只对齐值），已是新名的提前 skip；
// 无密文进 failed。
func TestBackfill_ExecuteRenamesLegacyOnly(t *testing.T) {
	ids := []int64{1, 2, 3, 4}
	names := map[int64]string{1: "ls-token-1", 2: "want-u102-t2", 3: "ops-manual-key"}
	secrets := map[int64]string{1: "s1", 2: "s2", 3: "s3"}
	svc, mock, fs := newBackfillSvc(t, ids, names, secrets)

	created, renamed, skipped, conflicts, failed, err := svc.BackfillCallerKeys(context.Background(), 0, false)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	// uid = 100 + id%3 → 令牌1=101、令牌2=102、令牌3=100。
	// 执行态：令牌1（旧自动名→改名）与令牌3（手工名，Ensure 语义=名字不动只对齐值）进 Ensure；
	// 令牌2 已是新名提前 skip；令牌4 无密文 failed。
	if len(fs.calls) != 2 {
		t.Fatalf("Ensure 调用数不符：%v", fs.calls)
	}
	if fs.calls[0] != "101:1:name-1:user-101" || fs.calls[1] != "100:3:name-3:user-100" {
		t.Fatalf("Ensure 调用序不符（令牌1 改名 + 令牌3 手工名对齐，末段=username）：%v", fs.calls)
	}
	// already-bound 提前 skip（不走 Ensure）；manual 名由 Ensure 语义保证名字不动。
	if skipped["already-bound"] != 1 || skipped["manual-named"] != 0 {
		t.Fatalf("skip 桶不符：%v", skipped)
	}
	// 手工名对齐成功计 created=1；旧自动名改名计 renamed=1。
	if created != 1 || renamed != 1 {
		t.Fatalf("created=%d renamed=%d（应各 1：手工名对齐 + 旧名改名）", created, renamed)
	}
	if len(conflicts) != 0 {
		t.Fatalf("执行态无撞名注入不应有冲突：%v", conflicts)
	}
	if len(failed) != 1 || failed[0]["TokenID"] != int64(4) {
		t.Fatalf("无密文应进 failed：%v", failed)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestBackfill_IdempotentRerun 重跑幂等：第一次改名后当前名==新名，第二次全部 already-bound。
func TestBackfill_IdempotentRerun(t *testing.T) {
	ids := []int64{1}
	// 第一轮：旧名。
	svc1, mock1, fs1 := newBackfillSvc(t, ids, map[int64]string{1: "ls-token-1"}, map[int64]string{1: "s1"})
	c1, r1, sk1, cf1, f1, err := svc1.BackfillCallerKeys(context.Background(), 0, false)
	if err != nil || len(f1) != 0 {
		t.Fatalf("first: err=%v failed=%v", err, f1)
	}
	if c1 != 0 || r1 != 1 || sk1["renamed"] != 0 || len(cf1) != 0 {
		t.Fatalf("first: created=%d renamed=%d skipped=%v conflicts=%v", c1, r1, sk1, cf1)
	}
	_ = fs1
	if err := mock1.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
	// 第二轮：当前名已是新口径（Ensure 已推进）。
	svc2, mock2, _ := newBackfillSvc(t, ids, map[int64]string{1: "want-u101-t1"}, map[int64]string{1: "s1"})
	c2, r2, sk2, cf2, f2, err := svc2.BackfillCallerKeys(context.Background(), 0, false)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if c2 != 0 || r2 != 0 || sk2["already-bound"] != 1 || len(f2) != 0 || len(cf2) != 0 {
		t.Fatalf("second: created=%d renamed=%d skipped=%v failed=%v conflicts=%v", c2, r2, sk2, f2, cf2)
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
}

// newBackfillHandler 装配走 HTTP 层的 TokenHandler（rename 端点行为用例）。
func newBackfillHandler(t *testing.T, svc *TokenService) (*TokenHandler, http.Handler) {
	t.Helper()
	h := NewTokenHandler(svc, func(ctx context.Context) (int64, bool) { return 1, true })
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/admin/tokens/magpie-keys/rename", h.HandleAdminBackfillMagpieKeys)
	return h, mux
}

func postRename(t *testing.T, h http.Handler, rawURL, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rdr *bytes.Reader
	if body == "" {
		rdr = bytes.NewReader(nil)
	} else {
		rdr = bytes.NewReader([]byte(body))
	}
	req := httptest.NewRequest(http.MethodPost, rawURL, rdr)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestRenameHandler_DryRunDefault HTTP 层锁「dry-run 默认开启」：无参调用只分桶不执行。
func TestRenameHandler_DryRunDefault(t *testing.T) {
	ids := []int64{1}
	names := map[int64]string{1: "ls-token-1"}
	svc, _, fs := newBackfillSvc(t, ids, names, map[int64]string{1: "s1"})
	_, mux := newBackfillHandler(t, svc)

	rec := postRename(t, mux, "/api/v1/admin/tokens/magpie-keys/rename", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("默认 dry-run 应 200：%d %s", rec.Code, rec.Body.String())
	}
	var m struct {
		Data struct {
			DryRun  bool           `json:"DryRun"`
			Renamed int            `json:"Renamed"`
			Skipped map[string]int `json:"Skipped"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应解析：%v", err)
	}
	if !m.Data.DryRun {
		t.Fatalf("DryRun 应默认 true：%s", rec.Body.String())
	}
	if m.Data.Renamed != 0 || m.Data.Skipped["renamed"] != 1 {
		t.Fatalf("dry-run 只分桶不执行：%+v", m.Data)
	}
	if len(fs.calls) != 0 {
		t.Fatalf("dry-run 不应触发 Ensure：%v", fs.calls)
	}
}

// TestRenameHandler_ExecuteByQueryParam ?dry_run=false 才执行。
func TestRenameHandler_ExecuteByQueryParam(t *testing.T) {
	ids := []int64{1}
	names := map[int64]string{1: "ls-token-1"}
	svc, _, fs := newBackfillSvc(t, ids, names, map[int64]string{1: "s1"})
	_, mux := newBackfillHandler(t, svc)

	rec := postRename(t, mux, "/api/v1/admin/tokens/magpie-keys/rename?dry_run=false", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("执行应 200：%d %s", rec.Code, rec.Body.String())
	}
	if len(fs.calls) != 1 {
		t.Fatalf("执行应触发 Ensure：%v", fs.calls)
	}
}

// TestRenameHandler_Conflict409Details 消歧重试一轮仍撞 → 409（契约：挂起人工）。
// 评审必改：断言必须是**响应结构**而非文案子串——data.Failed[].TokenID / Conflict:true、
// data.Conflicts[].TokenID 都要真实可读（「409 挂人工」的闭环靠响应体给令牌号）。
func TestRenameHandler_Conflict409Details(t *testing.T) {
	ids := []int64{1}
	names := map[int64]string{1: "ls-token-1"}
	svc, _, fs := newBackfillSvc(t, ids, names, map[int64]string{1: "s1"})
	fs.fail[1] = ErrCallerKeyNameTaken // 注入：该令牌消歧耗尽
	_, mux := newBackfillHandler(t, svc)

	rec := postRename(t, mux, "/api/v1/admin/tokens/magpie-keys/rename?dry_run=false", "")
	if rec.Code != http.StatusConflict {
		t.Fatalf("消歧撞名应 409：%d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code   int    `json:"Code"`
		Reason string `json:"Reason"`
		Data   struct {
			Created   int              `json:"Created"`
			Renamed   int              `json:"Renamed"`
			Failed    []map[string]any `json:"Failed"`
			Conflicts []map[string]any `json:"Conflicts"`
			DryRun    bool             `json:"DryRun"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("409 响应应可解析：%v %s", err, rec.Body.String())
	}
	if body.Reason != "caller_key_name_conflict" {
		t.Fatalf("409 应带机器可读 Reason：%s", rec.Body.String())
	}
	if len(body.Data.Failed) != 1 {
		t.Fatalf("data.Failed 应含撞名令牌：%s", rec.Body.String())
	}
	if got, _ := body.Data.Failed[0]["TokenID"].(float64); int64(got) != 1 {
		t.Fatalf("data.Failed[0].TokenID 应为 1：%s", rec.Body.String())
	}
	if c, _ := body.Data.Failed[0]["Conflict"].(bool); !c {
		t.Fatalf("data.Failed[0].Conflict 应为 true：%s", rec.Body.String())
	}
	if len(body.Data.Conflicts) != 1 {
		t.Fatalf("data.Conflicts 应含撞名令牌明细：%s", rec.Body.String())
	}
	if got, _ := body.Data.Conflicts[0]["TokenID"].(float64); int64(got) != 1 {
		t.Fatalf("data.Conflicts[0].TokenID 应为 1：%s", rec.Body.String())
	}
	if body.Data.Conflicts[0]["Reason"] == nil || body.Data.Conflicts[0]["DisplayName"] == nil {
		t.Fatalf("Conflicts 明细应带 Reason 与 DisplayName：%s", rec.Body.String())
	}
	if body.Data.DryRun {
		t.Fatalf("执行态响应 DryRun 应为 false：%s", rec.Body.String())
	}
}

// TestRenameHandler_DryRunProbesConflict dry-run 必须能探出 Conflict 并带明细
// （评审必改：dry-run 只比名不查占用 → 冲突令牌在预览里隐形，放行后执行才发现 409）。
// 探测走 ProbeName（零写零代理侧动作），HTTP 200 + data.Conflicts 可见。
func TestRenameHandler_DryRunProbesConflict(t *testing.T) {
	ids := []int64{1}
	names := map[int64]string{1: "ls-token-1"}
	svc, _, fs := newBackfillSvc(t, ids, names, map[int64]string{1: "s1"})
	fs.probeTaken[1] = ErrCallerKeyNameTaken // 注入：预演即撞名
	_, mux := newBackfillHandler(t, svc)

	rec := postRename(t, mux, "/api/v1/admin/tokens/magpie-keys/rename", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("dry-run 预览不执行动作、不应 409：%d %s", rec.Code, rec.Body.String())
	}
	if len(fs.calls) != 0 {
		t.Fatalf("dry-run 不得触发执行动作：%v", fs.calls)
	}
	var body struct {
		Data struct {
			Skipped   map[string]int   `json:"Skipped"`
			Conflicts []map[string]any `json:"Conflicts"`
			DryRun    bool             `json:"DryRun"`
		} `json:"Data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析失败：%v %s", err, rec.Body.String())
	}
	if !body.Data.DryRun || body.Data.Skipped["renamed"] != 1 {
		t.Fatalf("dry-run 预览分桶应保留：%s", rec.Body.String())
	}
	if len(body.Data.Conflicts) != 1 {
		t.Fatalf("dry-run 应探出 Conflict 明细：%s", rec.Body.String())
	}
	if got, _ := body.Data.Conflicts[0]["TokenID"].(float64); int64(got) != 1 {
		t.Fatalf("Conflicts[0].TokenID 应为 1：%s", rec.Body.String())
	}
}

// TestRenameHandler_BodyDryRunFalse 请求体 {"DryRun":false} 等效执行（指针区分未传）。
func TestRenameHandler_BodyDryRunFalse(t *testing.T) {
	ids := []int64{1}
	names := map[int64]string{1: "ls-token-1"}
	svc, _, fs := newBackfillSvc(t, ids, names, map[int64]string{1: "s1"})
	_, mux := newBackfillHandler(t, svc)

	rec := postRename(t, mux, "/api/v1/admin/tokens/magpie-keys/rename", `{"DryRun":false}`)
	if rec.Code != http.StatusOK || len(fs.calls) != 1 {
		t.Fatalf("body 显式 false 应执行：%d calls=%v", rec.Code, fs.calls)
	}
}

// TestBackfill_DisabledBucketSkips 停用映射行（令牌停用时 Remove 删过代理 key）：
// dry-run 与执行态都进 disabled 桶、Ensure 零调用、不进 failed（评审必改①的行为锚）。
// 旧自动名的停用行也必须走这里 —— 自动路径不碰它（改名只在显式启用/管理端开通后发生）。
func TestBackfill_DisabledBucketSkips(t *testing.T) {
	ids := []int64{1, 2}
	names := map[int64]string{
		1: "ops-manual-key", // 停用 + 手工名
		2: "ls-token-2",     // 停用 + 旧自动名（评审洞②形状：自动改名绝不能碰到它）
	}
	secrets := map[int64]string{1: "s1", 2: "s2"}
	states := map[int64]string{1: "DISABLED", 2: "DISABLED"}

	// dry-run：分桶 + 零执行。
	svc, mock, fs := newBackfillSvcStates(t, ids, names, secrets, states)
	created, renamed, skipped, conflicts, failed, err := svc.BackfillCallerKeys(context.Background(), 0, true)
	if err != nil {
		t.Fatalf("dry-run: %v", err)
	}
	if created != 0 || renamed != 0 || len(failed) != 0 || len(fs.calls) != 0 {
		t.Fatalf("dry-run 不得执行/报错：created=%d renamed=%d failed=%v calls=%v", created, renamed, failed, fs.calls)
	}
	if skipped["disabled"] != 2 || len(conflicts) != 0 {
		t.Fatalf("停用行应双双进 disabled 桶：%v conflicts=%v", skipped, conflicts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}

	// 执行态：同样分桶跳过、Ensure 零调用、无 failed（不产生重试压力）。
	svc2, mock2, fs2 := newBackfillSvcStates(t, ids, names, secrets, states)
	created, renamed, skipped, conflicts, failed, err = svc2.BackfillCallerKeys(context.Background(), 0, false)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if created != 0 || renamed != 0 || len(failed) != 0 || len(fs2.calls) != 0 {
		t.Fatalf("执行态停用行必须跳过：created=%d renamed=%d failed=%v calls=%v", created, renamed, failed, fs2.calls)
	}
	if skipped["disabled"] != 2 {
		t.Fatalf("执行态停用桶不符：%v", skipped)
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestBackfill_DisabledBySentinel 状态钩子缺位（未注入 SetCallerKeyStateOf）时，
// Ensure 自带的 state 守卫兜底：停用行进 disabled 桶，不进 failed。
func TestBackfill_DisabledBySentinel(t *testing.T) {
	ids := []int64{1}
	names := map[int64]string{1: "ls-token-1"}
	secrets := map[int64]string{1: "s1"}
	svc, mock, fs := newBackfillSvc(t, ids, names, secrets)
	// 不注入 state 钩子；Ensure 对停用行返回 ErrCallerKeyDisabled（自动路径守卫）。
	fs.EnsureDisabled = true

	created, renamed, skipped, conflicts, failed, err := svc.BackfillCallerKeys(context.Background(), 0, false)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	if created != 0 || renamed != 0 || len(failed) != 0 || len(conflicts) != 0 {
		t.Fatalf("哨兵应进 disabled 桶而非 failed：created=%d renamed=%d failed=%v conflicts=%v", created, renamed, failed, conflicts)
	}
	if skipped["disabled"] != 1 {
		t.Fatalf("disabled 桶不符：%v", skipped)
	}
	if len(fs.calls) != 0 {
		t.Fatalf("Ensure 之外的调用不应发生：%v", fs.calls)
	}
	_ = mock
}

// TestWriteServiceErr_CallerKeyConflict409 创建侧撞名对外可辨识（Gate 遗留②）：
// Create 的回滚包装错误经 writeServiceErr 必须映射 409 + CodeConflict + 机器可读
// Reason（此前统一 500，调用方看不出「换令牌名重试」能解）。
func TestWriteServiceErr_CallerKeyConflict409(t *testing.T) {
	wrapped := fmt.Errorf("令牌已创建但开通代理访问失败（已回滚）：%w", ErrCallerKeyNameTaken)
	rec := httptest.NewRecorder()
	writeServiceErr(rec, httptest.NewRequest(http.MethodPost, "/api/v1/admin/tokens", nil), wrapped)
	if rec.Code != http.StatusConflict {
		t.Fatalf("撞名应 409：status=%d body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Code   int    `json:"Code"`
		Reason string `json:"Reason"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("解析失败：%v %s", err, rec.Body.String())
	}
	if body.Code != 40901 || body.Reason != "caller_key_name_conflict" {
		t.Fatalf("409 应带业务码与 Reason：%s", rec.Body.String())
	}
	// 非哨兵错误不受影响（回归锚：普通 wrap 仍 500）。
	rec2 := httptest.NewRecorder()
	writeServiceErr(rec2, httptest.NewRequest(http.MethodPost, "/api/v1/admin/tokens", nil), fmt.Errorf("别的错误"))
	if rec2.Code != http.StatusInternalServerError {
		t.Fatalf("非撞名错误应保持 500：%d", rec2.Code)
	}
}
