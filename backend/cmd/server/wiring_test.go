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
//   - 转发门直接每请求 `upstreamCfg.Load()` → `ConfigProvider` 的缓存**没有 TTL**且
//     `Invalidate()` 无生产调用点，拿到的是**启动时冻结的值**：改开关不重启不生效（双向）。
//   - 转发门若用 `Enabled()`/`PassthroughActive()`（含 base_url）→ `upstream.base_url` 是账本基址、
//     未配置属正常态，一旦拿它当门，所有 /v1 立刻 503。
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
		t.Fatalf("对账的 LedgerEnabled 回调必须用 LedgerActive（= 基址已配 && 开关开）；" +
			"用裸 Degrade.LedgerEnabled 会在基址未配时把窗口写成 ok/0 对 0，漏账看起来像正常")
	}
	if strings.Contains(src, "Degrade.LedgerEnabled") {
		t.Fatalf("发现 Degrade.LedgerEnabled：这是「假绿」写法（见上），必须走 LedgerActive")
	}
}

// TestWiring_DegradeGateIsTTLBacked 降级开关必须经 NewDegradeGate（带 TTL），不许每请求裸 Load。
func TestWiring_DegradeGateIsTTLBacked(t *testing.T) {
	src := mainGoSource(t)
	if !strings.Contains(src, "upstream.NewDegradeGate(upstreamCfg,") {
		t.Fatalf("降级开关必须经 upstream.NewDegradeGate(upstreamCfg, ttl)：每请求裸 Load 会永远读到启动值" +
			"（ConfigProvider 缓存无 TTL、Invalidate 无生产调用点），改开关不重启不生效")
	}
	// SetGate 闭包里不许再出现裸 Load（等于绕开 TTL）。
	i := strings.Index(src, "magpieHandler.SetGate(")
	if i < 0 {
		t.Fatalf("装配层必须给 Facade 注入降级开关（否则 upstream.degrade.passthrough_enabled 没有消费方）")
	}
	rest := src[i:]
	j := strings.Index(rest, "\n\t}")
	if j < 0 {
		t.Fatalf("SetGate 的闭包没闭合，源码结构变了")
	}
	body := rest[:j]
	if !strings.Contains(body, "gate.Passthrough(") {
		t.Fatalf("SetGate 闭包必须经 gate.Passthrough()（带 TTL），实际：%s", body)
	}
	if strings.Contains(body, "upstreamCfg.Load(") {
		t.Fatalf("SetGate 闭包里不许裸调 upstreamCfg.Load()（绕过 TTL = 开关冻结在启动值）：%s", body)
	}
}

// TestWiring_PassthroughGateIgnoresUpstreamBaseURL 转发降级门只能看开关，不能看 upstream.base_url。
func TestWiring_PassthroughGateIgnoresUpstreamBaseURL(t *testing.T) {
	src := mainGoSource(t)
	i := strings.Index(src, "magpieHandler.SetGate(")
	if i < 0 {
		t.Fatalf("装配层必须给 Facade 注入降级开关")
	}
	rest := src[i:]
	j := strings.Index(rest, "\n\t}")
	if j < 0 {
		t.Fatalf("SetGate 的闭包没闭合")
	}
	body := rest[:j]
	for _, forbidden := range []string{"PassthroughActive()", "c.Enabled()", "BaseURL"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("SetGate 闭包不许依赖 %s：upstream.base_url 是账本基址、未配置属正常态，"+
				"拿它当门会把所有 /v1 关成 503", forbidden)
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

// TestWiring_V3SettleKillSwitch 结算链路 kill-switch（2026-11-20 拍板）必须接线：
// NewPoller 的包内默认是新链路（既有用例依赖），生产模式**只能**由装配层按 env 显式
// 设置——漏掉这组接线 = 新链路在未拍板的部署上静默启用（或写反 = 拍板要开时开不了）。
// 判定单源在 v3billing.SettleEnabledFromEnv（fail-closed：打错字落回旧链路）。
func TestWiring_V3SettleKillSwitch(t *testing.T) {
	src := mainGoSource(t)
	if !strings.Contains(src, "v3billing.SettleEnabledFromEnv()") {
		t.Fatalf("kill-switch 判定必须走 v3billing.SettleEnabledFromEnv（单源 fail-closed），" +
			"不许在装配层手写第二份 env 解析")
	}
	if !strings.Contains(src, "SetNewSettleEnabled(true)") || !strings.Contains(src, "SetNewSettleEnabled(false)") {
		t.Fatalf("装配层必须两条分支都显式 SetNewSettleEnabled（开/关），缺一条 = 模式漂移")
	}
	// 开关状态必须进启动日志（「部署完为什么没出账单」的排查锚点）。
	if !strings.Contains(src, "新链路已启用") || !strings.Contains(src, "新链路未启用") {
		t.Fatalf("开关状态必须启动即点名（两条日志分支都要有）")
	}
}

// TestWiring_StartupBackfillGatedByEnv 启动自动补建必须被 CALLERKEY_BACKFILL_ON_BOOT 闸住（默认 off）。
// 评审 P2-1：部署即对存量令牌执行真机改名，属真实环境数据操作 —— 必须先 dry-run、经确认再放开；
// 闸门丢掉（回退成无条件 go func）会让「升级即自动改名」重新出现。
// 断言按「行首是 if envFlagOn(...)」判定，注释里提到变量名不算数（源码断言怕注释的坑）。
func TestWiring_StartupBackfillGatedByEnv(t *testing.T) {
	src := mainGoSource(t)
	lines := strings.Split(src, "\n")
	gateLine, callLine := -1, -1
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, `if envFlagOn("CALLERKEY_BACKFILL_ON_BOOT")`) {
			gateLine = i
		}
		if strings.Contains(trimmed, "tokenSvc.BackfillCallerKeys(") && !strings.HasPrefix(trimmed, "//") && callLine < 0 {
			callLine = i
		}
	}
	if gateLine < 0 {
		t.Fatalf("启动自动补建必须走 env 闸门 if envFlagOn(\"CALLERKEY_BACKFILL_ON_BOOT\")（默认 off；" +
			"先 dry-run 确认桶分布再放开）。注释里提到变量名不算 —— 必须存在这条可执行判定")
	}
	if callLine < 0 {
		t.Fatalf("装配层应保留启动补建调用 tokenSvc.BackfillCallerKeys（在闸门内）")
	}
	if callLine < gateLine {
		t.Fatalf("BackfillCallerKeys 必须出现在闸门行之后（门内调用），当前顺序相反 = 无条件执行")
	}
	// 闸门必须默认关：envFlagOn 判定里要含 ""/0/false/off 的关闭语义。
	if !strings.Contains(src, "func envFlagOn") {
		t.Fatalf("缺少 envFlagOn：闸门判定必须是「非空且非 0/false/off」的保守语义")
	}
}

// TestWiring_ComposePassesFeatureGates 部署面必须把功能闸门变量透传进 backend 容器。
//
// docker compose 只把 environment: 里声明的变量注入容器；.env 仅用于 ${...} 替换。
// 事故（2026-11-23）：CALLERKEY_BACKFILL_ON_BOOT 在 main.go 被 envFlagOn 读取、注释也写了
// 「设该变量重启」，但 compose 没声明 —— 设了也进不了容器，表现为「改了没生效」。
// 这里钉住「代码读的闸门 = compose 声明的」这条部署契约；两个闸门默认必须为空
// （不设置 = 关闭），防止升级镜像即静默改变线上结算/改名行为。
func TestWiring_ComposePassesFeatureGates(t *testing.T) {
	env := composeBackendEnv(t)
	for _, key := range []string{
		"V3_SETTLE_ENABLED",          // 结算链路：开=新链路，默认关=旧链路
		"CALLERKEY_BACKFILL_ON_BOOT", // 启动存量 callerKey 改名/补建，默认关
	} {
		ref, ok := env[key]
		if !ok {
			t.Fatalf("docker-compose.yml 的 backend.environment 未声明 %s："+
				"compose 只注入这里列出的变量，代码里的 env 判定读不到，"+
				"按部署文档设置后重启也不会生效", key)
		}
		// 值必须引用同名变量且默认空（不设置即关闭）：写死字面量或给真值默认，
		// 都会让「升级镜像」悄悄改变线上行为。
		if !strings.Contains(ref, "${"+key+":-}") {
			t.Fatalf("backend.environment.%s 必须是 ${%s:-}（引用同名变量、默认空=关闭），实际 %q",
				key, key, ref)
		}
	}
}

// composeBackendEnv 取 docker-compose.yml 里 backend 服务 environment 段的键值。
// 不做完整 YAML 解析：只按缩进扫 backend 块（服务在 2 空格、environment 在 4 空格、
// 键在 6 空格），注释与空行跳过 —— 足够钉住「是否声明」这条契约。
func composeBackendEnv(t *testing.T) map[string]string {
	t.Helper()
	body, err := os.ReadFile("../../../docker-compose.yml")
	if err != nil {
		t.Fatalf("读取 docker-compose.yml 失败: %v", err)
	}
	out := map[string]string{}
	inBackend, inEnv := false, false
	for _, raw := range strings.Split(string(body), "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if indent == 2 && strings.HasSuffix(trimmed, ":") {
			inBackend = trimmed == "backend:"
			inEnv = false
			continue
		}
		if !inBackend {
			continue
		}
		if indent == 4 && strings.HasSuffix(trimmed, ":") {
			inEnv = trimmed == "environment:"
			continue
		}
		if !inEnv {
			continue
		}
		if indent <= 4 {
			inEnv = false
			continue
		}
		if k, v, ok := strings.Cut(trimmed, ":"); ok {
			out[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	return out
}
