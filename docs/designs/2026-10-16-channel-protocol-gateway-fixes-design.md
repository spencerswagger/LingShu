---
design_type: feature
created_at: 2026-10-16
---

# 渠道协议配置网关修复（v0.5.1-rc1）

> Status: ALIGNED
> Branch: fix/channel-protocol-gateway-rc1
> Author: agent
> Last updated: 2026-10-16

## Intent Contract

```
intent: 修复渠道协议（channel_protocols）配置网关的 5 类可用性/一致性问题，使协议可编辑、可承载完整模型配置、可物化为渠道模型，并让请求头策略在协议变更后即时生效。
constraints:
  - 不破坏既有 openai-compat 主链路与既有单测（go test ./... 必须绿）。
  - 不改动计费公式本身；usage 口径已统一为三段（见 .claude/artifacts/designs/channel-protocol.md AC-16），本设计不涉及该口径变更。
  - channel_protocols 表结构与 segment 兼容：models JSONB 只做向后兼容的字段扩展。
  - 迁移单向新增，不改现有列语义。
  - 前端沿用现有 Element Plus + 组件范式（ModelPricing / ModelPriceCatalog / KvpEditor 等）。
success_criteria:
  - 渠道协议列表整行可点击进入编辑页（与渠道列表一致）。
  - 协议配置合并进「基本信息」首屏，输入框高度统一。
  - 请求头策略：单一名单列表 + 透传切换；none/all 隐藏名单；真实 HTTP/OpenAI/Anthropic header 候选；覆盖列表键有 header 下拉、值有变量下拉，高度统一。
  - 协议模型清单具备完整配置（对外模型绑定 / 成本 / 时段 / 分档 / 协议子集），支持从 /models 拉取（弹窗临时 Key）与 models.dev 参考价。
  - 引用协议的渠道自动物化出 channel_models；协议变更后引用渠道模型与运行时同步刷新。
  - 协议更新后，网关立即使用新的请求头策略（无重启、无渠道重存）。
risk_level: high   # 触及鉴权注入 / 请求头策略 / 运行时刷新等安全相关路径
```

## Verification Contract

```
verify_steps:
  - run tests: cd backend && go test ./...
  - build: cd backend && go build ./... ; cd frontend && npm run build
  - check: 起后端 + 假上游，脚本复现（/tmp/repro*.sh 思路）
  - confirm:
      1) 渠道协议列表 row-click 跳转 /admin/channel-protocols/{id}/edit；
      2) 创建协议（含模型+对外绑定）→ 创建渠道引用 → GET /admin/channels/{id}/models 非空；
      3) 更新协议请求头策略 → 不改渠道 → 再次网关请求，上游收到的是新策略头。
```

## Governance Contract

```
approval_gates:
  - 数据模型变更（ModelGateway 扩展字段）需评审（已由用户确认「自动物化为渠道模型」）。
  - 运行时刷新钩子接入 main.go 装配，属高风险路径，需人工评审。
rollback: 代码回滚到 main；迁移为 JSONB 字段扩展，无 schema 破坏性变更，回滚无需数据修复。
ownership: agent 实现，用户验收。
```

## Scope

| In scope | Out of scope |
|---|---|
| 渠道协议列表整行点击进入编辑 | 跨协议转换（二期） |
| 协议配置并入基本信息 Tab + 统一控件高度 | 预设包格式（docs/preset-pack-format.md）重定义 |
| 请求头策略 UI 重构（单名单/透传切换/真实 header 候选/覆盖下拉） | 响应头配置化（维持硬编码+全局白名单） |
| 协议模型清单完整配置 + /models 拉取 + models.dev 参考价 | 渠道级运行参数（限流/探测/可靠性）下沉到协议模型 |
| 协议模型自动物化为 channel_models + 协议变更刷新引用渠道 | 删除协议模型时级联清理渠道模型 |
| 协议 CRUD/Sync 后刷新引用渠道运行时（修复 5b） | 多实例分布式刷新（单实例内存模型，沿用现状） |

## Decisions

| # | decision | choice | rejected alternatives |
|---|---|---|---|
| D1 | 协议模型与渠道模型联动方式 | 渠道引用协议时**自动物化**为 channel_models；协议/渠道变更后自动刷新 | 手动「从协议导入」按钮；网关直读协议模型 |
| D2 | 协议模型拉取 /models 的凭据 | 拉取弹窗**临时输入 Key**（不落库） | 借用引用渠道密钥；匿名拉取 |
| D3 | 协议模型 JSON 字段 | 在 `ModelGateway` 上追加 externalModelID / costRates / timeConfig / contextTiers（camelCase，向后兼容） | 新建独立表；改预设格式 |
| D4 | 协议变更刷新粒度 | 全量 `chMgr.SyncFromDB` + 定向重物化引用渠道 | 仅重建单渠道（复杂，无必要） |
| D5 | 物化范围 | 仅物化 `externalModelID>0` 的协议模型行（channel_models.external_model_id NOT NULL） | 允许空绑定（需改列可空） |

## Surface

**Backend**
- `channelprotocol/model.go`：`ModelGateway` 扩展 `ExternalModelID int64 json:"externalModelID,string,omitempty"`、`CostRates billing.WireRates json:"costRates,omitempty"`、`TimeConfig *billing.TimeCoeffConfig`、`ContextTiers []billing.TierRule`。
- `channelprotocol/service.go`：新增变更钩子 `SetChannelRefresher(func(ctx, protocolID int64) error)`；Create/Update/Delete/Sync 成功后调用。
- `channel/channel_service`：新增 `RematerializeProtocol(ctx, protocolID)`（按协议重物化引用渠道的 channel_models）与 `materializeFromProtocol`；Create/Update 渠道时若引用协议则物化。
- `channel/store.go`：新增 `ListByProtocol(ctx, protocolID)`。
- `channelprotocol/handler.go` + `source.go`：新增 `POST /admin/channel-protocols/{id}/models/pull`（body `{ApiKey}`），用协议 baseUrl(openai-chat) + 临时 Key 调 `/models`，返回 `{List:[{ID,Object,OwnedBy}]}`。
- 迁移：无需新列（models 为 JSONB）。

**Frontend**
- `views/admin/ChannelProtocolList.vue`：整行点击进入编辑 + `clickable-rows`。
- `views/admin/ChannelProtocolEdit.vue`：协议配置并入基本信息 Tab；请求头策略重构；模型清单完整配置 + 拉取 + models.dev。
- `components/KvpEditor.vue` / `StringListEditor.vue`：统一高度；可选 key/value 下拉候选。
- `api/admin.ts`：扩展 `ChannelProtocolModelOverride`（externalModelID/costRates/timeConfig/contextTiers）与 `pullChannelProtocolModels(id, ApiKey)`。

## Risks & Open Questions

| Risk | Mitigation |
|---|---|
| 物化覆盖渠道侧手工微调 | 物化仅在有协议且行 externalModelID>0 时 upsert；保留 channel_model 状态；不删除协议未列出的渠道模型 |
| 运行时刷新导致状态丢失 | 复用 SyncFromDB/Upsert（保留 machine state / key state） |
| 请求头候选暴露敏感头 | 候选仅作提示，仍受强制黑名单约束 |
| Open question: 删除协议模型是否清理渠道模型 | 本期不清理（仅记录），避免误删渠道配置 |
| Open question: 协议模型是否含运行参数（限流/探测） | 本期不含，沿用渠道级 |
```
