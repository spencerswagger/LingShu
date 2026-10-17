package gateway

import (
	"strconv"
	"strings"
	"testing"
)

// 列数与占位符数必须一致：两者都由人工维护，只改一处会在真实 PG 报
// "INSERT has more target columns than expressions"，且每次写调用日志都会失败。
// console/calllog_test.go 的 sqlmock 只比对 SQL 文本（两处一起改错也照样通过），
// 因此这里按结构做一次数量守卫。
func TestInsertCallLogSQL_ColumnPlaceholderCount(t *testing.T) {
	colList := extractBetween(t, insertCallLogSQL, "call_logs(", ")")
	valList := extractBetween(t, insertCallLogSQL, "VALUES(", ")")

	cols := strings.Split(colList, ",")
	vals := strings.Split(valList, ",")
	if len(cols) != len(vals) {
		t.Fatalf("列数 %d 与占位符数 %d 不一致（会导致写调用日志全部失败）:\n%s", len(cols), len(vals), insertCallLogSQL)
	}
	// 占位符必须是 $1..$N 的连续序列（跳号同样会让参数绑定错位）。
	for i, v := range vals {
		want := "$" + strconv.Itoa(i+1)
		if strings.TrimSpace(v) != want {
			t.Fatalf("第 %d 个占位符应为 %s，实际 %s", i+1, want, strings.TrimSpace(v))
		}
	}
}

// extractBetween 取 s 中 start 与 end 之间（不含两端）的首段内容。
func extractBetween(t *testing.T, s, start, end string) string {
	t.Helper()
	i := strings.Index(s, start)
	if i < 0 {
		t.Fatalf("未找到 %q:\n%s", start, s)
	}
	rest := s[i+len(start):]
	j := strings.Index(rest, end)
	if j < 0 {
		t.Fatalf("未找到 %q:\n%s", end, s)
	}
	return rest[:j]
}
