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
// 与 60s 结算轮争锁。
//
// 失败隔离（毒行不堵整批）：单行失败（按当前配置解析不到售价 / 缺 user_id / 计费错误 /
// tokens 损坏等）只把该行原因写进响应的 Failed 明细，批次继续走完；已成功行保留。
// 运营照单修复后**重放补齐**——幂等三层保证重放不会重复扣款。
// 逐行「扣款+账单」同事务保证 err 非空的行一定没有半扣。

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
	// ErrRebillDisabled kill-switch 关闭（env V3_SETTLE_ENABLED 未设置走旧链路）。
	// 补收的定价逻辑 = 新链路 resolvePrice：开关关闭通常意味着对新链路定价存疑（真机
	// 出账异常时的应急动作），此时放行补收等于把同一套存疑逻辑重放进账本 → 一并拒绝（400）。
	ErrRebillDisabled = fmt.Errorf("新结算链路未启用（%s 未设置，走旧链路），补收不可用", EnvSettleEnabled)
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
	// BrokenReason 非空 = 扫描层无法解析该行（tokens 损坏等）：只进 Failed 明细，不进入补收。
	BrokenReason string
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
	// Failed 是逐行失败明细（毒行不堵整批：批次照常走完，失败行带原因进响应）。
	// omitempty = 无失败行时既有响应逐字节不变（前端六字段契约不动）。
	Failed []RebillRowFailure `json:"Failed,omitempty"`
}

// RebillRowFailure 是一条补收失败的原行（按 billing_id 定位，运营照单修复后重放补齐）。
type RebillRowFailure struct {
	BillingID string `json:"BillingID"`
	// Reason 机器可读键：**以 rebill_handler.go 常量实码为准**（前端按键分支，注释不参与
	// wire —— 键列表曾写错导致前端映射全死，Owl P1 教训）。行级实发五键及触发条件：
	//   rebill_row_unresolvable —— 行级解析不到售价（候选链落空）或 user_id 守卫兜底（见下）；
	//   rebill_no_wallet        —— 用户没有积分钱包（Record 前拦截，避免失败残单堵死重放）；
	//   rebill_broken_row       —— 扫描层毒行（tokens 损坏等，该行进不了计价）；
	//   rebill_record_error     —— 计费/落库层错误（余额不足、标记守卫拒绝等，含原因原文）；
	//   rebill_already_rebilled —— 行级撞上「已被补收」（billing_id 唯一索引 23505 回声）。
	// 批次级键 rebill_group_unpriced 不进本明细（整批未开始时走 400 + Reason）。
	Reason  string `json:"Reason"`
	Message string `json:"Message"`
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
//
// 入口归一（评审 P3，2026-11-20）：目标组用 groupRefForms **多形态**集合表达（组 ref /
// 去前缀组 id / auto\<model\> / 裸名 → 抽屉形态 / slug 档全部互认），预检与行匹配共用同一
// 集合——不再折叠成单一 ref 形态。此前单形态归一在「无组快照桥」时会把能按绑定定价的
// Ref（auto/<m>、裸名）误判成「该组没有售价口径」直接 400。
func (p *Poller) Rebill(ctx context.Context, groupName string) (*RebillResult, error) {
	if !p.newSettle {
		return nil, ErrRebillDisabled
	}
	refs := groupRefForms(groupName)
	if len(refs) == 0 {
		return nil, errors.New("组名不能为空（支持组 ref、组 id 或 auto/<model> 形态）")
	}
	// 展示用代表形态：groupRefForms 首位恒为 groupRefForm 的归一结果（响应 GroupRef
	// 语义与旧版逐字节兼容）；预检与行匹配按整个集合生效。
	ref := refs[0]
	res := &RebillResult{GroupRef: ref}

	groups, err := p.loadGroupNodes(ctx)
	if err != nil {
		// 快照缺失只影响「人可读名」档（同主链口径），ref/别名/route_id 档仍可用。
		p.log.Warn("V3 补收：读取组快照失败，按「无组快照」匹配", "err", err)
		groups = nil
	}

	// 预检：该组此刻是否有售价。解析输入 = 全部 ref 形态 + 组显示名（覆盖全部候选档，
	// 与行级解析同一实现 resolvePrice）。没有就整体拒绝，避免「扣了几行才发现组还没定价」。
	displayName := ""
	for _, g := range groups {
		if !refInSet(strings.TrimSpace(g.Ref), refs) {
			continue
		}
		displayName = strings.TrimSpace(g.DisplayName)
		break
	}
	var priced bool
	for _, rf := range refs {
		price, _, err := p.resolvePrice(ctx, row{reqModel: rf, model: displayName, routeID: 1}, groups)
		if err != nil {
			return nil, err
		}
		if price != nil {
			priced = true
			break
		}
	}
	if !priced {
		return nil, fmt.Errorf("%w：%s", ErrRebillGroupUnpriced, strings.Join(refs, " / "))
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
		if !rebillMatchesRow(r, refs, groups) {
			continue
		}
		res.Scanned++
		if r.BrokenReason != "" {
			// 毒行隔离（扫描层）：带原因进明细，批次继续（毒行不堵整批）。
			res.Failed = append(res.Failed, RebillRowFailure{
				BillingID: r.BillingID, Reason: ReasonRebillBrokenRow, Message: r.BrokenReason,
			})
			continue
		}
		if strings.Contains(r.ErrorMessage, rebillMarkPrefix) {
			// 扫描快照里已带标记：跳过（并发场景由行内二次判定兜底，见 rebillOne）。
			res.AlreadyMarked++
			continue
		}
		newID, credits, existed, failReason, failMsg, err := p.rebillOne(ctx, r, groups)
		if err != nil {
			// 毒行隔离（行级）：确定性失败（解析不到售价 / 缺 user_id / 计费错误）不堵整批，
			// 带原因进响应明细、批次继续走完；已成功行保留。库级故障同样按失败行隔离——
			// 此时后续行大概率同样失败，逐行失败明细就是完整的故障现场（不多扣一笔、不丢一条）。
			res.Failed = append(res.Failed, RebillRowFailure{
				BillingID: r.BillingID, Reason: rebillFailReason(err), Message: err.Error(),
			})
			continue
		}
		if failMsg != "" {
			res.Failed = append(res.Failed, RebillRowFailure{
				BillingID: r.BillingID, Reason: failReason, Message: failMsg,
			})
			continue
		}
		if existed {
			res.AlreadyMarked++
			continue
		}
		if err := markRebilled(ctx, p.db, r.BillingID, newID); err != nil {
			res.Failed = append(res.Failed, RebillRowFailure{
				BillingID: r.BillingID, Reason: ReasonRebillRecordError, Message: err.Error(),
			})
			continue
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
	if len(res.Failed) > 0 {
		p.log.Warn("V3 补收：存在失败行（已带原因进响应明细，批次已走完）",
			"group_ref", ref, "failed", len(res.Failed), "rebilled", res.Rebilled)
	}
	return res, nil
}

// rebillMatchesRow 判断一条 unpriced 账单是否属于目标组（目标组 = refs 多形态集合，
// 与补收入口 Rebill 的预检共用同一集合）。
// 口径与主链候选链同源：名字形态归一（ref/组 id/auto slug/抽屉形态）+ 组快照（display_name 双向）。
//
// 候选集不对称的说明（评审 P2-1 顺手项，刻意保持现状）：
//   - 匹配侧（本函数）扫 req/served/external 三名——扫描需要「这行账是不是这组」的
//     最大召回：账单的 external_model_name 可能是 unpricedName 的兜底产物（req 空 → served →
//     "unknown"），漏一个名字就是漏一笔可补收的账；
//   - 售价解析侧（rebillOne 的 row{reqModel, model}）只喂 req/served 两名——与主链
//     settleOne 的解析输入完全同源（账本行的 req_model/served_model）；external 名只作
//     req 空缺时的回落，不当独立候选。若把 external 也当独立候选，会给售价链加一个
//     主链没有的档，同一行在「结算落 unpriced」与「补收能算价」之间出现口径分叉
//     （结算按主链口径落不出去、补收却能算出价），账目两边对不上。
//   - 两名单集差异不产生误伤：external 名与 req/served 名不同时，匹配进来的行在
//     解析侧自然落 Failed 明细（Reason=rebill_row_unresolvable），不会错算价。
func rebillMatchesRow(r rebillRow, refs []string, groups []groupNode) bool {
	for _, name := range []string{r.ReqModel, r.ServedModel, r.ExternalModel} {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		for _, f := range groupRefForms(name) {
			if refInSet(f, refs) {
				return true
			}
		}
		for _, g := range groups {
			if refInSet(strings.TrimSpace(g.Ref), refs) && groupMatches(g, name) {
				return true
			}
		}
	}
	return false
}

// refInSet 判定 ref 是否命中目标集合（大小写不敏感；集合来自 groupRefForms）。
func refInSet(ref string, refs []string) bool {
	for _, rf := range refs {
		if strings.EqualFold(ref, rf) {
			return true
		}
	}
	return false
}

// scanRebillRows 读取扫描结果。单行损坏（tokens JSON 解析失败等）不再毒死整批：
// 该行带 BrokenReason 返回、其余行照常补收（响应里进 Failed 明细，运营照单修数据）。
func scanRebillRows(rows *sql.Rows) ([]rebillRow, error) {
	defer rows.Close()
	out := make([]rebillRow, 0, 8)
	for rows.Next() {
		var r rebillRow
		var tokensRaw []byte
		if err := rows.Scan(&r.BillingID, &r.UserID, &r.TokenID, &r.ExternalModel, &r.InternalModelID,
			&tokensRaw, &r.CostCredits, &r.ErrorMessage, &r.ReqModel, &r.ServedModel, &r.RouteID); err != nil {
			// 列级 Scan 失败没有可定位的行号可依赖：整体失败（这是 DB 驱动层故障，不是数据毒行）。
			return nil, fmt.Errorf("读取 unpriced 账单失败：%w", err)
		}
		if err := json.Unmarshal(tokensRaw, &r.Tokens); err != nil {
			// 毒行隔离：扫描层就能确定该行必失败（计价要读 tokens），但整批不该被它堵死。
			r.Tokens = billing.Usage{}
			r.BrokenReason = fmt.Sprintf("tokens 字段损坏（%v）", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// rebillStateCheck 行内二次判定（一次往返拿三个事实）：
// col_mark = 原行标记的最新原文（并发标记兜底）；
// col_new  = 新账单状态（NULL=不存在；completed=已补收；failed 等=上次扣款失败留的残单）；
// col_wallet = 原行用户是否有钱包（无钱包时 Record 必失败且按 billing 既有口径落 failed 残单，
// 先拦住残单就不会产生——运营开通钱包后重放即可补齐，不留死档）。
const rebillStateSQL = `
	SELECT (SELECT error_message FROM billing_records WHERE billing_id = $1),
	       (SELECT status FROM billing_records WHERE billing_id = $2),
	       EXISTS(SELECT 1 FROM credit_wallets
	               WHERE user_id = (SELECT user_id FROM billing_records WHERE billing_id = $1))`

// rebillOne 给一条 unpriced 账单开新账单：
//   - failMsg 非空 = 该行按当前配置**确定**补不了（解析不到售价 / 无钱包 / 缺 user_id）：
//     计费通道没动过（也避免 billing 失败通道落 failed 残单），调用方把键与原因放进响应明细；
//   - existed=true 表示该原行已被补收过（标记或新账单已存在）→ 本次跳过、不扣款；
//   - 正常路径：补收时刻售价 × 原行用量，Record 内部「扣款+流水+账单」同事务。
//     err 非空 = 这一次调用没有落库成功（Record 同事务保证扣款随失败回滚），不存在半扣。
func (p *Poller) rebillOne(ctx context.Context, r rebillRow, groups []groupNode) (newID string, credits float64, existed bool, rowFailReason, rowFailMsg string, err error) {
	newID = rebillNewBillingID(r.BillingID)

	// 行内新鲜判定（扫描快照可能过期）：标记已带 / 新账单已完成 / 无钱包 → 各按口径处置。
	var origMark sql.NullString
	var newStatus sql.NullString
	var hasWallet bool
	if err := p.db.QueryRowContext(ctx, rebillStateSQL, r.BillingID, newID).
		Scan(&origMark, &newStatus, &hasWallet); err != nil {
		return "", 0, false, "", "", fmt.Errorf("读取补收状态失败：%w", err)
	}
	if strings.Contains(origMark.String, rebillMarkPrefix) {
		return "", 0, true, "", "", nil
	}
	if newStatus.String == billing.StatusCompleted {
		// 新账单已完成（重放/并发）：补成本沿用（行为被既有测试钉死，不补标记——
		// 标记由首次成功路径写，这里补标记会把「账单到底扣没扣过」这一事实抹掉）。
		if r.CostCredits != nil && *r.CostCredits > 0 {
			if _, err := p.db.ExecContext(ctx, `UPDATE billing_records SET cost_credits = $1, cost_source = 'ledger'
				 WHERE billing_id = $2 AND COALESCE(cost_credits,0) = 0`, *r.CostCredits, newID); err != nil {
				return "", 0, false, "", "", fmt.Errorf("补齐新账单 %s 的成本失败：%w", newID, err)
			}
		}
		return "", 0, true, "", "", nil
	}
	if !hasWallet {
		// 用户没有钱包：Record 必失败且按 billing 既有口径落 failed 残单；先拦住，
		// 残单就不会产生（重放稳定在失败明细，运营开通钱包后即补齐）。
		return "", 0, false, ReasonRebillNoWallet,
			fmt.Sprintf("账单 %s 的用户 %d 没有积分钱包，无法扣款（开通钱包后重放补齐）", r.BillingID, r.UserID), nil
	}
	// newStatus 非空且非 completed（如 failed 残单）：Record 内部会把历史失败记录的
	// 原因透传出来，本次按行级失败隔离继续（不堵整批）。

	// 售价解析：复用主链候选链（req/served/组快照/route_id 顺序完全一致）。
	lr := row{reqModel: r.ReqModel, model: r.ServedModel, routeID: r.RouteID}
	if lr.reqModel == "" {
		lr.reqModel = r.ExternalModel // 无账本行时回落账单上存的名字
	}
	price, tried, err := p.resolvePrice(ctx, lr, groups)
	if err != nil {
		return "", 0, false, "", "", err
	}
	if price == nil {
		return "", 0, false, ReasonRebillRowUnresolvable,
			fmt.Sprintf("账单 %s 按当前配置仍解析不到售价（已尝试：%s）——请核对该组的绑定与售价配置",
				r.BillingID, strings.Join(tried, ", ")), nil
	}
	if r.UserID <= 0 {
		// 事实注明（Owl P1 注释纠偏）：这个分支**当前不可达**——billing_records.user_id
		// 是 NOT NULL + FK→users(id)，库里不存在 user_id≤0 的账单；留着是纵深防御
		// （万一未来出现无外键的导入通道），真触发时按解析失败口径进明细。行为不改。
		return "", 0, false, ReasonRebillRowUnresolvable,
			fmt.Sprintf("账单 %s 缺合法 user_id，无法归属扣款", r.BillingID), nil
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
			return "", 0, true, "", "", nil
		}
		return "", 0, false, "", "", err
	}
	if rec == nil || rec.BillingID == "" {
		return "", 0, false, "", "", errors.New("计费服务未返回账单号")
	}

	// 成本沿用原行（不重算，避免漂移）：Record 不带 CostRates（不走公式），这里直接补写。
	if r.CostCredits != nil && *r.CostCredits > 0 {
		if _, err := p.db.ExecContext(ctx, `UPDATE billing_records SET cost_credits = $1, cost_source = 'ledger'
			 WHERE billing_id = $2 AND COALESCE(cost_credits,0) = 0`, *r.CostCredits, rec.BillingID); err != nil {
			return "", 0, false, "", "", fmt.Errorf("回填补收账单 %s 的成本失败：%w", rec.BillingID, err)
		}
	}
	return rec.BillingID, rec.CreditsConsumed, false, "", "", nil
}

// rebillFailReason 把行级失败归到机器可读键（前端按 Reason 分支展示）。
func rebillFailReason(err error) string {
	switch {
	case errors.Is(err, ErrRebillGroupUnpriced):
		return ReasonRebillGroupUnpriced
	case isRebillDuplicate(err):
		return ReasonRebillAlreadyRebilled
	default:
		return ReasonRebillRecordError
	}
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
