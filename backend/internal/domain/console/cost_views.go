package console

// cost_views.go 成本页增强视图的**功能开关**（默认关闭 = 现状不变）。
//
// 一个开关同时管两个新增的**只读**能力：
//   - 定价曲线多线对比：GET /api/v1/admin/cost/pricing-curve 的 ChannelModelIDs 参数与 Lines 字段；
//   - 会话渠道轨迹：GET /api/v1/admin/cost/session/{id}/trace。
//
// 边界（本轮明确不做）：开关只决定这两个只读接口是否可用，
// 不参与计费公式、不参与路由决策、不把引擎决策落库。
// 关闭时旧接口的响应逐字节不变（新增字段用 omitempty，新端点在关闭时直接拒绝）。

import (
	"context"
	"encoding/json"
	"strings"
)

// costViewsSwitchKey 功能开关的 sys_configs 键；值语义为 JSON 布尔（也容忍 "true"/"1" 之类字符串）。
const costViewsSwitchKey = "console.cost_multichannel_trace"

// costViewsSwitchDisabledMsg 关闭时对外的一句话原因（前端据此隐藏入口）。
const costViewsSwitchDisabledMsg = "成本页增强视图（多线对比/渠道轨迹）未启用：请先在系统配置中打开 console.cost_multichannel_trace"

// costViewsEnabled 读开关。键缺失 / 值非法 / 未装配 DB 一律按**关闭**处理（默认关闭 = 现状不变）。
func (h *Admin) costViewsEnabled(ctx context.Context) bool {
	if h.db == nil {
		return false
	}
	raw, err := h.sysConfigRaw(ctx, costViewsSwitchKey)
	if err != nil {
		return false
	}
	return parseConfigBool(raw)
}

// costViewsEnabledJSON 与 costViewsEnabled 同源，但直接返回可序列化的 JSON 原文（供系统配置 GET 下发）。
func (h *Admin) costViewsEnabledJSON(ctx context.Context) json.RawMessage {
	b, _ := json.Marshal(h.costViewsEnabled(ctx))
	return json.RawMessage(b)
}

// parseConfigBool 解析配置布尔值：JSON 布尔优先，其次容忍 "true"/"1"/"yes"/"on" 字符串；
// 其余一律 false（不改默认口径）。
func parseConfigBool(raw []byte) bool {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return false
	}
	var b bool
	if err := json.Unmarshal([]byte(trimmed), &b); err == nil {
		return b
	}
	var s string
	if err := json.Unmarshal([]byte(trimmed), &s); err == nil {
		switch strings.ToLower(strings.TrimSpace(s)) {
		case "true", "1", "yes", "on":
			return true
		}
	}
	// 兼容运维手改成的数字（1/0）。
	var n float64
	if err := json.Unmarshal([]byte(trimmed), &n); err == nil {
		return n != 0
	}
	return false
}
