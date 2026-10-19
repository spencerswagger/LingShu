---
design_type: phase
created_at: 2026-10-19
---

# 多协议互转（渠道协议体系 · 二期）

> Status: APPROVED
> Phase: 2 / 渠道协议体系
> Branch: feat/multi-protocol-conversion
> Author: agent
> Last updated: 2026-10-19
> Supersedes: 一期 AC-18（直通未命中即返回固定错误）——本设计「默认开启转换」取代之

## Intent Contract

```
intent: 让网关在入站协议与渠道实际支持的上游协议不一致时，对请求体、响应体与流式 SSE 事件做协议转换后转发；协议一致时保持零转换直通。
constraints:
  - 直通路径必须保持字节级透传，不实例化编解码器（一期 AC-1 零转换不回退）。
  - 计费三段口径与 usage 归一化不变；usage 始终由物理上游协议解析。
  - 直通判定优先于转换；转换仅在直通未命中时发生。
  - 不新增第 4 个基础协议；协议名与端点常量对齐现有 channelprotocol 常量。
success_criteria:
  - openai-chat / openai-responses / anthropic-messages 两两 6 个方向的请求体、响应体、流式事件均可转换。
  - 直通场景与一期逐字节一致；既有 go test ./... 全绿。
  - 无法在目标协议表达的字段被丢弃时，决策轨迹与调用日志带 lossy 明细。
risk_level: high
```

## Verification Contract

```
verify_steps:
  - run tests: cd backend && go test ./...
  - build: cd backend && go build ./... ; cd frontend && npm run build
  - check: 起后端 + 三个协议假上游，跑转换矩阵用例
  - confirm:
      1) 直通：入站协议 ∈ 渠道可达集 → 上游收到原样字节，响应原样回传；
      2) 转换非流式：入站 A、渠道仅支持 B → 上游收到合法 B 请求，返回体为标准 A 形状；
      3) 转换流式：上游 B 的 SSE 被实时改写为 A 的事件序列，usage 仍按 B 解析；
      4) 有损字段出现在决策轨迹 lossy 明细。
```

### Acceptance criteria

- AC-P2-1 直通不变：入站协议 ∈ 渠道可达集 → 上游请求体与下游响应体字节级不变（与一期一致）。
- AC-P2-2 非流式转换：入站 A、渠道仅支持 B → 上游收到合法 B 请求；下游收到合法 A 响应（含 usage 与结束原因映射）。
- AC-P2-3 流式转换：上游 B 的 SSE 事件流被改写为 A 的事件序列，顺序保真、无丢帧；usage 由 B 解析并归一三段。
- AC-P2-4 全矩阵：6 个方向（chat / responses / anthropic 两两）均有往返用例覆盖。
- AC-P2-5 目标选择：渠道支持多个上游协议时按固定优先序选定；渠道协议/模型覆盖列表优先于全局序。
- AC-P2-6 有损留痕：不可表达字段（如 anthropic thinking / cache_control → openai-chat）被丢弃时，决策轨迹与调用日志带 lossy 明细。
- AC-P2-7 关闭转换：渠道协议/模型 ConvertEnabled=false 时，直通未命中仍返回一期 AC-4 固定错误，不转换。
- AC-P2-8 计费口径：转换场景 usage 仍由物理上游协议解析，call_logs.upstream_usage 记物理协议名与原文。
- AC-P2-9 失败语义：入站请求解析失败 → 400；序列化失败 → 500；上游响应/流解析失败 → 沿用既有 502/流式中断分支。

## Governance Contract

```
approval_gates:
  - 路由语义变更（默认开启转换取代一期 AC-18）：已由用户确认「确认取代 AC-18」。
  - 流式转换 pump 接入热路径：属高风险路径，需人工评审。
rollback: 代码回滚到主线；配置仅 JSONB / sys_config 字段扩展，无破坏性 schema 变更，回滚无需数据修复。
ownership: agent 实现，用户验收。
```

## Scope

| In scope | Out of scope |
|---|---|
| 全矩阵 6 方向互转（经统一 IR） | 新增第 4 个基础协议 |
| 非流式 + 流式（SSE 事件实时改写）转换 | 错误响应体按入站协议改形（维持统一 OpenAI 错误体） |
| 直通优先、转换兜底、默认开启 | 转换的严格模式（strict：不可映射即失败） |
| 目标协议固定优先序 + 渠道/模型覆盖 | 按渠道健康度/成本动态择优目标 |
| 有损字段尽力而为 + 诊断留痕 | `/v1/completions`、`/v1/embeddings` 的协议语义扩展 |
| 渠道协议/模型级转换开关 + 目标白名单 | 多实例分布式配置刷新（沿用单实例内存模型） |

## Decisions

| # | decision | choice | rejected alternatives |
|---|---|---|---|
| D1 | 转换实现形态 | 统一中间表示 IR + 每协议「解析/序列化」编解码器（3 解析 + 3 序列化） | 6 个两两转换器（N² 重复、难维护）；经 openai-chat 单枢纽（双跳有损，且无法 anthropic↔responses 直转） |
| D2 | 触发条件 | 直通优先；未直通时按配置上游协议转换；默认开启 | 默认关闭、需显式开关才转换；无条件全自动（跳过直通判定） |
| D3 | 直通性能 | 协议相同走既有逐字节透传，不实例化编解码器 | 统一走 IR（给直通引入解析开销与行为变化） |
| D4 | 有损字段 | 尽力而为：核心字段严格映射，其余丢弃并记 lossy 明细 | 严格失败（400）；strict/lenient 双模式 |
| D5 | 目标协议选择 | 全局固定优先序 + 渠道协议/模型可覆盖有序列表 | 必填显式列表；健康度/成本动态择优 |
| D6 | usage/计费 | 仍由物理上游协议解析（三段口径不变）；upstream_usage 记物理协议原文 | 按入站协议解析（口径错） |
| D7 | 错误体形状 | 维持统一 OpenAI 错误体（与一期一致） | 按入站协议改形（扩大本期范围） |

## Surface

**新增转换包**：独立包承载 IR 与编解码器——IR 定义、三份协议编解码器（openai-chat / openai-responses / anthropic 各一份「请求解析 + 序列化 + 响应解析 + 序列化 + 流式事件编解码」）、转换编排与有损报告。

IR 形状（仅描述结构，不含实现）：
- `CanonicalRequest`：Model、System、Messages（role + 内容块列表）、Tools、ToolChoice、Temperature、TopP、MaxTokens、Stop、Stream、Metadata、Extra。
- 内容块 `ContentBlock`：text / image / tool_use / tool_result / thinking。
- `CanonicalResponse`：ID、Model、Role、内容块列表、FinishReason、Usage、Extra。
- `CanonicalEvent`（流式事件并集）：message_start（含 usage）、content_block_start、content_block_delta（text / thinking / tool_json 增量）、content_block_stop、message_delta（stop_reason + usage）、message_stop、error、ping。
- `LossyReport`：被丢弃字段与备注列表，随决策轨迹/调用日志落库。

**gateway 层**：候选循环从「直通判定」扩展为「先直通、未命中再按目标协议转换」——为每个候选求出「有效上游协议」（upstreamProto）与「是否转换」标记，并把两者带入上游请求构造上下文；非流式用上游 Provider 取到响应体后经 IR 回转为入站协议体再下发；流式新增 pump：把上游 SSE 解码为 canonical 事件、再用入站协议编解码器写回下游；直通沿用既有透传路径。

**配置面**：渠道协议的模型条目（JSONB models[]）与渠道模型追加「允许转换」开关与「目标协议优先序」覆盖列表；全局 sys_config 增总开关（默认开启）；目标优先序为固定常量（openai-chat → anthropic-messages → openai-responses）。

**前端**：渠道协议编辑器的模型清单增加「允许转换 / 目标协议优先序」控件，沿用现有控件范式（StringListEditor 等）。

**存储**：均为 JSONB / sys_config 字段扩展，无新增列语义变更，无破坏性迁移。

## Risks & Open Questions

| Risk / Open Question | Mitigation / 状态 |
|---|---|
| 默认开启使一期 AC-18 失效（模型仅声明 anthropic 时不再对其返回 400） | 已由用户确认取代；提供渠道协议/模型级 ConvertEnabled=false 回退到 AC-4 |
| IR 无法无损表达某协议核心语义 | 核心字段强映射 + 6 方向往返测试覆盖；不可表达字段走 lossy 明细 |
| 流式事件改写丢帧/顺序错乱 | 事件序号严格保序，增量文本直拼不做 JSON 解析；矩阵流式用例覆盖 |
| 转换失败与既有 retry 语义冲突 | 入站请求解析失败=400 不重试；序列化失败=500；上游响应/流解析失败沿用既有 502/中断分支 |
| 目标优先序不确定导致落点漂移 | 固定常量 + 可覆盖有序列表；决策轨迹记录入站/上游协议 |
| Open question：转换后的 assistant 消息（调用日志展示）是否按入站协议形状落库 | 本期按入站协议形状落库（与下游可见一致），待评审确认 |
| Open question：错误体是否需按入站协议改形 | 本期不改（维持统一 OpenAI 错误体，D7），留待后续 |