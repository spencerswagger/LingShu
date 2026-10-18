package gateway

import (
	"strconv"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/domain/router"
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

// Step 7：decision 组装为兼容扩展——被排除候选（新轨迹项）与流式诊断（新字段）
// 在克隆时不丢失；既有键名（attempts / result / order / reason 等）不变。
func TestCallLog_DecisionPreservesNewFields(t *testing.T) {
	g := &Gateway{}
	attempts := []map[string]any{
		{"order": 1, "reason": "drained", "skipped": true, "hint": "查渠道/密钥状态或手动探测"},
		{"order": 2, "reason": "上游失败", "delivered": false, "first_byte_received": false, "elapsed_ms": int64(5)},
	}
	dec := g.callLogDecision(attempts, "success", preCharge{},
		&router.RouteResult{ChannelID: 1, ChannelKeyID: 1, InternalModelID: "gpt-4-internal"})

	if dec["result"] != "success" {
		t.Fatalf("既有键 result 语义被改: %v", dec["result"])
	}
	got, ok := dec["attempts"].([]map[string]any)
	if !ok {
		t.Fatalf("attempts 类型异常: %T", dec["attempts"])
	}
	if len(got) != 3 {
		t.Fatalf("attempts want 3（2 克隆 + 当前候选）, got %d", len(got))
	}
	// 新轨迹项字段（skipped / hint）在克隆后保持。
	if got[0]["reason"] != "drained" || got[0]["skipped"] != true || got[0]["hint"] == nil {
		t.Errorf("排除项新字段丢失: %+v", got[0])
	}
	// 新诊断字段（delivered 等）在克隆后保持。
	if _, ok := got[1]["delivered"]; !ok {
		t.Errorf("流式诊断字段 delivered 丢失: %+v", got[1])
	}
	if _, ok := got[1]["elapsed_ms"]; !ok {
		t.Errorf("流式诊断字段 elapsed_ms 丢失: %+v", got[1])
	}
	// 当前候选仍按既有键组装，order 递增。
	if got[2]["order"] != 3 || got[2]["reason"] != "success" {
		t.Errorf("当前候选条目异常: %+v", got[2])
	}
}
