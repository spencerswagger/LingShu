// 上游原始 usage（call_logs.upstream_usage）→ 四段用量展示口径。
//
// 背景：计费侧只有归一化三段（input / output / cache_read，cache_creation 并入 input，
// 见后端 billing.Usage），而定位「上游到底报了什么」必须看原文。各协议 naming 不一致：
//   - OpenAI Chat：prompt_tokens / completion_tokens / prompt_tokens_details.cached_tokens
//   - OpenAI Responses：input_tokens / output_tokens / input_tokens_details.cached_tokens
//   - Anthropic Messages：input_tokens / output_tokens /
//     cache_read_input_tokens / cache_creation_input_tokens
//
// 因此这里按「键名别名 + 任意层级」取值，取不到就是 undefined —— **不补 0**。
// 展示层据此显示「未解析」，避免把「上游没报」误读成「用量为 0」。

/** 一段用量：undefined 表示该字段在上游 usage 里不存在（未解析），0 表示上游明确报了 0。 */
export interface UsageSegments {
  Input?: number
  Output?: number
  CacheRead?: number
  CacheCreation?: number
}

/** 上游原始 usage：{ Protocol, Usage } 包装（后端 CallLogView.UpstreamUsage）或直接是扁平对象。 */
export interface UpstreamUsageView {
  Protocol?: string
  Usage?: Record<string, unknown>
}

// 键名别名（按优先级从前往后取第一个存在的数值）。
const ALIASES: Record<keyof UsageSegments, string[]> = {
  Input: ['input_tokens', 'prompt_tokens', 'inputTokens', 'promptTokens', 'input'],
  Output: ['output_tokens', 'completion_tokens', 'outputTokens', 'completionTokens', 'output'],
  CacheRead: [
    'cache_read_input_tokens',
    'cache_read_tokens',
    'cached_tokens', // OpenAI details.cached_tokens
    'prompt_cache_hit_tokens', // 部分国内厂商命名
    'cacheRead',
    'cache_read',
  ],
  CacheCreation: [
    'cache_creation_input_tokens',
    'cache_creation_tokens',
    'prompt_cache_miss_tokens',
    'cacheCreation',
    'cache_creation',
  ],
}

/** 递归收集所有「键 → 数值」叶子（任意层级），首个命中的优先级由 ALIASES 决定。 */
function collectNumbers(node: unknown, out: Map<string, number[]>, depth = 0): void {
  if (node == null || depth > 6) return
  if (Array.isArray(node)) {
    for (const item of node) collectNumbers(item, out, depth + 1)
    return
  }
  if (typeof node !== 'object') return
  for (const [k, v] of Object.entries(node as Record<string, unknown>)) {
    if (typeof v === 'number' && Number.isFinite(v)) {
      const arr = out.get(k) || []
      arr.push(v)
      out.set(k, arr)
      continue
    }
    if (v && typeof v === 'object') collectNumbers(v, out, depth + 1)
  }
}

/** 取上游 usage 的四段用量；缺失字段留 undefined（调用方显示「未解析」）。 */
export function parseUsageSegments(raw: unknown): UsageSegments {
  const out: UsageSegments = {}
  if (!raw || typeof raw !== 'object') return out
  // 兼容 { Protocol, Usage:{...} } 包装与扁平对象两种形状。
  const wrapped = raw as UpstreamUsageView
  const body = wrapped.Usage && typeof wrapped.Usage === 'object' ? wrapped.Usage : raw
  const nums = new Map<string, number[]>()
  collectNumbers(body, nums)
  for (const [seg, aliases] of Object.entries(ALIASES) as Array<[keyof UsageSegments, string[]]>) {
    for (const alias of aliases) {
      const hit = nums.get(alias)
      if (hit && hit.length) {
        out[seg] = hit[0]
        break
      }
    }
  }
  return out
}

/** 拆出 { Protocol, Usage } 视图；形状不认识时返回 null（调用方不展示用量块）。 */
export function asUpstreamUsage(raw: unknown): UpstreamUsageView | null {
  if (!raw || typeof raw !== 'object') return null
  const v = raw as UpstreamUsageView
  if (v.Usage && typeof v.Usage === 'object') return v
  // 扁平对象：把整体当作 Usage，协议未知。
  return { Usage: raw as Record<string, unknown> }
}

/** 用量段展示：缺失 → 「未解析」（而不是 0）。 */
export function fmtSeg(n?: number): string {
  if (n == null) return '未解析'
  return n.toLocaleString()
}

/** 该段是否已解析（供样式区分灰字/实字）。 */
export function segResolved(n?: number): boolean {
  return n != null
}
