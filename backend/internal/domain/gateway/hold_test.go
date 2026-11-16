package gateway

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/team/llmgateway/internal/domain/identity"
	"github.com/team/llmgateway/internal/domain/upstream"
	"github.com/team/llmgateway/internal/pkg/logger"
)

// fakeHolds 记录预扣登记的三件事：建了多少钱、终结成什么状态、终结了几次。
type fakeHolds struct {
	mu       sync.Mutex
	err      error
	reserved []float64
	closed   []string
}

func (f *fakeHolds) ReserveHold(_ context.Context, billingID string, _ int64, _ int64, amount float64, _, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return f.err
	}
	if billingID == "" {
		return errors.New("登记缺少 billing_id")
	}
	f.reserved = append(f.reserved, amount)
	return nil
}

func (f *fakeHolds) CloseHold(_ context.Context, billingID, status, _ string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if billingID == "" {
		return false, errors.New("终结缺少 billing_id")
	}
	f.closed = append(f.closed, status)
	return true, nil
}

// postWithRequestID 发一次请求并把 request_id 注入 ctx（预扣登记键就是它）。
func (h *v2Harness) postWithRequestID(t *testing.T, path, body, reqID string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req = req.WithContext(logger.SetRequestID(context.Background(), reqID))
	req.Header.Set("Authorization", "Bearer good-token")
	rec := httptest.NewRecorder()
	h.gw.Handler().ServeHTTP(rec, req)
	return rec
}

// TestV2_Hold_SettledOnSuccess 锁定 A1 的正常路径：
// 有登记能力时预扣走登记（不再直接扣），账单落库后登记终结为 settled。
func TestV2_Hold_SettledOnSuccess(t *testing.T) {
	h := newV2Harness(t, upstream.DefaultRuntimeConfig())
	holds := &fakeHolds{}
	h.gw.SetCreditHolds(holds)
	body := `{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`

	rec := h.postWithRequestID(t, "/v1/chat/completions", body, "req-hold-1")
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，got %d body=%s", rec.Code, rec.Body.String())
	}
	if len(holds.reserved) != 1 {
		t.Fatalf("应建一条预扣登记，got %d", len(holds.reserved))
	}
	if len(h.credit.consumed) != 0 {
		t.Fatalf("装配了登记能力就不该再走直接扣款，got %d 次", len(h.credit.consumed))
	}
	if len(h.billing.reqs) != 1 || h.billing.reqs[0].PreConsumed != holds.reserved[0] {
		t.Fatalf("账单应带上登记的预扣金额：holds=%v billing=%+v", holds.reserved, h.billing.reqs)
	}
	if len(holds.closed) != 1 || holds.closed[0] != identity.HoldSettled {
		t.Fatalf("成功后登记应终结为 settled，got %v", holds.closed)
	}
}

// TestV2_Hold_RefundedOnFailure 锁定 A1 的失败路径：上游 5xx → 退预扣 + 登记终结为 refunded。
func TestV2_Hold_RefundedOnFailure(t *testing.T) {
	h := newV2Harness(t, upstream.DefaultRuntimeConfig())
	holds := &fakeHolds{}
	h.gw.SetCreditHolds(holds)
	h.respCode = http.StatusInternalServerError
	h.respBody = []byte(`{"error":{"message":"boom"}}`)
	body := `{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`

	rec := h.postWithRequestID(t, "/v1/chat/completions", body, "req-hold-2")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("上游 5xx 应原样透传，got %d", rec.Code)
	}
	if len(holds.reserved) != 1 {
		t.Fatalf("应建一条预扣登记，got %d", len(holds.reserved))
	}
	if len(h.credit.refunded) != 1 {
		t.Fatalf("应退回预扣，got %v", h.credit.refunded)
	}
	if len(holds.closed) != 1 || holds.closed[0] != identity.HoldRefunded {
		t.Fatalf("失败后登记应终结为 refunded，got %v", holds.closed)
	}
}

// TestV2_Hold_ReserveRejectedReturns402：登记（含扣款）失败 → 402，且不打上游。
func TestV2_Hold_ReserveRejectedReturns402(t *testing.T) {
	h := newV2Harness(t, upstream.DefaultRuntimeConfig())
	holds := &fakeHolds{err: errors.New("insufficient")}
	h.gw.SetCreditHolds(holds)
	body := `{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`

	rec := h.postWithRequestID(t, "/v1/chat/completions", body, "req-hold-3")
	if rec.Code != http.StatusPaymentRequired {
		t.Fatalf("预扣失败应为 402，got %d", rec.Code)
	}
	if len(h.billing.reqs) != 0 {
		t.Fatalf("预扣失败不该落账单，got %d", len(h.billing.reqs))
	}
	if len(holds.closed) != 0 {
		t.Fatalf("没建成登记就不该有终结调用，got %v", holds.closed)
	}
}

// TestV2_Hold_NoRequestIDFallsBackToDirectPreConsume：
// 拿不到 request_id 时不能建登记（一请求一登记的唯一键缺失），退回直接扣款 —— 行为必须仍是「扣过一次」。
func TestV2_Hold_NoRequestIDFallsBackToDirectPreConsume(t *testing.T) {
	h := newV2Harness(t, upstream.DefaultRuntimeConfig())
	holds := &fakeHolds{}
	h.gw.SetCreditHolds(holds)
	body := `{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`

	rec := h.post(t, "/v1/chat/completions", body, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码应为 200，got %d", rec.Code)
	}
	if len(holds.reserved) != 0 {
		t.Fatalf("无 request_id 时不该建登记，got %v", holds.reserved)
	}
	if len(h.credit.consumed) != 1 {
		t.Fatalf("无 request_id 时应退回直接扣款，got %v", h.credit.consumed)
	}
}
