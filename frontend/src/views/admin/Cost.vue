<script setup lang="ts">
// 「成本」只读页（仅管理端）：三块能力——
//   A. 渠道 × 内部模型 成本统计表（含 Total 汇总行，可排序）；
//   B. 模型定价曲线（多选内部模型 + 三个参数 + 成本/售价/两者切换 + 同指标交点标注）；
//   C. 会话成本曲线（选一场会话，逐轮绘制 成本 与 售价）。
// 纯只读：全部走 GET，无任何写路径。
import { computed, onMounted, onUnmounted, reactive, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { Refresh } from '@element-plus/icons-vue'
import * as echarts from 'echarts/core'
import type { EChartsCoreOption } from 'echarts/core'
import { MarkLineComponent, MarkPointComponent } from 'echarts/components'
import VChart from '@/components/VChart.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'
import {
  getCostStats,
  getCostDefaults,
  getCostCurves,
  getCostSession,
  listSessions,
  type CostStatRow,
  type CostStatTotal,
  type CurveItem,
  type SessionPoint,
  type AdminSession,
} from '@/api/admin'

// 交点标注依赖 markPoint/markLine（VChart 未注册，这里按需补充注册）。
echarts.use([MarkLineComponent, MarkPointComponent])

// 模型曲线的轮次上限（后端契约含 turns 参数；本页固定 K=30）。
const TURNS = 30

// ===== 通用格式化 =====
function fmt2(v: number): string {
  return Number.isFinite(v) ? v.toFixed(2) : '-'
}
function fmtPct(v: number): string {
  return Number.isFinite(v) ? (v * 100).toFixed(2) + '%' : '-'
}
function fmtInt(v: number): string {
  return Number.isFinite(v) ? v.toLocaleString('zh-CN') : '-'
}
function round2(v: number): number {
  return Math.round(v * 100) / 100
}
function round4(v: number): number {
  return Math.round(v * 10000) / 10000
}

// ===== 全局状态 =====
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
function setErr(e: any) {
  errInfo.value = { message: e?.message || '', requestId: e?.requestId || '' }
}

// ===================================================================
// A. 统计表
// ===================================================================
const loadingStats = ref(false)
const statsRows = ref<CostStatRow[]>([])
const statsTotal = ref<CostStatTotal | null>(null)

async function loadStats() {
  loadingStats.value = true
  try {
    const res = await getCostStats()
    statsRows.value = res.Data.Rows || []
    statsTotal.value = res.Data.Total || null
  } catch (e: any) {
    setErr(e)
  } finally {
    loadingStats.value = false
  }
}

// 单元格展示格式化（保留原始数值用于排序）
function cellCost(_row: CostStatRow, _col: unknown, v: number): string {
  return fmt2(v)
}
function cellInt(_row: CostStatRow, _col: unknown, v: number): string {
  return fmtInt(v)
}
function cellHit(_row: CostStatRow, _col: unknown, v: number): string {
  return fmtPct(v)
}
function cellRatio(_row: CostStatRow, _col: unknown, v: number): string {
  return fmt2(v)
}
// 汇总行：展示后端返回的 Total（全量口径），不随排序变化。
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

// ===================================================================
// B. 模型定价曲线
// ===================================================================
const PALETTE = ['#5a5ce8', '#e6a23c', '#67c23a', '#f56c6c', '#36cfc9', '#b37feb', '#ff85c0', '#909399']

const loadingCurves = ref(false)
const curves = ref<CurveItem[]>([])
const curveTurns = ref(TURNS)
const selectedKeys = ref<string[]>([])
const mode = ref<'cost' | 'sale' | 'both'>('both')

// 参数：命中率以 0~1 存储（输入框按百分比展示）；与后端全平台 token 默认值对齐。
const params = reactive({ perTurnInput: 10000, hitRate: 0, inOutRatio: 1 })
// 命中率百分比视图（0~100 双向绑定到 params.hitRate）。
const hitRatePct = computed<number>({
  get: () => round2(params.hitRate * 100),
  set: (v: number) => {
    params.hitRate = (Number.isFinite(v) ? v : 0) / 100
  },
})

function curveKey(c: CurveItem): string {
  return `${c.ChannelID}|${c.InternalModelID}`
}
function curveLabel(c: CurveItem): string {
  return `${c.ChannelName} / ${c.InternalModelID}`
}

async function loadDefaults() {
  try {
    const res = await getCostDefaults()
    const d = res.Data
    // 每轮输入 token 需 >0：缺失/非正时回退 10000，避免输入框出现 0/负数导致曲线退化。
    params.perTurnInput = d.PerTurnInputTokens > 0 ? d.PerTurnInputTokens : 10000
    params.hitRate = d.CacheHitRate
    params.inOutRatio = d.InputOutputRatio
  } catch {
    /* 默认值获取失败不阻塞页面其余部分 */
  }
}

// 曲线请求自增序号：丢弃过期响应，避免旧响应覆盖新响应（竞态）。
let curveReqSeq = 0

async function loadCurves() {
  const seq = ++curveReqSeq
  loadingCurves.value = true
  try {
    const res = await getCostCurves({
      perTurnInput: params.perTurnInput,
      hitRate: params.hitRate,
      inOutRatio: params.inOutRatio,
      turns: TURNS,
    })
    if (seq !== curveReqSeq) return // 已有更新的请求发出，丢弃本次过期响应
    curves.value = res.Data.Curves || []
    curveTurns.value = res.Data.Turns || TURNS
    const valid = new Set(curves.value.map(curveKey))
    if (!selectedKeys.value.length) {
      // 首次加载默认全选
      selectedKeys.value = curves.value.map(curveKey)
    } else {
      // 剔除已不存在的选中项
      selectedKeys.value = selectedKeys.value.filter((k) => valid.has(k))
    }
  } catch (e: any) {
    if (seq !== curveReqSeq) return
    setErr(e)
  } finally {
    if (seq === curveReqSeq) loadingCurves.value = false
  }
}

// 参数变化 debounce(~300ms) 后重新拉取曲线。
let debounceTimer: number | undefined
function scheduleReload() {
  if (debounceTimer !== undefined) window.clearTimeout(debounceTimer)
  debounceTimer = window.setTimeout(() => {
    debounceTimer = undefined
    loadCurves()
  }, 300)
}

const selectedCurves = computed<CurveItem[]>(() =>
  curves.value.filter((c) => selectedKeys.value.includes(curveKey(c))),
)

// 当前展示序列：每个所选模型按模式产出 成本 / 售价（成本实线、售价虚线，同色同模型）。
// metric 标记指标类型（cost/sale），供交点计算仅在「同指标」内配对。
interface BuildSeries {
  name: string
  color: string
  dashed: boolean
  metric: 'cost' | 'sale'
  points: { t: number; y: number }[]
}
const displaySeries = computed<BuildSeries[]>(() => {
  const out: BuildSeries[] = []
  selectedCurves.value.forEach((c, idx) => {
    const color = PALETTE[idx % PALETTE.length]
    const label = curveLabel(c)
    if (mode.value === 'cost' || mode.value === 'both') {
      out.push({
        name: mode.value === 'both' ? `${label} · 成本` : label,
        color,
        dashed: false,
        metric: 'cost',
        points: c.Cost.map((y, i) => ({ t: i + 1, y })),
      })
    }
    if (mode.value === 'sale' || mode.value === 'both') {
      // 售价曲线：后端在 sale_rates 缺失/全 0 时返回空数组，此处跳过空序列，避免画出误导的全 0 线。
      if (!c.Sale.length) return
      out.push({
        name: mode.value === 'both' ? `${label} · 售价` : label,
        color,
        dashed: true,
        metric: 'sale',
        points: c.Sale.map((y, i) => ({ t: i + 1, y })),
      })
    }
  })
  return out
})

// 最小二乘线性拟合：y = m·t + c（用于求相邻曲线交点）。
interface FitLine {
  m: number
  c: number
}
function linearFit(pts: { t: number; y: number }[]): FitLine {
  const n = pts.length
  if (n < 2) return { m: 0, c: pts[0]?.y ?? 0 }
  let sx = 0
  let sy = 0
  let sxx = 0
  let sxy = 0
  for (const p of pts) {
    sx += p.t
    sy += p.y
    sxx += p.t * p.t
    sxy += p.t * p.y
  }
  const den = n * sxx - sx * sx
  if (Math.abs(den) <= 1e-12) return { m: 0, c: sy / n }
  const m = (n * sxy - sx * sy) / den
  const c = (sy - m * sx) / n
  return { m, c }
}

// 相邻两条 series 的交点：k = (c2 − c1) / (m1 − m2)，y = m·k + c。
// 交点含义：在第 k 轮，两条「同指标」曲线（成本↔成本 或 售价↔售价）的每轮值相等——
// 成本口径下代表两条渠道/模型在该轮成本持平，售价口径下代表售价持平；跨指标（成本×售价）无业务含义。
interface Crossing {
  t: number
  y: number
  tag: string
  color: string
}
const crossings = computed<Crossing[]>(() => {
  const ss = displaySeries.value
  const out: Crossing[] = []
  const maxT = Math.max(curveTurns.value, 2)
  // 仅在「同指标」内两两求交：成本组之间、售价组之间分别配对；
  // 禁止把 成本×售价 当作相邻对求交（两者语义/量纲虽同为积分但不可比）。
  // 售价组内每条序列对应一个模型，天然去重（不会与自己求交）。
  for (const metric of ['cost', 'sale'] as const) {
    const group = ss.filter((s) => s.metric === metric)
    for (let i = 0; i + 1 < group.length; i++) {
      const a = group[i]
      const b = group[i + 1]
      const fa = linearFit(a.points)
      const fb = linearFit(b.points)
      const dm = fa.m - fb.m
      if (Math.abs(dm) <= 1e-12) continue
      const t = (fb.c - fa.c) / dm
      const y = fa.m * t + fa.c
      if (!Number.isFinite(t) || !Number.isFinite(y)) continue
      if (t < 1 || t > maxT) continue
      out.push({ t: round2(t), y: round4(y), tag: `${a.name} × ${b.name}`, color: a.color })
    }
  }
  return out
})

const curveOption = computed<EChartsCoreOption>(() => {
  const turns = Math.max(curveTurns.value, 2)
  const series: Record<string, unknown>[] = displaySeries.value.map((s) => ({
    name: s.name,
    type: 'line',
    showSymbol: true,
    symbolSize: 4,
    smooth: false,
    lineStyle: { width: 2, type: s.dashed ? 'dashed' : 'solid', color: s.color },
    itemStyle: { color: s.color },
    data: s.points.map((p) => [p.t, p.y]),
  }))
  // 交点：独立 series 承载 markPoint（pin + 序号标签）。
  if (crossings.value.length) {
    series.push({
      name: '交点',
      type: 'line',
      showSymbol: false,
      silent: true,
      lineStyle: { width: 0, opacity: 0 },
      itemStyle: { color: '#e6a23c' },
      markPoint: {
        symbol: 'pin',
        symbolSize: 40,
        itemStyle: { color: '#e6a23c' },
        label: { show: true, fontSize: 10, color: '#fff', formatter: (p: any) => p?.data?.name || '交点' },
        data: crossings.value.map((c, i) => ({ name: `#${i + 1}`, coord: [c.t, c.y] })),
      },
      data: [],
      z: 10,
    })
  }
  return {
    tooltip: {
      trigger: 'axis',
      valueFormatter: (v: unknown) => (typeof v === 'number' ? String(round4(v)) : String(v)),
    },
    legend: { type: 'scroll', top: 0 },
    grid: { left: 56, right: 24, top: 48, bottom: 40 },
    xAxis: { type: 'value', name: '轮次', nameLocation: 'middle', nameGap: 26, min: 1, max: turns, minInterval: 1 },
    yAxis: { type: 'value', name: '每轮（积分）' },
    series,
  }
})

// ===================================================================
// C. 会话成本曲线
// ===================================================================
const loadingSessions = ref(false)
const sessions = ref<AdminSession[]>([])
const sessionId = ref('')
const loadingSession = ref(false)
const sessionPoints = ref<SessionPoint[]>([])
const sessionTotals = ref<{ CostCredits: number; SaleCredits: number } | null>(null)

// 会话候选：复用既有会话列表接口（字段 SessionID / SessionName）。
async function loadSessions() {
  loadingSessions.value = true
  try {
    const res = await listSessions({ Page: 1, Size: 200 })
    sessions.value = res.Data.List || []
    if (!sessionId.value && sessions.value.length) sessionId.value = sessions.value[0].SessionID
  } catch (e: any) {
    setErr(e)
  } finally {
    loadingSessions.value = false
  }
}
function sessionLabel(s: AdminSession): string {
  return s.SessionName ? `${s.SessionName}（${s.SessionID}）` : s.SessionID
}

async function loadSession() {
  if (!sessionId.value) return
  loadingSession.value = true
  try {
    const res = await getCostSession(sessionId.value)
    sessionPoints.value = res.Data.Points || []
    sessionTotals.value = res.Data.Totals || null
  } catch (e: any) {
    setErr(e)
  } finally {
    loadingSession.value = false
  }
}

const sessionOption = computed<EChartsCoreOption>(() => {
  const pts = sessionPoints.value
  return {
    tooltip: {
      trigger: 'axis',
      valueFormatter: (v: unknown) => (typeof v === 'number' ? String(round4(v)) : String(v)),
    },
    legend: { type: 'scroll', top: 0, data: ['成本', '售价'] },
    grid: { left: 56, right: 24, top: 48, bottom: 40 },
    xAxis: { type: 'value', name: '轮次', nameLocation: 'middle', nameGap: 26, min: 1, minInterval: 1 },
    yAxis: { type: 'value', name: '积分' },
    series: [
      {
        name: '成本',
        type: 'line',
        showSymbol: true,
        symbolSize: 5,
        lineStyle: { width: 2, color: '#5a5ce8' },
        itemStyle: { color: '#5a5ce8' },
        data: pts.map((p) => [p.Turn, p.CostCredits]),
      },
      {
        name: '售价',
        type: 'line',
        showSymbol: true,
        symbolSize: 5,
        lineStyle: { width: 2, color: '#e6a23c' },
        itemStyle: { color: '#e6a23c' },
        data: pts.map((p) => [p.Turn, p.SaleCredits]),
      },
    ],
  }
})

// ===================================================================
// 加载编排
// ===================================================================
const loading = computed(() => loadingStats.value || loadingCurves.value || loadingSession.value)

async function loadAll() {
  errInfo.value = { message: '', requestId: '' }
  await Promise.all([loadStats(), loadDefaults(), loadSessions()])
  await loadCurves()
  await loadSession()
}

async function onRefresh() {
  await loadAll()
  if (!errInfo.value.message) ElMessage.success('已刷新')
}

onMounted(loadAll)
onUnmounted(() => {
  if (debounceTimer !== undefined) window.clearTimeout(debounceTimer)
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
      <!-- A. 统计表 -->
      <div class="card">
        <div class="block-title">
          成本统计
          <el-tag size="small" effect="plain" type="info">渠道 × 内部模型 × 对外模型</el-tag>
        </div>
        <div class="block-desc">
          按「渠道 × 内部模型 × 对外模型」聚合（仅 completed 消耗）：成本 = Σcost_credits、售价 = Σcredits_consumed、请求数、三段 token（输入/缓存读/输出）；
          缓存命中率 = 缓存读 ÷ (输入 + 缓存读)，输入输出比 = 输入 ÷ 输出。表尾为全量合计行（不随排序变化）。
        </div>
        <el-table
          v-loading="loadingStats"
          :data="statsRows"
          border
          stripe
          show-summary
          :summary-method="sumSummary"
          class="table-nowrap"
        >
          <el-table-column prop="ChannelName" label="渠道" min-width="150" sortable />
          <el-table-column prop="InternalModelID" label="内部模型" min-width="170" sortable show-overflow-tooltip />
          <el-table-column prop="ExternalModelName" label="对外模型" min-width="150" sortable show-overflow-tooltip />
          <el-table-column prop="CostCredits" label="成本(积分)" min-width="120" align="right" sortable :formatter="cellCost" />
          <el-table-column prop="SaleCredits" label="售价(积分)" min-width="120" align="right" sortable :formatter="cellCost" />
          <el-table-column prop="CacheHitRate" label="缓存命中率(%)" min-width="140" align="right" sortable :formatter="cellHit" />
          <el-table-column prop="InputOutputRatio" label="输入输出比" min-width="120" align="right" sortable :formatter="cellRatio" />
          <el-table-column prop="Requests" label="请求数" min-width="100" align="right" sortable :formatter="cellInt" />
          <el-table-column prop="InputTokens" label="输入token" min-width="120" align="right" sortable :formatter="cellInt" />
          <el-table-column prop="CacheReadTokens" label="缓存读token" min-width="130" align="right" sortable :formatter="cellInt" />
          <el-table-column prop="OutputTokens" label="输出token" min-width="120" align="right" sortable :formatter="cellInt" />
        </el-table>
        <el-empty v-if="!loadingStats && !statsRows.length" description="暂无成本数据" />
      </div>

      <!-- B. 模型定价曲线 -->
      <div class="card">
        <div class="block-title">
          模型定价曲线
          <el-tag size="small" effect="plain">x = 轮次 1..{{ curveTurns }}</el-tag>
        </div>
        <div class="block-desc">
          每轮成本 = （缓存读×缓存读价 + 新输入×输入价 + 输出×输出价）÷ R（积分；R 默认 10000）；缓存读随轮次累计增长。
          曲线用当前费率推算，与会话曲线的历史实际值口径不同。
        </div>
        <div class="curve-toolbar">
          <span class="p-label">内部模型</span>
          <el-select
            v-model="selectedKeys"
            multiple
            filterable
            collapse-tags
            collapse-tags-tooltip
            placeholder="选择内部模型（可多选）"
            class="curve-select"
          >
            <el-option v-for="c in curves" :key="curveKey(c)" :label="curveLabel(c)" :value="curveKey(c)" />
          </el-select>
          <span class="p-label">每轮新增输入token</span>
          <el-input-number
            v-model="params.perTurnInput"
            :min="0"
            :step="1000"
            controls-position="right"
            class="curve-num"
            @change="scheduleReload"
          />
          <span class="p-label">缓存命中率(%)</span>
          <el-input-number
            v-model="hitRatePct"
            :min="0"
            :max="100"
            :step="1"
            controls-position="right"
            class="curve-num"
            @change="scheduleReload"
          />
          <span class="p-label">输入输出比</span>
          <el-input-number
            v-model="params.inOutRatio"
            :min="0"
            :step="0.1"
            controls-position="right"
            class="curve-num"
            @change="scheduleReload"
          />
          <el-radio-group v-model="mode" class="curve-mode">
            <el-radio-button value="cost">成本</el-radio-button>
            <el-radio-button value="sale">售价</el-radio-button>
            <el-radio-button value="both">两者</el-radio-button>
          </el-radio-group>
        </div>
        <VChart v-if="displaySeries.length" v-loading="loadingCurves" :option="curveOption" height="380px" />
        <div v-else class="empty-hint">
          {{ curves.length ? '请选择至少一个内部模型以绘制曲线。' : '暂无可绘制的模型曲线。' }}
        </div>
        <div v-if="crossings.length" class="cross-list">
          <span class="cross-title">同指标交点（成本↔成本 / 售价↔售价；轮次 · 积分）：</span>
          <el-tag v-for="(c, i) in crossings" :key="i" size="small" type="warning" effect="plain" class="cross-tag">
            #{{ i + 1 }} {{ c.tag }} ≈ 轮次 {{ fmt2(c.t) }} · {{ fmt2(c.y) }}
          </el-tag>
        </div>
      </div>

      <!-- C. 会话成本曲线 -->
      <div class="card">
        <div class="block-title">
          会话成本曲线
          <el-tag size="small" effect="plain" type="info">{{ sessionId || '未选择' }}</el-tag>
        </div>
        <div class="block-desc">
          取该会话每一轮的历史实际 cost_credits / credits_consumed（含当时时段/分档系数），x = 轮次。
        </div>
        <div class="toolbar">
          <span class="p-label">会话</span>
          <el-select
            v-model="sessionId"
            filterable
            :loading="loadingSessions"
            placeholder="选择会话"
            class="session-select"
            @change="loadSession"
          >
            <el-option v-for="s in sessions" :key="s.SessionID" :label="sessionLabel(s)" :value="s.SessionID" />
          </el-select>
          <template v-if="sessionTotals">
            <el-tag size="small" effect="plain" type="info">合计成本 {{ fmt2(sessionTotals.CostCredits) }}</el-tag>
            <el-tag size="small" effect="plain" type="success">合计售价 {{ fmt2(sessionTotals.SaleCredits) }}</el-tag>
            <span class="page-count">共 {{ sessionPoints.length }} 轮</span>
          </template>
        </div>
        <VChart v-if="sessionPoints.length" v-loading="loadingSession" :option="sessionOption" height="340px" />
        <div v-else class="empty-hint">
          {{ sessionId ? '该会话暂无逐轮成本数据。' : '请选择一个会话以查看逐轮成本/售价曲线。' }}
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
  min-width: 320px;
  max-width: 520px;
}
.curve-num {
  width: 140px;
}
.curve-mode {
  margin-left: 4px;
}
.session-select {
  width: 380px;
}
.cross-list {
  margin-top: 12px;
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
}
.cross-title {
  font-size: 13px;
  color: var(--color-text-secondary);
}
.cross-tag {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
}
.empty-hint {
  font-size: 13px;
  color: var(--color-text-tertiary);
  padding: 8px 4px;
}
</style>