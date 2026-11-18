<script setup lang="ts">
// 拓扑画布：账号 → 供应商（含实际模型）→ 路由组 → 对外模型。
// 只画真实数据（GET /admin/topology）。节点可拖动、可从「模型/路由组」右侧句柄拉线到空白处
// 新建一个绑定该模型的对外模型；点节点在右侧抽屉里查看/编辑（不跳页）。
import { computed } from 'vue'
import { VueFlow, Handle, Position, type Node, type Edge } from '@vue-flow/core'
import { Background } from '@vue-flow/background'
import { Controls } from '@vue-flow/controls'
import '@vue-flow/core/dist/style.css'
import '@vue-flow/core/dist/theme-default.css'
import '@vue-flow/controls/dist/style.css'
import type { InternalModel, InternalModelView } from '@/api/v2'
import { fmtCompact } from '@/utils/format'

const props = defineProps<{ view: InternalModelView; prices?: Record<string, { Input?: number; Output?: number }> }>()
const emit = defineEmits<{ select: [node: { kind: string; ref: string; data: Record<string, unknown> }]; connect: [ref: string] }>()

const X_ACCOUNT = 0
const X_PROVIDER = 200
const X_GROUP = 560
const X_EXTERNAL = 820

interface ModelRow { Ref: string; Name: string; ContextLen: number; Stale: boolean; Sold: number }

function rowsOf(providerRef: string, items: InternalModel[], soldMap: Map<string, number>): ModelRow[] {
  return items
    .filter((m) => m.Kind === 'direct' && m.ProviderRef === providerRef)
    .map((m) => ({
      Ref: m.Ref,
      Name: m.DisplayName || m.Ref,
      ContextLen: m.ContextLen,
      Stale: m.Stale,
      Sold: soldMap.get(m.Ref) || 0,
    }))
}

const soldMap = computed(() => {
  const map = new Map<string, number>()
  for (const e of props.view.Externals) {
    if (!e.BoundInternalRef) continue
    map.set(e.BoundInternalRef, (map.get(e.BoundInternalRef) || 0) + 1)
  }
  return map
})

const groupItems = computed(() => props.view.Items.filter((m) => m.Kind === 'group'))

const graph = computed<{ nodes: Node[]; edges: Edge[] }>(() => {
  const view = props.view
  const nodes: Node[] = []
  const edges: Edge[] = []
  let providerY = 0
  let groupY = 0
  let externalY = 0

  for (const p of view.Providers) {
    if (!p.Provider) continue
    const rows = rowsOf(p.Provider, view.Items, soldMap.value)
    const accounts = view.Accounts.filter((a) => a.Provider === p.Provider)
    const cardHeight = 44 + rows.length * 26 + 8
    const providerId = `p:${p.Provider}`
    nodes.push({
      id: providerId,
      type: 'provider',
      position: { x: X_PROVIDER, y: providerY },
      data: { Ref: p.Provider, Accounts: p.Accounts || accounts.length, StaleCount: p.Stale || 0, Rows: rows },
    })
    accounts.forEach((a, i) => {
      const accId = `a:${a.Provider}:${a.Account}`
      nodes.push({
        id: accId,
        type: 'account',
        position: { x: X_ACCOUNT, y: providerY + 8 + i * 40 },
        data: { Account: a.Account, Provider: a.Provider, Subscribed: a.Subscribed },
      })
      edges.push({ id: `e:${accId}->${providerId}`, source: accId, target: providerId, type: 'smoothstep', animated: true,
        style: { stroke: '#3a4a6b', strokeWidth: 1.2, strokeDasharray: '4 4' } })
    })
    providerY += Math.max(cardHeight, accounts.length * 40 + 10) + 24
  }

  for (const g of groupItems.value) {
    const gid = `g:${g.Ref}`
    nodes.push({
      id: gid,
      type: 'group',
      position: { x: X_GROUP, y: groupY },
      data: { Ref: g.Ref, Name: g.DisplayName || g.Ref, ContextLen: g.ContextLen, Stale: g.Stale, Sold: soldMap.value.get(g.Ref) || 0 },
    })
    groupY += 84
  }

  const externals = [...view.Externals].sort((a, b) => Number(!!a.BoundInternalRef) - Number(!!b.BoundInternalRef))
  for (const e of externals) {
    const xid = `x:${e.ID}`
    const dangling = e.BoundInternalRef !== '' && !e.InternalModelHit
    nodes.push({
      id: xid,
      type: 'external',
      position: { x: X_EXTERNAL, y: externalY },
      data: { ID: e.ID, Name: e.ExternalName, Bound: e.BoundInternalRef, Enabled: e.Enabled, Dangling: dangling },
    })
    externalY += 92

    if (!e.BoundInternalRef || dangling) continue
    const bound = e.BoundInternalRef
    const isGroup = view.Items.some((m) => m.Ref === bound && m.Kind === 'group')
    const providerRef = view.Items.find((m) => m.Ref === bound)?.ProviderRef || ''
    const source = isGroup ? `g:${bound}` : `p:${providerRef}`
    if (!nodes.some((n) => n.id === source)) continue
    edges.push({
      id: `e:${source}:${bound}->${xid}`,
      source,
      sourceHandle: isGroup ? undefined : `m:${bound}`,
      target: xid,
      type: 'smoothstep',
      animated: true,
      style: { stroke: '#5f9e6e', strokeWidth: 1.4, strokeDasharray: '5 4' },
    })
  }
  return { nodes, edges }
})

let justConnected = false
function pick(kind: string, ref: string, data: Record<string, unknown>) {
  if (justConnected) return // 拖线落在目标节点上时，不要顺带打开抽屉
  emit('select', { kind, ref, data })
}
function onConnectEvt(e: { source: string; sourceHandle?: string | null }) {
  if (!e.source) return
  justConnected = true
  setTimeout(() => (justConnected = false), 350)
  const ref = (e.sourceHandle || '').replace(/^m:/, '') || e.source.replace(/^[a-z]:/, '')
  emit('connect', ref)
}
</script>

<template>
  <div class="canvas-wrap">
    <VueFlow
      :nodes="graph.nodes"
      :edges="graph.edges"
      :min-zoom="0.25"
      :max-zoom="1.5"
      :nodes-draggable="true"
      :nodes-connectable="true"
      :elements-selectable="true"
      fit-view-on-init
      class="canvas"
      @connect="onConnectEvt"
    >
      <Background pattern-color="#1b2331" :gap="18" :size="1.2" />
      <Controls />

      <template #node-account="p">
        <div class="nd nd-account" @click="pick('account', p.data.Account, p.data)">
          <Handle type="source" :position="Position.Right" />
          <div class="nd-tag">账号</div>
          <div class="nd-title">{{ p.data.Account }}</div>
          <div class="nd-sub"><span class="chip" :class="p.data.Subscribed ? 'chip-plan' : ''">{{ p.data.Subscribed ? '订阅' : '按量' }}</span></div>
        </div>
      </template>

      <template #node-provider="p">
        <div class="nd nd-provider">
          <Handle type="target" :position="Position.Left" />
          <div class="pv-head" @click="pick('provider', p.data.Ref, p.data)">
            <span class="nd-title">{{ p.data.Ref }}</span>
            <span class="txt">{{ p.data.Accounts ? p.data.Accounts + ' 账号' : '账号待同步' }}<template v-if="p.data.StaleCount"> · 未见 {{ p.data.StaleCount }}</template></span>
          </div>
          <div v-for="r in p.data.Rows" :key="r.Ref" class="pv-row" :class="{ 'is-stale': r.Stale }">
            <span class="pv-row-name" @click="pick('model', r.Ref, r)">{{ r.Name }}</span>
            <span class="txt">{{ r.ContextLen ? fmtCompact(r.ContextLen) : '—' }}</span>
            <span class="chip" :class="r.Sold ? 'chip-sold' : 'chip-idle'">{{ r.Sold ? '×' + r.Sold : '未售' }}</span>
            <Handle :id="`m:${r.Ref}`" type="source" :position="Position.Right" />
          </div>
        </div>
      </template>

      <template #node-group="p">
        <div class="nd nd-group" :class="{ 'is-stale': p.data.Stale }" @click="pick('group', p.data.Ref, p.data)">
          <Handle type="target" :position="Position.Left" />
          <div class="nd-tag">组</div>
          <div class="nd-title">{{ p.data.Name }}</div>
          <div class="nd-sub"><span class="chip" :class="p.data.Sold ? 'chip-sold' : 'chip-idle'">{{ p.data.Sold ? '×' + p.data.Sold : '未售' }}</span></div>
          <Handle type="source" :position="Position.Right" />
        </div>
      </template>

      <template #node-external="p">
        <div
          class="nd nd-external"
          :class="{ 'is-off': p.data.Enabled === false, 'is-dangling': p.data.Dangling }"
          @click="pick('external', p.data.ID, p.data)"
        >
          <Handle type="target" :position="Position.Left" />
          <div class="nd-title">{{ p.data.Name || '(未命名)' }}</div>
          <div class="nd-sub">
            <span v-if="p.data.Dangling" class="chip chip-warn">悬空</span>
            <span v-else-if="p.data.Enabled === false" class="chip chip-idle">停用</span>
            <span v-else class="chip chip-sold">在售</span>
            <span v-if="p.data.In != null" class="price">入 {{ p.data.In }} / 出 {{ p.data.Out }}</span>
          </div>
        </div>
      </template>
    </VueFlow>
  </div>
</template>

<style scoped>
.canvas-wrap { height: 100%; min-height: 420px; }
.canvas { height: 100%; background: var(--bg); }

.nd { background: var(--panel); border: 1px solid var(--line); border-radius: 9px; padding: 8px 10px; color: var(--ink); font-size: 12px; cursor: pointer; }
.nd:hover { border-color: var(--line-2); }
.nd-account { width: 150px; }
.nd-tag { font-size: 10px; color: var(--ink-4); letter-spacing: .6px; }
.nd-title { font-size: 12.5px; font-weight: 500; }
.nd-sub { margin-top: 3px; }
.txt { color: var(--ink-3); font-size: 11px; }
.nd-provider { width: 330px; padding: 8px 10px; }
.pv-head { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; padding-bottom: 6px; border-bottom: 1px solid var(--line); }
.pv-row { position: relative; display: flex; align-items: center; gap: 8px; height: 26px; border-bottom: 1px dashed #141b26; }
.pv-row:last-child { border-bottom: 0; }
.pv-row-name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.is-stale { opacity: .5; }
.nd-group { width: 200px; }
.nd-external { width: 180px; }
.is-off { opacity: .6; }
.is-dangling { border-color: #443718; }
.chip { display: inline-flex; align-items: center; height: 16px; padding: 0 6px; border-radius: 8px; font-size: 10px; border: 1px solid #2a3346; color: var(--ink-2); }
.chip-sold { border-color: #26402f; color: #8fbf9f; background: rgba(95,158,110,.08); }
.chip-idle { color: var(--ink-3); }
.chip-plan { border-color: #2d3d68; color: #a8bdf5; background: rgba(76,111,232,.1); }
.price { margin-left: 6px; color: var(--ink-3); font-size: 10.5px; font-variant-numeric: tabular-nums; }
.chip-warn { border-color: #443718; color: #d6bd80; background: rgba(201,162,39,.08); }
:deep(.vue-flow__handle) { width: 7px; height: 7px; background: #2a3346; border: 1px solid #3a4a6b; }
:deep(.vue-flow__edge-path) { stroke-linecap: round; }
</style>
