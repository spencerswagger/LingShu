package channel

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

func TestStore_Insert(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewStore(db)

	c := &Channel{
		Name: "c1", Protocol: "openai-compat", BaseURL: "https://x.example.com",
		Tags: nil, Priority: 100,
	}
	now := time.Now()
	mock.ExpectQuery(regexp.QuoteMeta(insertChannelSQL)).
		WithArgs(sqlmock.AnyArg(), "c1", "openai-compat", "https://x.example.com",
			sqlmock.AnyArg(), 100, sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnRows(channelRow(&Channel{
			ID: 7, Name: "c1", Protocol: "openai-compat", BaseURL: "https://x.example.com",
			Priority:  100,
			CreatedAt: now, UpdatedAt: now,
		}))

	got, err := s.Insert(context.Background(), c)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if got.ID != 7 || got.Name != "c1" {
		t.Fatalf("unexpected result: %+v", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestStore_List(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewStore(db)

	now := time.Now()
	mock.ExpectQuery(`SELECT ` + regexp.QuoteMeta(channelCols) + ` FROM channels WHERE deleted_at IS NULL ORDER BY priority DESC, id ASC`).
		WillReturnRows(channelRow(&Channel{
			ID: 1, Name: "a", Protocol: "openai-compat", BaseURL: "https://a.example.com",
			Priority:  100,
			CreatedAt: now, UpdatedAt: now,
		}))

	chs, err := s.List(context.Background())
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(chs) != 1 || chs[0].ID != 1 {
		t.Fatalf("unexpected channels: %+v", chs)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestStore_DeleteChannels_CascadeNoBilling(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewStore(db)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM channel_tags WHERE channel_id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_models SET deleted_at = now() WHERE channel_id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
	// D3：删除渠道级联软删其全部 channel_keys（防止残留密钥继续参与路由/探测）。
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET deleted_at = now() WHERE channel_id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channels SET deleted_at = now() WHERE id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 2))
	mock.ExpectCommit()

	n, err := s.DeleteChannels(context.Background(), []int64{1, 2})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 deleted, got %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestStore_DeleteChannels_SoftDeletesModelsAndChannels(t *testing.T) {
	// 有账单记录的渠道同样允许软删除；历史 billing 保留（列表回显渠道名不按 deleted_at 过滤）。
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	s := NewStore(db)

	mock.ExpectBegin()
	mock.ExpectExec(regexp.QuoteMeta(`DELETE FROM channel_tags WHERE channel_id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_models SET deleted_at = now() WHERE channel_id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channel_keys SET deleted_at = now() WHERE channel_id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(regexp.QuoteMeta(`UPDATE channels SET deleted_at = now() WHERE id = ANY($1::bigint[])`)).
		WithArgs(sqlmock.AnyArg()).WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	n, err := s.DeleteChannels(context.Background(), []int64{99})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if n != 1 {
		t.Fatalf("expected 1 deleted, got %d", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

func TestChannelTags_ReplaceAndList(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	st := NewStore(db)
	ctx := context.Background()

	mock.MatchExpectationsInOrder(true)
	mock.ExpectBegin()
	mock.ExpectExec(`DELETE FROM channel_tags WHERE channel_id = \$1`).
		WithArgs(int64(7)).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec(`INSERT INTO channel_tags\(channel_id, tag_id\) VALUES \(\$1,\$2\)`).
		WithArgs(int64(7), int64(11)).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	if err := st.ReplaceChannelTags(ctx, nil, 7, []int64{11}); err != nil {
		t.Fatalf("replace: %v", err)
	}

	rows := sqlmock.NewRows([]string{"tag_id", "name", "kv_pairs"}).
		AddRow(int64(11), "金融", `{"region":"cn"}`)
	mock.ExpectQuery(`SELECT ct.tag_id, st.name, st.kv_pairs FROM channel_tags ct`).
		WithArgs(int64(7)).
		WillReturnRows(rows)
	refs, err := st.ListTagRefsByChannel(ctx, 7)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(refs) != 1 || refs[0].ID != 11 || refs[0].KV["region"] != "cn" {
		t.Fatalf("unexpected refs: %+v", refs)
	}

	mock.ExpectQuery(`SELECT COUNT\(\*\) FROM channel_tags WHERE tag_id = ANY`).
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"c"}).AddRow(int64(2)))
	n, err := st.CountChannelRefs(ctx, []int64{11})
	if err != nil || n != 2 {
		t.Fatalf("count refs: n=%d err=%v", n, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}
