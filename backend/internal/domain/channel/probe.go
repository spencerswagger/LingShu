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
	"strings"
	"time"
)

// ProbeUsage 探测请求的 token 开销（供记账）。
type ProbeUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CachedTokens     int `json:"cached_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// nilOr 返回空串兜底。
func nilOr(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// keyProbeLoop 密钥级探测循环（每密钥一个独立 goroutine）：
// 按该密钥当前状态调度（正常态 cron、排空态秒级、禁用态跳探）；运行时被摘除则退出。
// DISABLED 态不执行真实探测：sleepUntilNextKeyProbe 按 15s 周期复查内存 Machine.State()，
// 醒后经 keyProbeOnceUnlessDisabled 校验仍为 DISABLED 则跳过（不产生 HTTP/不写 probe_logs/不记账）。
func (m *Manager) keyProbeLoop(ctx context.Context, keyID int64) {
	for {
		if _, ok := m.GetKeyRuntime(keyID); !ok {
			return
		}
		if !m.sleepUntilNextKeyProbe(ctx, keyID) {
			return
		}
		m.keyProbeOnceUnlessDisabled(ctx, keyID)
	}
}

// keyProbeOnceUnlessDisabled 禁用态守卫：仅当密钥当前为非 DISABLED 时才执行真实探测。
// 手动恢复为 NORMAL/DRAIN 后（仅查内存 Machine.State()，不触库），下一轮正常探测。
func (m *Manager) keyProbeOnceUnlessDisabled(ctx context.Context, keyID int64) {
	kr, ok := m.GetKeyRuntime(keyID)
	if !ok || kr.Machine.State() == StateDisabled {
		return
	}
	m.keyProbeOnce(ctx, keyID)
}

// sleepUntilNextKeyProbe 依据该密钥当前状态计算下一次探测时间并休眠；ctx 取消或密钥运行时消失返回 false。
// 禁用：不探测（15s 复查是否被手动恢复）；排空：drain_interval_seconds；正常：渠道 health_probe.interval cron。
// 探测配置（interval/drain/timeout）取自共享渠道配置模板，状态取自该密钥状态机。
func (m *Manager) sleepUntilNextKeyProbe(ctx context.Context, keyID int64) bool {
	m.mu.RLock()
	kr, ok := m.keys[keyID]
	if !ok {
		m.mu.RUnlock()
		return false
	}
	hp := normalizeHealthProbe(kr.Channel.HealthProbe)
	state := kr.Machine.State()
	m.mu.RUnlock()

	var wait time.Duration
	switch {
	case state == StateDisabled:
		wait = 15 * time.Second
	case state == StateDrain:
		wait = time.Duration(hp.DrainIntervalSeconds) * time.Second
	default:
		wait = cronWait(hp.Interval, m.now())
	}
	if wait < time.Second {
		wait = time.Second
	}
	t := time.NewTimer(wait)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
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
	targets := m.pickKeyProbeTargets(kr)
	if len(targets) == 0 {
		errMsg := "无可用探测模型（全部禁用或无模型）"
		m.markKeyProbeResult(keyID, errMsg)
		m.feedKeyAndTransit(keyID, Feedback{IsSuccess: false, HasProbe: true, Now: m.now()})
		m.recordProbe(ctx, keyID, "", "key", "", false, errMsg, ProbeUsage{}, 0)
		return
	}
	for _, t := range targets {
		m.probeOnce(ctx, keyID, t.Model, t.Level)
	}
}

// probeTarget 单次探测目标（模型 + 层级）。
type probeTarget struct {
	Model string
	Level string
}

// pickKeyProbeTargets 密钥级探测目标选型（探测分两层，模型无独立探测循环）：
//  1. 未配置 health_probe.probe_model：模型层全探——每轮探测该渠道全部 state!=DISABLED 的内部模型
//     （level='model'；NORMAL 在前、DRAIN 次之，组内按模型 ID 稳定排序）；
//  2. 显式配置 probe_model：密钥层定向探测——该密钥仅探测此模型（level='key'）；
//  3. 全部禁用/无模型 → 返回空切片，探测视为失败（不回退 GET /models）。
func (m *Manager) pickKeyProbeTargets(kr *KeyRuntime) []probeTarget {
	hp := normalizeHealthProbe(kr.Channel.HealthProbe)
	if hp.ProbeModel != "" {
		return []probeTarget{{Model: hp.ProbeModel, Level: "key"}}
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
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
	targets := make([]probeTarget, 0, len(normal)+len(drain))
	for _, id := range normal {
		targets = append(targets, probeTarget{Model: m.mrt[kr.Channel.ID][id].Model.InternalModelID, Level: "model"})
	}
	for _, id := range drain {
		targets = append(targets, probeTarget{Model: m.mrt[kr.Channel.ID][id].Model.InternalModelID, Level: "model"})
	}
	return targets
}

// probeOnce 执行一次真实最小对话探测并驱动该密钥状态机：
// 不过限流器、不占用会话、不进可靠性窗口（HasProbe=true）；结果落 probe_logs 与记账钩子。
func (m *Manager) probeOnce(ctx context.Context, keyID int64, probeModel, level string) {
	kr, ok := m.GetKeyRuntime(keyID)
	if !ok {
		return
	}
	hp := normalizeHealthProbe(kr.Channel.HealthProbe)
	timeout := time.Duration(hp.TimeoutMS) * time.Millisecond
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	base := strings.TrimRight(kr.Channel.BaseURL, "/")
	target := base + "/chat/completions"

	start := m.now()
	fb, usage, errMsg := m.doProbe(ctx, target, kr.CredentialPlain, probeModel, timeout)
	dur := m.now().Sub(start).Milliseconds()

	m.markKeyProbeResult(keyID, errMsg)
	m.feedKeyAndTransit(keyID, ProbeFeedback(fb))
	m.recordProbe(ctx, keyID, probeModel, level, target, fb.IsSuccess, errMsg, usage, dur)
}

// ProbeFeedback 用标记探测属性的反馈包装为普通反馈（保持语义一致）。
func ProbeFeedback(fb Feedback) Feedback { return fb }

// doProbe 构建并执行最小对话探测请求，解析响应 usage 与状态分类。
// 2xx 成功；401/403 认证失败（→禁用）；429 上游限流；其余失败；超时标记 IsTimeout。
func (m *Manager) doProbe(ctx context.Context, url, auth, model string, timeout time.Duration) (Feedback, ProbeUsage, string) {
	fb := Feedback{HasProbe: true, Now: m.now()}
	var usage ProbeUsage
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
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}

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
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				TotalTokens         int `json:"total_tokens"`
				PromptTokensDetails struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&body); err == nil {
			usage.PromptTokens = body.Usage.PromptTokens
			usage.CompletionTokens = body.Usage.CompletionTokens
			usage.CachedTokens = body.Usage.PromptTokensDetails.CachedTokens
			usage.TotalTokens = body.Usage.TotalTokens
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
			InputTokens:  usage.PromptTokens,
			OutputTokens: usage.CompletionTokens,
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
