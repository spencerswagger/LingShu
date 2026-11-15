# PR #82 第二轮复审：1 条 P1 + 4 条评估（2026-11-15）

分支 `feat/lingshu-v2-p1-usage-billing`，基线 head `04cf6f6`。
所有数字都是「可复现输入 → 观测输出」，并且逐条做过「改回旧实现 → 用例红 → 还原 → 绿」的变异验证。

## P1 窗口级偏差把估算单/单侧缺账算进分子（已修）

位置 `usagebilling/reconcile.go::compareWindow`。旧实现：`sumLSTokens/sumMagToks/sumAbsDelta`
在「单侧缺账跳过」**之前**累加，且窗口分母用全量 `sumMagToks`。

修法：偏差口径单独累计 `sumCmpMag / sumAbsDelta / cmpGroups`，只在 `ls.Requests>0 && mg.Requests>0`
的分组上累加；分母为 0 → `DevPct=nil`（无从比较）。`detail` 增 `compared_groups`、
`token_sum_magpie_compared`；`token_sum_ls/token_sum_magpie` 保持全量（报告口径）。

| 场景 | 输入 | 旧实现 | 修复后 |
|---|---|---|---|
| ① 只有估算单、两侧完全一致 | local estimated in=15000 out=5000；magpie in=15000 out=5000 | `dev_pct=100`，`status=alarm` | `dev_pct=null`，`status=ok`，`compared_groups=0`，`token_sum_magpie=20000` |
| ② 单侧缺账 | local k1 in=16000（10 笔）；magpie k2 in=4000（10 笔） | `dev_pct=500`，`status=alarm` | `dev_pct=null`，`status=ok`，两侧各记 1 条 `absent_in_*` |
| ③ 可比组一致 + 两侧各有一条单侧缺账 | local k1 1000/1000 + k9 16000；magpie k1 1000/1000 + k8 4000 | 偏差被单侧组撑大 | `dev_pct=0`，`status=ok`，`compared_groups=1`，`token_sum_magpie_compared=2000`，`token_sum_magpie=6000` |

（复审给的 93.9% 是它自己那组输入的形状；同一类缺陷在我这组输入上是 500%，修好后都是 `null`/`ok`。）

用例：`TestCompareWindow_OnlyEstimatedLocalNoWindowDeviation`、
`TestCompareWindow_SingleSideMissingNoWindowDeviation`、
`TestCompareWindow_DeviationOnlyOverComparableGroups`。
变异（把累加挪回跳过之前 + 分母改回 `sumMagToks`）：三条全红，报 `dev_pct=100` / `dev_pct=500` / `dev_pct!=0`。

既有口径用例未回退：分组偏差/阈值内/小样本/magpie=0/estimated 排除/混合组/纯正常组/窗口阈值/streak 全 PASS。

## 4 条评估

### 1. 账本聚合形状忽略 `requests/count`（已修）

`groupsFromItems` 原来一律 `Requests=1`（还有一个 `if !hasFailed && !hasRetried` 的空分支）。
现在按 item 自带的 `requests`/`request_count`/`count`/`calls` 计数（缺省 1）。

- 用例 `TestParseLedgerBody_AggregateRequestCounts`：`requests:7` + `count:4` 同组 → `Requests=11`
  （旧实现 2）；逐笔形状仍是 1。
- 用例 `TestCompareWindow_AggregateRequestGateWorks`：聚合 7 笔 vs 本地 7 笔 + 100% 偏差 → `alarm`
  且 `req_count_mismatch=false`。旧实现 `alarm_groups=[]`、`req_count_mismatch=true`（门槛永不满足）。
- 变异（改回 `Requests=1`）：两条红（`实际 2` / 不报警）。

### 2. `LocalGroups` 只取 completed（不改）

代理侧同样把 `failed` 单排除在 token 对比之外（`groupsFromItems` 里 `continue`），两侧口径是**对称**的；
单侧把 failed 计入会立刻变成反向不一致（那类账单 `credits=0`、灵枢侧却带 token）。
要改必须两侧同时改，属口径变更 → 报产品/架构拍板，本件不动。
已知盲区：灵枢「结算失败（有 usage、已退款）」的请求在灵枢是 failed、在代理侧是成功 → 该组会落到
`absent_in_ls`，属于「能定位、不报警」，另有 `billing_anomaly` 审计兜底。风险：这类请求不会进偏差判定。

### 3. `Settle` 在 defer 里同步执行且没有 deadline（已修）

转发层给 hook 的 ctx 是 `context.WithoutCancel(r.Context())`（保留 requestId、去掉取消信号**和 deadline**），
DB/连接池卡住时这条 goroutine 会永久挂着。`Settle` 入口补 `settleTimeout = 30 * time.Second`
（覆盖扣款、退预扣 3 次重试、审计写）。

用例 `TestSettle_SetsDeadlineOnBillingCalls`：`context.WithoutCancel` 传入 → `billing.Record` 收到的 ctx
必须带 deadline 且 ≤ `settleTimeout`。变异（删掉 `WithTimeout`）：该用例红。

### 4. `settlePlain` 双记账（已修）

旧 `settleLedger`：注入事务后若事务在提交前失败（BeginTx 失败 / 账单 Insert 非唯一约束错误），
退回 `settlePlain` —— 那条路在**事务外**真扣一次；而调用方（usagebilling）只知道「结算失败」，
只会退预扣 + 落 failed 账单 ⇒ 差额被扣走、账单显示 0 = 双记账（且 `enqueueRetry` 之后可能再写一条 completed）。

修法：注入事务后**一律不退回非事务路径**，失败直接上抛；`settlePlain` 只保留给 `txdb == nil`（单测/未装配）。
钱不会被扣，调用方退预扣 + failed 账单 + `billing_anomaly` 审计三段留痕。

- 用例 `TestRecord_TxPath_InsertFailure_NoPlainFallback`（原 `..._FallsBackToPlain` 按新口径重写）：
  `tx=1 plain=0` 且错误上抛。变异（恢复回退）：红，报 `tx=1 plain=1`。
- 用例 `TestRecord_TxBeginFailure_NoPlainFallback`：BeginTx 失败 → `plain=0`。变异：红，报 `plain=1`。

## 门禁

- `gofmt -l internal/domain/{billing,usagebilling}` → 空。
- `go build ./...`、`go vet ./...` → 干净。
- `go test -race $(go list ./... | grep -v '/tests$') -count=1` → 25 包全 ok。
- `go test ./tests/` → ok。
- 真库（`LLMGATEWAY_TEST_PG_DSN=$TEST_DATABASE_URL`，PG17@5432）：`usagebilling` 三个锁用例
  （含 `TestAdvisoryLock_RealPG_ReleaseActuallyUnlocks`）PASS 非 SKIP；`billing` 包 2.45s 全 PASS。

## 未做（留后续单，已报）

- 代理侧账本的 `failed` 若以计数形式返回（`failed: 3`），`pickBoolOK` 不认 → 走「缺字段 → blocked」降级，
  属保守方向；要支持聚合失败计数得先定口径（是否按笔数扣减）。
- 结算失败的请求不进对账（见评估 2），需要两侧口径一起定。
