// 账单页时间区间初值：把 range 关键字（today|yesterday|month）换算为
// Asia/Shanghai 自然日/月区间，输出 `YYYY-MM-DDTHH:mm:ss+08:00`（RFC3339）。
// 固定按 +08:00 计算，与后端统计口径（Asia/Shanghai）一致，不受浏览器本地时区影响。
const SH_MS = 8 * 60 * 60 * 1000

// 返回当前「上海墙上时间」的年月日（借 UTC 字段读取，避免本地时区干扰）。
function shToday(): { year: number; month: number; day: number } {
  const t = new Date(Date.now() + SH_MS)
  return { year: t.getUTCFullYear(), month: t.getUTCMonth(), day: t.getUTCDate() }
}

function fmtSH(year: number, month: number, day: number, hh: number, mm: number, ss: number): string {
  const p = (n: number) => String(n).padStart(2, '0')
  return `${year}-${p(month + 1)}-${p(day)}T${p(hh)}:${p(mm)}:${p(ss)}+08:00`
}

export function resolveBillingRange(kind: unknown): [string, string] | null {
  if (kind !== 'today' && kind !== 'yesterday' && kind !== 'month') return null
  const { year, month, day } = shToday()
  const startOfDay = (y: number, mo: number, d: number) => fmtSH(y, mo, d, 0, 0, 0)
  const endOfDay = (y: number, mo: number, d: number) => fmtSH(y, mo, d, 23, 59, 59)
  if (kind === 'today') return [startOfDay(year, month, day), endOfDay(year, month, day)]
  if (kind === 'yesterday') {
    const y = new Date(Date.UTC(year, month, day - 1))
    return [
      startOfDay(y.getUTCFullYear(), y.getUTCMonth(), y.getUTCDate()),
      endOfDay(y.getUTCFullYear(), y.getUTCMonth(), y.getUTCDate()),
    ]
  }
  return [startOfDay(year, month, 1), endOfDay(year, month, day)]
}
