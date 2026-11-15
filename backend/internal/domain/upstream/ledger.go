package upstream

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/pkg/idgen"
	"github.com/team/llmgateway/internal/pkg/money"
)

// 账本（ledger_entries）九字段，列名/类型以架构侧定稿为准（见
// docs/designs/2026-11-15-v2-ledger-attribution.md 的字段表）：
//
//	id BIGINT PK · ledger_id TEXT UNIQUE（幂等键）· provider TEXT '' ·
//	provider_account TEXT '' · model TEXT '' · tokens_in BIGINT · tokens_out BIGINT ·
//	cost_credits NUMERIC(20,5) · occurred_at TIMESTAMPTZ
//
// 本期就这九列，**一列不加**（request_id/status/缓存读/思考 token/单价/币种属 POC 后的扩展）。
// 迁移文件由架构侧 0019–0023 统一落地；本包只按上述列名读写，不另写一套 DDL。
const (
	// LedgerTable 是账本表名（实施计划 §3 的 0022 口径）。
	LedgerTable = "ledger_entries"
	// LedgerFieldCount 是账本字段数（九）。
	LedgerFieldCount = 9
	// ledgerBatchRows 是单条 INSERT 的最大行数（9 列 × 200 行 = 1800 个参数，远低于上限）。
	ledgerBatchRows = 200
	// ledgerListDefaultLimit / ledgerListMaxLimit 是 offset 分页的默认与上限。
	ledgerListDefaultLimit = 200
	ledgerListMaxLimit     = 1000
)

// LedgerEntry 是一条账本记录（九字段）。
type LedgerEntry struct {
	// ID 是主键（雪花 ID）；写入方留 0 时由存储层生成。
	ID int64
	// LedgerID 是幂等键（上游账本条目 ID 或网关侧派生键），重复写入必须被 ON CONFLICT 吃掉。
	LedgerID string
	// Provider / ProviderAccount 是上游供应商与账号维度；缺失为空串（列默认 ''）。
	Provider        string
	ProviderAccount string
	// Model 是上游模型名；缺失为空串。
	Model string
	// TokensIn / TokensOut 是上游口径的输入/输出 token 数。
	TokensIn  int64
	TokensOut int64
	// CostCredits 是上游口径的成本积分（NUMERIC(20,5)）。
	CostCredits float64
	// OccurredAt 是上游发生时刻；**必须显式给**，不拿 now() 兜底（猜时间会把账算错位）。
	OccurredAt time.Time
}

// Validate 校验账本记录的可入库性（形状 + 幂等键 + 金额范围）。
func (e LedgerEntry) Validate() error {
	if strings.TrimSpace(e.LedgerID) == "" {
		return fmt.Errorf("账本记录缺少幂等键 ledger_id")
	}
	if e.OccurredAt.IsZero() {
		return fmt.Errorf("账本记录 %s 缺少 occurred_at（不许用 now() 兜底）", e.LedgerID)
	}
	if e.TokensIn < 0 || e.TokensOut < 0 {
		return fmt.Errorf("账本记录 %s 的 token 数为负（in=%d out=%d）", e.LedgerID, e.TokensIn, e.TokensOut)
	}
	if err := money.Validate(e.CostCredits, fmt.Sprintf("账本记录 %s 的 cost_credits", e.LedgerID)); err != nil {
		return err
	}
	return nil
}

// LedgerTx 是一轮同步的事务边界：跨页写入同一事务，失败/超限整轮回滚（不留半条账）。
type LedgerTx interface {
	// Upsert 幂等写入一批账本（ON CONFLICT (ledger_id) DO NOTHING），返回实际插入条数。
	Upsert(ctx context.Context, entries []LedgerEntry) (int, error)
	Commit() error
	Rollback() error
}

// LedgerTxBeginner 开一轮同步事务（*LedgerStore 满足）。
type LedgerTxBeginner interface {
	Begin(ctx context.Context) (LedgerTx, error)
}

// LedgerWriter 是账本读写通道。
type LedgerWriter interface {
	LedgerTxBeginner
	// List 按 offset + limit 读取账本（offset 从 0 开始，对账与排障用）。
	List(ctx context.Context, offset, limit int) ([]LedgerEntry, error)
}

// LedgerStore 是 ledger_entries 的 SQL 实现。
type LedgerStore struct {
	db *sql.DB
}

// NewLedgerStore 创建账本存储（db 为 nil 时各方法返回错误，不 panic）。
func NewLedgerStore(db *sql.DB) *LedgerStore { return &LedgerStore{db: db} }

var _ LedgerWriter = (*LedgerStore)(nil)

// Begin 开启一轮同步事务。
func (s *LedgerStore) Begin(ctx context.Context) (LedgerTx, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("账本存储未装配数据库连接")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("开启账本事务失败：%w", err)
	}
	return &ledgerTx{tx: tx}, nil
}

// List 按 offset 分页读取账本（occurred_at DESC, id DESC，offset 从 0 开始）。
func (s *LedgerStore) List(ctx context.Context, offset, limit int) ([]LedgerEntry, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("账本存储未装配数据库连接")
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 {
		limit = ledgerListDefaultLimit
	}
	if limit > ledgerListMaxLimit {
		limit = ledgerListMaxLimit
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, ledger_id, provider, provider_account, model, tokens_in, tokens_out, cost_credits, occurred_at
		   FROM `+LedgerTable+`
		  ORDER BY occurred_at DESC, id DESC
		  LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("读取账本失败：%w", err)
	}
	defer rows.Close()

	out := make([]LedgerEntry, 0, limit)
	for rows.Next() {
		var e LedgerEntry
		if err := rows.Scan(&e.ID, &e.LedgerID, &e.Provider, &e.ProviderAccount, &e.Model,
			&e.TokensIn, &e.TokensOut, &e.CostCredits, &e.OccurredAt); err != nil {
			return nil, fmt.Errorf("读取账本行失败：%w", err)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("读取账本失败：%w", err)
	}
	return out, nil
}

// ledgerTx 是 *sql.Tx 上的账本事务实现。
type ledgerTx struct {
	tx *sql.Tx
}

// Upsert 分批幂等写入：同一 ledger_id 重复出现只会插入第一条（DO NOTHING）。
func (t *ledgerTx) Upsert(ctx context.Context, entries []LedgerEntry) (int, error) {
	if len(entries) == 0 {
		return 0, nil
	}
	// 先整体校验：一条形状不对就整批不写（宁可整轮回滚，也不落半截账）。
	for i := range entries {
		if err := entries[i].Validate(); err != nil {
			return 0, err
		}
	}
	inserted := 0
	for start := 0; start < len(entries); start += ledgerBatchRows {
		end := start + ledgerBatchRows
		if end > len(entries) {
			end = len(entries)
		}
		rows := entries[start:end]
		query, args := buildLedgerInsert(rows)
		res, err := t.tx.ExecContext(ctx, query, args...)
		if err != nil {
			return inserted, fmt.Errorf("写入账本失败（%s 起 %d 条）：%w", rows[0].LedgerID, len(rows), err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return inserted, fmt.Errorf("读取账本写入行数失败：%w", err)
		}
		inserted += int(n)
	}
	return inserted, nil
}

// Commit 提交整轮账本写入。
func (t *ledgerTx) Commit() error { return t.tx.Commit() }

// Rollback 回滚整轮账本写入（已提交/已回滚时忽略错误）。
func (t *ledgerTx) Rollback() error { return t.tx.Rollback() }

// buildLedgerInsert 组装多行 INSERT（九列 + ON CONFLICT 幂等键）。
func buildLedgerInsert(entries []LedgerEntry) (string, []any) {
	var b strings.Builder
	b.WriteString(`INSERT INTO ` + LedgerTable + ` (id, ledger_id, provider, provider_account, model,
		tokens_in, tokens_out, cost_credits, occurred_at) VALUES `)
	args := make([]any, 0, len(entries)*LedgerFieldCount)
	for i := range entries {
		e := &entries[i]
		if e.ID == 0 {
			e.ID = idgen.New()
		}
		if i > 0 {
			b.WriteString(",")
		}
		base := i * LedgerFieldCount
		b.WriteString("($" + strconv.Itoa(base+1) + ",$" + strconv.Itoa(base+2) + ",$" + strconv.Itoa(base+3) +
			",$" + strconv.Itoa(base+4) + ",$" + strconv.Itoa(base+5) + ",$" + strconv.Itoa(base+6) +
			",$" + strconv.Itoa(base+7) + ",$" + strconv.Itoa(base+8) + ",$" + strconv.Itoa(base+9) + ")")
		args = append(args, e.ID, e.LedgerID, e.Provider, e.ProviderAccount, e.Model,
			e.TokensIn, e.TokensOut, e.CostCredits, e.OccurredAt)
	}
	b.WriteString(` ON CONFLICT (ledger_id) DO NOTHING`)
	return b.String(), args
}
