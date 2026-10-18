package tag

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/team/llmgateway/internal/pkg/resp"
)

func mockStore(t *testing.T) (*Service, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return NewService(NewStore(db)), mock
}

func TestService_CreateTag(t *testing.T) {
	s, mock := mockStore(t)
	now := time.Now()

	// 名称预检查：无行
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM semantic_tags WHERE name = $1`)).
		WithArgs("premium").
		WillReturnError(sql.ErrNoRows)
	// 插入
	mock.ExpectQuery(regexp.QuoteMeta(`INSERT INTO semantic_tags(id, name, description, kv_pairs, enabled, created_by) VALUES($1,$2,$3,$4,$5,$6) RETURNING `+cols)).
		WithArgs(sqlmock.AnyArg(), "premium", "desc", sqlmock.AnyArg(), true, nullableID(42)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "kv_pairs", "enabled", "created_by", "created_at"}).
			AddRow(int64(1), "premium", "desc", `{"tier":"gold"}`, true, int64(42), now))

	tag, err := s.CreateTag(context.Background(), TagInput{Name: "premium", Description: "desc", KVPairs: map[string]string{"tier": "gold"}}, 42)
	if err != nil {
		t.Fatalf("create tag: %v", err)
	}
	if tag.ID != 1 || tag.Name != "premium" || tag.KVPairs["tier"] != "gold" {
		t.Fatalf("unexpected tag: %+v", tag)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestService_CreateTag_Validation(t *testing.T) {
	s, _ := mockStore(t)
	_, err := s.CreateTag(context.Background(), TagInput{Name: "", KVPairs: map[string]string{}}, 0)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeBadRequest {
		t.Fatalf("期望 name 空的 40001，got %v", err)
	}
	_, err = s.CreateTag(context.Background(), TagInput{Name: "x", KVPairs: map[string]string{"": "v"}}, 0)
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeBadRequest {
		t.Fatalf("期望空键的 40001，got %v", err)
	}
}

func TestService_CreateTag_NameExists(t *testing.T) {
	s, mock := mockStore(t)
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM semantic_tags WHERE name = $1`)).
		WithArgs("dup").
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "kv_pairs", "enabled", "created_by", "created_at"}).
			AddRow(int64(9), "dup", "", `{}`, true, nil, now))
	_, err := s.CreateTag(context.Background(), TagInput{Name: "dup"}, 0)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeConflict {
		t.Fatalf("期望名称冲突 40901，got %v", err)
	}
}

func TestService_UpdateTag(t *testing.T) {
	s, mock := mockStore(t)
	now := time.Now()

	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM semantic_tags WHERE id = $1`)).
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "kv_pairs", "enabled", "created_by", "created_at"}).
			AddRow(int64(1), "gold", "d", `{"tier":"gold"}`, true, int64(1), now))
	// name 未变化，不触发唯一性预检查
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE semantic_tags SET name=$1, description=$2, kv_pairs=$3, enabled=$4 WHERE id=$5`)).
		WithArgs("gold", "new", sqlmock.AnyArg(), true, int64(1)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM semantic_tags WHERE id = $1`)).
		WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "kv_pairs", "enabled", "created_by", "created_at"}).
			AddRow(int64(1), "gold", "new", `{"tier":"gold"}`, true, int64(1), now))

	tag, err := s.UpdateTag(context.Background(), 1, TagInput{Name: "gold", Description: "new", KVPairs: map[string]string{"tier": "gold"}})
	if err != nil {
		t.Fatalf("update tag: %v", err)
	}
	if tag.Description != "new" {
		t.Fatalf("unexpected tag: %+v", tag)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestService_UpdateTag_NotFound(t *testing.T) {
	s, mock := mockStore(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM semantic_tags WHERE id = $1`)).
		WithArgs(int64(99)).WillReturnError(sql.ErrNoRows)
	_, err := s.UpdateTag(context.Background(), 99, TagInput{Name: "x"})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != resp.CodeNotFound {
		t.Fatalf("期望 40401，got %v", err)
	}
}

func TestService_ListTags(t *testing.T) {
	s, mock := mockStore(t)
	now := time.Now()
	enabled := true
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT ` + cols + ` FROM semantic_tags WHERE deleted_at IS NULL AND enabled = $1 ORDER BY id ASC`)).
		WithArgs(true).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "kv_pairs", "enabled", "created_by", "created_at"}).
			AddRow(int64(1), "gold", "", `{}`, true, nil, now))
	tags, err := s.ListTags(context.Background(), &enabled)
	if err != nil {
		t.Fatalf("list tags: %v", err)
	}
	if len(tags) != 1 || tags[0].ID != 1 {
		t.Fatalf("unexpected tags: %+v", tags)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestService_BatchDeleteTags(t *testing.T) {
	s, mock := mockStore(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM tokens WHERE tag_id = ANY($1::bigint[]) AND deleted_at IS NULL`)).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM channel_tags WHERE tag_id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE semantic_tags SET deleted_at = now() WHERE id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 2))
	n, err := s.BatchDeleteTags(context.Background(), []int64{1, 2})
	if err != nil {
		t.Fatalf("batch delete: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2, got %d", n)
	}
}

func TestService_BatchDeleteTags_Referenced(t *testing.T) {
	s, mock := mockStore(t)
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM tokens WHERE tag_id = ANY($1::bigint[]) AND deleted_at IS NULL`)).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(3)))
	_, err := s.BatchDeleteTags(context.Background(), []int64{1})
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusConflict {
		t.Fatalf("期望被引用 409，got %v", err)
	}
}

func TestService_BatchDeleteTags_Empty(t *testing.T) {
	s, _ := mockStore(t)
	_, err := s.BatchDeleteTags(context.Background(), nil)
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.HTTPStatus != http.StatusBadRequest {
		t.Fatalf("期望空 ids 400，got %v", err)
	}
}

func TestService_Resolve(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewService(NewStore(db))
	ctx := context.Background()

	rows := sqlmock.NewRows([]string{"id", "name", "description", "kv_pairs", "enabled", "created_by", "created_at"}).
		AddRow(int64(1), "金融", "", `{"region":"cn"}`, true, nil, time.Now())
	mock.ExpectQuery(`SELECT .* FROM semantic_tags WHERE id = ANY`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(rows)

	got, err := s.Resolve(ctx, []int64{1})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(got) != 1 || got[0].KVPairs["region"] != "cn" {
		t.Fatalf("unexpected: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

func TestService_BatchDelete_BlockedByChannel(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	st := NewStore(db)
	s := NewService(st)
	ctx := context.Background()

	mock.ExpectQuery(`(?i)SELECT COUNT\(\*\) FROM tokens WHERE tag_id = ANY`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(0)))
	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM channel_tags WHERE tag_id = ANY`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(1)))

	_, err = s.BatchDeleteTags(ctx, []int64{1})
	if err == nil || !strings.Contains(err.Error(), "渠道") {
		t.Fatalf("期望渠道引用冲突，got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestService_GetByID_CacheZeroDBAndInvalidate 验证标签读缓存（STEP 22）：
// 命中后零 DB（连续多次读取仅回源一次）；删除写路径后缓存立即失效（再读重新查库并读到新值）。
func TestService_GetByID_CacheZeroDBAndInvalidate(t *testing.T) {
	s, mock := mockStore(t)
	now := time.Now()
	q := regexp.QuoteMeta(`SELECT ` + cols + ` FROM semantic_tags WHERE id = $1 AND deleted_at IS NULL`)
	tagRows := func(name string) *sqlmock.Rows {
		return sqlmock.NewRows([]string{"id", "name", "description", "kv_pairs", "enabled", "created_by", "created_at"}).
			AddRow(int64(1), name, "", `{"tier":"gold"}`, true, nil, now)
	}

	// 首次读取回源一次。
	mock.ExpectQuery(q).WithArgs(int64(1)).WillReturnRows(tagRows("gold"))
	for i := 0; i < 3; i++ {
		tg, err := s.store.GetByID(context.Background(), 1)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		if tg.Name != "gold" {
			t.Fatalf("unexpected: %+v", tg)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("命中缓存不应再读库: %v", err)
	}

	// 写路径（批量删除）后缓存立即失效：再读重新查库并拿到新值。
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT count(*) FROM tokens WHERE tag_id = ANY($1::bigint[]) AND deleted_at IS NULL`)).
		WithArgs(sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectQuery(regexp.QuoteMeta(`SELECT COUNT(*) FROM channel_tags WHERE tag_id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE semantic_tags SET deleted_at = now() WHERE id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	if _, err := s.BatchDeleteTags(context.Background(), []int64{1}); err != nil {
		t.Fatalf("batch delete: %v", err)
	}

	mock.ExpectQuery(q).WithArgs(int64(1)).WillReturnRows(tagRows("gold-v2"))
	tg, err := s.store.GetByID(context.Background(), 1)
	if err != nil {
		t.Fatalf("get after invalidate: %v", err)
	}
	if tg.Name != "gold-v2" {
		t.Fatalf("失效后应读到新值，got %+v", tg)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// TestService_GetByID_DefensiveCopy 锁定 P2-7：GetByID 命中缓存返回防御性拷贝，
// 消费方（网关 TagKVResolver 拿到 KVPairs）就地修改不得污染缓存快照。
func TestService_GetByID_DefensiveCopy(t *testing.T) {
	s, mock := mockStore(t)
	now := time.Now()
	q := regexp.QuoteMeta(`SELECT ` + cols + ` FROM semantic_tags WHERE id = $1 AND deleted_at IS NULL`)
	mock.ExpectQuery(q).WithArgs(int64(1)).
		WillReturnRows(sqlmock.NewRows([]string{"id", "name", "description", "kv_pairs", "enabled", "created_by", "created_at"}).
			AddRow(int64(1), "gold", "", `{"tier":"gold"}`, true, nil, now))

	first, err := s.store.GetByID(context.Background(), 1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	// 就地篡改返回值：不应影响已缓存的快照。
	first.KVPairs["tier"] = "mutated"
	first.KVPairs["extra"] = "x"

	second, err := s.store.GetByID(context.Background(), 1) // 命中缓存，0 DB
	if err != nil {
		t.Fatalf("get2: %v", err)
	}
	if second.KVPairs["tier"] != "gold" {
		t.Fatalf("缓存被返回值修改污染：%v", second.KVPairs)
	}
	if _, ok := second.KVPairs["extra"]; ok {
		t.Fatalf("缓存被返回值修改污染（新增键泄漏）：%v", second.KVPairs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("命中缓存不应再读库: %v", err)
	}
}

// TestStore_StaleCacheFillAfterInvalidateRejected 与 model/identity 的同名用例同构：
// 读路径在回源 DB 前捕获代际；若回源期间发生写路径失效（bump 代际，如 Update/BatchDelete），
// 携带陈旧代际的回填必须被拒绝——否则失效前读到的旧标签快照会滞留（本缓存无 TTL），
// 后续鉴权热路径将持续读到该陈旧的 KVPairs/enabled 状态。
func TestStore_StaleCacheFillAfterInvalidateRejected(t *testing.T) {
	s := NewStore(nil)
	fresh := &Tag{ID: 1, Name: "gold", KVPairs: map[string]string{"tier": "gold"}}

	// 读路径：回源前捕获代际。
	gen := s.tagCache.Generation()
	// 期间写路径失效（如 UpdateTag/BatchDeleteTags 触发）。
	s.invalidateTagID(1)
	// 读路径回来回填（携带陈旧代际）。
	s.cacheTag(1, fresh, gen)

	if _, ok := s.tagCache.Get(tagCacheKey(1)); ok {
		t.Fatal("失效后携带陈旧代际的回填必须被拒绝（否则旧标签快照无限期滞留）")
	}

	// 对照组：期间无失效时，携带当前代际的回填应正常命中（证明不会误杀正常回填路径）。
	s.cacheTag(1, fresh, s.tagCache.Generation())
	if _, ok := s.tagCache.Get(tagCacheKey(1)); !ok {
		t.Fatal("无失效发生时回填应正常命中")
	}
}
