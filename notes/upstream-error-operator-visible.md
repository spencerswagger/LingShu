# 上游原始错误「运营侧可见」：调用日志/账单两个落点（2026-11-11）

> 口径（用户纠正版，覆盖前两版验收）：判断标准只有一条 —— **运营在后台看得到上游返回的原始错误体原文**。
> 下游客户端能不能读到不是本任务目标（对外契约保持 PR #77 v2 的样子，不为本任务再改）。

## 1. 落点

| 落点 | 入口 | 落地 |
| --- | --- | --- |
| 调用日志 / 账单 | `gateway/handler.go` 六个上游失败分支（429 / 401-403 / 其它，非流式 + 流式） | `call_logs.resp_body` = 上游原文逐字节（`upstreamErrBody.operatorView(ureq.Cred)`）；`error_message` 同一原文（摘要 4KB 上限）；`upstream_status` + `request_id` + 渠道/密钥/模型/时间/耗时不变 |

## 3. 「原文被吃掉」的两个真实原因

1. **按样式清洗**：`sanitizeErrSummary` 把 `sk-*`/Bearer/KV/**高熵长串（≥20 位字母数字混排）**
   一律换成 `***`。`resp_body` 历史上取自它 → 上游报文里的 request-id、随机串、模型名被整段抹掉
   （实测「第 45 字节后整段消失」）。
2. **落库上限静默**：无标注可循（历史无 32KB 概念）。

修法：新增第三视图 `operatorView`（`gateway/upstream_error.go`）——原文逐字节，**只**遮蔽
本次实际使用的凭据原值（`internal/pkg/errbody.MaskSecrets`，精确子串、长度 ≥8 才参与）；
超 32KB 同字段标注，读满 64KB 时如实写「超过读取上限」。`Clean` 保留给日志短字段与错误分类。

## 4. 测试（改现有，不重做）

`internal/domain/gateway/upstream_error_passthrough_test.go`：
- 删掉固定样例（`openCodeGoRegionErrBody` 那类「某厂商某段文案」）作为断言依据；
- `TestServeUpstreamError_ShapeTable` → `TestServeUpstreamError_RandomShapes`：**随机驱动**
  （seed 打印在日志里），每轮内容不同；
- 断言只有一条：随机内容**逐字节**出现在 `call_logs.resp_body`（超长则前缀逐字节 + 同字段标注）；
- 跑旧实现必红（已验证：把 `operatorView` 换回 `Clean`、把 `readProbeErrBody` 改成不读体 → 5 个用例 FAIL）。

`internal/pkg/errbody`：精确遮蔽与截断的单测。
真库：`TestCallLog_PgRoundTrip`（自建临时库 + 跑全部迁移）。

## 5. 边界

- 保存上限 32KB（`callLogErrBodyMaxBytes`）；读取上限 64KB。
  超限同字段标注，截断按 UTF-8 边界（不会切出半个汉字）。
- 只遮蔽「本次实际使用的凭据」：若上游回显的是**别的**凭据（例如上游自己的内部 token），会原样落库。
  这是刻意取舍——按形状猜会误伤原文，而误伤原文正是本任务要解决的问题。
- 前端账单详情要展示 `RespBody`（`UpstreamStatus` 同理），属 View 侧改动。

## 6. 分支

`fix/upstream-error-passthrough`（PR #77，同一特性分支上追加本组提交；未自行合入、未推 main）。

## 7. Gate 复验后追加：对外不得泄漏渠道凭据（必改，已修）

- 现象：上游 401 回显我们发过去的鉴权头时，**对外**响应把 `Bearer sk-...` 原样透出；
  `main` 不泄漏（旧实现一律替换成中文通用文案）→ 属本 PR 新引入的安全回归。
- 修法：`writeUpstreamError`/`upstreamErrorPayload` 增参接收本次 `ureq.Cred`，内容落盘/输出前
  统一过 `errbody.MaskSecrets`（与运营侧 `operatorView` **同一函数、同一规则**：只遮蔽本次凭据原值）；
  终结响应（候选全败）的凭据随 `fwdOutcome.upstreamCred` 带过去，保证最后一次上游报文也被遮蔽；
  截断标注里的字节数取**遮蔽前**长度。
- 回归测试：`TestServeUpstreamError_DownstreamNeverSeesChannelCredential`（OpenAI 形状直接返回 /
  纯文本终结 / 429 终结三路）——同时断言「对外无凭据」与「除凭据外逐字节保留」，
  防止遮蔽反向污染运营侧。已验证撤掉遮蔽即 FAIL（复现 Gate 的两行最小复现）。
- 运营侧未受影响：`call_logs.resp_body` 仍是原文（仅凭据位置为 `***`）。
