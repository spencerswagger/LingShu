// Package topology 是上游拓扑（V2 四层图）的本地镜像与同步。
//
// 三个职责，边界写死：
//  1. 同步（T3）：把网关的模型/组清单整批 upsert 进 internal_models，本轮未命中的置 stale
//     （**不删行**：删除判定归网关，本地镜像只标灰）；
//  2. 视图（T1）：把 internal_models + model_links + 对外模型绑定状态聚合成一页；
//     绑定真源只有 external_models.bound_internal_ref 一处，model_links 只存画布坐标；
//  3. 失败留痕：同步失败时**保留上期快照**并把原因写进同步状态（Spec P02 验收 3：
//     同步失败不许清空页面，也不许把「没同步到」当成「网关没有」）。
//
// 明确不做：不在这里改 external_models（绑定是 T5 的事），不写账务，不做删除。
package topology

import (
	"encoding/json"
	"errors"
	"time"
)

// 内部模型的两种节点（与 0019 的 CHECK 同源）。auto 组不入库。
const (
	KindDirect = "direct"
	KindGroup  = "group"
)

// 同步状态机（T1 的 Sync.State）。
const (
	StateOK     = "ok"
	StateFailed = "failed"
	StateNever  = "never"
)

// 同步状态里 State 的存储与展示口径：
//   - 从未成功也从未尝试 → 不落记录，视图回 StateNever；
//   - 尝试过但失败 → StateFailed + Message（原文，页面直接显示）；
//   - 成功 → StateOK + 计数。
const stateConfigKey = "topology.sync_state"

var (
	// ErrSyncInFlight 表示已有一轮同步在跑（手动点 + 定时轮撞车）：不许排队，
	// 否则运营连点会把网关打成一串串行请求。
	ErrSyncInFlight = errors.New("已有同步轮次在执行")

	// ErrSourceNotConfigured 表示未配置网关拓扑源（未配 magpie.base_url 等）。
	// 这不是「同步失败」而是「没得同步」：仍写 failed 状态 + 明确原因，页面据此指路。
	ErrSourceNotConfigured = errors.New("未配置网关拓扑源")

	// ErrLinkTargetMissing 表示画布落位指向了不存在的节点（内部模型或对外模型）。
	// 直接让外键报 23503 会把「填错了 ref」显示成 500，所以先查存在性再写。
	ErrLinkTargetMissing = errors.New("画布落位指向的节点不存在")

	// ErrEmptySnapshot 表示本轮一个节点都没取到。**不能**按「网关现在没有任何模型」处理：
	// 那会把全部存量节点标成 stale（页面集体变灰），而真实原因通常是网关地址配错或网关侧异常。
	ErrEmptySnapshot = errors.New("本轮未取到任何内部模型")
)

// Node 是 internal_models 的一行（网关模型镜像）。
type Node struct {
	Ref         string
	ProviderRef string
	Kind        string
	DisplayName string
	ContextLen  int64
	Caps        json.RawMessage
	ListPrice   json.RawMessage
	PosX        float64
	PosY        float64
	LastSeenAt  time.Time
	Stale       bool
	// CostRates 是灵枢可写的成本价（V3）；同步器不覆盖它。
	CostRates json.RawMessage
}

// Link 是 model_links 的一行（画布落位，不含绑定语义）。
type Link struct {
	InternalRef     string
	ExternalModelID int64
	PosX            float64
	PosY            float64
}

// Provider 是四层图第 1 层（供应商）的一行：由 internal_models 聚合出来，
// Accounts 由 handler 用第 2 层的账号清单回填（不在 SQL 里交叉聚合，避免一条查询算两件事）。
type Provider struct {
	Provider string
	Models   int
	Stale    int
	// Accounts 是「该供应商下出现过账号」的计数，由 Accounts 段回填。
	Accounts int
}

// Account 是四层图第 2 层（上游账号）的一行。
//
// 为什么账号没有镜像表：V2 的账号只存在于账单/账本/订阅里（网关侧账号清单不是稳定契约），
// 所以这一层是**现算**的：三处来源并集，按 (provider, account) 去重。
type Account struct {
	Provider string
	Account  string
	// Subscribed 表示这个账号已录入订阅（= 有固定月费，参与分摊）。
	Subscribed bool
	// LastSeenAt 是最近一次在账单/账本里出现的时刻；订阅来源没有则为零值。
	LastSeenAt time.Time
}

// External 是四层图第 4 层（对外模型）的一行，同时承载「绑定」这条边。
//
// 绑定真源只有 external_models.bound_internal_ref 一处，所以这一层**就是**绑定边：
// 画布据此画「对外模型 ← 内部模型」，不靠 model_links（那张表只存坐标）。
type External struct {
	ID               int64
	ExternalName     string
	Enabled          bool
	BoundInternalRef string // 空串 = 未绑定
}

// Counts 是 T1 顶部状态条的两个计数（各一条 SQL，不合成大查询）。
type Counts struct {
	Stale           int `json:"StaleCount"`
	UnboundExternal int `json:"UnboundExternal"`
}

// SyncState 是一轮同步的结果（落 sys_configs 的 topology.sync_state，跨重启保留）。
//
// 为什么不落表：它只是「最近一次同步」的运行时事实，加一张表要多一条迁移编号，
// 而 0024 已是本批的字典序最后一条（迁移右界 0025）。等有历史查询需求再升表。
type SyncState struct {
	LastRunAt     time.Time `json:"LastRunAt"`
	LastSuccessAt time.Time `json:"LastSuccessAt"`
	State         string    `json:"State"`
	Message       string    `json:"Message"`
	Created       int       `json:"Created"`
	Stale         int       `json:"Stale"`
}

// Result 是一轮同步的汇总（T3 直接返回它，与定时轮共用同一份实现）。
type Result struct {
	Fetched int
	Created int
	Stale   int
	State   string
	Message string
}
