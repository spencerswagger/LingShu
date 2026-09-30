<script setup lang="ts">
// 统一统计页：顶部关键指标带 + 「用量分析」「运维概览」两大块。
// 同一组件同时服务 /admin/dashboard（全局）与 /dev/dashboard（本人），
// 运维块与用户维度仅管理端可见（后续可按角色继续细化显隐）。
import { computed, onMounted, ref, watch } from 'vue'
import type { EChartsCoreOption } from 'echarts/core'
import {
  Coin,
  Connection,
  Cpu,
  DataLine,
  Key,
  Odometer,
  Timer,
  TrendCharts,
  CircleCheckFilled,
} from '@element-plus/icons-vue'
import { useAuthStore } from '@/stores/auth'
import { getStatsDashboard, type StatsDashboard, type StatsTopItem } from '@/api/stats'
import { listAnnouncements, type AdminAnnouncement } from '@/api/admin'
import { listDevAnnouncements } from '@/api/dev'
import VChart from '@/components/VChart.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'

const auth = useAuthStore()
const isAdmin = computed(() => auth.role === 'ADMIN')

const loading = ref(false)
const data = ref<StatsDashboard | null>(null)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const announcements = ref<AdminAnnouncement[]>([])

// ===== 时间范围 =====
type Preset = '7' | '30' | '90' | 'custom'
const preset = ref<Preset>('30')
const customRange = ref<[string, string] | null>(null)

const rangeText = computed(() => {
  if (preset.value === 'custom' && customRange.value) {
    return `${customRange.value[0]} ~ ${customRange.value[1]}`
  }
  return `近 ${preset.value} 天`
})

// 生成 RFC3339（带本地时区偏移），保证后端按本地日期切分。
function toRFC3339Local(d: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0')
  const off = -d.getTimezoneOffset()
  const sign = off >= 0 ? '+' : '-'
  const oh = pad(Math.floor(Math.abs(off) / 60))
  const om = pad(Math.abs(off) % 60)
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}T${pad(d.getHours())}:${pad(
    d.getMinutes(),
  )}:${pad(d.getSeconds())}${sign}${oh}:${om}`
}

function parseYMD(s: string): Date {
  const [y, m, d] = s.split('-').map(Number)
  return new Date(y, (m || 1) - 1, d || 1)
}

// 解析为 [起始日 00:00:00, 结束日 23:59:59]
function resolveRange(): { from: Date; to: Date } {
  const now = new Date()
  const end = new Date(now.getFullYear(), now.getMonth(), now.getDate(), 23, 59, 59)
  if (preset.value === 'custom' && customRange.value) {
    const from = parseYMD(customRange.value[0])
    const to = parseYMD(customRange.value[1])
    to.setHours(23, 59, 59)
    return { from, to }
  }
  const days = Number(preset.value) || 30
  const from = new Date(end)
  from.setDate(from.getDate() - (days - 1))
  from.setHours(0, 0, 0, 0)
  return { from, to: end }
}

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const { from, to } = resolveRange()
    const res = await getStatsDashboard(auth.role, {
      from: toRFC3339Local(from),
      to: toRFC3339Local(to),
    })
    data.value = res.data
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}

async function loadAnnouncements() {
  try {
    const res = isAdmin.value ? await listAnnouncements() : await listDevAnnouncements()
    announcements.value = (res.data.list || []).filter((a) => a.enabled).slice(0, 4)
  } catch {
    announcements.value = []
  }
}

onMounted(() => {
  load()
  loadAnnouncements()
})

watch(preset, () => {
  if (preset.value !== 'custom') load()
})
watch(customRange, (v) => {
  if (preset.value === 'custom' && v && v[0] && v[1]) load()
})

// ===== 格式化 =====
const fmtInt = (n: number) => Number(n || 0).toLocaleString('zh-CN')
const fmtMoney = (n: number) =>
  Number(n || 0).toLocaleString('zh-CN', { minimumFractionDigits: 2, maximumFractionDigits: 2 })
const fmtPct = (n: number, digits = 1) => `${(Number(n || 0) * 100).toFixed(digits)}%`
const fmtMs = (n: number) => {
  const v = Number(n || 0)
  return v >= 1000 ? `${(v / 1000).toFixed(2)}s` : `${Math.round(v)}ms`
}
const fmtCompact = (n: number) => {
  const v = Number(n || 0)
  const abs = Math.abs(v)
  if (abs >= 1e9) return `${(v / 1e9).toFixed(2)}B`
  if (abs >= 1e6) return `${(v / 1e6).toFixed(2)}M`
  if (abs >= 1e3) return `${(v / 1e3).toFixed(2)}K`
  if (abs >= 1) return v.toFixed(abs >= 100 ? 0 : 1)
  return v.toFixed(2)
}
const fmtDelta = (n: number) => `${n >= 0 ? '▲' : '▼'} ${Math.abs(Number(n || 0) * 100).toFixed(1)}%`

// ===== 图表主题 =====
const C = {
  primary: '#5a5ce8',
  violet: '#8b5cf6',
  success: '#12b76a',
  warning: '#f79009',
  danger: '#f04438',
  text: '#101828',
  sub: '#667085',
  grid: '#eef0f6',
  series: ['#5a5ce8', '#8b5cf6', '#12b76a', '#f79009', '#f04438', '#0ea5e9'],
}
const axisCommon = {
  axisLine: { lineStyle: { color: C.grid } },
  axisTick: { show: false },
  axisLabel: { color: C.sub, fontSize: 11 },
}
const tooltipCommon = {
  backgroundColor: 'rgba(255,255,255,0.96)',
  borderColor: C.grid,
  textStyle: { color: C.text, fontSize: 12 },
  extraCssText: 'box-shadow:0 6px 20px rgba(16,24,40,0.12);border-radius:8px;',
}

// ===== KPI =====
const daily = computed(() => data.value?.usage.daily ?? [])

function sparkPoints(vals: number[]): string {
  if (!vals.length) return ''
  const w = 100
  const h = 28
  const max = Math.max(...vals)
  const min = Math.min(...vals)
  const span = max - min || 1
  const step = vals.length > 1 ? w / (vals.length - 1) : w
  return vals
    .map((v, i) => `${(i * step).toFixed(1)},${(h - 3 - ((v - min) / span) * (h - 6)).toFixed(1)}`)
    .join(' ')
}

const kpiCards = computed(() => {
  const k = data.value?.kpi
  const calls = daily.value.map((d) => d.calls)
  const tokens = daily.value.map((d) => d.tokens)
  const credits = daily.value.map((d) => Number(d.credits))
  return [
    {
      label: '总 Token',
      value: k ? fmtCompact(k.total_tokens) : '-',
      sub: `输入 ${fmtCompact(data.value?.usage.total_tokens.input ?? 0)} · 输出 ${fmtCompact(
        data.value?.usage.total_tokens.output ?? 0,
      )}`,
      delta: k?.delta_tokens,
      tone: 'primary',
      icon: DataLine,
      spark: sparkPoints(tokens),
    },
    {
      label: '总消费积分',
      value: k ? fmtMoney(k.total_credits) : '-',
      sub: `日均 ${fmtMoney((k?.total_credits ?? 0) / Math.max(data.value?.usage.days ?? 1, 1))}`,
      delta: k?.delta_credits,
      tone: 'violet',
      icon: Coin,
      spark: sparkPoints(credits),
    },
    {
      label: '总请求数',
      value: k ? fmtInt(k.total_requests) : '-',
      sub: `失败 ${fmtInt(k?.failed_requests ?? 0)} 次`,
      delta: k?.delta_requests,
      tone: 'primary',
      icon: Odometer,
      spark: sparkPoints(calls),
    },
    {
      label: '成功率',
      value: k ? fmtPct(k.success_rate) : '-',
      sub: `平均耗时 ${k ? fmtMs(k.avg_duration_ms) : '-'}`,
      tone: 'success',
      icon: CircleCheckFilled,
    },
    {
      label: '平均耗时',
      value: k ? fmtMs(k.avg_duration_ms) : '-',
      sub: `首字 ${k ? fmtMs(k.avg_first_token_ms) : '-'}`,
      tone: 'warning',
      icon: Timer,
    },
    {
      label: '吞吐量',
      value: k ? `${fmtCompact(k.tpm)} TPM` : '-',
      sub: `平均 ${k ? k.rpm.toFixed(2) : '-'} RPM（区间每分钟均值）`,
      tone: 'primary',
      icon: TrendCharts,
    },
  ]
})

// ===== 用量图表 =====
const trendMetric = ref<'tokens' | 'credits' | 'calls'>('tokens')
const trendOption = computed<EChartsCoreOption>(() => {
  const metric = trendMetric.value
  const name = metric === 'tokens' ? 'Token' : metric === 'credits' ? '积分' : '请求数'
  const values = daily.value.map((d) =>
    metric === 'tokens' ? d.tokens : metric === 'credits' ? Number(d.credits) : d.calls,
  )
  return {
    grid: { left: 6, right: 16, top: 22, bottom: 4, containLabel: true },
    tooltip: {
      ...tooltipCommon,
      trigger: 'axis',
      valueFormatter: (v: number) => (metric === 'credits' ? fmtMoney(v) : fmtInt(v)),
    },
    xAxis: {
      type: 'category',
      boundaryGap: false,
      data: daily.value.map((d) => d.date.slice(5)),
      ...axisCommon,
    },
    yAxis: {
      type: 'value',
      splitLine: { lineStyle: { color: C.grid } },
      axisLabel: { color: C.sub, fontSize: 11, formatter: (v: number) => fmtCompact(v) },
    },
    series: [
      {
        name,
        type: 'line',
        smooth: true,
        showSymbol: false,
        data: values,
        lineStyle: { width: 2.4, color: C.primary },
        itemStyle: { color: C.primary },
        areaStyle: { color: 'rgba(90,92,232,0.12)' },
      },
    ],
  }
})

const compositionOption = computed<EChartsCoreOption>(() => {
  const t = data.value?.usage.total_tokens
  const items = [
    { name: '输入', value: t?.input ?? 0 },
    { name: '输出', value: t?.output ?? 0 },
    { name: '缓存读', value: t?.cache_read ?? 0 },
    { name: '缓存写', value: t?.cache_write ?? 0 },
    { name: '推理', value: t?.reasoning ?? 0 },
  ].filter((i) => i.value > 0)
  return {
    tooltip: {
      ...tooltipCommon,
      trigger: 'item',
      formatter: (p: any) => `${p.name}<br/>${fmtInt(p.value)} (${p.percent}%)`,
    },
    legend: { bottom: 0, icon: 'circle', itemWidth: 8, textStyle: { color: C.sub, fontSize: 11 } },
    series: [
      {
        type: 'pie',
        radius: ['48%', '72%'],
        center: ['50%', '44%'],
        avoidLabelOverlap: true,
        itemStyle: { borderColor: '#fff', borderWidth: 2 },
        label: { show: false },
        color: C.series,
        data: items.length ? items : [{ name: '暂无数据', value: 0 }],
      },
    ],
  }
})

// 横向柱状图（Top 维度通用）
function hbarOption(items: StatsTopItem[], valueFn: (i: StatsTopItem) => number, color: string): EChartsCoreOption {
  const rows = items.slice().reverse()
  return {
    grid: { left: 6, right: 28, top: 8, bottom: 4, containLabel: true },
    tooltip: {
      ...tooltipCommon,
      trigger: 'axis',
      axisPointer: { type: 'shadow' },
      formatter: (ps: any) => {
        const p = ps[0]
        const it = rows[p.dataIndex]
        return `${it.label}${it.sub_label ? ' · ' + it.sub_label : ''}<br/>${fmtInt(valueFn(it))}`
      },
    },
    xAxis: {
      type: 'value',
      splitLine: { lineStyle: { color: C.grid } },
      axisLabel: { color: C.sub, fontSize: 11, formatter: (v: number) => fmtCompact(v) },
    },
    yAxis: {
      type: 'category',
      data: rows.map((i) => (i.sub_label ? `${i.label} · ${i.sub_label}` : i.label)),
      ...axisCommon,
      axisLabel: { color: C.text, fontSize: 11, width: 120, overflow: 'truncate' },
    },
    series: [
      {
        type: 'bar',
        data: rows.map(valueFn),
        barMaxWidth: 14,
        itemStyle: { color, borderRadius: [0, 4, 4, 0] },
      },
    ],
  }
}
const modelOption = computed<EChartsCoreOption>(() =>
  hbarOption(data.value?.usage.by_model ?? [], (i) => i.tokens, C.primary),
)
const keyOption = computed<EChartsCoreOption>(() =>
  hbarOption(data.value?.usage.by_key ?? [], (i) => i.tokens, C.violet),
)

// ===== 运维图表 =====
const ops = computed(() => data.value?.ops)

const successOption = computed<EChartsCoreOption>(() => ({
  grid: { left: 6, right: 16, top: 22, bottom: 4, containLabel: true },
  tooltip: {
    ...tooltipCommon,
    trigger: 'axis',
    valueFormatter: (v: number) => `${Number(v).toFixed(1)}%`,
  },
  xAxis: { type: 'category', boundaryGap: false, data: daily.value.map((d) => d.date.slice(5)), ...axisCommon },
  yAxis: {
    type: 'value',
    min: 0,
    max: 100,
    splitLine: { lineStyle: { color: C.grid } },
    axisLabel: { color: C.sub, fontSize: 11, formatter: '{value}%' },
  },
  series: [
    {
      name: '成功率',
      type: 'line',
      smooth: true,
      showSymbol: false,
      data: daily.value.map((d) => Number((d.success_rate * 100).toFixed(1))),
      lineStyle: { width: 2.4, color: C.success },
      itemStyle: { color: C.success },
      areaStyle: { color: 'rgba(18,183,106,0.10)' },
    },
  ],
}))

const latencyOption = computed<EChartsCoreOption>(() => {
  const b = data.value?.usage.latency ?? []
  return {
    grid: { left: 6, right: 16, top: 22, bottom: 4, containLabel: true },
    tooltip: { ...tooltipCommon, trigger: 'axis', axisPointer: { type: 'shadow' } },
    xAxis: { type: 'category', data: b.map((x) => x.bucket), ...axisCommon },
    yAxis: {
      type: 'value',
      splitLine: { lineStyle: { color: C.grid } },
      axisLabel: { color: C.sub, fontSize: 11 },
    },
    series: [
      {
        type: 'bar',
        data: b.map((x) => x.count),
        barMaxWidth: 30,
        itemStyle: { color: C.warning, borderRadius: [4, 4, 0, 0] },
      },
    ],
  }
})

const throughputOption = computed<EChartsCoreOption>(() => ({
  grid: { left: 6, right: 20, top: 30, bottom: 4, containLabel: true },
  tooltip: { ...tooltipCommon, trigger: 'axis' },
  legend: { top: 0, icon: 'circle', itemWidth: 8, textStyle: { color: C.sub, fontSize: 11 } },
  xAxis: { type: 'category', data: daily.value.map((d) => d.date.slice(5)), ...axisCommon },
  yAxis: [
    {
      type: 'value',
      name: 'Token',
      nameTextStyle: { color: C.sub, fontSize: 11 },
      splitLine: { lineStyle: { color: C.grid } },
      axisLabel: { color: C.sub, fontSize: 11, formatter: (v: number) => fmtCompact(v) },
    },
    {
      type: 'value',
      name: '请求',
      nameTextStyle: { color: C.sub, fontSize: 11 },
      splitLine: { show: false },
      axisLabel: { color: C.sub, fontSize: 11 },
    },
  ],
  series: [
    {
      name: 'Token',
      type: 'bar',
      data: daily.value.map((d) => d.tokens),
      barMaxWidth: 16,
      itemStyle: { color: 'rgba(90,92,232,0.75)', borderRadius: [4, 4, 0, 0] },
    },
    {
      name: '请求数',
      type: 'line',
      yAxisIndex: 1,
      smooth: true,
      showSymbol: false,
      data: daily.value.map((d) => d.calls),
      lineStyle: { width: 2.2, color: C.success },
      itemStyle: { color: C.success },
    },
  ],
}))

// 运维计数卡
const opsCounts = computed(() => {
  const o = ops.value
  if (!o) return []
  return [
    {
      icon: Connection,
      title: '渠道',
      total: o.channels.total,
      rows: [
        { label: '健康', value: o.channels.normal, tone: 'success' },
        { label: '排空', value: o.channels.drain, tone: 'warning' },
        { label: '停用', value: o.channels.disabled, tone: 'danger' },
      ],
    },
    {
      icon: Key,
      title: '渠道密钥',
      total: o.keys.total,
      rows: [
        { label: '健康', value: o.keys.normal, tone: 'success' },
        { label: '排空', value: o.keys.drain, tone: 'warning' },
        { label: '停用', value: o.keys.disabled, tone: 'danger' },
      ],
    },
    {
      icon: Cpu,
      title: '对外模型',
      total: o.models.external_total,
      rows: [
        { label: '已启用', value: o.models.external_enabled, tone: 'success' },
        { label: '未启用', value: o.models.external_total - o.models.external_enabled, tone: 'info' },
      ],
    },
    {
      icon: DataLine,
      title: '渠道内部模型',
      total: o.models.internal_total,
      rows: [
        { label: '正常', value: o.models.internal_normal, tone: 'success' },
        { label: '异常', value: o.models.internal_total - o.models.internal_normal, tone: 'warning' },
      ],
    },
  ]
})

const levelType = (l: string) => (['danger', 'warning'].includes(l) ? l : 'primary')
const modeLabel = (m: string) =>
  m === 'sale' ? '售价模式' : m === 'cost' ? '成本模式' : m === '' ? '其他（探测等）' : m
</script>

<template>
  <div class="page-container">
    <!-- 工具栏：时间范围 -->
    <div class="page-header">
      <span class="page-count">统计范围：{{ rangeText }}</span>
      <el-radio-group v-model="preset" size="small">
        <el-radio-button value="7">近 7 天</el-radio-button>
        <el-radio-button value="30">近 30 天</el-radio-button>
        <el-radio-button value="90">近 90 天</el-radio-button>
        <el-radio-button value="custom">自定义</el-radio-button>
      </el-radio-group>
      <el-date-picker
        v-if="preset === 'custom'"
        v-model="customRange"
        type="daterange"
        size="small"
        value-format="YYYY-MM-DD"
        range-separator="至"
        start-placeholder="开始日期"
        end-placeholder="结束日期"
        style="width: 240px"
      />
      <el-button size="small" :loading="loading" @click="load">刷新</el-button>
    </div>

    <div v-loading="loading">
      <!-- 顶部关键指标 -->
      <div class="kpi-grid">
        <div v-for="c in kpiCards" :key="c.label" class="kpi-card" :data-tone="c.tone">
          <div class="kpi-top">
            <span class="kpi-label">{{ c.label }}</span>
            <el-icon class="kpi-ic"><component :is="c.icon" /></el-icon>
          </div>
          <div class="kpi-value">{{ c.value }}</div>
          <div class="kpi-foot">
            <span class="kpi-sub">{{ c.sub }}</span>
            <span v-if="c.delta !== undefined" class="kpi-delta" :class="c.delta >= 0 ? 'up' : 'down'">
              {{ fmtDelta(c.delta as number) }}
            </span>
          </div>
          <svg v-if="c.spark" class="kpi-spark" viewBox="0 0 100 28" preserveAspectRatio="none">
            <polyline :points="c.spark" />
          </svg>
        </div>
      </div>

      <!-- 用量分析 -->
      <div class="section-head">
        <span class="section-bar" />
        <h2 class="section-title">用量分析</h2>
        <span class="section-en">USAGE</span>
        <span class="section-line" />
        <span class="section-note">多维度使用情况</span>
      </div>
      <div class="grid">
        <div class="card span-2">
          <div class="block-title">
            用量趋势
            <el-radio-group v-model="trendMetric" size="small" class="inline-switch">
              <el-radio-button value="tokens">Token</el-radio-button>
              <el-radio-button value="credits">积分</el-radio-button>
              <el-radio-button value="calls">请求数</el-radio-button>
            </el-radio-group>
          </div>
          <VChart :option="trendOption" height="300px" />
        </div>

        <div class="card">
          <div class="block-title">Token 构成</div>
          <VChart :option="compositionOption" height="300px" />
        </div>

        <div class="card">
          <div class="block-title">Top 模型</div>
          <VChart v-if="(data?.usage.by_model?.length ?? 0) > 0" :option="modelOption" height="260px" />
          <el-empty v-else description="暂无数据" :image-size="60" />
        </div>

        <div class="card">
          <div class="block-title">Top 渠道密钥</div>
          <VChart v-if="(data?.usage.by_key?.length ?? 0) > 0" :option="keyOption" height="260px" />
          <el-empty v-else description="暂无数据" :image-size="60" />
        </div>

        <div class="card">
          <div class="block-title">计费模式分布</div>
          <div class="mode-list">
            <div v-for="m in data?.usage.by_mode ?? []" :key="m.key" class="mode-item">
              <div class="mode-head">
                <span class="mode-name">{{ modeLabel(m.key) }}</span>
                <span class="mode-calls">{{ fmtInt(m.calls) }} 次</span>
              </div>
              <div class="mode-bar">
                <div
                  class="mode-bar-in"
                  :style="{
                    width:
                      ((m.calls / Math.max((data?.usage.by_mode ?? []).reduce((s, x) => s + x.calls, 0), 1)) *
                        100).toFixed(1) + '%',
                  }"
                />
              </div>
              <div class="mode-foot">
                <span>{{ fmtCompact(m.tokens) }} Token</span>
                <span>{{ fmtMoney(m.credits) }} 积分</span>
              </div>
            </div>
            <el-empty v-if="!(data?.usage.by_mode?.length ?? 0)" description="暂无数据" :image-size="60" />
          </div>
        </div>

        <div v-if="isAdmin" class="card span-2">
          <div class="block-title">Top 用户</div>
          <el-table
            :data="(data?.usage.by_user ?? []).slice(0, 8)"
            size="small"
            class="table-nowrap"
            empty-text="暂无数据"
          >
            <el-table-column type="index" label="#" width="52" />
            <el-table-column label="用户" min-width="140">
              <template #default="{ row }">
                <span class="cell-main">{{ row.label }}</span>
                <span v-if="row.sub_label" class="cell-sub">{{ row.sub_label }}</span>
              </template>
            </el-table-column>
            <el-table-column label="请求数" align="right" min-width="100">
              <template #default="{ row }">{{ fmtInt(row.calls) }}</template>
            </el-table-column>
            <el-table-column label="失败" align="right" min-width="90">
              <template #default="{ row }">{{ fmtInt(row.failed) }}</template>
            </el-table-column>
            <el-table-column label="Token" align="right" min-width="110">
              <template #default="{ row }">{{ fmtCompact(row.tokens) }}</template>
            </el-table-column>
            <el-table-column label="积分" align="right" min-width="110">
              <template #default="{ row }">{{ fmtMoney(row.credits) }}</template>
            </el-table-column>
          </el-table>
        </div>
      </div>

      <!-- 运维概览（仅管理端） -->
      <template v-if="isAdmin">
        <div class="section-head">
          <span class="section-bar section-bar--ops" />
          <h2 class="section-title">运维概览</h2>
          <span class="section-en">OPERATIONS</span>
          <span class="section-line" />
          <span class="section-note">渠道密钥 / 模型 / 稳定性</span>
        </div>

        <div class="ops-count-grid">
          <div v-for="g in opsCounts" :key="g.title" class="card ops-count-card">
            <div class="ops-count-head">
              <el-icon class="ops-count-ic"><component :is="g.icon" /></el-icon>
              <span class="ops-count-title">{{ g.title }}</span>
              <span class="ops-count-total">{{ g.total }}</span>
            </div>
            <div class="ops-count-rows">
              <div v-for="r in g.rows" :key="r.label" class="ops-count-row">
                <span class="ops-dot" :data-tone="r.tone" />
                <span class="ops-count-label">{{ r.label }}</span>
                <span class="ops-count-value">{{ r.value }}</span>
              </div>
            </div>
          </div>
        </div>

        <div class="grid">
          <div class="card span-2">
            <div class="block-title">成功率趋势</div>
            <VChart :option="successOption" height="280px" />
          </div>
          <div class="card">
            <div class="block-title">
              耗时分布
              <span class="block-note">
                P50 {{ fmtMs(data?.usage.p50_duration_ms ?? 0) }} · P90
                {{ fmtMs(data?.usage.p90_duration_ms ?? 0) }} · P95
                {{ fmtMs(data?.usage.p95_duration_ms ?? 0) }}
              </span>
            </div>
            <VChart :option="latencyOption" height="280px" />
          </div>
          <div class="card span-2">
            <div class="block-title">吞吐量趋势</div>
            <VChart :option="throughputOption" height="280px" />
          </div>
          <div class="card">
            <div class="block-title">错误 Top</div>
            <el-table :data="(data?.usage.errors ?? []).slice(0, 8)" size="small" empty-text="暂无失败记录">
              <el-table-column label="错误信息" min-width="200">
                <template #default="{ row }">
                  <span class="cell-main">{{ row.label }}</span>
                </template>
              </el-table-column>
              <el-table-column label="次数" width="90" align="right">
                <template #default="{ row }">{{ fmtInt(row.calls) }}</template>
              </el-table-column>
            </el-table>
          </div>
        </div>
      </template>

      <!-- 最近公告 -->
      <div v-if="announcements.length" class="section-head">
        <span class="section-bar section-bar--news" />
        <h2 class="section-title">最近公告</h2>
        <span class="section-line" />
      </div>
      <div v-if="announcements.length" class="card announce-card">
        <div v-for="a in announcements" :key="a.id" class="announce-item">
          <el-tag :type="(levelType(a.level) as any)" size="small" effect="light">{{ a.level }}</el-tag>
          <span class="announce-title">{{ a.title }}</span>
          <span class="announce-time">{{ a.publish_at || '立即发布' }}</span>
        </div>
      </div>

      <ErrorBubble
        v-if="errInfo.message || errInfo.requestId"
        :message="errInfo.message"
        :request-id="errInfo.requestId"
      />
    </div>
  </div>
</template>

<style scoped>
/* ===== 顶部指标带 ===== */
.kpi-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(210px, 1fr));
  gap: 14px;
  margin-bottom: 22px;
}
.kpi-card {
  position: relative;
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-m);
  padding: 14px 16px 30px;
  box-shadow: var(--shadow-card);
  transition: transform 0.18s ease, box-shadow 0.18s ease;
  overflow: hidden;
  min-width: 0;
}
.kpi-card:hover {
  transform: translateY(-2px);
  box-shadow: var(--shadow-pop);
}
.kpi-card::before {
  content: '';
  position: absolute;
  inset: 0 auto 0 0;
  width: 3px;
  background: var(--brand-primary);
}
.kpi-card[data-tone='violet']::before { background: var(--brand-violet); }
.kpi-card[data-tone='success']::before { background: var(--color-success); }
.kpi-card[data-tone='warning']::before { background: var(--color-warning); }
.kpi-top {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-bottom: 6px;
}
.kpi-label {
  font-size: 13px;
  color: var(--color-text-secondary);
}
.kpi-ic {
  font-size: 16px;
  color: var(--color-text-tertiary);
}
.kpi-value {
  font-size: 26px;
  font-weight: 700;
  line-height: 1.15;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.kpi-foot {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 6px;
  font-size: 12px;
  color: var(--color-text-secondary);
  min-width: 0;
}
.kpi-sub {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.kpi-delta {
  flex-shrink: 0;
  font-weight: 600;
}
.kpi-delta.up { color: var(--color-success); }
.kpi-delta.down { color: var(--color-danger); }
.kpi-spark {
  position: absolute;
  left: 0;
  right: 0;
  bottom: 0;
  width: 100%;
  height: 28px;
  opacity: 0.5;
}
.kpi-spark polyline {
  fill: none;
  stroke: var(--brand-primary);
  stroke-width: 1.6;
  vector-effect: non-scaling-stroke;
}
.kpi-card[data-tone='violet'] .kpi-spark polyline { stroke: var(--brand-violet); }
.kpi-card[data-tone='success'] .kpi-spark polyline { stroke: var(--color-success); }
.kpi-card[data-tone='warning'] .kpi-spark polyline { stroke: var(--color-warning); }

/* ===== 区块标题 ===== */
.section-head {
  display: flex;
  align-items: center;
  gap: 10px;
  margin: 6px 0 14px;
}
.section-bar {
  width: 4px;
  height: 18px;
  border-radius: 3px;
  background: var(--brand-gradient);
}
.section-bar--ops { background: linear-gradient(135deg, #12b76a 0%, #0ea5e9 100%); }
.section-bar--news { background: linear-gradient(135deg, #f79009 0%, #f04438 100%); }
.section-title {
  margin: 0;
  font-size: 17px;
  font-weight: 700;
  color: var(--color-text);
}
.section-en {
  font-size: 11px;
  letter-spacing: 0.14em;
  color: var(--color-text-tertiary);
  text-transform: uppercase;
}
.section-line {
  flex: 1;
  height: 1px;
  background: var(--color-border);
}
.section-note {
  font-size: 12px;
  color: var(--color-text-tertiary);
}

/* ===== 网格与卡片 ===== */
.grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 16px;
  margin-bottom: 10px;
}
.grid .span-2 {
  grid-column: 1 / -1;
}
@media (max-width: 1000px) {
  .grid {
    grid-template-columns: 1fr;
  }
}
.card {
  padding: 16px 18px;
}
.block-note {
  margin-left: auto;
  font-size: 12px;
  font-weight: 400;
  color: var(--color-text-secondary);
}
.inline-switch {
  margin-left: auto;
}

/* 计费模式分布 */
.mode-list {
  display: flex;
  flex-direction: column;
  gap: 14px;
  padding-top: 4px;
}
.mode-item {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.mode-head {
  display: flex;
  justify-content: space-between;
  font-size: 13px;
}
.mode-name { font-weight: 600; color: var(--color-text); }
.mode-calls { color: var(--color-text-secondary); }
.mode-bar {
  height: 8px;
  border-radius: 6px;
  background: var(--color-border);
  overflow: hidden;
}
.mode-bar-in {
  height: 100%;
  border-radius: 6px;
  background: var(--brand-gradient);
  transition: width 0.4s ease;
}
.mode-foot {
  display: flex;
  justify-content: space-between;
  font-size: 12px;
  color: var(--color-text-secondary);
}

/* 运维计数卡 */
.ops-count-grid {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(220px, 1fr));
  gap: 16px;
  margin-bottom: 16px;
}
.ops-count-card {
  padding: 14px 16px;
}
.ops-count-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 10px;
}
.ops-count-ic {
  font-size: 16px;
  color: var(--brand-primary);
}
.ops-count-title {
  font-size: 14px;
  font-weight: 600;
  color: var(--color-text);
}
.ops-count-total {
  margin-left: auto;
  font-size: 22px;
  font-weight: 700;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
}
.ops-count-rows {
  display: flex;
  flex-wrap: wrap;
  gap: 6px 18px;
}
.ops-count-row {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  color: var(--color-text-secondary);
}
.ops-dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--color-info);
}
.ops-dot[data-tone='success'] { background: var(--color-success); }
.ops-dot[data-tone='warning'] { background: var(--color-warning); }
.ops-dot[data-tone='danger'] { background: var(--color-danger); }
.ops-dot[data-tone='info'] { background: var(--color-text-tertiary); }
.ops-count-value {
  font-weight: 600;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
}

/* 表格辅助 */
.cell-main { color: var(--color-text); }
.cell-sub {
  margin-left: 8px;
  font-size: 12px;
  color: var(--color-text-tertiary);
}

/* 公告 */
.announce-card {
  margin-bottom: 6px;
}
.announce-item {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 9px 0;
  border-bottom: 1px solid var(--color-border);
}
.announce-item:last-child {
  border-bottom: none;
}
.announce-title {
  flex: 1;
  font-weight: 500;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.announce-time {
  color: var(--color-text-secondary);
  font-size: 12px;
}
</style>