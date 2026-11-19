package upstream

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/pkg/dbx"
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
	// LedgerFieldCount 是账本字段数（十四：九字段 + req/served/caller/cache_read/cache_write）。
	LedgerFieldCount = 14
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
	// TokensIn / TokensOut 是**提示词侧总量 / 输出**。
	//
	// TokensIn 不是代理侧的 `in` 原值，而是 `in + cache_read + cache_write`（缓存并入输入侧）：
	// 灵枢 billing_records 的三段口径是纯输入（含 cache_write）+ cache_read + output，
	// 只有把缓存并进 tokens_in，`tokens_in + tokens_out` 才与 `billing.TokenSumExpr` 同口径
	// （代理侧 `in` 不含缓存）。适配层的合并点见 magpie_ledger.go 的 mapMagpieFeedRow。
	TokensIn  int64
	TokensOut int64
	// ReqModel 是客户端请求的模型名（=对外模型/路由组名，算营收），ServedModel 是厂商回执自称的模型。
	ReqModel    string
	ServedModel string
	// CallerKeyName 是代理侧 callerKey 名（=灵枢令牌映射，ls-token-<id>）。
	CallerKeyName string
	// CacheRead / CacheWrite 是缓存读/写 token（tokens_in 已含 cache_write，用于成本细分时使用）。
	CacheRead  int64
	CacheWrite int64
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
	// Upsert 幂等写入一批账本（ON CONFLICT (ledger_id) DO UPDATE；同 id 高 seq 覆盖旧值），
	// 返回受影响的条数（新插入 + 覆盖；仅用于观测，不参与正确性判断）。
	Upsert(ctx context.Context, entries []LedgerEntry) (int, error)
	// Delete 按代理侧 deleted 列表删除账本 id（行被合并/清除时必须同步删除，否则重复计数）。
	// 返回实际删除条数。
	Delete(ctx context.Context, ledgerIDs []string) (int, error)
	// Truncate 清空本地账本（返回删除条数）。
	//
	// 只在**游标失效重放**时用：代理侧 `cursor=start` 返回的是「账本现在的样子」，且明确
	// 「不给 deleted 列表」（docs/reference.md：Reading with `cursor=start` lists the ledger as
	// it is and no deleted ids）。只 upsert 的话，代理侧已经删掉/合并掉的旧 id 会永久留在本地，
	// 与重放出来的新 id 并存 → 同一通调用被计两次（实测 u_session 与 u_gateway 并存）。
	// 所以 reset 语义是「整段替换」：同一个事务里先 Truncate 再 Upsert。
	Truncate(ctx context.Context) (int64, error)
	Commit() error
	Rollback() error
	// Execer 返回事务执行器：窗口行必须与账本同事务写（契约 §6.2），
	// 通过它把「同步窗口状态」写进同一事务，而不是提交后再补一刀。
	Execer() dbx.Execer
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
		`SELECT id, ledger_id, provider, provider_account, model, tokens_in, tokens_out, cost_credits, occurred_at,
		        req_model, served_model, caller_key_name, cache_read, cache_write
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
			&e.TokensIn, &e.TokensOut, &e.CostCredits, &e.OccurredAt,
			&e.ReqModel, &e.ServedModel, &e.CallerKeyName, &e.CacheRead, &e.CacheWrite); err != nil {
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

// Delete 按代理侧 deleted 列表删除账本 id（同事务，与 upsert 一起提交）。
//
// 用 IN($1..$n) 而不是 ANY($1::text[])：不依赖驱动对切片参数的编码，sqlmock 也能断言。
func (t *ledgerTx) Delete(ctx context.Context, ledgerIDs []string) (int, error) {
	if len(ledgerIDs) == 0 {
		return 0, nil
	}
	deleted := 0
	for start := 0; start < len(ledgerIDs); start += ledgerBatchRows {
		end := start + ledgerBatchRows
		if end > len(ledgerIDs) {
			end = len(ledgerIDs)
		}
		ids := ledgerIDs[start:end]
		marks := make([]string, len(ids))
		args := make([]any, len(ids))
		for i, id := range ids {
			marks[i] = "$" + strconv.Itoa(i+1)
			args[i] = id
		}
		res, err := t.tx.ExecContext(ctx,
			`DELETE FROM `+LedgerTable+` WHERE ledger_id IN (`+strings.Join(marks, ", ")+`)`, args...)
		if err != nil {
			return deleted, fmt.Errorf("删除账本失败（%s 起 %d 条）：%w", ids[0], len(ids), err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return deleted, fmt.Errorf("读取账本删除行数失败：%w", err)
		}
		deleted += int(n)
	}
	return deleted, nil
}

// Truncate 清空本地账本（游标失效重放时用，见 LedgerTx.Truncate 的说明）。
//
// 与 Upsert 同事务：先删后写要么都生效、要么都不生效 —— 中途失败时旧账本原样保留，
// 不会留下「清空了但没重放」的空账（那会让对账把整段流量当成年账本侧缺失）。
func (t *ledgerTx) Truncate(ctx context.Context) (int64, error) {
	res, err := t.tx.ExecContext(ctx, `DELETE FROM `+LedgerTable)
	if err != nil {
		return 0, fmt.Errorf("清空本地账本失败：%w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("读取清空账本行数失败：%w", err)
	}
	return n, nil
}

// Commit 提交整轮账本写入。
func (t *ledgerTx) Commit() error { return t.tx.Commit() }

// Rollback 回滚整轮账本写入（已提交/已回滚时忽略错误）。
func (t *ledgerTx) Rollback() error { return t.tx.Rollback() }

// Execer 返回事务执行器（供窗口行同事务写入）。
func (t *ledgerTx) Execer() dbx.Execer { return t.tx }

// buildLedgerInsert 组装多行 INSERT（九列 + ON CONFLICT 幂等键）。
func buildLedgerInsert(entries []LedgerEntry) (string, []any) {
	var b strings.Builder
	b.WriteString(`INSERT INTO ` + LedgerTable + ` (id, ledger_id, provider, provider_account, model,
		tokens_in, tokens_out, cost_credits, occurred_at,
		req_model, served_model, caller_key_name, cache_read, cache_write) VALUES `)
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
		ph := make([]string, LedgerFieldCount)
		for j := 0; j < LedgerFieldCount; j++ {
			ph[j] = "$" + strconv.Itoa(base+j+1)
		}
		b.WriteString("(" + strings.Join(ph, ",") + ")")
		args = append(args, e.ID, e.LedgerID, e.Provider, e.ProviderAccount, e.Model,
			e.TokensIn, e.TokensOut, e.CostCredits, e.OccurredAt,
			e.ReqModel, e.ServedModel, e.CallerKeyName, e.CacheRead, e.CacheWrite)
	}
	// 同一 id 以更高 seq 再次出现 = 代理侧这一行被改过（例如成本补齐、账号修正），
	// 必须覆盖；DO NOTHING 会让灵枢永久保留第一次读到的旧值。
	b.WriteString(` ON CONFLICT (ledger_id) DO UPDATE SET
		provider = EXCLUDED.provider,
		provider_account = EXCLUDED.provider_account,
		model = EXCLUDED.model,
		tokens_in = EXCLUDED.tokens_in,
		tokens_out = EXCLUDED.tokens_out,
		cost_credits = EXCLUDED.cost_credits,
		occurred_at = EXCLUDED.occurred_at,
		req_model = EXCLUDED.req_model,
		served_model = EXCLUDED.served_model,
		caller_key_name = EXCLUDED.caller_key_name,
		cache_read = EXCLUDED.cache_read,
		cache_write = EXCLUDED.cache_write`)
	return b.String(), args
}
