package topology

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Store 是拓扑的读写端口（单测注入 fake，生产用 SQLStore）。
type Store interface {
	// Nodes 返回镜像里的全部节点（含 stale）。
	Nodes(ctx context.Context) ([]Node, error)
	// Links 返回画布落位（缺行 = 自动布局，不影响售卖/计费）。
	Links(ctx context.Context) ([]Link, error)
	// Counts 返回 stale 节点数与「未绑定对外模型」数。
	Counts(ctx context.Context) (Counts, error)
	// Providers 返回第 1 层：供应商及其内部模型计数（由镜像聚合，不落单独的表）。
	Providers(ctx context.Context) ([]Provider, error)
	// Accounts 返回第 2 层：账单/账本/订阅里出现过的上游账号（并集去重）。
	Accounts(ctx context.Context) ([]Account, error)
	// Externals 返回第 4 层：未软删的对外模型 + 绑定目标（绑定边即出自这里）。
	Externals(ctx context.Context) ([]External, error)
	// SaveLinks 覆写画布落位（T2）：按 (internal_ref, external_model_id) upsert。
	// 返回实际写入行数；目标不存在（内部模型或对外模型）时返回 ErrLinkTargetMissing。
	SaveLinks(ctx context.Context, links []Link) (int, error)
	// LastSeenAt 返回最近一次同步命中的时间（没有任何节点时 ok=false）。
	LastSeenAt(ctx context.Context) (time.Time, bool, error)
	// Replace 在一轮同步内整批 upsert 节点，并把本轮未命中的老节点置 stale。
	// 返回本轮新增数与置 stale 数。**不删任何行**。
	Replace(ctx context.Context, nodes []Node, runAt time.Time) (created, stale int, err error)
	// LoadState 读同步状态；没有记录返回零值（State 空串，由视图映射成 never）。
	LoadState(ctx context.Context) (SyncState, error)
	// SaveState 覆写同步状态。
	SaveState(ctx context.Context, st SyncState) error
}

// SQLStore 是 Store 的 PostgreSQL 实现。
type SQLStore struct {
	db *sql.DB
}

// NewSQLStore 创建 SQL 存储。
func NewSQLStore(db *sql.DB) *SQLStore { return &SQLStore{db: db} }

var _ Store = (*SQLStore)(nil)

// Nodes 读镜像节点。
func (s *SQLStore) Nodes(ctx context.Context) ([]Node, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("拓扑存储未装配数据库连接")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT ref, provider_ref, kind, display_name, context_len, caps, list_price,
		        pos_x, pos_y, last_seen_at, stale, cost_rates
		   FROM internal_models
		  ORDER BY provider_ref, ref`)
	if err != nil {
		return nil, fmt.Errorf("读取内部模型失败：%w", err)
	}
	defer rows.Close()

	out := make([]Node, 0, 32)
	for rows.Next() {
		var (
			n      Node
			caps   []byte
			price  []byte
			cost   []byte
			lastAt sql.NullTime
		)
		if err := rows.Scan(&n.Ref, &n.ProviderRef, &n.Kind, &n.DisplayName, &n.ContextLen,
			&caps, &price, &n.PosX, &n.PosY, &lastAt, &n.Stale, &cost); err != nil {
			return nil, fmt.Errorf("扫描内部模型失败：%w", err)
		}
		n.Caps = rawOrEmpty(caps)
		n.ListPrice = rawOrEmpty(price)
		n.CostRates = rawOrEmpty(cost)
		if lastAt.Valid {
			n.LastSeenAt = lastAt.Time
		}
		out = append(out, n)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历内部模型失败：%w", err)
	}
	return out, nil
}

// Links 读画布落位。
func (s *SQLStore) Links(ctx context.Context) ([]Link, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("拓扑存储未装配数据库连接")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT internal_ref, external_model_id, pos_x, pos_y
		   FROM model_links
		  ORDER BY internal_ref, external_model_id`)
	if err != nil {
		return nil, fmt.Errorf("读取画布落位失败：%w", err)
	}
	defer rows.Close()

	out := make([]Link, 0, 16)
	for rows.Next() {
		var l Link
		if err := rows.Scan(&l.InternalRef, &l.ExternalModelID, &l.PosX, &l.PosY); err != nil {
			return nil, fmt.Errorf("扫描画布落位失败：%w", err)
		}
		out = append(out, l)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历画布落位失败：%w", err)
	}
	return out, nil
}

// Counts 读两个计数。
//
// 「未绑定对外模型」= 未软删的对外模型里 bound_internal_ref 为空/空白。空白也计入：
// 历史数据里空串与 NULL 都表示未绑定（绑定真源只有一列，两种写法必须同一个含义）。
func (s *SQLStore) Counts(ctx context.Context) (Counts, error) {
	if s == nil || s.db == nil {
		return Counts{}, errors.New("拓扑存储未装配数据库连接")
	}
	var c Counts
	err := s.db.QueryRowContext(ctx,
		`SELECT (SELECT count(*) FROM internal_models WHERE stale),
		        (SELECT count(*) FROM external_models
		          WHERE deleted_at IS NULL
		            AND (bound_internal_ref IS NULL OR btrim(bound_internal_ref) = ''))`).
		Scan(&c.Stale, &c.UnboundExternal)
	if err != nil {
		return Counts{}, fmt.Errorf("读取拓扑计数失败：%w", err)
	}
	return c, nil
}

// Providers 读第 1 层：镜像里按 provider_ref 聚合的模型计数。
//
// 空 provider_ref 不丢弃（显示成「(未知)」由前端决定）：同步器可能在网关未给 provider 时留空，
// 静默丢行会让「供应商层条数之和 ≠ 内部模型条数」。
func (s *SQLStore) Providers(ctx context.Context) ([]Provider, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("拓扑存储未装配数据库连接")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT provider_ref,
		        count(*)                        AS models,
		        count(*) FILTER (WHERE stale)   AS stale
		   FROM internal_models
		  GROUP BY provider_ref
		  ORDER BY count(*) DESC, provider_ref`)
	if err != nil {
		return nil, fmt.Errorf("读取供应商聚合失败：%w", err)
	}
	defer rows.Close()

	out := make([]Provider, 0, 8)
	for rows.Next() {
		var p Provider
		if err := rows.Scan(&p.Provider, &p.Models, &p.Stale); err != nil {
			return nil, fmt.Errorf("扫描供应商聚合失败：%w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历供应商聚合失败：%w", err)
	}
	return out, nil
}

// Accounts 读第 2 层：账单 / 账本 / 订阅三处的账号并集。
//
// 三处都要：只查账单会漏「账本已到、账单还没归因」的账号；漏订阅会把「有固定月费但本期没流量」
// 的账号从图上抹掉（运营正好要找它）。
//
// 成本：账单侧是一次 (provider, account) 聚合，走 0020 的部分索引 idx_billing_upstream_time
// （WHERE upstream_provider IS NOT NULL，正是这里的谓词），不回落堆表。
// 该查询的规模由「distinct 账号数」决定、与账单行数无关；账单表继续长大时这一条会变成 T1 的主要开销，
// 届时改成物化视图（本批不加迁移：迁移右界仍是 0025，且没有第二处写它）。
func (s *SQLStore) Accounts(ctx context.Context) ([]Account, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("拓扑存储未装配数据库连接")
	}
	rows, err := s.db.QueryContext(ctx,
		`WITH src AS (
		    SELECT upstream_provider    AS provider,
		           upstream_account     AS account,
		           max(call_time)       AS last_seen,
		           false                AS subscribed
		      FROM billing_records
		     -- 谓词与 0020 的部分索引 idx_billing_upstream_time 逐字一致，否则规划器用不上它
		     WHERE upstream_provider IS NOT NULL
		       AND upstream_account IS NOT NULL AND btrim(upstream_account) <> ''
		     GROUP BY 1, 2
		    UNION ALL
		    SELECT provider, provider_account, max(occurred_at), false
		      FROM ledger_entries
		     WHERE btrim(provider_account) <> ''
		     GROUP BY 1, 2
		    UNION ALL
		    SELECT provider_ref, account_ref, NULL::timestamptz, true
		      FROM subscriptions
		 )
		 SELECT provider, account,
		        bool_or(subscribed)    AS subscribed,   -- PG 没有 max(boolean)
		        max(last_seen)         AS last_seen
		   FROM src
		  GROUP BY 1, 2
		  ORDER BY 1, 2`)
	if err != nil {
		return nil, fmt.Errorf("读取账号层失败：%w", err)
	}
	defer rows.Close()

	out := make([]Account, 0, 32)
	for rows.Next() {
		var (
			a        Account
			lastSeen sql.NullTime
		)
		if err := rows.Scan(&a.Provider, &a.Account, &a.Subscribed, &lastSeen); err != nil {
			return nil, fmt.Errorf("扫描账号层失败：%w", err)
		}
		if lastSeen.Valid {
			a.LastSeenAt = lastSeen.Time
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历账号层失败：%w", err)
	}
	return out, nil
}

// Externals 读第 4 层：未软删的对外模型 + 绑定目标（绑定边出自这一列）。
func (s *SQLStore) Externals(ctx context.Context) ([]External, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("拓扑存储未装配数据库连接")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, external_name, enabled, COALESCE(bound_internal_ref, '')
		   FROM external_models
		  WHERE deleted_at IS NULL
		  ORDER BY external_name, id`)
	if err != nil {
		return nil, fmt.Errorf("读取对外模型层失败：%w", err)
	}
	defer rows.Close()

	out := make([]External, 0, 32)
	for rows.Next() {
		var e External
		if err := rows.Scan(&e.ID, &e.ExternalName, &e.Enabled, &e.BoundInternalRef); err != nil {
			return nil, fmt.Errorf("扫描对外模型层失败：%w", err)
		}
		e.BoundInternalRef = strings.TrimSpace(e.BoundInternalRef)
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("遍历对外模型层失败：%w", err)
	}
	return out, nil
}

// SaveLinks 覆写画布落位（T2）。
//
// 一句话口径：**model_links 只存坐标**，绝不在这里建立/修改绑定关系（真源是
// external_models.bound_internal_ref）。所以这里只 upsert 坐标，不写任何语义。
//
// 存在性检查放在 SQL 的 WHERE 里：不满足则 0 行，映射成 ErrLinkTargetMissing（400），
// 而不是让外键退化成 23503（500）—— 运营改错一个 ref 不该像服务端崩了。
func (s *SQLStore) SaveLinks(ctx context.Context, links []Link) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("拓扑存储未装配数据库连接")
	}
	if len(links) == 0 {
		return 0, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("开启画布落位事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback() }()

	saved := 0
	for _, l := range links {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO model_links(internal_ref, external_model_id, pos_x, pos_y, updated_at)
			 SELECT $1, $2, $3, $4, now()
			  WHERE EXISTS (SELECT 1 FROM internal_models WHERE ref = $1)
			    AND EXISTS (SELECT 1 FROM external_models WHERE id = $2 AND deleted_at IS NULL)
			 ON CONFLICT (internal_ref, external_model_id) DO UPDATE SET
			     pos_x      = EXCLUDED.pos_x,
			     pos_y      = EXCLUDED.pos_y,
			     updated_at = EXCLUDED.updated_at`,
			l.InternalRef, l.ExternalModelID, l.PosX, l.PosY)
		if err != nil {
			return 0, fmt.Errorf("写入画布落位失败（%s）：%w", l.InternalRef, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, fmt.Errorf("读取画布落位写入行数失败：%w", err)
		}
		if n == 0 {
			return 0, fmt.Errorf("%w：internal_ref=%s external_model_id=%d",
				ErrLinkTargetMissing, l.InternalRef, l.ExternalModelID)
		}
		saved += int(n)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("提交画布落位事务失败：%w", err)
	}
	return saved, nil
}

// LastSeenAt 读最近一次命中的时间。
func (s *SQLStore) LastSeenAt(ctx context.Context) (time.Time, bool, error) {
	if s == nil || s.db == nil {
		return time.Time{}, false, errors.New("拓扑存储未装配数据库连接")
	}
	var t sql.NullTime
	if err := s.db.QueryRowContext(ctx, `SELECT max(last_seen_at) FROM internal_models`).Scan(&t); err != nil {
		return time.Time{}, false, fmt.Errorf("读取最近同步时间失败：%w", err)
	}
	return t.Time, t.Valid, nil
}

// Replace 整批 upsert 并把未命中的置 stale（单事务）。
//
// `RETURNING (xmax = 0)` 是新旧行判别的标准手法：ON CONFLICT DO UPDATE 时插入行 xmax=0、
// 更新行 xmax≠0，因此「新增」不需要事后再查一次集合差。
func (s *SQLStore) Replace(ctx context.Context, nodes []Node, runAt time.Time) (int, int, error) {
	if s == nil || s.db == nil {
		return 0, 0, errors.New("拓扑存储未装配数据库连接")
	}
	if len(nodes) == 0 {
		// 空快照不许进 Replace：那会把全部存量节点标成 stale（页面集体变灰）。
		return 0, 0, ErrEmptySnapshot
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("开启拓扑同步事务失败：%w", err)
	}
	defer func() { _ = tx.Rollback() }()

	created := 0
	for _, n := range nodes {
		kind := n.Kind
		if kind != KindGroup {
			kind = KindDirect
		}
		var inserted bool
		err := tx.QueryRowContext(ctx,
			`INSERT INTO internal_models(ref, provider_ref, kind, display_name, context_len, caps, list_price,
			                             last_seen_at, stale, updated_at)
			 VALUES($1,$2,$3,$4,$5,$6::jsonb,$7::jsonb,$8,false,$8)
			 ON CONFLICT (ref) DO UPDATE SET
			     provider_ref = EXCLUDED.provider_ref,
			     kind         = EXCLUDED.kind,
			     display_name = EXCLUDED.display_name,
			     context_len  = EXCLUDED.context_len,
			     caps         = EXCLUDED.caps,
			     list_price   = EXCLUDED.list_price,
			     last_seen_at = EXCLUDED.last_seen_at,
			     stale        = false,
			     updated_at   = EXCLUDED.updated_at
			 RETURNING (xmax = 0)`,
			n.Ref, n.ProviderRef, kind, n.DisplayName, n.ContextLen,
			jsonOrEmpty(n.Caps), jsonOrEmpty(n.ListPrice), runAt).Scan(&inserted)
		if err != nil {
			return 0, 0, fmt.Errorf("写入内部模型 %s 失败：%w", n.Ref, err)
		}
		if inserted {
			created++
		}
	}

	// 本轮没命中的老节点标灰（不删行：删除判定归网关）。
	res, err := tx.ExecContext(ctx,
		`UPDATE internal_models SET stale = true, updated_at = $2
		  WHERE last_seen_at < $1 AND stale = false`, runAt, runAt)
	if err != nil {
		return 0, 0, fmt.Errorf("标记失效节点失败：%w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, 0, fmt.Errorf("读取失效节点数失败：%w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("提交拓扑同步事务失败：%w", err)
	}
	return created, int(n), nil
}

// LoadState 读同步状态。
func (s *SQLStore) LoadState(ctx context.Context) (SyncState, error) {
	if s == nil || s.db == nil {
		return SyncState{}, errors.New("拓扑存储未装配数据库连接")
	}
	var raw []byte
	err := s.db.QueryRowContext(ctx, `SELECT value FROM sys_configs WHERE key = $1`, stateConfigKey).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return SyncState{}, nil // 从未同步：State 空串，视图映射成 never
	}
	if err != nil {
		return SyncState{}, fmt.Errorf("读取同步状态失败：%w", err)
	}
	var st SyncState
	if err := json.Unmarshal(raw, &st); err != nil {
		// 状态坏了不该让整页 500：当作「从未同步」并保留可见原因。
		return SyncState{State: StateFailed, Message: "同步状态记录损坏，已忽略：" + err.Error()}, nil
	}
	return st, nil
}

// SaveState 覆写同步状态。
func (s *SQLStore) SaveState(ctx context.Context, st SyncState) error {
	if s == nil || s.db == nil {
		return errors.New("拓扑存储未装配数据库连接")
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return fmt.Errorf("序列化同步状态失败：%w", err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO sys_configs(key, value, updated_at) VALUES($1, $2::jsonb, now())
		 ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()`,
		stateConfigKey, raw); err != nil {
		return fmt.Errorf("写入同步状态失败：%w", err)
	}
	return nil
}

func rawOrEmpty(raw []byte) json.RawMessage {
	if len(raw) == 0 {
		return json.RawMessage(`{}`)
	}
	return json.RawMessage(raw)
}

func jsonOrEmpty(raw json.RawMessage) []byte {
	if len(raw) == 0 || string(raw) == "null" {
		return []byte(`{}`)
	}
	return raw
}

// SetCostRates 仅更新内部模型的成本价（灵枢可写字段，其余仍为网关镜像）。
func (s *SQLStore) SetCostRates(ctx context.Context, ref string, costJSON []byte) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("拓扑存储未装配数据库连接")
	}
	res, err := s.db.ExecContext(ctx, `UPDATE internal_models SET cost_rates = $1, updated_at = now() WHERE ref = $2`, costJSON, ref)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
