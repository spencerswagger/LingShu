package facadebill

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/magpie"
)

// 无 usage 不计费是本批的计费口径（口径①），但它的**代价**必须可观测（Owl 复审 P1-4）：
// 上游 2xx 却不给 usage 时，本次内容对租户是白送（预扣整笔退回、账单元）。
// 只有 `billing_records.error_message='no-usage'` 一条线索时，连「送出去多少」都量不出来，
// 所以这里钉住三件事：WARN 一定打、下行字节数一定在里面、失败原因不是 no-usage 时不许误报。

func newLogCapturingService(t *testing.T, ledger *fakeLedger, wallet *fakeWallet) (*Service, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	s := New(Options{
		Ledger: ledger,
		Wallet: wallet,
		Pricer: fakePricer{rates: billing.Rates{"input": 1, "output": 2, "cache_read": 0.1}},
		Logger: slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})),
	})
	if s == nil {
		t.Fatal("服务装配失败")
	}
	s.newID = func() string { return "bill-nousage-1" }
	return s, &buf
}

// TestOnComplete_NoUsageLogsDownstreamBytes 断言无 usage 时：整笔退预扣 + failed 账单 + 一条带字节数的 WARN。
func TestOnComplete_NoUsageLogsDownstreamBytes(t *testing.T) {
	ledger := &fakeLedger{estCredits: 4}
	wallet := &fakeWallet{}
	s, logs := newLogCapturingService(t, ledger, wallet)

	handle, deny := s.Reserve(context.Background(), reserveInput())
	if deny != nil {
		t.Fatalf("预扣不应被拒绝: %+v", deny)
	}

	s.OnComplete(context.Background(), magpie.Result{
		Billable:        true,
		Endpoint:        "chat.completions",
		ExternalModel:   "tenant-gpt",
		Status:          200,
		Streamed:        true,
		Reservation:     handle,
		HasUsage:        false,
		DownstreamBytes: 4096,
	})

	out := logs.String()
	if !strings.Contains(out, "上游未返回 usage") {
		t.Fatalf("无 usage 必须打 WARN，日志=%s", out)
	}
	if !strings.Contains(out, "downstream_bytes=4096") {
		t.Fatalf("WARN 必须带下行字节数（否则量不出白送规模），日志=%s", out)
	}
	if !strings.Contains(out, "stream=true") {
		t.Fatalf("WARN 必须标明是否流式，日志=%s", out)
	}
	// 钱：预扣整笔退回，且账单是 failed/0。
	if len(wallet.settleHolds) != 1 || wallet.settleHolds[0].Delta >= 0 {
		t.Fatalf("无 usage 必须整笔退预扣（Delta 应为负/全额退回），实际 %+v", wallet.settleHolds)
	}
	if len(failReqs(ledger)) != 1 || failReqs(ledger)[0].ErrorMessage != ReasonNoUsage {
		t.Fatalf("必须落一条 %s 的 failed 账单，实际 %+v", ReasonNoUsage, failReqs(ledger))
	}
}

// TestOnComplete_UpstreamErrorDoesNotClaimNoUsage 上游失败不许冒充「无 usage」告警（两者口径不同）。
func TestOnComplete_UpstreamErrorDoesNotClaimNoUsage(t *testing.T) {
	ledger := &fakeLedger{}
	wallet := &fakeWallet{}
	s, logs := newLogCapturingService(t, ledger, wallet)

	handle, deny := s.Reserve(context.Background(), reserveInput())
	if deny != nil {
		t.Fatalf("预扣不应被拒绝: %+v", deny)
	}
	s.OnComplete(context.Background(), magpie.Result{
		Billable: true, Endpoint: "chat.completions", ExternalModel: "tenant-gpt",
		Status: 500, Reservation: handle,
	})
	if strings.Contains(logs.String(), "上游未返回 usage") {
		t.Fatalf("5xx 属 upstream-error，不该打 no-usage 告警，日志=%s", logs.String())
	}
	if len(failReqs(ledger)) != 1 || failReqs(ledger)[0].ErrorMessage != ReasonUpstreamError {
		t.Fatalf("应落 %s 账单，实际 %+v", ReasonUpstreamError, failReqs(ledger))
	}
}

// failReqs 取 failed 账单（fakeLedger.records 里 Fail=true 的那些）。
func failReqs(f *fakeLedger) []billing.RecordReq {
	var out []billing.RecordReq
	for _, r := range f.records {
		if r.Fail {
			out = append(out, r)
		}
	}
	return out
}
