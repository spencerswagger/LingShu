// Package model 提供对外模型（external_models）的存储与 CRUD：对外名称 + 售价定价
// （三段 rates：input/output/cache_read + 模型级 time_config/context_tiers）。渠道内部模型与成本定价在
// channel 包（channel_models）管理，此处不涉及。
//
// 数据模型对齐 db/migrations/0001_init.sql 中 external_models 表。
package model

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/gencache"
	"github.com/team/llmgateway/internal/pkg/idgen"
)

// ExternalModel 对应 external_models 表的一行。
// SaleRates 为售价（三段单价）；TimeConfig/ContextTiers 可空，空则运行时回落全局。
type ExternalModel struct {
	ID           int64
	ExternalName string
	Description  string
	Enabled      bool
	SaleRates    billing.Rates
	TimeConfig   *billing.TimeCoeffConfig
	ContextTiers []billing.TierRule
	// Aliases 为仅用于模型名解析的别名列表（不出现在 GET /v1/models）；空为无别名。
	Aliases   []string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// cols 列出 external_models 表查询使用的全部列。
const cols = `id, external_name, description, enabled, sale_rates, time_config, context_tiers, aliases, created_at, updated_at`

// scanExternal 将一行扫描到 *ExternalModel，解析 sale_rates/time_config/context_tiers/aliases JSONB。
func scanExternal(row interface{ Scan(...any) error }) (*ExternalModel, error) {
	var m ExternalModel
	var saleRaw, timeRaw, tiersRaw, aliasesRaw []byte
	err := row.Scan(&m.ID, &m.ExternalName, &m.Description, &m.Enabled, &saleRaw,
		&timeRaw, &tiersRaw, &aliasesRaw, &m.CreatedAt, &m.UpdatedAt)
	if err != nil {
		return nil, err
	}
	if len(saleRaw) > 0 {
		if err := json.Unmarshal(saleRaw, &m.SaleRates); err != nil {
			return nil, fmt.Errorf("parse sale_rates: %w", err)
		}
	}
	if len(timeRaw) > 0 && string(timeRaw) != "null" {
		var tc billing.TimeCoeffConfig
		if err := json.Unmarshal(timeRaw, &tc); err != nil {
			return nil, fmt.Errorf("parse time_config: %w", err)
		}
		m.TimeConfig = &tc
	}
	if len(tiersRaw) > 0 && string(tiersRaw) != "null" {
		if err := json.Unmarshal(tiersRaw, &m.ContextTiers); err != nil {
			return nil, fmt.Errorf("parse context_tiers: %w", err)
		}
	}
	// aliases 恒为 JSONB NOT NULL；扫描结果统一为非 nil 切片（无别名为空切片）。
	m.Aliases = []string{}
	if len(aliasesRaw) > 0 && string(aliasesRaw) != "null" {
		if err := json.Unmarshal(aliasesRaw, &m.Aliases); err != nil {
			return nil, fmt.Errorf("parse aliases: %w", err)
		}
	}
	return &m, nil
}

// Store 提供 external_models 表的基础数据访问。
type Store struct {
	db *sql.DB

	// extNameCache 缓存 GetByExternalName 的结果（网关热路径每请求按对外名读取，配置低频变更）。
	// 存储、FIFO 淘汰、失效代际均由 pkg/gencache 原语承担；本类型只保留 readCacheOff 开关
	// 与 value 语义（负缓存 nil、读侧 cloneExternal 深拷贝）。
	//   - 数据来源：external_models 表（仅 enabled=true 且 deleted_at IS NULL 的启用行）；
	//   - 键值语义：键 = external_name；值为命中的启用模型（查询所得完整快照，含 id/enabled）；
	//     映射内 value=nil 表示「已确认无启用模型」的负缓存，避免不存在/停用名反复穿透到 DB；
	//   - 有界性：键来自请求体（用户可控），故**有界**（原语 FIFO 淘汰，上限 defaultExternalNameCacheMax）：
	//     防止认证用户以海量随机名打满负缓存造成内存耗尽 DoS。淘汰代价仅为回源一次 DB，不影响正确性。
	//   - 失效点（写路径显式失效，无 TTL）：
	//     本文件 Insert（按名失效）、Update/BatchDelete（无旧名信息，整体清空）；
	//     ApplySaleRates 自身**不再失效**（避免批量同步逐次全清写放大），由调用方负责：
	//     service.ApplyPrice 按名失效、service.SyncPrices 循环结束后整体清空一次（均含跨包 hook）；
	//     以及 service.go 的 CreateModel/UpdateModel/BatchDeleteModels/ApplyPrice/SyncPrices。
	//   - 关闭开关（SetReadCacheEnabled(false)）时退回直查，不读写缓存。
	extNameCache *gencache.Cache[*ExternalModel]
	// readCacheOff 为 true 时关闭缓存（安全态：全部直查 DB）。仅启动装配阶段设置，运行期不变。
	readCacheOff bool
}

// defaultExternalNameCacheMax 对外名读缓存条目上限（键用户可控，必须有界）。
const defaultExternalNameCacheMax = 100000

// NewStore 创建对外模型存储，db 为 pgx stdlib 连接池。
func NewStore(db *sql.DB) *Store {
	return &Store{db: db, extNameCache: gencache.New[*ExternalModel](defaultExternalNameCacheMax)}
}

// SetReadCacheEnabled 开关对外模型名读缓存（默认开启）；关闭后 GetByExternalName 直查 DB。
func (s *Store) SetReadCacheEnabled(on bool) { s.readCacheOff = !on }

// cacheExternalName 写入对外名缓存（m 为 nil 表示负缓存「无启用模型」），仅在代际仍为 gen 时写入。
// gen 为读路径回源前捕获的代际：回源期间若发生任一次失效（写路径），本次回填被原语拒绝，
// 避免失效前读到的旧快照（本缓存无 TTL）无限期滞留。
func (s *Store) cacheExternalName(name string, m *ExternalModel, gen uint64) {
	s.extNameCache.PutAtGen(name, "", m, gen)
}

// invalidateExternalName 失效单个对外名称的缓存（写入后调用）。
func (s *Store) invalidateExternalName(name string) {
	s.extNameCache.Invalidate(name)
}

// invalidateAllExternalNames 清空全部对外名称缓存（缺少旧名信息时使用，如改名/批量删除）。
func (s *Store) invalidateAllExternalNames() {
	s.extNameCache.InvalidateAll()
}

// cloneExternal 返回 ExternalModel 的防御性深拷贝，避免缓存内部快照被外部消费方就地修改而污染缓存（P2-7/P2-4）。
// 拷贝深度说明：
//   - 值字段（含 *billing.TimeCoeffConfig 结构体本体）按值复制；
//   - SaleRates map 复制为独立 map；
//   - ContextTiers 每个 TierRule 按值复制，且 Max *int64 解引用后重新取址，避免共享同一指针；
//   - TimeConfig.Periodic / Overrides 及其内层 Segments / Weekdays 切片均重新分配，
//     保证消费者修改任一嵌套切片元素都不会回写缓存快照。
func cloneExternal(m *ExternalModel) *ExternalModel {
	if m == nil {
		return nil
	}
	c := *m
	if m.SaleRates != nil {
		c.SaleRates = make(billing.Rates, len(m.SaleRates))
		for k, v := range m.SaleRates {
			c.SaleRates[k] = v
		}
	}
	if m.ContextTiers != nil {
		c.ContextTiers = make([]billing.TierRule, len(m.ContextTiers))
		for i, t := range m.ContextTiers {
			ct := t
			if t.Max != nil {
				v := *t.Max
				ct.Max = &v
			}
			c.ContextTiers[i] = ct
		}
	}
	if m.TimeConfig != nil {
		tc := *m.TimeConfig
		tc.Periodic = cloneSegments(m.TimeConfig.Periodic)
		tc.Overrides = cloneOverrides(m.TimeConfig.Overrides)
		c.TimeConfig = &tc
	}
	return &c
}

// cloneSegments 深拷贝时段切片（含 Weekdays 内层切片），nil 保持为 nil。
func cloneSegments(in []billing.Segment) []billing.Segment {
	if in == nil {
		return nil
	}
	out := make([]billing.Segment, len(in))
	for i, s := range in {
		cs := s
		if s.Weekdays != nil {
			cs.Weekdays = append([]time.Weekday(nil), s.Weekdays...)
		}
		out[i] = cs
	}
	return out
}

// cloneOverrides 深拷贝日期覆盖切片（含每个覆盖项内层 Segments），nil 保持为 nil。
func cloneOverrides(in []billing.DateOverride) []billing.DateOverride {
	if in == nil {
		return nil
	}
	out := make([]billing.DateOverride, len(in))
	for i, o := range in {
		co := o
		co.Segments = cloneSegments(o.Segments)
		out[i] = co
	}
	return out
}

// GetByID 按主键查询；不存在返回 sql.ErrNoRows。
func (s *Store) GetByID(ctx context.Context, id int64) (*ExternalModel, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM external_models WHERE id = $1 AND deleted_at IS NULL`, id)
	return scanExternal(row)
}

// GetByName 按对外名称查询（忽略 enabled），供唯一性检查与更新使用；不存在返回 sql.ErrNoRows。
func (s *Store) GetByName(ctx context.Context, name string) (*ExternalModel, error) {
	row := s.db.QueryRowContext(ctx, `SELECT `+cols+` FROM external_models WHERE external_name = $1 AND deleted_at IS NULL`, name)
	return scanExternal(row)
}

// NameOrAliasExists 判断某名称是否已被任一对外模型的 external_name 或 aliases 占用（忽略 enabled）。
// 供跨表重名校验使用：Auto 名不得与任何对外模型名或别名重名（扁平命名空间，双向校验）。
func (s *Store) NameOrAliasExists(ctx context.Context, name string) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM external_models
		 WHERE deleted_at IS NULL AND (external_name = $1 OR aliases @> to_jsonb(ARRAY[$1::text]))
		 LIMIT 1`, name).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// AliasExistsForOther 判断某别名是否已被「其他」对外模型（id != excludeID）作为别名占用（忽略 enabled）。
// 供别名命名空间的别名↔别名互斥校验：扁平命名空间下别名不得与他人别名重复。
// excludeID 为当前模型主键（创建传 0，更新传自身 ID），用于排除对自身已存别名的误判。
func (s *Store) AliasExistsForOther(ctx context.Context, alias string, excludeID int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM external_models
		 WHERE deleted_at IS NULL AND id <> $2 AND aliases @> to_jsonb(ARRAY[$1::text])
		 LIMIT 1`, alias, excludeID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// GetByExternalName 返回指定对外名称的启用模型（供路由/计费使用）。
// 带内存缓存（见 Store.extNameCache 注释）：命中直接返回，零 DB 查询；未命中回源并写缓存
// （含「无启用模型」负缓存）。无启用模型返回 sql.ErrNoRows。
// 返回值始终为缓存快照的**防御性拷贝**，调用方就地修改不会污染缓存（P2-7）。
func (s *Store) GetByExternalName(ctx context.Context, externalName string) (*ExternalModel, error) {
	if !s.readCacheOff {
		if cached, ok := s.extNameCache.Get(externalName); ok {
			if cached == nil {
				return nil, sql.ErrNoRows
			}
			return cloneExternal(cached), nil
		}
	}
	// 回源前捕获代际：回源期间若发生写路径失效（bump 代际），本次回填必须被拒绝（见 cacheExternalName）。
	gen := s.extNameCache.Generation()
	// 名称解析：命中本模型 external_name 或其任一别名（别名 JSONB 包含判定），仍限定启用且未删除。
	// 缓存键沿用入参名字（别名与其规范名为**不同键**、指向同一模型条目），负缓存语义不变。
	// 确定性排序：多行匹配（历史脏数据可能出现别名/名称重叠）时优先精确 external_name 命中，
	// 其后再按 id 升序，保证解析结果稳定不随物理行序漂移。
	row := s.db.QueryRowContext(ctx,
		`SELECT `+cols+` FROM external_models WHERE (external_name = $1 OR aliases @> to_jsonb(ARRAY[$1::text])) AND enabled = true AND deleted_at IS NULL ORDER BY (external_name = $1) DESC, id ASC LIMIT 1`, externalName)
	m, err := scanExternal(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			if !s.readCacheOff {
				s.cacheExternalName(externalName, nil, gen)
			}
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	if !s.readCacheOff {
		s.cacheExternalName(externalName, m, gen)
	}
	return cloneExternal(m), nil
}

// List 按 enabled 过滤查询，未指定时返回全部。
func (s *Store) List(ctx context.Context, enabled *bool) ([]ExternalModel, error) {
	query := `SELECT ` + cols + ` FROM external_models WHERE deleted_at IS NULL`
	args := []any{}
	if enabled != nil {
		query += ` AND enabled = $` + strconv.Itoa(len(args)+1)
		args = append(args, *enabled)
	}
	query += ` ORDER BY id ASC`
	return s.list(ctx, query, args...)
}

func (s *Store) list(ctx context.Context, query string, args ...any) ([]ExternalModel, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list external models: %w", err)
	}
	defer rows.Close()
	models := make([]ExternalModel, 0, 8)
	for rows.Next() {
		m, err := scanExternal(rows)
		if err != nil {
			return nil, fmt.Errorf("scan external model: %w", err)
		}
		models = append(models, *m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return models, nil
}

// ErrNameExists 表示对外名称唯一约束冲突（PG 23505）。
var ErrNameExists = errors.New("external model name already exists")

// Insert 插入对外模型并返回回填主键后的完整记录；external_name 冲突返回 ErrNameExists。
func (s *Store) Insert(ctx context.Context, m *ExternalModel) (*ExternalModel, error) {
	if m.ID == 0 {
		m.ID = idgen.New()
	}
	row := s.db.QueryRowContext(ctx,
		`INSERT INTO external_models(id, external_name, description, enabled, sale_rates, time_config, context_tiers, aliases)
		 VALUES($1,$2,$3,$4,$5,$6,$7,$8) RETURNING `+cols,
		m.ID, m.ExternalName, m.Description, m.Enabled, jsonB(m.SaleRates),
		nullableJSON(m.TimeConfig), nullableJSON(m.ContextTiers), jsonAliases(m.Aliases))
	created, err := scanExternal(row)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, ErrNameExists
		}
		return nil, err
	}
	// 新增启用模型：失效该对外名的（可能是负）缓存，保证随后读取立即看到新值。
	s.invalidateExternalName(m.ExternalName)
	// 别名与其规范名为不同缓存键：新增后旧别名可能仍滞留负缓存，需逐个失效。
	for _, a := range m.Aliases {
		s.invalidateExternalName(a)
	}
	return created, nil
}

// Update 全量更新对外模型可变字段（不含 id/created_at），并刷新 updated_at。
func (s *Store) Update(ctx context.Context, m *ExternalModel) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE external_models SET external_name=$1, description=$2, enabled=$3,
		        sale_rates=$4, time_config=$5, context_tiers=$6, aliases=$7, updated_at=now()
		 WHERE id=$8`,
		m.ExternalName, m.Description, m.Enabled, jsonB(m.SaleRates),
		nullableJSON(m.TimeConfig), nullableJSON(m.ContextTiers), jsonAliases(m.Aliases), m.ID)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return ErrNameExists
		}
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	// external_name/enabled 可能变化：无旧名信息，清空对外名缓存（写路径显式失效，无 TTL）。
	s.invalidateAllExternalNames()
	return nil
}

// ApplySaleRates 仅更新售价与 updated_at（一键应用 models.dev 参考价），不动其它字段。
// 缓存失效由调用方负责（service.ApplyPrice 按名精确失效、SyncPrices 循环结束后整体失效一次），
// 以免批量同步时每次调用都整体清空缓存造成写放大（此处不再自行 invalidateAll）。
func (s *Store) ApplySaleRates(ctx context.Context, id int64, rates billing.Rates) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE external_models SET sale_rates=$1, updated_at=now() WHERE id=$2`, jsonB(rates), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// BatchDelete 批量软删除外部模型，返回删除行数。
func (s *Store) BatchDelete(ctx context.Context, ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		parts = append(parts, strconv.FormatInt(id, 10))
	}
	anyArg := "{" + strings.Join(parts, ",") + "}"
	res, err := s.db.ExecContext(ctx,
		`UPDATE external_models SET deleted_at = now() WHERE id = ANY($1::bigint[])`, anyArg)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	// 删除后（按名）缓存的启用模型不应再可见：清空对外名缓存（写路径显式失效）。
	s.invalidateAllExternalNames()
	return n, nil
}

// jsonB 将值序列化为 JSON，供 NOT NULL JSONB 列写入；空值写 '{}' 而非 null。
func jsonB(v any) []byte {
	b, _ := json.Marshal(v)
	if b == nil {
		b = []byte("{}")
	}
	return b
}

// jsonAliases 将别名列表序列化为 JSONB 列值；空列表写 '[]'（而非 null），保持 NOT NULL 语义。
func jsonAliases(v []string) []byte {
	if len(v) == 0 {
		return []byte("[]")
	}
	b, err := json.Marshal(v)
	if err != nil || b == nil {
		return []byte("[]")
	}
	return b
}

// nullableJSON 将可空字段序列化：指向 nil 时写 NULL（回落全局），否则写 JSON。
func nullableJSON(v any) any {
	if v == nil {
		return nil
	}
	// 空指针/空切片视为未提供。
	if s, ok := v.(*billing.TimeCoeffConfig); ok && s == nil {
		return nil
	}
	b, err := json.Marshal(v)
	if err != nil || string(b) == "null" {
		return nil
	}
	return json.RawMessage(b)
}

// ErrNotFound 表示对外模型不存在（路由层据此判定「模型未配置或已停用」）。
var ErrNotFound = errors.New("external model not found")
