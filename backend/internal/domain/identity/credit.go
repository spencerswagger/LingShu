// credit.go 积分钱包与流水领域实现：乐观锁扣减、充值、调整、余额查询、流水分页，
// 以及对应 HTTP 处理器。金额用 float64 + decimalx.Round5（DB NUMERIC(20,5)）。
package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/team/llmgateway/internal/pkg/dbx"
	"github.com/team/llmgateway/internal/pkg/decimalx"
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
type Flow struct {
	ID           int64
	UserID       int64
	Type         string
	Amount       float64
	RefBillingID *string
	SessionID    string // 关联会话（预扣费等无账单流水直接携带，用于会话名解析）
	Remark       *string
	CreatedAt    time.Time
	// 查询聚合字段（非表列）：该笔流水后的钱包余额（按流水累计）与关联会话名称。
	BalanceAfter float64
	SessionName  *string
}

// CreditStore 提供 credit_wallets 与 credit_flows 的数据访问。
type CreditStore struct {
	db *sql.DB
}

// NewCreditStore 创建积分存储。
func NewCreditStore(db *sql.DB) *CreditStore {
	return &CreditStore{db: db}
}

// ConsumeCredit 原子扣减：余额足够才更新并返回扣减后余额。
// 返回 exists=false 表示无匹配钱包（余额不足或钱包缺失，由服务层区分）。
func (s *CreditStore) ConsumeCredit(ctx context.Context, userID int64, amount float64) (after float64, exists bool, err error) {
	return consumeCreditOn(ctx, s.db, userID, amount)
}

// ConsumeCreditTx 与 ConsumeCredit 等价，但在调用方给定执行器（事务）上执行，
// 供计费「扣款 + 积分流水 + 账单」同事务提交复用。
func (s *CreditStore) ConsumeCreditTx(ctx context.Context, ex dbx.Execer, userID int64, amount float64) (after float64, exists bool, err error) {
	return consumeCreditOn(ctx, ex, userID, amount)
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

// ChangeBalance 增加余额（amount 可为负）。requireSufficient 为真时要求余额充足。
// 返回 changed 表示是否真的更新到行。
func (s *CreditStore) ChangeBalance(ctx context.Context, userID int64, amount float64, requireSufficient bool) (changed bool, err error) {
	return changeBalanceOn(ctx, s.db, userID, amount, requireSufficient)
}

// ChangeBalanceTx 是 ChangeBalance 的事务执行器版本。
func (s *CreditStore) ChangeBalanceTx(ctx context.Context, ex dbx.Execer, userID int64, amount float64, requireSufficient bool) (changed bool, err error) {
	return changeBalanceOn(ctx, ex, userID, amount, requireSufficient)
}

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

// SetBalance 覆盖钱包余额（直接 set，不走增量，余额可为任意非负值）。
func (s *CreditStore) SetBalance(ctx context.Context, userID int64, balance float64) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE credit_wallets SET balance = $1, version = version + 1, updated_at = now()
		 WHERE user_id = $2`, balance, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// GetBalance 查询余额；钱包不存在返回 sql.ErrNoRows。
func (s *CreditStore) GetBalance(ctx context.Context, userID int64) (float64, error) {
	return getBalanceOn(ctx, s.db, userID)
}

// GetBalanceTx 是 GetBalance 的事务执行器版本（可读到本事务内的未提交变更）。
func (s *CreditStore) GetBalanceTx(ctx context.Context, ex dbx.Execer, userID int64) (float64, error) {
	return getBalanceOn(ctx, ex, userID)
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
	var created time.Time
	var sessionID any
	if f.SessionID != "" {
		sessionID = f.SessionID
	}
	err := ex.QueryRowContext(ctx,
		`INSERT INTO credit_flows(user_id, type, amount, ref_billing_id, session_id, remark)
		 VALUES($1, $2, $3, $4, $5, $6)
		 RETURNING id, created_at`,
		f.UserID, f.Type, f.Amount, f.RefBillingID, sessionID, f.Remark).
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
func (s *CreditService) Consume(ctx context.Context, userID int64, amount float64, billingID, sessionID, remark string) (before, after float64, err error) {
	return s.consumeOn(ctx, s.store.db, userID, amount, billingID, sessionID, remark)
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
func (s *CreditService) Settle(ctx context.Context, userID int64, delta float64, sessionID, remark string) (before, after float64, err error) {
	return s.settleOn(ctx, s.store.db, userID, delta, sessionID, remark)
}

// SettleTx 在调用方给定执行器（事务）内完成差额结算与流水写入。
func (s *CreditService) SettleTx(ctx context.Context, ex dbx.Execer, userID int64, delta float64, sessionID, remark string) (before, after float64, err error) {
	return s.settleOn(ctx, ex, userID, delta, sessionID, remark)
}

// SettleBalance 在预扣基础上多退少补：delta > 0 补扣（需余额充足），delta < 0 退回。
// 返回差额流水的前后余额；补扣余额不足返回错误（计费落 failed）。
func (s *CreditService) SettleBalance(ctx context.Context, userID int64, delta float64, sessionID, remark string) (before, after float64, err error) {
	return s.settleOn(ctx, s.store.db, userID, delta, sessionID, remark)
}

func (s *CreditService) settleOn(ctx context.Context, ex dbx.Execer, userID int64, delta float64, sessionID, remark string) (before, after float64, err error) {
	delta = decimalx.Round5(delta)
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
func (s *CreditService) Recharge(ctx context.Context, userID, operatorID int64, amount float64, remark string) error {
	amount = decimalx.Round5(amount)
	if amount <= 0 {
		return errBadRequest("充值积分必须大于 0")
	}
	if err := s.store.EnsureWallet(ctx, userID); err != nil {
		return fmt.Errorf("ensure wallet: %w", err)
	}
	if _, err := s.store.ChangeBalance(ctx, userID, amount, false); err != nil {
		return fmt.Errorf("recharge: %w", err)
	}
	_ = operatorID
	return s.insertFlow(ctx, FlowTypeRecharge, userID, amount, nil, "", remark, false)
}

// AdminSetBalance 覆盖钱包余额（直接 set），写入 set 流水。
func (s *CreditService) AdminSetBalance(ctx context.Context, userID int64, balance float64, remark string) error {
	balance = decimalx.Round5(balance)
	if balance < 0 {
		return errBadRequest("目标余额不能为负")
	}
	if err := s.store.EnsureWallet(ctx, userID); err != nil {
		return fmt.Errorf("ensure wallet: %w", err)
	}
	if err := s.store.SetBalance(ctx, userID, balance); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errNotFound()
		}
		return fmt.Errorf("set balance: %w", err)
	}
	return s.insertFlow(ctx, FlowTypeSet, userID, balance, nil, "", remark, false)
}

// Adjust 调整积分：amount > 0 上调，amount < 0 下调（需余额充足），写入 adjust 流水。
func (s *CreditService) Adjust(ctx context.Context, userID, operatorID int64, amount float64, remark string) error {
	amount = decimalx.Round5(amount)
	if amount == 0 {
		return errBadRequest("调整积分数不能为 0")
	}
	if err := s.store.EnsureWallet(ctx, userID); err != nil {
		return fmt.Errorf("ensure wallet: %w", err)
	}
	needSufficient := amount < 0
	changed, err := s.store.ChangeBalance(ctx, userID, amount, needSufficient)
	if err != nil {
		return fmt.Errorf("adjust: %w", err)
	}
	if !changed {
		return errInsufficient(-amount)
	}
	_ = operatorID
	return s.insertFlow(ctx, FlowTypeAdjust, userID, amount, nil, "", remark, false)
}

// insertFlow 写入流水。billingID 非空时同步到 ref_billing_id，sessionID 同步会话。
func (s *CreditService) insertFlow(ctx context.Context, flowType string, userID int64, amount float64, billingID *string, sessionID, remark string, hasBilling bool) error {
	return s.insertFlowOn(ctx, s.store.db, flowType, userID, amount, billingID, sessionID, remark, hasBilling)
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
	svc        *CreditService
	userIDFrom func(ctx context.Context) (int64, bool)
}

// NewCreditHandler 创建积分处理器。
func NewCreditHandler(svc *CreditService, userIDFrom func(ctx context.Context) (int64, bool)) *CreditHandler {
	return &CreditHandler{svc: svc, userIDFrom: userIDFrom}
}

type flowResponse struct {
	ID           int64     `json:"id"`
	Type         string    `json:"type"`
	Amount       float64   `json:"amount"`
	Balance      float64   `json:"balance"` // 该笔流水后的钱包余额
	SessionName  *string   `json:"session_name,omitempty"`
	RefBillingID *string   `json:"ref_billing_id,omitempty"`
	Remark       *string   `json:"remark,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
}

type adjustRequest struct {
	Amount float64 `json:"amount"`
	Remark string  `json:"remark"`
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
	resp.OK(w, r, map[string]any{"balance": bal})
}

// HandleAdminRecharge POST /api/v1/admin/users/{id}/wallet/recharge 管理员充值。
func (h *CreditHandler) HandleAdminRecharge(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ID 参数无效")
		return
	}
	var req struct {
		Amount float64 `json:"amount"`
		Remark string  `json:"remark"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	operatorID, _ := h.userIDFrom(r.Context())
	if err := h.svc.Recharge(r.Context(), id, operatorID, req.Amount, req.Remark); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]any{"affected": 1})
}

// HandleAdminSet POST /api/v1/admin/users/{id}/wallet/set 覆盖余额。
func (h *CreditHandler) HandleAdminSet(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ID 参数无效")
		return
	}
	var req struct {
		Balance float64 `json:"balance"`
		Remark  string  `json:"remark"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	if err := h.svc.AdminSetBalance(r.Context(), id, req.Balance, req.Remark); err != nil {
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]any{"balance": req.Balance})
}

// HandleAdminAdjust POST /api/v1/admin/users/{id}/wallet/adjust 管理员调整积分。
func (h *CreditHandler) HandleAdminAdjust(w http.ResponseWriter, r *http.Request) {
	id, err := pathID(r)
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ID 参数无效")
		return
	}
	var req adjustRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "请求体格式错误")
		return
	}
	operatorID, _ := h.userIDFrom(r.Context())
	if err := h.svc.Adjust(r.Context(), id, operatorID, req.Amount, req.Remark); err != nil {
		log.Printf("admin adjust failed: user=%d amount=%v err=%v", id, req.Amount, err)
		writeServiceErr(w, r, err)
		return
	}
	resp.OK(w, r, map[string]any{"affected": 1})
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
	resp.OK(w, r, map[string]any{"balance": bal})
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
	resp.OK(w, r, map[string]any{"list": items, "total": total, "page": page, "size": size})
}
