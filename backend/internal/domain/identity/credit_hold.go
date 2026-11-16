// credit_hold.go 是「预扣登记」的落库实现（阶段二 A1）。
//
// 背景：阶段一的预扣是「直接从钱包扣掉预估 → 失败再退回」。扣与退之间隔着一次上游调用，
// 进程崩溃 / 部署重启 / 上下文丢失都会让「扣了没退」永久留在账上（钱在用户头上算消费，
// 而请求其实没成功）。credit_holds 表（0022）就是为这件事准备的登记表：
//
//	创建：与「扣余额 + 写 consume 流水」**同一个事务**（要么三样都在，要么都不在）
//	结算：请求成功 → settled（带上匹配到的账本条目）
//	失败：请求失败 → refunded
//	超时：15 分钟仍未终结 → expired 并**全额退回**（过期扫描器）
//
// 三条不变量（改这个文件前先读）：
//  1. hold 只在 'held' 状态可被终结（终结用条件 UPDATE，天然幂等）；
//  2. 创建与扣款必须同事务。分两次写会出现「有 hold 没扣钱」→ 过期扫描退出一笔从未扣过的钱（凭空发钱）；
//  3. 过期退回的流水类型是 adjust（不是 recharge）；remark 逐字固定，报表按 remark 归集。
package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/team/llmgateway/internal/pkg/decimalx"
	"github.com/team/llmgateway/internal/pkg/idgen"
	"github.com/team/llmgateway/internal/pkg/money"
)

// 预扣登记状态（与 0022 的 CHECK 同源，别自己加值）。
const (
	HoldHeld     = "held"
	HoldSettled  = "settled"
	HoldRefunded = "refunded"
	HoldExpired  = "expired"
)

// DefaultHoldTTL 是预扣的默认有效期（与 0022 文件头一致：15 分钟）。
//
// 为什么是 15 分钟：长尾请求（大上下文 + 慢上游 + 流式）实测很少超过 10 分钟，
// 再长就没法区分「还在跑」与「进程已死」；过期退回是可重试的（用户重发一次即可），
// 而「永远冻着」不可恢复。
const DefaultHoldTTL = 15 * time.Minute

// RetryRemark 是预扣超时退回的流水备注（报表按它归集，逐字固定，别改文案）。
const RetryRemarkExpired = "预扣超时未结算退回"

// Hold 是 credit_holds 的一行。
type Hold struct {
	BillingID     string
	UserID        int64
	TokenID       *int64
	Amount        float64
	Status        string
	LedgerEntryID string
	ExpiresAt     time.Time
	ClosedAt      *time.Time
	CreatedAt     time.Time
}

// HoldRequest 是创建预扣登记的入参。
type HoldRequest struct {
	// BillingID 是**请求级唯一键**（网关传 request_id）。一请求一预扣，重试必须换新 id。
	BillingID string
	UserID    int64
	TokenID   *int64
	Amount    float64
	SessionID string
	Remark    string
	// TTL 为 0 时用 DefaultHoldTTL。
	TTL time.Duration
}

// ErrHoldNotFound 表示没有对应的预扣登记（例如尚未启用登记就调了终结接口）。
var ErrHoldNotFound = errors.New("预扣登记不存在")

// PreConsumeHold 创建预扣登记并原子扣款。
//
// 事务内顺序：写 hold（held）→ 条件扣余额 → 写 consume 流水。任一失败整体回滚：
// 余额不足返回 40201（钱包缺失 40401），此时**不留 hold**，过期扫描器不会去退一笔没扣过的钱。
func (s *CreditService) PreConsumeHold(ctx context.Context, req HoldRequest) (Hold, error) {
	if s == nil || s.store == nil || s.store.db == nil {
		return Hold{}, fmt.Errorf("积分服务未装配数据库连接")
	}
	if req.BillingID == "" {
		return Hold{}, fmt.Errorf("预扣登记缺少请求唯一键（billing_id）")
	}
	amount := decimalx.Round5(req.Amount)
	if amount <= 0 {
		// 零/负金额不建登记：没有钱要冻，登记只会制造噪音（零金额预扣仍走 Consume 的零余额守卫）。
		return Hold{}, fmt.Errorf("预扣金额必须大于 0（收到 %v）", req.Amount)
	}
	if err := money.Validate(amount, "预扣积分"); err != nil {
		return Hold{}, errBadRequest(err.Error())
	}
	ttl := req.TTL
	if ttl <= 0 {
		ttl = DefaultHoldTTL
	}
	expiresAt := time.Now().Add(ttl)

	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return Hold{}, fmt.Errorf("begin hold tx: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO credit_holds(id, billing_id, user_id, token_id, amount, status, expires_at)
		 VALUES ($1, $2, $3, $4, $5, 'held', $6)`,
		idgen.New(), req.BillingID, req.UserID, req.TokenID, amount, expiresAt); err != nil {
		return Hold{}, fmt.Errorf("登记预扣失败：%w", err)
	}
	if _, _, err := s.consumeOn(ctx, tx, req.UserID, amount, req.BillingID, req.SessionID, req.Remark); err != nil {
		// 余额不足/钱包缺失：整体回滚，hold 不落地。
		return Hold{}, err
	}
	if err := tx.Commit(); err != nil {
		return Hold{}, fmt.Errorf("commit hold tx: %w", err)
	}
	return Hold{
		BillingID: req.BillingID, UserID: req.UserID, TokenID: req.TokenID,
		Amount: amount, Status: HoldHeld, ExpiresAt: expiresAt, CreatedAt: time.Now(),
	}, nil
}

// CloseHold 终结一条预扣登记（settled / refunded）。返回 false 表示它已被终结过（幂等）。
//
// 只允许 held → settled/refunded：并发下「结算」与「过期扫描」抢同一行时，
// 条件 UPDATE 保证只有一方成功，另一方拿到 false（不会既退钱又结算）。
func (s *CreditService) CloseHold(ctx context.Context, billingID, status, ledgerEntryID string) (bool, error) {
	if s == nil || s.store == nil || s.store.db == nil {
		return false, fmt.Errorf("积分服务未装配数据库连接")
	}
	if status != HoldSettled && status != HoldRefunded {
		return false, fmt.Errorf("预扣终结状态只能是 settled/refunded（收到 %q）", status)
	}
	if billingID == "" {
		return false, fmt.Errorf("预扣终结缺少 billing_id")
	}
	res, err := s.store.db.ExecContext(ctx,
		`UPDATE credit_holds
		    SET status = $2,
		        ledger_entry_id = COALESCE(NULLIF($3, ''), ledger_entry_id),
		        closed_at = now()
		  WHERE billing_id = $1 AND status = 'held'`,
		billingID, status, ledgerEntryID)
	if err != nil {
		return false, fmt.Errorf("终结预扣登记失败：%w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("读取预扣终结行数失败：%w", err)
	}
	return n > 0, nil
}

// ExpireResult 是一轮过期扫描的结果。
type ExpireResult struct {
	// Expired 是本次退回的预扣笔数，Amount 是退回的积分合计。
	Expired int
	Amount  float64
}

// ExpireHolds 扫描超期未终结的预扣，逐条「终结为 expired + 全额退回 + 写 adjust 流水」。
//
// 并发安全：候选行用 `FOR UPDATE SKIP LOCKED` 取，多个副本同时扫不会重复退；
// 终结与退款在**同一事务**里，退钱成功而登记没终结（或反过来）都会造成重复退款。
//
// 幂等：再次运行扫不到 held 行即为空操作。
func (s *CreditService) ExpireHolds(ctx context.Context, now time.Time, limit int) (ExpireResult, error) {
	if s == nil || s.store == nil || s.store.db == nil {
		return ExpireResult{}, fmt.Errorf("积分服务未装配数据库连接")
	}
	if now.IsZero() {
		now = time.Now()
	}
	if limit <= 0 {
		limit = 200
	}
	var out ExpireResult

	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return out, fmt.Errorf("begin expire tx: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx,
		`SELECT billing_id, user_id, amount
		   FROM credit_holds
		  WHERE status = 'held' AND expires_at <= $1
		  ORDER BY expires_at
		  LIMIT $2
		  FOR UPDATE SKIP LOCKED`, now, limit)
	if err != nil {
		return out, fmt.Errorf("读取过期预扣失败：%w", err)
	}
	type candidate struct {
		billingID sql.NullString
		userID    int64
		amount    float64
	}
	var cands []candidate
	for rows.Next() {
		var c candidate
		// session_id 不在 credit_holds 上（表结构定稿十列），这里只取登记自身字段。
		if err := rows.Scan(&c.billingID, &c.userID, &c.amount); err != nil {
			rows.Close()
			return out, fmt.Errorf("扫描过期预扣失败：%w", err)
		}
		cands = append(cands, c)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return out, fmt.Errorf("遍历过期预扣失败：%w", err)
	}
	rows.Close()

	for _, c := range cands {
		res, err := tx.ExecContext(ctx,
			`UPDATE credit_holds SET status = 'expired', closed_at = now()
			  WHERE billing_id = $1 AND status = 'held'`, c.billingID.String)
		if err != nil {
			return out, fmt.Errorf("终结过期预扣 %s 失败：%w", c.billingID.String, err)
		}
		n, _ := res.RowsAffected()
		if n == 0 {
			// 已被别的路径终结（结算/退款），不重复退钱。
			continue
		}
		amount := decimalx.Round5(c.amount)
		if amount <= 0 {
			continue
		}
		if _, err := changeBalanceOn(ctx, tx, c.userID, amount, false); err != nil {
			return out, fmt.Errorf("退回预扣余额失败（user=%d）：%w", c.userID, err)
		}
		if err := s.insertFlowOn(ctx, tx, FlowTypeAdjust, c.userID, amount, nil, "", RetryRemarkExpired, false); err != nil {
			return out, err
		}
		out.Expired++
		out.Amount = decimalx.Round5(out.Amount + amount)
	}
	if err := tx.Commit(); err != nil {
		return ExpireResult{}, fmt.Errorf("commit expire tx: %w", err)
	}
	return out, nil
}

// GetHold 读一条预扣登记（排障与用例用；生产热路径不查它）。
func (s *CreditService) GetHold(ctx context.Context, billingID string) (Hold, error) {
	if s == nil || s.store == nil || s.store.db == nil {
		return Hold{}, fmt.Errorf("积分服务未装配数据库连接")
	}
	var (
		h        Hold
		tokenID  sql.NullInt64
		ledger   sql.NullString
		closed   sql.NullTime
		amountDB float64
	)
	err := s.store.db.QueryRowContext(ctx,
		`SELECT billing_id, user_id, token_id, amount, status,
		        COALESCE(ledger_entry_id, ''), expires_at, closed_at, created_at
		   FROM credit_holds WHERE billing_id = $1`, billingID).
		Scan(&h.BillingID, &h.UserID, &tokenID, &amountDB, &h.Status,
			&ledger, &h.ExpiresAt, &closed, &h.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Hold{}, ErrHoldNotFound
	}
	if err != nil {
		return Hold{}, fmt.Errorf("读取预扣登记失败：%w", err)
	}
	h.Amount = amountDB
	if tokenID.Valid {
		v := tokenID.Int64
		h.TokenID = &v
	}
	h.LedgerEntryID = ledger.String
	if closed.Valid {
		t := closed.Time
		h.ClosedAt = &t
	}
	return h, nil
}
