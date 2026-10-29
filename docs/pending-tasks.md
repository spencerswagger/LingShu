# 待办任务书（Pending Tasks）

> 维护人：主持人。格式见团队标准作业手册（`docs/` 内作业口径）：每条一个区块，
> 字段固定为 背景 / 验收标准 / 约束 / 状态 / 负责人。
> 状态取值：待确认 / 已确认 / 进行中 / 已完成 / 已回收。
> 忙时（08:00–22:00）只做方案确认与任务书维护，闲时按优先级取任务实现。

---

## [P1] 账单详情页请求定位改造（2026-10-26）

- **背景**：账单详情页无法定位「某次请求当初发了什么、为什么失败」。用户要求详情页展示完整增量消息 + 请求参数 + 决策轨迹 + 上游返回 + 用量 + 错误信息；同时一次请求若多候选失败会在账单里刷出多条流水，无法按 request_id 追溯。
- **验收标准**：
  1. `call_logs` 新增 `req_params`：存 `messages` 之外的**全部**入站请求参数（model、temperature、max_tokens、top_p、tools 摘要、stream、response_format、请求端点、上游协议、上游 URL、token_id 等），序列化 ≤8KB；超限按三级策略裁剪（受保护参数优先 → 单字段超限换省略标记 → 余下按优先级丢）并把被裁/被摘要字段名记入 `dropped` / `summarized`，**不得因超限整体丢失**；
  2. `req_messages` 完整不截断（含首轮/新会话；Responses `input` 形状同样可取）；
  3. 写入点统一在网关组包处（候选级快照含实际上游协议与上游 URL），单一写入漏斗落库，不在多个 handler 重复写；
  4. 网关错误响应体**顶层**带 `request_id`，响应头 `x-request-id` 保留不变；
  5. 账单列表支持按 `request_id` 搜索；一次请求的多条账单归并为一条主行（`Attempts` 标记，可展开子记录），列表 `Total` 语义为**请求数**；
  6. 开发者端在 **DTO 层**脱敏：渠道密钥（id/名）、内部模型 ID 与映射、成本金额一律不返回；管理端口径不变；开发者端按字段白名单返回，白名单外一律不返回；
  7. 前端账单详情页展示上述全部内容（增量消息、请求参数、决策轨迹、上游返回、用量、错误信息），会话详情复用同一视图，移动端适配。
- **约束**：
  - 串行开工，同一时刻只有一人动手；前端与后端不并行改同一批文件；
  - 改动走特性分支 + PR，**不合入 main**（合入权在 spencer）；
  - 不改 400 / usage 相关行为，不重构无关网关逻辑，不动 `.git` 结构（禁止 `git worktree` 命令）。
- **状态**：进行中
- **负责人**：Core（后端，已完成，PR #63）→ View（前端）→ Gate（验收）→ Owl（评审）
- **交付记录**：
  - 后端：分支 `feat/billing-detail-request-trace`，PR #63（OPEN，等 spencer 合入）；实现笔记 `notes/billing-detail-reqparams.md`；
  - 已定口径：列表 `Total` = 请求数（前端文案同步）；开发者端保留 `channel_name` 显示名，但必须按白名单返回。

---

## [P1] 渠道预设包 · 5 个代码执法点待拍板（2026-10-28）

- **背景**：`docs/preset-pack-format.md` v0.2（PR #64）已把数据集的字段口径写死，但其中「缺段 / 拼错键 / 数值填美元 / 误挂价格源」四类错在**同步阶段完全没有声音**（S2、S5、S8）。Core 只读核实后点名 5 个执法点；这份清单只登记，**今晚不写代码**，等 spencer 拍板后再排期。
- **验收标准**（每条独立可验收）：
  1. `channel/service.go:359-385` 的 `materializeFromProtocol` 在落库前调用 `billing.ValidateRates`，失败即 `return error` —— 因 `channelprotocol/service.go` 的 `Sync` 已在 `store.WithTx` 内，可整包回滚；效果：S2/S3 从静默升为同步失败。
  2. `channelprotocol/source.go:36` 的 `GatewayBlock` 与 `channelprotocol/model.go:97` 的 `ModelGateway` 补 `costCurrency` 字段（`json:"costCurrency"`，字符串），并在 `toChannelProtocol` 校验空值 / 非 `"CNY"`；效果：`costCurrency` 由纯标注变为执法点。
  3. `billing/rates.go:93` 的 `WireRates.UnmarshalJSON` **只在预设解析入口**对未知键加严（拒绝）；**不得**改动管理端既有 Pascal/snake 双向兼容；效果：拼错键（`Cache_Read` / `inputTokens`）立刻报错。
  4. `billing/service.go:300-302` 的 `cerr`（`ComputeCredits` 成本侧错误被 `err == nil` 吞掉）补一条 warn 日志，带 model / channel；效果：成本积分静默为 0 可被发现。
  5. 文档口径四条（V8 / S2 / S5 / S8 与 9.1「数值必须是人民币字面量」）已写进 `docs/preset-pack-format.md` v0.2，作为兰台冻结 Schema 的依据；本条无需改代码，仅确认口径。
- **约束**：
  - 纯后端改动，走特性分支 + PR；**不合入 main**（合入权在 spencer）。
  - 第 3 条**只能**在预设解析入口加严，改动 `UnmarshalJSON` 本身会波及管理端与历史数据兼容，属回归高风险。
  - 不改计费公式、不动 `billing.r` / `billing.cny_rate` 语义。
- **状态**：待确认（需 spencer 拍板；今晚不写代码）
- **负责人**：待定（Core 排期；口径由 Ark 维护）
- **交付记录**：
  - 口径正本：`docs/preset-pack-format.md` v0.2 §11.1 / §11.2 / §11.3，PR #64；
  - 执法点编号对齐：E1≈第1条、E2≈第2条、E3≈第3条、E4≈第4条、E5≈第5条（文档）、E6=附带建议（无协议模型 `continue` 加 warn）。

---

## [P1] 步骤 3 验收回写的 3 个代码执法点（2026-10-28）

- **背景**：Gate 在 `lingshu_test` 跑完步骤 3 拉取验收（通过：`sync {"confirm":true}` → 200 / Applied=40 / Pending=[] / Stale=0，二次 sync 幂等，`costRates` 与包逐位一致、无二次换算、无静默 0）。验收同时暴露 3 个代码侧问题，均为**只登记不修**；涉及 Go 改动，**今晚不写代码**，等 spencer 拍板。证据：`notes/pack-step3-acceptance.md`。
- **验收标准**（可分别验收）：
  1. **P1 空包直接 400 拦**：`POST /api/v1/admin/channel-protocols/sync` 在包解析出 **0 个 provider** 时返回 400 + 可读文案，**不得**进入 Sync 事务。现状实测：空包 `{}` → HTTP 200 / Code=0 / Applied=0 / 40 条 ACTIVE 全翻 `STALE`（正本 §11.1 V9 已按实测改写），只有 `Pending` 里能看到痕迹——对管理员等于"静默事故"。
  2. **P1 `insertOn` 绕过校验（校验口子）**：新增 provider 走 `insertOn` 时不经过 `validate()`，带 `passthroughMode:"bogus"` 的**新条目能 200 落库**；只有**已存在条目**走 `overwriteFromPreset` 才会 400 并整包回滚。要求：新建与覆盖两条路径的校验口径一致（`channelprotocol/service.go` 的 Sync 分支 + `insertOn`）。
  3. **P2 `pack_url` 指向非包 JSON 的错误可读性**：现在 `latest.json` 这类清单页会回**裸 500「服务器内部错误」**（`channelprotocol/handler.go:49-56` 的兜底分支，因为 `fetchPack` 返回的是普通 error 而非 `APIError`）。状态码安全（数据不动），但看不出原因。建议：包成 400 + 原文（如「解析预设包 JSON 失败：期望 provider 映射，实际是清单页」）。
- **约束**：
  - 第 1 条会改变 `Sync` 对空包的语义（从「成功但翻 STALE」变「拒绝」），需确认**首启空包**场景（`existing` 为空时无害）仍可放行或一并拒绝。
  - 第 2 条修完要回归「已存在条目 400 回滚」不变（事务原子性不能退化）。
  - 第 3 条只改错误包装，不改 HTTP 状态语义之外的行为。
- **状态**：待确认（需 spencer 拍板；涉及代码改动，今晚不写代码）
- **负责人**：待定（Core 排期；口径由 Ark 维护）
- **交付记录**：
  - 正本口径：`docs/preset-pack-format.md` §11.1 V9（实测）、§0.1 F3 第 ④ 种形态；
  - 验收证据与全部细节：`notes/pack-step3-acceptance.md`（Gate 产出）。
