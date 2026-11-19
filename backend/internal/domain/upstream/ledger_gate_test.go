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

// degradeJSONFull 生成三段开关的 JSON（passthrough/ledger/sync）。
func degradeJSONFull(passthrough, ledger, sync bool) string {
	return fmt.Sprintf(`{"passthrough_enabled":%t,"ledger_enabled":%t,"sync_enabled":%t}`,
		passthrough, ledger, sync)
}

// TestLedgerGate_TableDriven 是账本闸门的表驱动守卫（P0 回归核心）。
//
// 维度：账本端点（配/不配）× 降级开关（开/关）× upstream.base_url（空/非空）。
// 断言：
//   - 只要 UPSTREAM_LEDGER_URL 配了就拉（LedgerActive=true / ok=true），**无论** base_url 空不空、
//     降级开关开不开——base_url 与降级开关都不再是账本链路闸门；
//   - 没配必须 ok=false 且 reason 非空（装配层据此 WARN），绝不冒充成功。
func TestLedgerGate_TableDriven(t *testing.T) {
	ctx := context.Background()
	randURL := randomURL(t)
	randBase := randomURL(t)

	type tc struct {
		name      string
		ledgerURL string
		baseURL   string
		passth    bool
		ledgerOn  bool
		syncOn    bool
		want      bool
	}
	cases := []tc{
		// —— 端点已配：base_url 空/非空、开关任意组合都应「拉」——
		{"url+base空+全开", randURL, "", true, true, true, true},
		{"url+base非空+全开", randURL, randBase, true, true, true, true},
		{"url+base空+账本开关关", randURL, "", true, false, true, true},
		{"url+base非空+账本开关关", randURL, randBase, true, false, true, true},
		{"url+base空+同步开关关", randURL, "", true, true, false, true},
		{"url+base非空+同步开关关", randURL, randBase, true, true, false, true},
		{"url+base空+转发开关关", randURL, "", false, true, true, true},
		// —— 端点未配：base_url 空/非空、开关任意组合都应「不拉」——
		{"无url+base空+全开", "", "", true, true, true, false},
		{"无url+base非空+全开", "", randBase, true, true, true, false},
		{"无url+base非空+开关全关", "", randBase, false, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src := &fakeConfigSource{vals: map[string]string{
				ConfigKeyBaseURL:  fmt.Sprintf("%q", c.baseURL),
				ConfigKeyTimeouts: `{"stream_first_byte_ms":30000,"stream_idle_ms":120000}`,
				ConfigKeyDegrade:  degradeJSONFull(c.passth, c.ledgerOn, c.syncOn),
			}}
			gate := NewDegradeGate(NewConfigProvider(src), c.ledgerURL, 0)

			if got := gate.LedgerActive(ctx); got != c.want {
				t.Fatalf("LedgerActive=%v，期望 %v（ledgerURL=%q baseURL=%q degrade={p:%v,l:%v,s:%v}）",
					got, c.want, c.ledgerURL, c.baseURL, c.passth, c.ledgerOn, c.syncOn)
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

// TestLedgerGate_BaseURLIsIrrelevant 显式钉住：base_url 从前是闸门，现在完全不影响账本判定。
func TestLedgerGate_BaseURLIsIrrelevant(t *testing.T) {
	ctx := context.Background()
	url := randomURL(t)
	deg := degradeJSONFull(true, true, true)

	withBase := &fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL: fmt.Sprintf("%q", randomURL(t)), ConfigKeyDegrade: deg,
	}}
	noBase := &fakeConfigSource{vals: map[string]string{
		ConfigKeyBaseURL: `""`, ConfigKeyDegrade: deg,
	}}
	g1 := NewDegradeGate(NewConfigProvider(withBase), url, 0)
	g2 := NewDegradeGate(NewConfigProvider(noBase), url, 0)
	if !g1.LedgerActive(ctx) || !g2.LedgerActive(ctx) {
		t.Fatalf("base_url 空/非空都必须判为可用（base_url 已不参与账本链路）")
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
