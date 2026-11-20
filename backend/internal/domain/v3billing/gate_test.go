package v3billing

// gate_test.go：结算链路 kill-switch（env V3_SETTLE_ENABLED，默认关）的开/关两条路径。
//
// 口径（2026-11-20 拍板）：
//   - 关（默认）= 旧链路：settleOneLegacy（= main v0.15.0-rc4 语义：名字直查、查不到
//     留行重试、无 unpriced 兜底），补收入口整体拒绝（400 rebill_disabled）；
//   - 开 = 新链路：组 ref 多形态候选 + route_id 绑定优先 + unpriced 兜底 + 补收可用。
//
// 测试纪律：同一行账单数据在开/关两种模式下的行为差（A/B）是 kill-switch 的存在意义，
// 这里用同一形状的行分别断言；随机名（rseg）避免用例特判名字。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestSettleEnabledFromEnv_Table env 判定表（fail-closed：只有显式真值才开）。
// 「ture」这类打错字必须落回旧链路——新链路写账单动钱包，不许静默启用。
func TestSettleEnabledFromEnv_Table(t *testing.T) {
	cases := []struct {
		val  string
		want bool
	}{
		{"", false}, {"0", false}, {"false", false}, {"off", false}, {"no", false},
		{"OFF", false}, {" No ", false}, {"ture", false}, {"2", false},
		{"1", true}, {"true", true}, {"on", true}, {"yes", true},
		{"YES", true}, {" True ", true},
	}
	for _, tc := range cases {
		t.Setenv(EnvSettleEnabled, tc.val)
		if got := SettleEnabledFromEnv(); got != tc.want {
			t.Fatalf("env=%q 判定 = %v，期望 %v", tc.val, got, tc.want)
		}
	}
	t.Setenv(EnvSettleEnabled, "")
}

// TestNewPoller_DefaultsToNewSettle 包内默认走新链路（既有用例不经装配层也要测新链路）；
// 生产模式只能由 cmd/server 按 env 显式设置（wiring_test 钉装配）。
func TestNewPoller_DefaultsToNewSettle(t *testing.T) {
	p := NewPoller(nil, nil, nil, nil)
	if !p.newSettle {
		t.Fatalf("包内构造默认应走新链路（生产由装配层按 env 覆盖）")
	}
}

// TestOnce_KillSwitchAB_SameRowSettlesOnlyWhenEnabled 同一行账本数据的 A/B：
// 绑定在抽屉形态（bound_internal_ref=group/auto-<id>），req_model 是网关 auto 形态。
// 关 = 旧链路名字直查（查不到 → 留行重试，零账单）；开 = 绑定候选命中（正常结算）。
// 这是「出账异常时清掉 env 重启即回到已验证口径」的可执行证据。
func TestOnce_KillSwitchAB_SameRowSettlesOnlyWhenEnabled(t *testing.T) {
	reqModel := "auto/" + rseg(t)
	served := "served-" + rseg(t)
	caller := "caller-" + rseg(t)
	rates := map[string]float64{"input": 1, "output": 2, "cache_read": 0.1}
	rowVals := []any{"lg-leg", time.Now(), "prov", served, reqModel, caller,
		int64(7), int64(100), int64(50), int64(0), int64(0), float64(0)}

	t.Run("关：旧链路名字直查不到绑定，留行重试零账单", func(t *testing.T) {
		f := newFixture(t)
		f.p.SetNewSettleEnabled(false)
		f.expectOnceRows(rowVals)
		f.expectToken(caller)
		// 旧链路唯一候选：req_model 直查 external_name；绑定列/组快照都不碰。
		f.expectPriceMiss(false, reqModel)
		if _, err := f.p.Once(context.Background()); err != nil {
			t.Fatalf("Once 失败：%v", err)
		}
		if n := f.rec.callCount(); n != 0 {
			t.Fatalf("旧链路查不到售价不得落账单，Record 调用 = %d", n)
		}
		if !strings.Contains(f.logs.String(), "旧链路") {
			t.Fatalf("旧链路重试日志应点名模式，logs=%s", f.logs.String())
		}
	})

	t.Run("开：同一行按绑定命中正常结算", func(t *testing.T) {
		f := newFixture(t)
		f.expectOnceRows(rowVals)
		f.expectGroups()
		f.expectToken(caller)
		f.expectCostMiss("prov", served)
		f.expectPriceHit(true, "group/auto-"+strings.TrimPrefix(reqModel, "auto/"), "drawer-ext", rates)
		f.expectSettled("lg-leg", 0)
		if n, err := f.p.Once(context.Background()); err != nil || n != 1 {
			t.Fatalf("Once = (%d, %v)，期望 (1, nil)", n, err)
		}
		reqs := f.rec.reqs
		if len(reqs) != 1 || reqs[0].Fail || reqs[0].ExternalModel != "drawer-ext" {
			t.Fatalf("新链路应按绑定命中结算：reqs=%+v", reqs)
		}
	})
}

// TestOnce_KillSwitchOff_NoUnpricedFallback 旧链路没有 unpriced 兜底：名字查不到就
// 留行重试（区别于新链路的「落 unpriced 账单、可补收」）。毒行不产生任何账单形态。
func TestOnce_KillSwitchOff_NoUnpricedFallback(t *testing.T) {
	f := newFixture(t)
	f.p.SetNewSettleEnabled(false)
	unknown := "no-such-" + rseg(t)
	f.expectOnceRows([]any{"lg-x", time.Now(), "prov", "served-x", unknown, "caller-x",
		int64(0), int64(10), int64(5), int64(0), int64(0), float64(0)})
	f.expectToken("caller-x")
	f.expectPriceMiss(false, unknown)
	if _, err := f.p.Once(context.Background()); err != nil {
		t.Fatalf("Once 失败：%v", err)
	}
	if n := f.rec.callCount(); n != 0 {
		t.Fatalf("旧链路查不到售价不应有任何账单（含 unpriced），Record 调用 = %d", n)
	}
}

// TestRebill_KillSwitchOff_Rejects 关闭时补收整体拒绝：Rebill 返回哨兵错误且零 DB 访问
// （补收定价逻辑 = 新链路 resolvePrice，开关关闭时放行等于把存疑逻辑重放进账本）。
func TestRebill_KillSwitchOff_Rejects(t *testing.T) {
	f := newFixture(t)
	f.p.SetNewSettleEnabled(false)
	_, err := f.p.Rebill(context.Background(), "group/"+rseg(t))
	if !errors.Is(err, ErrRebillDisabled) {
		t.Fatalf("关闭时应返回 ErrRebillDisabled，实际：%v", err)
	}
	if err := f.mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("关闭时补收不得碰库：%v", err)
	}
	// HTTP 面：400 + rebill_disabled（不是 500；这是部署形态问题，不是服务故障）。
	h := NewRebillHandler(f.p)
	rec, body := doRebillRequest(t, h, `{"Ref":"group/x"}`)
	if rec.Code != 400 || !strings.Contains(body, ReasonRebillDisabled) {
		t.Fatalf("status=%d body=%s", rec.Code, body)
	}
}

// TestRebillEntrance_MultiFormNoSnapshot 补收入口多形态归一（评审 P3）：
// 无组快照桥时，三种调用形态都要能过预检——不再被单形态归一误判成「该组没有售价」。
func TestRebillEntrance_MultiFormNoSnapshot(t *testing.T) {
	ctx := context.Background()
	rates := map[string]float64{"input": 1, "output": 1, "cache_read": 0.1}

	t.Run("裸名输入按抽屉绑定过预检", func(t *testing.T) {
		f := newFixture(t)
		bare := rseg(t)
		f.expectGroups() // 无桥快照
		// refs = [group/<bare>, auto/<bare>, group/auto-<slug(bare)>]。
		// rf1=group/<bare>：非 auto 组 ref 不反推 auto/ → 全 miss；
		// rf2=auto/<bare>：绑定候选 [group/auto-<bare>, auto/<bare>]，第二发命中抽屉绑定。
		f.expectPriceMiss(true, "group/"+bare)
		f.expectPriceMiss(false, "group/"+bare)
		f.expectPriceMiss(true, "group/auto-"+bare)
		f.expectPriceHit(true, "auto/"+bare, "drawer-ext", rates)
		f.expectScan() // 该组没有 unpriced 账单 → 空扫描，预检通过即可
		res, err := f.p.Rebill(ctx, bare)
		if err != nil {
			t.Fatalf("裸名输入无快照应能过预检（修复前 400）：%v", err)
		}
		if res.GroupRef != "group/"+bare || res.Scanned != 0 {
			t.Fatalf("GroupRef=%q Scanned=%d", res.GroupRef, res.Scanned)
		}
	})

	t.Run("auto/<m> 输入按镜像绑定过预检", func(t *testing.T) {
		f := newFixture(t)
		m := rseg(t)
		f.expectGroups()
		// refs 首位 = group/auto-<m>，绑定存镜像形态 → 首发命中。
		f.expectPriceHit(true, "group/auto-"+m, "mirror-ext", rates)
		f.expectScan()
		res, err := f.p.Rebill(ctx, "auto/"+m)
		if err != nil {
			t.Fatalf("auto/<m> 输入无快照应能过预检：%v", err)
		}
		if res.GroupRef != "group/auto-"+m {
			t.Fatalf("GroupRef=%q", res.GroupRef)
		}
	})

	t.Run("slug 输入还原不出非 slug 绑定（已知局限，桥可解）", func(t *testing.T) {
		f := newFixture(t)
		display := "Flash " + rseg(t) // 大小写+空格：非 slug 安全
		slugInput := "auto/" + groupSlug(display)
		f.expectGroups()
		// 绑定真源存的是抽屉原样形态 auto/Flash <seg>；slug 有损，从 slug 串还原不出它。
		// 该形态必须走组快照桥（display_name）——入口多形态只保证无损方向的互认。
		f.expectAllPriceMiss("group/auto-"+groupSlug(display), "", nil, 1)
		f.expectAllPriceMiss(slugInput, "", nil, 1)
		if _, err := f.p.Rebill(ctx, slugInput); !errors.Is(err, ErrRebillGroupUnpriced) {
			t.Fatalf("slug 有损方向的已知局限应整体 400（ErrRebillGroupUnpriced），实际：%v", err)
		}
	})
}
