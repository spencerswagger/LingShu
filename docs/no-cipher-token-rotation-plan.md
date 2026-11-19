# 历史 no-cipher 令牌处置方案（轮换 → 自动注册 callerKey）

状态：**待拍板**（本方案只做分析与步骤设计，未执行任何轮换、未改任何数据）。
日期：2026-11-19（夜班）。代码基线：`origin/main` = `69eccb6`（v0.15.0-rc4，含 PR #164 `1b6d558` 的补建结果分级）。
方案分支：`docs/no-cipher-token-rotation`（PR 见仓库，不合入，等确认后按步骤执行）。

---

## 0. 背景：no-cipher 令牌是什么、为什么会卡住补建

V3 起令牌明文（`sk-gw-…`）同时就是代理侧 callerKey：

- 新令牌创建时（`identity.TokenService.Create`，token.go L447–471）：先 SM4 加密明文落 `tokens.secret_cipher`，
  再把明文注册到代理侧（`EnsureCallerKey`）并写 `magpie_keys` 映射（`ls-token-<id>`）。
- **V2 之前播种的系统探测令牌**（`seeding/seed.go` `seedSystem`，L118–125）走的是另一条 INSERT：
  只写 `token_hash / token_display / user_id / display_name / status`，**不写 `secret_cipher`**，
  且 `token_hash` 是字面量占位 `"system-probe-hash"`（不是真实 `sk-gw-…` 的哈希）。
  它本来就不是给调用方用的密钥，只承担「健康探测开销记账的归属身份」。

补建逻辑（`BackfillCallerKeys`，token.go L479–522）对每一条缺 `magpie_keys` ACTIVE 映射的存量令牌
调 `s.Secret()`（解密 `secret_cipher`）拿明文注册 callerKey；`secret_cipher` 为空时拿不到明文，
该令牌计入 `skipped["no-cipher"]` 并追加到 `failed`（L513–515，PR #164 `0f36cc8` 引入的分级报告）：

> `Reason: "no-cipher（该令牌未备份密钥，需轮换后自动开通）"`

这类令牌无法在服务端恢复明文（SM4 只能解密已有密文；没有密文就无解），
唯一的处置路径就是**轮换**（作废旧令牌 → 换发新令牌，新令牌天然带密文并自动注册 callerKey）。

## 1. 影响面盘点（实测数字）

### 1.1 查询 SQL（只读；已在开发库实际执行）

```sql
-- 受影响令牌清单（no-cipher 存量令牌）
SELECT t.id, t.display_name, t.status, u.username, u.is_system
  FROM tokens t JOIN users u ON u.id = t.user_id
  WHERE t.deleted_at IS NULL AND u.deleted_at IS NULL
    AND COALESCE(t.secret_cipher, '') = ''
  ORDER BY t.id;
```

**执行结果（lingshu_dev 库，2026-11-19）：**

```
        id         | display_name | status | username | is_system
-------------------+--------------+--------+----------+-----------
 11650277558976512 | system-probe | ACTIVE | system   | t
(1 row)
```

### 1.2 盘点结论（真数字）

| 项 | 数值 | 说明 |
|---|---|---|
| 受影响令牌 N | **1** | `system-probe`（id `11650277558976512`），属系统用户 `system`（`is_system=t`） |
| 全库有效令牌总数 | 101 | 其中 100 条带密文（均为压测造的 bench 令牌），1 条 no-cipher |
| ACTIVE 令牌总数 | 101 | no-cipher 的 1 条也是 ACTIVE |
| no-cipher 令牌的账单关联 | **0 条** | `billing_records` 里 `token_id=11650277558976512` 的行数为 0；`call_logs` 全表 0 行 |
| no-cipher 令牌的最后使用 | NULL（从未使用） | `last_used_at IS NULL` |
| 是否已绑定 callerKey | 无法在本库验证（`magpie_keys` 表不存在） | 开发库迁移链停在 V1（最后一条 `0020_user_daily_stats`），V2/V3 对象未建；生产库的核对 SQL 见 §2 步骤 0 |
| 生产/staging 库的实际条数 | **待确认** | 沙盒连不到生产；staging 库为空库（0 张业务表）。执行前必须先在目标库跑 §1.1 的 SQL 复核 N |

> 开发库的这一条是 `seedSystem` 播种的必然结果：种子 INSERT 不写 `secret_cipher`。
> 也就是说，**任何按当前种子代码初始化的环境，都会有恰好这 1 条 no-cipher**；
> 生产库若没有手工造过无密文令牌，N 也应为 1（同一条 system-probe）。

## 2. 处置方案：轮换 → 自动注册 callerKey（共 6 步）

> 核心机制（代码事实）：`Rotate`（token.go L647–663）= 作废旧令牌（`status='DISABLED'`）→
> 调 `Create` 换发新令牌；`Create` 内部（L463–471）**自动**完成「SM4 加密落库 →
> `EnsureCallerKey` 注册到代理侧（名字 `ls-token-<新id>`）→ 写 `magpie_keys` 映射」，
> 注册失败会回滚删除新令牌并报错，不存在「有令牌但调不通」的中间态。
> **所以「轮换令牌」和「自动注册 callerKey」是同一个动作，不需要单独触发注册。**

### 步骤 0（执行前核对）：在目标库跑盘点 SQL，确认 N 与清单

- 在哪做：DB（生产库，只读查询）。
- 前置条件：能连生产库；同时确认服务已在跑 V3（`magpie_keys` 表存在）。
- 操作：跑 §1.1 的 SQL，另跑一条绑定状态核对：
  ```sql
  -- no-cipher 令牌是否已有 callerKey 映射（预期没有；no-cipher 永远注册不了）
  SELECT t.id, t.display_name,
         (SELECT count(*) FROM magpie_keys mk
           WHERE mk.token_id = t.id AND mk.state = 'ACTIVE') AS active_key_rows
    FROM tokens t
   WHERE t.deleted_at IS NULL AND COALESCE(t.secret_cipher,'') = '';
  ```
- 验证：N 与本文档 §1.1 一致（预期 1，即 system-probe）；若 N > 1，先停下核对多出来的
  令牌是谁的（本文档步骤只覆盖「系统探测令牌」这一类，普通用户令牌要走 §3 的注意事项）。
- 本步不做任何写操作。

### 步骤 1：确认系统探测令牌当前没有消费方

- 在哪做：DB（只读）+ 管理面/代码核对。
- 前置条件：步骤 0 完成。
- 操作（只读）：
  ```sql
  SELECT count(*) FROM billing_records WHERE token_id = 11650277558976512;  -- 预期 0
  SELECT count(*) FROM call_logs     WHERE token_id = 11650277558976512;  -- 预期 0
  ```
- 依据（代码事实）：系统探测令牌的下游消费只有两处——① 成本统计的探测行排除条件
  按 `users.is_system OR display_name='system-probe'` 匹配（console/cost.go `costProbeExclusionSQL`），
  **不看令牌串本身**；② 探测账单记账按 token_id 归属（`SystemIdentity`）。轮换会换 token_id，
  两处都是按新 token_id 继续工作，不需要改代码。
- 验证：两个 count 均为 0（开发库实测如此）；若生产库不为 0，转「待确认」人工评估（见 §4）。

### 步骤 2：执行轮换（先删旧种子令牌，再重建）

> ⚠️ 为什么不用开发端 `POST /api/v1/dev/tokens/{id}/rotate`：`Rotate` 校验
> `t.UserID != userID` 即拒绝（L654），而 system-probe 属于系统用户 `system`
>（无登录口令、无人能登录），**没有任何账户能以它的身份调 rotate**。
> 管理端也没有 rotate 端点（server.go L123–126 只有 list/create/batch-delete/secret）。
> 所以系统令牌的轮换 = 管理端 batch-delete 删旧 + 管理端 create 建新（两者都是现成端点，
> 不写代码），或等价的 DB 操作。**推荐走管理端 API**，因为 create 会自动完成 callerKey 注册。

- 在哪做：管理面（ADMIN 登录）或对后端 `/api` 的管理端 HTTP 调用；不直接改 DB。
- 前置条件：步骤 1 确认旧令牌无消费方；`magpie_keys` 已在目标库就绪（V3 已上线）；SM4 密钥已配置
  （`backend_keys` 卷有 `sm4.key`——正常部署都有，否则 create 直接报「encrypt token secret」错误）。
- 操作（N=2 个请求）：
  1. `POST /api/v1/admin/tokens/batch-delete`，body：`{"IDs": ["11650277558976512"]}`
     （软删旧令牌；`BatchDelete` 会顺带尝试删代理侧 callerKey——它本来就没有，幂等无害）。
  2. `POST /api/v1/admin/tokens`，body（对齐 seedSystem 的语义）：
     `{"UserID": "<system 用户 id>", "DisplayName": "system-probe"}`，`TagID`/`ExpiresAt` 不传
     （与现状一致：无过期时间）。
     响应里的 `Plain` 是**新令牌明文（sk-gw-…），仅展示一次，立即记录到部署密钥保管处**
     ——后续如果需要「用系统身份直接调代理侧」才有用；仅记账归属用不到它。
- 验证（三查）：
  1. `GET /api/v1/admin/tokens`（或 DB）确认新令牌 ACTIVE、display_name 同名、属 system 用户；
  2. `GET /api/v1/admin/tokens/<新id>/magpie-key` 返回 `Bound:true, State:"ACTIVE"`（自动注册成功的直接证据）；
  3. DB 侧 `SELECT count(*) FROM tokens WHERE COALESCE(secret_cipher,'')='' AND deleted_at IS NULL;` 应为 **0**。
- 注意：`SystemIdentity`（`seeding/seed.go` L130–135）按 `display_name='system-probe'` 查系统令牌，
  新令牌沿用同名即保持兼容；`seedSystem` 下次启动的 `INSERT ... ON CONFLICT DO NOTHING`
  会因为「同 user 同 display_name 已存在」语义不再插入新行（幂等，不产生第二条）。
  ⚠️「待确认」：该 INSERT 的冲突保护依赖 `(user_id, display_name)` 的既有数据——当前代码没有
  唯一索引兜底，若 seed 在轮换后的库上重跑，理论上可能再插一条同名 no-cipher 令牌
  （受 `ON CONFLICT DO NOTHING` 保护，但该子句没有指定冲突目标，实际命中主键冲突才跳过）。
  执行完本方案后**重启一次服务并复查 N 是否仍为 0**，即可验证这一点（见步骤 4）。

### 步骤 3：确认补建循环收敛

- 在哪做：后端容器日志（`docker compose logs backend`）。
- 前置条件：步骤 2 完成后**重启 backend**（让启动补建 goroutine 重新跑一轮）。
- 操作：观察启动日志里的「V3 存量令牌补建完成」行（main.go L540）。
- 验证：该行出现 `skipped_no_cipher=0 failed=0` 且**只出现一次**（没有 10 轮重扫）——
  补建循环的退出条件是 `len(failed)==0`（main.go L541–543），no-cipher 清零后整轮扫描
  在第一轮就提前退出，§4 的遗留问题自然收敛。
- 若失败：`skipped_no_cipher` 不为 0 → 回步骤 0 复查 N；`failed` 含其他原因（如 magpie 不通）
  → 按 magpie-container.md §5.2 排代理侧凭据，与本方案无关。

### 步骤 4：全量回归核对（只读）

- 在哪做：DB（只读）。
- 操作：
  ```sql
  SELECT count(*) AS no_cipher_left
    FROM tokens t JOIN users u ON u.id=t.user_id
   WHERE t.deleted_at IS NULL AND u.deleted_at IS NULL
     AND COALESCE(t.secret_cipher,'') = '';              -- 预期 0
  SELECT count(*) AS unbound_active_tokens
    FROM tokens t
   WHERE t.deleted_at IS NULL AND t.status='ACTIVE'
     AND NOT EXISTS (SELECT 1 FROM magpie_keys mk
                     WHERE mk.token_id=t.id AND mk.state='ACTIVE');  -- 预期 0
  ```
- 验证：两个 count 都是 0 → 处置完成；之后每次重启不应再出现 no-cipher 计数。

### 步骤 5：备份与记录（非强制，建议）

- 在哪做：部署机。
- 操作：执行前照常做一次升级前备份（`pg_dump` 写 volume，流程见 docs/plans/2026-11-15-v2-p1-rollback.md §4）；
  把新令牌明文（如后续要用）与执行时间记入运维手册。
- 验证：备份文件校验通过（sha256 + `pg_restore -l` 计数，同上文档）。

## 3. 影响面分析

### 3.1 客户端要不要改？

| 场景 | 是否要改 | 说明 |
|---|---|---|
| 普通业务调用方（用自己令牌调 `/v1`） | **不用改** | 本方案只动 system-probe 一条系统令牌，业务令牌（token_hash、secret_cipher）原样不动；`/v1` 地址不变（nginx 直连代理侧网关端口，域名/端口都不变） |
| 系统令牌本身 | **看用途** | 开发/生产现状里系统令牌**从未被用于真实调用**（`last_used_at=NULL`、0 账单、0 调用日志；探测开销记账只按 token_id 归属，不走 HTTP 调用）。若未来要用系统身份直调 `/v1`，用的是轮换后 `POST /admin/tokens` 响应里的新明文（sk-gw-…，旧值 `sk-gw-system-probe` 从来不是有效密钥——它的 `token_hash` 只是占位串 `"system-probe-hash"`，无法通过任何哈希校验） |
| 管理面/前端 | 不用改 | 成本统计的探测排除条件按 `is_system OR display_name='system-probe'` 匹配，与令牌串/ID 无关 |

### 3.2 轮换期间会不会断服务？

**不会。**依据：

- 被轮换的旧令牌没有任何调用方在用（步骤 1 的 count=0），作废它不影响任何请求。
- 系统探测令牌的记账归属在新令牌创建后立即由新 token_id 承接；轮换窗口内（秒级）若有
  探测类账单写入，也只是归属 ID 换了名字（display_name 不变），金额与统计口径不变。
- `/v1` 转发链路完全不经手灵枢（nginx 直连 `magpie:3425`），本轮换不触碰转发路径。
- 无停机、无重启要求——步骤 3 的重启**不是功能需要**，是为了当场验证补建循环收敛
  （也可以等下一次自然重启再核）。

### 3.3 失败回滚怎么做？

| 失败点 | 现象 | 回滚动作 |
|---|---|---|
| create 新令牌失败（EnsureCallerKey 报错） | API 返回 500「令牌已创建但开通代理访问失败（已回滚）」 | 无需回滚：Create 原子性保证失败时新令牌已删；旧令牌此时已软删——重新执行步骤 2 第 2 步即可；期间无业务影响（该令牌无消费方） |
| 新令牌建好但 callerKey 状态不是 ACTIVE | `GET .../magpie-key` 返回 `State:"PENDING"` 或 Bound:false | 重跑一次 `POST /api/v1/admin/tokens/<新id>/magpie-key/auto`（现成端点，幂等：从密文解出明文重新 Ensure）；仍失败按 magpie-container.md §5.2 排代理侧 |
| 误删了不该删的令牌 | 步骤 0 清单之外的 ID 被删 | 用步骤 5 的升级前备份按 docs/plans/2026-11-15-v2-p1-rollback.md §4 还原整库（软删行可从备份找回）；**不要**手工 UPDATE 数据 |
| 轮换后发现需要旧 token_id 的兼容 | （不适用：无消费方） | 无操作；若未来有脚本按旧 ID 记账，脚本改查 `display_name='system-probe'` 的当前 ID |

## 4. 遗留问题（任务 1 上报、已采纳并入本方案）

**现象**：PR #164（`1b6d558`，随 v0.15.0-rc4 上线）把补建结果按原因分级后，
no-cipher 令牌**既计入 `skipped["no-cipher"]` 也追加进 `failed`**（token.go L514–515）。
而启动补建循环（main.go L538–548）的提前退出条件是 `len(failed)==0`：

```go
for i := 0; i < 10; i++ {
    created, skipped, failed, err := tokenSvc.BackfillCallerKeys(context.Background(), 0)
    if err == nil {
        logr.Info("V3 存量令牌补建完成", ...)
        if len(failed) == 0 {
            return
        }
    }
    ...
    time.Sleep(time.Duration(10*(i+1)) * time.Second)  // 10s→100s，共 10 轮
}
```

**后果（真数字）**：现网只要有 1 条 no-cipher 存量令牌（本文档 §1 盘点：恰好 1 条 system-probe），
每轮整表重扫（101 条 × 分页 50/页 ≈ 3 页查询）都把同一条记进 failed，
补建 goroutine 会跑满 **10 轮**，间隔 **10s→20s→…→100s 线性递增**（`10*(i+1)`），
总耗时约 **9 分钟**才放弃；期间每轮 INFO 日志都带 `failed=1`，容易误读成故障。
代码注释写「指数退避」实为线性递增（main.go L537 注释），行为无害但名实不符。

**本方案执行后如何收敛**：no-cipher 清零（§2 步骤 2+4）→ `failed` 清空 →
补建循环**第一轮即提前退出**（`len(failed)==0` → return），10 轮重扫不再发生。
即：**本方案就是该遗留问题的处置手段**，不需要改代码即可消除现象。
注意：failed 里若混入**非** no-cipher 的其他错误（如 magpie 暂时不通），循环仍会跑满 10 轮——
这是「用重试兜底瞬时故障」的既有设计，属预期行为。

**是否建议后续单独修复重试逻辑（只建议，不动手）**：建议（低优先级，P3）：

1. no-cipher 应从 `failed` 拆出（它不是可重试错误，重扫 10 轮的结果恒定不变）；
   退出条件可改为「failed 里无非 no-cipher 项即提前退出」。
2. 把 `time.Duration(10*(i+1))` 的注释「指数退避」改为「线性递增」，或真改成指数（10s→100s 上限）。

两项都等 spencer 拍板后再立单；本方案文档不附带代码改动。

**另一个顺带发现（待确认，不改）**：`HandleAdminBackfillMagpieKeys`（token.go L814–823，
`POST /api/v1/admin/tokens/magpie-keys/backfill`）在 v0.15.0-rc4 有 handler 定义但
`server.go` **没有注册该路由**（全仓 grep 无挂载点）；手动触发补建目前只能靠重启服务的
启动 goroutine。若运维需要不重启的手动补建入口，需补一行路由注册（单独小 PR）。

## 5. 背景事实（影响面分析的前提）

- `origin/main` = `69eccb6`（**v0.15.0-rc4**，2026-11-19 20:40 合并 PR #167）。
  注：派工单写「main = v0.15.0-rc3」，实际 main 已在 rc4（rc3 tag `a3e4020` 是 16:57 的上一站；
  本文档按 rc4 口径核对，相关代码两版一致，仅 #164 的日志字段分级是 rc4 新增）。
- `/v1` 由 nginx 直连代理侧网关端口 `magpie:3425`（frontend/nginx.conf L140–152：
  `proxy_pass http://$magpie_gw$request_uri`），灵枢不再逐请求转发；计费走 60s 账本轮询（v3billing）。
- PR #168（`chore/v2-proxy-preauth-removal`，已 READY 待合入）删除 V2 逐请求代理链与预扣后，
  `/v1` 对灵枢端口返回 410；管理面 callerKey 绑定（`/admin/tokens/{id}/magpie-key` 族）、
  拓扑、账本、对账全部保留。本文档的轮换端点与自动注册机制均属保留面，不受 #168 影响。
- 令牌明文 = callerKey 明文（同一串值）：灵枢创建令牌时把它注册到代理侧（`add-key` 自带 secret）；
  代理侧只在创建时显示一次，灵枢侧存 SM4 密文（列口径同 `tokens.secret_cipher`）。

## 6. 待确认清单（给 spencer）

1. 生产库执行 §1.1 盘点 SQL 的实际 N（沙盒只实测了开发库：N=1，恰好是 system-probe；
   预期生产同样为 1，但需实测确认）。
2. 系统令牌轮换走「管理端 batch-delete + create」是否符合预期（本文档推荐路径），
   还是希望给 seedSystem 补 `secret_cipher` 写入从根上消除 no-cipher（改代码，不在本方案范围）。
3. §4 的 P3 重试逻辑修复是否立单。
4. `POST /admin/tokens/magpie-keys/backfill` 有 handler 无路由（§4 顺带发现）是否补注册。
