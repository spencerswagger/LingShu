package billing

import (
	"context"
	"regexp"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// TestListRecords_FilterByKeyAndSession 断言 ListRecords 按渠道密钥 / 会话维度过滤：
// WHERE 使用 channel_key_id=$2、session_id=$3，参数顺序随索引平移。
func TestListRecords_FilterByKeyAndSession(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := NewSqlStore(db)
	keyID := int64(3)
	f := RecordFilter{ChannelKeyID: &keyID, SessionID: "sess-1", Page: 1, Size: 10}

	mock.ExpectQuery(`SELECT count\(\*\) FROM billing_records .*channel_key_id = \$2.*token_id = \$3.*session_id = \$4.*`).
		WithArgs(nil, int64(3), nil, "sess-1", "", "", "", nil, nil).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))
	mock.ExpectQuery(`SELECT .* FROM billing_records .*channel_key_id = \$2.*token_id = \$3.*session_id = \$4.*ORDER BY id DESC LIMIT \$10 OFFSET \$11`).
		WithArgs(nil, int64(3), nil, "sess-1", "", "", "", nil, nil, 10, 0).
		WillReturnRows(completedRow(t))

	list, total, err := store.ListRecords(context.Background(), f)
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if total != 1 {
		t.Fatalf("total=%d, want 1", total)
	}
	if len(list) != 1 || list[0].ChannelKeyID != 1 {
		t.Fatalf("list=%+v, want 1 row with channel_key_id=1", list)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestListRecords_NoFilter 断言零值过滤器仍可用：参数位全部占位且不报错。
func TestListRecords_NoFilter(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := NewSqlStore(db)
	mock.ExpectQuery(`SELECT count\(\*\) FROM billing_records .*channel_key_id = \$2`).
		WithArgs(nil, nil, nil, "", "", "", "", nil, nil).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(0)))
	mock.ExpectQuery(`SELECT .* FROM billing_records .*LIMIT \$10 OFFSET \$11`).
		WithArgs(nil, nil, nil, "", "", "", "", nil, nil, 20, 0).
		WillReturnRows(sqlmock.NewRows(recordColsForTest))

	list, total, err := store.ListRecords(context.Background(), RecordFilter{})
	if err != nil {
		t.Fatalf("ListRecords: %v", err)
	}
	if total != 0 || len(list) != 0 {
		t.Fatalf("total=%d len=%d, want 0/0", total, len(list))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestGetByBillingID_ScanDims 断言 GetByBillingID 能扫描带 channel_key_id/session_id 的行。
func TestGetByBillingID_ScanDims(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := NewSqlStore(db)
	mock.ExpectQuery(regexp.MustCompile(`SELECT .* FROM billing_records WHERE billing_id=\$1`).String()).
		WithArgs(billingID).
		WillReturnRows(sqlmock.NewRows(recordColsForTest).AddRow(
			int64(5), billingID, int64(7), "sale", nil, "ext-model", "int-model", int64(1), "sess-x", "会话A",
			time.Now(), `{}`, `{}`, []byte(`{"time":1,"context":1}`), int64(10000),
			nil, nil, nil, nil, nil, "completed", nil, nil, nil,
		))

	rec, err := store.GetByBillingID(context.Background(), billingID)
	if err != nil {
		t.Fatalf("GetByBillingID: %v", err)
	}
	if rec.ChannelKeyID != 1 || rec.SessionID != "sess-x" {
		t.Fatalf("扫到维度错误: channel_key_id=%d session_id=%q", rec.ChannelKeyID, rec.SessionID)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}
