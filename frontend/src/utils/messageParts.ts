// 消息内容 → 展示气泡的转换工具（展示层按结构化渲染，网关零耦合）。
// 与后端名称选取的 xmlSplit 语义保持一致。

/** 按 XML 标签切分后的文本片段：tag 为标签名（空 = 游离于标签外的正文）。 */
export interface XmlPart {
  tag: string | null
  body: string
}

const XML_NAME_RE = /^[A-Za-z_][A-Za-z0-9._-]*$/

/** 把文本按成对 XML 标签 <name…>…</name> 切分为片段，保持原文顺序。 */
export function wrapXml(s: string): XmlPart[] {
  const parts: XmlPart[] = []
  const pushFree = (free: string) => {
    const t = free.trim()
    if (t) parts.push({ tag: null, body: t })
  }
  let i = 0
  const n = s.length
  while (i < n) {
    const lt = s.indexOf('<', i)
    if (lt < 0) break
    if (lt > i) pushFree(s.slice(i, lt))
    const gt = s.indexOf('>', lt)
    if (gt < 0) {
      pushFree(s.slice(lt))
      break
    }
    const hdr = s.slice(lt + 1, gt).trim()
    if (!hdr || hdr.startsWith('/') || hdr.startsWith('!') || hdr.startsWith('?')) {
      // 闭合标签 / 注释 / 声明：按游离文本跳过本对 < >
      pushFree(s.slice(lt, gt + 1))
      i = gt + 1
      continue
    }
    let name = hdr
    const sp = hdr.search(/[\s/]/)
    if (sp >= 0) name = hdr.slice(0, sp)
    if (!XML_NAME_RE.test(name)) {
      pushFree(s.slice(lt, gt + 1))
      i = gt + 1
      continue
    }
    const closeTag = `</${name}>`
    const ci = s.indexOf(closeTag, gt + 1)
    if (ci < 0) {
      // 无配对闭合：开始标签按游离文本处理
      pushFree(s.slice(lt, gt + 1))
      i = gt + 1
      continue
    }
    const inner = s.slice(gt + 1, ci).trim()
    if (inner) parts.push({ tag: name, body: inner })
    i = ci + closeTag.length
  }
  if (i < n) pushFree(s.slice(i))
  return parts
}

/** 格式化 JSON（非对象原样字符串化）。 */
export function fmtJson(v: unknown): string {
  if (v == null) return ''
  try {
    return JSON.stringify(v, null, 2)
  } catch {
    return String(v)
  }
}

/** 展示气泡：text 正文 / xml 标签块 / image 图片 / tool 工具调用 / json 原始结构。 */
export type BubbleKind = 'text' | 'xml' | 'image' | 'tool' | 'json'

export interface MsgBubble {
  kind: BubbleKind
  tag?: string // kind=xml 时的标签名
  text: string
  url?: string // kind=image
}

/** 纯文本 → 气泡列表：含 XML 标签按标签切分，无标签则单个文本气泡。 */
function htmlBubbles(s: string): MsgBubble[] {
  const parts = wrapXml(s)
  if (!parts.length) return []
  return parts.map((p) =>
    p.tag
      ? { kind: 'xml' as const, tag: p.tag, text: p.body }
      : { kind: 'text' as const, text: p.body },
  )
}

/**
 * content（string 或 ChatGPT 多模态数组）→ 多气泡：
 * - 纯文本 string → 单个文本气泡（含 XML 则按标签拆分为带标注气泡）
 * - 多模态数组 → 每项按 type 渲染：text → 文本气泡；image_url → 图片气泡；
 *   tool_use → 工具气泡；其余 → JSON 气泡
 */
export function contentToBubbles(content: unknown): MsgBubble[] {
  if (content == null) return []
  if (typeof content === 'string') return htmlBubbles(content)
  if (Array.isArray(content)) {
    const out: MsgBubble[] = []
    for (const item of content) {
      if (typeof item === 'string') {
        out.push(...htmlBubbles(item))
        continue
      }
      if (!item || typeof item !== 'object') continue
      const p = item as Record<string, any>
      const type = String(p.type || '')
      if (type === 'text' || type === 'input_text') {
        const t = typeof p.text === 'string' ? p.text : textBlocksOf(p.content)
        if (t.trim()) out.push(...htmlBubbles(t))
      } else if (type === 'image_url') {
        const url =
          typeof p.image_url === 'string' ? p.image_url : (p.image_url?.url as string | undefined)
        if (url) out.push({ kind: 'image', text: '', url })
      } else if (type === 'tool_use') {
        out.push({ kind: 'tool', text: fmtJson(p) })
      } else {
        out.push({ kind: 'json', text: fmtJson(p) })
      }
    }
    return out
  }
  return [{ kind: 'json', text: fmtJson(content) }]
}

/** 递归收集 content 内的文本段（多模态嵌套）。 */
function textBlocksOf(v: unknown): string {
  if (v == null) return ''
  if (typeof v === 'string') return v
  if (Array.isArray(v)) {
    return v
      .map((x: any) => {
        if (typeof x === 'string') return x
        if (x && typeof x === 'object') {
          if (typeof x.text === 'string') return x.text
          if (typeof x.content === 'string' || Array.isArray(x.content)) return textBlocksOf(x.content)
        }
        return ''
      })
      .join('\n')
  }
  return ''
}

// ===== 消息 / 响应解析（会话时间线与账单详情共用，避免两处口径漂移） =====

/** 一条聊天消息（请求增量 / 会话消息）。 */
export interface ChatMessage {
  role?: string
  content?: unknown
}

/** 请求增量 / 会话消息列表：非数组返回空；元素非对象时按 {content} 包装（兼容纯字符串数组）。 */
export function asMessageList(v: unknown): ChatMessage[] {
  if (!Array.isArray(v)) return []
  return v.map((m) => (m && typeof m === 'object' ? (m as ChatMessage) : { content: m }))
}

/** 响应体的结构化内容：正文 / 推理 / 工具调用。 */
export interface RespInfo {
  text: string
  reasoning: string
  toolCalls: unknown[]
}

/**
 * 响应体 → 结构化内容：
 * 兼容非流式消息对象、流式 assistant 增量、OpenAI choices/message/delta 包裹层与纯文本；
 * 无法解析为 JSON 时整体按纯文本返回。三者为空表示「非消息形状」（如上游错误体），
 * 调用方应展示原始 JSON，不要吞掉原文。
 */
export function parseRespBody(body: string): RespInfo {
  const out: RespInfo = { text: '', reasoning: '', toolCalls: [] }
  if (!body) return out
  const walk = (p: unknown): void => {
    if (p == null) return
    if (typeof p === 'string') {
      if (p) out.text += p
      return
    }
    if (Array.isArray(p)) {
      for (const x of p) walk(x)
      return
    }
    if (typeof p === 'object') {
      const o = p as Record<string, any>
      if (typeof o.content === 'string' && o.content) out.text += o.content
      if (Array.isArray(o.content)) {
        for (const c of o.content) {
          if (typeof c === 'string' && c) out.text += c
          else if (c && typeof c.text === 'string' && c.text) out.text += c.text
        }
      }
      // 推理过程（推理型模型 delta 携带）
      if (typeof o.reasoning_content === 'string' && o.reasoning_content) out.reasoning += o.reasoning_content
      if (Array.isArray(o.reasoning_content)) {
        for (const c of o.reasoning_content) {
          if (typeof c === 'string' && c) out.reasoning += c
          else if (c && typeof c.text === 'string' && c.text) out.reasoning += c.text
        }
      }
      if (Array.isArray(o.tool_calls)) {
        out.toolCalls.push(...o.tool_calls.filter((t: any) => t && (t.function || t.id)))
      }
      // 包裹层（OpenAI choices / message / delta）
      for (const k of ['choices', 'message', 'delta']) {
        const inner = o[k]
        if (Array.isArray(inner)) for (const x of inner) walk(x)
        else if (inner && typeof inner === 'object') walk(inner)
      }
    }
  }
  try {
    walk(JSON.parse(body))
  } catch {
    return { text: body, reasoning: '', toolCalls: [] }
  }
  return out
}

/** 响应体 → 可直接交给 contentToBubbles 的内容：JSON 可解析则给解析结果，否则给原文。 */
export function contentOfBody(body: string): unknown {
  if (!body) return ''
  try {
    return JSON.parse(body)
  } catch {
    return body
  }
}
