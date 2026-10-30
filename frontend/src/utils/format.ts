// 统一时间展示：固定 Asia/Shanghai（东八区），不随浏览器本地时区漂移。
// 后端时间均为 UTC 存储，展示一律按东八区换算。

const fmtParts = new Intl.DateTimeFormat('en-CA', {
  timeZone: 'Asia/Shanghai',
  year: 'numeric',
  month: '2-digit',
  day: '2-digit',
  hour: '2-digit',
  minute: '2-digit',
  second: '2-digit',
  hourCycle: 'h23',
})

interface Parts {
  y: string
  m: string
  d: string
  h: string
  mi: string
  s: string
}

function parseParts(iso?: string | null): Parts | null {
  if (!iso) return null
  const dt = new Date(iso)
  if (isNaN(dt.getTime())) return null
  const p: Record<string, string> = {}
  for (const part of fmtParts.formatToParts(dt)) {
    if (part.type !== 'literal') p[part.type] = part.value
  }
  return { y: p.year, m: p.month, d: p.day, h: p.hour, mi: p.minute, s: p.second }
}

// 日期：YYYY-MM-DD
export function fmtDate(iso?: string | null): string {
  const p = parseParts(iso)
  if (!p) return iso && iso !== '-' ? String(iso).slice(0, 10) : '-'
  return `${p.y}-${p.m}-${p.d}`
}

// 时间：HH:mm:ss
export function fmtTime(iso?: string | null): string {
  const p = parseParts(iso)
  if (!p) return '-'
  return `${p.h}:${p.mi}:${p.s}`
}

// 日期时间：YYYY-MM-DD HH:mm:ss
export function fmtDateTime(iso?: string | null): string {
  const p = parseParts(iso)
  if (!p) return '-'
  return `${p.y}-${p.m}-${p.d} ${p.h}:${p.mi}:${p.s}`
}

// RFC3339 本地化：固定 +08:00 偏移（Asia/Shanghai），供统计区间请求参数使用。
export function toRFC3339CN(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(
    d.getMinutes(),
  )}:${pad(d.getSeconds())}+08:00`
}

// 大额计数紧凑格式化（K/M/B）：统计页三处吞吐量（首屏 / 模型广场 / 实时区）与
// 各处 Token 展示共用同一套规则，不要在各组件里各写一份。
//   1234 → 1.23K   1234567 → 1.23M   865.4 → 865   12.34 → 12.3   0.5 → 0.50
//   999.6 → 1.00K（四舍五入会进位到 1000 的值直接升档，不出现「1000」这种未缩写形态）
export function fmtCompact(n: number): string {
  const v = Number(n ?? 0)
  if (!Number.isFinite(v)) return '—'
  if (v === 0) return '0'
  const abs = Math.abs(v)
  if (abs < 999.5) {
    if (abs >= 1) return v.toFixed(abs >= 100 ? 0 : 1)
    return v.toFixed(2)
  }
  let base = 1e3
  let suffix = 'K'
  if (abs >= 999.5e3) {
    base = 1e6
    suffix = 'M'
  }
  if (abs >= 999.5e6) {
    base = 1e9
    suffix = 'B'
  }
  return `${(v / base).toFixed(2)}${suffix}`
}
