# 灵枢 V2 阶段一 · 令牌模型范围与会话归属定稿（替代 `tokens.tag_id` / `sessions.channel_key_id`）

> 2026-11-15 · Ark（架构）· 依据 `docs/tasks/v2-2026-11-15.md` 与
> `docs/designs/2026-11-15-v2-p1-data-contract.md`（0019–0023 已定稿并实测）。
> 本文只做**设计定稿**：迁移文件尚未落地（编号 **0024**，等派工）。
> 未建 PR、未推 main。
> **唯一来源**：本文与 0019–0023 迁移的唯一来源是分支 `docs/lingshu-v2-p1-data-contract`；
> 其它分支里的同名/同类副本视为过期，合并时丢弃。

## 0 一句话

V2 的灵枢**不再做路由**，所以这两列不能「再找一列存密钥」式替代，而要把两件事拆开：
**令牌的标签只表达「可调用哪些外部模型」（准入），会话不再记密钥、归属一律由账单行表达。**

## 1 现状：这两列各自承担什么（证据）

| 列 | 真实用途 | 证据 |
|---|---|---|
| `tokens.tag_id` → `semantic_tags.kv_pairs` | **只用于路由准入**：路由把令牌的 KV 标签与渠道绑定的标签做 `StrictContains` 匹配，命中才进候选（渠道密钥 + 内部模型） | `router/engine.go:427-431`；解析入口 `gateway/handler.go:290`（`g.tagKV(ctx, token.TagID)`） |
| `sessions.channel_key_id` | ①**会话亲和**：候选集内按「密钥 + 内部模型」精确匹配把原候选提到首位 ②会话列表展示 ③按密钥批量踢会话 | ①`router/engine.go:468` ②`console/admin.go:1126`（`sessionListCols`）③`channel/handler.go:677`（`KillByFilter(channel_key_id=keyID)`） |

两条反过来也成立，正是「不能只删」的原因：

- **统计不依赖 `tag_id`**：`billing/stats.go`、`billing/query.go` 里没有任何 tag 命中 ⇒ 删 tag 不改统计口径，改的是**准入行为**。
- **`sessions.channel_key_id` 不参与统计聚合**：它只做亲和、列表展示、踢人 ⇒ 删它不影响「按会话聚合」的数字，影响的是**每个会话还能不能定位到归属**。

⇒ 替代方案必须分别回答：**准入用什么表达**（§2）、**归属记在哪**（§3）。

## 2 令牌级模型范围（准入）

### 2.1 结论：一列 + 一张表，编号 0024（DDL 草案）

```sql
-- ① 显式模式：ALL=不限（与旧行为一致）；LIST=只允许 token_models 里列出的外部模型
ALTER TABLE tokens ADD COLUMN IF NOT EXISTS model_scope TEXT NOT NULL DEFAULT 'ALL'
    CHECK (model_scope IN ('ALL','LIST'));

-- ② 一令牌多模型：多行即多模型
CREATE TABLE IF NOT EXISTS token_models (
    token_id          BIGINT NOT NULL REFERENCES tokens(id) ON DELETE CASCADE,
    external_model_id BIGINT NOT NULL REFERENCES external_models(id) ON DELETE CASCADE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (token_id, external_model_id)
);
CREATE INDEX IF NOT EXISTS idx_token_models_model ON token_models(external_model_id);
```

| 对象 | 类型 | 可空 | 默认 | 约束 / 索引 |
|---|---|---|---|---|
| `tokens.model_scope` | `TEXT` | NOT NULL | `'ALL'` | `CHECK IN ('ALL','LIST')`（inline，随 `ADD COLUMN` 一次性创建，幂等） |
| `token_models.token_id` | `BIGINT` | NOT NULL | — | FK `tokens(id) ON DELETE CASCADE`；`PK(token_id, external_model_id)` |
| `token_models.external_model_id` | `BIGINT` | NOT NULL | — | FK `external_models(id) ON DELETE CASCADE`；反向索引 `idx_token_models_model` |
| `token_models.created_at` | `TIMESTAMPTZ` | NOT NULL | `now()` | — |

**「一令牌多模型」的表达**：`token_models` 里该 `token_id` 有几行就是几个模型，`PK(token_id, external_model_id)` 保证不重复；`external_model_id` 单列索引支撑「某个模型被哪些令牌授权」的反查（下架前查影响面）。

### 2.2 语义（三条，写死）

1. `model_scope = 'ALL'`：**不读 `token_models`**（表里有行也视为脏数据，准入忽略；留一条诊断 SQL 供巡检）。
2. `model_scope = 'LIST'`：只允许 `token_models` 里列出的 `external_model_id`，并且该模型仍须满足 `external_models.enabled = true` 与 `bound_internal_ref IS NOT NULL`（下架/未绑定即自动失效，不需要同步清表）。
3. `LIST` + 空表 = **一个模型都不能调**（fail-closed）。这是刻意的：如果「空=不限」，那么删掉最后一行授权会静默把权限放大成全部——这类 fail-open 在计费系统里等于送钱。

判定顺序（不写代码，只定顺序）：入站 `model` → 别名解析到 `external_models` 行 → ①`enabled` 且已绑定 ②按 `model_scope` 判白名单 → 不通过返回 403 `model_not_allowed`（错误体按入站协议形状）。**判定全部在灵枢侧，网关不需要知道令牌存在**。

### 2.3 为什么不是另外两种做法

- **不建 `model_scope`，用「有行=LIST / 无行=ALL」**：表达不了「一个都不许」，且删最后一行的权限放大无人察觉（见 2.2 第 3 条）。这是被否掉的主因。
- **按内部模型 / 组授权**（`internal_ref` 而非 `external_model_id`）：组名与直通名是**上游命名**，属于灵枢不该暴露给租户的运维细节（本轮验收还要求界面不出现上游模型名）；而且授权应挂在**售卖面**（外部模型），这样「同一个外部模型改绑到另一个内部模型」时令牌权限不需要重配。

### 2.4 需负责人点头（对外契约变更）

新增 `tokens.model_scope` 与 `token_models` 会改变**令牌 API 的字段面**（管理端令牌页要能选模型、开发者端只读展示）。两条路：

| 选项 | 代价 | 结论 |
|---|---|---|
| A（推荐） | 令牌 API 增 `model_scope` + `model_ids`（只增字段，不改既有字段语义） | 保住旧「按令牌限模型」的能力 |
| B | 全部令牌固定 `ALL`，`token_models` 延后建 | 零契约变更，但**能力净损失**：旧 tag 能限制的，V2 全放开 |

## 3 会话归属

### 3.1 结论

- `sessions` **不补** `upstream_provider` / `upstream_account` 列。会话归属的唯一真源 = `billing_records`（每次调用一行，已带 `upstream_*`，见 0020）。
- `sessions.internal_model_id` **保留**，但语义降级为「**绑定结果的快照**」：V2 里外部模型 `bound_internal_ref` 是确定映射，不是路由选择，所以它可推导；保留只为会话列表按模型筛选与历史可读。
- **禁止**用 `sessions` 做任何按上游/渠道的聚合。

### 3.2 为什么不在会话上记上游

1. **二义性**：旧模型允许会话中途换密钥（`EnsureSession` 的 `ON CONFLICT DO UPDATE SET channel_key_id=...`），所以「一个会话一个归属」在旧模型里就是伪命题；V2 换由网关做亲和/切换，灵枢更看不到全过程。会话跨 provider 是常态，单列快照必然误导。
2. **展示约束**：本轮要求界面不得出现上游 provider / 渠道 / 密钥 / 上游模型名。记了也只能烂在库里。
3. **成本可控**：`billing_records` 已有 `idx_billing_session(session_id)`（0001），按会话聚合是索引扫描，不需要冗余列。

### 3.3 会话维度统计怎么不产生二义

- 会话的**计量**（tokens/积分/成本）= `billing_records WHERE session_id = ?` 聚合；
- 会话的**实体信息**（名称、状态、创建/活跃/过期时间、绑定模型）= `sessions`；
- 两者用 `session_id` 关联，**不允许**用 `sessions` 里的任何列做归属聚合。
- 若产品确实要「这个会话当前走哪个上游」：加两列 `last_upstream_provider` / `last_upstream_account`，语义写死为「**最近一次成功请求**的快照」，仅管理端可见、不进开发者端 —— 默认不做，见 §8 第 3 条。

## 4 落位与顺序

**结论：新开 `0024_v2_token_scope.sql`，不改 0019–0023 任何一行。**

理由三条：

1. **改已执行过的迁移文件会踩我们刚写进手册的坑**：`migrate.go` 以**文件名**做幂等键，宿主/演练库一旦记了 `0019_v2_topology.sql`，改内容**永远不会重跑**，新列静默缺失且不报错（实测已证：旧分支库上五个文件全 exit 0、记录记到 0023、库缺列）。0019–0023 都已在演练库执行过，所以它们必须冻结。
2. **0021 只删不改结构**：它删的是 `channels`/`channel_keys`/`tag_id`/`channel_key_id`，而 0024 的新对象只引用 `tokens` 与 `external_models` —— 二者 0021 都不动，因此 **0024 与 0021 无依赖**。
3. **顺序敏感点确实存在，但本次不命中**：0021 按文件名字典序排第 3，任何「0021 执行时必须已存在」的对象只能塞进 0019/0020。本次没有任何这种依赖（不存在「0021 要引用的新列」），所以放 0024（第 6 位）是安全的。

若将来出现「0021 之前必须存在」的对象，正确做法只有两条：回填进 0019（前提是该库**从未**执行过 0019，需先探测）或另开一个 `0019b` 式新名字——**不允许**改文件内容。

## 5 统计口径（那 5 个失败用例按什么聚合）

| 维度 | 数据来源 | 展示位置 | 说明 |
|---|---|---|---|
| **主维度：内部模型（组/直通）** | `billing_records.internal_model_id` | 统计页、成本页 | 渠道维度消失后的自然继承者：灵枢自己拥有、稳定、与「内部模型→外部模型」绑定一一对应 |
| 成本下钻：上游 provider | `billing_records.upstream_provider` | 仅管理端 | 0020 新列；用于盈亏与对账，不进开发者端 |
| 成本下钻：上游账号 | `billing_records.upstream_account` | 仅管理端 | 同上；订阅摊销按它归集 |
| 协议 | `billing_records.upstream_protocol` | 筛选 / 分布 | 四值枚举；不做主分组（维度太粗，做分组会把图表切碎） |
| 会话 | `billing_records.session_id` | 会话详情 / 成本轨迹 | 按会话聚合只读账单（§3.3） |
| 渠道名 / 渠道密钥名 | —— | **下线** | 无新真源；历史一次性查询走 `call_logs` 的 1:1 快照（Core 已改） |

口径要点：

- **同一图表不许混用两套归属**：内部模型维度与上游 provider 维度是**两个不同的切面**，不要在一张表里并列求和（内部模型是售卖视角，provider 是成本视角，二者不构成同一层级的细分）。
- 5 个失败用例的期望结果：`GROUP BY internal_model_id` 后**行数不变或变少且可解释**（同一内部模型下多个渠道密钥的行会合并——这正是 V2 想要的），成本页的总额不变。
- 历史账单（0021 之前）没有 `upstream_*` 值：报表必须按「NULL = 未知」处理，不许 `COALESCE(...,'')` 造出空标签。

## 6 回滚与兼容

| 场景 | 结论 |
|---|---|
| 宿主已执行 0021，再补 0024 | **安全**。0024 是纯 ADD，与被删对象无依赖；不需要回到 0021 之前 |
| 回滚 0024 | `DROP TABLE token_models` + `DROP COLUMN tokens.model_scope`（inline CHECK 随列删除）。无数据损失：授权是配置，不是账本 |
| 数据回填 | **不做**。旧 tag 的 KV（如 `region=cn`）与「外部模型白名单」不是同一枚举，映射只能靠猜；统一 `model_scope='ALL'` = 保持「不限」，避免误杀线上调用。需要收紧的令牌由管理员在令牌页重配（迁移报告里列出原 tag 名便于对照） |
| 删模型 / 删令牌 | FK `ON DELETE CASCADE` 自动收回授权行 —— 这是期望行为（不留孤儿授权） |

## 7 映射表（旧列 → 新表达 → 迁移动作）

| 旧列/旧表 | 旧语义 | V2 新表达 | 迁移动作 |
|---|---|---|---|
| `tokens.tag_id` | 令牌 KV 标签，路由时与渠道标签匹配筛候选 | **删除**；等价物 = `tokens.model_scope` + `token_models`（模型白名单），**KV 匹配语义不保留** | 0021 `DROP COLUMN`（已定稿，未执行前先按手册做探测） |
| `semantic_tags` / `channel_tags` | 标签字典与渠道绑定 | 删除 | 0021 `DROP TABLE`（已定稿） |
| `channels` / `channel_keys` / `channel_models` | 路由实体 + 凭据 + 内部模型映射 | `internal_models`（镜像）+ `external_models.bound_internal_ref` | 0021 `DROP TABLE`；映射已由 0019 承接 |
| `sessions.channel_key_id` | 会话亲和 + 列表展示 + 踢人 | **不补列**；归属 = `billing_records.upstream_*`（按 `session_id` 关联） | 0021 `DROP COLUMN`（已定稿） |
| `billing_records.channel_key_id` | 账单的渠道密钥维度 | `upstream_provider` / `upstream_account` / `upstream_model` / `upstream_protocol` / `cost_source` | 0020 已加列；0021 `DROP COLUMN` |
| `call_logs.channel_id` / `channel_key_id` | 历史请求快照 | **保留不动**（唯一可回溯「当时走哪个渠道密钥」的地方） | 无（0021 明确不删这两列） |

## 8 待负责人点头（三条，均为范围/契约问题，不是技术阻塞）

1. **对外契约**：令牌 API 是否新增 `model_scope` + `model_ids`（§2.4 选项 A/B）。→ 决定 0024 是否落表。
2. **能力收缩**：旧 tag 的 KV 匹配（region/tier/…）在 V2 没有等价物，等价物只有「模型白名单」。确认这是可接受的收缩。
3. **会话「当前上游」展示**：默认不做（§3.2）；若产品要，加 `last_upstream_*` 两列并限定只读、仅管理端。

## 9 给 Gate 的断言建议（3 条）

1. `model_scope='LIST'` 且 `token_models` 空 → 任意模型请求 403 `model_not_allowed`（fail-closed，不许放行）。
2. `model_scope='ALL'` 且表里有脏行 → 请求照常放行（ALL 不读表）。
3. 同一窗口内「按会话聚合的 tokens/积分」= 「按账单聚合」逐字相等；「按 internal_model_id 聚合的成本总额」= 迁移前按渠道聚合的总额（渠道密钥合并后总额不变）。