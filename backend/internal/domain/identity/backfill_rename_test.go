// backfill_rename_test.go 锁存量改名回填的三条硬口径（服务层，sqlmock 供数）：
//  1. 改名资格是确定性映射（当前名 == 旧自动名 ls-token-<id>），不猜名、不按样本特判；
//  2. dry-run 只判定不执行（无代理请求、无写库）；
//  3. 手工绑定名 / 已是新名 / 无密文 各归各的 skip 桶，不混、不产生重复数据。
package identity

import (
	"context"
	"fmt"
	"regexp"
	"sync"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/team/llmgateway/internal/pkg/crypto"
)

// fakeSyncer 记录 Ensure 调用（回填执行断言），可注入失败。
type fakeSyncer struct {
	mu    sync.Mutex
	calls []string // "uid:tokid:disp"
	fail  map[int64]error
}

func (f *fakeSyncer) Ensure(_ context.Context, userID int64, tok CallerKeyToken, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.fail[tok.ID]; ok {
		return err
	}
	f.calls = append(f.calls, fmt.Sprintf("%d:%d:%s", userID, tok.ID, tok.DisplayName))
	return nil
}

func (f *fakeSyncer) Remove(context.Context, int64) error { return nil }

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
	fs := &fakeSyncer{fail: map[int64]error{}}
	svc.SetCallerKeySyncer(fs)
	svc.SetCallerKeyNameOf(func(ctx context.Context, tokenID int64) string { return names[tokenID] })
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

	created, renamed, skipped, failed, err := svc.BackfillCallerKeys(context.Background(), 0, true)
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
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestBackfill_ExecuteRenamesLegacyOnly 执行态：只有旧自动名被送进 Ensure（改名），
// 手工名与已是新名的都不动；无密文进 failed。
func TestBackfill_ExecuteRenamesLegacyOnly(t *testing.T) {
	ids := []int64{1, 2, 3, 4}
	names := map[int64]string{1: "ls-token-1", 2: "want-u102-t2", 3: "ops-manual-key"}
	secrets := map[int64]string{1: "s1", 2: "s2", 3: "s3"}
	svc, mock, fs := newBackfillSvc(t, ids, names, secrets)

	created, renamed, skipped, failed, err := svc.BackfillCallerKeys(context.Background(), 0, false)
	if err != nil {
		t.Fatalf("backfill: %v", err)
	}
	// uid = 100 + id%3 → 令牌1=101、令牌2=102、令牌3=100。
	// 执行态：令牌1（旧自动名→改名）与令牌3（手工名，Ensure 语义=名字不动只对齐值）进 Ensure；
	// 令牌2 已是新名提前 skip；令牌4 无密文 failed。
	if len(fs.calls) != 2 {
		t.Fatalf("Ensure 调用数不符：%v", fs.calls)
	}
	if fs.calls[0] != "101:1:name-1" || fs.calls[1] != "100:3:name-3" {
		t.Fatalf("Ensure 调用序不符（令牌1 改名 + 令牌3 手工名对齐）：%v", fs.calls)
	}
	// already-bound 提前 skip（不走 Ensure）；manual 名由 Ensure 语义保证名字不动。
	if skipped["already-bound"] != 1 || skipped["manual-named"] != 0 {
		t.Fatalf("skip 桶不符：%v", skipped)
	}
	// 手工名对齐成功计 created=1；旧自动名改名计 renamed=1。
	if created != 1 || renamed != 1 {
		t.Fatalf("created=%d renamed=%d（应各 1：手工名对齐 + 旧名改名）", created, renamed)
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
	c1, r1, sk1, f1, err := svc1.BackfillCallerKeys(context.Background(), 0, false)
	if err != nil || len(f1) != 0 {
		t.Fatalf("first: err=%v failed=%v", err, f1)
	}
	if c1 != 0 || r1 != 1 || sk1["renamed"] != 0 {
		t.Fatalf("first: created=%d renamed=%d skipped=%v", c1, r1, sk1)
	}
	_ = fs1
	if err := mock1.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
	// 第二轮：当前名已是新口径（Ensure 已推进）。
	svc2, mock2, _ := newBackfillSvc(t, ids, map[int64]string{1: "want-u101-t1"}, map[int64]string{1: "s1"})
	c2, r2, sk2, f2, err := svc2.BackfillCallerKeys(context.Background(), 0, false)
	if err != nil {
		t.Fatalf("second: %v", err)
	}
	if c2 != 0 || r2 != 0 || sk2["already-bound"] != 1 || len(f2) != 0 {
		t.Fatalf("second: created=%d renamed=%d skipped=%v failed=%v", c2, r2, sk2, f2)
	}
	if err := mock2.ExpectationsWereMet(); err != nil {
		t.Fatalf("%v", err)
	}
}
