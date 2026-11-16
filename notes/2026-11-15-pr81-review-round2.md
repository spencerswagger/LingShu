# PR #81 第二轮复审三条必改 · 验证记录（2026-11-15）

分支 `feat/lingshu-v2-p1-magpie-passthrough`，基线 head `65677ff`。
本文只记「可复现输入 → 观测输出」的实测数字与变异验证，口径落在
`docs/designs/2026-11-15-magpie-passthrough-link.md` §2.1。

## P1-1 租户令牌随 query 出境

- 根因两处：
  1. 判定用 `r.URL.Query().Get("key")`（**解码后**键名），剔除按**原始字面**比较 `key`
     → `?ke%79=<令牌>`、`?%6Bey=<令牌>` 判成「带了」却剔不掉。
  2. `Prepared.StripQueryKey` 只等于 `fromQuery`：**头鉴权 + 同时带 `?key=`** 这条路完全不剔。
- 修法：新增 `internal/domain/magpie/query.go`，键名比较统一走 `querySegmentKey`（`url.QueryUnescape`，
  解码失败回落字面）：
  - `queryParamValue` 给鉴权取凭据（与 `Query().Get` 同语义：取第一个）；
  - `stripQueryParam` 给剔除，其余段逐字节保留，未命中返回原串；
  - handler 侧 `StripQueryKey = fromQuery || hasQueryParam(rawQuery,"key")`——存在即剔，不再看鉴权来源。
- 实测（`TestForward_QueryKeyEncodedVariants_Stripped`，端到端假上游）：

  | 请求 | 修复前上游 `RawQuery` | 修复后 |
  |---|---|---|
  | `?%6Bey=tenant-token&alt=sse` | `%6Bey=tenant-token&alt=sse` | `alt=sse` |
  | `?ke%79=tenant-token&alt=sse` | `ke%79=tenant-token&alt=sse` | `alt=sse` |
  | `?%6B%65y=tenant-token&alt=sse` | `%6B%65y=tenant-token&alt=sse` | `alt=sse` |
  | 头鉴权 + `?key=tenant-token&alt=sse` | `key=tenant-token&alt=sse` | `alt=sse` |

  （修复前那三行是 200 且令牌原样出境——用「把键名比较改回字面」的变异复现，逐行红。）
- 不变式：`?alt=sse&&x=1%2F2&empty=&flag`（无 key）→ 上游 `RawQuery` 与入站**逐字节相同**
  （`TestForward_NoQueryKey_RawQueryByteIdentical`）。
- 反例守卫：`keys=t`、`other=t` 不剔；`key`（无 `=`）剔。

## P2-1 超 32MB 非流式响应静默截断

- 修法：越过 `maskBufferLimit`（32MiB）后，已读部分先写、**剩余继续流式转发到底**（尾部暂存遮蔽照旧），
  长度交 chunked。选这条而不是「显式截断」的理由：响应头未发出但状态码已确定（上游 200），
  截断只能靠改状态码/加自定义头表达，而「响应字节原样 + 只换两样」的契约里没有可加的语义；
  把剩余转发完既不丢字节也不改契约。
- 实测（证据用例一次跑，真上限 32MiB，body = `"A"×35,651,584`）：

  | 版本 | 上游发出 | 下游实收 | 差值 | 状态 | `Content-Length` |
  |---|---|---|---|---|---|
  | 修复前（越界即 return） | 35,651,584 | 33,554,433 | 2,097,151 | 200 | 已删 |
  | 修复后 | 35,651,584 | 35,651,584 | 0 | 200 | 已删（chunked） |

  与复审给的数字一致（33,554,433 = 32MiB+1）。
- 用例（把上限调到 1024 的低成本版）：`head(2000A) + " caller-secret " + tail(1000B)` → 实收 3012 字节
  （修复前 1025），且**越界之后**的凭据仍被遮蔽（`Result.CredentialMasked=true`）。

## P2-2 声称 gzip 但字节不是 gzip → 整段丢光

- 修法：`replayReader` 记录解码器从响应体读过的字节，`replay()` 给出「已消费前缀 + 剩余」。
  `gzip.NewReader`/`zlib.NewReader` 失败 → 回退原始字节；**读到中途失败**（头合法、流被截断）同样回退
  （非流式路径另行把 `Content-Encoding` 恢复回去，并标记 `UnmaskableEncoding` 落告警）。
  流式（SSE）只在创建期用快照，解码器建好即 `reset()` 丢弃，避免整条压缩流攒内存。
- 实测（body 1500 字节、声明 `Content-Encoding: gzip`）：

  | 版本 | 下游实收 | 状态 | 下游 `Content-Encoding` | `UnmaskableEncoding` |
  |---|---|---|---|---|
  | 修复前 | 0 | 200 | gzip | gzip |
  | 修复后 | 1500 | 200 | gzip（原样） | gzip（告警） |

  截断 gzip（头合法、砍掉尾 12 字节）：修复前 502「读取上游响应失败」（原始内容全丢，只剩网关错误体），
  修复后 200 + 完整原始字节（`Content-Encoding: gzip` 原样保留）。
- 正路不回归：合法 gzip / 合法 deflate 仍解压后遮蔽并删 `Content-Encoding`；SSE + gzip 仍逐帧遮蔽。

## P3（顺手）解码失败告警没有用例守着

- `TestHandler_UnmaskableEncodingWarns`：注入 `logger.New(&buf, DEBUG)`，上游回
  `Content-Encoding: br`，断言日志同时命中「上游响应编码无法解码」与 `encoding=br`。
- 变异：把 handler 里那段 `Warn` 整段删掉 → 该用例红（此前删掉全仓 25 包仍全绿）。

## 变异验证汇总（每条都是「改回旧实现 → 对应用例红 → 还原 → 绿」）

| 变异 | 变红用例 | 观测 |
|---|---|---|
| `querySegmentKey` 不做解码 | `TestQueryParamKey_DecodedVariants` | `hasQueryParam("%6Bey=t")=false` |
| `stripQueryParam` 按原始字面比较 | `TestForward_QueryKeyEncodedVariants_Stripped`（3 变体） | 上游 `%6Bey=tenant-token&alt=sse` |
| handler 的 `StripQueryKey` 退回 `fromQuery` | `TestForward_HeaderAuthWithQueryKey_Stripped`（3 头） | 上游 `key=tenant-token&alt=sse` |
| 越界分支改回 `return` | `TestForward_OversizeNonStream_NoSilentTruncation` | `got 1025, want 3012` |
| 去掉 `replayReader` 回退 | `TestForward_BogusEncoding_KeepsAllBytes` | `case[0] 整段响应被丢光（0 字节）` |
| 删 handler 的 `Warn` | `TestHandler_UnmaskableEncodingWarns` | 告警未触发 |

## 门禁

- `gofmt -l internal/domain/magpie/` → 空。
- `go build ./...`、`go vet ./...` → 干净。
- `go test -race $(go list ./... | grep -v '/tests$')` → 25 包全 ok（magpie 63 个顶层用例 + 子用例）。
- `go test ./tests/` → ok。
- 未改迁移、未改 #82 分支任何文件。