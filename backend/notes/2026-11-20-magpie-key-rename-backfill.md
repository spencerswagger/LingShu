magpie callerKey 改名 + 存量回填（0030）

改了什么：
- 迁移 0030_magpie_key_rename.sql：magpie_keys 加 legacy_key_name TEXT NOT NULL DEFAULT '' + 部分索引 idx_magpie_keys_legacy_name（WHERE legacy_key_name <> ''）。纯 ADD，幂等可回滚。
- magpie/keyname.go：新口径自动名唯一实现点。名字形状 ls-u<uid>-t<tid>-<slug>（slug 取令牌 display_name 归一 ≤16 字符）；MaxAutoKeyNameLen=64（自动名封顶）；MagpieKeyNameLimit=120（对齐 magpie 真机 1..120 字符上限，管理端手工绑定同步收紧 128→120）。
- magpie/adminclient.go：新增 RenameCallerKey（POST /api/caller-keys/rename-key，页面会话通道）。旧名不存在 / 目标名已存在 / 重复改名均幂等成功；>120 字符客户端拦截（不发请求）。
- magpie/callersync.go Ensure 四分支幂等：已是目标名（终态快检：解密明文一致且 ACTIVE → 零写零网络）；当前名==旧自动名 → 改名三步（①代理侧 rename-key 原地改、值不变 → ②确保新名明文 → ③本地行改名+激活+旧名落 legacy_key_name）；手工绑定名 → 名字不动只对齐明文；无映射 → 按新名建。任一步中断重跑可续。
- magpie/credentials.go：Key 加 LegacyKeyName，查询列加 coalesce(legacy_key_name,'')；新增 RenameAndActivate（UPDATE ... WHERE token_id=$1 AND state<>'DISABLED' RETURNING 全列）。
- identity/token.go：BackfillCallerKeys(ctx,limit,dryRun) 重写——改名资格=当前名==旧自动名 ls-token-<id>（确定性映射，不猜名）；dry-run 只分桶（renamed/already-bound/manual-named/no-binding/no-cipher）不执行；执行态旧名令牌走 Ensure 改名、手工名只对齐、无密文 failed。CallerKeySyncer 接口 Ensure 带上 userID+display_name。
- identity 新增 CallerKeyToken 类型（避免 magpie→identity 反向 import 环）；LegacyAutoKeyName 与 magpie.LegacyAutoName 语义一致（测试锁）。
- v3billing/poller.go：账本行反查令牌改为「先 key_name 查，查不到按 legacy_key_name 兜底 + WARN（账本行按旧名结算）」。两步都是精确等值 LIMIT 1。
- cmd/server/main.go：启动补建 goroutine 用新签名；回填路由 POST /api/v1/admin/tokens/magpie-keys/backfill（handler 已存在但此前未注册，本 PR 一并补上）。
- server.go：注册上述回填路由。

为什么：
- 账本按 caller_key_name 反查令牌结算。改名窗口之前的账本行是旧名快照，没有兜底列那些行反查失败、永久漏计费。
- 改名用页面 rename-key 原地改：ID/secret/限额都不变，客户端无 401 空窗（add 新删旧与 rotate 两条路都有空窗或被同值唯一堵死，已验证不可用）。

影响范围：
- magpie_keys 表结构（加一列）；callerKey 同步/回填/绑定端点；v3billing 结算反查。转发路径与计费金额逻辑不变。

怎么验证：
- 单测：keyname 形状/示例值/UTF-8 截断/无 PII 8 例；Ensure 四分支幂等（改名主链/重跑收敛/中断续跑/手工名不动/新建）5 例；RenameCallerKey 幂等与超限拦截 2 例；KeyStore 改名/无行/扫描 3 例；回填 dry-run 分桶/执行态/重跑幂等 3 例；结算反查新名直查/旧名兜底+WARN/查不到报错 3 例。全部 -race 通过。
- 变异验证：删 legacy 兜底查询 → 结算用例红；还原即绿。
- 真库（lingshu_test）：db.Migrate 跑 0030 → newly=1、列 NOT NULL DEFAULT ''、部分索引建成；重复跑 newly=0（幂等）；含旧名的行 key_name/legacy_key_name 同行共存验证通过。
- magpie 名字上限与 rename-key 端点行为来自真机探针（magpie-cli 0.1.4353 实测：120 过 121 拒；rename-key 200 且 ID/secret/限额不变）。

风险点：
- 启动补建 goroutine 会在部署后自动对存量令牌执行真机改名。需要先对真实环境跑 dry-run（POST backfill {"DryRun":true}）并确认桶分布，再决定是否放开自动执行；如需完全手动，可在部署前协商 kill-switch。
- legacy_key_name 只存「最近一次改名前的名字」；同一名多次改名只保留最近旧名（当前场景只有 ls-token-<id>→新名一次改名，够用）。
- magpie 页面接口无 rename 文档保证，属 UI 通道；magpie 升级若移除该端点，改名三步会在第一步失败（可观测，重跑可续，不影响已改名令牌）。

---

## 补记（同 PR 第二提交，2026-11-20）

- 管理端令牌列表（GET /api/v1/admin/tokens）每行补 `MagpieKeyName`（omitempty）；解析走
  `magpie.KeyStore.NamesByTokenIDs`（批量 IN 分批 500，只读）。装配层只注入管理端 handler，
  dev 列表与创建/轮换响应不带该字段。前端令牌列表加「代理侧 Key」列（未绑定显示占位）。
  ——对应命名定案①「UI 直接显示人可读的名字」。
- 真机核验复跑于 magpie **0.1.4454**（官方 magpie-cli-linux-amd64，sha256 校验通过；
  本机隔离 HOME 起 `magpie web --gateway`，页面会话 + 网关 3425 同进程实测）：
  - 名称上限：add 边界二分 120 成功 / 121 拒（`Use a name between 1 and 120 characters`）；
    1 成功 / 空（0）拒 —— 与 `MagpieKeyNameLimit=120` 一致，代码未改。
  - rename-key 端点：改 120 字目标名 200；id 与 masked secret 不变；重复改名幂等（两次 200）；
    121 目标名 400。改名后旧 secret 打 3425 /v1/models 仍 200（原地改、无 401 空窗）。
  - 0.1.4454 的 CLI `gateway-key` **没有 rename 子命令**（usage 已变）；回退/运维脚本不能依赖 CLI 改名。
  - magpie 仓文档副本 docs/reference.md 管理面路由表记载 gateway-keys/{action} 含 rename-key
    ——该端点有官方文档出处。
- CI：PR base 已改 v2（分支 rebase 到 origin/v2 后 force-with-lease 推送），head 双 job success。

---

## 补记二（契约定稿对齐批，2026-11-20 第 3 提交）

命名形状按契约定稿 §1.1 重做（推翻首提交的 ls-u<uid>-t<tid>-<slug> 形状）：

- **key 名 = 「用户名转写-令牌名转写」**（uid/tid 不进名；唯一性由 magpie_keys UNIQUE(token_id) 承担）。
  转写：小写 → 非 [a-z0-9] 折 '-' → 连续 '-' 合一 → 去首尾 '-'。
- 段空退化（契约 §1.1 转写规则行）：用户名无 ASCII → `u<uid>` 段；**令牌名无 ASCII → `w-`+4hex 段**
  （4hex = sha256("lingshu:caller-key:<tokenID>") 前 4 hex，确定性派生——回填重算可比对、无 PII）；
  两段全空（防御分支）→ `w-`+随机 4hex。示例（od 字节级核验）：zhangsan/办公室台式机 → `zhangsan-w-5866`；
  王五/机密项目 → `u9-w-8433`；李四/Claude Code → `u3-claude-code`。
- 64 封顶：先截转写段再拼消歧尾号，尾号永不截（reserve=4）。消歧：撞名 → `-2/-3…-9` 顺延
  （8 候选耗尽 → ErrNameUnavailable，不自动放大轮次）。
- 契约 §1.2 的 rename 端点补齐并改默认：`POST /api/v1/admin/tokens/magpie-keys/rename`
  **dry-run 默认开启**，`?dry_run=false`（或请求体 {"DryRun":false}）才执行；消歧耗尽 → **409**
  （resp.CodeConflict，明细在 Failed[].Conflict）。旧 /backfill 路由未合入过任何环境，直接更名替换。
- **回滚端点**：`POST /api/v1/admin/tokens/magpie-keys/rename-rollback`（dry-run 默认开启）。
  执行=按 legacy_key_name 复原旧名（RollbackToken：代理侧旧名复原值不变 → 本地行 key_name=旧名 →
  legacy 清空），三步与正向改名镜像、幂等可重跑；已复原行再跑返回 skipped 不报错。
- **契约 §2.5 限流退避**落进 PageAdmin.doJSON：429/5xx 指数退避 5s→20s→60s、最多 3 次、ctx 可中断。
  EnsureCallerKey 改为「先 list 判存在再 add/rotate」（真机 add-key 撞名回 5xx，直接探测会被退避拖 85s；
  单测锁 85s→0.01s）。
- identity 哨兵 ErrCallerKeyNameTaken 放 identity（magpie 已 import identity，反向成环）；
  magpie.ErrNameUnavailable 是它的别名。回填 failed 条目带 Conflict:true，handler 聚合成 409。
- Ensure 签名带 username（key 名第一段原料）；keyadmin auto 端点补 ownerOf（u<uid> 退化坐标）。

**契约条目的实现偏差（有意，报备）**：契约 §1.2 rename 流程写的是
「Ensure(新名)→Provision→DeleteCallerKey(旧名)」三步；实测（0.1.4454 真机）magpie 页面有
rename-key 原地改名端点（官方 reference.md 管理面路由表记载），原地改 ID/secret/限额不变、
无 401 空窗；add-then-delete 两步之间存在「新名已建、旧名还在」的双活窗口且 secret 全局唯一
会堵死同值迁移。故实现取 rename-key 原地改，终态与契约一致（新名生效、旧名只留在
legacy_key_name 兜底列）。回滚同理由 rename-key 反向改回。

怎么验证（本批）：
- 单测重写：keyname 形状/退化/封顶/无 PII；Ensure 改名主链/重跑收敛/中断续跑/手工名不动/新建/消歧
  （-2、-3、本令牌豁免、8 候选耗尽）；RollbackToken 复原/重跑幂等/无旧名跳过；rename 端点
  dry-run 默认/执行/409/body 显式 false；rollback 端点 dry-run 默认/执行/缺 sync 503；
  doJSON 429 重试/耗尽/4xx 不重试/ctx 取消；Once 层「两段皆空 Warn 跳过」。
- 变异：删 legacy 兜底 → 2 例红；删 dry-run 默认 → 红；还原全绿。
- 真库（LLMGATEWAY_TEST_PG_DSN 临时库全链迁移）：0030 两遍幂等（第二遍零报错）；
  RenameAndActivate → key_name/legacy 同行；RollbackToken 演练复原（代理侧旧名 + 本地行 +
  legacy 清空）；settleOne 两段反查 SQL 真库三态（新名命中 / 旧名经 legacy 兜底命中 / 未知名零命中）。
- 门禁：gofmt / go vet / `go test -race` 36 包 0 FAIL / `go test ./tests/` / vue-tsc 全绿。
- 日志样例（两段皆空 Warn 跳过，errbody 实录）：
  `time=2026-11-20T07:57:45.214Z level=WARN msg=单条结算失败（下轮重试） ledger_id=lg-evidence err="找不到 callerKey=ls-ghost 对应令牌：no rows"`

风险点（新增）：
- 消歧耗尽（409）后该令牌挂起人工：当前 8 候选覆盖同基础名 9 连撞之外的场景；
  真实环境同名令牌超过 8 个才会触发（触发时 failed 明细给出 tokenID）。
- 启动补建 goroutine 自动改名风险不变（部署前先 POST rename 默认 dry-run 看桶分布；
  需要完全手动可协商 kill-switch）。

---

## Owl II 评审修复（2026-11-20 夜，锚 head e2c77d2 之上）

必改 1（停用令牌两洞）——Ensure 加 state 守卫，DISABLED 行一律拒绝（ErrKeyDisabled）：
- 洞①：手工名/终态名分支 Provision upsert 无守卫，自动回填把停用行翻回 ACTIVE、
  代理侧 key 被重建（Toggle 刚删的被原样搬回）。
- 洞②：旧自动名分支先 rename 代理侧、本地 RenameAndActivate 被 state<>'DISABLED' 拒
  → 本地永远追不上、启动 goroutine 每轮重试死循环。
- 修法：Ensure 取行后先判 state（在任何代理侧动作与本地写之前）停用即返回哨兵；
  显式启用（Toggle 启用侧 / 管理端一键开通）改走新增 Reactivate —— 唯一允许把
  DISABLED 行翻回 ACTIVE 的入口（先按当前名恢复代理 key + 本地翻 ACTIVE，再复用
  Ensure 收敛改名/对齐）。
- 回填侧配套：新 disabled 分桶（dry-run 与执行态都跳过、不进 failed、不触发密文查询）；
  状态钩子（SetCallerKeyStateOf）缺位时由 Ensure 哨兵兜底（第二道防线）。
- 测试钉住：TestEnsure_DisabledRowUntouched（不翻状态/代理零调用/重复调用无副作用）、
  TestEnsure_DisabledLegacyRowNoProxyRename（洞②锚：绝不先 rename 代理侧）、
  TestReactivate_DisabledRowFlipsAndConverges（显式启用终态=从未停用）、
  TestReactivate_ActiveRowIsPlainEnsure、
  identity：TestBackfill_DisabledBucketSkips（两态分桶/Ensure 零调用）、
  TestBackfill_DisabledBySentinel（钩子缺位兜底）。
- 变异验证：删 state 守卫 → 两条 Ensure 用例红；还原全绿。

P2-1（主持人拍板：env 闸门默认关）：启动自动补建 goroutine 包进
  `if envFlagOn("CALLERKEY_BACKFILL_ON_BOOT")`（未设/0/false/off = 关，启动打 Info 说明
  「先 dry-run 再放开」）；部署顺序写进 main.go 注释与本 notes；
  cmd/server/wiring_test.go 新增 TestWiring_StartupBackfillGatedByEnv 源码级守卫
  （按行首 `if envFlagOn(...)` 判定，注释里提到变量名不算）。
  变异（闸门改 `if true`）→ 守卫红；还原绿。

P2-2a：KeyStore.Rename 零生产调用，已删（fake 同步清理）。

P2-2b：RenameAndActivate 的 legacy 兜底列写入钉死——
  ① keystore_test 期望改全文 ^…$ 锚定（逐字拷贝实现语句，故意不引用实现常量，
  防同源假绿）；② WithArgs 逐位钉 $2=旧名；③ 真库回写断言沿用
  rename_rollback_pg_test 的 RenameAndActivate → ByTokenID 往返（legacy 值逐字校验）。
  变异（legacy_key_name 写 ''）→ 全文锚定红；还原绿。

P2-3：任务书口径待 spencer 确认，不动代码。
