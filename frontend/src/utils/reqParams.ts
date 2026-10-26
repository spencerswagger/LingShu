// call_logs.req_params（入站请求参数快照）→ 展示口径。
//
// 后端落库形状（backend/internal/domain/gateway/reqparams.go 的 reqParamsSnapshot）：
//   { endpoint, inbound_protocol, upstream_protocol, upstream_url, converted, token_id,
//     model, auto_model_name, stream, message_count,
//     tools: {count, names}, params: {<入站参数名>: <原值>},
//     redacted: [敏感键名…], messages_truncated: {reason, limit_bytes, original_bytes,
//     original_count, kept_count, dropped_count, content_cut},
//     summarized: [字段名…], dropped: [字段名…], over_budget: bool }
//
// 四种「看到的不是全量」的语义必须如实呈现，不能显示成空值、也不能显示成完整：
//   1. summarized：**键还在**，但值因超单字段上限（2KB）被替换成字符串 "omitted:NB"；
//   2. dropped：该字段整体被裁掉（8KB 总预算不足），只在 dropped 里留名；
//   3. over_budget：连受保护参数都装不下，params 被清空；
//   4. redacted / messages_truncated：敏感键值被置成 "redacted"；消息体超 256KB 时
//      「保 system + 保最新、从最旧裁」（见 CallLogPanel 的消息区提示）。
// 另外 tools 恒为摘要（只留数量与前 20 个工具名，不含 schema），也会记进 summarized。

/** req_messages 被裁剪时的标记（后端 msgTruncInfo）。 */
export interface MsgTruncInfo {
  Reason?: string
  LimitBytes?: number
  OriginalBytes?: number
  OriginalCount?: number
  KeptCount?: number
  DroppedCount?: number
  // 因单条正文过长而被截断正文的条数
  ContentCut?: number
}

/** 快照视图（键名统一 PascalCase，便于模板直接读）。 */
export interface ReqParamsView {
  Endpoint?: string
  InboundProtocol?: string
  UpstreamProtocol?: string
  UpstreamURL?: string
  Converted: boolean
  TokenID?: string
  Model?: string
  AutoModelName?: string
  Stream: boolean
  MessageCount?: number
  Tools: { Count: number; Names: string[] } | null
  Params: Array<ParamRow>
  // 命中敏感键、值已被置为 "redacted" 的参数名
  Redacted: string[]
  // 消息体被裁剪（有值即表示「消息不是全量」）
  MessagesTruncated: MsgTruncInfo | null
  Summarized: string[]
  Dropped: string[]
  OverBudget: boolean
}

/** 单个入站参数的展示行。status 决定视觉标记。 */
export interface ParamRow {
  Key: string
  // ok=原值；omitted=被替换成 omitted:NB 占位；dropped=字段整体被裁（值不可见）；
  // redacted=命中敏感键，值被置成 "redacted"（原文不落库）
  Status: 'ok' | 'omitted' | 'dropped' | 'redacted'
  // 被省略时的原始体积（字节），来自 "omitted:NB" 标记
  OmittedBytes?: number
  // 已格式化的值（字符串直接给原文，对象/数组给缩进 JSON）
  Text: string
  // 值是否值得折叠（长 JSON）
  Long: boolean
}

// "omitted:2048B" → 2048
const OMITTED_RE = /^omitted:(\d+)B$/

function str(v: unknown): string | undefined {
  if (typeof v === 'string') {
    const s = v.trim()
    return s === '' ? undefined : s
  }
  if (typeof v === 'number' && Number.isFinite(v)) return String(v)
  return undefined
}

function bool(v: unknown): boolean {
  return v === true
}

function num(v: unknown): number | undefined {
  if (typeof v === 'number' && Number.isFinite(v)) return v
  if (typeof v === 'string' && v.trim() !== '' && !Number.isNaN(Number(v))) return Number(v)
  return undefined
}

function strList(v: unknown): string[] {
  if (!Array.isArray(v)) return []
  return v.filter((x): x is string => typeof x === 'string' && x !== '')
}

function fmtValue(v: unknown): { text: string; long: boolean } {
  if (v == null) return { text: '—', long: false }
  if (typeof v === 'string') return { text: v, long: v.length > 400 }
  if (typeof v === 'number' || typeof v === 'boolean') return { text: String(v), long: false }
  try {
    const s = JSON.stringify(v, null, 2)
    return { text: s, long: s.length > 400 }
  } catch {
    return { text: String(v), long: false }
  }
}

/**
 * 入口：把后端 JSONB 反序列化结果规整成视图。
 * 形状不认识（null / 非对象）返回 null —— 调用方展示「未记录」，不要假装为空参数。
 * 同时兼容 snake_case（后端契约）与 camelCase（防御性，避免契约微调即白屏）。
 */
export function toReqParamsView(raw: unknown): ReqParamsView | null {
  if (!raw || typeof raw !== 'object' || Array.isArray(raw)) return null
  const o = raw as Record<string, unknown>
  const pick = (...keys: string[]): unknown => {
    for (const k of keys) if (o[k] !== undefined && o[k] !== null) return o[k]
    return undefined
  }

  const dropped = strList(pick('dropped', 'Dropped'))
  const summarized = strList(pick('summarized', 'Summarized'))
  const redacted = strList(pick('redacted', 'Redacted'))
  const truncated = parseMsgTrunc(pick('messages_truncated', 'MessagesTruncated'))

  // tools 摘要：count/names（也接受 PascalCase）
  const toolsRaw = pick('tools', 'Tools')
  let tools: ReqParamsView['Tools'] = null
  if (toolsRaw && typeof toolsRaw === 'object') {
    const t = toolsRaw as Record<string, unknown>
    const count = num(t.count ?? t.Count)
    if (count != null) tools = { Count: count, Names: strList(t.names ?? t.Names) }
  }

  // params：按「受保护参数在前、其余按后端写入顺序」呈现；dropped 字段补成占位行，
  // 保证「有哪些键」这个契约不因裁剪而消失。
  const paramsRaw = pick('params', 'Params')
  const rows: ParamRow[] = []
  if (paramsRaw && typeof paramsRaw === 'object' && !Array.isArray(paramsRaw)) {
    for (const [k, v] of Object.entries(paramsRaw as Record<string, unknown>)) {
      const mark = typeof v === 'string' ? OMITTED_RE.exec(v) : null
      if (mark) {
        rows.push({ Key: k, Status: 'omitted', OmittedBytes: Number(mark[1]), Text: String(v), Long: false })
        continue
      }
      // 敏感键：值被后端置成 "redacted"（原文不落库）——要标出来，别让人以为是用户真传了这个值
      if (redacted.includes(k) || v === 'redacted') {
        rows.push({ Key: k, Status: 'redacted', Text: '', Long: false })
        continue
      }
      const f = fmtValue(v)
      rows.push({ Key: k, Status: 'ok', Text: f.text, Long: f.long })
    }
  }
  for (const k of dropped) {
    if (rows.some((r) => r.Key === k)) continue
    rows.push({ Key: k, Status: 'dropped', Text: '', Long: false })
  }

  return {
    Endpoint: str(pick('endpoint', 'Endpoint')),
    InboundProtocol: str(pick('inbound_protocol', 'InboundProtocol')),
    UpstreamProtocol: str(pick('upstream_protocol', 'UpstreamProtocol')),
    UpstreamURL: str(pick('upstream_url', 'UpstreamURL')),
    Converted: bool(pick('converted', 'Converted')),
    TokenID: str(pick('token_id', 'TokenID')),
    Model: str(pick('model', 'Model')),
    AutoModelName: str(pick('auto_model_name', 'AutoModelName')),
    Stream: bool(pick('stream', 'Stream')),
    MessageCount: num(pick('message_count', 'MessageCount')),
    Tools: tools,
    Params: rows,
    Redacted: redacted,
    MessagesTruncated: truncated,
    Summarized: summarized,
    Dropped: dropped,
    OverBudget: bool(pick('over_budget', 'OverBudget')),
  }
}

// 消息裁剪标记：只认识对象形状，字段缺失留 undefined（不要凭猜测补 0）。
function parseMsgTrunc(v: unknown): MsgTruncInfo | null {
  if (!v || typeof v !== 'object' || Array.isArray(v)) return null
  const o = v as Record<string, unknown>
  const n = (k: string): number | undefined => num(o[k] ?? o[pascal(k)])
  const info: MsgTruncInfo = {
    Reason: str(o.reason ?? o.Reason),
    LimitBytes: n('limit_bytes'),
    OriginalBytes: n('original_bytes'),
    OriginalCount: n('original_count'),
    KeptCount: n('kept_count'),
    DroppedCount: n('dropped_count'),
    ContentCut: n('content_cut'),
  }
  // 只要对象存在就说明发生过裁剪（后端是 omitempty，有值即有裁剪）
  return info
}

// snake_case → PascalCase（兼容 camelCase 契约微调）
function pascal(k: string): string {
  return k
    .split('_')
    .map((seg) => (seg ? seg[0].toUpperCase() + seg.slice(1) : seg))
    .join('')
}

/** 是否存在任何裁剪（用于给「请求参数」折叠面板挂提示点）。 */
export function hasClipping(v: ReqParamsView | null): boolean {
  if (!v) return false
  return (
    v.OverBudget ||
    v.Dropped.length > 0 ||
    v.Summarized.length > 0 ||
    v.Redacted.length > 0 ||
    v.MessagesTruncated != null
  )
}

/** 字节数展示（快照/消息上限、原始体积用）。 */
export function fmtBytes(n?: number): string {
  if (n == null) return '-'
  if (n >= 1024 * 1024) return `${(n / (1024 * 1024)).toFixed(1)} MB`
  if (n >= 1024) return `${Math.round(n / 1024)} KB`
  return `${n} B`
}
