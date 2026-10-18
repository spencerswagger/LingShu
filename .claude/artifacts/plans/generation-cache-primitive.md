# 通用 generationCache 原语（读缓存失效收敛）Implementation Plan

> Status: APPROVED
> Source: docs/designs/2026-10-18-gateway-hotpath-io-reduction-design.md（D8/D9、缓存失效 success_criteria）+ `.claude/artifacts/fixes/token-cache-stale-fill.md` Follow-ups
> Mode: --deliberate（自动升级：涉及**鉴权路径**缓存重构）
> Iterations: 1 / 3
> Author: user
> Last updated: 2026-10-18

## Requirements summary

统一「读回填 / 写失效无版本保护」这一类缺陷的修复载体：抽出一个带**失效代际**的通用有界读缓存原语，把 identity 令牌缓存、model 对外名缓存、automodel 成员解析缓存三处收敛到同一实现。identity 已在 P1 修复中手工加了 generation，本 plan 的目标是**消除三处重复实现**，并顺带修复 model / automodel 两处尚未处理的同类竞态（陈旧回填可滞留，且这两处**无 TTL**）。

## Acceptance criteria

- AC-1 新增通用原语 `pkg/gencache`：有界（FIFO 淘汰）+ 失效代际（generation）+ `putAtGen` 陈旧回填拒绝 + `invalidate(key)` / 可选 `invalidateGroup(group)` / `invalidateAll`；`get` 以 `(V, bool)` 区分「存在但零值」与「不存在」。
- AC-2 identity 令牌缓存改用原语，**对外行为不变**：现有 identity 全部测试（TTL、精确过期边界、令牌/用户失效矩阵、读-失效竞态拒绝）通过。
- AC-3 model 对外名缓存改用原语，新增「读回源期间发生失效 → 回填被拒绝」回归测试。
- AC-4 automodel 成员解析缓存改用原语，新增同类回归测试。
- AC-5 三处「读缓存开关」语义不变（identity `SetTokenCacheEnabled`、model/automodel `SetReadCacheEnabled`；关闭即直查、清空残留）。
- AC-6 `go build ./... && go vet ./... && go test ./...` 全绿；`go test -race ./internal/...` 零告警。

## RALPLAN-DR

### Principles

1. 最小代码：只抽真正致事的机制（存储 + FIFO + 代际 + 失效），value 语义一律外置。
2. 外科手术式：不改缓存对外可观测行为（开关/TTL/负缓存/失效点全部等价）。
3. 鉴权安全优先：identity 现有测试是重构的硬门禁，不得削弱。
4. 可验证：每处缓存补一条「读-失效竞态」回归测试；原语有独立单测。

### Decision drivers

- 消除三处重复的代际/失效实现（维护成本）。
- 鉴权路径回归风险必须可控（identity 已有 P1 修复 + 测试）。
- 不引入过度抽象（team 熟悉度 / 最小代码）。

### Viable options

**Option A（推荐）· 只抽「存储 + FIFO + 代际 + 失效」最小内核**
- 思路：`gencache.Cache[V]` 只管 bounded map + FIFO order + `gen` + `get/putAtGen/invalidate/invalidateGroup/invalidateAll/generation/count`。TTL、ExpiresAt 精确过期、防御性深拷贝、负缓存（nil）语义**全部留在各缓存 wrapper**。
- 改动文件：新增 `backend/internal/pkg/gencache/gencache.go`(+test)；`identity/token_cache.go`；`model/store.go`；`automodel/service.go`。
- Pros：覆盖面恰好等于致事机制；model/automodel 不被无用参数拖累；identity 的 TTL/clone 逻辑原地不动、回归面最小。
- Cons：identity 的 `byUser` 二级索引、TTL、clone 仍留在 wrapper（收敛不彻底）。

**Option B · 全功能原语（含 TTL / clone hook / group 索引 / 过期断言）**
- 思路：原语内建 TTL、每值 clone 钩子、可选二级 group 索引，三缓存变薄配置。
- Pros：最大去重。
- Cons：model/automodel 被迫携带用不到的 TTL/group/expiry 参数（违反最小代码）；把鉴权相关 TTL 与配置缓存混进同一抽象，泄漏面变大。判定：**不采纳**。

**Option C · 不抽抽象，给 model/automodel 各抄一份 `gen`**
- Pros：无新包、改动局部。
- Cons：四处重复同一竞态实现，后续再出新缓存仍会漏（正是本类 bug 的成因）。判定：**不采纳**（用户已在 A/B 二选一中选 B=抽原语）。

## Implementation steps

1. **新增原语包** — `backend/internal/pkg/gencache/gencache.go`：`type Cache[V any]` + `New[V](max int) *Cache[V]`；字段 `mu sync.RWMutex`、`m map[string]V`、`order []string`、`gen uint64`、`max int`。方法：`get(k) (V,bool)`、`generation() uint64`、`putAtGen(k string, v V, gen uint64) bool`、`invalidate(k)`、`invalidateGroup(g)`（基于可选的 group 索引）、`invalidateAll()`、`count()`。FIFO 淘汰与 `invalidate` 时同步摘除 order 条目（沿用 `model/store.go:removeExternalNameFromOrderLocked` 的严格一致思路）。全中文注释写明契约。
2. **原语单测** — `gencache_test.go`：有界 FIFO 淘汰；`putAtGen` 陈旧代际拒绝 + 新鲜代际接受；`invalidate` 后回填被拒；`invalidateGroup`/`invalidateAll` 清空；`(V,bool)` 对零值/nil 的区分；并发 `-race`。
3. **converge identity** — `identity/token_cache.go`：以 `gencache.Cache[*tokenCacheEntry]` 承载 map/order/gen/淘汰与 `invalidate/invalidateGroup(userID)/invalidateAll`；`get` 保留 TTL + ExpiresAt 判定 + 深拷贝；`put/putAtGen` 保留 value 深拷贝后写入。`token.go:LookupByPlain` 保持「回源前取 generation、回填用 putAtGen」。
4. **converge model** — `model/store.go`：`extNameCache`/`extNameOrder` 替换为 `*gencache.Cache[*ExternalModel]`；`GetByExternalName` 回源前取 `generation()`，负缓存存 nil，`cacheExternalName`→`putAtGen`；`invalidateExternalName`/`invalidateAllExternalNames` 走原语；`evictExternalNamesLocked`/`removeExternalNameFromOrderLocked` 删除（由原语承担）。
5. **converge automodel** — `automodel/service.go`：`membersCache` 替换为 `*gencache.Cache[resolveCacheEntry]`；`ResolveMembers` 回源前取 generation、`cacheResolvedMembers`→`putAtGen`；`invalidateMembersCache`/`evictMembersCacheLocked` 收敛到原语。
6. **补回归测试** — `model/service_test.go` 与 `automodel/service_test.go` 各加一条「读回源期间发生失效 → 回填被拒（读到新快照）」测试，方法与 identity `token_cache_test.go` 的竞态用例同构。
7. **接线核对** — 确认 `SetReadCacheEnabled` / `SetTokenCacheEnabled` / `readCacheOff` 开关与失效钩子（main.go 装配）无需改动；若无改动则在 plan 中记「无接线变更」。

## Workspace setup

- 实施前运行 `git status --short` 与 `git branch --show-current`。
- 当前分支 `trae/agent-52ia7Q`（任务分支，非 main/master/release），working tree 干净 → **在现有分支实施，无需新建 worktree**（PR #43 仍开放）。
- 若实施前 tree 变为 dirty，先保护现有改动，不混入本次。

## Risks & mitigations

| Risk | Mitigation |
|---|---|
| 重构令牌缓存丢失 TTL 或 ExpiresAt 精确过期 → 过期令牌被放行 | identity 现有全部测试（`token_cache_test.go`）作为门禁；AC-2 显式点名这两类用例 |
| 原语 `get` 对「零值/nil」与「不存在」区分错误 → 负缓存被当未命中，每次穿透 DB | 原语用 `(V, bool)` 语义；model 负缓存显式存 `*ExternalModel(nil)`；原语单测覆盖 |
| FIFO order 与 map 不一致 → 内存缓漏/错误淘汰 | 原语内保证 `invalidate` 同步摘除 order（沿用 model.store 现有严格一致实现），并有界性测试兜底 |
| 三处缓存开关/失效点语义漂移 | 步骤 7 显式核对开关与 main.go 失效钩子；AC-5 覆盖 |
| 泛型 API 过度设计 | Option A 严控内核面；TTL/clone/负缓存外置 |

## Verification steps

- AC-1：`cd backend && go test ./internal/pkg/gencache/`
- AC-2：`cd backend && go test ./internal/domain/identity/`（含 TTL/精确过期/失效矩阵/竞态）
- AC-3/AC-4：`cd backend && go test ./internal/domain/model/ ./internal/domain/automodel/`
- AC-5：`cd backend && go test ./internal/domain/... ./cmd/server/`
- AC-6：`cd backend && go build ./... && go vet ./... && go test -count=1 ./...` 与 `go test -race -count=1 ./internal/...`

## Pre-mortem (deliberate)

1. **Scenario**：令牌缓存重构后，某次请求用过期令牌通过鉴权。
   **Trigger**：`tokenCache.get` 迁移到原语时遗漏 `ExpiresAt` 判定或 TTL。
   **Mitigation**：TTL 与 ExpiresAt 逻辑**留在 wrapper 不动**（Option A 边界）；identity 现有测试全绿为合入门禁。
2. **Scenario**：对外名负缓存失效，随机模型名每请求穿透 DB，热路径 DB 往返回升。
   **Trigger**：原语把 `*ExternalModel(nil)` 当作「键不存在」。
   **Mitigation**：`(V, bool)` 语义 + 原语单测 + model 缓存命中零 DB 的既有断言。
3. **Scenario**：三缓存并发下 `-race` 报数据竞争。
   **Trigger**：原语 `get` 返回内部引用而非副本（如 model 的 `*ExternalModel`）。
   **Mitigation**：原语只存/取 `V`；防御性拷贝仍由 wrapper 在边界完成（identity cloneToken、model cloneExternal、automodel 切片拷贝）；`-race -count 多次` 验证。

## Expanded test plan (deliberate)

- **Unit**：`pkg/gencache` 原语（有界/代际/失效/group/(V,bool)/并发）。
- **Integration**：三缓存各自的「失效后重新回源读到新快照」+ 新增「读-失效竞态拒绝陈旧回填」；identity 既有 TTL/精确过期/失效矩阵回归。
- **E2E**：不适用（无新增端点；缓存为内部机制）。
- **Observability**：无新增指标；确认异步/缓存 Stats 与日志不受影响。

## ADR

- **Decision**：抽 `pkg/gencache` 最小内核（存储 + FIFO + 代际 + 失效），identity/model/automodel 三处缓存收敛其上；TTL、精确过期、防御性拷贝、负缓存等 value 语义留在各 wrapper。
- **Drivers**：消除三处重复代际实现（主因）> 鉴权回归可控 > 不过度抽象。
- **Alternatives considered**：
  - Option A —— **chosen**。
  - Option B（全功能原语）—— rejected：违反最小代码、把鉴权 TTL 与配置缓存混层。
  - Option C（各抄一份 gen）—— rejected：重复正是本类 bug 成因。
- **Why chosen**：致事的机制是三处共有的（有界存储 + 失效 + 代际回填校验），而差异（TTL/clone/负缓存/group）是 value 语义，抽共性、留差异能同时满足去重与最小改动。
- **Consequences**：新增一个内部包与泛型 API（正向：后续新缓存可直接复用；负向：identity 仍保留部分自有逻辑）。identity 已有 P1 修复与测试，迁移风险受现有测试约束。
- **Follow-ups**（本次不做）：把 `pricingModeCache` / `configValueCache`（`cmd/server/read_caches.go`）等手写单值缓存也评估是否纳入原语（当前非同类竞态，优先级低）。

## Review trail

- Planner draft v1：Option A/B/C 三选，favored A（最小内核），7 步。
- Architect challenge v1：steelman（「抽 30% 共性可能不如各加 gen」）+ tension（去重 vs 抽象泄漏）→ 收敛为「只抽存储+失效+代际，value 语义外置」。
- Critic verdict v1：REVISE —— 指出 Plan 未明确 `byUser` 二级索引归属（原语 group vs wrapper），并缺 model/automodel 竞态回归测试的可测性说明。
- Planner draft v2：原语补 `invalidateGroup`（group 索引归原语，model/automodel 传空 group）；步骤 6 显式补两处竞态回归测试；AC 二值化。
- Architect challenge v2：接受 Option A 边界；确认 `-race` 为并发门禁。
- Critic verdict v2：APPROVED，2 处改进并入（group 归属、竞态测试显式化）。
- Final iterations: 1 / 3

## Critic verdict（最终）

| 维度 | 状态 | 备注 |
|---|---|---|
| Principle consistency | ✓ | A 与「最小代码/外科手术」一致 |
| Alternative exploration | ✓ | A/B/C 真候选，B/C 有 invalidation rationale |
| Risk mitigation clarity | ✓ | 每条 risk 一行 mitigation |
| AC testability | ✓ | 6 条均二值可验证 |
| Verification concreteness | ✓ | 逐 AC 给命令 |
| File coverage | ✓ | 步骤均 cite 具体文件/函数 |
| Pre-mortem present | ✓ | 3 场景 + trigger + mitigation |
| Expanded test plan | ✓ | unit/integration/e2e/observability 齐 |

**Verdict: APPROVED**

### Reservations（必填）

- `invalidateGroup` 仅 identity 使用，model/automodel 将传空 group。我有保留：若空 group 分支使原语复杂化，退化为「group 可选、未启用时零开销」；实现时若发现空 group 拖累可读性，允许把 group 索引退回 identity wrapper（届时 ADR 需记该偏离）。
- 「读-失效竞态」回归测试需能注入时序（在原语单测中直接调 `generation()`+`invalidate`+`putAtGen` 即可，无需 sleep）。若某缓存无法在不加测试钩子的前提下构造该序列，则退回 wrapper 级手工注入，并在实现时说明。

## Open questions

- 原语包名 `gencache` 是否合适（备选 `cache`）——倾向于 `gencache` 以免与「缓存」通用词混淆；实施前可定。
