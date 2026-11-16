<script setup lang="ts">
// 内部模型（网关同步的只读镜像）+ 拓扑画布。
//
// 两条硬口径（数据层契约 §2.1–§2.3）：
//   1. 这门表是**镜像**，没有软删列：`stale=true` 只表示「本轮同步没见到」，仍展示但标灰，
//      删除判定交给网关（它才是真源）。
//   2. `model_links` 只存画布坐标，**不是关系真源**。缺行 = 用自动布局，不影响售卖与计费；
//      绑定真源是 `external_models.bound_internal_ref`。
import { computed, onMounted, ref } from 'vue'
import { getInternalModels, type InternalModel, type InternalModelView } from '@/api/v2'
import { mockInternalModels } from '@/api/v2.mock'
import { useV2Load } from '@/composables/useV2Load'
import V2Page from '@/components/v2/V2Page.vue'
import StatCards from '@/components/v2/StatCards.vue'
import StatusPill from '@/components/v2/StatusPill.vue'
import VChart from '@/components/VChart.vue'
import type { StatItem, V2Dep } from '@/components/v2/types'
import { fmtCompact, fmtDateTime } from '@/utils/format'

const tab = ref<'snapshot' | 'canvas'>('snapshot')
const keyword = ref('')

const { state, data, error, load, showDemo, exitDemo } = useV2Load<InternalModelView>(getInternalModels, mockInternalModels)

onMounted(() => void load())

const deps = computed<V2Dep[]>(() => [
  { path: 'GET /admin/internal-models', state: state.value === 'live' || state.value === 'demo' ? 'ready' : 'pending' },
  { path: 'PUT /admin/model-links', state: 'pending' },
])

const rows = computed<InternalModel[]>(() => {
  const kw = keyword.value.trim().toLowerCase()
  const items = data.value?.Items || []
  if (!kw) return items
  return items.filter(
    (m) =>
      m.Ref.toLowerCase().includes(kw) ||
      m.ProviderRef.toLowerCase().includes(kw) ||
      m.DisplayName.toLowerCase().includes(kw),
  )
})

const stats = computed<StatItem[]>(() => {
  const d = data.value
  if (!d) return []
  const groups = d.Items.filter((m) => m.Kind === 'group').length
  return [
    { label: '内部模型', value: fmtCompact(d.Items.length), hint: `组 ${groups} / 直通 ${d.Items.length - groups}` },
    { label: '本轮未见（stale）', value: fmtCompact(d.StaleCount), tone: d.StaleCount > 0 ? 'warn' : 'ok', hint: '标灰但仍在售；删除由网关判定' },
    {
      label: '未绑定内部模型的对外模型',
      value: fmtCompact(d.UnboundExternal),
      tone: d.UnboundExternal > 0 ? 'danger' : 'ok',
      hint: '未绑定 = 不售卖（客户端请求 404）',
    },
    { label: '最后同步', value: d.SyncedAt ? fmtDateTime(d.SyncedAt) : '—', hint: '镜像每轮同步刷新 last_seen_at' },
  ]
})

// 画布：内部模型节点（按 kind 上色）+ 已落位的外部模型节点 + 连线。
// 坐标来自 pos_x/pos_y；缺失（都为 0）时按索引自动环形布局，避免全部叠在原点。
const canvasOption = computed(() => {
  const d = data.value
  const items = d?.Items || []
  const links = d?.Links || []
  const positions = new Map<string, { x: number; y: number }>()
  items.forEach((m, i) => {
    const auto = m.PosX === 0 && m.PosY === 0
    positions.set(m.Ref, {
      x: auto ? 80 + Math.cos((i / Math.max(items.length, 1)) * Math.PI * 2) * 300 : m.PosX,
      y: auto ? 80 + Math.sin((i / Math.max(items.length, 1)) * Math.PI * 2) * 200 : m.PosY,
    })
  })

  const internalPts = items.map((m) => {
    const p = positions.get(m.Ref) || { x: 0, y: 0 }
    return {
      name: m.Ref,
      value: [p.x, p.y, m.ContextLen],
      kind: m.Kind,
      stale: m.Stale,
      itemStyle: { color: m.Kind === 'group' ? '#8b5cf6' : '#5a5ce8', opacity: m.Stale ? 0.45 : 1 },
    }
  })

  const externalPts = links.map((l) => {
    const base = positions.get(l.InternalRef) || { x: 0, y: 0 }
    return {
      name: `对外模型 #${l.ExternalModelID}`,
      // 外部模型的画布坐标同样来自 model_links；缺失时落在内部模型节点右侧
      value: [l.PosX || base.x + 120, l.PosY || base.y + 90],
      itemStyle: { color: '#12b76a' },
    }
  })

  const linkLines = links.map((l) => {
    const base = positions.get(l.InternalRef) || { x: 0, y: 0 }
    return {
      coords: [
        [base.x, base.y],
        [l.PosX || base.x + 120, l.PosY || base.y + 90],
      ],
    }
  })

  return {
    tooltip: { trigger: 'item' },
    legend: { top: 0, right: 8 },
    grid: { left: 8, right: 16, top: 28, bottom: 8, containLabel: true },
    xAxis: { type: 'value', name: 'X', axisLabel: { show: false }, splitLine: { show: false } },
    yAxis: { type: 'value', name: 'Y', axisLabel: { show: false }, splitLine: { show: false } },
    series: [
      {
        name: '内部模型',
        type: 'scatter',
        symbolSize: 16,
        label: { show: true, position: 'right', formatter: '{b}', fontSize: 11 },
        data: internalPts,
      },
      {
        name: '对外模型',
        type: 'scatter',
        symbolSize: 12,
        label: { show: false },
        data: externalPts,
      },
      {
        name: '绑定',
        type: 'lines',
        coordinateSystem: 'cartesian2d',
        lineStyle: { color: '#c1c2f8', width: 1, curveness: 0.1 },
        data: linkLines,
        silent: true,
      },
    ],
  }
})
</script>

<template>
  <V2Page
    title="内部模型与拓扑"
    intro="内部模型是网关同步过来的只读镜像（ref / 供应商 / 组或直通 / 能力 / 牌价）；拓扑画布只存落位坐标，不承载绑定关系。"
    source="数据层契约 §2.1–§2.3（internal_models / model_links）"
    :deps="deps"
    :state="state"
    :error="error"
    pending-hint="内部模型与拓扑接口属阶段一同步器交付，读取端点未上线。快照与画布已按 0019 表结构就位，接口上线即接真数据。"
    @reload="load()"
    @demo="showDemo"
    @exit-demo="exitDemo"
  >
    <template #actions>
      <el-input v-model="keyword" size="small" placeholder="搜索 ref / 供应商" clearable style="width: 180px" />
    </template>

    <template v-if="data">
      <StatCards :items="stats" :columns="4" />

      <el-tabs v-model="tab">
        <el-tab-pane label="快照" name="snapshot">
          <div class="card">
            <div class="table-wrap">
              <el-table :data="rows" size="small" border stripe empty-text="没有内部模型">
                <el-table-column prop="Ref" label="引用（ref）" min-width="200" show-overflow-tooltip />
                <el-table-column prop="ProviderRef" label="供应商" width="130" show-overflow-tooltip />
                <el-table-column label="类型" width="90">
                  <template #default="{ row }"><StatusPill domain="kind" :value="row.Kind" /></template>
                </el-table-column>
                <el-table-column prop="DisplayName" label="展示名" min-width="150" show-overflow-tooltip />
                <el-table-column label="上下文" width="110" align="right">
                  <template #default="{ row }">{{ row.ContextLen ? fmtCompact(row.ContextLen) : '—' }}</template>
                </el-table-column>
                <el-table-column label="最后见到" width="150">
                  <template #default="{ row }">{{ fmtDateTime(row.LastSeenAt) }}</template>
                </el-table-column>
                <el-table-column label="状态" width="100">
                  <template #default="{ row }">
                    <el-tag v-if="row.Stale" type="info" size="small" effect="plain">本轮未见</el-tag>
                    <el-tag v-else type="success" size="small" effect="plain">在售</el-tag>
                  </template>
                </el-table-column>
              </el-table>
            </div>
            <p class="foot">
              镜像表没有软删列：删除由网关侧决定，灵枢只按「本轮是否见到」标 stale；stale 的模型仍展示、仍可绑定。
            </p>
          </div>
        </el-tab-pane>

        <el-tab-pane label="拓扑" name="canvas">
          <div class="card">
            <div class="card-title">
              落位关系（{{ data.Items.length }} 个内部模型 / {{ data.Links.length }} 条落位）
            </div>
            <VChart :option="canvasOption" height="360px" />
            <p class="foot">
              绿点 = 已落位的外部模型，连线只表达「画布位置」；缺行时自动布局，不影响售卖与计费。画布拖拽落位等
              <code>PUT /admin/model-links</code> 上线后开放。
            </p>
          </div>
        </el-tab-pane>
      </el-tabs>
    </template>

    <el-empty v-else-if="state === 'live'" description="还没有内部模型：同步器未跑过，或网关未返回任何模型" />
  </V2Page>
</template>

<style scoped>
.card {
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-m);
  box-shadow: var(--shadow-card);
  padding: 12px 14px;
}
.card-title {
  font-size: 13px;
  font-weight: 600;
  margin-bottom: 8px;
}
.table-wrap {
  overflow-x: auto;
}
.foot {
  margin: 8px 0 0;
  font-size: 12px;
  color: var(--color-text-tertiary);
  line-height: 1.6;
}

@media (max-width: 640px) {
  .card {
    padding: 10px;
  }
  :deep(.el-table) {
    font-size: 14px;
  }
}
</style>
