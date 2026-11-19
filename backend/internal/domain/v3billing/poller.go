package v3billing

// V3 旁路计费：每 60s 把「已入库但未计费」的账本行（ledger_entries）记成账单。
//
// 营收 = 账本行的模型（req_model/served_model/组 ref）解析到的**对外模型售价**；
// 成本 = provider+model 对应的**对内模型 cost_rates**（缺则回落账本的 cost）。
//
// 解析不到对外模型的账本行**不许堵批次**：照样落一张 unpriced 账单（营收 0、成本按账本 cost
// 兜底、原因写 ErrorMessage），然后继续结算后面的行。否则这些行每轮都排在批次头位，
// 会让正常账单一条都出不来（真机事故，2026-11-19）。
//
// 幂等：billing_records.magpie_request_id = ledger_id 唯一。
import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/domain/magpie"
)

// recorder 是账单写入的最小依赖（*billing.Service 实现；抽出接口只为可测）。
type recorder interface {
	Record(ctx context.Context, req billing.RecordReq) (*billing.Record, error)
}

// Poller 定时结算。
type Poller struct {
	db       *sql.DB
	bill     recorder
	keySync  *magpie.CallerKeySync
	log      *slog.Logger
	interval time.Duration
	batch    int
}

// NewPoller 创建结算轮询器。
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
	routeID     int64
	tokensIn    int64
	tokensOut   int64
	cacheRead   int64
	cacheWrite  int64
	costCredits float64
}

// Once 结算一轮，返回本轮入账条数（含 unpriced 行）。
func (p *Poller) Once(ctx context.Context) (int, error) {
	rows, err := p.db.QueryContext(ctx, `
		SELECT e.ledger_id, e.occurred_at, e.provider, e.model, e.req_model, e.caller_key_name, e.route_id,
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
		if err := rows.Scan(&r.ledgerID, &r.occurredAt, &r.provider, &r.model, &r.reqModel, &r.callerKey, &r.routeID,
			&r.tokensIn, &r.tokensOut, &r.cacheRead, &r.cacheWrite, &r.costCredits); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, r)
	}
	rows.Close()
	if len(list) == 0 {
		return 0, nil
	}

	// 组快照每轮读一次（不是每行一次）：它只用来把「人可读名」映射到组 ref。
	// 读不到不阻塞结算 —— 直接模型仍能按名字命中，组路由行退化成 unpriced（可观测，不堵批次）。
	groups, err := p.loadGroupNodes(ctx)
	if err != nil {
		p.log.Warn("V3 结算：读取组快照失败，本轮按「无组快照」解析", "err", err)
		groups = nil
	}

	settled, unpriced := 0, 0
	for _, r := range list {
		got, isUnpriced, err := p.settleOne(ctx, r, groups)
		if err != nil {
			p.log.Warn("单条结算失败（下轮重试）", "ledger_id", r.ledgerID, "route_id", r.routeID, "err", err)
			continue
		}
		if got {
			settled++
		}
		if isUnpriced {
			unpriced++
		}
	}
	if unpriced > 0 {
		p.log.Warn("V3 结算：有账本行没有定价口径，已落 unpriced 账单（不重试，定价后只影响新账）",
			"unpriced", unpriced, "settled", settled, "round_rows", len(list))
	}
	return settled, nil
}

// loadGroupNodes 读组快照（internal_models 里 kind='group' 的行）。
// 不按 stale 过滤：售价是配置事实，与网关此刻是否还看得见这个组无关。
func (p *Poller) loadGroupNodes(ctx context.Context) ([]groupNode, error) {
	rows, err := p.db.QueryContext(ctx, `SELECT ref, display_name FROM internal_models WHERE kind = 'group'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]groupNode, 0, 16)
	for rows.Next() {
		var g groupNode
		if err := rows.Scan(&g.Ref, &g.DisplayName); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// priceInfo 是解析到的售价来源。
type priceInfo struct {
	externalName string // 命中的对外模型名（账单展示用；不是候选值）
	saleRates    map[string]float64
	timeRaw      []byte
	tiersRaw     []byte
}

// resolvePrice 按候选链找售价来源。返回 (nil, 试过的候选, nil) 表示一个都没命中（= 未定价）。
// 只有数据库出错才返回 error（那属于可重试故障，不是「没定价」）。
func (p *Poller) resolvePrice(ctx context.Context, r row, groups []groupNode) (*priceInfo, []string, error) {
	cands := priceCandidates(r.reqModel, r.model, groups, r.routeID)
	tried := make([]string, 0, len(cands))
	for _, c := range cands {
		tried = append(tried, c.value)
		query := extByNameSQL
		if c.byBinding {
			query = extByBindingSQL
		}
		var info priceInfo
		var saleRaw []byte
		err := p.db.QueryRowContext(ctx, query, c.value).Scan(&info.externalName, &saleRaw, &info.timeRaw, &info.tiersRaw)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, tried, fmt.Errorf("查询对外模型 %s 失败：%w", c.value, err)
		}
		_ = json.Unmarshal(saleRaw, &info.saleRates)
		if info.saleRates == nil {
			info.saleRates = map[string]float64{}
		}
		return &info, tried, nil
	}
	return nil, tried, nil
}

// 名字匹配与绑定匹配分两条语句：绑定匹配走 uq_external_models_bound_ref（部分唯一索引），
// 名字匹配走 uq_external_models_active。两条都排除软删行。
const extByNameSQL = `
	SELECT external_name, sale_rates, COALESCE(time_config,'null'::jsonb), COALESCE(context_tiers,'null'::jsonb)
	  FROM external_models WHERE external_name = $1 AND deleted_at IS NULL LIMIT 1`

const extByBindingSQL = `
	SELECT external_name, sale_rates, COALESCE(time_config,'null'::jsonb), COALESCE(context_tiers,'null'::jsonb)
	  FROM external_models WHERE bound_internal_ref = $1 AND deleted_at IS NULL LIMIT 1`

func (p *Poller) settleOne(ctx context.Context, r row, groups []groupNode) (settled bool, unpriced bool, err error) {
	if r.callerKey == "" {
		return false, false, fmt.Errorf("缺 caller_key_name，无法归属令牌")
	}
	var tokenID int64
	var userID int64
	// callerKeyName = magpie_keys.key_name；据此找令牌与用户
	if err := p.db.QueryRowContext(ctx, `
		SELECT k.token_id, t.user_id FROM magpie_keys k JOIN tokens t ON t.id = k.token_id
		 WHERE k.key_name = $1 AND k.state = 'ACTIVE' LIMIT 1`, r.callerKey).Scan(&tokenID, &userID); err != nil {
		return false, false, fmt.Errorf("找不到 callerKey=%s 对应令牌：%w", r.callerKey, err)
	}

	// 成本价（内部模型 ref = provider/model）
	internalRef := r.provider + "/" + r.model
	var costRaw []byte
	_ = p.db.QueryRowContext(ctx, `SELECT COALESCE(cost_rates,'{}'::jsonb) FROM internal_models WHERE ref = $1`, internalRef).Scan(&costRaw)
	var costRates map[string]float64
	_ = json.Unmarshal(billing.NormalizeRatesJSON(costRaw), &costRates) // 兼容历史 PascalCase 落库（写入侧已归一，这里是读取兜底）

	// tokens_in 含 cache_read+cache_write；billing.Usage 的 Input 是「纯输入（含 cache_write）」
	input := r.tokensIn - r.cacheRead
	if input < 0 {
		input = r.tokensIn
	}
	usage := billing.Usage{Input: input, Output: r.tokensOut, CacheRead: r.cacheRead}

	req := billing.RecordReq{
		BillingID:   "v3-" + r.ledgerID,
		UserID:      userID,
		PricingMode: "sale",
		TokenID:     &tokenID,
		CallTime:    r.occurredAt,
		Tokens:      usage,
		Remark:      "v3-bypass",
	}
	if len(costRates) > 0 {
		req.CostRates = billing.Rates(costRates)
	}

	price, tried, err := p.resolvePrice(ctx, r, groups)
	if err != nil {
		return false, false, err
	}
	if price != nil {
		req.ExternalModel = price.externalName
		req.InternalModelID = internalRef
		req.Rates = billing.Rates(price.saleRates)
		req.Status = billing.StatusCompleted
		// 时段/分档（可空）
		if len(price.timeRaw) > 0 && string(price.timeRaw) != "null" {
			var tc billing.TimeCoeffConfig
			if json.Unmarshal(price.timeRaw, &tc) == nil {
				req.TimeConfig = &tc
			}
		}
		if len(price.tiersRaw) > 0 && string(price.tiersRaw) != "null" {
			var tiers []billing.TierRule
			if json.Unmarshal(price.tiersRaw, &tiers) == nil {
				req.ContextTiers = tiers
			}
		}
	} else {
		// 未定价：照样出账单（否则这行永远占着批次头位，把后面的正常账单全部堵死）。
		// 走 Fail 通道 = 不扣积分、不读 R/系数，金额恒 0；成本由下面那条 UPDATE 用账本 cost 兜底。
		req.Fail = true
		req.Status = billing.StatusUnpriced
		req.ExternalModel = unpricedName(r)
		req.InternalModelID = internalRef
		req.ErrorMessage = unpricedReason(r, tried)
		p.log.Warn("V3 结算：账本行没有定价口径，落 unpriced 账单",
			"ledger_id", r.ledgerID, "route_id", r.routeID,
			"req_model", r.reqModel, "served_model", r.model, "tried", tried)
	}

	rec, err := p.bill.Record(ctx, req)
	if err != nil {
		return false, false, err
	}
	// 幂等键 + 成本兜底（没填成本价时用账本 cost；用了账本成本就把来源标成 ledger）
	if _, err := p.db.ExecContext(ctx, `
		UPDATE billing_records SET magpie_request_id = $1,
		       cost_credits = CASE WHEN $2::numeric > 0 THEN $2::numeric ELSE cost_credits END,
		       cost_source  = CASE WHEN $2::numeric > 0 THEN 'ledger' ELSE cost_source END
		 WHERE billing_id = $3`, r.ledgerID, r.costCredits, rec.BillingID); err != nil {
		return false, false, err
	}

	if price == nil {
		// unpriced 的 BalanceAfter 恒 0（Fail 通道不读钱包），**不能**据此禁用令牌：
		// 余额 0 是「没计费」的形态，不是「没钱」。
		return true, true, nil
	}
	// 余额 ≤0 → 禁用令牌并删代理 key（只对正常计费路径生效）
	if rec.BalanceAfter <= 0 {
		if _, err := p.db.ExecContext(ctx, `UPDATE tokens SET status = 'DISABLED', updated_at = now() WHERE id = $1 AND status = 'ACTIVE'`, tokenID); err != nil {
			// 禁用失败不许吞掉：余额 0 的令牌还能继续打代理，漏一条告警就是漏一笔坏账。
			p.log.Warn("禁用令牌失败（余额 0）", "token_id", tokenID, "err", err)
		} else {
			if p.keySync != nil {
				_ = p.keySync.Remove(ctx, tokenID)
			}
			p.log.Warn("令牌余额为 0，已禁用并删除代理凭据", "token_id", tokenID, "balance_after", rec.BalanceAfter)
		}
	}
	return true, false, nil
}

// unpricedName 是账单上「尽力给出的模型名」：req_model 优先，其次 served_model。
// billing_records.external_model_name 是 NOT NULL，不能留空。
func unpricedName(r row) string {
	for _, v := range []string{r.reqModel, r.model} {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return "unknown"
}

// unpricedReason 是运营在账单上看到的原文：哪一行、什么名字、试过哪些候选。
// 只写事实，不给「组一定存在/一定该定价」的断言（名字可能是客户端随手拼的）。
func unpricedReason(r row, tried []string) string {
	var b strings.Builder
	b.WriteString("未定价组：")
	b.WriteString(unpricedName(r))
	if r.model != "" && r.model != r.reqModel {
		b.WriteString("（served=" + r.model + "）")
	}
	if r.routeID > 0 {
		b.WriteString(fmt.Sprintf("（route_id=%d）", r.routeID))
	}
	if len(tried) > 0 {
		b.WriteString("；已尝试：" + strings.Join(tried, ", "))
	}
	return b.String()
}
