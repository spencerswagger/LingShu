package upstream

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// randomURL 造一个随机账本端点（用例不按具体值特判，杜绝「改回旧常量就绿」）。
func randomURL(t *testing.T) string {
	t.Helper()
	b := make([]byte, 8)
	if _, err := rand.Read(b); err != nil {
		t.Fatalf("生成随机端点失败: %v", err)
	}
	return fmt.Sprintf("http://%s.internal:%d", hex.EncodeToString(b), 30000+int(b[0])%2000)
}

// TestLedgerGate_TableDriven 是账本闸门的表驱动守卫（P0 回归核心）。
//
// 断言：只要 UPSTREAM_LEDGER_URL 配了就拉（LedgerActive=true / ok=true），
// 没配必须 ok=false 且 reason 非空（装配层据此 WARN），绝不冒充成功。
func TestLedgerGate_TableDriven(t *testing.T) {
	ctx := context.Background()
	randURL := randomURL(t)

	type tc struct {
		name      string
		ledgerURL string
		want      bool
	}
	cases := []tc{
		{"端点已配", randURL, true},
		{"端点未配", "", false},
		{"端点全空白", "   ", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gate := NewLedgerGate(c.ledgerURL)

			if got := gate.LedgerActive(ctx); got != c.want {
				t.Fatalf("LedgerActive=%v，期望 %v（ledgerURL=%q）", got, c.want, c.ledgerURL)
			}
			ok, reason := gate.LedgerAvailability()
			if ok != c.want {
				t.Fatalf("LedgerAvailability.ok=%v，期望 %v", ok, c.want)
			}
			if c.want {
				if reason != "" {
					t.Fatalf("配了端点不应有「不可跑」原因，got %q", reason)
				}
			} else {
				if strings.TrimSpace(reason) == "" {
					t.Fatalf("没配端点必须给出原因（否则装配层无从 WARN），got 空")
				}
				if !strings.Contains(reason, "UPSTREAM_LEDGER_URL") {
					t.Fatalf("原因应点名 UPSTREAM_LEDGER_URL，got %q", reason)
				}
			}
		})
	}
}

// TestEmptyRoundWarner 连续空轮告警：达阈值提示一次、报错/非空清零、带最近状态与错误。
func TestEmptyRoundWarner(t *testing.T) {
	w := NewEmptyRoundWarner(5)

	// 前 4 轮空 → 不提示。
	for i := 0; i < 4; i++ {
		if w.Observe(true, 200, nil) {
			t.Fatalf("第 %d 轮空不应告警（阈值 5）", i+1)
		}
	}
	if w.Streak() != 4 {
		t.Fatalf("连续空轮应为 4，got %d", w.Streak())
	}
	// 第 5 轮空 → 提示一次。
	if !w.Observe(true, 200, nil) {
		t.Fatalf("第 5 轮空应提示「疑似未通」")
	}
	if w.LastStatus() != 200 {
		t.Fatalf("应带最近 HTTP 状态 200，got %d", w.LastStatus())
	}
	// 第 6 轮仍空但已提示过 → 不重复刷。
	if w.Observe(true, 200, nil) {
		t.Fatalf("已提示过不应每轮重复告警（一条即可）")
	}

	// 一轮报错 → 连击清零、记下错误。
	boom := errors.New("connection refused")
	if w.Observe(false, 0, boom) {
		t.Fatalf("报错那轮不应走空轮告警")
	}
	if w.Streak() != 0 || w.LastError() == nil {
		t.Fatalf("报错应清零连击并记录错误，got streak=%d err=%v", w.Streak(), w.LastError())
	}

	// 再连续 5 轮空 → 重新提示，并带上上一次错误。
	for i := 0; i < 4; i++ {
		w.Observe(true, 0, nil)
	}
	if !w.Observe(true, 502, nil) {
		t.Fatalf("重新累计到阈值应再次提示")
	}
	if w.LastStatus() != 502 {
		t.Fatalf("应带最近 HTTP 状态 502，got %d", w.LastStatus())
	}
	if w.LastError() == nil {
		t.Fatalf("应保留最近一次错误用于告警上下文")
	}

	// 拉到数据（非空）→ 清零并清错误。
	if w.Observe(false, 200, nil) {
		t.Fatalf("非空轮不应告警")
	}
	if w.Streak() != 0 || w.LastError() != nil {
		t.Fatalf("非空轮应清零连击与错误")
	}
}
