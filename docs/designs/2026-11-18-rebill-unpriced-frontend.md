# unpriced 账单补收 · 前端交互（B 方案·手动触发）

> 日期：2026-11-18 · 分支：`fix/v3-settle-gap` · 依赖后端：`52668a7`（rebill 接口）
> 需求来源：spencer 拍板 —— 组售价保存成功后，检测历史 unpriced 账单，运营手动点按钮补收。

## 交互链

1. 组售价抽屉**保存成功**后自动检测该组 unpriced 账单条数 N（约 2~4 个请求，抽屉内显示「正在检测…」）。
2. N=0：抽屉照旧关闭（原行为不变）；N>0：抽屉**保持打开**，出现橙色块
   『检测到 N 条未定价账单，按当前售价补收？』按钮。
3. 点击 → `ElMessageBox.confirm` 确认（写明：按当前售价开新账单、从对应用户余额扣款、原账单保持不变）。
4. 成功：抽屉内展示补收条数 / 金额合计 / 跳过条数（此前已补收）/ 新账单号清单——**全部用后端返回
   `Rebilled / TotalCredits / AlreadyMarked / NewBillingIDs`，前端不自算**；同时清零本地计数。
5. 部分失败（毒行隔离，后端 `a9c8a0e` 起）：HTTP 仍 200，响应多 `Failed[]`
   （元素 `{BillingID, Reason, Message}`；`omitempty`，全部成功时字段缺席、展示不变）。
   结果块在成功条数之下追加红色失败清单：账单标识 + `Reason` 映射中文 + 「后端说明：Message」双展示。
   行级 Reason 键：`unpriced_row`（仍解析不到售价）/ `unresolved_user`（缺合法用户归属无法扣款）/
   `record_error`（计费落库出错）；未知键兜底展示原文 Message。
6. 批次级失败（组未定价，400 + `rebill_group_unpriced`）：抽屉内红色块——Reason 映射中文处置文案 +
   后端 `Message` 原文。批次级 `rebill_row_unresolvable` 已随毒行隔离废弃（改走 Failed[]）。
7. 切组/再保存会重置上一轮检测与结果（含竞态 token 守卫），不残留旧状态。

## 检测方式（不改后端，用现成接口）

`GET /admin/billings?Status=unpriced&Model=<组名形态>&Group=0&Size=200` 逐形态拉一页，按
`BillingID` 去重、按行内 `ErrorMessage` 含「已补收:」剔除（补收不改原行 status，`Total` 会含已补收行）。
组名形态集合（对齐后端 `groupRefForms` 保守近似）：`ref`、去 `group/` 前缀组 id、auto 两档 slug
（原样 + 非法字符替换 `-`）、组显示名（gateway 快照）、组 id；快照拿不到时退回前两档。

### 已知口径缺口（如实接受，不为此改后端）

1. **检测数偏窄**：补收扫描还匹配组快照 `display_name ↔ ref` 双向；快照缺失时前端数不到、
   补收却能算到 → 按钮上的 N 可能小于实际补收条数。
   → 差异不静默：预估差体现在返回 `Rebilled` 与 N 之差；数不到且解析失败的行会进 `Failed[]` 显性展示。
2. **检测数偏宽**：模型级售价覆盖组 ref 候选时，个别行实际能解析到（模型名形态命中），
   前端按组形态数到了 → N 可能大于实际补收条数。
   → 弹窗文案已写明「实际补收结果以执行后返回为准」，真实条数/金额以 `Rebilled/TotalCredits` 展示。
3. **单形态超 200 条不再深翻**：手动触发场景可接受；这种量级建议直接补收后看返回值。

## 附带修正（本分支顺手修，不属补收主链）

- 拓扑页顶栏 `.bar` 在窄屏不换行挤爆视口（390px 横向溢出 357px）→ 改可换行。
- 抽屉 560px 固定宽在 390px 视口溢出 → `<640px` 改 100% 宽。
- `http.ts` 错误对象补 `reason` 字段（后端响应壳早就有 `Reason`，前端一直没透传）。

## 走查

`/data/pv/rebill-check.mjs`（一次性脚本，不进仓库）：vite + puppeteer 拦 `/api/v1/**` 回桩，
29 项断言全过——保存后检测（多形态去重、剔除已补收行）、确认弹窗文案、部分失败
（成功合计与失败明细并列：账单标识 + Reason 中文 + Message）、全部成功展示不变、失败 Reason 展示、
N=0 抽屉照旧关闭、390px 页面与抽屉内均无横向溢出。
