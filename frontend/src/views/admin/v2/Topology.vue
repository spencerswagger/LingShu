<script setup lang="ts">
// 内部模型（网关同步的只读镜像）+ 拓扑画布。
//
// 两条硬口径（数据层契约 §2.1–§2.3）：
//   1. 这门表是**镜像**，没有软删列：`stale=true` 只表示「本轮同步没见到」，仍展示但标灰，
//      删除判定交给网关（它才是真源）。
//   2. `model_links` 只存画布坐标，**不是关系真源**。缺行 = 用自动布局，不影响售卖与计费；
//      绑定真源是 `external_models.bound_internal_ref`。
import { computed, onMounted, ref } from 'vue'
import { getTopology, type InternalModel, type InternalModelView } from '@/api/v2'
import { mockTopology } from '@/mock/topology'
import { useV2Load } from '@/composables/useV2Load'
import V2Page from '@/components/v2/V2Page.vue'
import StatCards from '@/components/v2/StatCards.vue'
import StatusPill from '@/components/v2/StatusPill.vue'
import VChart from '@/components/VChart.vue'
import type { StatItem, V2Dep } from '@/components/v2/types'
import { fmtCompact, fmtDateTime } from '@/utils/format'

const tab = ref<'snapshot' | 'canvas'>('snapshot')
const keyword = ref('')

const { state, data, error, load, showDemo, exitDemo } = useV2Load<InternalModelView>(getTopology, mockTopology)

onMounted(() => void load())

const deps = computed<V2Dep[]>(() => [
  { path: 'GET /admin/topology', state: state.value === 'live' ? 'ready' : 'pending' },
  { path: 'POST /admin/topology/sync', state: 'pending' },
  // 坐标保存是可延后项（T2）：未就绪时自动布局、坐标不持久化，不影响售卖与计费
  { path: 'PUT /admin/topology/links', state: 'pending' },
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

// 供应商层（四层里的第 1–2 层）：T1 会带 provider / account 聚合；未上线时用内部模型的 provider_ref 现算，
// 保证「供应商 → 内部模型」这两层的计数在页面上永远和列表条数对得上（Spec P02 验收 1）。
const providerRows = computed(() => {
  const map = new Map<string, { provider: string; models: number; stale: number }>()
  for (const m of data.value?.Items || []) {
    const p = m.ProviderRef || '(未知)'
    const cur = map.get(p) || { provider: p, models: 0, stale: 0 }
    cur.models += 1
    if (m.Stale) cur.stale += 1
    map.set(p, cur)
  }
  return [...map.values()].sort((a, b) => b.models - a.models)
})

const syncState = computed(() => data.value?.Sync || null)
const syncTone = computed(() => {
  const st = syncState.value?.State
  if (st === 'ok') return 'ok'
  if (st === 'failed') return 'danger'
  return 'warn'
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
    title="上游拓扑"
    intro="网关侧「供应商 → 账号 → 内部模型 → 对外模型」四层关系与同步新鲜度。内部模型是只读镜像，拓扑画布只存落位坐标，不承载绑定关系。"
    source="数据层契约 §2.1–§2.3（internal_models / model_links）"
    :deps="deps"
    :state="state"
    :error="error"
    pending-hint="拓扑聚合（T1）、手动同步（T3）、坐标保存（T2）待 Core 上线。快照/画布/同步状态条已按 0019 表结构就位，接口上线即接真数据。"
    @reload="load()"
    @demo="showDemo"
    @exit-demo="exitDemo"
  >
    <template #actions>
      <el-input v-model="keyword" size="small" placeholder="搜索 ref / 供应商" clearable style="width: 180px" />
      <!-- T3 未上线：禁用 + 说明，不做点了没反应的按钮（Spec §5 规则 5） -->
      <el-tooltip content="接口 T3（POST /admin/topology/sync）待上线；当前由后台定时同步" placement="top">
        <span><el-button size="small" disabled>立即同步</el-button></span>
      </el-tooltip>
    </template>

    <template v-if="data">
      <!-- 同步状态条：最后同步时间 / 结果 / 新增 / 失效。失败时**不清空**已有快照（Spec P02 验收 3） -->
      <div class="sync-bar" :class="`is-${syncTone}`">
        <span class="sync-item">
          最后成功
          <strong>{{ syncState?.LastSuccessAt ? fmtDateTime(syncState.LastSuccessAt) : '从未同步' }}</strong>
        </span>
        <span class="sync-item">
          同步结果
          <el-tag :type="syncTone === 'ok' ? 'success' : syncTone === 'danger' ? 'danger' : 'warning'" size="small" effect="plain">
            {{ syncState?.State === 'ok' ? '成功' : syncState?.State === 'failed' ? '失败' : '未运行' }}
          </el-tag>
          <span v-if="syncState?.Message" class="sync-msg">{{ syncState.Message }}</span>
        </span>
        <span class="sync-item">本轮新增 <strong>{{ syncState?.Created ?? 0 }}</strong></span>
        <span class="sync-item">本轮失效 <strong>{{ syncState?.Stale ?? 0 }}</strong></span>
        <span class="sync-item sub">同步失败时保留旧快照，不空白；手动同步接口（T3）上线后本按钮启用</span>
      </div>

      <StatCards :items="stats" :columns="4" />

      <!-- 供应商层 -->
      <div class="card">
        <div class="card-title">供应商（{{ providerRows.length }}）</div>
        <div class="provider-strip">
          <div v-for="p in providerRows" :key="p.provider" class="provider-chip">
            <span class="pv-name">{{ p.provider }}</span>
            <span class="pv-meta">{{ p.models }} 个内部模型<template v-if="p.stale"> · {{ p.stale }} 个本轮未见</template></span>
          </div>
        </div>
        <p class="foot">
          四层视图 = 供应商 → 账号 → 内部模型 → 对外模型。账号层需要 T1 的 account 聚合，接口上线后在这一段补一列；当前账号维度只在订阅页维护。
        </p>
      </div>

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
.sync-bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 14px;
  padding: 8px 12px;
  border-radius: var(--radius-s);
  border: 1px solid var(--color-border);
  background: #fafbfd;
  font-size: 13px;
}
.sync-bar.is-warn {
  background: #fff8e6;
  border-color: #ffe0a3;
}
.sync-bar.is-danger {
  background: #fef3f2;
  border-color: #fecdca;
}
.sync-item {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.sync-msg {
  color: #b42318;
  font-size: 12px;
}
.provider-strip {
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
}
.provider-chip {
  display: flex;
  flex-direction: column;
  gap: 2px;
  padding: 6px 10px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-s);
  background: #fff;
  min-width: 150px;
}
.pv-name {
  font-weight: 600;
  font-size: 13px;
}
.pv-meta {
  font-size: 12px;
  color: var(--color-text-tertiary);
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
