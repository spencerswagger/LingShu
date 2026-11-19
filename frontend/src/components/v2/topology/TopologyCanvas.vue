<script setup lang="ts">
// 拓扑画布：账号 → 供应商(卡内模型) → 路由组/Auto → 对外模型。
// 账号/模型/组 取自网关原样视图（gw）；对外模型与绑定取自灵枢（view）。
// 拖线：从模型行或组右侧拖到空白 = 新建绑定该模型/组的对外模型；拖到已有对外模型 = 直接绑定。
import { computed } from 'vue'
import { VueFlow, Handle, Position, type Node, type Edge } from '@vue-flow/core'
import { Background } from '@vue-flow/background'
import { Controls } from '@vue-flow/controls'
import '@vue-flow/core/dist/style.css'
import '@vue-flow/core/dist/theme-default.css'
import '@vue-flow/controls/dist/style.css'
import type { InternalModelView, GatewayView, GwModel } from '@/api/v2'
import { fmtCompact } from '@/utils/format'

const props = defineProps<{
  view: InternalModelView
  gw: GatewayView | null
  prices?: Record<string, { Input?: number; Output?: number }>
}>()
const emit = defineEmits<{
  select: [node: { kind: string; ref: string; data: Record<string, unknown> }]
  connect: [payload: { ref: string; target?: string | null }]
  bind: [payload: { ref: string; externalId: string }]
}>()

const X_ACCOUNT = 0
const X_PROVIDER = 240
const X_GROUP = 720
const X_EXTERNAL = 1010

function shortModel(m: GwModel) {
  return { id: m.id, name: m.name, provider: m.provider, context: m.context || 0 }
}

const graph = computed<{ nodes: Node[]; edges: Edge[] }>(() => {
  const nodes: Node[] = []
  const edges: Edge[] = []
  const gw = props.gw
  const externals = props.view?.Externals || []

  // 没有网关视图时：只画对外模型（未绑定），避免白屏
  if (!gw) {
    let y = 0
    for (const e of externals) {
      nodes.push({ id: `x:${e.ID}`, type: 'external', position: { x: X_EXTERNAL, y }, data: { ID: e.ID, Name: e.ExternalName, Bound: e.BoundInternalRef, Enabled: e.Enabled, In: props.prices?.[e.ID]?.Input, Out: props.prices?.[e.ID]?.Output } })
      y += 92
    }
    return { nodes, edges }
  }

  // 供应商（按 models 出现顺序）
  const providerOrder: string[] = []
  const byProvider = new Map<string, GwModel[]>()
  const providerName: Record<string, string> = {}
  for (const m of gw.models) {
    if (!byProvider.has(m.provider)) {
      byProvider.set(m.provider, [])
      providerOrder.push(m.provider)
      providerName[m.provider] = m.providerName || m.provider
    }
    byProvider.get(m.provider)!.push(m)
  }

  // 账号：按供应商归组
  const acctByProvider = new Map<string, { name: string; plan?: string }[]>()
  for (const [, list] of Object.entries(gw.accounts || {})) {
    for (const a of list) {
      if (!acctByProvider.has(a.provider)) acctByProvider.set(a.provider, [])
      acctByProvider.get(a.provider)!.push({ name: a.name, plan: a.plan })
    }
  }

  let providerY = 0
  for (const p of providerOrder) {
    const models = byProvider.get(p)!
    const accts = acctByProvider.get(p) || []
    const pid = `p:${p}`
    const cardH = 44 + models.length * 26 + 8
    nodes.push({
      id: pid,
      type: 'provider',
      position: { x: X_PROVIDER, y: providerY },
      data: { Ref: p, Name: providerName[p], Accounts: accts.length, Rows: models.map(shortModel) },
    })
    accts.forEach((a, i) => {
      const aid = `a:${p}:${a.name}`
      nodes.push({ id: aid, type: 'account', position: { x: X_ACCOUNT, y: providerY + 8 + i * 38 }, data: { Account: a.name, Provider: providerName[p], Plan: a.plan } })
      edges.push({ id: `e:${aid}->${pid}`, source: aid, target: pid, type: 'smoothstep', animated: true, style: { stroke: '#2f3d58', strokeWidth: 1, strokeDasharray: '3 5' } })
    })
    providerY += Math.max(cardH, accts.length * 38 + 10) + 26
  }

  // 路由组 / Auto
  let groupY = 0
  const groupRefs = new Set<string>()
  for (const g of gw.groups) {
    const gid = `g:${g.ref}`
    groupRefs.add(g.ref)
    nodes.push({
      id: gid,
      type: 'group',
      position: { x: X_GROUP, y: groupY },
      data: { Ref: g.ref, Name: g.name, Auto: !!g.auto, Members: g.members.length, Sold: 0 },
    })
    // 成员模型 → 组
    for (const mid of g.members) {
      const prov = mid.split('/')[0]
      if (!byProvider.has(prov)) continue
      edges.push({
        id: `e:${mid}->${gid}`,
        source: `p:${prov}`,
        sourceHandle: `m:${mid}`,
        target: gid,
        type: 'smoothstep',
        animated: true,
        style: { stroke: g.auto ? '#6b4fa8' : '#3a4a6b', strokeWidth: 1.1, strokeDasharray: '4 4' },
      })
    }
    groupY += 92
  }

  // 对外模型
  let extY = 0
  for (const e of externals) {
    const xid = `x:${e.ID}`
    const bound = e.BoundInternalRef || ''
    const dang = bound !== '' && !groupRefs.has(bound) && !byProvider.has(bound.split('/')[0])
    nodes.push({
      id: xid,
      type: 'external',
      position: { x: X_EXTERNAL, y: extY },
      data: { ID: e.ID, Name: e.ExternalName, Bound: bound, Enabled: e.Enabled, Dangling: dang, In: props.prices?.[e.ID]?.Input, Out: props.prices?.[e.ID]?.Output },
    })
    extY += 92
    if (!bound || dang) continue
    if (groupRefs.has(bound)) {
      edges.push({ id: `e:${bound}->${xid}`, source: `g:${bound}`, target: xid, type: 'smoothstep', animated: true, style: { stroke: '#5f9e6e', strokeWidth: 1.4, strokeDasharray: '5 4' } })
    } else {
      const prov = bound.split('/')[0]
      if (byProvider.has(prov)) {
        edges.push({ id: `e:${bound}->${xid}`, source: `p:${prov}`, sourceHandle: `m:${bound}`, target: xid, type: 'smoothstep', animated: true, style: { stroke: '#5f9e6e', strokeWidth: 1.4, strokeDasharray: '5 4' } })
      }
    }
  }

  return { nodes, edges }
})

let justConnected = false
function pick(kind: string, ref: string, data: Record<string, unknown>) {
  if (justConnected) return
  emit('select', { kind, ref, data })
}
function onConnectEvt(e: { source: string; sourceHandle?: string | null; target?: string | null }) {
  if (!e.source) return
  justConnected = true
  setTimeout(() => (justConnected = false), 350)
  const ref = (e.sourceHandle || '').replace(/^m:/, '') || e.source.replace(/^[a-z]:/, '')
  if (e.target && e.target.startsWith('x:')) {
    emit('bind', { ref, externalId: e.target.slice(2) })
    return
  }
  emit('connect', { ref, target: e.target })
}
</script>

<template>
  <div class="canvas-wrap">
    <VueFlow :nodes="graph.nodes" :edges="graph.edges" :min-zoom="0.2" :max-zoom="1.5"
      :nodes-draggable="true" :nodes-connectable="true" :elements-selectable="true" fit-view-on-init class="canvas"
      @connect="onConnectEvt">
      <Background pattern-color="#161d29" :gap="20" :size="1.1" />
      <Controls />

      <template #node-account="p">
        <div class="nd nd-account" @click="pick('account', p.data.Account, p.data)">
          <Handle type="source" :position="Position.Right" />
          <div class="nd-title">{{ p.data.Account }}</div>
          <div class="nd-sub"><span class="txt">{{ p.data.Plan || p.data.Provider }}</span></div>
        </div>
      </template>

      <template #node-provider="p">
        <div class="nd nd-provider">
          <Handle type="target" :position="Position.Left" />
          <div class="pv-head" @click="pick('provider', p.data.Ref, p.data)">
            <span class="nd-title">{{ p.data.Name }}</span>
            <span class="txt">{{ p.data.Accounts }} 账号</span>
          </div>
          <div v-for="r in p.data.Rows" :key="r.id" class="pv-row">
            <span class="pv-row-name" @click="pick('model', r.id, r)">{{ r.name }}</span>
            <span class="txt">{{ r.context ? fmtCompact(r.context) : '—' }}</span>
            <Handle :id="`m:${r.id}`" type="source" :position="Position.Right" />
          </div>
        </div>
      </template>

      <template #node-group="p">
        <div class="nd nd-group" :class="{ 'is-auto': p.data.Auto }" @click="pick('group', p.data.Ref, p.data)">
          <Handle type="target" :position="Position.Left" />
          <div class="nd-head"><span class="nd-tag">{{ p.data.Auto ? 'AUTO' : '组' }}</span><span class="txt">{{ p.data.Members }} 成员</span></div>
          <div class="nd-title">{{ p.data.Name }}</div>
          <Handle type="source" :position="Position.Right" />
        </div>
      </template>

      <template #node-external="p">
        <div class="nd nd-external" :class="{ 'is-dangling': p.data.Dangling }" @click="pick('external', p.data.ID, p.data)">
          <Handle type="target" :position="Position.Left" />
          <div class="nd-title">{{ p.data.Name || '(未命名)' }}</div>
          <div class="nd-sub">
            <span class="chip" :class="p.data.Enabled === false ? 'chip-idle' : 'chip-sold'">{{ p.data.Enabled === false ? '停用' : '在售' }}</span>
            <span v-if="p.data.In != null" class="price">入 {{ p.data.In }} / 出 {{ p.data.Out }}</span>
          </div>
        </div>
      </template>
    </VueFlow>
  </div>
</template>

<style scoped>
.canvas-wrap { height: 100%; min-height: 480px; }
.canvas { height: 100%; background: transparent; }
.nd { background: linear-gradient(175deg, #151a27, #0b0f18); border: 1px solid #2c3a52; border-radius: 10px; padding: 8px 10px; color: var(--ink); font-size: 12px; cursor: pointer; }
.nd:hover { border-color: #3d5170; }
.nd-account { width: 190px; }
.nd-provider { width: 430px; }
.nd-group { width: 240px; border-color: #3a3560; }
.nd-group.is-auto { border-style: dashed; border-color: #5b47a8; }
.nd-head { display: flex; align-items: center; justify-content: space-between; }
.nd-tag { font-size: 9.5px; letter-spacing: .8px; color: #a78bfa; }
.nd-group.is-auto .nd-tag { color: #c4b5fd; }
.nd-title { font-size: 12.5px; font-weight: 500; }
.nd-sub { margin-top: 3px; }
.txt { color: var(--ink-3); font-size: 11px; }
.pv-head { display: flex; align-items: baseline; justify-content: space-between; gap: 8px; padding-bottom: 6px; border-bottom: 1px solid #1b2331; }
.pv-row { position: relative; display: flex; align-items: center; gap: 8px; height: 26px; border-bottom: 1px dashed #141b26; padding-right: 10px; }
.pv-row:last-child { border-bottom: 0; }
.pv-row-name { flex: 1; min-width: 0; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.nd-external { width: 220px; }
.is-dangling { border-color: #443718; }
.chip { display: inline-flex; align-items: center; height: 16px; padding: 0 6px; border-radius: 8px; font-size: 10px; border: 1px solid #2a3346; color: var(--ink-2); }
.chip-sold { border-color: #26402f; color: #8fbf9f; background: rgba(95,158,110,.08); }
.chip-idle { color: var(--ink-3); }
.price { margin-left: 6px; color: var(--ink-3); font-size: 10.5px; font-variant-numeric: tabular-nums; }
:deep(.vue-flow__handle) { width: 7px; height: 7px; background: #2a3346; border: 1px solid #3a4a6b; }
</style>
