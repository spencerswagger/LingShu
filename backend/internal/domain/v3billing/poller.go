package v3billing

// V3 旁路计费：每 60s 把「已入库但未计费」的账本行（ledger_entries）记成账单。
// 营收=req_model 对应对外模型的售价；成本=provider+model 对应对内模型 cost_rates（缺则回落账本 cost）。
// 幂等：billing_records.magpie_request_id = ledger_id 唯一。
import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/magpie"
)

// Poller 定时结算。
type Poller struct {
	db       *sql.DB
	bill     *billing.Service
	keySync  *magpie.CallerKeySync
	log      *slog.Logger
	interval time.Duration
	batch    int
}

func NewPoller(db *sql.DB, bill *billing.Service, keySync *magpie.CallerKeySync, log *slog.Logger) *Poller {
	return &Poller{db: db, bill: bill, keySync: keySync, log: log, interval: 60 * time.Second, batch: 200}
}

// SetKeySync 注入 callerKey 同步器（余额≤0 时删代理凭据）；装配顺序晚于构造，故单独 setter。
func (p *Poller) SetKeySync(k *magpie.CallerKeySync) { p.keySync = k }

// Run 阻塞循环；ctx 取消退出。
func (p *Poller) Run(ctx context.Context) {
	t := time.NewTicker(p.interval)
	defer t.Stop()
	for {
		if n, err := p.Once(ctx); err != nil {
			p.log.Error("V3 计费轮次失败", "err", err, "settled", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type row struct {
	ledgerID    string
	occurredAt  time.Time
	provider    string
	model       string
	reqModel    string
	callerKey   string
	tokensIn    int64
	tokensOut   int64
	cacheRead   int64
	cacheWrite  int64
	costCredits float64
}

// Once 结算一轮，返回本轮入账条数。
func (p *Poller) Once(ctx context.Context) (int, error) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT e.ledger_id, e.occurred_at, e.provider, e.model, e.req_model, e.caller_key_name,
		       e.tokens_in, e.tokens_out, e.cache_read, e.cache_write, e.cost_credits
		  FROM ledger_entries e
		 WHERE NOT EXISTS (SELECT 1 FROM billing_records b WHERE b.magpie_request_id = e.ledger_id)
		 ORDER BY e.occurred_at ASC, e.id ASC
		 LIMIT $1`, p.batch)
	if err != nil {
		return 0, err
	}
	var list []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.ledgerID, &r.occurredAt, &r.provider, &r.model, &r.reqModel, &r.callerKey,
			&r.tokensIn, &r.tokensOut, &r.cacheRead, &r.cacheWrite, &r.costCredits); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, r)
	}
	rows.Close()

	settled := 0
	for _, r := range list {
		ok, err := p.settleOne(ctx, r)
		if err != nil {
			p.log.Warn("单条结算失败（下轮重试）", "ledger_id", r.ledgerID, "err", err)
			continue
		}
		if ok {
			settled++
		}
	}
	return settled, nil
}

func (p *Poller) settleOne(ctx context.Context, r row) (bool, error) {
	if r.callerKey == "" {
		return false, fmt.Errorf("缺 caller_key_name，无法归属令牌")
	}
	var tokenID int64
	var userID int64
	// callerKeyName = magpie_keys.key_name；据此找令牌与用户。
	// 改名兜底：改名窗口之前的账本行是旧名快照（legacy_key_name），先按当前名查，
	// 查不到再按旧名查（0030 加列）。两步都是 LIMIT 1 且按名精确等值，不猜名。
	if err := p.db.QueryRowContext(ctx, `
		SELECT k.token_id, t.user_id FROM magpie_keys k JOIN tokens t ON t.id = k.token_id
		 WHERE k.key_name = $1 AND k.state = 'ACTIVE' LIMIT 1`, r.callerKey).Scan(&tokenID, &userID); err != nil {
		// 旧名兜底：legacy_key_name 只在改名过的行上非空（部分索引）。
		if lerr := p.db.QueryRowContext(ctx, `
			SELECT k.token_id, t.user_id FROM magpie_keys k JOIN tokens t ON t.id = k.token_id
			 WHERE k.legacy_key_name = $1 AND k.state = 'ACTIVE' LIMIT 1`, r.callerKey).Scan(&tokenID, &userID); lerr != nil {
			return false, fmt.Errorf("找不到 callerKey=%s 对应令牌：%w", r.callerKey, err)
		}
		p.log.Warn("账本行按旧名结算（改名窗口内）", "caller_key_name", r.callerKey, "ledger_id", r.ledgerID)
	}

	// 售价（req_model；为空则用 model 兜底）
	extName := r.reqModel
	if extName == "" {
		extName = r.model
	}
	var saleRaw, timeRaw, tiersRaw []byte
	var internalID string
	err := p.db.QueryRowContext(ctx, `
		SELECT sale_rates, COALESCE(time_config,'null'::jsonb), COALESCE(context_tiers,'null'::jsonb), COALESCE(magpie_model,'')
		  FROM external_models WHERE external_name = $1 AND deleted_at IS NULL LIMIT 1`, extName).
		Scan(&saleRaw, &timeRaw, &tiersRaw, &internalID)
	if err != nil {
		return false, fmt.Errorf("找不到对外模型 %s：%w", extName, err)
	}
	var saleRates map[string]float64
	_ = json.Unmarshal(saleRaw, &saleRates)
	if saleRates == nil {
		saleRates = map[string]float64{}
	}

	// 成本价（内部模型 ref = provider/model）
	ref := r.provider + "/" + r.model
	var costRaw []byte
	_ = p.db.QueryRowContext(ctx, `SELECT COALESCE(cost_rates,'{}'::jsonb) FROM internal_models WHERE ref = $1`, ref).Scan(&costRaw)
	var costRates map[string]float64
	_ = json.Unmarshal(billing.NormalizeRatesJSON(costRaw), &costRates) // 兼容历史 PascalCase 落库（写入侧已归一，这里是读取兜底）

	// tokens_in 含 cache_read+cache_write；billing.Usage 的 Input 是「纯输入（含 cache_write）」
	input := r.tokensIn - r.cacheRead
	if input < 0 {
		input = r.tokensIn
	}
	req := billing.RecordReq{
		BillingID:       "v3-" + r.ledgerID,
		UserID:          userID,
		PricingMode:     "sale",
		TokenID:         &tokenID,
		ExternalModel:   extName,
		InternalModelID: ref,
		CallTime:        r.occurredAt,
		Tokens:          billing.Usage{Input: input, Output: r.tokensOut, CacheRead: r.cacheRead},
		Rates:           billing.Rates(saleRates),
		Status:          "completed",
		Remark:          "v3-bypass",
	}
	if len(costRates) > 0 {
		req.CostRates = billing.Rates(costRates)
	}
	// 时段/分档（可空）
	if len(timeRaw) > 0 && string(timeRaw) != "null" {
		var tc billing.TimeCoeffConfig
		if json.Unmarshal(timeRaw, &tc) == nil {
			req.TimeConfig = &tc
		}
	}
	if len(tiersRaw) > 0 && string(tiersRaw) != "null" {
		var tiers []billing.TierRule
		if json.Unmarshal(tiersRaw, &tiers) == nil {
			req.ContextTiers = tiers
		}
	}

	rec, err := p.bill.Record(ctx, req)
	if err != nil {
		return false, err
	}
	// 幂等键 + 成本兜底（没填成本价时用账本 cost）
	if _, err := p.db.ExecContext(ctx, `
		UPDATE billing_records SET magpie_request_id = $1,
		       cost_credits = CASE WHEN $2::numeric > 0 THEN $2::numeric ELSE cost_credits END
		 WHERE billing_id = $3`, r.ledgerID, r.costCredits, rec.BillingID); err != nil {
		return false, err
	}

	// 余额 ≤0 → 禁用令牌并删代理 key
	if rec.BalanceAfter <= 0 {
		if _, err := p.db.ExecContext(ctx, `UPDATE tokens SET status = 'DISABLED', updated_at = now() WHERE id = $1 AND status = 'ACTIVE'`, tokenID); err == nil {
			if p.keySync != nil {
				_ = p.keySync.Remove(ctx, tokenID)
			}
			p.log.Warn("令牌余额为 0，已禁用并删除代理凭据", "token_id", tokenID, "balance_after", rec.BalanceAfter)
		}
	}
	return true, nil
}
