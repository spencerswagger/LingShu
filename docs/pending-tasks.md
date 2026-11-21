# 灵枢 pending-tasks 队列

> 队列惯例见团队手册 §1。格式：每条一个区块，字段 = 背景 / 验收标准 / 约束 / 状态 / 负责人；状态取值：待确认 / 已确认 / 进行中 / 已完成 / 已回收。
> **本文件自 2026-11-20 起随分支维护**（Owl II 对 PR #171 的 P2 建议：设计依据须随分支可见；此前「本地暂存区、不进 git」口径作废）。
> 设计依据：`magpie-key-naming-agent-id` 与 `modelsdev-v2-billing-plan` 两份报告（lingshu-preview.pages.dev，2026-11-20 已定案版）。报告与本文冲突时，以本文定案口径为准。
> 白名单终表与接口契约定稿（2026-11-20 spencer 批准后落定）：`docs/designs/magpie-whitelist-final-table.md`、`docs/designs/magpie-passthrough-contract-final.md`；与本文冲突时，以本文为准。

## [P0] ① key 改名 = 用户名-令牌名 + 存量令牌回填迁移（2026-11-20）

- 背景：spencer 定案①②（原话摘要：「key 名 = 用户名-令牌名，如 张三-办公室台式机；重名时在 key 尾部追加序号消歧；存量令牌一并回填改名」）。现状 `ls-token-<id>` 分不出是谁的（报告 §2.1 代码证据）。
- 负责人：Core（后端主责）+ View（令牌列表展示列）+ Gate（验收）；Ark 设计已定稿不再改。
- 实施步骤（报告 §2.3 M1–M4 原样执行）：
  1. M1 代码期：`keyNameFor(username, displayName, tokenID)`（ASCII 转写 + 尾号消歧，规则=报告 §2.2 表）；`Ensure/Remove` 扩签名带 username + displayName；`magpie_keys` 加 `legacy_key_name TEXT`（纯 ADD 迁移，编号顺现行链取未用号）。
  2. M2 回填期：`POST /api/v1/admin/tokens/magpie-keys/rename`（dry-run 默认开启）；逐令牌 `Ensure(新名,同一明文)` → `Provision` → `DeleteCallerKey(旧名)`，失败停在中间态可幂等重跑。
  3. M3 对账期：`poller.settleOne` 反查 `key_name` 失败后回退 `legacy_key_name`（救改名窗口前的账本行）。
  4. M4 收尾：旧名全部删除、报表口径统一按新名。
- 验收标准（可量化）：
  1. 新建令牌 key_name 100% 命中「用户名转写-令牌名转写」格式；纯中文退化为 ID 段/`w-`+4hex（各有单测）。
  2. 同用户重名令牌追加尾号 `-2`；跨用户撞名消歧成功；UNIQUE 409 次数 = 0。
  3. 存量令牌回填成功率 100%；回滚演练通过（`legacy_key_name` 可复原旧名）。
  4. M3 上线后改名窗口前未结算账本行清零（改名前后「未结算行数」对比相等）。
  5. 调用方无感：改名前后同一令牌请求均 200，密钥值不变。
  6. 门禁四步全绿（gofmt / vet / race 单测 / vue-tsc）。
- 约束：不许动 tokens 表结构；不许用 rotate-key 换值；新旧名不得同时 ACTIVE；账本历史行不重算；迁移零 DROP。
- 状态：已确认（spencer 2026-11-20 确认设计，本日转任务书）
- 前置依赖：无。

## [P0] ② nginx 白名单收紧：只放推理四协议 + /v1/models + Codex WS（2026-11-20）

- 背景：定案③（原话摘要：「nginx 白名单只放推理四协议 + /v1/models + Codex WS，health 仅内网，其余默认拒绝」）。安全缺口：现网 `/v1/` 前缀整段转发，`/v1/magpie/*` 管理面暴露——任一租户令牌可读全站聚合用量（报告 §4.5）。
- 负责人：spencer/运维（nginx 属宿主机变更）+ Gate（冒烟验收）。
- 实施步骤（草案照报告 §4.7 执行）：
  1. 落地白名单 6 类点位：OpenAI chat/completions、OpenAI responses（HTTP/SSE + WS，补 Upgrade/Connection 头）、Anthropic messages(+count_tokens)、Gemini generateContent/streamGenerateContent/countTokens、/v1/models；白名单外 `/v1/*` 一律 404。
  2. health 仅内网：`location = /healthz` 反代灵枢后端，不在 /v1/ 前缀内，确认不进对外白名单、维持现状。
  3. 低峰灰度生效，观察 1 个低峰窗再转常态。
- 验收标准（可量化）：
  1. 白名单 6 类点位四协议冒烟全 200 / WS 握手 101（含 Codex 实连）。
  2. 白名单外抽样全部 404：`/v1/magpie/usage`、`/v1/magpie/admin/*`、`/v1/embeddings`、`/v1/completions`。
  3. `/healthz` 外网不可达（404/403）、内网正常。
  4. 四家官方 SDK 初始化各跑一次零报错。
- 约束：`MAGPIE_TRUST_PROXY` 保持不设；Host 头不改内部名；不装 headers-more 模块；沙盒不动手，变更在宿主机执行。
- 状态：已确认
- 前置依赖：无硬依赖；建议排在①之后，避免入口与账本归属同时变更难归因。

## [P0] ③ 响应头剥除：X-Magpie-* 全剥 + 品牌/版本头（2026-11-20）

- 背景：定案④（原话摘要：「剥除 X-Magpie-* 与一切品牌/版本响应头（Server 等）」）。动机：防外部按 magpie 已知漏洞对灵枢入口定向攻击（报告 §4.4）。
- 负责人：spencer/运维（与②同一 nginx 变更窗执行）+ Gate（验收断言）。
- 实施步骤：
  1. `proxy_hide_header` 枚举全量 `X-Magpie-*`（实现时从 magpie 源码 + 抓包核全清单，现网已知 7+ 个）。
  2. `Server` / `X-Powered-By` 维持剥除（server_tokens off 已生效）；供应商身份类响应头按 F8 名单剥（保留 `x-ratelimit-*` / `Retry-After` 等通用头）。
  3. 429 提示兜底：`X-Magpie-Limit-Reset` 剥除后由 nginx `add_header Retry-After` 补（或接受无提示，实现时定）。
- 验收标准（可量化）：
  1. 对外响应头零「magpie」字样（大小写不敏感全量断言）。
  2. `Server` 头不含版本号。
  3. SSE 流式响应不受剥头影响（四协议流式冒烟正常）。
  4. 抓包核对的剥头清单归档进报告附件。
- 约束：不动请求头透传（Authorization / UA / anthropic-beta 等 P0 清单=报告 §4.2）；错误体 sub_filter 不做（Content-Length/SSE 风险）。
- 状态：已确认
- 前置依赖：与②同窗合一次变更（同一 nginx 文件、一次 reload）。

## [P1] ④ 计费四项落地（modelsdev-v2 方案）（2026-11-20）

- 背景：定案⑤，计费按报告四项执行：① cache_write 启用、四价（input/output/cache_read/cache_write）与 magpie 逐档对齐、token 分桶互斥（OpenAI 类自动写、Anthropic 类请求内 cache_control 触发）；② service_tier 差价不在售卖层做（magpie 不感知该字段）；③ web_search 不做独立计价段（预留 MCP/工具调用）；④ multipliers 按模型区分、不开放用户组层。
- 负责人：Core（后端主责）+ View（计费配置页）+ Gate（守恒/对账测试）。
- 实施步骤（报告 §5 三步走）：
  1. S1 落库：迁移 ≥0028（纯 ADD：ext_meta/cost_v2/sale_v2/price_eval 4 列 + catalog_models/catalog_match 2 表）；usage 拆段先行（`billing_records.tokens` 增 cache_write 键，历史行缺键按 0、不重算）；Resolver 求值器 + 60min 同步器换 v2 URL（只落暂存区）。
  2. S2 双读：预扣 EstimateBreakdown 与 v3 结算 settleOne 切 Resolver（预扣不含 cache_write 段，结算笔笔计）；对账告警连续 3 窗零误报才进 S3。
  3. S3 切换：新计价正式生效，旧列只读归档；ApplyCatalog（diff 预览→人工确认→写入）替代一键 ApplyPrice。
- 验收标准（可量化）：
  1. token 分桶互斥：同一 token 只落 input/cache_read/cache_write 之一（新增单测断言）。
  2. 成本对账：cache_write 段与 magpie 账本逐档一致（60s 窗差异 = 0）。
  3. 余额守恒：切换窗口前后用户积分差 ≤ 0.01。
  4. 回归：service_tier 不同的请求售价不变；web_search 调用不计费、元数据照存；multipliers 无用户组配置入口。
  5. 迁移在 dev/test/staging 三库演练通过、编号无撞号（对照 R8 占用清单）。
- 约束：全链路无 float（BIGINT 微元 + INTEGER ppm）；magpie 代码不改；计费字段永不自动覆盖；0011 历史行不动。
- 状态：已确认
- 前置依赖：S1 可先行；S3 切换排在②③稳定之后，且避开①的回填窗。

## [P0] ⑤ 白名单终表 + 接口契约定稿落地执行（2026-11-20，spencer 已批准「系统配置改造」设计）

- 背景：spencer 2026-11-20 09:30 批准开工（原话摘要：「设计通过，批准按任务书开工；Ark 出白名单终表（含 count_tokens 两点）+ 契约定稿后合成正式任务书；实施按 Core 排期串行推进」）。终表与契约已定稿：`docs/designs/magpie-whitelist-final-table.md`（24 条点位 + count_tokens 两点 + legacy_key_name 反查）、`docs/designs/magpie-passthrough-contract-final.md`（分层分工：nginx 拦路径/剥头/WS 升级，应用管鉴权/预扣/`?key=` 剔除）。
- 负责人：Core（串行排期：命名+存量回填（legacy_key_name 反查）→ nginx 白名单+剥头 → 前端展示）+ Gate（每批证据）+ spencer（合入点头）。
- 实施步骤（按批准排期串行，三批）：
  1. 第 1 批 = 任务①（M1–M4 命名+回填，反查按终表 §3：先 key_name 后 legacy_key_name，都查不到才跳过；rename 端点 dry-run 默认开启）。
  2. 第 2 批 = 任务②+③ 同一 nginx 变更窗（白名单 6 类 + count_tokens×2 按 §1.1 location 正则 + Upgrade/Connection 头 + proxy_hide_header 枚举剥头）。
  3. 第 3 批 = 前端展示（逻辑名列 + 尾号淡显/tooltip，窄宽截断按报告 §2.2 示例）。
- 验收标准（可量化，逐条出自终表 §4）：
  1. 每批完成出证据（接口实测/日志/截图），缺证据不进下一批；四 SDK 冒烟零报错 + Codex WS 101。
  2. count_tokens 两点实测 200 且不产生账本行（前后 ledger_entries 计数不变）。
  3. 白名单外抽样 404（/v1/magpie/usage、/v1/embeddings、/v1/completions）；/healthz 外网不可达内网正常；对外响应头零 magpie 字样。
  4. M3 后改名窗口前未结算行清零（前后「未结算行数」对比相等）。
- 约束：MR 挂 Owl II 评审、spencer 点头才合入；429/5xx 指数退避 5s→20s→60s 最多 3 次；nginx 变更宿主机执行、沙盒不动手；本条登记本身只动 docs/。
- 状态：已确认
- 前置依赖：任务①②③ 为其实施载体，本条是排期与验收总纲；与①②③冲突时以本条排期为准。

## 建议排期顺序（按优先级与人力，不承诺到小时）

1. **① key 改名**（第 1 单开工，即 ⑤ 的第 1 批）：纯代码不依赖运维窗口；Core 主力，View 跟展示列。
2. **②+③ nginx 白名单 + 剥头**（第 2 步，即 ⑤ 的第 2 批）：合并为同一次宿主机变更窗；spencer/运维执行，Gate 冒烟。
3. **前端展示列**（⑤ 的第 3 批，随①收尾）。
4. **④ 计费四项**（第 3 步）：S1 落库可与①并行启动；S3 切换最后做，前置 ②③ 稳定 + 三库演练通过。

---

# 历史条目（2026-10-26 ~ 10-30 · 合并入仓前仓库既有，原样保留）

> 以下条目为合并前的仓库版本原样快照，状态与交付记录未随合并刷新；「进行中 / 待确认」项由主持人按需收口。

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

## [P0] 渠道预设包数据集（2026-10-28）

- **背景**：spencer「灵枢渠道协议本来就设计了拉取功能，但一直没提供数据集。本次仅做测试，**不改灵枢代码和部署**」。范围：① 冻结数据集字段口径（schema / 币种 / 优惠字段 / 验收清单）→ ③ 测试环境拉取验收。
- **验收标准**：数据集能被 `preset.pack_url` 拉取并物化为 40 条渠道协议；`costRates` 为人民币字面量、无二次换算、无静默 0；空包、清单页、302 指针等边界形态有明确结论。
- **约束**：不改灵枢代码、不部署、不调灵枢库/网关真实数据；拒绝猜测，缺数据标「待定」。
- **状态**：已完成（2026-10-28）
- **负责人**：Ark（口径与正本）→ 兰台（数据集发布）→ Gate（步骤 3 验收）
- **交付记录**：
  - **步骤 1 口径**：`docs/preset-pack-format.md` v0.2（含 §0.1 冻结前 5 项规范）+ `docs/preset-pack-format.schema.json`（ajv 2020-12 用例通过），PR #64 **@ 0b2e1f4**（OPEN，待 Owl 评审、合入权在 spencer）。
  - **步骤 3 验收**：**通过** —— `Applied=40 / Pending=[] / Stale=0`、`STALE=0`、重复各 0、`costRates` 与包差值 0、`latest.json` 走 302 可用；空包 `{}` 实测会把 40 条全翻 STALE。收口记录 `notes/p0-dataset-closeout-2026-10-28.md`。
  - **遗留（均为代码改动，已另立待确认）**：空包改 400 拦、`insertOn` 绕过校验的口子、清单页裸 500 包成 400 —— 见本文件下方两段 P1。

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

---

## [P1] 统计页 v4（v6→v4 方案）（2026-10-25 定稿，2026-10-30 补录）

- **背景**：spencer 已确认 v6→v4 改版；后端能力（小时聚合、前 3 天同时段、实时会话/在线用户、模型广场聚合、会话已花积分）已在 main（PR #61 已合入），实时区字段补充在 PR #66、前端在 PR #67，两者均在等 Owl。
- **验收标准**：
  1. 删「运维概览」块；首屏 3 大数字 + 一排小指标（不做成本/毛利）。
  2. 中部上下两张图：用量趋势（今天/昨天/前天/大前天四线，按小时，缺数据断线）+ 模型广场（Token/吞吐/请求失败/首字全程四指标，可排序切换，空模型折叠）。
  3. 底部实时会话：30s 自动刷新、**会话准入仅「请求中」**（`Inflight > 0`）、用户只做汇总；同一时刻只允许一个在飞请求。计数口径（**两套，不得互换**）：**`SessionsTotal` / `Truncated` / `UsersTotal` 严格按 `Inflight > 0`**（`UsersTotal` = 有 inflight 会话的用户数，即「请求中用户」）；**`RangeUsersTotal` = 所选范围内有过请求的用户数**（即「范围内活跃用户」，与范围用户行 `RangeUsers[]`（原 `Users[].Range*`）同源，**不是** inflight 口径）。**淡出过渡不得改变上述任何计数**。前端两个数字分别按「请求中用户」/「范围内活跃用户」标注，**不得同名、不得合并展示**。
  4. 窗口标签显性化（读 KPI.WindowFrom/WindowTo/WindowMode，前端不自拼）。
  5. 移动端 ≤640px 单列。
  6. 接口向后兼容：不传新参数时响应与 origin/main 逐字节一致（golden 快照固定）。
- **✅ 两个用户数口径已定稿（Owl P2 对齐 #69，2026-10-30）**：
  - **请求中用户 = `UsersTotal`** —— 有 inflight 会话的用户数（#69 已恢复为 inflight 口径，已 READY）。
  - **范围内活跃用户 = `RangeUsersTotal`** —— 所选范围内有过请求的用户数，与范围用户行 `RangeUsers[]`（原 `Users[].Range*`）同源，**不是** inflight 口径。
  - **两者不同源（一个是 inflight 会话、一个是范围内请求记录），不得互相换算，也不得同名展示**；首屏若同时展示，标签必须分别写「请求中用户」与「范围内活跃用户」。
- **约束**：不改统计口径定义、不动状态页（ServiceStatus）、不改计费/路由决策逻辑、不引入新依赖。
- **状态**：进行中（等 Owl：PR #66 / #67）
- **负责人**：Core（后端）/ View（前端）/ Gate（验收）
- **口径变更记录**：2026-10-30 Owl P2 —— 原写「`UsersTotal` = 范围内有过请求的用户数」与 #69 相反，已改为「`UsersTotal` = inflight 用户数（= 请求中用户）」，范围口径改名 `RangeUsersTotal`；原「待 Core 确认字段来源」一条随之作废并改为定稿结论。
- **口径变更记录（二次校准，2026-10-30）**：Owl P2 措辞收口 —— 范围口径字段名统一写 `RangeUsersTotal` / 范围用户行 `RangeUsers[]`（注原 `Users[].Range*`）；「两套口径」改为「**不同源**，不得互相换算」；淡出条补明 `UsersTotal` 为 inflight 口径、`RangeUsersTotal` 与淡出无关；全文已无「`UsersTotal` = 范围内用户数」的残留。
- **✅ 已拍板（spencer，2026-10-30）：实时会话口径定稿**
  - **准入 = 仅「请求中」**（`Inflight > 0`），与 main 现状、原派工一致。
  - 「最近 5 分钟活跃」**只作降级/淡出过渡**（请求结束后短暂停留再消失，前端视觉过渡），**不作准入条件**。
  - 淡出项**不计入** `SessionsTotal` / `UsersTotal` / `Truncated`（`UsersTotal` 自 #69 起为 **inflight 口径**，淡出项不得抬高它；`RangeUsersTotal` 由范围内请求记录决定，与淡出无关）；若要给淡出提供数据，必须用**独立字段**（如 `Fading` / `RecentlyExited[]`），不得污染统计口径。
  - 实现口径：Core 按此改 PR #66（原 `Inflight > 0 || LastActive ≥ now-5min` 的准入写法作废）。
