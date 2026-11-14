// ledger_report_test.go 固化契约 §8.2 的「最小字段集 + 降级表」：
// 必需项缺任一 / 时间戳不带时区 / 分页只有 offset → 该窗口不可对账（标 blocked: pending POC，
// 既不算通过也不算不通过）；cache_read 可降级但必须显式标记缺失。
package usagebilling

import (
	"strings"
	"testing"
)

// 齐全形状：9 项都在 + 接口回了 next_cursor → 可对账。
const fullRecordBody = `{"items":[{
  "id":"req-1",
  "created_at":"2026-11-15T10:00:00+08:00",
  "caller_key_name":"k1",
  "model":"m1",
  "input_tokens":100,
  "output_tokens":40,
  "cache_read_tokens":30,
  "failed":false,
  "retried":false
}],"next_cursor":"p2"}`

func TestLedgerReport_FullShapeReconcilable(t *testing.T) {
	_, rep, err := parseLedgerBody([]byte(fullRecordBody))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !rep.Reconcilable() {
		t.Fatalf("齐全形状应可对账: reasons=%v", rep.BlockedReasons())
	}
	if rep.Paging != "cursor" {
		t.Fatalf("paging=%s", rep.Paging)
	}
	if rep.CacheReadMissing || rep.TimestampWithoutZone || rep.Records != 1 {
		t.Fatalf("report=%+v", rep)
	}
}

// 必需项缺任一 → 不可对账，原因里点名缺的项（Gate 直接断言项名）。
func TestLedgerReport_MissingRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"缺逐笔键", `{"items":[{"created_at":"2026-11-15T10:00:00Z","caller_key_name":"k","model":"m","input_tokens":1,"output_tokens":1,"failed":false,"retried":false}]}`, FieldRecordKey},
		{"缺时间戳", `{"items":[{"id":"r","caller_key_name":"k","model":"m","input_tokens":1,"output_tokens":1,"failed":false,"retried":false}]}`, FieldTimestamp},
		{"缺 caller key", `{"items":[{"id":"r","created_at":"2026-11-15T10:00:00Z","model":"m","input_tokens":1,"output_tokens":1,"failed":false,"retried":false}]}`, FieldCallerKey},
		{"缺 model", `{"items":[{"id":"r","created_at":"2026-11-15T10:00:00Z","caller_key_name":"k","input_tokens":1,"output_tokens":1,"failed":false,"retried":false}]}`, FieldModel},
		{"缺 input tokens", `{"items":[{"id":"r","created_at":"2026-11-15T10:00:00Z","caller_key_name":"k","model":"m","output_tokens":1,"failed":false,"retried":false}]}`, FieldInput},
		{"缺 output tokens", `{"items":[{"id":"r","created_at":"2026-11-15T10:00:00Z","caller_key_name":"k","model":"m","input_tokens":1,"failed":false,"retried":false}]}`, FieldOutput},
		{"缺 failed", `{"items":[{"id":"r","created_at":"2026-11-15T10:00:00Z","caller_key_name":"k","model":"m","input_tokens":1,"output_tokens":1,"retried":false}]}`, FieldFailed},
		{"缺 retried", `{"items":[{"id":"r","created_at":"2026-11-15T10:00:00Z","caller_key_name":"k","model":"m","input_tokens":1,"output_tokens":1,"failed":false}]}`, FieldRetried},
	}
	for _, c := range cases {
		_, rep, err := parseLedgerBody([]byte(c.body))
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if rep.Reconcilable() {
			t.Fatalf("%s: 缺必需项不该判可对账", c.name)
		}
		joined := strings.Join(rep.BlockedReasons(), "|")
		if !strings.Contains(joined, c.want) {
			t.Fatalf("%s: 原因=%s 未点名 %s", c.name, joined, c.want)
		}
	}
}

// cache_read 缺失可降级，但必须显式标记；其余齐 → 仍可对账。
func TestLedgerReport_CacheReadDegradable(t *testing.T) {
	body := `{"items":[{"id":"r","created_at":"2026-11-15T10:00:00Z","caller_key_name":"k","model":"m","input_tokens":10,"output_tokens":5,"failed":false,"retried":false}],"next_cursor":"p2"}`
	_, rep, err := parseLedgerBody([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !rep.CacheReadMissing {
		t.Fatal("cache_read 缺失必须显式标记")
	}
	if !rep.Reconcilable() {
		t.Fatalf("cache_read 缺失属可降级，不该判不可对账: %v", rep.BlockedReasons())
	}
}

// 时间戳不带时区 → 等价于缺时区 → 不可对账。
func TestLedgerReport_TimestampWithoutTimezone(t *testing.T) {
	body := `{"items":[{"id":"r","created_at":"2026-11-15 10:00:00","caller_key_name":"k","model":"m","input_tokens":10,"output_tokens":5,"failed":false,"retried":false}],"next_cursor":"p2"}`
	_, rep, err := parseLedgerBody([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !rep.TimestampWithoutZone || rep.Reconcilable() {
		t.Fatalf("无时区时间戳应不可对账: %+v", rep)
	}
	if !strings.Contains(strings.Join(rep.BlockedReasons(), "|"), "timestamp_without_timezone") {
		t.Fatalf("原因=%v", rep.BlockedReasons())
	}
}

// 分页只支持 offset（接口回了 total、没有 cursor 字段）→ 不可对账（不臆造 cursor 支持）。
func TestLedgerReport_OffsetPagingBlocked(t *testing.T) {
	body := `{"items":[{"id":"r","created_at":"2026-11-15T10:00:00Z","caller_key_name":"k","model":"m","input_tokens":10,"output_tokens":5,"failed":false,"retried":false}],"total":5}`
	_, rep, err := parseLedgerBody([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if rep.Paging != "offset" {
		t.Fatalf("paging=%s", rep.Paging)
	}
	if rep.Reconcilable() || !strings.Contains(strings.Join(rep.BlockedReasons(), "|"), "paging:offset") {
		t.Fatalf("offset 分页应不可对账: %+v", rep)
	}
}

// failed 单不参与 token 对比（灵枢侧也只统计 completed），但笔数记进报告。
func TestLedgerReport_FailedRecordsExcluded(t *testing.T) {
	body := `{"items":[
      {"id":"r1","created_at":"2026-11-15T10:00:00Z","caller_key_name":"k","model":"m","input_tokens":10,"output_tokens":5,"failed":false,"retried":true},
      {"id":"r2","created_at":"2026-11-15T10:00:01Z","caller_key_name":"k","model":"m","input_tokens":99,"output_tokens":99,"failed":true,"retried":false}
    ],"next_cursor":"p2"}`
	gs, rep, err := parseLedgerBody([]byte(body))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(gs) != 1 || gs[0].Input != 10 || gs[0].Requests != 1 {
		t.Fatalf("failed 单不该进聚合: %+v", gs)
	}
	if rep.FailedRecords != 1 || rep.RetriedRecords != 1 || rep.Records != 2 {
		t.Fatalf("report=%+v", rep)
	}
}

// 多批合并：任一批的记录里见到就算见到（口径：一批能证明字段存在即可）。
func TestLedgerReport_MergeBatches(t *testing.T) {
	acc := newLedgerReportAcc()
	acc.add(LedgerReport{Items: 1, Records: 1, Paging: "unknown", Missing: []string{FieldFailed}})
	acc.add(LedgerReport{Items: 2, Records: 2, Paging: "cursor"})
	m := acc.finish()
	if m.Paging != "cursor" || len(m.Missing) != 0 || m.Records != 3 || m.Items != 3 {
		t.Fatalf("merge=%+v", m)
	}
	if !m.Reconcilable() {
		t.Fatalf("两批合起来字段齐全就该可对账: %v", m.BlockedReasons())
	}
}

// 空批（items: []）不能证明也不能否定字段集：只有空批的窗口 = 未验 → 不可对账（no_samples）。
func TestLedgerReport_NoSamplesBlocked(t *testing.T) {
	_, rep, err := parseLedgerBody([]byte(`{"items":[],"next_cursor":"p2"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	acc := newLedgerReportAcc()
	acc.add(rep)
	got := acc.finish()
	if got.Reconcilable() {
		t.Fatalf("没有记录就是未验，不该判可对账: %+v", got)
	}
	joined := strings.Join(got.BlockedReasons(), "|")
	if !strings.Contains(joined, "no_samples") {
		t.Fatalf("原因应显式说明 no_samples: %s", joined)
	}
}
