# Bug: asyncwrite 配置了 WALPath 却打开失败时静默降级为内存模式（违背「绝不静默丢弃」）

> Status: FIXED
> Mode: default
> Severity: blocker
> Author: agent
> Last updated: 2026-10-18

## Symptom

`asyncwrite` 配置了 `WALPath` 时，若 WAL 打不开（目录不可写 / 磁盘满 / 父路径是普通文件），
进程仅打一条启动日志便继续运行，之后四类「持久化」写（会话投影 / 渠道状态事件 / 调用日志）
实际上处于纯内存模式：背压溢出或崩溃即静默丢失，无任何记录可重放。

## Expected

配置了 WAL 即代表该写要求持久化（DurableSpool，丢失窗口 < 50ms）。WAL 不可用时必须显式暴露：
构造期可被装配层感知并致命化启动；运行期拒绝新的异步提交，绝不静默接受后丢失。

## Reproduction

- 命令：`cd backend && go test -run TestWALUnavailableRefusesAsyncSubmit ./internal/pkg/asyncwrite/`
- 测试位置：`backend/internal/pkg/asyncwrite/asyncwrite_test.go:708`
- 复现稳定性：3/3 reliably fails（回退修复后）——`Submit` 返回 nil、任务被静默接受。

## Hypotheses & diagnosis

| # | Hypothesis | Verdict | Evidence |
|---|---|---|---|
| H1 | `New` 打开 WAL 失败后 `walFile=nil`，`appendWAL` 对 nil 直接返回 `(0,nil)`，使「配置了 WAL」与「显式内存模式」不可区分 → 静默降级 | confirmed (root cause) | `asyncwrite.go` New 的 `OpenFile` 失败分支仅 log；`appendWAL` 对 `walFile==nil` 无差别返回 nil |
| H2 | `commit` 运行期重开 WAL 失败也置 `walFile=nil`，同样静默丢失 | confirmed（同一 root cause 的另一入口） | `commit()` 重开失败 `w.walFile = nil` 后无任何拒绝 |
| H3 | 调用方（main）已在别处校验 WAL 可用性 | eliminated | `main.buildApp` 构造后未检查任何 WAL 错误 |

## Root cause

`Writer` 不区分「未配置 WAL（显式内存模式，合法）」与「配置了 WAL 但不可用（异常）」：
两种情况都表现为 `walFile == nil`，而 `appendWAL` 对 nil 一律返回成功。于是异常被静默降级为内存模式。

## Fix

- 改动文件：
  - `backend/internal/pkg/asyncwrite/asyncwrite.go`：
    - 新增 `walErr` 字段；`New` 在目录创建/打开失败时记录 `walErr`；新增 `Err() error` 暴露之。
    - 新增 `ErrWALUnavailable`；`appendWAL` 在 `walFile==nil && cfg.WALPath != ""` 时返回该错误（覆盖运行期重开失败）。
  - `backend/cmd/server/main.go`：`buildApp` 构造 `asyncWriter` 后若 `Err() != nil` 直接返回错误 → 致命化启动。
- 代码 diff 摘要：
  - `New`：`if err != nil { w.walErr = fmt.Errorf(...) }`
  - `appendWAL`：`if w.walFile == nil { if w.cfg.WALPath != "" { return 0, ErrWALUnavailable } ; return 0, nil }`
  - `main.buildApp`：`if err := asyncWriter.Err(); err != nil { return nil, fmt.Errorf(...) }`

## Verification

- V-1：`go test -run TestWALUnavailableRefusesAsyncSubmit ./internal/pkg/asyncwrite/` → GREEN ✓
- V-2：临时回退 `appendWAL` 守卫 → 重新 RED ✓（证明测试真捕获 bug；已恢复）
- V-3：`go build ./... && go vet ./... && go test -count=1 ./...` → all GREEN ✓
- V-4：`go test -race -count=1 ./internal/pkg/asyncwrite/ ./internal/domain/identity/ ./cmd/server/` → 零告警 ✓
- （无 instrumentation，7d N/A）

## Regression test

- 路径：`backend/internal/pkg/asyncwrite/asyncwrite_test.go:708` `TestWALUnavailableRefusesAsyncSubmit`
- 断言：`Err() != nil` 且 `Submit` 返回非 nil 错误，且 `Shutdown` 不 panic。

## Pattern analysis

| 搜索方式 | 命中数 | 是否本次同类隐患 |
|---|---|---|
| `grep -n "OpenFile(\|os.Open(" backend` | 7 | 否 —— `billing/retry.go:57` 为对照项：打开失败即 `return error`（fail-loud），非同类；`asyncwrite.go` 的 tmp/重开路径已由本次同一守卫覆盖 |

结论：同类「打开失败静默降级」仅 asyncwrite 一处，已修复；billing 重试队列已是正确模式，无需改动。

## Open questions / Follow-ups

- `commit()` 内 tmp 文件创建/写入失败仅 log 并返回（WAL 保持不变、记录仍 pending），无数据丢失，暂不改动。