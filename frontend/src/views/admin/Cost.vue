<script setup lang="ts">
// 「成本」只读页（仅管理端），三块能力：
//   A. 成本统计表：按「当前实体」（渠道 × 内部模型 × 对外模型）聚合，row-key = EntityKey；
//   B. 模型定价曲线：新接口 /admin/cost/pricing-curve，后端按真实公式逐 token 出点，前端只渲染
//      （成本线 = 渠道内部模型配置、售价线 = 对外模型配置；分档拐点画竖线、超档断线标红）；
//   C. 会话成本曲线：逐轮历史实际 cost_credits / credits_consumed（切换会话有请求序号守卫）。
// 纯只读：全部走 GET，无任何写路径。桌面用表格、窄屏用「数字墙 + 实体行」单列表。
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Refresh } from '@element-plus/icons-vue'
import * as echarts from 'echarts/core'
import type { EChartsCoreOption } from 'echarts/core'
import { MarkAreaComponent, MarkLineComponent, MarkPointComponent } from 'echarts/components'
import VChart from '@/components/VChart.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'
import {
  getCostStats,
  getCostPricingCurve,
  getCostSession,
  listSessions,
  type AdminSession,
  type CostPricingCurveData,
  type CostPricingCurveParams,
  type CostPricingLine,
  type CostStatRow,
  type CostStatTotal,
  type SessionPoint,
} from '@/api/admin'

// 拐点竖线（markLine）、超档红标（markPoint）、超档区间底色（markArea）按需注册。
echarts.use([MarkLineComponent, MarkPointComponent, MarkAreaComponent])

// ===== 通用格式化 =====
const fmt2 = (v: number) => (Number.isFinite(v) ? v.toFixed(2) : '-')
const fmtPct = (v: number) => (Number.isFinite(v) ? (v * 100).toFixed(2) + '%' : '-')
const fmtInt = (v: number) => (Number.isFinite(v) ? v.toLocaleString('zh-CN') : '-')
const round4 = (v: number) => Math.round(v * 10000) / 10000

// 窄屏（移动版）判定：图表高度、布局切换用。
const NARROW_QUERY = '(max-width: 768px)'
const isNarrow = ref(false)
let mq: MediaQueryList | undefined
function onMqChange(e: MediaQueryListEvent | MediaQueryList) {
  isNarrow.value = e.matches
}

// ===== 全局错误 =====
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
function setErr(e: any) {
  errInfo.value = { message: e?.message || '', requestId: e?.requestId || '' }
}

// ===================================================================
// A. 成本统计表（按实体归一）
// ===================================================================
const loadingStats = ref(false)
const statsRows = ref<CostStatRow[]>([])
const statsTotal = ref<CostStatTotal | null>(null)

async function loadStats() {
  loadingStats.value = true
  try {
    const res = await getCostStats()
    statsRows.value = res.Data.Rows ?? []
    statsTotal.value = res.Data.Total ?? null
  } catch (e: any) {
    setErr(e)
  } finally {
    loadingStats.value = false
  }
}

// 曾用名：SnapshotNames 去掉当前展示名后剩下的历史文本（长度 >1 才标注）。
function oldNames(row: CostStatRow): string[] {
  const all = row.SnapshotNames ?? []
  return all.filter((n) => n && n !== row.ExternalModelName)
}
function oldNamesTitle(row: CostStatRow): string {
  const list = oldNames(row)
  return list.length ? `曾用名：${list.join(' / ')}` : ''
}

// 表格单元格格式化（保留原始数值用于排序）
const cellCost = (_r: CostStatRow, _c: unknown, v: number) => fmt2(v)
const cellInt = (_r: CostStatRow, _c: unknown, v: number) => fmtInt(v)
const cellHit = (_r: CostStatRow, _c: unknown, v: number) => fmtPct(v)
const cellRatio = (_r: CostStatRow, _c: unknown, v: number) => fmt2(v)

// 汇总行取后端 Total（全量口径，不随排序变化）。
function sumSummary({ columns }: { columns: { property?: string }[] }): string[] {
  const t = statsTotal.value
  return columns.map((col, i) => {
    if (i === 0) return '合计'
    if (!t) return ''
    switch (col.property) {
      case 'CostCredits':
        return fmt2(t.CostCredits)
      case 'SaleCredits':
        return fmt2(t.SaleCredits)
      case 'CacheHitRate':
        return fmtPct(t.CacheHitRate)
      case 'InputOutputRatio':
        return fmt2(t.InputOutputRatio)
      case 'Requests':
        return fmtInt(t.Requests)
      case 'InputTokens':
        return fmtInt(t.InputTokens)
      case 'CacheReadTokens':
        return fmtInt(t.CacheReadTokens)
      case 'OutputTokens':
        return fmtInt(t.OutputTokens)
      default:
        return ''
    }
  })
}

const rowKey = (row: CostStatRow) => row.EntityKey

// ===================================================================
// B. 模型定价曲线（/admin/cost/pricing-curve）
// ===================================================================
const loadingCurve = ref(false)
const curve = ref<CostPricingCurveData | null>(null)
const curveEntity = ref('') // 选中的 EntityKey（模型来自统计表的实体行）
// 采样参数：首次响应回填后端生效值，之后用户改动才带参重取。
const curveParams = reactive({ step: 0, maxTokens: 0, hitRate: 0.9, inOutRatio: 10 })
const paramsInited = ref(false)
// 命中率按百分比展示（0~100），内部仍存 0~1。
const hitRatePct = computed<number>({
  get: () => round2(curveParams.hitRate * 100),
  set: (v: number) => {
    curveParams.hitRate = (Number.isFinite(v) ? v : 0) / 100
  },
})
function round2(v: number) {
  return Math.round(v * 100) / 100
}
const useCoeff = ref(true) // 开 = Credits（含时段×分档系数）；关 = CreditsNoCoeff
const curveMode = ref<'both' | 'cost' | 'sale'>('both')
const timeOptionKey = ref('') // 选中的时段（Name|Start|End）
const timeTouched = ref(false) // 用户是否手动选过时段

const curveEntities = computed<CostStatRow[]>(() => statsRows.value)
const currentEntity = computed(() => curveEntities.value.find((r) => r.EntityKey === curveEntity.value) || null)

const timeOptions = computed(() => curve.value?.TimeOptions ?? [])
function timeOptionLabel(t: { Name: string; Start: string; End: string; Coeff: number }) {
  const span = t.Start && t.End ? `${t.Start}–${t.End}` : ''
  return `${t.Name}${span ? ' ' + span : ''} ×${t.Coeff}`
}
function timeOptionValue(t: { Name: string; Start: string; End: string }) {
  return `${t.Name}|${t.Start}|${t.End}`
}
const currentTimeOption = computed(() =>
  timeOptions.value.find((t) => timeOptionValue(t) === timeOptionKey.value) || null,
)

// 曲线请求序号：参数/模型快速切换时丢弃过期响应。
let curveSeq = 0

async function loadCurve() {
  const seq = ++curveSeq
  loadingCurve.value = true
  try {
    const row = currentEntity.value
    const p: CostPricingCurveParams = {}
    if (row?.ChannelModelID) p.ChannelModelID = row.ChannelModelID
    if (row?.ExternalModelID) p.ExternalModelID = row.ExternalModelID
    else if (row?.ExternalModelName) p.ExternalModelName = row.ExternalModelName
    if (paramsInited.value) {
      if (curveParams.step > 0) p.Step = curveParams.step
      if (curveParams.maxTokens > 0) p.MaxTokens = curveParams.maxTokens
      p.HitRate = curveParams.hitRate
      p.InOutRatio = curveParams.inOutRatio
    }
    // 时段：用所选时段的起点作为 At，让后端按该时段算 timeCoeff。
    if (timeTouched.value && currentTimeOption.value?.Start) p.At = currentTimeOption.value.Start

    const res = await getCostPricingCurve(p)
    if (seq !== curveSeq) return
    curve.value = res.Data
    if (!paramsInited.value) {
      curveParams.step = res.Data.Step
      curveParams.maxTokens = res.Data.MaxTokens
      curveParams.hitRate = res.Data.HitRate
      curveParams.inOutRatio = res.Data.InOutRatio
      paramsInited.value = true
    }
    // 时段下拉默认高亮：按后端实际使用的 At 落在哪个时段。
    if (!timeTouched.value) {
      const hhmm = atHHMM(res.Data.At)
      const hit = timeOptions.value.find((t) => inTimeRange(hhmm, t.Start, t.End))
      timeOptionKey.value = hit ? timeOptionValue(hit) : (timeOptions.value[0] ? timeOptionValue(timeOptions.value[0]) : '')
    }
  } catch (e: any) {
    if (seq !== curveSeq) return
    setErr(e)
  } finally {
    if (seq === curveSeq) loadingCurve.value = false
  }
}

// 从后端回显的 RFC3339 里取「北京时间 HH:MM」。
// 直接切字符串而不用 Date.getHours()：浏览器的本地时区未必是 +08:00，转换会错位。
function atHHMM(iso: string): string {
  const m = /\d{2}:\d{2}(?=:\d{2}|[+Z])/.exec(iso || '')
  if (m) return m[0]
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return ''
  return `${String(d.getHours()).padStart(2, '0')}:${String(d.getMinutes()).padStart(2, '0')}`
}
function inTimeRange(hhmm: string, start: string, end: string): boolean {
  if (!hhmm) return false
  const toMin = (s: string) => {
    const [h, m] = (s || '').split(':').map(Number)
    return (h || 0) * 60 + (m || 0)
  }
  const cur = toMin(hhmm)
  return cur >= toMin(start) && cur < (toMin(end) || 24 * 60)
}

function onTimeChange() {
  timeTouched.value = true
  loadCurve()
}
function onParamChange() {
  loadCurve()
}

// 取数序列：同一批 Points，只按「系数开关」切字段（不自己乘）。
function pointValue(p: { Credits: number; CreditsNoCoeff: number }): number {
  return useCoeff.value ? p.Credits : p.CreditsNoCoeff
}

interface CurveSeries {
  key: 'cost' | 'sale'
  name: string
  color: string
  dashed: boolean
  line: CostPricingLine
}

const availableLines = computed<CurveSeries[]>(() => {
  const c = curve.value
  if (!c) return []
  const out: CurveSeries[] = []
  if (c.Cost?.Available) out.push({ key: 'cost', name: '成本', color: '#5a5ce8', dashed: false, line: c.Cost })
  if (c.Sale?.Available) out.push({ key: 'sale', name: '售价', color: '#e6a23c', dashed: true, line: c.Sale })
  return out
})

const shownLines = computed<CurveSeries[]>(() =>
  availableLines.value.filter((l) =>
    curveMode.value === 'both' ? true : curveMode.value === 'cost' ? l.key === 'cost' : l.key === 'sale',
  ),
)

// 不可用线的原因（Available=false 时按 Reason 明确提示，不画空线、不把 0 当价格）
const unavailableHints = computed(() => {
  const c = curve.value
  if (!c) return []
  const out: { name: string; reason: string }[] = []
  if (!c.Cost?.Available) out.push({ name: '成本线', reason: c.Cost?.Reason || '不可用' })
  if (!c.Sale?.Available) out.push({ name: '售价线', reason: c.Sale?.Reason || '不可用' })
  return out
})

const curveOption = computed<EChartsCoreOption>(() => {
  const lines = shownLines.value
  const series: Record<string, unknown>[] = []
  let markLineDone = false
  let markAreaDone = false

  for (const s of lines) {
    const points = s.line.Points ?? []
    // 超档点：真实计费会失败 → 断线（y = null），并在下方补红标
    const data = points.map((p) => [p.Tokens, p.OverTier ? null : pointValue(p)])
    const item: Record<string, unknown> = {
      name: s.name,
      type: 'line',
      smooth: false,
      showSymbol: false,
      connectNulls: false, // 超档 / 缺数据断线，不补 0
      lineStyle: { width: s.key === 'cost' ? 2.6 : 2, type: s.dashed ? 'dashed' : 'solid', color: s.color },
      itemStyle: { color: s.color },
      data,
    }
    // 拐点竖线只挂一条序列，避免重复绘制
    if (!markLineDone && (s.line.TierBreaks?.length ?? 0) > 0) {
      markLineDone = true
      item.markLine = {
        silent: true,
        symbol: 'none',
        lineStyle: { color: '#f79009', type: 'dashed', width: 1.2 },
        label: { formatter: (p: any) => `×${p?.data?.toCoeff ?? ''}`, color: '#f79009', fontSize: 10 },
        data: (s.line.TierBreaks ?? []).map((b) => ({
          xAxis: b.Tokens,
          toCoeff: b.ToCoeff,
        })),
      }
    }
    // 超档区间底色
    if (!markAreaDone && (s.line.OverTier?.length ?? 0) > 0) {
      markAreaDone = true
      item.markArea = {
        silent: true,
        itemStyle: { color: 'rgba(240,68,56,0.08)' },
        data: (s.line.OverTier ?? []).map((r) => [{ xAxis: r.FromTokens }, { xAxis: r.ToTokens }]),
      }
    }
    series.push(item)
  }

  // 超档红点（第一条可用线的超档区间起点）
  const overPts: [number, number][] = []
  for (const s of lines) {
    for (const r of s.line.OverTier ?? []) overPts.push([r.FromTokens, 0])
  }
  if (lines.length) {
    series.push({
      name: '超档（计费失败）',
      type: 'scatter',
      symbolSize: 8,
      itemStyle: { color: '#f04438' },
      data: overPts,
      z: 10,
    })
  }

  return {
    tooltip: {
      trigger: 'axis',
      valueFormatter: (v: unknown) => (typeof v === 'number' ? String(round4(v)) : String(v)),
    },
    legend: { show: false }, // 图例用页面下方 HTML 版（移动版也能读）
    grid: { left: 12, right: 20, top: 28, bottom: 44, containLabel: true },
    xAxis: {
      type: 'value',
      name: '上下文 token',
      nameLocation: 'middle',
      nameGap: 26,
      axisLabel: { formatter: (v: number) => (v >= 1000 ? `${Math.round(v / 1000)}K` : String(v)) },
      splitLine: { lineStyle: { color: '#eef0f6' } },
    },
    yAxis: {
      type: 'value',
      name: '积分',
      axisLabel: { formatter: (v: number) => String(round4(v)) },
      splitLine: { lineStyle: { color: '#eef0f6' } },
    },
    series,
  }
})

const curveMeta = computed(() => {
  const c = curve.value
  if (!c) return ''
  const parts = [
    `R=${c.R}`,
    `采样 ${c.PointCount} 点（步进 ${fmtInt(c.Step)} / 上限 ${fmtInt(c.MaxTokens)}）`,
    `命中率 ${fmtPct(c.HitRate)} · 输入输出比 ${fmt2(c.InOutRatio)}`,
  ]
  const cost = c.Cost
  if (cost?.Available) {
    parts.push(`成本时段系数 ×${fmt2(cost.TimeCoeff)}（${cost.TimeConfigSource === 'model' ? '模型级' : '全局'}）`)
    parts.push(`分档来源 ${cost.TierSource === 'none' ? '未配分档' : cost.TierSource === 'model' ? '模型级' : '全局'}`)
  }
  return parts.join(' · ')
})

// 模型下拉：优先用统计表实体（ChannelModelID 能直接喂给曲线接口）
function entityLabel(r: CostStatRow) {
  const parts = [r.ChannelName || '(无渠道)', r.InternalModelID, r.ExternalModelName]
  return parts.filter(Boolean).join(' / ')
}
function onEntityChange() {
  paramsInited.value = false // 换模型后重新吃后端给出的默认采样参数
  timeTouched.value = false
  loadCurve()
}

// ===================================================================
// C. 会话成本曲线
// ===================================================================
const loadingSessions = ref(false)
const sessions = ref<AdminSession[]>([])
const sessionId = ref('')
const loadingSession = ref(false)
const sessionPoints = ref<SessionPoint[]>([])
const sessionTotals = ref<{ CostCredits: number; SaleCredits: number } | null>(null)

// 会话请求序号守卫：切换会话立即清空旧数据，并丢弃过期响应（防止旧响应后到覆盖新会话）。
let sessionSeq = 0

async function loadSessions() {
  loadingSessions.value = true
  try {
    const res = await listSessions({ Page: 1, Size: 200 })
    sessions.value = res.Data.List ?? []
    if (!sessionId.value && sessions.value.length) {
      sessionId.value = sessions.value[0].SessionID
      await loadSession()
    }
  } catch (e: any) {
    setErr(e)
  } finally {
    loadingSessions.value = false
  }
}

// 下拉标签：名字 · 用户 · 模型 · 时间 · N 次调用（长文本由 el-select 截断，title 给全文）
function sessionLabel(s: AdminSession): string {
  const t = (s.LastActive || s.CreatedAt || '').slice(5, 16).replace('T', ' ')
  const name = s.SessionName || s.SessionID.slice(0, 8)
  const user = s.UserNickname || s.UserName || ''
  return [name, user, s.Model, t, `${s.Calls ?? 0} 次调用`].filter(Boolean).join(' · ')
}

// 切换会话：先清空（不给用户看到上一个会话的残留数据），再带序号取新数据。
function onSessionChange() {
  sessionPoints.value = []
  sessionTotals.value = null
  loadSession()
}

async function loadSession() {
  const seq = ++sessionSeq
  if (!sessionId.value) return
  loadingSession.value = true
  try {
    const res = await getCostSession(sessionId.value)
    if (seq !== sessionSeq) return // 已切到别的会话，丢弃过期响应
    sessionPoints.value = res.Data.Points ?? []
    sessionTotals.value = res.Data.Totals ?? null
  } catch (e: any) {
    if (seq !== sessionSeq) return
    setErr(e)
  } finally {
    if (seq === sessionSeq) loadingSession.value = false
  }
}

// 失败轮判定：后端当前只返回 completed 行（无 Status/Failed 字段），
// 字段就绪后这里自动生效——失败轮画红点、金额按 0 计，但保留 x 轴位置不跳过。
function isFailedPoint(p: SessionPoint): boolean {
  return p.Failed === true || (p.Status || '').toLowerCase() === 'failed'
}

const sessionFailedPoints = computed<[number, number][]>(() =>
  sessionPoints.value.filter(isFailedPoint).map((p) => [p.Turn, 0]),
)

const sessionOption = computed<EChartsCoreOption>(() => {
  const pts = sessionPoints.value
  return {
    tooltip: {
      trigger: 'axis',
      valueFormatter: (v: unknown) => (typeof v === 'number' ? String(round4(v)) : String(v)),
    },
    legend: { show: false },
    grid: { left: 12, right: 20, top: 24, bottom: 44, containLabel: true },
    xAxis: {
      type: 'value',
      name: '轮次',
      nameLocation: 'middle',
      nameGap: 26,
      min: 1,
      minInterval: 1,
      splitLine: { lineStyle: { color: '#eef0f6' } },
    },
    yAxis: {
      type: 'value',
      name: '积分',
      axisLabel: { formatter: (v: number) => String(round4(v)) },
      splitLine: { lineStyle: { color: '#eef0f6' } },
    },
    series: [
      {
        name: '成本',
        type: 'line',
        showSymbol: true,
        symbolSize: 5,
        lineStyle: { width: 2.4, color: '#5a5ce8' },
        itemStyle: { color: '#5a5ce8' },
        // 失败轮金额按 0 计，但保留该点（不跳过），x 轴不错位
        data: pts.map((p) => [p.Turn, isFailedPoint(p) ? 0 : p.CostCredits]),
      },
      {
        name: '售价',
        type: 'line',
        showSymbol: true,
        symbolSize: 5,
        lineStyle: { width: 2, type: 'dashed', color: '#e6a23c' },
        itemStyle: { color: '#e6a23c' },
        data: pts.map((p) => [p.Turn, isFailedPoint(p) ? 0 : p.SaleCredits]),
      },
      {
        name: '失败轮',
        type: 'scatter',
        symbolSize: 9,
        itemStyle: { color: '#f04438' },
        data: sessionFailedPoints.value,
        z: 10,
      },
    ],
  }
})

// ===================================================================
// 加载编排
// ===================================================================
const loading = computed(() => loadingStats.value || loadingCurve.value || loadingSession.value)

async function loadAll() {
  errInfo.value = { message: '', requestId: '' }
  await loadStats()
  if (!curveEntity.value && curveEntities.value.length) {
    const prefer = curveEntities.value.find((r) => r.ChannelModelID > 0) || curveEntities.value[0]
    curveEntity.value = prefer.EntityKey
  }
  await Promise.all([loadCurve(), loadSessions()])
}

async function onRefresh() {
  paramsInited.value = false
  await loadAll()
  if (!errInfo.value.message) ElMessage.success('已刷新')
}

onMounted(() => {
  mq = window.matchMedia(NARROW_QUERY)
  onMqChange(mq)
  mq.addEventListener('change', onMqChange)
  loadAll()
})
onUnmounted(() => {
  mq?.removeEventListener('change', onMqChange)
})
</script>

<template>
  <div class="page-container cost-page">
    <div class="toolbar">
      <span class="tb-hint">全部历史口径（仅积分，不做汇率折算）；本页纯只读。</span>
      <div class="toolbar-spacer" />
      <el-button :icon="Refresh" :loading="loading" @click="onRefresh">刷新</el-button>
    </div>

    <div class="panel-stack">
      <!-- A. 成本统计（按当前实体聚合） -->
      <div class="card">
        <div class="block-title">
          成本统计
          <el-tag size="small" effect="plain">按当前实体聚合</el-tag>
          <el-tag size="small" effect="plain" type="info">曾用名标注</el-tag>
        </div>
        <div class="block-desc">
          按「渠道 × 内部模型 × 对外模型」<b>当前实体</b>聚合（仅 completed 消耗）：改名/换绑自动归一到当前实体，
          历史文本快照以「曾用名」标注；健康探测记账已从统计中移除（口径已确认，售价数字下降属预期修正）。
          表尾为全量合计（不随排序变化）。
        </div>

        <!-- 桌面：表格 -->
        <div class="desktop-only">
          <el-table
            v-loading="loadingStats"
            :data="statsRows"
            :row-key="rowKey"
            border
            stripe
            show-summary
            :summary-method="sumSummary"
            class="table-nowrap"
          >
            <el-table-column prop="ChannelName" label="渠道" min-width="140" sortable />
            <el-table-column prop="InternalModelID" label="内部模型" min-width="160" sortable show-overflow-tooltip />
            <el-table-column prop="ExternalModelName" label="对外模型" min-width="170" sortable show-overflow-tooltip>
              <template #default="{ row }">
                <span>{{ row.ExternalModelName }}</span>
                <el-tooltip v-if="oldNames(row).length" :content="oldNamesTitle(row)" placement="top" :show-after="150">
                  <span class="oldname">曾用名 {{ oldNames(row).length }}</span>
                </el-tooltip>
              </template>
            </el-table-column>
            <el-table-column prop="CostCredits" label="成本(积分)" min-width="118" align="right" sortable :formatter="cellCost" />
            <el-table-column prop="SaleCredits" label="售价(积分)" min-width="118" align="right" sortable :formatter="cellCost" />
            <el-table-column prop="CacheHitRate" label="缓存命中率" min-width="120" align="right" sortable :formatter="cellHit" />
            <el-table-column prop="InputOutputRatio" label="输入输出比" min-width="110" align="right" sortable :formatter="cellRatio" />
            <el-table-column prop="Requests" label="请求数" min-width="96" align="right" sortable :formatter="cellInt" />
            <el-table-column prop="InputTokens" label="输入token" min-width="110" align="right" sortable :formatter="cellInt" />
            <el-table-column prop="CacheReadTokens" label="缓存读token" min-width="126" align="right" sortable :formatter="cellInt" />
            <el-table-column prop="OutputTokens" label="输出token" min-width="110" align="right" sortable :formatter="cellInt" />
          </el-table>
          <el-empty v-if="!loadingStats && !statsRows.length" description="暂无成本数据" />
        </div>

        <!-- 窄屏：数字墙 + 实体行（单列、不横向滚动） -->
        <div class="mobile-only">
          <div class="sum-wall">
            <div class="sum-item">
              <b>{{ statsTotal ? fmt2(statsTotal.CostCredits) : '-' }}</b>
              <span>总成本</span>
            </div>
            <div class="sum-item sale">
              <b>{{ statsTotal ? fmt2(statsTotal.SaleCredits) : '-' }}</b>
              <span>总售价</span>
            </div>
            <div class="sum-item">
              <b>{{ statsTotal ? fmtInt(statsTotal.Requests) : '-' }}</b>
              <span>请求</span>
            </div>
          </div>
          <div v-for="r in statsRows" :key="r.EntityKey" class="entity-row">
            <div class="entity-name">
              <div class="n1">{{ r.ExternalModelName || r.InternalModelID || '(未知模型)' }}</div>
              <div class="n2">
                {{ r.ChannelName || '(无渠道)' }} · {{ r.InternalModelID }}
                <span v-if="oldNames(r).length" class="oldname">曾用名 {{ oldNames(r).length }}</span>
              </div>
            </div>
            <div class="entity-nums">
              <div class="n-price">{{ fmt2(r.SaleCredits) }}</div>
              <div class="n-sub">售价 · 成本 {{ fmt2(r.CostCredits) }}</div>
              <div class="n-sub">请求 {{ fmtInt(r.Requests) }} · 命中 {{ fmtPct(r.CacheHitRate) }}</div>
            </div>
          </div>
          <el-empty v-if="!loadingStats && !statsRows.length" description="暂无成本数据" :image-size="60" />
        </div>
      </div>

      <!-- B. 模型定价曲线 -->
      <div class="card">
        <div class="block-title">
          模型定价曲线
          <el-tag size="small" effect="plain">含分档 × 分时段</el-tag>
        </div>
        <div class="block-desc">
          每轮成本 =（输入×输入价 + 缓存读×缓存读价 + 输出×输出价）× 时段系数 × 分档系数 ÷ R；
          成本线取渠道内部模型配置、售价线取对外模型配置。跨档画拐点竖线，超出分档上限的区段断线并标红
          （真实请求会失败，不再画更贵的线）。
        </div>

        <div class="curve-toolbar">
          <span class="p-label">内部模型</span>
          <el-select
            v-model="curveEntity"
            filterable
            placeholder="选择模型（来自成本统计实体）"
            class="curve-select"
            @change="onEntityChange"
          >
            <el-option v-for="r in curveEntities" :key="r.EntityKey" :label="entityLabel(r)" :value="r.EntityKey">
              <span class="opt-main">{{ entityLabel(r) }}</span>
              <span class="opt-sub">{{ fmt2(r.CostCredits) }} / {{ fmt2(r.SaleCredits) }}</span>
            </el-option>
          </el-select>

          <span class="p-label">步进</span>
          <el-input-number
            v-model="curveParams.step"
            :min="1"
            :step="500"
            :controls="!isNarrow"
            controls-position="right"
            class="curve-num"
            @change="onParamChange"
          />
          <span class="p-label">上限 token</span>
          <el-input-number
            v-model="curveParams.maxTokens"
            :min="0"
            :step="10000"
            :controls="!isNarrow"
            controls-position="right"
            class="curve-num"
            @change="onParamChange"
          />
          <span class="p-label">缓存命中率 %</span>
          <el-input-number
            v-model="hitRatePct"
            :min="0"
            :max="100"
            :step="5"
            :controls="!isNarrow"
            controls-position="right"
            class="curve-num"
            @change="onParamChange"
          />
          <span class="p-label">输入输出比</span>
          <el-input-number
            v-model="curveParams.inOutRatio"
            :min="0"
            :step="1"
            :controls="!isNarrow"
            controls-position="right"
            class="curve-num"
            @change="onParamChange"
          />
          <span class="p-label">时段</span>
          <el-select v-model="timeOptionKey" placeholder="时段" class="curve-time" @change="onTimeChange">
            <el-option v-for="t in timeOptions" :key="timeOptionValue(t)" :label="timeOptionLabel(t)" :value="timeOptionValue(t)" />
          </el-select>
          <span class="switch-wrap">
            <el-switch v-model="useCoeff" size="small" />
            <span class="p-label">{{ useCoeff ? '含系数' : '不含系数' }}</span>
          </span>
          <el-radio-group v-model="curveMode" class="curve-mode">
            <el-radio-button value="both">两条</el-radio-button>
            <el-radio-button value="cost">成本</el-radio-button>
            <el-radio-button value="sale">售价</el-radio-button>
          </el-radio-group>
        </div>

        <div v-loading="loadingCurve">
          <VChart
            v-if="shownLines.length"
            :option="curveOption"
            :height="isNarrow ? '240px' : '360px'"
          />
          <div v-else class="empty-hint">
            {{ curve ? '所选模型没有可用曲线（见下方原因）。' : '暂无定价曲线数据。' }}
          </div>
          <div v-if="unavailableHints.length" class="unavailable">
            <el-tag v-for="h in unavailableHints" :key="h.name" size="small" type="danger" effect="plain">
              {{ h.name }}不可用：{{ h.reason }}
            </el-tag>
          </div>
        </div>

        <div class="legend">
          <span v-for="l in shownLines" :key="l.key">
            <i
              class="line-mark"
              :class="{ dashed: l.dashed }"
              :style="l.dashed ? { borderColor: l.color } : { background: l.color }"
            />{{ l.name }}
          </span>
          <span><i class="dot-mark tier" />升档拐点</span>
          <span><i class="dot-mark over" />超档（计费失败）</span>
        </div>
        <div class="hint">{{ curveMeta }}</div>
        <div class="hint">
          时段系数按所选时段整笔计（跨时段不切分）；系数开关只切后端给的两套序列（含 / 不含系数），前端不做乘法。
        </div>
      </div>

      <!-- C. 会话成本曲线 -->
      <div class="card">
        <div class="block-title">
          会话成本曲线
          <el-tag size="small" effect="plain" type="info">切换防竞态</el-tag>
        </div>
        <div class="block-desc">
          取该会话逐轮历史实际 cost_credits / credits_consumed（含当时时段/分档系数）。切换会话时立即清空旧数据并丢弃过期响应。
        </div>
        <div class="curve-toolbar">
          <span class="p-label">会话</span>
          <el-select
            v-model="sessionId"
            filterable
            :loading="loadingSessions"
            placeholder="选择会话"
            class="session-select"
            @change="onSessionChange"
          >
            <el-option v-for="s in sessions" :key="s.SessionID" :label="sessionLabel(s)" :value="s.SessionID">
              <span class="opt-main" :title="sessionLabel(s)">{{ sessionLabel(s) }}</span>
            </el-option>
          </el-select>
          <template v-if="sessionTotals">
            <el-tag size="small" effect="plain" type="info">成本 {{ fmt2(sessionTotals.CostCredits) }}</el-tag>
            <el-tag size="small" effect="plain" type="warning">售价 {{ fmt2(sessionTotals.SaleCredits) }}</el-tag>
            <span class="page-count">{{ sessionPoints.length }} 轮</span>
          </template>
        </div>
        <div v-loading="loadingSession">
          <VChart
            v-if="sessionPoints.length"
            :option="sessionOption"
            :height="isNarrow ? '220px' : '320px'"
          />
          <div v-else class="empty-hint">
            {{ sessionId ? '该会话暂无逐轮成本数据。' : '请选择一个会话以查看逐轮成本/售价曲线。' }}
          </div>
        </div>
        <div class="legend">
          <span><i class="line-mark" style="background: #5a5ce8" />成本</span>
          <span><i class="line-mark dashed" style="border-color: #e6a23c" />售价</span>
          <span><i class="dot-mark over" />失败轮（计 0）</span>
        </div>
        <div class="hint">
          失败轮在图上打红点并计 0 积分（保留该轮位置，不让 x 轴错位）；当前后端只返回已完成轮，失败轮标注待接口补齐字段后自动生效。
        </div>
      </div>

      <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
    </div>
  </div>
</template>

<style scoped>
.cost-page {
  display: flex;
  flex-direction: column;
}
.panel-stack {
  display: flex;
  flex-direction: column;
  gap: 16px;
}
.block-desc {
  color: var(--color-text-secondary);
  font-size: 13px;
  margin-bottom: 14px;
  line-height: 1.6;
}
.tb-hint {
  color: var(--color-text-tertiary);
  font-size: 12px;
}
.p-label {
  color: var(--color-text-secondary);
  font-size: 13px;
  white-space: nowrap;
}
.curve-toolbar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 12px 14px;
  margin-bottom: 14px;
}
.curve-select {
  min-width: 280px;
  max-width: 460px;
}
.curve-time {
  min-width: 200px;
}
.curve-num {
  width: 132px;
}
.switch-wrap {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.curve-mode {
  margin-left: 4px;
}
.session-select {
  width: 420px;
  max-width: 100%;
}
.opt-main {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.opt-sub {
  float: right;
  color: var(--color-text-tertiary);
  font-size: 11px;
  margin-left: 12px;
}
.oldname {
  margin-left: 6px;
  font-size: 11px;
  color: var(--color-text-tertiary);
  cursor: help;
}
.unavailable {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  margin-top: 10px;
}
.legend {
  display: flex;
  flex-wrap: wrap;
  gap: 14px;
  margin-top: 10px;
  font-size: 12px;
  color: var(--color-text-secondary);
}
.line-mark {
  display: inline-block;
  width: 14px;
  height: 3px;
  border-radius: 2px;
  margin-right: 5px;
  vertical-align: 2px;
}
.line-mark.dashed {
  height: 0;
  border-top: 2px dashed currentColor;
  background: transparent;
  border-radius: 0;
}
.dot-mark {
  display: inline-block;
  width: 9px;
  height: 9px;
  border-radius: 2px;
  margin-right: 5px;
  vertical-align: 0;
}
.dot-mark.tier {
  background: #f79009;
}
.dot-mark.over {
  background: #f04438;
  border-radius: 50%;
}
.hint {
  margin-top: 6px;
  font-size: 11px;
  line-height: 1.6;
  color: var(--color-text-tertiary);
}
.empty-hint {
  font-size: 13px;
  color: var(--color-text-tertiary);
  padding: 10px 4px;
}

/* ===== 移动版：数字墙 + 实体行 ===== */
.desktop-only {
  display: block;
}
.mobile-only {
  display: none;
}
.sum-wall {
  display: grid;
  grid-template-columns: repeat(3, minmax(0, 1fr));
  gap: 10px;
  margin-bottom: 12px;
}
.sum-item {
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: 10px;
  padding: 10px 12px;
  display: flex;
  flex-direction: column;
  gap: 2px;
}
.sum-item b {
  font-size: 18px;
  font-weight: 800;
  font-variant-numeric: tabular-nums;
  font-family: 'SF Mono', Consolas, monospace;
  color: var(--color-text);
}
.sum-item span {
  font-size: 12px;
  color: var(--color-text-secondary);
}
.sum-item.sale b {
  color: #b45309;
}
.entity-row {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 10px;
  padding: 10px 0;
  border-bottom: 1px dashed var(--color-border);
}
.entity-row:last-of-type {
  border-bottom: none;
}
.entity-name {
  min-width: 0;
  flex: 1;
}
.entity-name .n1 {
  font-size: 14px;
  font-weight: 700;
  color: var(--color-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.entity-name .n2 {
  font-size: 12px;
  color: var(--color-text-tertiary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.entity-nums {
  text-align: right;
  flex-shrink: 0;
}
.entity-nums .n-price {
  font-size: 15px;
  font-weight: 700;
  font-variant-numeric: tabular-nums;
  font-family: 'SF Mono', Consolas, monospace;
  color: var(--color-text);
}
.entity-nums .n-sub {
  font-size: 11px;
  color: var(--color-text-tertiary);
}

@media (max-width: 768px) {
  .desktop-only {
    display: none;
  }
  .mobile-only {
    display: block;
  }
  .block-desc {
    font-size: 14px;
  }
  .curve-toolbar {
    gap: 8px;
    align-items: stretch;
  }
  .curve-select,
  .curve-time,
  .session-select,
  .curve-num {
    width: 100%;
    min-width: 0;
    max-width: none;
  }
  .p-label {
    font-size: 13px;
  }
}
</style>
