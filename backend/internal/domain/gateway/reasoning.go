package gateway

import (
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// 会话级推理缓存：DeepSeek 等 thinking 模式上游要求客户端在后续请求中把上一轮
// assistant 消息的 reasoning_content 原样回传（典型为带 tool_calls 的助手消息），
// 但多数客户端（Anthropic 风格客户端、只保留 content 的工具链）不会回传该字段，
// 导致上游 400「reasoning_content must be passed back」。
//
// 网关在生成响应时把推理内容按「会话 + tool_call_id」记入进程内缓存，
// 在下一轮请求构造上游请求体前回填到缺失该字段的 assistant 消息上。
//
// 字段名的两个命名空间（刻意区分，勿混用）：
//   - 写入（读取上游响应）：流式与跨协议转换的 assistant 消息由网关按 reasoning_content
//     拼装，缓存写入沿用同一口径；非流式的上游原始响应体按别名集合容错读取
//     （reasoning_content → reasoning，见 reasoningAliasText），不按渠道声明的请求侧字段名读响应体。
//   - 回填（写入上游请求）：字段名由渠道声明的请求侧字段名决定（Backfill 的 field 参数，
//     缺省 reasoning_content），与 thinking-roundtrip / 转换注入同源。
//
// 约束：
//   - 仅内存、进程内（多实例部署不共享，缓存未命中时行为退化为改造前，不会报错）；
//   - 有 TTL 与条目上限，超限按随机淘汰（缓存语义，丢失只会退回原行为）；
//   - 只回填缺失/为空的推理字段，已有内容一律不改写；
//   - 只在目标上游协议为 openai-chat 时生效（其他协议的请求体形状不同）；
//   - 回填由调用方按渠道声明门控（未声明支持推理回传的渠道不回填），缓存写入不做门控；
//   - 缓存键仅含 session+tool_call_id，不含渠道/模型：同一会话的推理历史对任一声明支持的
//     上游都是正确的对话内容（thinking 上游本就要求回传），跨渠道 failover 时仍可回填；
//     若未来需要严格隔离可按渠道维度扩键，但会破坏 failover 场景的回填能力，故有意不加。
const (
	// reasoningCacheTTL 是条目的默认存活时长（覆盖一次多轮工具调用会话的常见跨度）。
	reasoningCacheTTL = time.Hour
	// reasoningCacheMax 是条目上限（按 tool_call_id 计），防止长驻进程内存无界增长。
	reasoningCacheMax = 5000
	// reasoningCacheSweepInterval 是惰性清理过期条目的最小间隔（避免每次写入都全量扫描）。
	reasoningCacheSweepInterval = time.Minute
	// reasoningKeySeparator 分隔会话标识与 tool_call_id，避免跨会话串用。
	reasoningKeySeparator = "\x1f"
	// reasoningResponseField 是缓存写入时读取上游响应的推理字段名（默认命名空间的字面量）。
	reasoningResponseField = "reasoning_content"
)

// reasoningEntry 是单条推理缓存。
type reasoningEntry struct {
	reasoning string
	expiresAt time.Time
}

// ReasoningCache 是按会话隔离的推理内容缓存（并发安全）。
type ReasoningCache struct {
	mu        sync.Mutex
	entries   map[string]reasoningEntry
	ttl       time.Duration
	max       int
	now       func() time.Time
	lastSweep time.Time
}

// NewReasoningCache 创建会话级推理缓存（默认 TTL 与条目上限）。
func NewReasoningCache() *ReasoningCache {
	return &ReasoningCache{
		entries: make(map[string]reasoningEntry, 64),
		ttl:     reasoningCacheTTL,
		max:     reasoningCacheMax,
		now:     time.Now,
	}
}

// RememberMessage 把一条网关拼装的 assistant 消息中的推理内容记入缓存。
// 读取按响应侧别名优先级容错（reasoning_content → reasoning，见 reasoningAliasText）。
// 仅当消息带该字段且带 tool_calls（有 id）时记录：thinking 模式下需要回传推理内容的
// 正是这类消息；其余消息记了也不会被回填。
func (c *ReasoningCache) RememberMessage(sessionID string, msg []byte) {
	if c == nil || sessionID == "" || len(msg) == 0 {
		return
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(msg, &m); err != nil {
		return
	}
	var tcs []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(m["tool_calls"], &tcs) != nil || len(tcs) == 0 {
		return
	}
	reasoning := reasoningAliasText(m)
	if reasoning == "" {
		return
	}
	ids := make([]string, 0, len(tcs))
	for _, tc := range tcs {
		ids = append(ids, tc.ID)
	}
	c.insert(sessionID, reasoning, ids)
}

// RememberResponse 从 OpenAI Chat Completions 非流式响应体中提取 choices[].message 记入缓存。
func (c *ReasoningCache) RememberResponse(sessionID string, body []byte) {
	if c == nil || sessionID == "" || len(body) == 0 {
		return
	}
	var r struct {
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return
	}
	for _, ch := range r.Choices {
		c.RememberMessage(sessionID, ch.Message)
	}
}

// Backfill 回填请求体中缺失的 assistant 推理内容：对带 tool_calls 且 field 字段
// 缺失/为空的 assistant 消息，按其中的 tool_call_id 命中缓存后写入该字段。
// field 为渠道声明的请求侧字段名（缺省 reasoning_content，见文件头「字段名的两个命名空间」）；
// 无可回填内容时原样返回入参（返回同一 slice，不产生任何改写）。
//
// 与直通 thinking-roundtrip 的差异（有意）：直通改写只在「键不存在」时补（客户端显式传空串
// 会被保留），而本回填把空串视作缺失并填充——客户端常把 reasoning_content 回显为空串，
// 空值无法满足上游「必须回传」的要求，填充才能真正消除 400。
func (c *ReasoningCache) Backfill(sessionID, field string, body []byte) []byte {
	if c == nil || sessionID == "" || len(body) == 0 {
		return body
	}
	field = normalizeReasoningField(field)
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	rawMsgs, ok := obj["messages"]
	if !ok || len(rawMsgs) == 0 {
		return body
	}
	var msgs []json.RawMessage
	if err := json.Unmarshal(rawMsgs, &msgs); err != nil {
		return body
	}
	changed := false
	for i, raw := range msgs {
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			continue
		}
		var role string
		if json.Unmarshal(m["role"], &role) != nil || role != "assistant" {
			continue
		}
		if isNonEmptyReasoning(m[field]) {
			continue // 客户端已回传：不改写
		}
		reasoning, hit := c.lookup(sessionID, m["tool_calls"])
		if !hit {
			continue
		}
		enc, err := json.Marshal(reasoning)
		if err != nil {
			continue
		}
		m[field] = enc
		next, err := json.Marshal(m)
		if err != nil {
			continue
		}
		msgs[i] = next
		changed = true
	}
	if !changed {
		return body
	}
	newMsgs, err := json.Marshal(msgs)
	if err != nil {
		return body
	}
	obj["messages"] = newMsgs
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// normalizeReasoningField 归一请求侧推理字段名：去空白后为空则回落 reasoning_content。
// 去空白是必要的：渠道参数可能带前后空格，直接用于 JSON 键会产出非法字段名。
func normalizeReasoningField(field string) string {
	if f := strings.TrimSpace(field); f != "" {
		return f
	}
	return reasoningResponseField
}

// lookup 按 assistant 消息的 tool_calls 查找缓存：任一个 tool_call_id 命中即可。
// 命中已过期条目时顺带删除。
func (c *ReasoningCache) lookup(sessionID string, rawToolCalls json.RawMessage) (string, bool) {
	if len(rawToolCalls) == 0 {
		return "", false
	}
	var tcs []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(rawToolCalls, &tcs); err != nil {
		return "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for _, tc := range tcs {
		if tc.ID == "" {
			continue
		}
		key := reasoningKey(sessionID, tc.ID)
		e, ok := c.entries[key]
		if !ok {
			continue
		}
		if !now.Before(e.expiresAt) {
			delete(c.entries, key)
			continue
		}
		return e.reasoning, true
	}
	return "", false
}

// insert 写入缓存条目（同一 tool_call_id 重复写入时以最新推理内容覆盖并续期）。
func (c *ReasoningCache) insert(sessionID, reasoning string, ids []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	if c.entries == nil {
		c.entries = make(map[string]reasoningEntry)
	}
	if now.Sub(c.lastSweep) >= reasoningCacheSweepInterval {
		c.sweepLocked(now)
		c.lastSweep = now
	}
	exp := now.Add(c.ttl)
	for _, id := range ids {
		if id == "" {
			continue
		}
		c.entries[reasoningKey(sessionID, id)] = reasoningEntry{reasoning: reasoning, expiresAt: exp}
	}
	c.trimLocked()
}

// sweepLocked 惰性清理已过期条目（调用方持锁）。
func (c *ReasoningCache) sweepLocked(now time.Time) {
	for k, e := range c.entries {
		if !now.Before(e.expiresAt) {
			delete(c.entries, k)
		}
	}
}

// trimLocked 在条目超限时随机淘汰（调用方持锁；缓存语义，不保证 LRU）。
func (c *ReasoningCache) trimLocked() {
	if c.max <= 0 || len(c.entries) <= c.max {
		return
	}
	for k := range c.entries {
		if len(c.entries) <= c.max {
			return
		}
		delete(c.entries, k)
	}
}

// reasoningKey 生成缓存键（会话隔离 + tool_call_id）。
func reasoningKey(sessionID, toolCallID string) string {
	return sessionID + reasoningKeySeparator + toolCallID
}

// reasoningText 取推理内容的文本形式：仅接受 JSON 字符串，并去除首尾空白
// （空白视为无内容，与 blocksThinkingText / isNonEmptyReasoning 的 TrimSpace 判定一致，
// 避免空白推理被缓存并回填——对上游无意义且无法满足「必须回传」要求）。
func reasoningText(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) != nil {
		return ""
	}
	return strings.TrimSpace(s)
}

// reasoningAliasText 按别名优先级取首个非空推理文本（reasoning_content → reasoning）。
// 写入侧对「流式/转换由网关拼装」的情形恒为 reasoning_content；对「非流式原始响应体」则
// 需要容错部分上游使用 reasoning 别名的情形，两者共用同一别名集合，避免静默 miss。
//
// 与请求侧 reasoningSource 的差异（有意）：本函数只接受 JSON 字符串（缓存值需为文本，
// 数组/对象形态无统一语义）；reasoningSource 接受数组/对象并原样搬运，用于直通回传。
func reasoningAliasText(m map[string]json.RawMessage) string {
	for _, key := range reasoningAliases {
		if text := reasoningText(m[key]); text != "" {
			return text
		}
	}
	return ""
}
