package channel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/domain/channelprotocol"
	"github.com/team/llmgateway/internal/pkg/idgen"
)

// effectiveProtocolBaseURL 求渠道某基础协议的有效上游基址：渠道级 base_url 优先，
// 回落所引用渠道协议（协议级 baseUrl 优先，再回落协议级 base_url）。仅引用协议时渠道内联 base_url 可为空。
func effectiveProtocolBaseURL(rt *RuntimeChannel, protocol string) string {
	if s := strings.TrimSpace(rt.Channel.BaseURL); s != "" {
		return s
	}
	if rt.Protocol != nil {
		return rt.Protocol.EffectiveBaseURL(protocol)
	}
	return ""
}

// effectiveProtocolPath 求某基础协议的有效端点路径：渠道协议 path 覆盖优先，回落内置默认。
func effectiveProtocolPath(rt *RuntimeChannel, protocol, defaultPath string) string {
	if rt.Protocol != nil {
		if spec, ok := rt.Protocol.Protocols[protocol]; ok && strings.TrimSpace(spec.Path) != "" {
			return strings.TrimSpace(spec.Path)
		}
	}
	return defaultPath
}

// renderAuth 把凭据按基础协议隐含方式渲染为注入头名/头值。
// 无凭据时返回空头值，由调用方跳过注入（与网关 injectProtocolAuth 语义一致）。
func renderAuth(protocol, cred string) (header, value string) {
	header, scheme := channelprotocol.ProtocolAuth(protocol)
	if cred == "" {
		return header, ""
	}
	value = cred
	if scheme == "bearer" {
		value = "Bearer " + cred
	}
	return header, value
}

// headerOverrideVars 构造「覆盖(set)」的变量表：model 恒取内部模型 ID（与网关热路径口径一致）；
// session_id/request_id 随机生成——探测/模型拉取无请求上下文，随机值保证头非空、行为与真实转发一致。
func headerOverrideVars(model string) map[string]string {
	return map[string]string{
		"model":      model,
		"session_id": strconv.FormatInt(idgen.New(), 10),
		"request_id": strconv.FormatInt(idgen.New(), 10),
	}
}

// joinProbeURL 拼基址与路径（去基址尾 /、路径头 /）。
func joinProbeURL(base, path string) string {
	return strings.TrimRight(base, "/") + "/" + strings.TrimLeft(path, "/")
}

// probeRequest 求一次探测的目标 URL 与鉴权头名/值：按渠道协议解析有效 baseURL/path/auth
// （内联渠道回落 base_url + /chat/completions + Bearer）。base 为空时返回空 target，由调用方以可读错误处理。
// 探测语义固定为 OpenAI 兼容的最小对话请求，故协议键取 openai-chat。
func (m *Manager) probeRequest(ch *RuntimeChannel, cred string) (target, authHeader, authValue string) {
	protocol := channelprotocol.ProtocolOpenAIChat
	header, value := renderAuth(protocol, cred)
	base := effectiveProtocolBaseURL(ch, protocol)
	if strings.TrimSpace(base) == "" {
		return "", header, value
	}
	path := effectiveProtocolPath(ch, protocol, "/chat/completions")
	return joinProbeURL(base, path), header, value
}

// ProbeUsage 探测请求的三段 token 开销（口径与 billing.Usage 一致）：
//   - InputTokens：纯输入，不含缓存读（上游缓存写 token 属于输入，已含在其中）；
//   - OutputTokens：生成 token，含推理；
//   - CachedTokens：命中提示词缓存读取的输入；
//   - TotalTokens：优先采用上游 total_tokens（其定义即 prompt+completion，等于三段之和）；
//     上游缺失(0)时按三段求和兜底。
//
// probe_logs 与探测记账（系统身份）共用该口径，不再保留上游原始的「输入含缓存读」形态。
type ProbeUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	CachedTokens int `json:"cached_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ProbeOutcome 手动触发一轮健康探测的返回结果（供控制台展示）。
// State 为探测后的状态机状态；OK 表示本次探测是否成功（密钥探测以 LastErr 是否为空判定）。
type ProbeOutcome struct {
	OK         bool   `json:"OK"`
	Error      string `json:"Error,omitempty"`
	DurationMS int64  `json:"DurationMS"`
	ModelID    string `json:"ModelID,omitempty"`
	State      string `json:"State"`
}

// nilOr 返回空串兜底。
func nilOr(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// keyProbeLoop 密钥级探测循环（每密钥一个独立 goroutine）：
// 按该密钥及其渠道内各内部模型的**当前状态**调度（正常态 cron、排空态秒级、禁用态跳探）；
// 运行时被摘除则退出。睡眠期间只要出现状态/配置变化就会被唤醒并重算等待，
// 因此排空可以发生在等待中途而不被漏探。
func (m *Manager) keyProbeLoop(ctx context.Context, keyID int64) {
	for {
		if _, ok := m.GetKeyRuntime(keyID); !ok {
			return
		}
		fired, branch := m.waitUntilNextKeyProbe(ctx, keyID)
		if !fired {
			return
		}
		m.keyProbeOnceUnlessDisabledFor(ctx, keyID, branch)
	}
}

// keyProbeOnceUnlessDisabled 禁用态守卫：仅当密钥当前为非 DISABLED 时才执行真实探测。
// 手动恢复为 NORMAL/DRAIN 后（仅查内存 Machine.State()，不触库），下一轮正常探测。
func (m *Manager) keyProbeOnceUnlessDisabled(ctx context.Context, keyID int64) {
	m.keyProbeOnceUnlessDisabledFor(ctx, keyID, branchCron)
}

// keyProbeOnceUnlessDisabledFor 带本轮调度分支的禁用态守卫（排空轮次只探被排空的模型）。
func (m *Manager) keyProbeOnceUnlessDisabledFor(ctx context.Context, keyID int64, branch string) {
	kr, ok := m.GetKeyRuntime(keyID)
	if !ok || kr.Machine.State() == StateDisabled {
		return
	}
	m.keyProbeOnceWithBranch(ctx, keyID, branch)
}

// disabledRecheckSeconds 禁用态复查周期：禁用期间不探测，只定期看一眼是否被手动恢复。
const disabledRecheckSeconds = 15

// 排空态调度分支（诊断/日志用）。
const (
	branchCron       = "cron"
	branchKeyDrain   = "key_drain"
	branchModelDrain = "model_drain"
	branchDisabled   = "disabled"
)

// drainProbeBackoffFails 同一模型连续代探失败达此次数后退出 15s 轮转、退回 cron 覆盖：
// 上游真的坏掉时不再每间隔都打它，同时保留一条自动恢复检测路径（cron 轮次仍会探一次）。
const drainProbeBackoffFails = 3

// waitUntilNextKeyProbe 等待到下一次探测：到点返回 true；ctx 取消或密钥运行时消失返回 false。
// 等待期间被唤醒（状态/配置变化）时**不重置整段等待**，而是重算截止时刻：
//   - 排空/禁用分支只能提前、不能推后（沿用更早的旧截止），否则同渠道其它对象的流转会把本次
//     探测一路推后，多密钥渠道的首探就会滑出排空间隔；
//   - cron 分支从现在重算（cron 是绝对边界，重算结果一致）：恢复成 NORMAL 后立即回到 cron 语义，
//     不会残留一次「排空遗留」的秒级探测。
func (m *Manager) waitUntilNextKeyProbe(ctx context.Context, keyID int64) (bool, string) {
	var deadline time.Time // 本轮探测的截止时刻，跨唤醒保留
	for {
		// 先订阅唤醒通道再算 wait：否则「算完 wait → 订阅」窗口内发生的状态变化会被漏掉。
		wake := m.probeWake.subscribe()
		wait, branch, ok := m.nextKeyProbeWait(keyID)
		if !ok {
			return false, ""
		}
		if wait < time.Second {
			wait = time.Second
		}
		next := time.Now().Add(wait)
		if branch == branchCron || deadline.IsZero() || next.Before(deadline) {
			deadline = next
		}
		remain := time.Until(deadline)
		if remain <= 0 {
			return true, branch // 截止已过（例如上游探测耗时超过间隔）：立即探测
		}
		t := time.NewTimer(remain)
		select {
		case <-ctx.Done():
			t.Stop()
			return false, ""
		case <-wake:
			t.Stop()
			m.logger.Debug("key probe wait rescheduled", "key_id", keyID, "branch", branch)
		case <-t.C:
			return true, branch
		}
	}
}

// drainProbeKeyLocked 选「排空代探密钥」：每渠道只借 1 个密钥做模型排空代探
// （优先 NORMAL，其次 DRAIN；必须有可用凭据），其余密钥保持各自原有节奏，
// 避免一条渠道被模型排空拖成「全部密钥秒级探测」。调用方须持有 m.mu。
func (m *Manager) drainProbeKeyLocked(channelID int64) (int64, bool) {
	var normal, drain []int64
	for keyID := range m.keyOfCh[channelID] {
		kr, ok := m.keys[keyID]
		if !ok || kr.CredentialPlain == "" {
			continue
		}
		switch kr.Machine.State() {
		case StateNormal:
			normal = append(normal, keyID)
		case StateDrain:
			drain = append(drain, keyID)
		}
	}
	slices.Sort(normal)
	slices.Sort(drain)
	if len(normal) > 0 {
		return normal[0], true
	}
	if len(drain) > 0 {
		return drain[0], true
	}
	return 0, false
}

// drainProbeCandidatesLocked 当前可参与 15s 轮转的排空模型（行 ID 升序）：
// 跳过实体/状态机禁用的，以及连续代探失败已达退避阈值的。调用方须持有 m.mu。
func (m *Manager) drainProbeCandidatesLocked(channelID int64) []int64 {
	out := make([]int64, 0, len(m.mrt[channelID]))
	for id, mr := range m.mrt[channelID] {
		if mr.Model.State == StateDisabled || mr.Machine.State() != StateDrain {
			continue
		}
		if mr.drainProbeFails >= drainProbeBackoffFails {
			continue // 已退避：改由 cron 轮次覆盖
		}
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// drainedModelsLocked 该渠道全部排空模型（含已退避的），供退避后的 cron 覆盖探测。
// 调用方须持有 m.mu。
func (m *Manager) drainedModelsLocked(channelID int64) []int64 {
	out := make([]int64, 0, len(m.mrt[channelID]))
	for id, mr := range m.mrt[channelID] {
		if mr.Model.State == StateDisabled || mr.Machine.State() != StateDrain {
			continue
		}
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// nextKeyProbeWait 计算该密钥下一次探测的等待时长，与调度分支。
//
// 分支取三档对象里「最紧急」的那个（这是排空恢复快慢的唯一决定点）：
//   - 密钥 DISABLED：不探测，disabledRecheckSeconds 复查是否被手动恢复（禁用态不再看模型）；
//   - 密钥 DRAIN：drain_interval_seconds；
//   - 该渠道任一内部模型 DRAIN：该模型的排空间隔（模型级配置优先，缺省继承渠道）。
//     模型没有独立探测循环，由密钥循环代探（见 pickKeyProbeTargets），所以只看密钥状态必然漏这批；
//   - 都没有：渠道 health_probe.interval 的 cron。
//
// 密钥与模型同时排空时取最小间隔。运行时不存在返回 ok=false。
func (m *Manager) nextKeyProbeWait(keyID int64) (time.Duration, string, bool) {
	m.mu.RLock()
	kr, ok := m.keys[keyID]
	if !ok {
		m.mu.RUnlock()
		return 0, "", false
	}
	hp := normalizeHealthProbe(kr.Channel.HealthProbe)
	drainSeconds, branch := 0, branchCron
	switch kr.Machine.State() {
	case StateDisabled:
		drainSeconds, branch = disabledRecheckSeconds, branchDisabled
	case StateDrain:
		drainSeconds, branch = hp.DrainIntervalSeconds, branchKeyDrain
	}
	// 模型层排空：即使密钥仍 NORMAL，也要按排空间隔代探（模型自身没有探测循环）。
	//
	// 必须与 pickKeyProbeTargets 的选型同源——只统计「本轮真的会被探测的模型」：未配 probe_model 时是
	// 全部非禁用模型；配了 probe_model 时只有该模型本身。作用域外的模型排空既探不到也恢复不了，
	// 拿它决定调度会让该渠道全部密钥永久秒级探测、永不回 cron（反向用例的同型退化）。
	// 模型层排空：按排空间隔「定向代探该模型本身」（模型没有独立探测循环）。
	//
	// 只由**排空代探密钥**承担（每渠道 1 个），且只统计可轮转的候选（未退避）：
	// 这样模型排空不会把渠道全部密钥拖进秒级节奏，也不会因为「作用域外模型永远不会被探」而永久空转。
	if branch != branchDisabled {
		if probeKey, ok := m.drainProbeKeyLocked(kr.Channel.ID); ok && probeKey == keyID {
			for _, id := range m.drainProbeCandidatesLocked(kr.Channel.ID) {
				mr := m.mrt[kr.Channel.ID][id]
				if mr == nil {
					continue
				}
				iv := mergeHealthProbe(mr.Model.HealthProbe, kr.Channel.HealthProbe).DrainIntervalSeconds
				if iv <= 0 {
					iv = hp.DrainIntervalSeconds
				}
				if drainSeconds > 0 && iv >= drainSeconds {
					continue
				}
				drainSeconds, branch = iv, branchModelDrain
			}
		}
	}
	m.mu.RUnlock()

	if drainSeconds > 0 {
		return time.Duration(drainSeconds) * time.Second, branch, true
	}
	return cronWait(hp.Interval, m.now()), branch, true
}

// cronWait 返回距下次 cron 匹配的时间；表达式非法时回退 60 秒。
func cronWait(expr string, now time.Time) time.Duration {
	spec := cronSpec(expr)
	if spec == nil {
		return 60 * time.Second
	}
	next := spec.Next(now)
	if next.IsZero() {
		return time.Hour
	}
	return next.Sub(now)
}

// keyProbeOnce 执行一轮密钥级探测（模型无独立探测循环）：
//   - 凭据为空/解密失败 → 探测失败记录（level='key'，不发起请求）；
//   - 目标选型为空（全部禁用/无模型）→ 探测失败记录（level='key'，不回退 GET /models）；
//   - 否则以该密钥明文凭据作 Bearer，向渠道 base_url 对全部可用模型逐一发起最小对话探测。
func (m *Manager) keyProbeOnce(ctx context.Context, keyID int64) {
	// 无分支上下文（手动探测等）：按常态口径选型（静态作用域 + 已退避模型的 cron 覆盖）。
	m.keyProbeOnceWithBranch(ctx, keyID, branchCron)
}

// keyProbeOnceWithBranch 按本轮调度分支选型：排空档轮次只探「被排空的那个模型」，
// 常态档轮次走静态作用域（含已退避模型的 cron 覆盖）。
func (m *Manager) keyProbeOnceWithBranch(ctx context.Context, keyID int64, branch string) {
	kr, ok := m.GetKeyRuntime(keyID)
	if !ok {
		return
	}
	if kr.CredentialPlain == "" {
		errMsg := "密钥凭据为空或解密失败，无法探测"
		m.markKeyProbeResult(keyID, errMsg)
		m.feedKeyAndTransit(keyID, Feedback{IsSuccess: false, HasProbe: true, Now: m.now()})
		m.recordProbe(ctx, keyID, "", "key", "", false, errMsg, ProbeUsage{}, 0)
		return
	}
	targets := m.pickKeyProbeTargetsFor(kr, branch)
	if len(targets) == 0 {
		errMsg := "无可用探测模型（全部禁用或无模型）"
		m.markKeyProbeResult(keyID, errMsg)
		m.feedKeyAndTransit(keyID, Feedback{IsSuccess: false, HasProbe: true, Now: m.now()})
		m.recordProbe(ctx, keyID, "", "key", "", false, errMsg, ProbeUsage{}, 0)
		return
	}
	for _, t := range targets {
		m.probeOnce(ctx, keyID, t.Model, t.Level, t.ModelRowID, t.DrainOnly)
	}
}

// probeTarget 单次探测目标（模型 + 层级 + 模型行 ID）。
type probeTarget struct {
	Model      string
	Level      string
	ModelRowID int64 // level='model' 时的 channel_models 行 ID（探测结果回喂模型状态机用）；0=无
	// DrainOnly 排空态动态叠加的「代探」目标：只回喂该模型状态机，不回喂密钥状态机
	//（代探验证的是模型健康，不是密钥健康；否则密钥会被别的模型的故障拖进 DRAIN）。
	DrainOnly bool
}

// pickKeyProbeTargets 密钥级探测目标选型（探测分两层，模型无独立探测循环；probe_model 是偏好不是白名单）：
//  1. 未配置 health_probe.probe_model：模型层全探——每轮探测该渠道全部 state!=DISABLED 的内部模型
//     （level='model'；NORMAL 在前、DRAIN 次之，组内按模型 ID 稳定排序）；
//  2. 显式配置 probe_model：密钥层定向探测——该密钥仅探测此模型（level='key'；能落到模型实体时
//     同时回喂该模型状态机，见 probeTarget.ModelRowID）；
//  3. 全部禁用/无模型 → 返回空切片，探测视为失败（不回退 GET /models）。
func (m *Manager) pickKeyProbeTargets(kr *KeyRuntime) []probeTarget {
	return m.pickKeyProbeTargetsFor(kr, branchCron)
}

// pickKeyProbeTargetsFor 按本轮调度分支选型（排空档只探被排空的模型；常态档走静态作用域）。
func (m *Manager) pickKeyProbeTargetsFor(kr *KeyRuntime, branch string) []probeTarget {
	hp := normalizeHealthProbe(kr.Channel.HealthProbe)
	// 写锁：需要推进「每渠道每间隔最多 1 次代探」的轮转游标。
	m.mu.Lock()
	defer m.mu.Unlock()

	var targets []probeTarget
	seen := make(map[string]bool, 4)
	add := func(t probeTarget) {
		if seen[t.Model] {
			return
		}
		seen[t.Model] = true
		targets = append(targets, t)
	}

	// 排空档轮次：这一轮就是为「被排空的那个模型」而跑，只探它——静态作用域只在 cron 轮次出现，
	// 否则 probe_model 渠道的常态对象会被顺带拖进排空节奏（成本与语义都不对）。
	// 只对「配了 probe_model」的渠道收窄：这类渠道的静态作用域不包含作用域外模型，排空轮次必须专探它，
	// 不能顺带把 probe_model 也拖进排空节奏。普通渠道的静态作用域本来就覆盖全部模型，保持原口径
	//（每轮探全部非禁用模型，探测结果正常回喂密钥与模型，且不计退避）。
	if branch == branchModelDrain && hp.ProbeModel != "" {
		if probeKey, ok := m.drainProbeKeyLocked(kr.Channel.ID); ok && probeKey == kr.Key.ID {
			if cands := m.drainProbeCandidatesLocked(kr.Channel.ID); len(cands) > 0 {
				idx := 0
				if rt := m.rt[kr.Channel.ID]; rt != nil {
					idx = rt.drainProbeSeq % len(cands)
					rt.drainProbeSeq++
				}
				if mr := m.mrt[kr.Channel.ID][cands[idx]]; mr != nil {
					return []probeTarget{{Model: mr.Model.InternalModelID, Level: "model",
						ModelRowID: cands[idx], DrainOnly: true}}
				}
			}
		}
	}

	// 静态作用域（常态探测对象）。
	if hp.ProbeModel != "" {
		// 定向探测：日志仍记 level='key'（沿用既有口径）。若该模型能落到 channel_models 行，
		// 一并带上 ModelRowID——probeOnce 据此回喂该模型状态机；否则 probe_model 渠道里被排空的
		// 目标模型即使探成功也无人接收，永久 DRAIN。
		t := probeTarget{Model: hp.ProbeModel, Level: "key"}
		for id, mr := range m.mrt[kr.Channel.ID] {
			if mr.Model.InternalModelID == hp.ProbeModel {
				t.ModelRowID = id
				break
			}
		}
		add(t)
	} else {
		var normal, drain []int64
		for id, mr := range m.mrt[kr.Channel.ID] {
			if mr.Model.State == StateDisabled {
				continue
			}
			switch mr.Machine.State() {
			case StateNormal:
				normal = append(normal, id)
			case StateDrain:
				drain = append(drain, id)
			}
		}
		slices.Sort(normal)
		slices.Sort(drain)
		for _, id := range append(normal, drain...) {
			mr := m.mrt[kr.Channel.ID][id]
			add(probeTarget{Model: mr.Model.InternalModelID, Level: "model", ModelRowID: id})
		}
	}

	// 动态叠加：任何进入排空的模型都要被定向代探（probe_model 只是常态偏好，不是白名单）。
	// 每渠道每个间隔最多 1 个模型，多个同时排空时轮转；只由「排空代探密钥」承担。
	if probeKey, ok := m.drainProbeKeyLocked(kr.Channel.ID); ok && probeKey == kr.Key.ID {
		// 常态轮次不代探（轮转只在排空轮次推进）；只有「候选全部退避」时才顺带覆盖一次。
		if len(m.drainProbeCandidatesLocked(kr.Channel.ID)) == 0 && kr.Machine.State() == StateNormal {
			// 全部退避：退回 cron 覆盖——密钥仍是常态节奏，只在自己的 cron 轮次顺带探一次，
			// 保证上游恢复后仍能被自动发现。
			for _, id := range m.drainedModelsLocked(kr.Channel.ID) {
				if mr := m.mrt[kr.Channel.ID][id]; mr != nil {
					add(probeTarget{Model: mr.Model.InternalModelID, Level: "model", ModelRowID: id, DrainOnly: true})
				}
			}
		}
	}
	return targets
}

// probeOnce 执行一次真实最小对话探测并驱动状态机：
// 不过限流器、不占用会话、不进可靠性窗口（HasProbe=true）；结果落 probe_logs 与记账钩子。
// level='model'（带了 modelRowID）时同时回喂该内部模型状态机——定时探测据此恢复被真实调用
// 打到 DRAIN 的模型，避免模型级排空成为永久死锁（此前只回喂密钥状态机）。
func (m *Manager) probeOnce(ctx context.Context, keyID int64, probeModel, level string, modelRowID int64, drainOnly bool) {
	kr, ok := m.GetKeyRuntime(keyID)
	if !ok {
		return
	}
	hp := normalizeHealthProbe(kr.Channel.HealthProbe)
	timeout := time.Duration(hp.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	chRT, ok := m.GetRuntime(kr.Channel.ID)
	if !ok {
		// 兜底：补齐渠道协议，避免协议渠道探测回落默认 Bearer 鉴权。
		chRT = &RuntimeChannel{Channel: *kr.Channel}
		chRT.Protocol = m.lookupChannelProtocol(ctx, kr.Channel.ChannelProtocolID)
	}
	target, authHeader, authValue := m.probeRequest(chRT, kr.CredentialPlain)
	// 探测与真实转发同源：套用协议「覆盖(set)」头，否则上游要求自定义头（如会话标识）时探测必失败。
	policy := chRT.Protocol.EffectiveHeaderPolicy()

	start := m.now()
	fb, usage, errMsg := m.doProbeAuth(ctx, target, authHeader, authValue, probeModel, policy, kr.CredentialPlain, timeout)
	dur := m.now().Sub(start).Milliseconds()

	m.markChannelProbeResult(kr.Key.ChannelID)
	if !drainOnly {
		m.markKeyProbeResult(keyID, errMsg)
	}
	if modelRowID > 0 {
		m.markModelProbeResult(kr.Key.ChannelID, modelRowID, errMsg)
	}
	// 代探（drainOnly）只回喂模型状态机：验证的是模型健康，不是这把密钥的健康，
	// 否则一把好密钥会被别的模型的故障拖进 DRAIN。
	if !drainOnly {
		m.feedKeyAndTransit(keyID, ProbeFeedback(fb))
	}
	if modelRowID > 0 {
		m.feedModelAndTransit(kr.Key.ChannelID, modelRowID, ProbeFeedback(fb))
		m.mu.Lock()
		if mr := m.mrt[kr.Key.ChannelID][modelRowID]; mr != nil {
			if fb.IsSuccess {
				mr.drainProbeFails = 0
			} else if drainOnly {
				mr.drainProbeFails++
			}
		}
		m.mu.Unlock()
	}
	m.recordProbe(ctx, keyID, probeModel, level, target, fb.IsSuccess, errMsg, usage, dur)
}

// ProbeFeedback 用标记探测属性的反馈包装为普通反馈（保持语义一致）。
func ProbeFeedback(fb Feedback) Feedback { return fb }

// doProbeAuth 构建并执行最小对话探测请求，解析响应 usage 与状态分类（鉴权头名/值可定制，
// 以支持 x-api-key/raw 等渠道协议鉴权），并套用协议「覆盖(set)」请求头（与网关热路径同源）。
// 2xx 成功；401/403 认证失败（→禁用）；429 上游限流；其余失败；超时标记 IsTimeout。
func (m *Manager) doProbeAuth(ctx context.Context, url, authHeader, authValue, model string, policy channelprotocol.HeaderPolicy, cred string, timeout time.Duration) (Feedback, ProbeUsage, string) {
	fb := Feedback{HasProbe: true, Now: m.now()}
	var usage ProbeUsage
	if strings.TrimSpace(url) == "" {
		return fb, usage, "渠道未配置 Base URL，无法探测"
	}
	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     false,
	})
	if err != nil {
		return fb, usage, err.Error()
	}

	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(cctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return fb, usage, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	if authValue != "" {
		req.Header.Set(authHeader, authValue)
	}
	// 「覆盖(set)」：与网关热路径同一引擎（channelprotocol.ApplySet），支持 {api_key}/{model}/{session_id}/{request_id} 等变量。
	channelprotocol.ApplySet(req.Header, policy.Set, cred, headerOverrideVars(model), nil)

	resp, err := m.probeHTTP.Do(req)
	if err != nil {
		var nerr net.Error
		if errors.As(err, &nerr) && nerr.Timeout() {
			fb.IsTimeout = true
		} else if cctx.Err() != nil {
			fb.IsTimeout = true
		}
		return fb, usage, err.Error()
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		fb.IsSuccess = true
		// 尽量解析 token 开销（探测记录与账单用）；解析失败不阻塞探测判定。
		var body struct {
			Usage struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				TotalTokens      int `json:"total_tokens"`
				// DeepSeek 原生风格：命中数在顶层 prompt_cache_hit_tokens（与 cached_tokens 同义）。
				PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
				PromptTokensDetails  struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
			// 三段口径（与 billing.Usage 一致）：探测固定走 OpenAI 兼容请求，prompt_tokens 是「含」口径，
			// 命中缓存的 token 必须从输入中剔除；同时兜底 DeepSeek 原生顶层字段。
			cached := body.Usage.PromptTokensDetails.CachedTokens
			if cached == 0 {
				cached = body.Usage.PromptCacheHitTokens
			}
			input := body.Usage.PromptTokens - cached
			if input < 0 {
				input = 0
			}
			usage.InputTokens = input
			usage.OutputTokens = body.Usage.CompletionTokens
			usage.CachedTokens = cached
			// 上游 total_tokens 的定义即 prompt_tokens + completion_tokens，等于「输入+缓存读+输出」三段之和，
			// 故直接采用上游值；仅在缺失（为 0）时按三段求和兜底。
			usage.TotalTokens = body.Usage.TotalTokens
			if usage.TotalTokens == 0 {
				usage.TotalTokens = input + cached + body.Usage.CompletionTokens
			}
		}
		return fb, usage, ""
	case resp.StatusCode == http.StatusTooManyRequests:
		fb.Is429 = true
		return fb, usage, fmt.Sprintf("probe http status: %d", resp.StatusCode)
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		fb.IsAuthFailure = true
		return fb, usage, fmt.Sprintf("probe auth failure: %d（凭据失效）", resp.StatusCode)
	default:
		return fb, usage, fmt.Sprintf("probe http status: %d", resp.StatusCode)
	}
}

// markKeyProbeResult 更新该密钥的 lastProbe 与 LastErr（锁内写）。
func (m *Manager) markKeyProbeResult(keyID int64, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if kr, ok := m.keys[keyID]; ok {
		kr.lastProbe = m.now()
		kr.Key.LastErr = errMsg
	}
}

// markModelProbeResult 更新该内部模型的 lastProbe（锁内写）。
// 不改 LastErr：模型「最近错误」不展示，避免引入瞬时 429 噪音，保持现有语义。
func (m *Manager) markModelProbeResult(channelID, modelRowID int64, errMsg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mr := m.mrt[channelID][modelRowID]; mr != nil {
		mr.lastProbe = m.now()
	}
}

// markChannelProbeResult 更新该渠道的 lastProbe（锁内写）。
func (m *Manager) markChannelProbeResult(channelID int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if rt := m.rt[channelID]; rt != nil {
		rt.lastProbe = m.now()
	}
}

// feedKeyAndTransit 向该密钥状态机注入反馈，捕获状态流转并锁外持久化（channel_key_events + channel_keys.state）。
// 仅驱动密钥状态机（渠道为配置模板，模型为配置实体，均不在此探测链路回喂）。
func (m *Manager) feedKeyAndTransit(keyID int64, fb Feedback) {
	var tr *pendingTransition
	m.mu.Lock()
	if kr, ok := m.keys[keyID]; ok {
		prev := kr.Machine.State()
		kr.Machine.Feed(fb)
		if cur := kr.Machine.State(); cur != prev {
			tr = &pendingTransition{from: prev, to: cur, reason: kr.Machine.LastReason()}
		}
	}
	m.mu.Unlock()

	if tr != nil {
		// 探测自身造成的流转（首次失败→DRAIN、连续成功→NORMAL）同样要让调度重算。
		m.wakeProbeScheduling()
		m.persistKeyTransition(keyID, tr.from, tr.to, tr.reason)
		m.logger.Info("key state transition", "key_id", keyID,
			"from", tr.from, "to", tr.to, "reason", tr.reason)
	}
}

// recordProbe 落库探测记录（密钥维度）并触发记账钩子（系统身份，见装配层）；两者失败仅记日志。
// ProbeLog.ChannelKeyID 对应 probe_logs.channel_key_id（密钥维度）。
func (m *Manager) recordProbe(ctx context.Context, keyID int64, modelID, level, target string, ok bool, errMsg string, usage ProbeUsage, durationMS int64) {
	if m.cmStore != nil {
		pctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		if err := m.cmStore.InsertProbeLog(pctx, &ProbeLog{
			ChannelKeyID: keyID,
			ModelID:      modelID,
			Level:        level,
			Target:       target,
			OK:           ok,
			Error:        errMsg,
			InputTokens:  usage.InputTokens,
			OutputTokens: usage.OutputTokens,
			CachedTokens: usage.CachedTokens,
			TotalTokens:  usage.TotalTokens,
			DurationMS:   int(durationMS),
		}); err != nil {
			m.logger.Error("insert probe log failed", "key_id", keyID, "err", err)
		}
		cancel()
	}
	if m.onProbe != nil {
		// C1: 改为 ChannelKeyID 字段——当前 keyID 经 ProbeRecorder 首参、由装配层 RecordReq.ChannelID 落库。
		if err := m.onProbe(ctx, keyID, modelID, target, ok, errMsg, usage, durationMS); err != nil {
			m.logger.Error("record probe billing failed", "key_id", keyID, "err", err)
		}
	}
}

// ServiceStatusString 供 UI 展示 LastErr（空值显示占位）。
func ServiceStatusString(s string) string { return nilOr(s) }
