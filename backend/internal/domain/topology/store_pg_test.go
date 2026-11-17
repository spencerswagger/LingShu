package topology

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"
)

func seedTopology(t *testing.T, conn *sql.DB) {
	t.Helper()
	ctx := context.Background()
	// 用户/账单（第 2 层的来源之一）
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO users(id, username, password_hash, role) VALUES (1, 'u1', 'x', 'DEVELOPER')`); err != nil {
		t.Fatalf("造用户失败：%v", err)
	}
	// 内部模型镜像（第 1、3 层）
	for i, ref := range []string{"acme/gpt-x", "acme/gpt-y", "borealis/claude-z"} {
		provider := "acme"
		if i == 2 {
			provider = "borealis"
		}
		if _, err := conn.ExecContext(ctx,
			`INSERT INTO internal_models(ref, provider_ref, kind, display_name, stale)
			 VALUES ($1, $2, 'direct', $1, $3)`, ref, provider, i == 1); err != nil {
			t.Fatalf("造内部模型失败：%v", err)
		}
	}
	// 对外模型（第 4 层）：一个绑定，一个悬空绑定，一个未绑定
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO external_models(id, external_name, enabled, bound_internal_ref) VALUES
		   (300, 'gpt-x', true, 'acme/gpt-x'),
		   (301, 'claude-z', true, 'borealis/gone'),
		   (302, 'gpt-y', false, NULL)`); err != nil {
		t.Fatalf("造对外模型失败：%v", err)
	}
	// 账号三处来源：账单 / 账本 / 订阅
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO billing_records
		   (id, billing_id, user_id, pricing_mode, external_model_name, internal_model_id,
		    call_time, tokens, r_value, upstream_provider, upstream_account, upstream_model, cost_source)
		 VALUES (1, 'b1', 1, 'sale', 'gpt-x', 'acme/gpt-x', $1, '{"input":1}'::jsonb, 1,
		         'acme', 'acct-bill', 'gpt-x', 'none')`,
		time.Date(2026, 11, 16, 10, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("造账单失败：%v", err)
	}
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO ledger_entries(id, ledger_id, provider, provider_account, model,
		                            tokens_in, tokens_out, cost_credits, occurred_at)
		 VALUES (1, 'led-1', 'acme', 'acct-ledger', 'gpt-x', 1, 1, 1, $1)`,
		time.Date(2026, 11, 16, 11, 0, 0, 0, time.UTC)); err != nil {
		t.Fatalf("造账本条目失败：%v", err)
	}
	// 只出现在订阅里的账号（本期没流量）也必须出现在账号层
	if _, err := conn.ExecContext(ctx,
		`INSERT INTO subscriptions(id, provider_ref, account_ref, price_amount, billing_cycle, cycle_start)
		 VALUES (1, 'borealis', 'acct-sub', 40, 'month', DATE '2026-11-01')`); err != nil {
		t.Fatalf("造订阅失败：%v", err)
	}
}

// 四层图的第 1/2/4 层在真库上必须有数（#88 ②：只给 Items 时页面只剩供应商卡）。
func TestSQLStore_ProvidersAccountsExternals_RealSQL(t *testing.T) {
	conn := openTopologyTempDB(t)
	seedTopology(t, conn)
	ctx := context.Background()
	store := NewSQLStore(conn)

	providers, err := store.Providers(ctx)
	if err != nil {
		t.Fatalf("Providers: %v", err)
	}
	byProvider := map[string]Provider{}
	for _, p := range providers {
		byProvider[p.Provider] = p
	}
	if byProvider["acme"].Models != 2 || byProvider["acme"].Stale != 1 {
		t.Fatalf("acme 供应商聚合错误：%+v", byProvider["acme"])
	}
	if byProvider["borealis"].Models != 1 {
		t.Fatalf("borealis 供应商聚合错误：%+v", byProvider["borealis"])
	}

	accounts, err := store.Accounts(ctx)
	if err != nil {
		t.Fatalf("Accounts: %v", err)
	}
	byAccount := map[string]Account{}
	for _, a := range accounts {
		byAccount[a.Provider+"|"+a.Account] = a
	}
	if len(accounts) != 3 {
		t.Fatalf("账号层应有 3 行（账单/账本/订阅各一）：%+v", accounts)
	}
	bill, ok := byAccount["acme|acct-bill"]
	if !ok || bill.Subscribed || bill.LastSeenAt.IsZero() {
		t.Fatalf("账单侧账号错误：%+v", bill)
	}
	led, ok := byAccount["acme|acct-ledger"]
	if !ok || led.Subscribed || led.LastSeenAt.IsZero() {
		t.Fatalf("账本侧账号错误：%+v", led)
	}
	sub, ok := byAccount["borealis|acct-sub"]
	if !ok || !sub.Subscribed {
		t.Fatalf("订阅侧账号必须标 Subscribed：%+v", sub)
	}
	if !sub.LastSeenAt.IsZero() {
		t.Fatalf("没有账单痕迹的账号 LastSeenAt 必须是零值：%+v", sub)
	}

	externals, err := store.Externals(ctx)
	if err != nil {
		t.Fatalf("Externals: %v", err)
	}
	if len(externals) != 3 {
		t.Fatalf("对外模型层应有 3 行：%+v", externals)
	}
	byID := map[int64]External{}
	for _, e := range externals {
		byID[e.ID] = e
	}
	if byID[300].BoundInternalRef != "acme/gpt-x" || !byID[300].Enabled {
		t.Fatalf("绑定目标应原样返回：%+v", byID[300])
	}
	if byID[301].BoundInternalRef != "borealis/gone" {
		t.Fatalf("悬空绑定也要原样返回（由视图标记）：%+v", byID[301])
	}
	if byID[302].BoundInternalRef != "" {
		t.Fatalf("NULL 绑定必须归一成空串：%+v", byID[302])
	}
}

// T2：坐标落位能写、能覆盖；目标不存在时给业务错误且整批回滚。
func TestSQLStore_SaveLinks_RealSQL(t *testing.T) {
	conn := openTopologyTempDB(t)
	seedTopology(t, conn)
	ctx := context.Background()
	store := NewSQLStore(conn)

	n, err := store.SaveLinks(ctx, []Link{{InternalRef: "acme/gpt-x", ExternalModelID: 300, PosX: 240, PosY: 190}})
	if err != nil || n != 1 {
		t.Fatalf("首次落位：n=%d err=%v", n, err)
	}
	// 第二次是 upsert（覆盖坐标，不是报唯一键冲突）
	if _, err := store.SaveLinks(ctx, []Link{{InternalRef: "acme/gpt-x", ExternalModelID: 300, PosX: 1, PosY: 2}}); err != nil {
		t.Fatalf("重复落位应 upsert：%v", err)
	}
	links, err := store.Links(ctx)
	if err != nil {
		t.Fatalf("Links: %v", err)
	}
	if len(links) != 1 || links[0].PosX != 1 || links[0].PosY != 2 {
		t.Fatalf("坐标未覆盖：%+v", links)
	}

	// 外部模型 id 不存在 → ErrLinkTargetMissing（400），且不写任何行
	_, err = store.SaveLinks(ctx, []Link{{InternalRef: "acme/gpt-x", ExternalModelID: 999, PosX: 0, PosY: 0}})
	if !errors.Is(err, ErrLinkTargetMissing) {
		t.Fatalf("err = %v，期望 ErrLinkTargetMissing", err)
	}
	// 内部 ref 不存在 → 同样
	_, err = store.SaveLinks(ctx, []Link{{InternalRef: "acme/nope", ExternalModelID: 300, PosX: 0, PosY: 0}})
	if !errors.Is(err, ErrLinkTargetMissing) {
		t.Fatalf("err = %v，期望 ErrLinkTargetMissing", err)
	}
	links, _ = store.Links(ctx)
	if len(links) != 1 {
		t.Fatalf("失败的批次不许落库：%+v", links)
	}

	// 软删的对外模型不许落位（deleted_at 非空 = 已删）
	if _, err := conn.ExecContext(ctx, `UPDATE external_models SET deleted_at = now() WHERE id = 302`); err != nil {
		t.Fatalf("软删对外模型失败：%v", err)
	}
	if _, err := store.SaveLinks(ctx, []Link{{InternalRef: "acme/gpt-x", ExternalModelID: 302, PosX: 0, PosY: 0}}); !errors.Is(err, ErrLinkTargetMissing) {
		t.Fatalf("软删的对外模型不许落位，err = %v", err)
	}
}
