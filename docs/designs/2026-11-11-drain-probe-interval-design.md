# 排空探测频率与唤醒路径（drain probe interval）

- 日期：2026-11-11
- 范围：`internal/domain/channel` 的探测调度、状态机退出排空、探活日志
- 不涉及：计费、路由决策、鉴权语义、DB schema

## 1. 现象

密钥 / 内部模型 / 渠道（=渠道下全部密钥）任意一档进入 `DRAIN` 后，不按
`health_probe.drain_interval_seconds`（默认 15 秒）探测，要等到下一次 `health_probe.interval`
（默认 `0 0 0 * * *`，每天 00:00）才探一次 —— 排空恢复因此最长要拖到次日。

复现实测（`origin/main` 3720ed7，渠道 `interval` 设长、`drain_interval_seconds=15`）：

| 用例 | 排空后 40s 内探测次数 | 恢复 |
| --- | --- | --- |
| 密钥排空（模型 NORMAL） | 0 | 未恢复 |
| 内部模型排空（密钥 NORMAL） | 0 | 未恢复 |
| 渠道排空（2 个密钥） | 0 | 未恢复 |
| 启动即排空（模型 DB 里已 DRAIN） | 0 | 未恢复 |

复跑步骤与本轮实测数字见 `backend/scripts/drain-probe-repro/README.md`。

## 2. 根因

两个缺陷叠加，缺一不可：

1. **等待时长只看密钥状态机**（`probe.go:147` `sleepUntilNextKeyProbe`，分支选择在 `probe.go:152-166`）：
   `switch` 只读 `kr.Machine.State()`。而探测目标由 `pickKeyProbeTargets`（`probe.go:229`）给出，
   它包含该渠道**全部非 DISABLED 的内部模型**——模型没有独立探测循环（`probe.go:193` 注释即此意）。
   于是「密钥 NORMAL + 内部模型 DRAIN」落入 `default` 分支 → `cronWait(hp.Interval)`，
   秒级分支进不去。渠道级排空不存在独立状态机，落地形态就是「渠道下全部密钥 DRAIN」，
   仍受下一层缺陷影响。
2. **状态变化不唤醒已排定的等待**（`probe.go:170` `t := time.NewTimer(wait)`）：
   `select` 只等 `ctx.Done()` 和定时器，没有任何通知通道。一次性的 `time.Timer` 一旦按
   cron（默认次日 00:00）排定，此后发生的排空（管理员排空、真实流量触发可靠性窗口、
   协议级错误策略处置、配置改动）都不会打断它；进程启动时按持久化状态算出的等待同理。

## 3. 修法

### 3.1 等待时长改由「三档里最紧急的那档」决定

`probe.go:205` `nextKeyProbeWait(keyID)`：

- 密钥 `DISABLED` → `disabledRecheckSeconds`（15s 复查，不发探测；禁用态不再看模型）；
- 密钥 `DRAIN` → 渠道 `drain_interval_seconds`；
- 该渠道任一内部模型 `DRAIN`（且实体/状态机不是 DISABLED）→ 该模型的有效排空间隔
  （模型级配置优先，缺省继承渠道，走 `mergeHealthProbe`）；
- 都没有 → `cronWait(health_probe.interval)`。

同时排空时取最小间隔，并返回分支名（`key_drain` / `model_drain` / `disabled` / `cron`）供日志诊断。
模型级 `DrainIntervalSeconds` 由此真正进入调度（此前只在视图里可见）。

### 3.2 唤醒路径：状态/配置一变就重算

`manager.go:1480` `probeWaker`：关闭当前 channel 并换新 channel 的广播器（不是 buffered channel：
一次变化要唤醒**全部**密钥循环，且不能丢唤醒）。`probe.go:161` `waitUntilNextKeyProbe` 先订阅
唤醒通道再计算等待，`select` 增加 `<-wake` 分支。

唤醒来源（全部在状态真正变化时触发，锁外广播，非阻塞）：

| 位置 | 场景 |
| --- | --- |
| `ManualSetKeyState` / `ForceKeyState` / `ForceKeyNormal` | 管理员排空/恢复、协议级错误策略处置 |
| `ManualSetModelState` / `ForceModelNormal` | 管理员模型排空/恢复、手动探活成功 |
| `feedAndTransit`（真实调用回喂） | 可靠性窗口把密钥/模型打到 DRAIN |
| `feedKeyAndTransit` / `feedModelAndTransit`（探测回喂） | 首次探测失败 → DRAIN、连续成功 → NORMAL |
| `SyncFromDB` / `Upsert` / `UpsertKey` / `UpsertModel` | 启动装载、热重载、渠道探测配置变更 |
| `RemoveKey` / `Drop` | 运行时消失，循环尽快退出（否则要睡满当前等待） |

### 3.3 唤醒语义：只能提前，不能推后；恢复后不残留秒级探测

唤醒不是「立刻探测」，而是在同一轮等待内**重算截止时刻**：

- 排空/禁用分支沿用更早的旧截止 → 每次唤醒只能让探测更早，不会被同渠道其它对象的流转一路推后
  （多密钥渠道首探原本会因此滑出 15 秒档）；
- cron 分支从现在重算 → 恢复成 NORMAL 后立刻回到 cron 语义，不会残留一次「排空遗留」的秒级探测
  （否则恢复后还会多探一次，与「回到常态」不符）。

截止已过（例如上游探测耗时超过间隔）时立即探测。

### 3.4 失败原因

排空态探测走既有 `keyProbeOnce → probeOnce → recordProbe` 链路，`probe_logs` 记录
`level=key|model`、`ModelID`、`ok=false`、`error`（形如 `probe http status: 500`、`probe auth failure: 401（凭据失效）`）
与 token/耗时；「凭据为空」「无可用探测模型」「未配置 Base URL」也各有一条可读失败原因。
新增用例 `TestDrainProbe_FailureReasonRecorded` 把「原因必须入 probe_logs」钉住。

### 3.5 `probe_model` 是探测偏好，不是白名单

`health_probe.probe_model` 只决定**常态**探测对象；排空态是**动态叠加**：任何模型进入 `DRAIN` 都要被
定向（模型级）代探，即使它不是 `probe_model`。否则「作用域外模型排空」没有任何自动恢复路径，
模型容量静默丢失（`router` 会把非 NORMAL 模型剔出候选，连真实流量都回喂不到它）。

| 配置 | 常态探测（cron 节奏） | 排空态追加 |
| --- | --- | --- |
| 未配 `probe_model` | 该渠道全部非 DISABLED 模型，`level=model`（回喂密钥 + 模型） | 已包含全部模型，无需追加 |
| 配了 `probe_model` | 仅该模型，`level=key`（回喂密钥 + 该模型） | 1 个模型级定向代探目标（`level=model`，回喂该模型，**不回喂**密钥） |

动态代探的四条硬约束（都可断言）：

1. **每渠道每个 `drain_interval_seconds` 最多代探 1 个模型**：多个模型同时排空时按候选行 ID 升序轮转
   （`RuntimeChannel.drainProbeSeq`），不会因为排空模型变多而线性放大探测成本。
2. **只借 1 个密钥**：每渠道选一个「排空代探密钥」（优先 NORMAL、其次 DRAIN，且必须有可用凭据；
   `drainProbeKeyLocked`），其余密钥保持各自原有 cron 节奏，不被拖进秒级探测。
3. **同一模型连续代探失败 3 次退避**（`drainProbeBackoffFails`）：退出 15s 轮转，改由该密钥的
   cron 轮次覆盖探测（每轮顺带探一次），既不持续打坏模型，也保留一条自动恢复检测路径；
   任意一次成功或模型退出 `DRAIN` 即清零计数。
4. **代探不回喂密钥状态机**（`probeTarget.DrainOnly`）：代探验证的是模型健康，不是这把密钥的健康，
   否则一把好密钥会被别的模型的故障拖进 `DRAIN`（连带整渠道降级）。代探结果只回喂被探模型。

退出排空后（模型恢复 `NORMAL`）候选集为空，代探密钥自然回到 cron；恢复判定成立即退出排空由状态机
既有的 `RecoveryThreshold` 完成。

轮次语义（保证「成本上限」与「常态对象不被牵连」同时成立）：

- **排空档轮次**（`branch=model_drain`）在配了 `probe_model` 的渠道里**只探被排空的那个模型**——这一轮
  本来就是为它而跑；静态 `probe_model` 只在 cron 轮次出现，不会因为渠道里有模型排空就被按 15s 反复探。
- **普通渠道**（未配 `probe_model`）保持原口径：每轮探该渠道全部非禁用模型（静态作用域本来就覆盖它们），
  探测结果正常回喂密钥与模型状态机，也不参与退避计数——退避只约束「动态代探」，不改变既有常态语义。
- 退避生效后候选为空，排空档消失，调度自然回到 cron；cron 轮次会顺带覆盖已排空模型，保证上游恢复后
  仍能被自动发现（检测粒度 = cron 间隔）。


## 4. 验证

| 项 | 结果 |
| --- | --- |
| 复现脚本（`drainrepro` 标签） | 修复前三档 + 启动即排空全红（0 次探测）；修复后 A/B/C/启动全绿 |
| 首次探测 | 15.0s（门限 ≤18s） |
| 连续间隔 | 15.0s（门限 15±3，无 <10s） |
| 窗口内次数（48s） | 每密钥 ≥3 |
| 恢复耗时 | 12.0s（门限 ≤18s，`recovery_threshold=1`） |
| 恢复后 40s | 0 次探测（已回 cron） |
| 默认套件新增 | `probe_drain_schedule_test.go`（三档×三态等待计算、唤醒、恢复后回 cron、失败原因、广播语义） |
| 门禁 | `go build ./...` / `go vet ./...` / `gofmt`（仅改动文件）/ `go test -race`（30 包）全绿 |

## 5. 边界与风险

- **模型排空代探已收敛为单密钥**：每渠道只借 1 个密钥、每间隔 1 个模型；密钥自身排空（key 档）仍按各自密钥循环，这是产品语义（每把密钥独立健康）。
- **全部内部模型 DISABLED**：`pickKeyProbeTargets` 返回空 → 密钥探测记失败并进 DRAIN，
  该密钥无法靠探测自愈。属既有设计边界（本次未改语义），修好唤醒后表现为「每 15 秒一条失败记录」。
- **禁用态不发探测**：`DISABLED` 仍是终态，只做 15 秒「是否被手动恢复」的复查。
- **排空代探是单密钥 + 单模型轮转**：每渠道每个间隔 1 次代探；普通渠道（未配 `probe_model`）的常态轮次仍是「该密钥探全部模型」，即普通渠道排空期的成本是 1×M（不是 K×M），而 `probe_model` 渠道是 1×1。两者口径不同源于「常态探测对象本来就不同」，已按产品口径固定。
- **`cronx` 周字段**：`*` 解析成 `[0..7]` 导致 `isFullRangeWeek()` 失败，形如 `0 0 0 1 1 *`
  实际退化成每天一次。只影响 cron 等待上限（一年 → 一天），与排空分支无关；修它改变既有 cron
  语义，留作后续任务。
- **多副本**：唤醒是进程内的，各副本按自己的状态与配置独立调度（与既有行为一致）。
