---
design_type: feature
created_at: 2026-10-18
---

# 网关热路径 IO 削减：统一异步写层 + 读缓存 + 资金写收口

> Status: DRAFT (rev3 — 已通过两轮 document-review：首轮 REVISE 14 项已修复，次轮 PASS + 6 项 non-blocking concern 已收敛)
> Branch: trae/agent-52ia7Q (后续可另开分支)
> Author: agent
> Last updated: 2026-10-18

## Intent Contract

```
intent: 削减网关请求热路径上的同步 DB 往返：把「可缓存的配置类读」降为零次 DB，把「非资金且不必即时落库的写」移出热路径（统一异步持久化层），把资金类写收口为最少的同步事务；网络/上游行为与计费口径不变。
constraints:
  - 部署假设：单实例，不做多实例一致性（不引入 Redis 广播）。
  - 资金语义不可变：余额校验/预扣、结算、账单必须同步且保持既有错误码语义（钱包缺失 40401 / 余额不足 40201 可区分）与零余额拦截（含预扣为 0 的情形）。
  - 不改变计费公式与系数、路由选择结果、响应语义。
  - 缓存与异步必须显式：每个写操作的分类、每个缓存的失效点、异步丢失窗口都要写死在代码注释；不得隐性默认。
  - 令牌/用户状态缓存安全敏感：失效点覆盖全部写路径，缓存项带过期时间于读取时校验。
  - 大量复用既有实现：billing.RetryQueue（本地 WAL）、billing.SetTxBeginner（账单+扣款同事务，已存在）、channel.Manager 内存快照、billing 配置缓存 + 写钩子、pkg/session 用户状态缓存。
  - 读侧提供「直查 fallback 开关」，默认安全态（可强制退回直查）；异步层提供「同步写开关」，可退回原路径。
  - go build / go vet / go test ./... 必须绿；并发相关 go test -race 零告警。
success_criteria:
  - 可缓存的配置类读命中后为 0 次 DB，明确清单：令牌 hash、用户状态、标签、Auto 成员、对外模型名（GetByExternalName）、
    定价模式、billing.r、billing.context_tiers、渠道内部模型（GetByChannelAndInternal）、上游响应头白名单（sys_configs 键）。
  - 保留的同步资金操作仅两处（各为一个事务，且仅限「成功热路径」）：① 预扣（条件 UPDATE + 必要时区分 40401 的 SELECT + 流水 INSERT）；②「账单 + 积分结算」（复用既有 SetTxBeginner，含其幂等 SELECT）。失败路径（退款 Chargeback、网关拒绝落账）为额外事务，不计入该口径。
  - 非资金写经统一 asyncwrite 提交且：同 Key 保序、幂等可重放（含事件类）、崩溃丢失窗口 < 50ms（默认 fsync 间隔 10ms 或每 100 条，先到为准，可配置）、优雅退出 flush。
  - 状态列语义：密钥/模型「状态列」落库保持同步（仅事件异步），保证重启后不复活已禁用实体。
  - 令牌/用户缓存：撤销/禁用/删除经写路径钩子即时生效；自然过期精确（读时比对 ExpiresAt）；安全 TTL 仅作最后防线，不计入即时性验收。
risk_level: high   # 触及鉴权缓存（失效漏接=认证绕过）与资金事务语义
```

## Verification Contract

```
verify_steps:
  - run tests: cd backend && go test ./...
  - race: cd backend && go test -race ./internal/...   # 必须覆盖 asyncwrite 包与失效钩子装配（并发核心），不得只跑 domain
  - build: cd backend && go build ./... && go vet ./...
  - check:
      1) 缓存失效矩阵（表驱动）：对每一条写路径各一例，断言对应缓存项即刻失效；并对「未变更」请求断言零 DB（注入 fake store 计数）。
      2) 令牌/用户：撤销、禁用、软删、过期（到点）四类各一例，断言下一次请求被拒。
      3) asyncwrite：同 Key 保序、事件幂等重放（重复提交只落一行）、崩溃 replay（重启后不丢不重）、Shutdown flush。
      4) 资金语义：钱包缺失 / 余额不足 / 零余额且预扣为 0 三类用例，断言错误码与拦截行为与合并前一致。
      5) 路由与响应一致性：同一输入在缓存开关 on/off 下路由结果一致；错误码与响应体一致。
  - confirm:
      1) 配置变更后鉴权/路由即时生效；未变更请求的配置类 DB 读为 0。
      2) 令牌自然过期在到点后被拒（无 TTL 延迟）。
      3) 压测下仅剩两处同步资金往返，-race 零告警。
      4) kill -TERM 优雅退出时 spool 全部 flush；重启 replay 无丢写、事件不重复。
      5) 缓存/异步开关切到安全态（直查/同步写）后行为与改造前一致。
```

## Governance Contract

```
approval_gates:
  - 令牌/用户状态缓存落地前，人工评审「失效点清单完整性」（auth 路径，risk_level high）。
  - 资金写收口（去掉独立余额读取、预扣条件写）落地前，人工评审错误码语义与对账口径不变。
  - asyncwrite 接入服务生命周期（启动 replay / 退出 flush / 背压策略 / 状态列同步边界）需评审。
  - 交付顺序约束：失效钩子接线必须先于缓存启用（否则存在认证绕过窗口）。
rollback: 逐批独立提交可单独回滚；缓存保留直查 fallback（默认安全态），异常一键退回直查；异步层保留同步写开关；状态列同步写不随异步化改动。
ownership: agent 实现，用户验收。
```

## Scope

| In scope | Out of scope |
|---|---|
| 统一异步写层 asyncwrite（有界队列 + DurableSpool 批量 fsync + 分片保序 + 幂等 + replay + flush + 观测），分类可开关 | 多实例一致性 / Redis 失效广播（单实例假设） |
| 非资金写迁移：调用日志、令牌 LastUsed、会话落库（Attach/Renew/SetClosed/Delete 同分片）、渠道密钥/模型「事件」 | 密钥/模型「状态列」异步化（保持同步，见 D5） |
| 资金写收口：去掉独立余额读取，预扣条件写作为唯一资金写；账单+结算复用既有同事务（非新交付） | 计费口径、路由算法、协议逻辑的语义变更 |
| 读缓存：令牌+用户状态、标签、对外模型名、Auto 成员、定价模式、billing.r、billing.context_tiers、渠道内部模型查询、sys_configs 剩余键、上游响应头白名单 | 事件/日志/会话表结构变更（除事件去重键外） |
| 失效接线：identity 令牌/用户写路径、tag CRUD、model CRUD、automodel CRUD、sys_configs 写钩子、渠道内部模型 CRUD | 前端展示变更 |
| 既有 pkg/session 用户状态缓存与新网关缓存的一致性收敛（同一写点接两处） | 探测链路与控制台聚合端点的独立优化 |

## Decisions

| # | decision | choice | rejected alternatives |
|---|---|---|---|
| D1 | 异步写层形态 | 最小形态起步：有界内存队列 + 复用 RetryQueue 的本地 WAL 落盘 + 分片 worker + 批量 fsync（group commit）；背压默认「短暂阻塞（绝不静默丢弃审计/会话）+ 饱和告警」，drop 仅在显式配置下启用 | 一次性引入完整机制（分片+背压+多级队列）从第一天就上；各处各自 go func；每条 fsync |
| D2 | 分类与丢失窗口 | 默认 DurableSpool（丢失窗口 < 50ms，默认 fsync 间隔 10ms 或每 100 条）；仅「令牌 LastUsed」可 MemoryOnly | 全部 MemoryOnly（丢审计）；全部同步（无收益） |
| D3 | 会话/事件/日志分类 | 统一 DurableSpool；会话的 Upsert 与 SetClosed/Delete 必须落同一分片键，保证同一会话行不乱序 | 仅会话 MemoryOnly；Close/Kill 保持同步而 Upsert 异步（会乱序，除非同分片） |
| D4 | 资金写收口 | 账单+结算同事务「复用既有 SetTxBeginner/settleTx」，非新交付；新工作 = 去掉独立余额读取，改为预扣原子条件写；该条件写必须同时满足 `balance >= amount` 且 `balance > 0`（amount=0 也下发），以保留 40401/40201 区分与「零余额（含预扣为 0、费率缺失）」拦截 | 保持两次余额往返；把资金异步化 |
| D5 | 状态列同步边界 | 密钥/模型「状态列」落库保持同步；仅「事件」异步 | 状态列也异步（重启会用陈旧状态覆盖恢复） |
| D6 | 事件幂等键 | 事件 job 预生成事件 ID，插入用 ON CONFLICT(id) DO NOTHING；现有表无可用唯一键则新增去重键（迁移） | 依赖行无唯一键、接受重复插入 |
| D7 | 令牌/用户缓存失效 | 两级键：plain→entry、userID→entries；写路径钩子同步失效 + 缓存项带 ExpiresAt 读时校验 + 安全 TTL 兜底（仅最后防线） | 纯短 TTL（撤销不即时/可能绕过）；仅 tokenID 建索引（BatchDelete/Rotate/Toggle 只有 ID，无法失效 plain 键） |
| D8 | 读缓存载体 | 复用/扩展既有模式（channel.Manager 快照、billing 配置缓存 + 写钩子），按实测热点收敛缓存键 | 引入独立缓存中间件；一次性把全部读都上缓存 |
| D9 | 交付顺序 | 失效钩子接线先于缓存启用；fallback 开关默认安全态（直查）；异步层默认同步写、逐类开启 | 先启缓存后接钩子 |

## Surface

**新增组件（asyncwrite 包）**

- API：`Submit(job)`（非阻塞）、`Start(ctx)`、`Shutdown(ctx)`（flush）、`Stats()`；`Kind` 为显式枚举（会话/密钥事件/模型事件/调用日志/LastUsed）。
- 队列：有界内存队列 + 本地 WAL（复用 RetryQueue 的追加与原子改名思路，改为常开 fd + 批量 fsync）。
- worker：按 `Key` 分片保证同 Key FIFO；批量 drain + 指数退避；启动 replay；关闭 flush。
- 分类开关：每个 `Kind` 可独立切「异步 / 同步」，默认安全态。

**后端接入点（shape 级，不含行号）**

- gateway：去掉独立余额读取，预扣条件写作为唯一资金写（条件含 `balance > 0`，amount=0 也下发，保住零余额/费率缺失拦截与 40401/40201 区分）；结算+账单沿用既有同事务；调用日志写入改提交 asyncwrite。
- 会话注册表：内存计数与并发闸门保持同步；落库（Upsert/SetClosed/Delete）改提交 asyncwrite 并落同一会话分片键。
- 渠道管理器：密钥/模型「状态列」保持同步写；「事件」改提交 asyncwrite（带预生成事件 ID 做幂等）。
- identity：令牌/用户状态缓存（两级键）+ ExpiresAt 读时校验；在令牌创建/删除/轮换/启停与用户更新/删除/创建/重置等写路径挂失效；与既有 pkg/session 用户状态缓存接同一写点。
- 标签 / 对外模型 / Auto / 渠道内部模型 / sys_configs / 上游响应头白名单：读缓存 + 各自写路径失效钩子（sys_configs 复用既有写钩子机制）。
- 装配层：构建 asyncwrite 并注入各存储；启动 replay、退出 flush；接线全部缓存失效钩子，且先于缓存启用。

**前端**：无改动。

## Risks & Open Questions

| Risk | Mitigation |
|---|---|
| 令牌/用户缓存失效点漏接 → 认证绕过 | 全量枚举写路径 + 每条路径单测断言失效；auth 路径设人工评审门；D9 顺序保证（钩子先于缓存） |
| 两级键缺失导致 BatchDelete/Rotate/Toggle 无法失效 plain 项 | 键结构显式设计（plain→entry、userID→entries）；写路径能反查 hash |
| 与既有 pkg/session 用户状态缓存双缓存漂移 | 两处缓存接同一服务写点；单测断言同时失效 |
| 事件非幂等导致重放重复 | D6：预生成事件 ID + ON CONFLICT DO NOTHING（必要时加去重键迁移） |
| 状态列异步 → 重启复活已禁用实体 | D5：状态列保持同步，仅事件异步 |
| 会话 Upsert 异步与同步 Close/Kill 乱序 | D3：同一会话分片键；或 Close/Kill 一并异步 |
| 去掉独立余额读取改变错误语义 | D4：保留 40401/40201 区分与零余额（含预扣为 0）拦截；表驱动用例覆盖三类 |
| 资金收口改变对账口径 | 公式/系数/幂等键不变；对账回归 + 人工评审门 |
| 异步层新增磁盘 IO 与运维面 | WAL 路径可配置；暴露队列深度/积压/重试观测 |
| Open question：fsync 间隔与丢失窗口 | 默认 10ms 或每 100 条、窗口上界 50ms，做成配置项；压测后可调 |
| Open question：会话在 fsync 成本显著时是否降级 MemoryOnly | 默认 DurableSpool；仅当压测显示 fsync 成本显著时评估降级，且触发降级需新开人工评审门，不作为初始选择 |
| Open question：缓存键收敛范围 | 先按实测热点启用（令牌/用户、对外模型、定价模式、billing.r/tiers 优先），其余按收益排期 |
```
