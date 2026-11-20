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
