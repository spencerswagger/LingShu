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
  { path: 'POST /admin/topology/sync', state: state.value === 'live' ? 'ready' : 'pending' },
  // 坐标保存（T2）：缺行时自动布局，不影响售卖与计费
  { path: 'PUT /admin/topology/links', state: 'ready' },
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
      hint: '绑定能力尚未上线，当前不拦截；此处只作事实展示',
    },
    { label: '最后同步', value: d.SyncedAt ? fmtDateTime(d.SyncedAt) : '—', hint: '镜像每轮同步刷新 last_seen_at' },
  ]
})

// 供应商层（四层里的第 1–2 层）：T1 会带 provider / account 聚合；未上线时用内部模型的 provider_ref 现算，
// 保证「供应商 → 内部模型」这两层的计数在页面上永远和列表条数对得上（Spec P02 验收 1）。
const providerRows = computed(() => {
  // 后端 T1 直接给 Providers（含账号计数）；老响应/演示数据没有这一段时按 Items 现算，
  // 保证「供应商 → 内部模型」两层的计数在页面上永远和列表条数对得上（Spec P02 验收 1）。
  const fromApi = data.value?.Providers || []
  if (fromApi.length > 0) {
    return fromApi
      .map((p) => ({ provider: p.Provider || '(未知)', models: p.Models, stale: p.Stale, accounts: p.Accounts }))
      .sort((a, b) => b.models - a.models)
  }
  const map = new Map<string, { provider: string; models: number; stale: number; accounts: number }>()
  for (const m of data.value?.Items || []) {
    const p = m.ProviderRef || '(未知)'
    const cur = map.get(p) || { provider: p, models: 0, stale: 0, accounts: 0 }
    cur.models += 1
    if (m.Stale) cur.stale += 1
    map.set(p, cur)
  }
  return [...map.values()].sort((a, b) => b.models - a.models)
})

// 账号层（第 2 层）：账单/账本/订阅里出现过的上游账号；Subscribed = 已录入订阅（有固定月费）。
const accountRows = computed(() =>
  [...(data.value?.Accounts || [])].sort(
    (a, b) => a.Provider.localeCompare(b.Provider) || a.Account.localeCompare(b.Account),
  ),
)

// 绑定边：真源是 external_models.bound_internal_ref（不是 model_links —— 那张表只存坐标）。
const bindingEdges = computed(() =>
  (data.value?.Externals || []).filter((e) => e.BoundInternalRef !== ''),
)

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
  const externals = d?.Externals || []
  const positions = new Map<string, { x: number; y: number }>()
  items.forEach((m, i) => {
    const auto = m.PosX === 0 && m.PosY === 0
    positions.set(m.Ref, {
      x: auto ? 80 + Math.cos((i / Math.max(items.length, 1)) * Math.PI * 2) * 300 : m.PosX,
      y: auto ? 80 + Math.sin((i / Math.max(items.length, 1)) * Math.PI * 2) * 200 : m.PosY,
    })
  })
  // 坐标只来自 model_links（按下标落位）；缺行 = 落在所绑定内部模型的右侧。
  const coordOf = new Map<string, { x: number; y: number }>()
  for (const l of links) coordOf.set(`${l.InternalRef}|${l.ExternalModelID}`, { x: l.PosX, y: l.PosY })

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

  // 第 4 层：对外模型。未绑定的按列排在右下方（一眼看出「这些还没接内部模型」）。
  let unboundIdx = 0
  const externalPts = externals.map((e) => {
    const base = (e.BoundInternalRef && positions.get(e.BoundInternalRef)) || null
    const coord = coordOf.get(`${e.BoundInternalRef}|${e.ID}`)
    let value: [number, number]
    if (coord) value = [coord.x, coord.y]
    else if (base) value = [base.x + 140, base.y + 70]
    else value = [520, 60 + unboundIdx++ * 60]
    return {
      name: e.ExternalName || `对外模型 #${e.ID}`,
      value,
      itemStyle: {
        color: e.Enabled ? '#12b76a' : '#98a2b3',
        opacity: e.InternalModelHit || !e.BoundInternalRef ? 1 : 0.4,
      },
    }
  })

  // 绑定边：只画真实存在的绑定（bound_internal_ref 非空），不是「画布落位」。
  const linkLines = bindingEdges.value.map((e) => {
    const base = positions.get(e.BoundInternalRef) || { x: 0, y: 0 }
    const coord = coordOf.get(`${e.BoundInternalRef}|${e.ID}`)
    const target = coord ? { x: coord.x, y: coord.y } : { x: base.x + 140, y: base.y + 70 }
    return {
      coords: [
        [base.x, base.y],
        [target.x, target.y],
      ],
      lineStyle: e.InternalModelHit ? undefined : { type: 'dashed', color: '#f04438' },
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
        name: '绑定（悬空为红色虚线）',
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

      <!-- 第 1 层：供应商 -->
      <div class="card">
        <div class="card-title">供应商（{{ providerRows.length }}）</div>
        <div class="provider-strip">
          <div v-for="p in providerRows" :key="p.provider" class="provider-chip">
            <span class="pv-name">{{ p.provider }}</span>
            <span class="pv-meta">
              {{ p.models }} 个内部模型<template v-if="p.accounts"> · {{ p.accounts }} 个账号</template>
              <template v-if="p.stale"> · {{ p.stale }} 个本轮未见</template>
            </span>
          </div>
        </div>
      </div>

      <!-- 第 2 层：上游账号（账单/账本/订阅三处并集；已录订阅的才有固定月费） -->
      <div class="card">
        <div class="card-title">上游账号（{{ accountRows.length }}）</div>
        <div v-if="accountRows.length" class="account-strip">
          <div v-for="a in accountRows" :key="`${a.Provider}/${a.Account}`" class="provider-chip">
            <span class="pv-name">{{ a.Account }}</span>
            <span class="pv-meta">
              {{ a.Provider }}
              <template v-if="a.Subscribed"> · <el-tag type="success" size="small" effect="plain">已录订阅</el-tag></template>
              <template v-if="a.LastSeenAt"> · 最近 {{ fmtDateTime(a.LastSeenAt) }}</template>
            </span>
          </div>
        </div>
        <p v-else class="foot">还没有账号痕迹：账单/账本/订阅都没有该供应商下的账号。</p>
        <p class="foot">
          账号层是现算的（账单 ∪ 账本 ∪ 订阅），没有独立镜像表；「已录订阅」= 该账号有固定月费，参与 A5 分摊。
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
              四层关系（{{ data.Items.length }} 个内部模型 / {{ bindingEdges.length }} 条绑定 /
              {{ data.Externals.length }} 个对外模型）
            </div>
            <VChart :option="canvasOption" height="360px" />
            <p class="foot">
              绿点 = 对外模型，连线 = 绑定（真源 <code>external_models.bound_internal_ref</code>）；
              绑定目标已不在镜像里时画红色虚线（悬空绑定）。坐标来自 <code>model_links</code>（
              <code>PUT /admin/topology/links</code>），缺行时自动布局，不影响售卖与计费。
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
.provider-strip,
.account-strip {
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
