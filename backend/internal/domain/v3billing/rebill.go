package v3billing

// rebill.go：unpriced 账单补收（B 方案，手动触发）。
//
// 背景：主链（poller.go）把「解析不到定价口径」的账本行落成 unpriced 账单（营收 0、不扣钱）。
// 运营在拓扑页给组定价后，这批历史账单按当前售价补收。
//
// 规则（spencer 拍板，不可偏离）：
//  1. 补开**新账单**、不改旧行 —— 原行只允许追加标记（error_message 尾部『已补收:<新单id>』），
//     不允许改金额/状态；
//  2. 售价取**补收时刻**的当前售价（与主链共用 resolvePrice，不复制第二套定价逻辑）；
//     用量取原行 tokens；成本沿用原行 cost（不重算，避免漂移）；
//  3. 幂等：同一原行只能被补收一次，重复补收必须拒绝。
//
// 落库事实（为什么标记走 error_message）：billing_records **没有 remark 列**；主链把
// unpriced 原因写在 error_message（账单详情原文可见）。补收沿用同一通道：
//   - 新账单 error_message = 『补收 unpriced <原账单id>』（任务口径原文）；
//   - 原行 error_message 尾部追加『；已补收:<新账单id>』。
//
// 幂等三层（任何一层都独立成立，任何时点中断都可安全重放——重放不会重复扣款）：
//  a. 新账单 billing_id = rebill-<原billing_id>-v1 **固定派生**：billing_id 唯一索引保证
//     同一原行的第二张补收单必然撞 23505（Record 的事务路径撞库时扣款随事务回滚，不重复扣）；
//  b. 扣款与账单同事务：Record（SetTxBeginner 已装配）内部原子提交「扣款+流水+账单」，
//     补收逐行调用，行与行之间互不牵连；
//  c. 原行标记守卫 UPDATE（WHERE status='unpriced' AND error_message NOT LIKE '%已补收:%'）：
//     RowsAffected==0 = 标记已存在 → 拒绝（防「标记在而账单不在」的畸形数据被二次扣款）。
//
// 匹配范围（口径与主链同源，复用 groupRefForms/groupMatches/resolvePrice）：
//   组名/别名匹配（组 ref、去前缀组 id、auto/<model> slug 归一、组快照 display_name 双向）
//   + route_id 映射（route_id>0 时绑定候选优先 —— 权威映射在网关内存里、镜像不到，与主链口径一致：
//   扫描侧 route_id 不引入额外名字，售价解析侧按主链候选顺序生效）。
//
// 原子性取舍（刻意不开外层大事务）：补收是运营手动动作、逐行金额独立；行级自愈重放
// （a+b+c）已消灭「重复扣款/重复开单」，而外层事务会把长时间行锁摊到整批扫描行上，
// 与 60s 结算轮争锁。失败行随报错返回，已成功行保留（响应里可见），修复后重放补齐。

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/dbx"
)

// dbxExecer 是 dbx.Execer 的包内别名（标记写入要与生产事务/裸连接共用同一签名）。
type dbxExecer = dbx.Execer

// 标记与文案锚点：守卫 LIKE、追加、扫描过滤用同一常量，改文案三处一起改。
const (
	rebillMarkPrefix = "已补收:"
	rebillMarkSep    = "；"
	rebillNewPrefix  = "rebill-"
	rebillNewSuffix  = "-v1"
	// rebillNewRemark 新账单的『Remark』口径原文（落 error_message，见文件头说明）。
	rebillNewRemark = "补收 unpriced "
)

// 可分类错误（handler 据此映射状态码/Reason）。
var (
	// ErrRebillGroupUnpriced 该组当前没有售价：先定价再补收（客户端问题 → 400）。
	ErrRebillGroupUnpriced = errors.New("该组当前没有可用的售价口径，请先在拓扑页绑定售价后再补收")
	// ErrRebillRowUnresolvable 个别行按当前配置仍解析不到售价（客户端问题 → 400）。
	ErrRebillRowUnresolvable = errors.New("按当前配置仍解析不到售价")
)

// rebillRow 是扫描到的一条待补收 unpriced 账单（req/served/route_id 来自账本行 JOIN）。
type rebillRow struct {
	BillingID       string
	UserID          int64
	TokenID         *int64
	ExternalModel   string
	InternalModelID string
	Tokens          billing.Usage
	CostCredits     *float64
	ErrorMessage    string
	// 账本侧原始解析输入（LEFT JOIN ledger_entries，缺账本行时为空 → 回落 external_model_name）。
	ReqModel    string
	ServedModel string
	RouteID     int64
}

// rebillNewBillingID 由原账单号固定派生新账单号（重放幂等的根基）。
func rebillNewBillingID(origBillingID string) string {
	return rebillNewPrefix + origBillingID + rebillNewSuffix
}

// RebillResult 是一次补收的汇总（前端弹窗展示条数与金额合计）。
type RebillResult struct {
	GroupRef      string   `json:"GroupRef"`
	Scanned       int      `json:"Scanned"`
	Rebilled      int      `json:"Rebilled"`
	TotalCredits  float64  `json:"TotalCredits"`
	NewBillingIDs []string `json:"NewBillingIDs"`
	AlreadyMarked int      `json:"AlreadyMarked"`
}

// rebillScanSQL 扫描全部 unpriced 账单 + 账本侧解析输入。
//
// 为什么全表扫 unpriced、在 Go 侧过滤匹配：匹配口径（组名形态归一/别名/快照映射）只有
// 一份实现在 Go（groupRefForms/groupMatches），SQL 里再写一遍 LIKE 就是第二套口径。
// unpriced 行的量是有界的（主链只在解析失败时落），管理端手动触发可接受。
// LEFT JOIN ledger_entries 取回当初结算时的 req_model/served_model/route_id ——
// billing_records 本身不存这三个值，账本行（magpie_request_id = ledger_id）才是原始输入。
const rebillScanSQL = `
	SELECT b.billing_id, b.user_id, b.token_id, b.external_model_name, b.internal_model_id,
	       b.tokens, b.cost_credits, COALESCE(b.error_message,''),
	       COALESCE(e.req_model,''), COALESCE(e.served_model,''), COALESCE(e.route_id,0)
	  FROM billing_records b
	  LEFT JOIN ledger_entries e ON e.ledger_id = b.magpie_request_id
	 WHERE b.status = 'unpriced'
	 ORDER BY b.call_time ASC, b.billing_id ASC`

// Rebill 对一个组执行补收：扫描 → 预检售价 → 逐条新账单（扣款同事务）→ 原行标记。
// 返回汇总；个别行失败时报错并携带已完成的进度（重放自愈，见文件头「幂等三层」）。
func (p *Poller) Rebill(ctx context.Context, groupName string) (*RebillResult, error) {
	ref := groupRefForm(groupName)
	if ref == "" {
		return nil, errors.New("组名不能为空（支持组 ref、组 id 或 auto/<model> 形态）")
	}
	res := &RebillResult{GroupRef: ref}

	groups, err := p.loadGroupNodes(ctx)
	if err != nil {
		// 快照缺失只影响「人可读名」档（同主链口径），ref/别名/route_id 档仍可用。
		p.log.Warn("V3 补收：读取组快照失败，按「无组快照」匹配", "err", err)
		groups = nil
	}

	// 预检：该组此刻是否有售价。解析输入 = ref 本身 + 组显示名（覆盖全部候选档，与行级
	// 解析同一实现 resolvePrice）。没有就整体拒绝，避免「扣了几行才发现组还没定价」。
	displayName := ""
	for _, g := range groups {
		if strings.EqualFold(strings.TrimSpace(g.Ref), ref) {
			displayName = strings.TrimSpace(g.DisplayName)
			break
		}
	}
	price, _, err := p.resolvePrice(ctx, row{reqModel: ref, model: displayName, routeID: 1}, groups)
	if err != nil {
		return nil, err
	}
	if price == nil {
		return nil, fmt.Errorf("%w：%s", ErrRebillGroupUnpriced, ref)
	}

	rows, err := p.db.QueryContext(ctx, rebillScanSQL)
	if err != nil {
		return nil, fmt.Errorf("扫描 unpriced 账单失败：%w", err)
	}
	all, err := scanRebillRows(rows)
	if err != nil {
		return nil, err
	}

	for _, r := range all {
		if !rebillMatchesRow(r, ref, groups) {
			continue
		}
		res.Scanned++
		if strings.Contains(r.ErrorMessage, rebillMarkPrefix) {
			// 扫描快照里已带标记：跳过（并发场景由行内二次判定兜底，见 rebillOne）。
			res.AlreadyMarked++
			continue
		}
		newID, credits, existed, err := p.rebillOne(ctx, r, groups)
		if err != nil {
			return res, fmt.Errorf("账单 %s 补收失败：%w", r.BillingID, err)
		}
		if existed {
			res.AlreadyMarked++
			continue
		}
		if err := markRebilled(ctx, p.db, r.BillingID, newID); err != nil {
			return res, fmt.Errorf("账单 %s 补收失败：%w", r.BillingID, err)
		}
		res.Rebilled++
		res.TotalCredits += credits
		res.NewBillingIDs = append(res.NewBillingIDs, newID)
	}

	if res.Rebilled > 0 {
		p.log.Warn("V3 补收完成",
			"group_ref", ref, "rebilled", res.Rebilled, "total_credits", res.TotalCredits,
			"already_marked", res.AlreadyMarked, "scanned", res.Scanned)
	}
	return res, nil
}

// rebillMatchesRow 判断一条 unpriced 账单是否属于目标组。
// 口径与主链候选链同源：名字形态归一（ref/组 id/auto slug）+ 组快照（display_name 双向）。
func rebillMatchesRow(r rebillRow, ref string, groups []groupNode) bool {
	for _, name := range []string{r.ReqModel, r.ServedModel, r.ExternalModel} {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		for _, f := range groupRefForms(name) {
			if strings.EqualFold(f, ref) {
				return true
			}
		}
		for _, g := range groups {
			if strings.TrimSpace(g.Ref) == ref && groupMatches(g, name) {
				return true
			}
		}
	}
	return false
}

// scanRebillRows 读取扫描结果。
func scanRebillRows(rows *sql.Rows) ([]rebillRow, error) {
	defer rows.Close()
	out := make([]rebillRow, 0, 8)
	for rows.Next() {
		var r rebillRow
		var tokensRaw []byte
		if err := rows.Scan(&r.BillingID, &r.UserID, &r.TokenID, &r.ExternalModel, &r.InternalModelID,
			&tokensRaw, &r.CostCredits, &r.ErrorMessage, &r.ReqModel, &r.ServedModel, &r.RouteID); err != nil {
			return nil, fmt.Errorf("读取 unpriced 账单失败：%w", err)
		}
		if err := json.Unmarshal(tokensRaw, &r.Tokens); err != nil {
			return nil, fmt.Errorf("解析账单 %s 的 tokens 失败：%w", r.BillingID, err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// rebillStateCheck 行内二次判定（一次往返拿两个事实）：
// col_new = 新账单是否已存在（重放/并发），col_mark = 原行标记的最新原文（并发标记兜底）。
const rebillStateSQL = `
	SELECT (SELECT error_message FROM billing_records WHERE billing_id = $1),
	       EXISTS(SELECT 1 FROM billing_records WHERE billing_id = $2)`

// rebillOne 给一条 unpriced 账单开新账单：
//   - existed=true 表示该原行已被补收过（标记或新账单已存在）→ 本次跳过、不扣款；
//   - 正常路径：补收时刻售价 × 原行用量，Record 内部「扣款+流水+账单」同事务。
func (p *Poller) rebillOne(ctx context.Context, r rebillRow, groups []groupNode) (newID string, credits float64, existed bool, err error) {
	newID = rebillNewBillingID(r.BillingID)

	// 行内新鲜判定（扫描快照可能过期）：标记已带 / 新账单已存在 → 跳过。
	var origMark sql.NullString
	var newExists bool
	if err := p.db.QueryRowContext(ctx, rebillStateSQL, r.BillingID, newID).
		Scan(&origMark, &newExists); err != nil {
		return "", 0, false, fmt.Errorf("读取补收状态失败：%w", err)
	}
	if strings.Contains(origMark.String, rebillMarkPrefix) {
		return "", 0, true, nil
	}
	if newExists {
		// 新账单在而标记没跟上（历史中断）：补标记 + 把成本沿用补齐，本次不计新收。
		if r.CostCredits != nil && *r.CostCredits > 0 {
			if _, err := p.db.ExecContext(ctx, `UPDATE billing_records SET cost_credits = $1, cost_source = 'ledger'
				 WHERE billing_id = $2 AND COALESCE(cost_credits,0) = 0`, *r.CostCredits, newID); err != nil {
				return "", 0, false, fmt.Errorf("补齐新账单 %s 的成本失败：%w", newID, err)
			}
		}
		return "", 0, true, nil
	}

	// 售价解析：复用主链候选链（req/served/组快照/route_id 顺序完全一致）。
	lr := row{reqModel: r.ReqModel, model: r.ServedModel, routeID: r.RouteID}
	if lr.reqModel == "" {
		lr.reqModel = r.ExternalModel // 无账本行时回落账单上存的名字
	}
	price, tried, err := p.resolvePrice(ctx, lr, groups)
	if err != nil {
		return "", 0, false, err
	}
	if price == nil {
		return "", 0, false, fmt.Errorf("%w（账单 %s，已尝试：%s）——请核对该组的绑定与售价配置",
			ErrRebillRowUnresolvable, r.BillingID, strings.Join(tried, ", "))
	}
	if r.UserID <= 0 {
		return "", 0, false, fmt.Errorf("账单缺 user_id，无法归属扣款")
	}

	req := billing.RecordReq{
		BillingID:       newID,
		UserID:          r.UserID,
		PricingMode:     "sale",
		TokenID:         r.TokenID,
		ExternalModel:   price.externalName,
		InternalModelID: r.InternalModelID,
		CallTime:        time.Now(),
		Tokens:          r.Tokens,
		Rates:           billing.Rates(price.saleRates),
		Status:          billing.StatusCompleted,
		ErrorMessage:    rebillNewRemark + r.BillingID,
	}
	// 时段/分档随当前售价走（补收时刻口径）。
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

	rec, err := p.bill.Record(ctx, req)
	if err != nil {
		if isRebillDuplicate(err) {
			// 并发对手刚建了同一张单（扣款在其事务里、我们的撞库扣款已回滚）。
			return "", 0, true, nil
		}
		return "", 0, false, err
	}
	if rec == nil || rec.BillingID == "" {
		return "", 0, false, errors.New("计费服务未返回账单号")
	}

	// 成本沿用原行（不重算，避免漂移）：Record 不带 CostRates（不走公式），这里直接补写。
	if r.CostCredits != nil && *r.CostCredits > 0 {
		if _, err := p.db.ExecContext(ctx, `UPDATE billing_records SET cost_credits = $1, cost_source = 'ledger'
			 WHERE billing_id = $2 AND COALESCE(cost_credits,0) = 0`, *r.CostCredits, rec.BillingID); err != nil {
			return "", 0, false, fmt.Errorf("回填补收账单 %s 的成本失败：%w", rec.BillingID, err)
		}
	}
	return rec.BillingID, rec.CreditsConsumed, false, nil
}

// markRebilled 给原行追加标记（只追加 error_message 尾部，不改金额/状态）。
// 守卫条件与扫描过滤、行内判定共用 rebillMarkPrefix —— 「同一原行只能补收一次」的最后兜底。
func markRebilled(ctx context.Context, ex dbxExecer, origBillingID, newBillingID string) error {
	mark := rebillMarkPrefix + newBillingID
	res, err := ex.ExecContext(ctx, `
		UPDATE billing_records SET error_message = CASE
			WHEN COALESCE(error_message,'') = '' THEN $2
			ELSE error_message || $3 END
		 WHERE billing_id = $1
		   AND status = 'unpriced'
		   AND COALESCE(error_message,'') NOT LIKE '%`+rebillMarkPrefix+`%'`,
		origBillingID, mark, rebillMarkSep+mark)
	if err != nil {
		return fmt.Errorf("标记原账单 %s 失败：%w", origBillingID, err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("原账单 %s 已带补收标记或状态已变，拒绝重复补收", origBillingID)
	}
	return nil
}

// isRebillDuplicate 判定 billing_id 唯一约束冲突（23505）——「同一原行只能补收一次」的库层回声。
func isRebillDuplicate(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	return false
}
