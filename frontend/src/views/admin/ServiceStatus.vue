<script setup lang="ts">
import { computed, onMounted, onUnmounted, reactive, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { Refresh } from '@element-plus/icons-vue'
import {
  getServiceStatus,
  probeChannelKey,
  probeChannelModel,
  channelModelState,
  channelKeyState,
  listModelEvents,
  listProbeLogs,
  listChannelKeyEvents,
  type ServiceStatusView,
  type ChannelDetail,
  type ModelDetail,
  type KeyDetail,
  type MachineMetrics,
  type CandidateItem,
  type ExternalModelItem,
  type ReliabilityConfig,
} from '@/api/admin'
import StatusTag from '@/components/StatusTag.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'
import { fmtDateTime } from '@/utils/format'

// ===== 维度与行模型 =====
// 维度 key：external=对外模型 / internal=内部模型 / channel=渠道 / key=密钥。
// 页面固定全量展示四个维度（不再提供勾选）：行轴 = 四维度叉积；四个维度列恒定可见。
// 对外模型与内部模型共享同一条「模型轴」（值同源 ModelDetail.View）。
// 维度固定全量展示：四个维度列恒定可见，行轴恒为「对外模型 × 内部模型 × 渠道 × 密钥」的叉积。
const dims = ref<string[]>(['external', 'internal', 'channel', 'key'])

// 刷新间隔（秒；manual 表示不自动刷新）。
const refreshInterval = ref('30') // 默认 30 秒

// 通路矩阵一行 = 四个维度叉积（对外模型 × 内部模型 × 渠道 × 密钥），每行一条独立通路。
// model 仅在模型轴进入行轴时有值；key 仅在密钥轴进入行轴时有值。
interface MatrixRow {
  _id: string
  channel: ChannelDetail
  model: ModelDetail | null
  key: KeyDetail | null
  models: ModelDetail[] // 该行覆盖的内部模型集合（折叠时为其渠道全部模型）
  keys: KeyDetail[] // 该行覆盖的密钥集合（折叠时为其渠道全部密钥）
}

const view = ref<ServiceStatusView | null>(null)
const loading = ref(false)
const errInfo = ref({ message: '', requestId: '' })

// 轴开关：四个维度恒为全量展示，以下开关恒为 true（保留以便阅读语义）。
const modelAxis = computed(() => dims.value.includes('external') || dims.value.includes('internal'))
const keyAxis = computed(() => dims.value.includes('key'))

const externalModels = computed<ExternalModelItem[]>(() => view.value?.ExternalModels || [])
const externalNameByID = computed<Record<string, string>>(() => {
  const map: Record<string, string> = {}
  for (const e of externalModels.value) map[e.ID] = e.ExternalName
  return map
})
// 数字 ID 升序（雪花 ID 超出 Number 安全整数范围，须用 BigInt 精确比较）；非纯数字按字典序。
function idCmp(a: string, b: string): number {
  if (a === b) return 0
  try {
    const ba = BigInt(a)
    const bb = BigInt(b)
    return ba < bb ? -1 : ba > bb ? 1 : 0
  } catch {
    return a < b ? -1 : a > b ? 1 : 0
  }
}
// 排序键：对外模型标识 | 内部模型标识 | 渠道 ID | 密钥 ID（稳定顺序，刷新后不跳动）。
function cmpRow(a: MatrixRow, b: MatrixRow): number {
  const ae = a.model ? a.model.View.ExternalModelID : ''
  const be = b.model ? b.model.View.ExternalModelID : ''
  const ai = a.model ? a.model.View.InternalModelID : ''
  const bi = b.model ? b.model.View.InternalModelID : ''
  return (
    idCmp(ae, be) ||
    (ai < bi ? -1 : ai > bi ? 1 : 0) ||
    idCmp(a.channel.View.ID, b.channel.View.ID) ||
    idCmp(a.key ? a.key.View.KeyID : '', b.key ? b.key.View.KeyID : '')
  )
}

// 生成通路行：行轴 = 已勾选维度叉积；折叠维度的覆盖集合在行内聚合。
//   - 模型轴 + 密钥轴 → (模型, 渠道, 密钥)
//   - 模型轴仅 → (模型, 渠道)
//   - 密钥轴仅 → (渠道, 密钥)
//   - 仅渠道 → 各渠道
const rows = computed<MatrixRow[]>(() => {
  const v = view.value
  if (!v) return []
  const out: MatrixRow[] = []
  const mA = modelAxis.value
  const kA = keyAxis.value
  for (const c of v.Channels) {
    if (mA && kA) {
      for (const m of c.Models) {
        for (const k of c.Keys) {
          out.push({ _id: `${c.View.ID}|${m.View.ID}|${k.View.KeyID}`, channel: c, model: m, key: k, models: [m], keys: [k] })
        }
      }
    } else if (mA) {
      for (const m of c.Models) {
        out.push({ _id: `${c.View.ID}|${m.View.ID}|`, channel: c, model: m, key: null, models: [m], keys: c.Keys })
      }
    } else if (kA) {
      for (const k of c.Keys) {
        out.push({ _id: `${c.View.ID}||${k.View.KeyID}`, channel: c, model: null, key: k, models: c.Models, keys: [k] })
      }
    } else {
      out.push({ _id: `${c.View.ID}||`, channel: c, model: null, key: null, models: c.Models, keys: c.Keys })
    }
  }
  out.sort(cmpRow)
  return out
})

// 四个维度全量展示：每行即为一条独立通路（对外模型 × 内部模型 × 渠道 × 密钥）。
// 因此不做任何单元格合并——避免「同名不同状态的行被并成一格」以及合并块随状态抖动而跳动。

// ===== 状态灯与原因 =====
// 状态严重度：DISABLED > DRAIN > NORMAL（决策 7）。折叠列状态灯取覆盖集合中最差状态。
const STATE_RANK: Record<string, number> = { NORMAL: 0, DRAIN: 1, DISABLED: 2 }
function worstState(states: string[]): string {
  let w = 'NORMAL'
  for (const s of states) if ((STATE_RANK[s] ?? 0) > (STATE_RANK[w] ?? 0)) w = s
  return w
}
// 状态中文名（含聚合态 PARTIAL）。
function stateLabel(state: string): string {
  switch (state) {
    case 'NORMAL':
    case 'HEALTHY':
    case 'ENABLED':
    case 'ACTIVE':
      return '正常'
    case 'DRAIN':
    case 'DRAIN_ONLY':
      return '排空'
    case 'DISABLED':
    case 'UNAVAILABLE':
      return '禁用'
    case 'PARTIAL':
      return '部分可用'
    default:
      return state || '未知'
  }
}
// 状态灯色档：正常=绿（ok）· 排空=橙（warn）· 禁用=红（bad）· 未知=灰（off）。
function lampState(state?: string): string {
  switch (state) {
    case 'NORMAL':
    case 'HEALTHY':
    case 'ENABLED':
    case 'ACTIVE':
      return 'ok'
    case 'DRAIN':
    case 'DRAIN_ONLY':
    case 'PARTIAL':
      return 'warn'
    case 'DISABLED':
    case 'UNAVAILABLE':
      return 'bad'
    default:
      return 'off'
  }
}
// 用量灯色档：usage/limit ≥100% 红 · ≥80% 橙 · 其余绿；无上限（≤0）灰。
function usageLamp(cur: number, limit: number): string {
  if (!Number.isFinite(limit) || limit <= 0) return 'off'
  const r = cur / limit
  if (r >= 1) return 'bad'
  if (r >= 0.8) return 'warn'
  return 'ok'
}

// 悬停原因行（danger=true 表示超出阈值，渲染为红字）。
interface ReasonLine {
  text: string
  danger?: boolean
}
function pct(v: number): string {
  return `${(Number.isFinite(v) ? v : 0).toFixed(2)}%`
}
function machineReasonLines(m: MachineMetrics, rel: ReliabilityConfig): ReasonLine[] {
  const out: ReasonLine[] = []
  if (m.DrainingReasons && m.DrainingReasons.length) {
    out.push({ text: '排空原因：' + m.DrainingReasons.join('；') })
  }
  out.push({ text: `错误率 ${pct(m.ErrorRatePct)}（阈值 ${rel.ErrorRatePct}%）`, danger: rel.ErrorRatePct > 0 && m.ErrorRatePct > rel.ErrorRatePct })
  out.push({ text: `429 占比 ${pct(m.Rate429Pct)}（阈值 ${rel.Rate429Pct}%）`, danger: rel.Rate429Pct > 0 && m.Rate429Pct > rel.Rate429Pct })
  out.push({ text: `P99 ${m.P99LatencyMS}ms（阈值 ${rel.P99LatencyMS}ms）`, danger: rel.P99LatencyMS > 0 && m.P99LatencyMS > rel.P99LatencyMS })
  out.push({ text: `窗口 ${m.WindowSeconds}s · 样本 ${m.Total} · 失败 ${m.Failures}` })
  return out
}
// 渠道无独立状态机（渠道=配置模板）：原因由其内部模型与密钥的异常项派生。
function channelReasonOf(c: ChannelDetail): ReasonLine[] {
  const lines: ReasonLine[] = []
  if (c.View.LastErr) lines.push({ text: '上次错误：' + c.View.LastErr, danger: true })
  for (const m of c.Models) {
    if (m.View.State !== 'NORMAL') {
      let t = `内部模型 ${m.View.InternalModelID}：${m.View.State}`
      if (m.View.LastErr) t += `（${m.View.LastErr}）`
      lines.push({ text: t, danger: m.View.State === 'DISABLED' })
    }
  }
  for (const k of c.Keys) {
    if (k.View.State !== 'NORMAL') {
      let t = `密钥 ${k.View.Name || k.View.KeyID}：${k.View.State}`
      if (k.View.LastErr) t += `（${k.View.LastErr}）`
      lines.push({ text: t, danger: k.View.State === 'DISABLED' })
    }
  }
  if (!lines.length) lines.push({ text: '全部内部模型与密钥正常' })
  return lines
}
function modelReasonOf(m: ModelDetail): ReasonLine[] {
  const lines = machineReasonLines(m.Metrics, m.View.Reliability)
  if (m.View.LastErr) lines.unshift({ text: '上次错误：' + m.View.LastErr, danger: true })
  return lines
}
function keyReasonOf(k: KeyDetail): ReasonLine[] {
  const lines: ReasonLine[] = []
  if (k.View.LastErr) lines.push({ text: '上次错误：' + k.View.LastErr, danger: true })
  if (k.View.DrainReasons && k.View.DrainReasons.length) {
    lines.push({ text: '排空原因：' + k.View.DrainReasons.join('；') })
  }
  if (!lines.length) lines.push({ text: '无异常' })
  return lines
}

function extEnabledOf(extId: string): boolean {
  const e = externalModels.value.find((x) => x.ID === extId)
  return !!e && e.Enabled
}
// 对外模型无自身状态机：可用性 = 其名下全部「渠道×内部模型」通路的健康度。
//   全部正常 → NORMAL（绿）；部分异常但仍有可用通路 → PARTIAL（橙·部分可用）；全不可用/停用 → DISABLED（红）。
// 不用「最差值」直接压成红色——那会把「个别渠道/密钥坏、模型整体仍可用」误报成整体不可用。
function extPathsOf(extId: string): { channel: string; model: string; state: string }[] {
  const out: { channel: string; model: string; state: string }[] = []
  for (const c of view.value?.Channels || []) {
    for (const m of c.Models) {
      if (m.View.ExternalModelID === extId) out.push({ channel: c.View.Name, model: m.View.InternalModelID, state: m.View.State })
    }
  }
  return out
}
// 对外模型无自身状态机。行口径：本行通路是否可用 = 对外模型启用 ? 本行内部模型状态 : 禁用。
// 这样同一行的「对外模型灯」与「内部模型灯」口径一致，不会出现「对外黄、内部绿」的矛盾。
// 全局可用性（跨渠道 x/y 通路）只放在 tooltip 里说明，不再用一个聚合色灯误导。
function extRowState(r: MatrixRow): string {
  if (!r.model) return 'NORMAL'
  return extEnabledOf(r.model.View.ExternalModelID) ? r.model.View.State : 'DISABLED'
}
function extReasonOf(extId: string): ReasonLine[] {
  const enabled = extEnabledOf(extId)
  const paths = extPathsOf(extId)
  const healthy = paths.filter((p) => p.state === 'NORMAL').length
  const lines: ReasonLine[] = [{ text: enabled ? '启用' : '停用', danger: !enabled }]
  if (!enabled) {
    lines.push({ text: '已停用：不参与路由' })
    return lines
  }
  lines.push({ text: `全局可用通路 ${healthy}/${paths.length}`, danger: healthy === 0 })
  for (const p of paths) {
    if (p.state !== 'NORMAL') lines.push({ text: `渠道 ${p.channel} · 模型 ${p.model}：${stateLabel(p.state)}`, danger: p.state === 'DISABLED' })
  }
  if (healthy === paths.length) lines.push({ text: '全部通路正常' })
  else if (healthy > 0) lines.push({ text: '仍有可用通路，模型整体可用' })
  return lines
}

// 折叠摘要文案：覆盖多个值时形如「共 n 项 · n 项异常」；恰好 1 值时正常显示单值。
function foldText(n: number, abnormal: number): string {
  return `共 ${n} 项 · ${abnormal} 项异常`
}
function foldModelsReason(ms: ModelDetail[]): ReasonLine[] {
  const lines: ReasonLine[] = []
  for (const m of ms) {
    if (m.View.State !== 'NORMAL') {
      let t = `${m.View.InternalModelID}：${m.View.State}`
      if (m.View.LastErr) t += `（${m.View.LastErr}）`
      lines.push({ text: t, danger: m.View.State === 'DISABLED' })
    }
  }
  if (!lines.length) lines.push({ text: '全部内部模型正常' })
  return lines
}
function foldKeysReason(ks: KeyDetail[]): ReasonLine[] {
  const lines: ReasonLine[] = []
  for (const k of ks) {
    if (k.View.State !== 'NORMAL') {
      let t = `${k.View.Name || k.View.KeyID}：${k.View.State}`
      if (k.View.LastErr) t += `（${k.View.LastErr}）`
      lines.push({ text: t, danger: k.View.State === 'DISABLED' })
    }
  }
  if (!lines.length) lines.push({ text: '全部密钥正常' })
  return lines
}

// 维度单元格视图模型：勾选列=单值；折叠列覆盖多值时=聚合摘要。
// 规范（不允许隐性逻辑）：维度列只展示名称（不含 ID / 行主键），状态以状态灯表达。
interface DimCell {
  state: string
  name: string
  sub: string
  reason: ReasonLine[]
  multi: boolean
}
function externalCell(r: MatrixRow): DimCell {
  const extId = r.model ? r.model.View.ExternalModelID : r.models[0]?.View.ExternalModelID
  if (!extId) return { state: 'NORMAL', name: '—', sub: '', reason: [{ text: '无对外模型' }], multi: false }
  return { state: extRowState(r), name: externalNameByID.value[extId] || extId, sub: '', reason: extReasonOf(extId), multi: false }
}
function internalCell(r: MatrixRow): DimCell {
  const ms = r.models
  if (ms.length === 0) return { state: 'NORMAL', name: '—', sub: '', reason: [{ text: '无内部模型' }], multi: false }
  if (ms.length === 1) {
    const m = ms[0]
    return { state: m.View.State, name: m.View.InternalModelID, sub: '', reason: modelReasonOf(m), multi: false }
  }
  return {
    state: worstState(ms.map((m) => m.View.State)),
    name: foldText(ms.length, ms.filter((m) => m.View.State !== 'NORMAL').length),
    sub: '',
    reason: foldModelsReason(ms),
    multi: true,
  }
}
function channelCell(r: MatrixRow): DimCell {
  return { state: r.channel.State, name: r.channel.View.Name, sub: '', reason: channelReasonOf(r.channel), multi: false }
}
function keyCell(r: MatrixRow): DimCell {
  const ks = r.keys
  if (ks.length === 0) return { state: 'NORMAL', name: '—', sub: '', reason: [{ text: '无密钥' }], multi: false }
  if (ks.length === 1) {
    const k = ks[0]
    return { state: k.View.State, name: k.View.Name || `密钥 ${k.View.KeyID}`, sub: k.View.CredentialTail || '', reason: keyReasonOf(k), multi: false }
  }
  return {
    state: worstState(ks.map((k) => k.View.State)),
    name: foldText(ks.length, ks.filter((k) => k.View.State !== 'NORMAL').length),
    sub: '',
    reason: foldKeysReason(ks),
    multi: true,
  }
}
// 预计算每行的四个维度单元格，供模板取用。
const EMPTY_CELL: DimCell = { state: 'NORMAL', name: '—', sub: '', reason: [], multi: false }
const cellsByRow = computed<Record<string, { external: DimCell; internal: DimCell; channel: DimCell; key: DimCell }>>(() => {
  const map: Record<string, { external: DimCell; internal: DimCell; channel: DimCell; key: DimCell }> = {}
  for (const r of rows.value) {
    map[r._id] = { external: externalCell(r), internal: internalCell(r), channel: channelCell(r), key: keyCell(r) }
  }
  return map
})
function cell(row: MatrixRow, dim: 'external' | 'internal' | 'channel' | 'key'): DimCell {
  return cellsByRow.value[row._id]?.[dim] ?? EMPTY_CELL
}

// ===== 序位色条（决策 5 / R9）=====
// 仅在「该行所属对外模型已在 Candidates 中有键」时出现；顺序唯一来自后端 Rank，前端绝不重算。
// 需模型轴与密钥轴同时进入行轴（否则该行覆盖多通路，无法映射单一候选）。
function rankOf(r: MatrixRow): number | 'none' | null {
  if (!r.model || !r.key) return null
  const list = view.value?.Candidates?.[r.model.View.ExternalModelID]
  if (!list) return null
  const c = list.find((x) => x.ChannelID === r.channel.View.ID && x.ChannelKeyID === r.key!.View.KeyID)
  return c ? c.Rank : 'none'
}
function rankClass(r: MatrixRow): string {
  const rank = rankOf(r)
  if (rank === null) return ''
  if (rank === 'none') return 'rank-none'
  if (rank === 1) return 'rank-first'
  return 'rank-second'
}
// 序位悬停释义（色条即序位语义）。
function rankTip(r: MatrixRow): string {
  const rank = rankOf(r)
  if (rank === null) return ''
  if (rank === 'none') return '不参与新会话（未进入候选排序）'
  if (rank === 1) return '首选（第 1 候选）'
  if (rank === 2) return '次选（第 2 候选）'
  return `第 ${rank} 候选`
}

// ===== 指标列取值 =====
function fmtNum(v: number): string {
  if (!Number.isFinite(v)) return '—'
  return String(Number(v.toFixed(6)))
}
function fmtInt(v: number): string {
  return Number.isFinite(v) ? String(v) : '—'
}
// TPM 等大额计数统一紧凑格式化（K/M/B），与「会话」等处保持一致。
function compact(v: number): string {
  if (!Number.isFinite(v)) return '—'
  const abs = Math.abs(v)
  if (abs >= 1e9) return `${(v / 1e9).toFixed(2)}B`
  if (abs >= 1e6) return `${(v / 1e6).toFixed(2)}M`
  if (abs >= 1e3) return `${(v / 1e3).toFixed(2)}K`
  return fmtInt(v)
}
// 「RPM 当前/上限 · TPM 当前/上限」文本（表格与弹窗共用）。
function rpmTpmText(rpm: number, rpmLimit: number, tpm: number, tpmLimit: number): string {
  return `RPM ${fmtInt(rpm)}/${fmtInt(rpmLimit)} · TPM ${compact(tpm)}/${compact(tpmLimit)}`
}
// 模型级 RPM/TPM：（渠道 × 内部模型）级；模型折叠时按组聚合（该行覆盖模型的合计）。
function modelRpmVal(r: MatrixRow): number {
  if (modelAxis.value) return r.model ? r.model.RPM : 0
  return r.models.reduce((s, m) => s + m.RPM, 0)
}
function modelRpmLimit(r: MatrixRow): number {
  if (modelAxis.value) return r.model ? r.model.RPMLimit : 0
  return r.models.reduce((s, m) => s + m.RPMLimit, 0)
}
function modelTpmVal(r: MatrixRow): number {
  if (modelAxis.value) return r.model ? r.model.TPM : 0
  return r.models.reduce((s, m) => s + m.TPM, 0)
}
function modelTpmLimit(r: MatrixRow): number {
  if (modelAxis.value) return r.model ? r.model.TPMLimit : 0
  return r.models.reduce((s, m) => s + m.TPMLimit, 0)
}
// 密钥级 RPM/TPM 与会话数：密钥折叠时按组聚合（该行覆盖密钥的合计）。
function keyRpmVal(r: MatrixRow): number {
  if (keyAxis.value) return r.key ? r.key.RPM : 0
  return r.keys.reduce((s, k) => s + k.RPM, 0)
}
function keyRpmLimit(r: MatrixRow): number {
  if (keyAxis.value) return r.key ? r.key.RPMLimit : 0
  return r.keys.reduce((s, k) => s + k.RPMLimit, 0)
}
function keyTpmVal(r: MatrixRow): number {
  if (keyAxis.value) return r.key ? r.key.TPM : 0
  return r.keys.reduce((s, k) => s + k.TPM, 0)
}
function keyTpmLimit(r: MatrixRow): number {
  if (keyAxis.value) return r.key ? r.key.TPMLimit : 0
  return r.keys.reduce((s, k) => s + k.TPMLimit, 0)
}
// 会话数 = 存活会话 ActiveSessions；上限 = MaxSessions（密钥级并发上限）。
function sessionsVal(r: MatrixRow): number {
  if (keyAxis.value) return r.key ? r.key.ActiveSessions : 0
  return r.keys.reduce((s, k) => s + k.ActiveSessions, 0)
}
function sessionsLimit(r: MatrixRow): number {
  if (keyAxis.value) return r.key ? r.key.MaxSessions : 0
  return r.keys.reduce((s, k) => s + k.MaxSessions, 0)
}

// 合并限流列：按行预计算模型级/密钥级两组「值-上限」，单元格内两行并置。
interface LimitCell {
  model: { rpm: number; rpmLimit: number; tpm: number; tpmLimit: number }
  key: { rpm: number; rpmLimit: number; tpm: number; tpmLimit: number }
}
const EMPTY_LIMIT: LimitCell = {
  model: { rpm: 0, rpmLimit: 0, tpm: 0, tpmLimit: 0 },
  key: { rpm: 0, rpmLimit: 0, tpm: 0, tpmLimit: 0 },
}
const limitsByRow = computed<Record<string, LimitCell>>(() => {
  const map: Record<string, LimitCell> = {}
  for (const r of rows.value) {
    map[r._id] = {
      model: { rpm: modelRpmVal(r), rpmLimit: modelRpmLimit(r), tpm: modelTpmVal(r), tpmLimit: modelTpmLimit(r) },
      key: { rpm: keyRpmVal(r), rpmLimit: keyRpmLimit(r), tpm: keyTpmVal(r), tpmLimit: keyTpmLimit(r) },
    }
  }
  return map
})
function lm(row: MatrixRow): LimitCell {
  return limitsByRow.value[row._id] ?? EMPTY_LIMIT
}

// 弹窗展示用：可靠性阈值与窗口指标文本。
function relText(r: ReliabilityConfig): string {
  return `错误率 <${r.ErrorRatePct}% · 429 <${r.Rate429Pct}% · P99 <${r.P99LatencyMS}ms`
}
function metricsText(m: MachineMetrics): string {
  return `错误率 ${pct(m.ErrorRatePct)} · 429 ${pct(m.Rate429Pct)} · P99 ${m.P99LatencyMS}ms`
}
// 探测时间展示：空值统一用「—」占位，与其它空值占位一致。
function dt(v?: string | null): string {
  return v ? fmtDateTime(v) : '—'
}

// 成本比较样值（候选区样值说明用）。
const sampleUsageText = computed(() => {
  const s = view.value?.SampleUsage
  if (!s) return ''
  return `input/output/cache_read = ${fmtNum(s.input)}/${fmtNum(s.output)}/${fmtNum(s.cache_read)}`
})

// ===== 选中行与详情弹窗 =====
const selectedId = ref<string | null>(null)
const selectedRow = computed<MatrixRow | null>(() => (selectedId.value ? rows.value.find((r) => r._id === selectedId.value) || null : null))
const detailVisible = ref(false)
// 顶层 tab：详情 / 事件链。打开弹窗默认停在「详情」。
const activeTopTab = ref<'detail' | 'event'>('detail')
// 切换行才重置事件链缓存（loadedFor 与数据同清）；重复选中同一行保留已加载数据，
// 避免清空后因 selectedId 未变、watch 不触发而导致事件链恒为空。
function selectRow(row: MatrixRow) {
  if (selectedId.value !== row._id) resetTabs()
  selectedId.value = row._id
}
// 点击列表任意行 → 打开「详情」弹窗（交互对齐「渠道」列表：行即入口，不再占用右侧栏）。
function onRowClick(row: MatrixRow) {
  selectRow(row)
  activeTopTab.value = 'detail'
  detailVisible.value = true
}

// ===== 事件链 tab（渠道无独立状态机，故无「渠道事件」tab；仅在切到事件链 tab 时按需加载）=====
const tabKeys = ['model', 'probe', 'key'] as const
type TabKey = (typeof tabKeys)[number]
interface TabRec {
  data: any[]
  loading: boolean
  error: { message: string; requestId: string }
  loadedFor: string
}
function blankTab(): TabRec {
  return { data: [], loading: false, error: { message: '', requestId: '' }, loadedFor: '' }
}
const tabState = reactive<Record<TabKey, TabRec>>({
  model: blankTab(),
  probe: blankTab(),
  key: blankTab(),
})
const activeTab = ref<TabKey>('model')
function resetTabs() {
  for (const k of tabKeys) tabState[k] = blankTab()
}
async function ensureTab(tab: TabKey) {
  const row = selectedRow.value
  if (!row) return
  const rec = tabState[tab]
  if (rec.loadedFor === row._id) return
  rec.loading = true
  rec.error = { message: '', requestId: '' }
  try {
    const chId = row.channel.View.ID
    if (tab === 'model') rec.data = (await listModelEvents(chId)).Data || []
    else if (tab === 'probe') rec.data = (await listProbeLogs(chId, 50)).Data || []
    else if (row.key) rec.data = (await listChannelKeyEvents(chId, row.key.View.KeyID)).Data || []
    else rec.data = []
  } catch (e: any) {
    rec.error = { message: e?.message || '', requestId: e?.requestId || '' }
  } finally {
    // 无论成功或失败都标记已加载，避免轮询触发时反复报错。
    rec.loadedFor = row._id
    rec.loading = false
  }
}
// 切到事件链顶层 tab、或事件链内切换子 tab 时按需加载（loadedFor 去重）。
watch(activeTopTab, (t) => {
  if (t === 'event') ensureTab(activeTab.value)
})
watch(activeTab, () => {
  if (activeTopTab.value === 'event') ensureTab(activeTab.value)
})

// ===== 候选区（详情弹窗）=====
const selectedCandidates = computed<CandidateItem[]>(() => {
  const r = selectedRow.value
  if (!r?.model) return []
  return view.value?.Candidates?.[r.model.View.ExternalModelID] || []
})
function isSelectedCandidate(c: CandidateItem): boolean {
  const r = selectedRow.value
  if (!r || !r.key) return false
  return c.ChannelID === r.channel.View.ID && c.ChannelKeyID === r.key.View.KeyID
}
// 候选序位显示名称（回退显示 ID）：渠道名 / 密钥名（尾号）。
const channelNameByID = computed<Record<string, string>>(() => {
  const map: Record<string, string> = {}
  for (const c of view.value?.Channels || []) map[c.View.ID] = c.View.Name
  return map
})
const keyNameByID = computed<Record<string, { name: string; tail: string }>>(() => {
  const map: Record<string, { name: string; tail: string }> = {}
  for (const c of view.value?.Channels || []) {
    for (const k of c.Keys) map[`${c.View.ID}|${k.View.KeyID}`] = { name: k.View.Name || '', tail: k.View.CredentialTail || '' }
  }
  return map
})
function channelLabel(id: string): string {
  return channelNameByID.value[id] || `渠道 ${id}`
}
function keyLabel(chId: string, keyId: string): string {
  const k = keyNameByID.value[`${chId}|${keyId}`]
  if (!k) return `密钥 ${keyId}`
  const nm = k.name || `密钥 ${keyId}`
  return k.tail ? `${nm}（${k.tail}）` : nm
}

// ===== 快速恢复操作编排（复用既有已审计写端点，不新增端点）=====
const opLoading = ref(false)
const opErr = ref({ message: '', requestId: '' })
async function runOp(fn: () => Promise<any>, okText: (data: any) => string) {
  opLoading.value = true
  opErr.value = { message: '', requestId: '' }
  try {
    const res = await fn()
    ElMessage.success(okText(res?.Data))
    await load()
  } catch (e: any) {
    opErr.value = { message: e?.message || '', requestId: e?.requestId || '' }
  } finally {
    opLoading.value = false
  }
}
function probeMsg(d: any): string {
  if (!d) return '探测完成'
  return `探测${d.OK ? '成功' : '失败'} · 状态 ${d.State ?? '-'}${d.Error ? ' · ' + d.Error : ''}`
}
function doProbeKey() {
  const r = selectedRow.value
  if (!r || !r.key) return
  runOp(() => probeChannelKey(r.channel.View.ID, r.key!.View.KeyID), probeMsg)
}
function doProbeModel() {
  const r = selectedRow.value
  if (!r || !r.model) return
  const mid = r.model.View.ID
  runOp(() => probeChannelModel(r.channel.View.ID, mid), probeMsg)
}
// 渠道无独立状态机，故不支持渠道级强制状态（仅内部模型/密钥可手动流转）。
function doState(level: 'model' | 'key', action: 'normal' | 'drain' | 'disable') {
  const r = selectedRow.value
  if (!r) return
  if (level === 'model') {
    if (!r.model) return
    const mid = r.model.View.ID
    runOp(() => channelModelState(r.channel.View.ID, mid, action), () => '模型状态已更新')
  } else {
    if (!r.key) return
    runOp(() => channelKeyState(r.channel.View.ID, r.key!.View.KeyID, action), () => '密钥状态已更新')
  }
}

// ===== 加载与轮询 =====
async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await getServiceStatus()
    view.value = res.Data
  } catch (e: any) {
    errInfo.value = { message: e?.message || '', requestId: e?.requestId || '' }
  } finally {
    loading.value = false
  }
}
let timer: number | undefined
function stopTimer() {
  if (timer !== undefined) {
    window.clearInterval(timer)
    timer = undefined
  }
}
function startTimer() {
  stopTimer()
  if (refreshInterval.value === 'manual') return
  timer = window.setInterval(load, Number(refreshInterval.value) * 1000)
}

onMounted(async () => {
  await load()
  startTimer()
  watch(refreshInterval, () => startTimer())
})
onUnmounted(stopTimer)
</script>

<template>
  <div class="page-container">
    <!-- 工具条：刷新间隔 + 手动刷新 / 数据时间（维度固定全量展示，不再选择） -->
    <div class="card toolbar">
      <div class="tb-item">
        <span class="tb-label">刷新</span>
        <el-select v-model="refreshInterval" size="small" class="tb-int">
          <el-option label="5 秒" value="5" />
          <el-option label="10 秒" value="10" />
          <el-option label="30 秒" value="30" />
          <el-option label="手动" value="manual" />
        </el-select>
        <!-- 手动刷新：置于刷新间隔右侧，图标化 -->
        <el-tooltip content="手动刷新" placement="top">
          <el-button :icon="Refresh" circle size="small" :loading="loading" @click="load" />
        </el-tooltip>
      </div>
      <div class="tb-time">
        数据时间 {{ fmtDateTime(view?.DataTime) }}
        <span class="tb-hint">快照数据，允许毫秒级偏差</span>
      </div>
    </div>

    <!-- 通路矩阵（点击任意行打开「详情」弹窗） -->
    <div v-loading="loading" class="matrix card">
      <el-table
        :data="rows"
        border
        stripe
        class="table-nowrap clickable-rows"
        row-key="_id"
        @row-click="onRowClick"
      >
        <!-- 对外模型 -->
        <el-table-column label="对外模型" min-width="160">
          <template #default="{ row }">
            <div class="hcell">
              <el-tooltip placement="top" :show-after="150">
                <template #content>
                  <div class="tt">
                    <div v-for="(l, i) in cell(row, 'external').reason" :key="i" class="tt-line" :class="{ danger: l.danger }">{{ l.text }}</div>
                  </div>
                </template>
                <span class="lamp" :data-state="lampState(cell(row, 'external').state)" />
              </el-tooltip>
              <span class="hcell-name" :title="cell(row, 'external').name">{{ cell(row, 'external').name }}</span>
            </div>
          </template>
        </el-table-column>

        <!-- 内部模型 -->
        <el-table-column label="内部模型" min-width="160">
          <template #default="{ row }">
            <div class="hcell">
              <el-tooltip placement="top" :show-after="150">
                <template #content>
                  <div class="tt">
                    <div v-for="(l, i) in cell(row, 'internal').reason" :key="i" class="tt-line" :class="{ danger: l.danger }">{{ l.text }}</div>
                  </div>
                </template>
                <span class="lamp" :data-state="lampState(cell(row, 'internal').state)" />
              </el-tooltip>
              <span class="hcell-name" :title="cell(row, 'internal').name">{{ cell(row, 'internal').name }}</span>
            </div>
          </template>
        </el-table-column>

        <!-- 渠道（渠道无独立状态机，不显示派生状态灯） -->
        <el-table-column label="渠道" min-width="180">
          <template #default="{ row }">
            <div class="hcell">
              <el-tooltip placement="top" :show-after="150">
                <template #content>
                  <div class="tt">
                    <div v-for="(l, i) in cell(row, 'channel').reason" :key="i" class="tt-line" :class="{ danger: l.danger }">{{ l.text }}</div>
                  </div>
                </template>
                <span class="hcell-name" :title="cell(row, 'channel').name">{{ cell(row, 'channel').name }}</span>
              </el-tooltip>
            </div>
          </template>
        </el-table-column>

        <!-- 密钥 -->
        <el-table-column label="密钥" min-width="200">
          <template #default="{ row }">
            <div class="hcell">
              <el-tooltip v-if="rankClass(row)" :content="rankTip(row)" placement="top">
                <span class="rank-bar" :class="rankClass(row)" />
              </el-tooltip>
              <el-tooltip placement="top" :show-after="150">
                <template #content>
                  <div class="tt">
                    <div v-for="(l, i) in cell(row, 'key').reason" :key="i" class="tt-line" :class="{ danger: l.danger }">{{ l.text }}</div>
                  </div>
                </template>
                <span class="lamp" :data-state="lampState(cell(row, 'key').state)" />
              </el-tooltip>
              <span class="hcell-name" :title="cell(row, 'key').name">{{ cell(row, 'key').name }}</span>
              <span v-if="cell(row, 'key').sub" class="hcell-sub">{{ cell(row, 'key').sub }}</span>
            </div>
          </template>
        </el-table-column>

        <!-- 优先级（渠道级合并） -->
        <el-table-column label="优先级" min-width="80" align="center">
          <template #header>
            <el-tooltip content="数字越大越先被路由（渠道级）" placement="top"><span>优先级</span></el-tooltip>
          </template>
          <template #default="{ row }">{{ row.channel.View.Priority }}</template>
        </el-table-column>

        <!-- RPM / TPM 合并列：单元格内两行（上行模型级、下行密钥级），各为「当前/上限」+ 用量灯 -->
        <el-table-column label="RPM / TPM" min-width="190" align="center">
          <template #header>
            <el-tooltip placement="top">
              <template #content>
                <div class="tt">
                  <div class="tt-line">上行=模型级、下行=密钥级；均为「当前值/上限」</div>
                  <div class="tt-line">用量灯：≥100% 红 · ≥80% 橙 · 其余绿</div>
                  <div class="tt-line">折叠维度按覆盖集合求和</div>
                </div>
              </template>
              <span>RPM / TPM</span>
            </el-tooltip>
          </template>
          <template #default="{ row }">
            <div class="lmcell">
              <div class="lmrow">
                <span class="lamp" :data-state="usageLamp(lm(row).model.rpm, lm(row).model.rpmLimit)" />
                <span class="lmtext"><span class="lmk">模型</span>{{ fmtInt(lm(row).model.rpm) }}/{{ fmtInt(lm(row).model.rpmLimit) }} · {{ compact(lm(row).model.tpm) }}/{{ compact(lm(row).model.tpmLimit) }}</span>
              </div>
              <div class="lmrow">
                <span class="lamp" :data-state="usageLamp(lm(row).key.rpm, lm(row).key.rpmLimit)" />
                <span class="lmtext"><span class="lmk">密钥</span>{{ fmtInt(lm(row).key.rpm) }}/{{ fmtInt(lm(row).key.rpmLimit) }} · {{ compact(lm(row).key.tpm) }}/{{ compact(lm(row).key.tpmLimit) }}</span>
              </div>
            </div>
          </template>
        </el-table-column>

        <!-- 会话数（密钥级）：存活会话/并发上限 + 用量灯 -->
        <el-table-column label="会话数" min-width="110" align="center">
          <template #header>
            <el-tooltip content="会话数 = 存活会话 ActiveSessions；上限 = MaxSessions（密钥并发上限）" placement="top"><span>会话数</span></el-tooltip>
          </template>
          <template #default="{ row }">
            <div class="mcell">
              <span class="lamp" :data-state="usageLamp(sessionsVal(row), sessionsLimit(row))" />
              <span class="mtext">{{ fmtInt(sessionsVal(row)) }}/{{ fmtInt(sessionsLimit(row)) }}</span>
            </div>
          </template>
        </el-table-column>
      </el-table>

      <el-empty v-if="!loading && !rows.length" description="暂无通路" />
      <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
    </div>

    <!-- 详情弹窗：点击列表行打开；顶部 tab 切换「详情 / 事件链」 -->
    <el-dialog v-model="detailVisible" title="详情" width="min(1120px, 94vw)" top="3vh" class="status-dlg">
      <div v-if="!selectedRow" class="insp-empty">请选择一行查看详情</div>
      <el-tabs v-else v-model="activeTopTab" class="dlg-tabs">
        <!-- ===== 详情：四列字段对比表（列 = 对外模型→内部模型→渠道→密钥）+ 候选序位 ===== -->
        <el-tab-pane label="详情" name="detail">
          <table class="cmp-table">
            <thead>
              <tr>
                <th class="cmp-field"></th>
                <th>对外模型</th>
                <th>内部模型</th>
                <th>渠道</th>
                <th>密钥</th>
              </tr>
            </thead>
            <tbody>
              <tr>
                <td class="cmp-field">名称</td>
                <td>{{ cell(selectedRow, 'external').name }}</td>
                <td>{{ cell(selectedRow, 'internal').name }}</td>
                <td>{{ cell(selectedRow, 'channel').name }}</td>
                <td>
                  <span>{{ cell(selectedRow, 'key').name }}</span>
                  <span v-if="cell(selectedRow, 'key').sub" class="cmp-sub">{{ cell(selectedRow, 'key').sub }}</span>
                </td>
              </tr>
              <tr>
                <td class="cmp-field">状态</td>
                <td>{{ selectedRow.model ? (extEnabledOf(selectedRow.model.View.ExternalModelID) ? '启用' : '停用') : '—' }}</td>
                <td><StatusTag :value="cell(selectedRow, 'internal').state" /></td>
                <td class="cmp-muted">—</td>
                <td><StatusTag :value="cell(selectedRow, 'key').state" /></td>
              </tr>
              <tr>
                <td class="cmp-field">协议 · 接口地址</td>
                <td class="cmp-muted">—</td>
                <td class="cmp-muted">—</td>
                <td>
                  <div>{{ selectedRow.channel.View.Protocol }}</div>
                  <div class="cmp-sub">{{ selectedRow.channel.View.BaseURL }}</div>
                </td>
                <td class="cmp-muted">—</td>
              </tr>
              <tr>
                <td class="cmp-field">优先级</td>
                <td class="cmp-muted">—</td>
                <td class="cmp-muted">—</td>
                <td>{{ selectedRow.channel.View.Priority }}</td>
                <td class="cmp-muted">—</td>
              </tr>
              <tr>
                <td class="cmp-field">限流 RPM/TPM</td>
                <td class="cmp-muted">—</td>
                <td>{{ selectedRow.model ? rpmTpmText(selectedRow.model.RPM, selectedRow.model.RPMLimit, selectedRow.model.TPM, selectedRow.model.TPMLimit) : '—' }}</td>
                <td>{{ rpmTpmText(selectedRow.channel.RPM, selectedRow.channel.RPMLimit, selectedRow.channel.TPM, selectedRow.channel.TPMLimit) }}</td>
                <td>{{ selectedRow.keys.length ? rpmTpmText(keyRpmVal(selectedRow), keyRpmLimit(selectedRow), keyTpmVal(selectedRow), keyTpmLimit(selectedRow)) : '—' }}</td>
              </tr>
              <tr>
                <td class="cmp-field">会话数</td>
                <td class="cmp-muted">—</td>
                <td class="cmp-muted">—</td>
                <td class="cmp-muted">—</td>
                <td>{{ selectedRow.keys.length ? `${fmtInt(sessionsVal(selectedRow))}/${fmtInt(sessionsLimit(selectedRow))}` : '—' }}</td>
              </tr>
              <tr>
                <td class="cmp-field">可靠性阈值</td>
                <td class="cmp-muted">—</td>
                <td>{{ selectedRow.model ? relText(selectedRow.model.View.Reliability) : '—' }}</td>
                <td>{{ relText(selectedRow.channel.View.Reliability) }}</td>
                <td class="cmp-muted">—</td>
              </tr>
              <tr>
                <td class="cmp-field">窗口指标</td>
                <td class="cmp-muted">—</td>
                <td>{{ selectedRow.model ? metricsText(selectedRow.model.Metrics) : '—' }}</td>
                <td class="cmp-muted">—</td>
                <td class="cmp-muted">—</td>
              </tr>
              <tr>
                <td class="cmp-field">最近探测</td>
                <td class="cmp-muted">—</td>
                <td>{{ selectedRow.model ? dt(selectedRow.model.View.LastProbe) : '—' }}</td>
                <td>{{ dt(selectedRow.channel.View.LastProbe) }}</td>
                <td>{{ selectedRow.key ? dt(selectedRow.key.View.LastProbe) : '—' }}</td>
              </tr>
              <tr>
                <td class="cmp-field">最近错误</td>
                <td class="cmp-muted">—</td>
                <td>
                  <span v-if="selectedRow.model && selectedRow.model.View.LastErr" class="danger">{{ selectedRow.model.View.LastErr }}</span>
                  <span v-else class="cmp-muted">—</span>
                </td>
                <td>
                  <span v-if="selectedRow.channel.View.LastErr" class="danger">{{ selectedRow.channel.View.LastErr }}</span>
                  <span v-else class="cmp-muted">—</span>
                </td>
                <td>
                  <template v-if="selectedRow.key && (selectedRow.key.View.LastErr || (selectedRow.key.View.DrainReasons && selectedRow.key.View.DrainReasons.length))">
                    <div v-if="selectedRow.key.View.LastErr" class="danger">{{ selectedRow.key.View.LastErr }}</div>
                    <div v-if="selectedRow.key.View.DrainReasons && selectedRow.key.View.DrainReasons.length" class="danger">排空原因：{{ selectedRow.key.View.DrainReasons.join('；') }}</div>
                  </template>
                  <span v-else class="cmp-muted">—</span>
                </td>
              </tr>
              <tr>
                <td class="cmp-field">操作</td>
                <td class="cmp-muted">—</td>
                <td>
                  <div v-if="selectedRow.model" class="op-btns">
                    <el-button size="small" :loading="opLoading" :disabled="opLoading" @click="doProbeModel">重新探测</el-button>
                    <el-button size="small" type="success" :disabled="opLoading || selectedRow.model.View.State === 'NORMAL'" @click="doState('model', 'normal')">正常</el-button>
                    <el-button size="small" type="warning" :disabled="opLoading || selectedRow.model.View.State === 'DRAIN'" @click="doState('model', 'drain')">排空</el-button>
                    <el-button size="small" type="danger" :disabled="opLoading || selectedRow.model.View.State === 'DISABLED'" @click="doState('model', 'disable')">禁用</el-button>
                  </div>
                  <span v-else class="cmp-muted">—</span>
                </td>
                <td class="cmp-muted">—</td>
                <td>
                  <div v-if="selectedRow.key" class="op-btns">
                    <el-button size="small" :loading="opLoading" :disabled="opLoading" @click="doProbeKey">重新探测</el-button>
                    <el-button size="small" type="success" :disabled="opLoading || selectedRow.key.View.State === 'NORMAL'" @click="doState('key', 'normal')">正常</el-button>
                    <el-button size="small" type="warning" :disabled="opLoading || selectedRow.key.View.State === 'DRAIN'" @click="doState('key', 'drain')">排空</el-button>
                    <el-button size="small" type="danger" :disabled="opLoading || selectedRow.key.View.State === 'DISABLED'" @click="doState('key', 'disable')">禁用</el-button>
                  </div>
                  <span v-else class="cmp-muted">—</span>
                </td>
              </tr>
            </tbody>
          </table>
          <ErrorBubble v-if="opErr.message || opErr.requestId" :message="opErr.message" :request-id="opErr.requestId" />

          <!-- 候选序位：显示渠道名 + 密钥名（回退显示 ID） -->
          <div class="insp-sec">
            <div class="insp-sec-title">候选序位（{{ selectedRow.model ? (externalNameByID[selectedRow.model.View.ExternalModelID] || selectedRow.model.View.ExternalModelID) : '—' }}）</div>
            <div v-if="!selectedCandidates.length" class="insp-empty-inline">模型维度未纳入行轴，或该对外模型当前无可用候选（停用/未绑定可用渠道密钥）</div>
            <div v-else class="cand-list">
              <div v-for="c in selectedCandidates" :key="c.Rank" class="cand-row" :class="{ hit: isSelectedCandidate(c) }">
                <span class="cand-rank">#{{ c.Rank }}</span>
                <span class="cand-id">渠道 {{ channelLabel(c.ChannelID) }} · 密钥 {{ keyLabel(c.ChannelID, c.ChannelKeyID) }}</span>
                <span class="cand-cost">{{ c.CostKnown ? fmtNum(c.Cost) : '未知' }}<template v-if="c.TimeCoeff">（×{{ fmtNum(c.TimeCoeff) }}）</template></span>
              </div>
            </div>
            <div v-if="selectedCandidates.length" class="cand-sample">样值：{{ sampleUsageText }}</div>
          </div>
        </el-tab-pane>

        <!-- ===== 事件链 ===== -->
        <el-tab-pane label="事件链" name="event">
          <el-tabs v-model="activeTab">
            <el-tab-pane label="模型事件" name="model">
              <div v-loading="tabState.model.loading">
                <ErrorBubble v-if="tabState.model.error.message || tabState.model.error.requestId" :message="tabState.model.error.message" :request-id="tabState.model.error.requestId" />
                <div v-for="e in tabState.model.data" :key="e.ID" class="ev-row">
                  <span class="ev-model">#{{ e.ModelID }}</span>
                  <StatusTag :value="e.FromState" /><span class="ev-arrow">→</span><StatusTag :value="e.ToState" />
                  <span class="ev-reason">{{ e.Reason || '—' }}</span>
                  <span class="ev-time">{{ fmtDateTime(e.CreatedAt) }}</span>
                </div>
                <div v-if="!tabState.model.loading && !tabState.model.data.length" class="ev-empty">暂无事件</div>
              </div>
            </el-tab-pane>
            <el-tab-pane label="探测日志" name="probe">
              <div v-loading="tabState.probe.loading">
                <ErrorBubble v-if="tabState.probe.error.message || tabState.probe.error.requestId" :message="tabState.probe.error.message" :request-id="tabState.probe.error.requestId" />
                <div v-for="e in tabState.probe.data" :key="e.ID" class="ev-row">
                  <el-tag :type="e.OK ? 'success' : 'danger'" size="small" effect="light">{{ e.OK ? '成功' : '失败' }}</el-tag>
                  <span class="ev-reason">{{ e.Level }} / {{ e.Target }} · {{ e.DurationMS }}ms{{ e.Error ? ' · ' + e.Error : '' }}</span>
                  <span class="ev-time">{{ fmtDateTime(e.CreatedAt) }}</span>
                </div>
                <div v-if="!tabState.probe.loading && !tabState.probe.data.length" class="ev-empty">暂无探测日志</div>
              </div>
            </el-tab-pane>
            <el-tab-pane label="密钥事件" name="key">
              <div v-loading="tabState.key.loading">
                <ErrorBubble v-if="tabState.key.error.message || tabState.key.error.requestId" :message="tabState.key.error.message" :request-id="tabState.key.error.requestId" />
                <div v-for="e in tabState.key.data" :key="e.ID" class="ev-row">
                  <StatusTag :value="e.FromState" /><span class="ev-arrow">→</span><StatusTag :value="e.ToState" />
                  <span class="ev-reason">{{ e.Reason || '—' }}</span>
                  <span class="ev-time">{{ fmtDateTime(e.CreatedAt) }}</span>
                </div>
                <div v-if="!tabState.key.loading && !tabState.key.data.length" class="ev-empty">暂无事件</div>
              </div>
            </el-tab-pane>
          </el-tabs>
        </el-tab-pane>
      </el-tabs>
    </el-dialog>

    <!-- 页脚：聚合语义显式成文（用户规范：不允许隐性逻辑） -->
    <div class="foot-note">
      维度固定全量展示：行轴恒为「对外模型 × 内部模型 × 渠道 × 密钥」叉积；列固定为四维度列 + 优先级/限流/会话数。
    </div>
    <div class="foot-note">
      状态灯按「本行通路」着色，同一行的对外模型灯与内部模型灯口径一致；悬停状态灯可见全局可用通路 x/y 与具体异常渠道/模型。
    </div>
    <div class="foot-note">
      密钥单元格左侧色条为序位：金=首选 · 浅灰=次选 · 深灰=不参与新会话（悬停色条查看释义）；顺序由后端候选排序决定。
    </div>
    <div class="foot-note">
      渠道无独立状态机，故渠道列不展示状态灯；RPM/TPM 合并列上行为模型级、下行为密钥级，均展示「当前值/上限」并附用量灯（≥100% 红、≥80% 橙）。
    </div>
  </div>
</template>

<style scoped>
.toolbar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 20px;
  margin-bottom: 14px;
  padding: 12px 20px;
}
.tb-item {
  display: flex;
  align-items: center;
  gap: 8px;
}
.tb-label {
  font-size: 12px;
  color: var(--color-text-secondary);
  white-space: nowrap;
}
.tb-int {
  width: 96px;
}
.tb-time {
  margin-left: auto;
  font-size: 12px;
  color: var(--color-text-secondary);
  font-variant-numeric: tabular-nums;
}
.tb-hint {
  margin-left: 6px;
  color: var(--color-text-tertiary);
}
.matrix {
  width: 100%;
  min-width: 0;
}
/* 弹窗：顶部 tab 承载详情与事件链 */
.dlg-tabs :deep(.el-tabs__header) {
  margin-bottom: 12px;
}
/* 四列字段对比表 */
.cmp-table {
  width: 100%;
  table-layout: fixed;
  border-collapse: collapse;
  font-size: 12px;
}
.cmp-table th,
.cmp-table td {
  border: 1px solid var(--color-border);
  padding: 6px 8px;
  text-align: left;
  vertical-align: top;
  word-break: normal;
  overflow-wrap: break-word;
}
.cmp-table thead th {
  background: var(--el-fill-color-light);
  font-weight: 600;
  color: var(--color-text);
}
.cmp-table .cmp-field {
  width: 76px;
  background: var(--el-fill-color-light);
  color: var(--color-text-tertiary);
  font-weight: 400;
}
.cmp-muted {
  color: var(--color-text-tertiary);
}
.cmp-sub {
  margin-left: 6px;
  color: var(--color-text-tertiary);
}
/* 错误文本告警色（详情对比表「最近错误」等） */
.danger {
  color: var(--color-danger);
}
.insp-empty,
.insp-empty-inline {
  font-size: 12px;
  color: var(--color-text-tertiary);
  padding: 6px 0;
}
.insp-sec {
  padding: 10px 0;
  border-top: 1px solid var(--color-border);
  margin-top: 12px;
}
.insp-sec-title {
  font-size: 13px;
  font-weight: 600;
  color: var(--color-text);
  margin-bottom: 6px;
}
.hcell {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
}
.hcell-name {
  font-size: 12px;
  color: var(--color-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.hcell-sub {
  font-size: 11px;
  color: var(--color-text-tertiary);
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}
/* 状态灯：绿=正常 · 橙=排空 · 红=禁用/超限 · 灰=未知（与「会话」列表的光效圆点一致） */
.lamp {
  --lamp-ok: var(--color-success);
  --lamp-warn: var(--color-warning);
  --lamp-bad: var(--color-danger);
  --lamp-off: var(--color-text-tertiary);
  --lamp-ok-glow: rgba(18, 183, 106, 0.5);
  --lamp-warn-glow: rgba(247, 144, 9, 0.5);
  --lamp-bad-glow: rgba(240, 68, 56, 0.5);
  flex: none;
  width: 9px;
  height: 9px;
  border-radius: 50%;
  background: var(--lamp-off);
}
.lamp[data-state='ok'] {
  background: var(--lamp-ok);
  box-shadow: 0 0 6px var(--lamp-ok-glow);
}
.lamp[data-state='warn'] {
  background: var(--lamp-warn);
  box-shadow: 0 0 6px var(--lamp-warn-glow);
}
.lamp[data-state='bad'] {
  background: var(--lamp-bad);
  box-shadow: 0 0 6px var(--lamp-bad-glow);
}
.lamp[data-state='off'] {
  background: var(--lamp-off);
}
/* 指标单元格：用量灯 + 当前值/上限 */
.mcell {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  justify-content: center;
}
.mtext {
  font-variant-numeric: tabular-nums;
}
/* 合并限流列：模型级/密钥级两行 */
.lmcell {
  display: flex;
  flex-direction: column;
  gap: 2px;
  align-items: flex-start;
  line-height: 1.4;
}
.lmrow {
  display: flex;
  align-items: center;
  gap: 4px;
}
.lmtext {
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}
.lmk {
  display: inline-block;
  width: 30px;
  color: var(--color-text-tertiary);
}
/* 操作按钮：列内自动换行 */
.op-btns {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}
/* 序位色条：置于密钥单元格最左侧；金=首选 · 浅灰=次选 · 深灰=不参与新会话（悬停看释义） */
.rank-bar {
  flex: none;
  width: 4px;
  height: 18px;
  border-radius: 2px;
  background: currentColor;
}
.rank-bar.rank-first {
  color: #f0b429;
}
.rank-bar.rank-second {
  color: #b6bccd;
}
.rank-bar.rank-none {
  color: #98a2b3;
}
.tt {
  max-width: 320px;
  font-size: 12px;
  line-height: 1.7;
  white-space: normal;
}
.tt-line.danger {
  color: #ffbaba;
}
.cand-list {
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.cand-row {
  display: flex;
  gap: 8px;
  font-size: 12px;
  padding: 2px 4px;
  border-radius: 4px;
}
.cand-row.hit {
  background: var(--brand-primary-soft);
}
.cand-rank {
  flex: none;
  width: 28px;
  font-weight: 600;
}
.cand-id {
  min-width: 0;
  color: var(--color-text-secondary);
}
.cand-cost {
  margin-left: auto;
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
}
.cand-sample {
  margin-top: 6px;
  font-size: 11px;
  color: var(--color-text-tertiary);
  word-break: break-all;
}
.ev-row {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
  font-size: 12px;
  padding: 4px 0;
  border-top: 1px solid var(--color-border);
}
.ev-row:first-child {
  border-top: none;
}
.ev-arrow {
  color: var(--color-text-tertiary);
}
.ev-model {
  color: var(--color-text-secondary);
}
.ev-reason {
  color: var(--color-text);
  word-break: break-all;
}
.ev-time {
  margin-left: auto;
  color: var(--color-text-tertiary);
  font-variant-numeric: tabular-nums;
}
.ev-empty {
  font-size: 12px;
  color: var(--color-text-tertiary);
  padding: 6px 0;
}
.foot-note {
  margin-top: 8px;
  font-size: 12px;
  color: var(--color-text-tertiary);
  line-height: 1.7;
}
</style>

<!-- 弹窗被 teleport 到 body，scoped 样式无法作用于 .el-dialog__body，故单独用非 scoped 样式限制高度 -->
<style>
.status-dlg .el-dialog__body {
  max-height: 82vh;
  overflow: auto;
}
</style>
