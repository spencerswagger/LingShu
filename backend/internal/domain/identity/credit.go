// credit.go 积分钱包与流水领域实现：乐观锁扣减、充值、调整、余额查询、流水分页，
// 以及对应 HTTP 处理器。金额用 float64 + decimalx.Round5（DB NUMERIC(20,5)）。
package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/team/llmgateway/internal/pkg/dbx"
	"github.com/team/llmgateway/internal/pkg/decimalx"
	"github.com/team/llmgateway/internal/pkg/idgen"
	"github.com/team/llmgateway/internal/pkg/money"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// 流水类型常量。
const (
	FlowTypeRecharge = "recharge"
	FlowTypeConsume  = "consume"
	FlowTypeAdjust   = "adjust"
	FlowTypeSet      = "set"
)

// Wallet 对应 credit_wallets 一行。
type Wallet struct {
	UserID    int64
	Balance   float64
	Version   int64
	UpdatedAt time.Time
}

// Flow 对应 credit_flows 一行。
// 雪花 ID 超出 JS 安全整数，ID/UserID 以字符串序列化。
type Flow struct {
	ID           int64     `json:"ID,string"`
	UserID       int64     `json:"UserID,string"`
	Type         string    `json:"Type"`
	Amount       float64   `json:"Amount"`
	RefBillingID *string   `json:"RefBillingID,omitempty"`
	SessionID    string    `json:"SessionID,omitempty"` // 关联会话（预扣费等无账单流水直接携带，用于会话名解析）
	Remark       *string   `json:"Remark,omitempty"`
	CreatedAt    time.Time `json:"CreatedAt"`
	// 查询聚合字段（非表列）：该笔流水后的钱包余额（按流水累计）与关联会话名称。
	BalanceAfter float64 `json:"BalanceAfter"`
	SessionName  *string `json:"SessionName,omitempty"`
}

// CreditStore 提供 credit_wallets 与 credit_flows 的数据访问。
type CreditStore struct {
	db *sql.DB
}

// NewCreditStore 创建积分存储。
func NewCreditStore(db *sql.DB) *CreditStore {
	return &CreditStore{db: db}
}

func consumeCreditOn(ctx context.Context, ex dbx.Execer, userID int64, amount float64) (after float64, exists bool, err error) {
	var bal float64
	err = ex.QueryRowContext(ctx,
		`UPDATE credit_wallets SET balance = balance - $1, version = version + 1, updated_at = now()
		 WHERE user_id = $2 AND balance >= $1
		 RETURNING balance`,
		amount, userID).Scan(&bal)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return bal, true, nil
}

// changeBalanceOn 增加余额（amount 可为负）。requireSufficient 为真时要求余额充足。
// 返回 changed 表示是否真的更新到行。
func changeBalanceOn(ctx context.Context, ex dbx.Execer, userID int64, amount float64, requireSufficient bool) (changed bool, err error) {
	q := `UPDATE credit_wallets SET balance = balance + $1, version = version + 1, updated_at = now()
	      WHERE user_id = $2`
	args := []any{amount, userID}
	if requireSufficient {
		// 注意：余额充足条件用独立参数（正值），不要在 SQL 里对 $1 做一元负号——参数类型未定型时
		// PostgreSQL 会报 “operator is not unique: - unknown” (42725)。
		q += ` AND balance >= $3`
		args = append(args, -amount)
	}
	res, err := ex.ExecContext(ctx, q, args...)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// GetBalance 查询余额；钱包不存在返回 sql.ErrNoRows。
func (s *CreditStore) GetBalance(ctx context.Context, userID int64) (float64, error) {
	return getBalanceOn(ctx, s.db, userID)
}

func getBalanceOn(ctx context.Context, ex dbx.Execer, userID int64) (float64, error) {
	var bal float64
	err := ex.QueryRowContext(ctx,
		`SELECT balance FROM credit_wallets WHERE user_id = $1`, userID).Scan(&bal)
	if err != nil {
		return 0, err
	}
	return bal, nil
}

// EnsureWallet 为指定用户创建 credit_wallets 行（已存在则跳过），余额初始为 0。
func (s *CreditStore) EnsureWallet(ctx context.Context, userID int64) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO credit_wallets(user_id, balance) VALUES ($1, 0)
		 ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
		return fmt.Errorf("ensure wallet: %w", err)
	}
	return nil
}

// InsertFlow 写入一条积分流水。
func (s *CreditStore) InsertFlow(ctx context.Context, f *Flow) (*Flow, error) {
	return insertFlowOn(ctx, s.db, f)
}

// InsertFlowTx 是 InsertFlow 的事务执行器版本。
func (s *CreditStore) InsertFlowTx(ctx context.Context, ex dbx.Execer, f *Flow) (*Flow, error) {
	return insertFlowOn(ctx, ex, f)
}

func insertFlowOn(ctx context.Context, ex dbx.Execer, f *Flow) (*Flow, error) {
	if f.ID == 0 {
		f.ID = idgen.New()
	}
	var created time.Time
	var sessionID any
	if f.SessionID != "" {
		sessionID = f.SessionID
	}
	err := ex.QueryRowContext(ctx,
		`INSERT INTO credit_flows(id, user_id, type, amount, ref_billing_id, session_id, remark)
		 VALUES($1, $2, $3, $4, $5, $6, $7)
		 RETURNING id, created_at`,
		f.ID, f.UserID, f.Type, f.Amount, f.RefBillingID, sessionID, f.Remark).
		Scan(&f.ID, &created)
	if err != nil {
		return nil, err
	}
	f.CreatedAt = created
	return f, nil
}

// ListFlows 分页查询某用户流水，返回列表与总条数。
// balance_after 为窗口累计的流水后余额（每笔流水都会记账，累计值即钱包余额轨迹）；
// session_name 解析顺序：流水自带 cf.session_id → 消费账单的 b.session_id；
// 先取账单冗余的 session_name 快照，缺失再回查 sessions 表（会话投影过期也不影响）。
func (s *CreditStore) ListFlows(ctx context.Context, userID int64, page, size int) ([]Flow, int64, error) {
	if page < 1 {
		page = 1
	}
	if size < 1 {
		size = 20
	}
	offset := int64((page - 1) * size)

	var total int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM credit_flows WHERE user_id = $1`, userID).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count flows: %w", err)
	}

	rows, err := s.db.QueryContext(ctx,
		`WITH base AS (
			SELECT cf.*,
			       MAX(CASE WHEN cf.type = 'set' THEN cf.id END)
			           OVER (ORDER BY cf.id ROWS UNBOUNDED PRECEDING) AS base_id
			FROM credit_flows cf
			WHERE cf.user_id = $1
		),
		calc AS (
			SELECT b.*, bs.amount AS base_balance
			FROM base b
			LEFT JOIN base bs ON bs.id = b.base_id
		)
		 SELECT c.id, c.user_id, c.type, c.amount, c.ref_billing_id, c.session_id, c.remark, c.created_at,
		        CASE WHEN c.id = c.base_id THEN c.base_balance
		             ELSE COALESCE(c.base_balance, 0)
		                  + SUM(CASE WHEN c.id > COALESCE(c.base_id, 0) THEN c.amount ELSE 0 END)
		                        OVER (ORDER BY c.id)
		        END AS balance_after,
		        COALESCE(NULLIF(br.session_name, ''), se.name) AS session_name
		 FROM calc c
		 LEFT JOIN billing_records br ON br.billing_id = c.ref_billing_id
		 LEFT JOIN sessions se ON se.session_id = COALESCE(c.session_id, br.session_id)
		 ORDER BY c.id DESC LIMIT $2 OFFSET $3`,
		userID, size, offset)
	if err != nil {
		return nil, 0, fmt.Errorf("list flows: %w", err)
	}
	defer rows.Close()

	list := make([]Flow, 0, size)
	for rows.Next() {
		var f Flow
		var refBillingID, sessionID, remark, sessionName sql.NullString
		if err := rows.Scan(&f.ID, &f.UserID, &f.Type, &f.Amount, &refBillingID, &sessionID, &remark, &f.CreatedAt,
			&f.BalanceAfter, &sessionName); err != nil {
			return nil, 0, fmt.Errorf("scan flow: %w", err)
		}
		if refBillingID.Valid {
			f.RefBillingID = &refBillingID.String
		}
		if sessionID.Valid {
			f.SessionID = sessionID.String
		}
		if remark.Valid {
			f.Remark = &remark.String
		}
		if sessionName.Valid {
			f.SessionName = &sessionName.String
		}
		list = append(list, f)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// CreditService 承载积分业务逻辑。Consume 供计费（Task 6）调用。
type CreditService struct {
	store *CreditStore
}

// NewCreditService 创建积分服务。
func NewCreditService(store *CreditStore) *CreditService {
	return &CreditService{store: store}
}

func errWalletNotFound() *APIError {
	return &APIError{HTTPStatus: http.StatusNotFound, Code: resp.CodeNotFound, Message: "用户积分钱包不存在"}
}

func errInsufficient(need float64) *APIError {
	return &APIError{
		HTTPStatus: http.StatusPaymentRequired,
		Code:       resp.CodeInsufficient,
		Message:    fmt.Sprintf("积分余额不足，本次消耗需 %s 积分", formatFloat(decimalx.Round2(need))),
	}
}

// formatFloat 移除 float64 精度噪声（如 1.5000000000000002 → 1.5）。
func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}

// Consume 原子扣减积分并写入 consume 流水。返回扣减前后的余额。
// 余额不足返回 40201，钱包不存在返回 40401。sessionID 非空时流水直接关联会话。
// 未传外部事务时内部开启事务，保证「改余额 + 写流水」原子提交。
func (s *CreditService) Consume(ctx context.Context, userID int64, amount float64, billingID, sessionID, remark string) (before, after float64, err error) {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin consume tx: %w", err)
	}
	defer tx.Rollback()
	before, after, err = s.consumeOn(ctx, tx, userID, amount, billingID, sessionID, remark)
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit consume tx: %w", err)
	}
	return before, after, nil
}

// ConsumeTx 在调用方给定执行器（事务）内完成扣减与流水写入；
// 供计费把「扣款 + 积分流水 + 账单」放进同一个 PostgreSQL 事务提交。
func (s *CreditService) ConsumeTx(ctx context.Context, ex dbx.Execer, userID int64, amount float64, billingID, sessionID, remark string) (before, after float64, err error) {
	return s.consumeOn(ctx, ex, userID, amount, billingID, sessionID, remark)
}

func (s *CreditService) consumeOn(ctx context.Context, ex dbx.Execer, userID int64, amount float64, billingID, sessionID, remark string) (before, after float64, err error) {
	amount = decimalx.Round5(amount)
	if amount <= 0 {
		return 0, 0, errBadRequest("扣减积分必须大于 0")
	}
	if err := money.Validate(amount, "扣减积分"); err != nil {
		return 0, 0, errBadRequest(err.Error())
	}
	after, changed, err := consumeCreditOn(ctx, ex, userID, amount)
	if err != nil {
		return 0, 0, fmt.Errorf("consume credit: %w", err)
	}
	if !changed {
		// 先区分是钱包缺失还是余额不足。
		if _, gErr := getBalanceOn(ctx, ex, userID); errors.Is(gErr, sql.ErrNoRows) {
			return 0, 0, errWalletNotFound()
		}
		return 0, 0, errInsufficient(amount)
	}
	before = decimalx.Round5(after) + amount
	if err := s.insertFlowOn(ctx, ex, FlowTypeConsume, userID, -amount, &billingID, sessionID, remark, true); err != nil {
		return 0, 0, err
	}
	return before, after, nil
}

// Settle 为 SettleBalance 的别名，满足 billing.CreditService 接口。
// 未传外部事务时内部开启事务，保证「改余额 + 写流水」原子提交。
func (s *CreditService) Settle(ctx context.Context, userID int64, delta float64, sessionID, remark string) (before, after float64, err error) {
	return s.settleWithTx(ctx, userID, delta, sessionID, remark)
}

// SettleTx 在调用方给定执行器（事务）内完成差额结算与流水写入。
func (s *CreditService) SettleTx(ctx context.Context, ex dbx.Execer, userID int64, delta float64, sessionID, remark string) (before, after float64, err error) {
	return s.settleOn(ctx, ex, userID, delta, sessionID, remark)
}

// SettleBalance 在预扣基础上多退少补：delta > 0 补扣（需余额充足），delta < 0 退回。
// 返回差额流水的前后余额；补扣余额不足返回错误（计费落 failed）。
// 未传外部事务时内部开启事务，保证「改余额 + 写流水」原子提交。
func (s *CreditService) SettleBalance(ctx context.Context, userID int64, delta float64, sessionID, remark string) (before, after float64, err error) {
	return s.settleWithTx(ctx, userID, delta, sessionID, remark)
}

// settleWithTx 内部开启事务执行差额结算（非 Tx 入口共用）。
func (s *CreditService) settleWithTx(ctx context.Context, userID int64, delta float64, sessionID, remark string) (before, after float64, err error) {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("begin settle tx: %w", err)
	}
	defer tx.Rollback()
	before, after, err = s.settleOn(ctx, tx, userID, delta, sessionID, remark)
	if err != nil {
		return 0, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("commit settle tx: %w", err)
	}
	return before, after, nil
}

func (s *CreditService) settleOn(ctx context.Context, ex dbx.Execer, userID int64, delta float64, sessionID, remark string) (before, after float64, err error) {
	delta = decimalx.Round5(delta)
	if err := money.Validate(delta, "结算差额"); err != nil {
		return 0, 0, errBadRequest(err.Error())
	}
	before, gErr := getBalanceOn(ctx, ex, userID)
	if gErr != nil {
		return 0, 0, gErr
	}
	if delta == 0 {
		return before, before, nil
	}
	if delta < 0 {
		if _, e := changeBalanceOn(ctx, ex, userID, -delta, false); e != nil {
			return 0, 0, fmt.Errorf("settle refund: %w", e)
		}
	} else {
		changed, e := changeBalanceOn(ctx, ex, userID, -delta, true)
		if e != nil {
			return 0, 0, fmt.Errorf("settle charge: %w", e)
		}
		if !changed {
			return before, before, errInsufficient(delta)
		}
	}
	after, gErr = getBalanceOn(ctx, ex, userID)
	if gErr != nil {
		return 0, 0, gErr
	}
	if err := s.insertFlowOn(ctx, ex, FlowTypeAdjust, userID, -delta, nil, sessionID, remark, false); err != nil {
		return 0, 0, err
	}
	return before, after, nil
}

// Recharge 充值积分（amount > 0），写入 recharge 流水，operatorID 为操作管理员。
// 改余额 + 写流水在同一事务内原子提交；operatorID 写入流水备注（操作员#<id>）。
func (s *CreditService) Recharge(ctx context.Context, userID, operatorID int64, amount float64, remark string) error {
	amount = decimalx.Round5(amount)
	if amount <= 0 {
		return errBadRequest("充值积分必须大于 0")
	}
	if err := money.Validate(amount, "充值积分"); err != nil {
		return errBadRequest(err.Error())
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin recharge tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO credit_wallets(user_id, balance) VALUES ($1, 0) ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
		return fmt.Errorf("ensure wallet: %w", err)
	}
	if _, err := changeBalanceOn(ctx, tx, userID, amount, false); err != nil {
		return fmt.Errorf("recharge: %w", err)
	}
	if err := s.insertFlowOn(ctx, tx, FlowTypeRecharge, userID, amount, nil, "", operatorRemark(remark, operatorID), false); err != nil {
		return err
	}
	return tx.Commit()
}

// AdminSetBalance 覆盖钱包余额（直接 set），写入 set 流水。
// 改余额 + 写流水在同一事务内原子提交。
func (s *CreditService) AdminSetBalance(ctx context.Context, userID int64, balance float64, remark string) error {
	balance = decimalx.Round5(balance)
	if balance < 0 {
		return errBadRequest("目标余额不能为负")
	}
	if err := money.Validate(balance, "目标余额"); err != nil {
		return errBadRequest(err.Error())
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin set balance tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO credit_wallets(user_id, balance) VALUES ($1, 0) ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
		return fmt.Errorf("ensure wallet: %w", err)
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE credit_wallets SET balance = $1, version = version + 1, updated_at = now()
		 WHERE user_id = $2`, balance, userID)
	if err != nil {
		return fmt.Errorf("set balance: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return errNotFound()
	}
	if err := s.insertFlowOn(ctx, tx, FlowTypeSet, userID, balance, nil, "", remark, false); err != nil {
		return err
	}
	return tx.Commit()
}

// Adjust 调整积分：amount > 0 上调，amount < 0 下调（需余额充足），写入 adjust 流水。
// operatorID 为操作管理员；改余额 + 写流水在同一事务内原子提交；operatorID 写入流水备注。
func (s *CreditService) Adjust(ctx context.Context, userID, operatorID int64, amount float64, remark string) error {
	amount = decimalx.Round5(amount)
	if amount == 0 {
		return errBadRequest("调整积分数不能为 0")
	}
	if err := money.Validate(amount, "调整积分数"); err != nil {
		return errBadRequest(err.Error())
	}
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin adjust tx: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO credit_wallets(user_id, balance) VALUES ($1, 0) ON CONFLICT (user_id) DO NOTHING`, userID); err != nil {
		return fmt.Errorf("ensure wallet: %w", err)
	}
	needSufficient := amount < 0
	changed, err := changeBalanceOn(ctx, tx, userID, amount, needSufficient)
	if err != nil {
		return fmt.Errorf("adjust: %w", err)
	}
	if !changed {
		return errInsufficient(-amount)
	}
	if err := s.insertFlowOn(ctx, tx, FlowTypeAdjust, userID, amount, nil, "", operatorRemark(remark, operatorID), false); err != nil {
		return err
	}
	return tx.Commit()
}

// operatorRemark 将操作管理员 ID 写入流水备注前缀（operatorID=0 表示系统/无操作者，不加前缀）。
func operatorRemark(remark string, operatorID int64) string {
	if operatorID == 0 {
		return remark
	}
	prefix := fmt.Sprintf("操作员#%d", operatorID)
	if remark == "" {
		return prefix
	}
	return prefix + " " + remark
}

// insertFlowOn 在给定执行器上写入流水（事务路径复用）。
func (s *CreditService) insertFlowOn(ctx context.Context, ex dbx.Execer, flowType string, userID int64, amount float64, billingID *string, sessionID, remark string, hasBilling bool) error {
	var refBillingID *string
	if hasBilling && billingID != nil && *billingID != "" {
		refBillingID = billingID
	}
	var remarkPtr *string
	if remark != "" {
		remarkPtr = &remark
	}
	if _, err := insertFlowOn(ctx, ex, &Flow{
		UserID:       userID,
		Type:         flowType,
		Amount:       amount,
		RefBillingID: refBillingID,
		SessionID:    sessionID,
		Remark:       remarkPtr,
	}); err != nil {
		return fmt.Errorf("insert %s flow: %w", flowType, err)
	}
	return nil
}

// GetWallet 查询用户钱包余额；钱包不存在时兜底创建 0 余额钱包并返回 0。
func (s *CreditService) GetWallet(ctx context.Context, userID int64) (float64, error) {
	bal, err := s.store.GetBalance(ctx, userID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if cerr := s.store.EnsureWallet(ctx, userID); cerr != nil {
				return 0, fmt.Errorf("ensure wallet: %w", cerr)
			}
			return 0, nil
		}
		return 0, fmt.Errorf("get wallet: %w", err)
	}
	return bal, nil
}

// ListFlows 分页查询用户流水。
func (s *CreditService) ListFlows(ctx context.Context, userID int64, page, size int) ([]Flow, int64, error) {
	return s.store.ListFlows(ctx, userID, page, size)
}

// EnsureWallet 确保用户存在钱包（不存在则建 balance=0）；建用户时已兜底，此处容错再查。
func (s *CreditService) EnsureWallet(ctx context.Context, userID int64) error {
	return s.store.EnsureWallet(ctx, userID)
}

// CreditHandler 暴露积分 HTTP 处理器。userIDFrom 从 context 解析当前登录用户。
type CreditHandler struct {
	svc            *CreditService
	userIDFrom     func(ctx context.Context) (int64, bool)
	verifyPassword func(ctx context.Context, userID int64, password string) error
}

// NewCreditHandler 创建积分处理器。
func NewCreditHandler(svc *CreditService, userIDFrom func(ctx context.Context) (int64, bool)) *CreditHandler {
	return &CreditHandler{svc: svc, userIDFrom: userIDFrom}
}

// SetPasswordVerifier 注入当前口令校验（充值/调整等资金敏感操作二次验证；nil=跳过校验）。
func (h *CreditHandler) SetPasswordVerifier(fn func(ctx context.Context, userID int64, password string) error) {
	h.verifyPassword = fn
}

type flowResponse struct {
	ID           int64     `json:"ID,string"`
	Type         string    `json:"Type"`
	Amount       float64   `json:"Amount"`
	Balance      float64   `json:"Balance"` // 该笔流水后的钱包余额
	SessionName  *string   `json:"SessionName,omitempty"`
	RefBillingID *string   `json:"RefBillingID,omitempty"`
	Remark       *string   `json:"Remark,omitempty"`
	CreatedAt    time.Time `json:"CreatedAt"`
}

// HandleAdminWallet GET /api/v1/admin/users/{id}/wallet 查询用户钱包余额。
func (h *CreditHandler) HandleAdminWallet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ID 参数无效")
		return
	}
	bal, err := h.svc.GetWallet(r.Context(), id)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]any{"Balance": bal})
}

// HandleAdminRecharge POST /api/v1/admin/users/{id}/wallet/recharge 管理员充值。
// 资金敏感操作：需当前口令二次验证。
func (h *CreditHandler) HandleAdminRecharge(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ID 参数无效")
		return
	}
	var req struct {
		Amount   float64 `json:"Amount"`
		Remark   string  `json:"Remark"`
		Password string  `json:"Password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	operatorID, _ := h.userIDFrom(r.Context())
	if h.verifyPassword != nil && operatorID != 0 {
		if verr := h.verifyPassword(r.Context(), operatorID, req.Password); verr != nil {
			writeServiceErr(w, r, verr)
			return
		}
	}
	if err := h.svc.Recharge(r.Context(), id, operatorID, req.Amount, req.Remark); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]any{"Affected": 1})
}

// HandleAdminSet POST /api/v1/admin/users/{id}/wallet/set 覆盖余额。
// 资金敏感操作：需当前口令二次验证。
func (h *CreditHandler) HandleAdminSet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ID 参数无效")
		return
	}
	var req struct {
		Balance  float64 `json:"Balance"`
		Remark   string  `json:"Remark"`
		Password string  `json:"Password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	operatorID, _ := h.userIDFrom(r.Context())
	if h.verifyPassword != nil && operatorID != 0 {
		if verr := h.verifyPassword(r.Context(), operatorID, req.Password); verr != nil {
			writeServiceErr(w, r, verr)
			return
		}
	}
	if err := h.svc.AdminSetBalance(r.Context(), id, req.Balance, req.Remark); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]any{"Balance": req.Balance})
}

// HandleAdminAdjust POST /api/v1/admin/users/{id}/wallet/adjust 管理员调整积分。
// 资金敏感操作：需当前口令二次验证。
func (h *CreditHandler) HandleAdminAdjust(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ID 参数无效")
		return
	}
	var req struct {
		Amount   float64 `json:"Amount"`
		Remark   string  `json:"Remark"`
		Password string  `json:"Password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	operatorID, _ := h.userIDFrom(r.Context())
	if h.verifyPassword != nil && operatorID != 0 {
		if verr := h.verifyPassword(r.Context(), operatorID, req.Password); verr != nil {
			writeServiceErr(w, r, verr)
			return
		}
	}
	if err := h.svc.Adjust(r.Context(), id, operatorID, req.Amount, req.Remark); err != nil {
		slog.Error("admin adjust failed", "user", id, "amount", req.Amount, "err", err)
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]any{"Affected": 1})
}

// HandleAdminFlows GET /api/v1/admin/users/{id}/wallet/flows 查询用户流水。
func (h *CreditHandler) HandleAdminFlows(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ID 参数无效")
		return
	}
	page, size := parsePageSize(r.URL.Query())
	h.respondFlows(w, r, id, page, size)
}

// HandleDevWallet GET /api/v1/dev/wallet 查询本人钱包余额。
func (h *CreditHandler) HandleDevWallet(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return
	}
	bal, err := h.svc.GetWallet(r.Context(), userID)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]any{"Balance": bal})
}

// HandleDevFlows GET /api/v1/dev/wallet/flows 查询本人流水。
func (h *CreditHandler) HandleDevFlows(w http.ResponseWriter, r *http.Request) {
	userID, ok := h.userIDFrom(r.Context())
	if !ok {
		resp.Err(w, r, http.StatusUnauthorized, resp.CodeUnauthorized, "未登录或登录已过期")
		return
	}
	page, size := parsePageSize(r.URL.Query())
	h.respondFlows(w, r, userID, page, size)
}

// respondFlows 输出流水分页结果。
func (h *CreditHandler) respondFlows(w http.ResponseWriter, r *http.Request, userID int64, page, size int) {
	list, total, err := h.svc.ListFlows(r.Context(), userID, page, size)
	if err != nil {
		writeServiceErr(w, r, err)
		return
	}
	items := make([]flowResponse, 0, len(list))
	for _, f := range list {
		items = append(items, flowResponse{
			ID:           f.ID,
			Type:         f.Type,
			Amount:       f.Amount,
			Balance:      f.BalanceAfter,
			SessionName:  f.SessionName,
			RefBillingID: f.RefBillingID,
			Remark:       f.Remark,
			CreatedAt:    f.CreatedAt,
		})
	}
	resp.OK(w, r, map[string]any{"List": items, "Total": total, "Page": page, "Size": size})
}
