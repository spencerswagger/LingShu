package main

import (
	"os"
	"strings"
	"testing"
)

// 装配层守卫（源码级）：这两类接错，单测和门禁都发现不了，只有真机跑几分钟才看得出来。
//
// 实测代价（复审 #84 两轮）：
//   - 对账用裸的 `Degrade.LedgerEnabled` → `upstream.base_url` 默认空串时账本**从没拉过**，
//     却每 60s 把窗口写成 `status=ok`、0 对 0 —— 漏账看起来像正常（0023/D4 要防的假绿）。
//
// V3 起逐请求代理链与预扣已下线：/v1 由 nginx 直连代理侧网关端口，转发降级开关
// （upstream.degrade.passthrough_enabled）随代理链移除，不再有 SetGate 这类消费方。
// 账本链路可用性只看环境变量 UPSTREAM_LEDGER_URL（upstream.LedgerGate）。
//
// `cmd/server` 没有可注入的运行时用例，所以钉成源码级断言：改错即红。

func mainGoSource(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读取 main.go 失败: %v", err)
	}
	return string(body)
}

// TestWiring_ReconcileLedgerGateUsesLedgerActive 对账的「账本可用」判定必须走 LedgerActive。
func TestWiring_ReconcileLedgerGateUsesLedgerActive(t *testing.T) {
	src := mainGoSource(t)
	if !strings.Contains(src, "gate.LedgerActive(") && !strings.Contains(src, "c.LedgerActive()") {
		t.Fatalf("对账的 LedgerEnabled 回调必须用 LedgerActive（= UPSTREAM_LEDGER_URL 已配置）；" +
			"用裸 Degrade.LedgerEnabled 会在基址未配时把窗口写成 ok/0 对 0，漏账看起来像正常")
	}
	if strings.Contains(src, "Degrade.LedgerEnabled") {
		t.Fatalf("发现 Degrade.LedgerEnabled：这是「假绿」写法（见上），必须走 LedgerActive")
	}
}

// TestWiring_LedgerGateIsEnvBased 账本可用性闸门只认 UPSTREAM_LEDGER_URL（env），
// 不许把已下线的 sys_configs 运行参数（upstream.base_url / degrade）接回来当判据。
func TestWiring_LedgerGateIsEnvBased(t *testing.T) {
	src := mainGoSource(t)
	if !strings.Contains(src, "upstream.NewLedgerGate(ledgerURL)") {
		t.Fatalf("账本闸门必须经 upstream.NewLedgerGate(ledgerURL)（env UPSTREAM_LEDGER_URL 唯一判据）")
	}
	for _, forbidden := range []string{"NewDegradeGate", "upstream.NewConfigProvider", "Passthrough("} {
		if strings.Contains(src, forbidden) {
			t.Fatalf("装配层出现 %s：V2 转发降级开关链已随代理链下线，账本可用性只看 UPSTREAM_LEDGER_URL", forbidden)
		}
	}
}

// TestWiring_PollerTriggeredAfterLedgerRound 拉完即结：账本同步轮成功后必须立即触发
// v3Poller.Once —— 否则「拉取 60s + 结算 60s」串行两跳，端到端账单耗时会在 120s 验收线上抖。
func TestWiring_PollerTriggeredAfterLedgerRound(t *testing.T) {
	src := mainGoSource(t)
	if !strings.Contains(src, "settleNow()") {
		t.Fatalf("账本同步轮成功后必须调用 settleNow()（拉完即结），" +
			"否则结算要再等 poller 自己的 60s ticker，端到端耗时抖过 120s")
	}
	// 触发点必须真的在同步轮的函数体里（检查 settleNow 定义与调用在同一 goroutine 段落）。
	if !strings.Contains(src, "v3Poller.Once(context.Background())") {
		t.Fatalf("settleNow 必须调 v3Poller.Once（账本拉完立即结算）")
	}
}
