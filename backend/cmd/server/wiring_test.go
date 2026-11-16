package main

import (
	"os"
	"strings"
	"testing"
)

// 装配层守卫（源码级）：两个「开关」如果接错函数，单测和门禁都发现不了，只有真机上跑几分钟才看得出来。
//
// 这两条的实测代价（Owl 复审 #84 P1-2）：
//   - 对账用裸的 `Degrade.LedgerEnabled` → `upstream.base_url` 默认空串时账本**从没拉过**，
//     但每 60s 仍把窗口写成 `status=ok`、0 对 0 —— 漏账从此看起来像正常（正是 0023/D4 要防的假绿）。
//   - 转发用 `Enabled()`（含 base_url）当门 → 关键的是反方向：`upstream.base_url` 是账本基址、
//     不配属正常态，一旦拿它当门，所有 /v1 立刻 503。
//
// `cmd/server` 没有可注入的运行时用例，所以钉成源码级断言：改错即红，且注释里写清为什么。

func mainGoSource(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读取 main.go 失败: %v", err)
	}
	return string(body)
}

// TestWiring_ReconcileLedgerGateUsesLedgerActive 对账的「账本可用」判定必须是 LedgerActive()。
func TestWiring_ReconcileLedgerGateUsesLedgerActive(t *testing.T) {
	src := mainGoSource(t)
	if !strings.Contains(src, "c.LedgerActive()") {
		t.Fatalf("对账的 LedgerEnabled 回调必须用 c.LedgerActive()（= 基址已配 && 开关开）；" +
			"用裸 Degrade.LedgerEnabled 会在基址未配时把窗口写成 ok/0 对 0，漏账看起来像正常")
	}
	if strings.Contains(src, "return c.Degrade.LedgerEnabled") {
		t.Fatalf("发现 return c.Degrade.LedgerEnabled：这是「假绿」写法（见上），必须改成 c.LedgerActive()")
	}
}

// TestWiring_PassthroughGateIgnoresUpstreamBaseURL 转发降级门只能看开关，不能看 upstream.base_url。
func TestWiring_PassthroughGateIgnoresUpstreamBaseURL(t *testing.T) {
	src := mainGoSource(t)
	i := strings.Index(src, "magpieHandler.SetGate(")
	if i < 0 {
		t.Fatalf("装配层必须给 Facade 注入降级开关（否则 upstream.degrade.passthrough_enabled 没有消费方，开关是假的）")
	}
	rest := src[i:]
	j := strings.Index(rest, "})")
	if j < 0 {
		t.Fatalf("SetGate 的闭包没闭合，源码结构变了")
	}
	body := rest[:j]
	if !strings.Contains(body, "Degrade.PassthroughEnabled") {
		t.Fatalf("SetGate 闭包必须读 Degrade.PassthroughEnabled，实际：%s", body)
	}
	for _, forbidden := range []string{"PassthroughActive()", "c.Enabled()", "BaseURL"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("SetGate 闭包不许依赖 %s：upstream.base_url 是账本基址、未配置属正常态，"+
				"拿它当门会把所有 /v1 关成 503", forbidden)
		}
	}
}
