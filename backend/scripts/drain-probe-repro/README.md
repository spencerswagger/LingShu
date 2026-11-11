# 排空（drain）探测频率：复现与验证

针对缺陷：密钥 / 内部模型 / 渠道（=渠道下全部密钥）被排空后，没有按
`health_probe.drain_interval_seconds`（默认 15 秒）探测，恢复要等很久。

本目录只放**可复跑的技术复现步骤**：一段带 `drainrepro` 构建标签的 Go 测试，直接驱动真实的
`channel.Manager` 探测循环（真实定时器 + 真实 HTTP 假上游），打印状态机取值、实际探测时刻、
探测间隔与恢复耗时，并按验收门限自动判红/判绿。

---

## 1. 跑法

```sh
cd backend

# 复现 / 验收（约 5 分钟：A 101s、B 61s、C 101s、启动即排空 30s）
go test ./internal/domain/channel -tags drainrepro -run TestDrainProbeRepro -v

# 只看某档
go test ./internal/domain/channel -tags drainrepro -run 'TestDrainProbeRepro$/A' -v
```

`drainrepro` 标签保证它**不进入默认测试**（默认套件不承担 5 分钟墙钟开销）。

## 2. 场景与门限

渠道配置：`Interval = "0 0 0 * * *"`（每天 00:00，等价长间隔）、`DrainIntervalSeconds = 15`、
`TimeoutMS = 2000`、`FailThreshold = 1`、`RecoveryThreshold = 1`（与验收剧本 seed 一致）。
上游先 `500`，排空后观测；转 `200` 后测恢复。

| 用例 | 排空对象 | 断言 |
| --- | --- | --- |
| A | 密钥（模型保持 NORMAL） | 首次探测 ≤18s、间隔 ∈[12,18]、无 <10s、窗口内 ≥3 次、恢复 ≤18s、恢复后 40s 内无新探测 |
| B | 内部模型（密钥保持 NORMAL） | 同上 |
| C | 渠道：渠道下 2 个密钥同时排空 | 每个密钥各自满足：首次 ≤18s、间隔 ∈[12,18]、无 <10s、≥3 次、恢复 ≤18s |
| 启动即排空 | 模型在 DB 里已是 DRAIN，进程刚起 | 首次探测 ≤18s |

判定：任一不满足即 `t.Errorf/Fatalf`（报告里直接给出实测秒数）。

## 3. 修复前（`origin/main` 3720ed7）实测

```
[A] 初态 key=NORMAL model=NORMAL branch=cron（等待已进入 cron 长睡）
[A] 排空瞬间 key=DRAIN model=NORMAL branch=key_drain
[A] 观察窗口 40s 内探测次数=0 首次探测=-1.0s
[A] 恢复: 成立=false 耗时=45.1s 期间新增探测=0 key=DRAIN model=NORMAL
[B] 排空瞬间 key=NORMAL model=DRAIN branch=cron        ← 分支就选错了
[B] 观察窗口 40s 内探测次数=0
[C] 观察窗口 40s 内探测次数=0
[启动即排空] key=NORMAL model=DRAIN，40s 内 0 次探测
```

三档全部「排空后一次探测都没有」，恢复无从谈起。

## 4. 修复后实测

```
[A-密钥排空]     首次 +15.0s，间隔 [15.0 15.0]，3 次；恢复 12.0s；恢复后 40s 内无新探测
[B-内部模型排空] 首次 +15.0s，间隔 [15.0 15.0]，3 次；恢复 12.0s
[C-渠道排空]     keyA 首次 +15.0s 间隔 [15.0 15.0]；keyB 首次 +15.0s 间隔 [15.0 15.0]；恢复 12.0s
[启动即排空]     首次 +15.0s，间隔 [15.0]，2 次
```

## 5. 默认套件里的快测（不等 15 秒）

`backend/internal/domain/channel/probe_drain_schedule_test.go`：

- `TestNextKeyProbeWait_ThreeTiers`：三档对象 × 三态（NORMAL/DRAIN/DISABLED）的等待计算，
  含模型级 `DrainIntervalSeconds` 覆盖、密钥与模型同时排空取最小间隔、禁用态不代探模型；
- `TestKeyProbeLoop_WakesOnManualDrain`：排空发生在 cron 长睡中途也要被唤醒（1s 排空间隔，秒级判定）；
- `TestKeyProbeLoop_ModelDrainRecoversThenBackToCron`：模型排空被探测恢复后，立刻停止秒级探测（回到 cron）；
- `TestDrainProbe_FailureReasonRecorded`：排空探测失败原因（含上游状态码）落 `probe_logs`；
- `TestProbeWaker_WakesAllSubscribers`：唤醒是广播语义（一次变化唤醒全部密钥循环）；
- `TestKeyProbeLoop_NormalStateWakeDoesNotProbe`：常态下连续唤醒 20 次不产生探测、仍指向 cron 边界；
- `TestNextKeyProbeWait_ProbeModelScope` / `TestKeyProbeLoop_ProbeModelScopeStaysCron` /
  `TestKeyProbeOnce_ProbeModelIsKeyLevel`：配了 `probe_model` 时作用域外的模型排空不参与调度（回 cron），
  该模式下的探测是 `level=key` 且不回喂模型状态机。

## 6. 观测到的一个相邻事实（不在本修复范围）

`internal/pkg/cronx` 的周字段解析把 `*` 展开成 `[0..7]`（8 个值，7 与 0 都表示周日），
导致 `isFullRangeWeek()` 判定失败 → `matchDay` 落到「日 或 周」分支 → 形如
`0 0 0 1 1 *`（本意每年 1 月 1 日）实际退化成正每天 00:00。调度侧影响只是「等待上限从一年变成一天」，
不影响排空分支；修复它会改变既有 cron 语义，故单独留作后续任务。
