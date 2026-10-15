package channel

import (
	"sort"
	"time"
)

// ChannelDetail 渠道层运行明细：渠道视图 + 状态机指标 + 限流窗口用量/额度 + 内部模型/密钥明细。
type ChannelDetail struct {
	View     ChannelView
	Metrics  MachineMetrics
	RPM      int
	TPM      int64
	RPMLimit int
	TPMLimit int64
	Models   []ModelDetail
	Keys     []KeyDetail
}

// ModelDetail 内部模型层运行明细：模型视图 + 状态机指标 + 限流窗口用量/额度。
type ModelDetail struct {
	View     ModelView
	Metrics  MachineMetrics
	RPM      int
	TPM      int64
	RPMLimit int
	TPMLimit int64
}

// KeyDetail 密钥层运行明细：密钥视图 + 状态机指标 + 限流窗口用量/额度 + 并发会话上限。
type KeyDetail struct {
	View        KeyRuntimeView
	Metrics     MachineMetrics
	RPM         int
	TPM         int64
	RPMLimit    int
	TPMLimit    int64
	MaxSessions int
	// ActiveSessions 密钥存活会话数，由 console 聚合端点经路由会话注册表填充；
	// RuntimeDetails() 本身无会话注册表，故此处默认 0。
	ActiveSessions int
}

// RuntimeDetails 一次性只读聚合渠道三层运行时明细（渠道/内部模型/密钥），
// 供管理员「服务状态」页导出状态机指标与限流窗口用量。
// 单锁单切片：全程仅一次 m.mu.RLock；允许与路由热路径毫秒级错位（最终一致，设计决策 13）。
// 渠道/模型/密钥各层内部按 ID 升序输出；空渠道/空模型/空密钥返回空切片（非 nil）。
func (m *Manager) RuntimeDetails() []ChannelDetail {
	m.mu.RLock()
	defer m.mu.RUnlock()
	now := m.now() // 入口取一次，复用于各层 Usage，避免多次调用产生时钟漂移

	chIDs := make([]int64, 0, len(m.rt))
	for id := range m.rt {
		chIDs = append(chIDs, id)
	}
	sort.Slice(chIDs, func(i, j int) bool { return chIDs[i] < chIDs[j] })

	out := make([]ChannelDetail, 0, len(m.rt))
	for _, chID := range chIDs {
		rt := m.rt[chID]
		if rt == nil {
			continue
		}
		c := rt.Channel

		var lp *time.Time
		if !rt.lastProbe.IsZero() {
			t := rt.lastProbe
			lp = &t
		}
		rl := normalizeRateLimit(c.RateLimit)
		srcRefs := m.boundTags[c.ID]
		btCp := make([]TagRef, len(srcRefs))
		copy(btCp, srcRefs)
		cRPM, cTPM := rt.Limiter.Usage(now)

		detail := ChannelDetail{
			View: ChannelView{
				ID:          c.ID,
				Name:        c.Name,
				Protocol:    c.Protocol,
				BaseURL:     c.BaseURL,
				Tags:        c.Tags,
				BoundTags:   btCp,
				Priority:    c.Priority,
				State:       rt.Machine.State(),
				LastErr:     rt.LastErr,
				LastProbe:   lp,
				RateLimit:   rl,
				HealthProbe: normalizeHealthProbe(c.HealthProbe),
				Reliability: normalizeReliability(c.Reliability),
				MaxSessions: rl.MaxConcurrent,
				CreatedAt:   c.CreatedAt,
				UpdatedAt:   c.UpdatedAt,
			},
			Metrics:  rt.Machine.Metrics(),
			RPM:      cRPM,
			TPM:      cTPM,
			RPMLimit: rt.Limiter.RPMLimit(),
			TPMLimit: rt.Limiter.TokenLimit(),
			Models:   make([]ModelDetail, 0),
			Keys:     make([]KeyDetail, 0),
		}

		// 模型层：状态机指标 + 限流用量；视图填充与 ModelViews 同源（配置缺省以渠道兜底）。
		for _, mr := range m.mrt[chID] {
			if mr == nil {
				continue
			}
			var mLp *time.Time
			if !mr.lastProbe.IsZero() {
				t := mr.lastProbe
				mLp = &t
			}
			mRPM, mTPM := mr.Limiter.Usage(now)
			mRL := normalizeRateLimit(mergeRateLimit(mr.Model.RateLimit, c.RateLimit))
			detail.Models = append(detail.Models, ModelDetail{
				View: ModelView{
					ID:              mr.Model.ID,
					InternalModelID: mr.Model.InternalModelID,
					ExternalModelID: mr.Model.ExternalModelID,
					State:           mr.Machine.State(),
					LastErr:         mr.LastErr,
					LastProbe:       mLp,
					RateLimit:       mRL,
					HealthProbe:     normalizeHealthProbe(mergeHealthProbe(mr.Model.HealthProbe, c.HealthProbe)),
					Reliability:     normalizeReliability(mergeReliability(mr.Model.Reliability, c.Reliability)),
					MaxSessions:     mRL.MaxConcurrent,
				},
				Metrics:  mr.Machine.Metrics(),
				RPM:      mRPM,
				TPM:      mTPM,
				RPMLimit: mr.Limiter.RPMLimit(),
				TPMLimit: mr.Limiter.TokenLimit(),
			})
		}
		sort.Slice(detail.Models, func(i, j int) bool { return detail.Models[i].View.ID < detail.Models[j].View.ID })

		// 密钥层：状态机指标 + 限流用量 + 并发会话上限；视图填充与 KeyViews 同源。
		for keyID := range m.keyOfCh[chID] {
			kr, ok := m.keys[keyID]
			if !ok || kr == nil {
				continue
			}
			var kLp *time.Time
			if !kr.lastProbe.IsZero() {
				t := kr.lastProbe
				kLp = &t
			}
			kRPM, kTPM := kr.Limiter.Usage(now)
			detail.Keys = append(detail.Keys, KeyDetail{
				View: KeyRuntimeView{
					KeyID:          kr.Key.ID,
					Name:           kr.Key.Name,
					State:          kr.Machine.State(),
					LastErr:        kr.Key.LastErr,
					CredentialTail: credentialTail(kr.CredentialPlain),
					LastProbe:      kLp,
					DrainReasons:   append([]string(nil), kr.Machine.DrainReasons()...),
				},
				Metrics:     kr.Machine.Metrics(),
				RPM:         kRPM,
				TPM:         kTPM,
				RPMLimit:    kr.Limiter.RPMLimit(),
				TPMLimit:    kr.Limiter.TokenLimit(),
				MaxSessions: normalizeRateLimit(kr.Channel.RateLimit).MaxConcurrent,
			})
		}
		sort.Slice(detail.Keys, func(i, j int) bool { return detail.Keys[i].View.KeyID < detail.Keys[j].View.KeyID })

		out = append(out, detail)
	}
	return out
}
