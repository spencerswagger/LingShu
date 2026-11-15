# #81 矩阵三条缺陷修复（P0-① / P0-② / P1-③）

分支 `feat/lingshu-v2-p1-magpie-passthrough`，基线 head `7bd19a7`。三条都做「改回旧实现 → 用例红 → 还原 → 绿」的变异验证。

## P0-① 未启用/不存在模型返回 500 internal_error

**根因**：`model.Service.GetByExternalName` 只返回**启用**记录，查不到就折叠成 `model.ErrNotFound`
（`sql.ErrNoRows` 已被它吃掉）。`magpie.AdmitToken` 只 `errors.Is(err, sql.ErrNoRows)` → 落进
「未知错误」分支 → `500 internal_error`。因此「未启用」与「不存在」在生产路径上是同一个错误值。

**修法**：`errors.Is(err, model.ErrNotFound) || errors.Is(err, sql.ErrNoRows)` → `404 model_not_found`。
`!ext.Enabled`（别的实现返回了停用行）同口径 → 404，同一句话。
真正的服务故障（DB 等）仍 `500 internal_error`，另有用例守。

**为什么选 404 而不是 403**：① 停用与不存在必须对外不可区分，否则任意租户能枚举出「哪些模型被停用」；
② 生产路径里两者本来就是同一个错误值，选两个状态码会出现「测试 403 / 生产 404」的不一致。

| 输入 | 旧观测 | 新观测 |
|---|---|---|
| `POST /v1/chat/completions`，`{"model":"not-enabled-model",...}`，Bearer 有效令牌 | `500`，`code=internal_error`，文案「服务内部错误」 | `404`，`code=model_not_found`，文案「模型不存在或未对租户开放」，**响应体 130 字节**（`{"error":{"message":"模型不存在或未对租户开放","type":"invalid_request_error","param":null,"code":"model_not_found"}}`） |

用例：`TestAdmit_ModelErrNotFound_Is404Not500`（单元）、`TestHandler_UnknownModel_HTTP404`（HTTP 级，
含「文案不含 http:// / 127.0.0.1 / magpie / external_models」断言）、`TestAdmit_OtherLookupError_Stays500`。
变异（只认 `sql.ErrNoRows`）：两条红，报 `deny = 500/internal_error`、`状态码=500`。

## P0-② 未映射模型返回 403 model_not_allowed

**修法**：`magpie_model` 为空 → `404 model_not_found`，文案「模型尚未接入转发服务」。
与「未开通 callerKey」的 `403 magpie_key_missing`、「订阅池」的 `403 model_not_allowed` 在**状态码、
归类码、文案三层**都不同。文案刻意不提「代理/上游」等内部实现名。

用例：`TestAdmit_UnmappedVsNoPermission_AreDistinguishable`（两分支逐字段比较「不得混淆」）、
`TestAdmit_SubscriptionPool_Stays403`、`TestAdmit_Denies/未映射代理模型`（改判 404）。
变异（改回 403 model_not_allowed）：两条红。

## P1-③ 重复顶层 model 被整体替换

**修法**：`RewriteModelJSON` 只替换**最后一处**（JSON「重复键后者生效」，与 `ModelFromJSON` 的取值口径一致），
其余字节不动。不变式「除生效 model 外逐字节不动」保持。

| 输入 | 旧输出 | 新输出 |
|---|---|---|
| `{"model":"a","model":"b"}`（25B） | 两处都变 `mp-openai`（33B） | 只有后者变（33B，首处 `"a"` 原样） |
| 乱序键 + 重复（69B） | 两处都变 | 只有后者变（77B） |
| 异常空白/换行 + 重复（53B） | 两处都变 | 只有后者变（61B） |

用例：`TestRewriteModelJSON_DuplicateModelOnlyEffectiveRewritten`（三输入，并做**反向验证**
「把生效处换回原值 == 输入逐字节」）、`TestRewriteModelJSON_DuplicateDoesNotMutateInput`；
`TestRewriteModelJSON_PreservesAllOtherBytes` 的重复键用例同步改成「只改生效处」。
变异（改回逐处替换）：三条子用例 + 核心契约用例红。

## 未做（按要求）

- R-09（两边 0 笔 → `blocked` vs `skipped`）未动，等拍板。
- P-11（401 文案含 "Bearer"）未动，等 Gate 裁定正则 vs 真泄漏。
- #82 分支未碰。

## 门禁

`gofmt -l internal/domain/magpie` 空；`go build ./...`；`go vet ./...` 干净；
`go test -race $(go list ./... | grep -v '/tests$')` 25 包全 ok；`go test ./tests/` ok。

## 顺带记录（不属本次范围）

`WriteDeny` 对 OpenAI 形状固定输出 `param:null`；若以后要按协议区分，注意 Anthropic/Gemini 形状里
没有 `code` 字段（`model_not_found` 只在 OpenAI 形状可见），调用方按 body 分支时须知。
