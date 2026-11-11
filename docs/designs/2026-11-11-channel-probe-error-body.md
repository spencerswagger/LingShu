# 渠道探活：上游错误体落库与列表/详情端点（2026-11-11）

> 本文件是从「上游错误可见性」分支摘出的**渠道探活**部分，独立成单。
> 探活过去只记「probe http status: 400」，上游到底回了什么（区域未开通 / 余额不足 / 模型无权）
> 全被吞掉，运营无法据此改配置。

## 1. 落库

`probe_logs` 新增两列（迁移 `0020_probe_logs_error_body.sql`）：

| 列 | 类型 | 语义 |
| --- | --- | --- |
| `error_body` | `TEXT NOT NULL DEFAULT ''` | 上游错误响应体原文（运营可见；仅遮蔽本次凭据值；>32KB 按 UTF-8 边界截断并同字段标注；空 = 上游无错误体） |
| `upstream_status` | `SMALLINT NULL` | 本次探测的上游 HTTP 状态码；`NULL` = 没打到上游（连接失败/超时/无可用模型或密钥） |

`ProbeOutcome.ErrorBody` 与 `ProbeLog.ErrorBody` 同步暴露该原文。
迁移只新增列（带默认值），**不修改、不删除、不回填**任何既有列与既有行；
回滚路径与「为什么不用索引」写在该迁移文件尾部注释里。

## 2. 接口

- 列表 `GET /api/v1/admin/channels/{id}/probe-logs`：行数上限 **100**，每行 `ErrorBody` 只给 **4KB 预览**
  并置 `ErrorBodyTruncated=true`，同字段标注完整原文该去哪里取。
- 详情 `GET /api/v1/admin/channels/{id}/probe-logs/{logID}`：返回单条记录的**完整原文**
  （SQL 里 JOIN `channel_keys` 限定 `channel_id`，跨渠道读不到别人的报文）。

**前端目前尚未消费** `ErrorBody` / `ErrorBodyTruncated` / `UpstreamStatus` 三个字段，
运营界面还看不到原文——本期只做到「API 与落库可见」，界面展示属后续单。

## 3. 与「上游错误可见性」单的关系

两单共用「上游原文对运营可见」这一目标，但代码面互不依赖：
本单只动 `internal/domain/channel`、迁移 0020、`server.go` 的一条路由；
`/v1` 对外错误契约（状态码映射、`error.message/type/code/upstream_status`）不在此单范围内。
