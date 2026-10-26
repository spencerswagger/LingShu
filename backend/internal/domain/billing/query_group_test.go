package billing

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
)

// groupedRows 构造归并查询的返回行：recordCols（25 列）+ rid + cnt。
func groupedRows(t *testing.T, rid string, cnt int) *sqlmock.Rows {
	t.Helper()
	cols := append(append([]string{}, recordColsForTest...), "rid", "cnt")
	return sqlmock.NewRows(cols).AddRow(
		int64(9), "bill-main", int64(7), "sale", nil, "ext-model", nil, "int-model", int64(1), nil, "",
		time.Now(), `{}`, `{}`, []byte(`{"time":1,"context":1}`), int64(10000),
		nil, nil, nil, nil, nil, "completed", nil, nil, nil,
		rid, int64(cnt),
	)
}

// TestListRecords_GroupByRequestMergesAttempts 断言归并列表：
//   - SQL 走 CTE grp（按 call_logs.request_id 分组，无日志时回落 billing_id）；
//   - 主行优先 completed、其次最新，并带回 request_id 与该请求的账单条数 Attempts；
//   - Total 是「请求数」（分组的组数），不是账单行数。
func TestListRecords_GroupByRequestMergesAttempts(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := NewSqlStore(db)
	f := RecordFilter{GroupByRequest: true, Page: 1, Size: 20}

	// 组数统计：count(*) FROM ( SELECT 1 FROM billing_records br LEFT JOIN call_logs cl ... GROUP BY ... )。
	mock.ExpectQuery(`SELECT count\(\*\) FROM \(\s*SELECT 1 FROM billing_records br LEFT JOIN call_logs cl .*GROUP BY COALESCE\(NULLIF\(cl.request_id, ''\), br.billing_id\)\) grp`).
		WithArgs(nil, nil, nil, "", "", "", "", nil, nil).
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(3)))
	// 主行选取：array_agg 按 (status='completed') DESC, id DESC 取第一，并带回 rid/cnt。
	mock.ExpectQuery(`WITH grp AS \(\s*SELECT COALESCE\(NULLIF\(cl.request_id, ''\), br.billing_id\) AS g,.*array_agg\(br.billing_id ORDER BY \(br.status = 'completed'\) DESC, br.id DESC\)\)\[1\] AS main_bid,.*count\(\*\)::int AS cnt.*SELECT .*grp.rid, grp.cnt.*ORDER BY billing_records.id DESC LIMIT \$10 OFFSET \$11`).
		WithArgs(nil, nil, nil, "", "", "", "", nil, nil, 20, 0).
		WillReturnRows(groupedRows(t, "req-1", 2))

	list, total, err := store.ListRecords(context.Background(), f)
	if err != nil {
		t.Fatalf("ListRecords grouped: %v", err)
	}
	if total != 3 {
		t.Fatalf("total=%d, want 3 (请求数而非账单行数)", total)
	}
	if len(list) != 1 {
		t.Fatalf("len=%d, want 1", len(list))
	}
	if list[0].RequestID != "req-1" {
		t.Fatalf("RequestID=%q, want req-1", list[0].RequestID)
	}
	if list[0].Attempts != 2 {
		t.Fatalf("Attempts=%d, want 2", list[0].Attempts)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestListRecords_FilterByRequestID 断言按 request_id 过滤：
// billing_records 无该列，条件经 call_logs 子查询命中（且参数位不污染既有 9 个过滤位）。
func TestListRecords_FilterByRequestID(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()

	store := NewSqlStore(db)
	f := RecordFilter{RequestID: "req-xyz", Page: 1, Size: 10}

	mock.ExpectQuery(`SELECT count\(\*\) FROM billing_records .*AND EXISTS \(SELECT 1 FROM call_logs clr\s+WHERE clr.billing_id = billing_id AND clr.request_id = \$10\)`).
		WithArgs(nil, nil, nil, "", "", "", "", nil, nil, "req-xyz").
		WillReturnRows(sqlmock.NewRows([]string{"count"}).AddRow(int64(1)))
	mock.ExpectQuery(`SELECT .* FROM billing_records .*clr.request_id = \$10.*ORDER BY id DESC LIMIT \$11 OFFSET \$12`).
		WithArgs(nil, nil, nil, "", "", "", "", nil, nil, "req-xyz", 10, 0).
		WillReturnRows(completedRow(t))

	list, total, err := store.ListRecords(context.Background(), f)
	if err != nil {
		t.Fatalf("ListRecords by request id: %v", err)
	}
	if total != 1 || len(list) != 1 {
		t.Fatalf("total=%d len=%d, want 1/1", total, len(list))
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("mock expectations: %v", err)
	}
}

// TestRecordWhere_RequestIDQualified 断言带表别名的 WHERE（归并查询用）列名已限定，
// 避免 call_logs 与 billing_records 同名列歧义；且 request_id 条件用 $10 而非硬编码位次。
func TestRecordWhere_RequestIDQualified(t *testing.T) {
	where, args := recordWhereOn("br.", RecordFilter{RequestID: "r1"})
	if len(args) != 10 {
		t.Fatalf("args=%d, want 10", len(args))
	}
	for _, frag := range []string{"br.user_id = $1", "br.channel_key_id = $2", "br.status = $7", "clr.request_id = $10"} {
		if !containsAll(where, frag) {
			t.Fatalf("where missing %q: %s", frag, where)
		}
	}
	// 未指定 request_id 时保持历史口径（9 个参数、无 EXISTS）。
	where2, args2 := recordWhereOn("", RecordFilter{})
	if len(args2) != 9 {
		t.Fatalf("args=%d, want 9", len(args2))
	}
	if containsAll(where2, "EXISTS") {
		t.Fatalf("no request id should not add EXISTS: %s", where2)
	}
}

// containsAll 判断 s 是否包含全部子串（测试小工具）。
func containsAll(s string, subs ...string) bool {
	for _, sub := range subs {
		if !contains(s, sub) {
			return false
		}
	}
	return true
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
