// cost_pricing.go 模型定价曲线：按真实计费公式在后端逐 token 出点。
//
// 公式（与 billing.ComputeCredits 完全一致，不另立一套）：
//
//	credits = Σ(段 token × 段费率) × timeCoeff × ctxCoeff ÷ R
//
// 三个系数语义：
//   - Σ(段 token × 段费率)：三段（输入/缓存读/输出）求和，输入只算纯输入、缓存读单列；
//   - timeCoeff：单值，取**结算时刻**命中的时段段系数（跨时段请求按结算时刻整笔计一个系数，
//     不按分钟切分——spencer 已确认保持现状）；
//   - ctxCoeff：逐轮按「输入 + 缓存读」（= 上下文长度）查上下文分档，因此长会话在某一轮会跳档；
//   - R：积分换算系数（billing.r，默认 10000）。
//
// 两条线分别用各自的配置：售价线 = external_models（sale_rates/time_config/context_tiers），
// 成本线 = channel_models（cost_rates/time_config/context_tiers），不互相借用。
//
// 时段/分档的取值优先级与结算完全一致：模型级配置非空则用模型级，否则回落全局
// sys_configs（billing.time_config / billing.context_tiers），并回报实际来源。
package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/team/llmgateway/internal/domain/billing"
	"github.com/team/llmgateway/internal/pkg/decimalx"
	"github.com/team/llmgateway/internal/pkg/resp"
)

// 定价曲线的参数默认值与上限。
const (
	pricingCurveDefaultStep     = 1000
	pricingCurveMaxStep         = 100000
	pricingCurveDefaultMaxToken = 200000
	pricingCurveMaxMaxToken     = 5000000
	pricingCurveMaxPoints       = 2000
	pricingCurveDefaultHitRate  = 0.9 // 缓存读占输入比例，与 routing.sample_profile 默认同源
	pricingCurveDefaultInOut    = 10.0
	pricingCurveMaxInOutRatio   = 1e6
	// pricingCurveMaxLines ChannelModelIDs 单次最多几条线（超了直接 400，不静默截断）。
	pricingCurveMaxLines = 10
)

// pricingCurveParams 曲线采样参数（已收敛到合法范围）。
type pricingCurveParams struct {
	Step       int64
	MaxTokens  int64
	HitRate    float64
	InOutRatio float64
	At         time.Time
}

// pricingPoint 曲线上的一个点（横轴 = 上下文长度 = 输入 + 缓存读）。
type pricingPoint struct {
	Tokens          int64   `json:"Tokens"`
	InputTokens     int64   `json:"InputTokens"`
	CacheReadTokens int64   `json:"CacheReadTokens"`
	OutputTokens    int64   `json:"OutputTokens"`
	CtxCoeff        float64 `json:"CtxCoeff"`
	Credits         float64 `json:"Credits"`        // 含时段 + 分档系数（真实计费口径）
	CreditsNoCoeff  float64 `json:"CreditsNoCoeff"` // 不含任何系数（仅费率 × token ÷ R）
	OverTier        bool    `json:"OverTier"`       // 落在分档之外：真实计费会失败，前端断线红标
}

// pricingTierBreak 跳档拐点（供前端 markLine）。
type pricingTierBreak struct {
	Tokens    int64   `json:"Tokens"`
	FromCoeff float64 `json:"FromCoeff"`
	ToCoeff   float64 `json:"ToCoeff"`
}

// pricingTokenRange 一段 token 区间（超档区段）。
type pricingTokenRange struct {
	FromTokens int64 `json:"FromTokens"`
	ToTokens   int64 `json:"ToTokens"`
}

// pricingTimeOption 时段下拉可选项（来自全局 billing.time_config 的周期段）。
type pricingTimeOption struct {
	Name  string  `json:"Name"`
	Start string  `json:"Start"`
	End   string  `json:"End"`
	Coeff float64 `json:"Coeff"`
}

// pricingLine 一条定价线（售价线 / 成本线）。
type pricingLine struct {
	Available        bool                `json:"Available"`
	Source           string              `json:"Source"` // external_model | channel_model
	Name             string              `json:"Name"`
	ExternalModelID  int64               `json:"ExternalModelID"`
	ChannelModelID   int64               `json:"ChannelModelID"`
	ChannelID        int64               `json:"ChannelID"`
	ChannelName      string              `json:"ChannelName"`
	InternalModelID  string              `json:"InternalModelID"`
	Rates            billing.WireRates   `json:"Rates"`
	TimeZone         string              `json:"TimeZone"`
	TimeCoeff        float64             `json:"TimeCoeff"`
	TimeConfigSource string              `json:"TimeConfigSource"` // model | global
	TierSource       string              `json:"TierSource"`       // model | global | none
	Tiers            []billing.TierRule  `json:"Tiers"`
	Points           []pricingPoint      `json:"Points"`
	TierBreaks       []pricingTierBreak  `json:"TierBreaks"`
	OverTier         []pricingTokenRange `json:"OverTier"`
	Reason           string              `json:"Reason"` // Available=false 时的原因说明
}

// pricingCurveResponse 定价曲线响应。Sale/Cost 恒为对象（缺配置时 Available=false + Reason），不返回 null。
//
// Lines 只在传了 ChannelModelIDs 时出现（omitempty）：不传该参数时该字段**整个不出现**，
// 响应与改造前逐字节一致（有 testdata 里的 golden 快照兜底）。
type pricingCurveResponse struct {
	At               string              `json:"At"`
	TimeZone         string              `json:"TimeZone"`
	R                int64               `json:"R"`
	Step             int64               `json:"Step"`
	MaxTokens        int64               `json:"MaxTokens"`
	PointCount       int                 `json:"PointCount"`
	HitRate          float64             `json:"HitRate"`
	InOutRatio       float64             `json:"InOutRatio"`
	TimeOptions      []pricingTimeOption `json:"TimeOptions"`
	DefaultTimeCoeff float64             `json:"DefaultTimeCoeff"`
	Sale             pricingLine         `json:"Sale"`
	Cost             pricingLine         `json:"Cost"`
	// Lines 多渠道多线对比（可选）：每行一个 (渠道, 渠道内部模型)，各带自己的成本线与售价线。
	// 缺省 nil → 序列化时整个键不出现（向后兼容的硬要求，见 TestHandleCostPricingCurve_GoldenUnchanged）。
	Lines []pricingMultiLine `json:"Lines,omitempty"`
}

// pricingMultiLine 多线对比里的一条线。
//
// 键语义：ChannelModelID 是请求里给的渠道内部模型 ID（不存在也原样回吐，方便前端对号）；
// Available/Reason 取**成本线**的判定（成本费率缺失=画不出成本线），售价线另看 Sale.Available。
type pricingMultiLine struct {
	LineIndex         int         `json:"LineIndex"` // 去重后的请求顺序下标，供前端图例排序
	ChannelModelID    int64       `json:"ChannelModelID"`
	ChannelID         int64       `json:"ChannelID"`
	ChannelName       string      `json:"ChannelName"`
	InternalModelID   string      `json:"InternalModelID"`
	ExternalModelID   int64       `json:"ExternalModelID"`
	ExternalModelName string      `json:"ExternalModelName"`
	Available         bool        `json:"Available"`
	Reason            string      `json:"Reason"`
	PointCount        int         `json:"PointCount"`
	Cost              pricingLine `json:"Cost"`
	Sale              pricingLine `json:"Sale"`
}

// multiLineInputs 一条多线对比的原始配置（尚未出点）。
type multiLineInputs struct {
	LineIndex int
	Cost      pricingModelConfig
	Sale      pricingModelConfig
}

// pricingModelConfig 建线所需的模型配置（模型级已与全局合并完毕）。
type pricingModelConfig struct {
	Source           string
	Name             string
	ExternalModelID  int64
	ChannelModelID   int64
	ChannelID        int64
	ChannelName      string
	InternalModelID  string
	Rates            billing.Rates
	TimeConfig       billing.TimeCoeffConfig
	Tiers            []billing.TierRule
	TimeConfigSource string
	TierSource       string
	Available        bool
	Reason           string
}

// newPricingLine 生成一条不可用（缺配置）的线：恒非 nil，前端不用判空。
func newPricingLine(source, reason string) pricingLine {
	return pricingLine{
		Available:  false,
		Source:     source,
		Reason:     reason,
		Tiers:      []billing.TierRule{},
		Points:     []pricingPoint{},
		TierBreaks: []pricingTierBreak{},
		OverTier:   []pricingTokenRange{},
	}
}

// buildPricingLine 按真实公式逐 token 出点。纯函数（不碰 DB），便于单测覆盖拐点/超档/系数开关。
//
// 输入拆分：上下文长度 L = input + cache_read；
//   - cache_read = round(L × HitRate)，input = L − cache_read；
//   - output = round(input ÷ InOutRatio)（InOutRatio ≤ 0 时输出记 0）。
//
// 这样横轴就是「一次请求实际处理的上下文规模」，与 ctxCoeff 的查档口径（input+cache_read）一致。
func buildPricingLine(cfg pricingModelConfig, p pricingCurveParams, r int64) (pricingLine, error) {
	line := pricingLine{
		Available:        cfg.Available,
		Source:           cfg.Source,
		Name:             cfg.Name,
		ExternalModelID:  cfg.ExternalModelID,
		ChannelModelID:   cfg.ChannelModelID,
		ChannelID:        cfg.ChannelID,
		ChannelName:      cfg.ChannelName,
		InternalModelID:  cfg.InternalModelID,
		Rates:            billing.WireRates(cfg.Rates),
		TimeZone:         cfg.TimeConfig.Timezone,
		TimeConfigSource: cfg.TimeConfigSource,
		TierSource:       cfg.TierSource,
		Tiers:            cfg.Tiers,
		Points:           make([]pricingPoint, 0, 64),
		TierBreaks:       make([]pricingTierBreak, 0, 4),
		OverTier:         make([]pricingTokenRange, 0, 2),
	}
	if line.Tiers == nil {
		line.Tiers = []billing.TierRule{}
	}
	if !cfg.Available {
		line.Reason = cfg.Reason
		return line, nil
	}
	if r <= 0 {
		return line, errors.New("积分换算系数 R 非法")
	}
	timeCoeff, err := cfg.TimeConfig.CoeffFor(p.At)
	if err != nil {
		return line, fmt.Errorf("时段系数配置非法: %w", err)
	}
	line.TimeCoeff = timeCoeff

	// 跳档拐点：相邻两档系数不同 → 拐点在下一档的 Min。
	for i := 0; i+1 < len(cfg.Tiers); i++ {
		if cfg.Tiers[i].Coeff == cfg.Tiers[i+1].Coeff {
			continue
		}
		line.TierBreaks = append(line.TierBreaks, pricingTierBreak{
			Tokens:    cfg.Tiers[i+1].Min,
			FromCoeff: cfg.Tiers[i].Coeff,
			ToCoeff:   cfg.Tiers[i+1].Coeff,
		})
	}

	step := p.Step
	if step <= 0 {
		step = pricingCurveDefaultStep
	}
	maxTokens := p.MaxTokens
	if maxTokens <= 0 {
		maxTokens = defaultCurveMaxTokens(cfg.Tiers)
	}
	// 点数上限：超大 maxTokens 时自动截断（Step 不变，保证点的 token 语义不变）。
	if maxTokens/step > pricingCurveMaxPoints {
		maxTokens = step * pricingCurveMaxPoints
	}

	rF := float64(r)
	var openRange *pricingTokenRange // 正在累积的超档区段
	for tokens := step; tokens <= maxTokens; tokens += step {
		cacheRead := int64(roundHalfUp(float64(tokens) * p.HitRate))
		if cacheRead < 0 {
			cacheRead = 0
		}
		input := tokens - cacheRead
		output := int64(0)
		if p.InOutRatio > 0 {
			output = int64(roundHalfUp(float64(input) / p.InOutRatio))
		}
		sum := float64(input)*cfg.Rates["input"] +
			float64(cacheRead)*cfg.Rates["cache_read"] +
			float64(output)*cfg.Rates["output"]

		pt := pricingPoint{
			Tokens:          tokens,
			InputTokens:     input,
			CacheReadTokens: cacheRead,
			OutputTokens:    output,
			CreditsNoCoeff:  decimalx.Round5(sum / rF),
		}
		ctxCoeff, cerr := billing.ContextTierCoeff(tokens, cfg.Tiers)
		if cerr != nil {
			// 分档之外：真实计费会因「未命中任何上下文分档」失败，这里如实标记，
			// 不编造系数、不给假金额（前端据此断线并画超档区段）。
			pt.OverTier = true
			if openRange == nil {
				openRange = &pricingTokenRange{FromTokens: tokens, ToTokens: tokens}
			} else {
				openRange.ToTokens = tokens
			}
		} else {
			if openRange != nil {
				line.OverTier = append(line.OverTier, *openRange)
				openRange = nil
			}
			pt.CtxCoeff = ctxCoeff
			pt.Credits = decimalx.Round5(sum * timeCoeff * ctxCoeff / rF)
		}
		line.Points = append(line.Points, pt)
	}
	if openRange != nil {
		line.OverTier = append(line.OverTier, *openRange)
	}
	return line, nil
}

// roundHalfUp 四舍五入到整数（正数场景；负数按远离 0 处理，与本处语义无冲突）。
func roundHalfUp(v float64) float64 {
	if v >= 0 {
		return float64(int64(v + 0.5))
	}
	return float64(int64(v - 0.5))
}

// defaultCurveMaxTokens 缺省横轴上限：分档全有上界时取最高上界，否则 200000。
func defaultCurveMaxTokens(tiers []billing.TierRule) int64 {
	if len(tiers) == 0 {
		return pricingCurveDefaultMaxToken
	}
	last := tiers[len(tiers)-1]
	if last.Max != nil && *last.Max > 0 {
		return *last.Max
	}
	if last.Min > 0 && last.Min*2 > pricingCurveDefaultMaxToken {
		return last.Min * 2
	}
	return pricingCurveDefaultMaxToken
}

// timeOptions 把全局时段配置转成下拉可选项（按 Name+Start+End 去重）；
// 没有周期段时给一个「全天」选项，保证前端下拉恒有内容。
func timeOptions(cfg billing.TimeCoeffConfig) ([]pricingTimeOption, float64) {
	out := make([]pricingTimeOption, 0, len(cfg.Periodic))
	seen := make(map[string]bool, len(cfg.Periodic))
	for _, sg := range cfg.Periodic {
		key := sg.Name + "|" + sg.Start + "|" + sg.End
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, pricingTimeOption{Name: sg.Name, Start: sg.Start, End: sg.End, Coeff: sg.Coeff})
	}
	if len(out) == 0 {
		out = append(out, pricingTimeOption{Name: "全天", Start: "00:00", End: "24:00", Coeff: cfg.Default})
	}
	return out, cfg.Default
}

// parsePricingCurveParams 解析曲线参数：越界值收敛，非数字报错。
func parsePricingCurveParams(q url.Values, now time.Time) (pricingCurveParams, error) {
	get := func(k string) string { return q.Get(k) }
	p := pricingCurveParams{
		Step:       pricingCurveDefaultStep,
		MaxTokens:  0, // 0 = 按分档推导
		HitRate:    pricingCurveDefaultHitRate,
		InOutRatio: pricingCurveDefaultInOut,
	}
	if raw := get("Step"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return p, errors.New("Step 必须为整数")
		}
		if v < 1 {
			v = pricingCurveDefaultStep
		}
		if v > pricingCurveMaxStep {
			v = pricingCurveMaxStep
		}
		p.Step = v
	}
	if raw := get("MaxTokens"); raw != "" {
		v, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			return p, errors.New("MaxTokens 必须为整数")
		}
		if v < 0 {
			v = 0
		}
		if v > pricingCurveMaxMaxToken {
			v = pricingCurveMaxMaxToken
		}
		p.MaxTokens = v
	}
	if raw := get("HitRate"); raw != "" {
		v, err := parseCostFiniteFloat(raw)
		if err != nil {
			return p, errors.New("HitRate 参数非法")
		}
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		p.HitRate = v
	}
	if raw := get("InOutRatio"); raw != "" {
		v, err := parseCostFiniteFloat(raw)
		if err != nil {
			return p, errors.New("InOutRatio 参数非法")
		}
		if v < 0 {
			v = 0
		}
		if v > pricingCurveMaxInOutRatio {
			v = pricingCurveMaxInOutRatio
		}
		p.InOutRatio = v
	}
	p.At = now
	if raw := get("At"); raw != "" {
		t, err := parseCurveAt(raw, now)
		if err != nil {
			return p, err
		}
		p.At = t
	}
	return p, nil
}

// parseCurveAt 解析结算时刻：支持 RFC3339（含时区）与 HH:MM / HH:MM:SS（按 Asia/Shanghai 当天）。
func parseCurveAt(raw string, now time.Time) (time.Time, error) {
	loc := cnLocation()
	if t, err := parseRFC3339Lenient(raw); err == nil {
		return t.In(loc), nil
	}
	for _, layout := range []string{"15:04", "15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			n := now.In(loc)
			return time.Date(n.Year(), n.Month(), n.Day(), t.Hour(), t.Minute(), t.Second(), 0, loc), nil
		}
	}
	return time.Time{}, errors.New("At 应为 RFC3339 或 HH:MM")
}

// HandleCostPricingCurve GET /api/v1/admin/cost/pricing-curve
// 模型定价曲线：售价线用对外模型配置、成本线用渠道内部模型配置，逐 token 出点。
//
// 参数（全部可选）：
//   - ExternalModelID / ExternalModelName：指定售价线模型（二选一）；
//   - ChannelModelID：指定成本线模型（其对外模型在未显式指定售价模型时用于售价线）；
//   - ChannelModelIDs：**多线对比**，逗号分隔的渠道内部模型 ID（最多 10 个）。传了就额外返回 Lines，
//     每行一个 (渠道, 渠道内部模型) 各带自己的成本/售价曲线；不传时响应与改造前逐字节一致。
//     受功能开关 console.cost_multichannel_trace 控制（默认关闭，关闭时传该参数返回 **403**：
//     属「能力未启用」而非「参数写错」，前端据此隐藏入口）。
//   - Step / MaxTokens / HitRate / InOutRatio：采样参数；
//   - At：结算时刻（RFC3339 或 HH:MM），决定时段系数，缺省 now。
//
// 两个都没指定时返回两条 Available=false 的线 + 全局时段选项（不报错、不返回 null）。
func (h *Admin) HandleCostPricingCurve(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := parsePricingCurveParams(q, h.now())
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	externalID, err := parseOptionalInt64(q.Get("ExternalModelID"))
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ExternalModelID 必须为数字")
		return
	}
	channelModelID, err := parseOptionalInt64(q.Get("ChannelModelID"))
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, "ChannelModelID 必须为数字")
		return
	}
	multiIDs, err := parseChannelModelIDs(q.Get("ChannelModelIDs"))
	if err != nil {
		resp.Err(w, r, http.StatusBadRequest, resp.CodeBadRequest, err.Error())
		return
	}
	// 功能开关：默认关闭。关闭时**只**拒绝新参数，不传该参数的旧请求完全不受影响。
	if len(multiIDs) > 0 && !h.costViewsEnabled(r.Context()) {
		resp.Err(w, r, http.StatusForbidden, resp.CodeForbidden, costViewsSwitchDisabledMsg)
		return
	}
	externalName := q.Get("ExternalModelName")

	out := pricingCurveResponse{
		At:          p.At.Format(time.RFC3339),
		TimeZone:    cnLocation().String(),
		R:           h.resolveR(),
		Step:        p.Step,
		MaxTokens:   p.MaxTokens,
		HitRate:     p.HitRate,
		InOutRatio:  p.InOutRatio,
		Sale:        newPricingLine("external_model", "未指定对外模型"),
		Cost:        newPricingLine("channel_model", "未指定渠道内部模型"),
		TimeOptions: []pricingTimeOption{},
	}
	if h.db == nil {
		resp.OK(w, r, out)
		return
	}

	globalTime := billing.TimeCoeffConfig{Timezone: cnLocation().String(), Default: 1}
	if cfg, err := h.loadGlobalTimeConfig(r.Context()); err == nil {
		globalTime = cfg
	}
	globalTiers := h.loadGlobalContextTiers(r.Context())
	out.TimeOptions, out.DefaultTimeCoeff = timeOptions(globalTime)

	// 多线对比：先装每条线的配置（只有传了新参数才进这里，旧请求的查询顺序与条数完全不变）。
	var multiInputs []multiLineInputs
	if len(multiIDs) > 0 {
		multiInputs, err = h.loadMultiLines(r.Context(), multiIDs, globalTime, globalTiers)
		if err != nil {
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询渠道内部模型配置失败")
			return
		}
	}

	// 成本线（同时带出它的对外模型配置，供未显式指定售价模型时复用）。
	costCfg := pricingModelConfig{
		Source: "channel_model", Reason: "未指定渠道内部模型",
		TimeConfig: globalTime, Tiers: globalTiers, TimeConfigSource: "global", TierSource: tierSource(globalTiers),
	}
	if channelModelID > 0 {
		costCfg, err = h.loadChannelModelConfig(r.Context(), channelModelID, globalTime, globalTiers)
		if err != nil {
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询渠道内部模型配置失败")
			return
		}
	}

	// 售价线：显式指定优先，否则用成本线的对外模型。
	saleModel := pricingModelConfig{Source: "external_model", Reason: "未指定对外模型",
		TimeConfig: globalTime, Tiers: globalTiers, TimeConfigSource: "global", TierSource: tierSource(globalTiers)}
	switch {
	case externalID > 0 || externalName != "":
		saleModel, err = h.loadSaleModelConfig(r.Context(), externalID, externalName, globalTime, globalTiers)
	case channelModelID > 0:
		saleModel, err = h.loadSaleModelByChannelModel(r.Context(), channelModelID, globalTime, globalTiers)
	}
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "查询对外模型配置失败")
		return
	}

	// 横轴上限：未指定时由分档推导；再按点数上限收敛（保证响应里回报的就是实际用的）。
	if p.MaxTokens <= 0 {
		if len(multiInputs) > 0 {
			// 多线：取所有线的最大推导值，避免某条线因别家分档更小被提前截断。
			p.MaxTokens = multiLineMaxTokens(multiInputs)
		} else {
			tiers := costCfg.Tiers
			if saleModel.Available || len(saleModel.Tiers) > 0 {
				tiers = saleModel.Tiers
			}
			p.MaxTokens = defaultCurveMaxTokens(tiers)
		}
	}
	if p.Step > 0 && p.MaxTokens/p.Step > pricingCurveMaxPoints {
		p.MaxTokens = p.Step * pricingCurveMaxPoints
	}
	// 多线模式的**总点数**上限仍是 pricingCurveMaxPoints：线多了就按线数等比下调横轴上限，
	// 保证响应体量与单线模式同一量级；实际用的 MaxTokens 会在响应里回报，前端不用猜。
	if n := len(multiInputs); n > 1 && p.Step > 0 {
		perLine := int64(pricingCurveMaxPoints / n)
		if perLine < 1 {
			perLine = 1
		}
		if p.MaxTokens/p.Step > perLine {
			p.MaxTokens = p.Step * perLine
		}
	}
	out.MaxTokens = p.MaxTokens

	saleLine, err := buildPricingLine(saleModel, p, out.R)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "计算售价曲线失败："+err.Error())
		return
	}
	costLine, err := buildPricingLine(costCfg, p, out.R)
	if err != nil {
		resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, "计算成本曲线失败："+err.Error())
		return
	}
	out.PointCount = len(saleLine.Points)
	if out.PointCount == 0 {
		out.PointCount = len(costLine.Points)
	}
	out.Sale = saleLine
	out.Cost = costLine
	// 多线：与单线同一套采样参数出点；Lines 非空才会被序列化（omitempty）。
	if len(multiInputs) > 0 {
		lines, lerr := buildMultiLines(multiInputs, p, out.R)
		if lerr != nil {
			resp.Err(w, r, http.StatusInternalServerError, resp.CodeInternalError, lerr.Error())
			return
		}
		out.Lines = lines
		if len(lines) > 0 {
			out.PointCount = lines[0].PointCount
		}
	}
	resp.OK(w, r, out)
}

// parseOptionalInt64 解析可空 int64：空串返回 0，非法返回错误。
func parseOptionalInt64(raw string) (int64, error) {
	if raw == "" {
		return 0, nil
	}
	return strconv.ParseInt(raw, 10, 64)
}

// parseChannelModelIDs 解析 ChannelModelIDs=a,b,c：空返回 nil（= 单线模式，行为不变）；
// 去空白、去重（保序）、每项必须为正整数；超过 pricingCurveMaxLines 直接报错（不静默截断）。
func parseChannelModelIDs(raw string) ([]int64, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	out := make([]int64, 0, len(parts))
	seen := make(map[int64]bool, len(parts))
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue // 容忍尾部/连续逗号，不当错误
		}
		v, err := strconv.ParseInt(part, 10, 64)
		if err != nil || v <= 0 {
			return nil, fmt.Errorf("ChannelModelIDs 含非法 ID：%q", part)
		}
		if seen[v] {
			continue
		}
		seen[v] = true
		out = append(out, v)
	}
	if len(out) > pricingCurveMaxLines {
		return nil, fmt.Errorf("ChannelModelIDs 最多 %d 个（本次 %d 个）", pricingCurveMaxLines, len(out))
	}
	return out, nil
}

// loadMultiLines 按请求顺序装载每条线的成本配置与其对外模型的售价配置。
//
// 查询报错 → 整体返回错误（500）；「行不存在/费率缺失」不算错误，而是 Available=false + Reason，
// 与单线模式口径一致（选了不存在的模型给一条不可用的线，而不是整个请求失败）。
func (h *Admin) loadMultiLines(ctx context.Context, ids []int64,
	globalTime billing.TimeCoeffConfig, globalTiers []billing.TierRule) ([]multiLineInputs, error) {
	out := make([]multiLineInputs, 0, len(ids))
	for i, id := range ids {
		costCfg, err := h.loadChannelModelConfig(ctx, id, globalTime, globalTiers)
		if err != nil {
			return nil, err
		}
		saleCfg, err := h.loadSaleModelByChannelModel(ctx, id, globalTime, globalTiers)
		if err != nil {
			return nil, err
		}
		out = append(out, multiLineInputs{LineIndex: i, Cost: costCfg, Sale: saleCfg})
	}
	return out, nil
}

// multiLineMaxTokens 多线模式下的缺省横轴上限：取所有线（成本/售价两侧）分档推导值的最大值，
// 避免任何一条线因别家的分档更小而提前被截断。
func multiLineMaxTokens(inputs []multiLineInputs) int64 {
	maxTokens := int64(0)
	for _, in := range inputs {
		if v := defaultCurveMaxTokens(in.Cost.Tiers); v > maxTokens {
			maxTokens = v
		}
		if v := defaultCurveMaxTokens(in.Sale.Tiers); v > maxTokens {
			maxTokens = v
		}
	}
	if maxTokens <= 0 {
		maxTokens = pricingCurveDefaultMaxToken
	}
	return maxTokens
}

// buildMultiLines 用同一套采样参数给每条线出点，装配多线响应。
func buildMultiLines(inputs []multiLineInputs, p pricingCurveParams, r int64) ([]pricingMultiLine, error) {
	out := make([]pricingMultiLine, 0, len(inputs))
	for _, in := range inputs {
		costLine, err := buildPricingLine(in.Cost, p, r)
		if err != nil {
			return nil, fmt.Errorf("计算成本曲线失败（ChannelModelID=%d）：%w", in.Cost.ChannelModelID, err)
		}
		saleLine, err := buildPricingLine(in.Sale, p, r)
		if err != nil {
			return nil, fmt.Errorf("计算售价曲线失败（ChannelModelID=%d）：%w", in.Cost.ChannelModelID, err)
		}
		line := pricingMultiLine{
			LineIndex:         in.LineIndex,
			ChannelModelID:    in.Cost.ChannelModelID,
			ChannelID:         in.Cost.ChannelID,
			ChannelName:       in.Cost.ChannelName,
			InternalModelID:   in.Cost.InternalModelID,
			ExternalModelID:   in.Sale.ExternalModelID,
			ExternalModelName: in.Sale.Name,
			Available:         costLine.Available,
			Reason:            costLine.Reason,
			PointCount:        len(costLine.Points),
			Cost:              costLine,
			Sale:              saleLine,
		}
		if line.Reason == "" {
			line.Reason = saleLine.Reason
		}
		out = append(out, line)
	}
	return out, nil
}

// loadGlobalTimeConfig 读全局 billing.time_config；缺失/非法时回退「全天系数 1」。
func (h *Admin) loadGlobalTimeConfig(ctx context.Context) (billing.TimeCoeffConfig, error) {
	fallback := billing.TimeCoeffConfig{Timezone: cnLocation().String(), Default: 1}
	raw, err := h.sysConfigRaw(ctx, "billing.time_config")
	if err != nil {
		return fallback, err
	}
	cfg, err := billing.ParseTimeConfig(raw)
	if err != nil {
		return fallback, err
	}
	if cfg.Timezone == "" {
		cfg.Timezone = fallback.Timezone
	}
	return cfg, nil
}

// loadGlobalContextTiers 读全局 billing.context_tiers；缺失/非法时返回空（= 不分档）。
func (h *Admin) loadGlobalContextTiers(ctx context.Context) []billing.TierRule {
	raw, err := h.sysConfigRaw(ctx, "billing.context_tiers")
	if err != nil {
		return nil
	}
	var tiers []billing.TierRule
	if err := json.Unmarshal(raw, &tiers); err != nil {
		return nil
	}
	return tiers
}

// sysConfigRaw 读 sys_configs 原始 JSON 值。
func (h *Admin) sysConfigRaw(ctx context.Context, key string) ([]byte, error) {
	var raw []byte
	if err := h.db.QueryRowContext(ctx, `SELECT value FROM sys_configs WHERE key = $1`, key).Scan(&raw); err != nil {
		return nil, err
	}
	return raw, nil
}

// loadSaleModelConfig 加载对外模型（售价）配置。id>0 优先按 id，否则按名称（含启用性不过滤：
// 停用模型也要能看曲线，否则前端选了停用模型会莫名其妙空图）。
func (h *Admin) loadSaleModelConfig(ctx context.Context, id int64, name string,
	globalTime billing.TimeCoeffConfig, globalTiers []billing.TierRule) (pricingModelConfig, error) {
	out := pricingModelConfig{Source: "external_model"}
	var (
		ratesRaw, timeRaw, tiersRaw []byte
	)
	query := `SELECT id, external_name, sale_rates, time_config, context_tiers
	          FROM external_models WHERE deleted_at IS NULL AND id = $1`
	arg := any(id)
	if id <= 0 {
		query = `SELECT id, external_name, sale_rates, time_config, context_tiers
		         FROM external_models WHERE deleted_at IS NULL AND external_name = $1`
		arg = name
	}
	err := h.db.QueryRowContext(ctx, query, arg).Scan(&out.ExternalModelID, &out.Name, &ratesRaw, &timeRaw, &tiersRaw)
	if errors.Is(err, sql.ErrNoRows) {
		out.Available = false
		out.Reason = "对外模型不存在或已删除"
		out.TimeConfig = globalTime
		out.Tiers = globalTiers
		out.TimeConfigSource = "global"
		out.TierSource = tierSource(globalTiers)
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.Rates = parseRates(ratesRaw)
	if !hasNonZeroRates(out.Rates) {
		out.Available = false
		out.Reason = "对外模型未配置售价费率"
	}
	out.TimeConfig, out.TimeConfigSource = mergeTimeConfig(timeRaw, globalTime)
	out.Tiers, out.TierSource = mergeTiers(tiersRaw, globalTiers)
	if out.Reason == "" {
		out.Available = true
	}
	return out, nil
}

// loadSaleModelByChannelModel 由渠道内部模型反查它的对外模型（成本线已选、售价线未选时用）。
func (h *Admin) loadSaleModelByChannelModel(ctx context.Context, channelModelID int64,
	globalTime billing.TimeCoeffConfig, globalTiers []billing.TierRule) (pricingModelConfig, error) {
	out := pricingModelConfig{Source: "external_model"}
	var (
		externalID                  sql.NullInt64
		ratesRaw, timeRaw, tiersRaw []byte
		name                        sql.NullString
	)
	err := h.db.QueryRowContext(ctx, `
SELECT cm.external_model_id, em.external_name, em.sale_rates, em.time_config, em.context_tiers
FROM channel_models cm
LEFT JOIN external_models em ON em.id = cm.external_model_id
WHERE cm.id = $1`, channelModelID).Scan(&externalID, &name, &ratesRaw, &timeRaw, &tiersRaw)
	if errors.Is(err, sql.ErrNoRows) {
		out.Reason = "渠道内部模型不存在"
		out.TimeConfig = globalTime
		out.Tiers = globalTiers
		out.TimeConfigSource = "global"
		out.TierSource = tierSource(globalTiers)
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.ExternalModelID = externalID.Int64
	out.Name = name.String
	out.Rates = parseRates(ratesRaw)
	out.TimeConfig, out.TimeConfigSource = mergeTimeConfig(timeRaw, globalTime)
	out.Tiers, out.TierSource = mergeTiers(tiersRaw, globalTiers)
	switch {
	case !externalID.Valid:
		out.Reason = "渠道内部模型未绑定对外模型"
	case !hasNonZeroRates(out.Rates):
		out.Reason = "对外模型未配置售价费率"
	default:
		out.Available = true
	}
	return out, nil
}

// loadChannelModelConfig 加载渠道内部模型（成本）配置。
func (h *Admin) loadChannelModelConfig(ctx context.Context, channelModelID int64,
	globalTime billing.TimeCoeffConfig, globalTiers []billing.TierRule) (pricingModelConfig, error) {
	out := pricingModelConfig{Source: "channel_model", ChannelModelID: channelModelID}
	var (
		ratesRaw, timeRaw, tiersRaw []byte
		channelName                 sql.NullString
	)
	err := h.db.QueryRowContext(ctx, `
SELECT cm.id, cm.channel_id, c.name, cm.internal_model_id, cm.cost_rates, cm.time_config, cm.context_tiers
FROM channel_models cm
LEFT JOIN channels c ON c.id = cm.channel_id
WHERE cm.id = $1 AND cm.deleted_at IS NULL`, channelModelID).
		Scan(&out.ChannelModelID, &out.ChannelID, &channelName, &out.InternalModelID, &ratesRaw, &timeRaw, &tiersRaw)
	if errors.Is(err, sql.ErrNoRows) {
		out.Reason = "渠道内部模型不存在或已删除"
		out.TimeConfig = globalTime
		out.Tiers = globalTiers
		out.TimeConfigSource = "global"
		out.TierSource = tierSource(globalTiers)
		return out, nil
	}
	if err != nil {
		return out, err
	}
	out.ChannelName = channelName.String
	out.Name = out.InternalModelID
	out.Rates = parseRates(ratesRaw)
	out.TimeConfig, out.TimeConfigSource = mergeTimeConfig(timeRaw, globalTime)
	out.Tiers, out.TierSource = mergeTiers(tiersRaw, globalTiers)
	if !hasNonZeroRates(out.Rates) {
		out.Reason = "渠道内部模型未配置成本费率"
		return out, nil
	}
	out.Available = true
	return out, nil
}

// parseRates 解析费率 JSONB；非法/缺失返回空表（由 hasNonZeroRates 判为不可用）。
func parseRates(raw []byte) billing.Rates {
	if len(raw) == 0 || string(raw) == "null" {
		return billing.Rates{}
	}
	var rates billing.Rates
	if err := json.Unmarshal(raw, &rates); err != nil {
		return billing.Rates{}
	}
	return rates
}

// mergeTimeConfig 模型级时段配置优先，空/NULL 回落全局；返回实际来源。
func mergeTimeConfig(raw []byte, global billing.TimeCoeffConfig) (billing.TimeCoeffConfig, string) {
	if len(raw) == 0 || string(raw) == "null" {
		return global, "global"
	}
	cfg, err := billing.ParseTimeConfig(raw)
	if err != nil {
		return global, "global"
	}
	// 空对象（Timezone 为空且无段且 Default=0）视为未配置，回落全局。
	if cfg.Timezone == "" && len(cfg.Periodic) == 0 && len(cfg.Overrides) == 0 && cfg.Default == 0 {
		return global, "global"
	}
	if cfg.Timezone == "" {
		cfg.Timezone = global.Timezone
	}
	return cfg, "model"
}

// mergeTiers 模型级分档优先，空/NULL 回落全局；返回实际来源（none = 不分档）。
func mergeTiers(raw []byte, global []billing.TierRule) ([]billing.TierRule, string) {
	if len(raw) > 0 && string(raw) != "null" {
		var tiers []billing.TierRule
		if err := json.Unmarshal(raw, &tiers); err == nil && len(tiers) > 0 {
			return tiers, "model"
		}
	}
	if len(global) > 0 {
		return global, "global"
	}
	return []billing.TierRule{}, "none"
}

// tierSource 全局分档来源标记。
func tierSource(tiers []billing.TierRule) string {
	if len(tiers) == 0 {
		return "none"
	}
	return "global"
}
