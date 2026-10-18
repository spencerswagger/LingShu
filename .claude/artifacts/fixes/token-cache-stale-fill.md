# Bug: 令牌缓存「回源期间发生失效」后回填陈旧 ACTIVE 快照（认证绕过窗口）

> Status: FIXED
> Mode: default
> Severity: blocker
> Author: agent
> Last updated: 2026-10-18

## Symptom

令牌鉴权读路径（`TokenService.LookupByPlain`）在缓存未命中时「先读 DB、后回填缓存」，
而写路径（禁用/删除用户、撤销/轮换/停用令牌）是「先改 DB、后失效缓存」，两者无版本保护。
若一次读在回源期间与一次写交错，读路径会把**失效前**读到的陈旧 ACTIVE 快照回填进缓存；
在该条目存活期（至多安全 TTL 60s，且更早前无 TTL）内，被禁用/软删用户的令牌仍可通过鉴权。

## Expected

任一失效发生后，回填必须被拒绝；撤销/禁用/删除对该令牌「即时生效」（设计契约），
不得出现陈旧 ACTIVE 条目回填导致的认证绕过窗口。

## Reproduction

- 命令：`cd backend && go test -run TestTokenCache_StalePutAfterInvalidateRejected ./internal/domain/identity/`
- 测试位置：`backend/internal/domain/identity/token_cache_test.go:156`
- 复现稳定性：3/3 reliably fails（回退修复后）——陈旧回填被接受、`get` 命中。

## Hypotheses & diagnosis

| # | Hypothesis | Verdict | Evidence |
|---|---|---|---|
| H1 | 读路径「回源后回填」与写路径「失效」之间无代际/版本校验 → 失效后仍可回填陈旧快照 | confirmed (root cause) | `LookupByPlain` 直接 `cache.put`；`put` 无任何「期间是否发生失效」判据 |
| H2 | 失效钩子未接线（写路径根本不失效） | eliminated | `TestTokenInvalidate_WritePaths`/`TestUserInvalidate_WritePaths` 已覆盖 8 条写路径失效 |
| H3 | 安全 TTL 会兜底，风险可忽略 | eliminated | TTL 仅「最后防线」；设计明确要求撤销/禁用「即时生效」，非 TTL 延迟 |

## Root cause

`tokenCache` 无失效代际（generation）概念，回填（`put`）无法判断「回源期间是否发生过失效」，
因此写路径的失效可被并发读路径的迟到回填「撤销」，把陈旧 ACTIVE 快照重新种入缓存。

## Fix

- 改动文件：
  - `backend/internal/domain/identity/token_cache.go`：新增 `gen uint64` 失效代际；
    `invalidateByHash`/`invalidateByUserID`/`clearLocked` 递增 `gen`；新增 `generation()` 与
    `putAtGen(hash,t,status,gen)`（仅当 `c.gen == gen` 才写入）；`put` 改为 `generation()` + `putAtGen` 薄封装，
    写入主体抽为 `putLocked`。
  - `backend/internal/domain/identity/token.go`：`LookupByPlain` 在**回源 DB 之前**捕获
    `cacheGen := cache.generation()`，末尾改用 `cache.putAtGen(hash, t, status, cacheGen)`。
- 代码 diff 摘要：
  - `if w.cfg...`（略）
  - `putAtGen`: `if c.gen != gen { return }`（回源期间发生失效 → 丢弃回填）
  - `LookupByPlain`: 回源前 `cacheGen = s.cache.generation()`；回填 `s.cache.putAtGen(..., cacheGen)`

## Verification

- V-1：`go test -run TestTokenCache_StalePutAfterInvalidateRejected ./internal/domain/identity/` → GREEN ✓
- V-2：临时移除 `putAtGen` 代际守卫 → 重新 RED ✓（证明测试真捕获 bug；已恢复）
- V-3：`go build ./... && go vet ./... && go test -count=1 ./...` → all GREEN ✓（含既有 8 条失效路径用例）
- V-4：`go test -race -count=1 ./internal/domain/identity/` → 零告警 ✓
- （无 instrumentation，7d N/A）

## Regression test

- 路径：`backend/internal/domain/identity/token_cache_test.go:156` `TestTokenCache_StalePutAfterInvalidateRejected`
- 断言：失效后携带陈旧代际的回填被拒绝（`get` 未命中）；无失效时携带当前代际的回填正常命中（不误杀）。

## Pattern analysis

| 搜索方式 | 命中数 | 是否本次同类隐患 |
|---|---|---|
| `grep -n "cacheExternalName(\|cacheResolvedMembers(" backend` | 6 | **是** —— 同形「读回填 / 写失效无版本保护」存在于 `model.Store`（对外名缓存，无 TTL）与 `automodel.Service`（成员解析缓存，无 TTL） |

结论：「回源后回填可被并发失效撤销」的形状在本仓库多个无 TTL 读缓存中存在。本次只修**鉴权路径**
（令牌/用户状态缓存，风险最高：认证绕过）；其余为功能性缓存（读到陈旧价格/解析结果，非越权），
按 surgical 原则**不在本次修复夹带**，列入 Follow-ups。

## Open questions / Follow-ups

- `model.Store.GetByExternalName` 的对外名缓存（键用户可控、无 TTL）：并发「写失效 + 读回填」可能使陈旧
  售价/启用态滞留至下次同键写入。建议后续以同一 generation 模式加固。
- `automodel.Service.ResolveMembers` 的成员解析缓存（无 TTL）：同上。
- 建议在装配层或缓存抽象层统一「代际」机制，避免逐缓存重复实现（需 dev-plan 评估，不在本次范围）。