package subscription

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"

	"github.com/team/llmgateway/internal/pkg/billingday"
)

// Store 是订阅的读写端口（单测注入 fake，生产用 SQLStore）。
type Store interface {
	// Providers 返回拓扑镜像里的供应商集合（internal_models.provider_ref 去重，去空）。
	Providers(ctx context.Context) ([]string, error)
	// KnownAccounts 返回近 since 以来在账单/账本里出现过的上游账号（软校验用）。
	KnownAccounts(ctx context.Context, since time.Time) (map[string]struct{}, error)
	// Create 插入一行；自然键冲突返回 ErrDuplicate。
	Create(ctx context.Context, row *Row) error
	// GetByNaturalKey 按 (provider, account) 查一行；没有返回 ErrNotFound。
	GetByNaturalKey(ctx context.Context, provider, account string) (*Row, error)
	// Allocated 返回该账号在 [from,to) 内已被订阅摊销写下的成本合计（S1/S4 的「本期已摊」现算）。
	Allocated(ctx context.Context, provider, account string, from, to time.Time) (float64, error)
	// List 返回全部订阅 + 各自本期已摊（一条 SQL 算完，避免 N+1）。
	List(ctx context.Context) ([]ListRow, error)
}

// ListRow 是列表行：订阅本体 + 本期已摊。
type ListRow struct {
	Row
	// Allocated 是本账期内已摊积分；账期缺失（cycle_start IS NULL）时为 0 且 HasCycle=false。
	Allocated float64
	HasCycle  bool
}

// SQLStore 是 Store 的 PostgreSQL 实现。
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore 创建 SQL 存储。
func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

var _ Store = (*SQLStore)(nil)

// Providers 读拓扑镜像里的供应商集合。
func (s *SQLStore) Providers(ctx context.Context) ([]string, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("订阅存储未装配数据库连接")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT provider_ref FROM internal_models WHERE btrim(provider_ref) <> '' ORDER BY provider_ref`)
	if err != nil {
		return nil, fmt.Errorf("读取供应商候选失败：%w", err)
	}
	defer rows.Close()
	out := make([]string, 0, 8)
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("扫描供应商候选失败：%w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历供应商候选失败：%w", err)
	}
	return out, nil
}

// KnownAccounts 读近 30 天账单与账本里出现过的上游账号（软校验，不阻断）。
//
// 两个来源都要：账单侧是「我们自己记的归因」，账本侧是「上游给的账号」——
// 只有账单时，账本拉到但账单还没归因的账号会被误报为「未验证」。
func (s *SQLStore) KnownAccounts(ctx context.Context, since time.Time) (map[string]struct{}, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("订阅存储未装配数据库连接")
	}
	out := make(map[string]struct{}, 32)
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT upstream_account FROM billing_records
		  WHERE call_time >= $1 AND upstream_account IS NOT NULL AND btrim(upstream_account) <> ''
		 UNION
		 SELECT DISTINCT provider_account FROM ledger_entries
		  WHERE occurred_at >= $1 AND btrim(provider_account) <> ''`, since)
	if err != nil {
		return nil, fmt.Errorf("读取已知账号失败：%w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, fmt.Errorf("扫描已知账号失败：%w", err)
		}
		out[a] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历已知账号失败：%w", err)
	}
	return out, nil
}

const subscriptionCols = `id, provider_ref, account_ref, price_amount, price_currency,
	billing_cycle, pay_day, share_cost, cycle_start, note, created_at, updated_at`

// Create 插入一行订阅。
func (s *SQLStore) Create(ctx context.Context, row *Row) error {
	if s == nil || s.db == nil {
		return errors.New("订阅存储未装配数据库连接")
	}
	// DATE 列只收 `YYYY-MM-DD` 文本：传 time.Time 会先经「按会话时区取日期」，
	// 在非 UTC+8 的连接上整整差一天（billingday 包注释里有完整说明）。
	var start any
	if row.CycleStart != nil {
		start = billingday.Date(*row.CycleStart)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO subscriptions(`+subscriptionCols+`)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9::date,$10,now(),now())`,
		row.ID, row.Provider, row.Account, row.PriceCredits, row.Currency,
		row.Cycle, row.PayDay, row.ShareCost, start, row.Note)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrDuplicate
		}
		return fmt.Errorf("写入订阅失败：%w", err)
	}
	return nil
}

// GetByNaturalKey 按自然键查一行。
func (s *SQLStore) GetByNaturalKey(ctx context.Context, provider, account string) (*Row, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("订阅存储未装配数据库连接")
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT `+subscriptionCols+` FROM subscriptions WHERE provider_ref = $1 AND account_ref = $2`,
		provider, account)
	out, err := scanRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return out, err
}

// Allocated 读该账号本期已摊金额（cost_source='amortized'）。
//
// 只看 amortized：ledger 是变动成本（账本口径），混进来会把「订阅摊了多少」算多。
func (s *SQLStore) Allocated(ctx context.Context, provider, account string, from, to time.Time) (float64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("订阅存储未装配数据库连接")
	}
	var sum float64
	if err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(sum(cost_credits), 0) FROM billing_records
		  WHERE upstream_provider = $1 AND upstream_account = $2
		    AND cost_source = 'amortized'
		    AND call_time >= $3 AND call_time < $4`,
		provider, account, from, to).Scan(&sum); err != nil {
		return 0, fmt.Errorf("读取本期已摊失败：%w", err)
	}
	return sum, nil
}

func scanRow(row interface{ Scan(...any) error }) (*Row, error) {
	var (
		r     Row
		start sql.NullTime
	)
	if err := row.Scan(&r.ID, &r.Provider, &r.Account, &r.PriceCredits, &r.Currency,
		&r.Cycle, &r.PayDay, &r.ShareCost, &start, &r.Note, &r.CreatedAt, &r.UpdatedAt); err != nil {
		return nil, err
	}
	if start.Valid {
		t := start.Time
		r.CycleStart = &t
	}
	r.Currency = strings.ToUpper(strings.TrimSpace(r.Currency))
	return &r, nil
}

// listSQL 一次算完「订阅 + 本期已摊」。
//
// 本期窗口按每行自己的 cycle_start 与 billing_cycle 现算（LATERAL 子查询），
// 所以不能用一条固定区间的 GROUP BY —— 各订阅账期起点不同，固定区间会把别的账期的钱算进来。
// cycle_start 为空的行（状态 missing_cycle）窗口无意义，用 NULL 过滤掉 → Allocated=0 且 HasCycle=false。
// 时区口径：`cycle_start` 是 DATE，直接与 timestamptz 比较时 PG 会按**会话的 TimeZone** 把
// DATE 转成时刻 —— 会话时区不是 China/Asia 时，窗口整体错位（列表与分摊各算一个期）。
// 所以显式 `AT TIME ZONE $1`（$1 = billingday.Name()），与 Go 侧窗口同源，不依赖会话设置。
const listSQL = `
SELECT s.id, s.provider_ref, s.account_ref, s.price_amount, s.price_currency,
       s.billing_cycle, s.pay_day, s.share_cost, s.cycle_start, s.note,
       s.created_at, s.updated_at,
       s.cycle_start IS NOT NULL AS has_cycle,
       COALESCE(a.allocated, 0) AS allocated
  FROM subscriptions s
  LEFT JOIN LATERAL (
        SELECT sum(b.cost_credits) AS allocated
          FROM billing_records b
         WHERE b.upstream_provider = s.provider_ref
           AND b.upstream_account = s.account_ref
           AND b.cost_source = 'amortized'
           AND b.call_time >= (s.cycle_start::timestamp AT TIME ZONE $1)
           AND b.call_time <  ((CASE s.billing_cycle
                                 WHEN 'year' THEN s.cycle_start + interval '1 year'
                                 WHEN 'once' THEN s.cycle_start + interval '1 day'
                                 ELSE s.cycle_start + interval '1 month'
                               END) AT TIME ZONE $1)
       ) a ON true
 ORDER BY s.created_at DESC, s.id DESC`

// List 返回全部订阅（过滤/分页在服务层做：状态是现算的，SQL 里过滤不了）。
func (s *SQLStore) List(ctx context.Context) ([]ListRow, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("订阅存储未装配数据库连接")
	}
	rows, err := s.db.QueryContext(ctx, listSQL, billingday.Name())
	if err != nil {
		return nil, fmt.Errorf("读取订阅列表失败：%w", err)
	}
	defer rows.Close()
	out := make([]ListRow, 0, 16)
	for rows.Next() {
		var (
			it     ListRow
			start  sql.NullTime
			hasCyc bool
		)
		if err := rows.Scan(&it.ID, &it.Provider, &it.Account, &it.PriceCredits, &it.Currency,
			&it.Cycle, &it.PayDay, &it.ShareCost, &start, &it.Note,
			&it.CreatedAt, &it.UpdatedAt, &hasCyc, &it.Allocated); err != nil {
			return nil, fmt.Errorf("扫描订阅列表失败：%w", err)
		}
		if start.Valid {
			t := start.Time
			it.CycleStart = &t
		}
		it.HasCycle = hasCyc && start.Valid
		it.Currency = strings.ToUpper(strings.TrimSpace(it.Currency))
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历订阅列表失败：%w", err)
	}
	return out, nil
}
