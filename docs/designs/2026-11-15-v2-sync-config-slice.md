# V2 阶段一 · 同步器 + 运行参数 + 账本写通道（第一切片）

日期：2026-11-15 · 范围：实施计划 §2/§3 里**不依赖表结构**的部分
（V2 从 0019 重新起号，旧的 p1 分支及其 0019/0020 已废弃，本切片不含任何迁移）。

## 1 本切片交付三件

| 件 | 落点 | 一句话 |
|---|---|---|
| 同步器骨架 | `backend/internal/domain/upstream/sync.go` + `backend/internal/pkg/backoff/backoff.go` | 严格串行；失败退避 5s/20s/60s、重试 3 次后放弃并留可诊断错误 |
| 运行参数 | `backend/internal/domain/upstream/config.go` + `seeding` 种子 | 基址 / 超时 / 降级开关进 `sys_configs`；密钥一律不入 |
| 账本写通道 | `backend/internal/domain/upstream/ledger.go` | 只定接口与占位实现；字段名等 Ark 定稿，本切片不命名任何列 |

## 2 重试口径（唯一实现）

`pkg/backoff` 是这几个数字的唯一来源，其它包不得硬编码：

- 首次尝试**立即执行**，不等待；
- 失败后退避 **5s → 20s → 60s**，即**重试 3 次**；加上首次，最多调用 **4 次**；
- 3 次重试用完仍未成功 → 返回 `*backoff.ExhaustedError`（含操作名、实际调用次数、重试次数、
  退避序列、原始错误），同步器再记一条 ERROR 日志（`step / attempts / retries / backoff / err`）。
  **不吞错**：`errors.Is/As` 可穿透到原始错误。
- 等待期间 ctx 取消/超时 → 立即返回 `*backoff.CanceledError`，不再把 60s 睡完。
- 零值 `Policy{}` = **不重试**（只调一次）。刻意不做「零值静默变成重试 4 次」，
  否则「只想试一次」的调用方会多打三次上游；标准节奏请显式用 `backoff.Default()`。

## 3 严格串行（两层保证）

1. **单轮内**按注册顺序逐个 await，同 goroutine，天然不并发；
2. **重复触发**由 `running` 标记直接拒绝（`ErrBusy`，属 `*SkipError`），不排队不并发——
   排队会让积压轮次在网络恢复后一起打上游。

`Start` 用「**跑完再等**」而不是固定 ticker：一轮耗时超过周期时不会堆积下一轮
（ticker 会补发 tick，等于偷偷并发）。

每个步骤**独立重试**，某一步耗尽后记错误但继续跑后续步骤（一个接口故障不该拖死整轮），
本轮所有失败用 `errors.Join` 汇总返回。

## 4 sys_configs 三项参数

| 键 | 含义 | 默认 | 校验 |
|---|---|---|---|
| `upstream.base_url` | 上游网关基址（空 = 未接入） | `""` | 只收 http/https；**拒绝 URL 内带凭据**（`user:pass@`） |
| `upstream.timeout_ms` | 单次调用超时（毫秒） | `30000` | 1s ~ 10min，越界报问题并沿用上期值 |
| `upstream.degrade` | 降级开关（true = 同步停摆、沿用上期快照） | `false` | 布尔；非法值沿用上期值 |

- **密钥红线**：三项全是非敏感运行参数。凭据（admin key / callerKey / token / password）
  只由装配层逐请求注入，不落 `sys_configs`、不进代码常量、不进日志。用例 `TestConfigKeys_CarryNoSecrets`
  对键名做黑名单扫描，`normalizeBaseURL` 直接拒绝带凭据的 URL。
- **失败方向**：读失败/值非法都**不把已有值抹空**，沿用上期成功值（`lastGood`）并返回问题清单
  供装配层记 WARN。基址为空时同步根本不启动（不对着空地址狂重试）。
- 缓存：三项都干净读到时才写缓存；`Invalidate()` 供 sys_configs 写入钩子调用（与 billing
  既有做法一致，无 TTL → 直改表需重启或走应用内写入）。

## 5 账本写通道（占位）

`LedgerWriter` 只定义通道形态，不定义业务字段：

```go
type LedgerWriter interface {
    Append(ctx context.Context, rec LedgerRecord) (LedgerRef, error) // 幂等，返回账本引用
    List(ctx context.Context, offset, limit int) ([]LedgerRecord, error) // offset 分页
}
```

- `LedgerRecord{Values []any}`，长度必须 == `LedgerFieldCount`（**9**，来自已批准开放项 ②）；
  字段顺序与语义以 Ark 定稿的 0022 迁移为准，**本包不写死任何一个列名**——先猜名字，
  等 DDL 落地就要账单侧/对账侧/前端一起改名。
- 占位实现 `UnavailableWriter` **明确返回** `ErrLedgerUnavailable`，不返回零值成功：
  账本是钱的口径，静默成功等于丢账。
- 幂等由实现方按 0022 的唯一键保证（`Append` 同一业务事实只能得到同一条账本），
  归因对账要求 100% 匹配。

## 6 测点（本切片）

| 用例 | 断言 |
|---|---|
| `TestDefaultPolicy_Sequence` / `TestDo_RetrySequenceExactly` | 序列恰为 5s/20s/60s；调用 4 次；错误链可穿透 |
| `TestDo_StopsAfterThirdRetry` | 第 3 次重试后不再调用、不再退避 |
| `TestDo_CancelDuringWait` / `TestDo_ZeroPolicyNoRetry` | 取消即返回；零值策略只试一次 |
| `TestSyncer_SerialSteps` / `TestSyncer_StartNeverOverlaps` | 步骤不并发、不乱序；定时轮次不重叠 |
| `TestSyncer_ConcurrentRunRejected` | 上一轮未结束时立即 `ErrBusy`（带 2s 超时保护，守卫被删会快速变红） |
| `TestSyncer_RetryBackoffAndGiveUp` | 单步耗尽不影响后续步骤；日志/错误点名步骤与次数 |
| `TestSyncer_GateSkipsWithoutCallingSteps` | 降级/未配置时一步不跑、不退避 |
| `TestConfigProvider_*` | 默认值、宽松格式、读失败/非法值沿用上期值、缓存与失效 |
| `TestConfigGate` / `TestConfigGate_NilProvider` | 三态闸门；未装配配置源安全跳过 |
| `TestSeedUpstreamConfigMatchesCodeDefault` | 种子走运行时同一解析后等于代码默认 |
| `TestLedgerRecord_Validate` / `TestUnavailableWriter` | 九字段形状；占位实现明确失败 |

上表每条都做过**变异验证**（改回缺陷实现确认变红），记录见 PR 描述与切片报告。

## 7 还差什么（等 Ark / 后续切片）

1. **0022 账本字段定稿** → 用真实字段替换 `LedgerRecord.Values`，并接上 `billing_records.ledger_entry_id`。
2. **上游接口契约**（models / groups / providers / quotas 的路径与响应形状）→ 才有真实 `Step` 可注册；
   当前骨架未接入 `cmd/server` 装配（无步骤可跑，接进去只是空转）。
3. **sys_configs 写入路径**：需要管理端写端点或装配层的写钩子调 `ConfigProvider.Invalidate()`，
   否则改配置要重启（与 billing 现状一致，不是本切片引入的行为）。
4. 0021 的 `DROP` 三步证据（pg_dump → 临时库还原 → 行数比对）属迁移切片，本切片不涉及。