# 状态页问题修复清单（交给实现 subagent）

环境：仓库根 `/workspace`。前端 `frontend/`（Vue3 + Element Plus），后端 `backend/`（Go）。
**只改这两个文件（除非另有说明）**：
- `frontend/src/views/admin/ServiceStatus.vue`
- `backend/internal/domain/channel/probe.go`

不要改其它页面、不要改接口契约、不要新增依赖。

---

## BUG-1（中）序位图标被合并隐藏 → 改为独立「序位」列

现状：★/☆/·N 序位图标渲染在「渠道」列的单元格里，而「渠道」列按渠道合并（`mergeKeyOf('channel')`），
导致同一渠道多密钥时只有首行显示图标，其余密钥的序位（如 ☆）不可见。

修复：
1. 在 `ServiceStatus.vue` 模板中，**移除**「渠道」列单元格里的 `rank-sym` 图标及其 `el-tooltip`（保留渠道名 + 原有 reason tooltip）。
   左缘色条（`:deep(.el-table__row.rank-*) td:first-child`）保留不动。
2. **新增一个独立的「序位」列**，`v-if="modelAxis && keyAxis"`（即仅当模型轴与密钥轴同时进入行轴、行粒度为单一通路时显示）。
   - 该列**不参与合并**（`mergeKeyOf('rank')` 返回 `null`）。
   - 表头文案「序位」，表头加 `el-tooltip` 图例：`★ 首选 · ☆ 次选 · ·N 第 N 候选 · — 不参与新会话`，并注明「顺序来自后端候选排序（Rank）」。
   - 单元格复用现有 `rankSymbol(row)` / `rankClass(row)` / `rankTip(row)`，用 `class="rank-sym"` 渲染（带 `rankTip` 悬浮）。
   - 列宽 `min-width="56"`，居中。
3. 同步更新列索引真源，**必须保证 `cols` 顺序与模板列顺序逐字一致**（`spanMethod` 依赖 `columnIndex`）：
   `type ColKey` 增加 `'rank'`；`cols` 在「已勾选维度列」之后、「priority」之前插入 `'rank'`（仅当 `modelAxis && keyAxis`）。
4. 更新页脚说明：把「序位图标」那条改为说明它现在是独立列、仅模型+密钥轴同时勾选时出现。

验收：勾选 4 个维度后，每个「通路行」（对外模型×渠道×密钥）各自的序位图标都能看到，互不遮挡。

---

## BUG-2（中）密钥列过窄、凭据尾号被折行 → 加宽 + 禁止折行

现状：`密钥` 单元格里名称被截断，且 `.hcell-sub`（凭据尾号）被折成两行（如 `3584-` 换行 `1`）。
原因：`.hcell-sub` 无 `white-space:nowrap`；维度列 `min-width` 偏小。

修复（在 `ServiceStatus.vue`）：
1. 样式 `.hcell-sub` 增加 `white-space: nowrap`（与名称一样单行，靠省略号/`title` 兜底）。
2. 提高维度列 `min-width`：对外模型/内部模型 `min-width="160"`，渠道 `min-width="180"`，密钥 `min-width="200"`。
   （指标列不改，保持现有 min-width。）

验收：密钥名称单行截断（有 `title` 悬浮），凭据尾号不再换行。

---

## BUG-3（低）详情对比表长文本从数字中间换行 → 改断词策略

现状：`.cmp-table` 用了 `word-break: break-all`，导致 `P99 <5000ms` 被断成 `P99 <500` / `0ms`。

修复：把 `.cmp-table th, .cmp-table td` 的 `word-break: break-all` 改为 `overflow-wrap: break-word`（保留 `word-break: normal`）。
这样数字/单位保持完整，超长 URL 仍会在需要时换行。字段名列宽由 `88px` 调为 `76px`。

验收：`可靠性阈值` 等在空格处换行，`5000ms` 不被从中间截断；长 BaseURL 仍可换行不溢出。

---

## BUG-4（低）占位符不统一（`-` vs `—`）

现状：详情弹窗「最近探测」用 `fmtDateTime(v)`，空值返回 `-`；其它处空值用 `—`。

修复：在 `ServiceStatus.vue` 增加 `function dt(v?: string | null): string { return v ? fmtDateTime(v) : '—' }`，
把弹窗「最近探测」行的 `fmtDateTime(...)` 全部替换为 `dt(...)`。

验收：空探测时间显示 `—`，与其它空值占位一致。

---

## BUG-5（低）内部模型 / 渠道的「最近探测」恒为空 → 后端回填

现状：`channel.RuntimeChannel.lastProbe` / `ModelRuntime.lastProbe` 字段存在，且 `detail.go` 已在其非零时
映射为 `ChannelView.LastProbe` / `ModelView.LastProbe`，但**只有密钥**在 `probe.go` 的 `markKeyProbeResult`
里被写入，模型与渠道永远为零值 → 前端恒显示空。

修复（`backend/internal/domain/channel/probe.go`）：
1. 新增 `markModelProbeResult(channelID, modelRowID int64, errMsg string)`：在 `m.mu` 锁内，取
   `m.mrt[channelID][modelRowID]`，非 nil 时 `mr.lastProbe = m.now()`（**不要**改 `mr.LastErr`，避免引入
   瞬时 429 噪音，保持现有语义）。
2. 新增 `markChannelProbeResult(channelID int64)`：锁内取 `m.rt[channelID]`，非 nil 时 `rt.lastProbe = m.now()`。
3. 在 `probeOnce(ctx, keyID, probeModel, level, modelRowID)` 里，`markKeyProbeResult(keyID, errMsg)` 之后调用：
   - `m.markChannelProbeResult(kr.Key.ChannelID)`
   - `if modelRowID > 0 { m.markModelProbeResult(kr.Key.ChannelID, modelRowID) }`
   （沿用 `feedModelAndTransit` 的 channelID/modelRowID 取法：`kr.Key.ChannelID`）
4. 不改 `LastErr`（模型/渠道的「最近错误」保持不显示，属预期）。

验收：`go build ./...` 通过；探测若干轮后，`GET /api/v1/admin/service-status` 返回的
`Channels[].View.LastProbe` 与 `Channels[].Models[].View.LastProbe` 非空。

---

## BUG-6（低）表格行高偏大 → 轻微收紧

修复：`.lmcell` 的 `line-height` 调到 `1.4`、`gap` 保持 `2px`；`.lmrow` 的 `gap` 调 `4px`。
（不要改动行高其它来源。）

---

## 通用要求
- 只做上述改动，保持现有代码风格（中文注释、`function`/`computed` 命名习惯），**不新增注释除非必要**。
- 改完必须自检：
  - 前端：`cd frontend && npx vue-tsc --noEmit`（若报 `vue-tsc: not found`，先 `npm ci`）。
  - 后端：`cd backend && go build ./...`
- 报告：逐条说明改了哪里、以及上述两种自检命令的实际输出（原样贴关键行）。
