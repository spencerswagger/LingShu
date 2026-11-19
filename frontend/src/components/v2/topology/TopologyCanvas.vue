<script setup lang="ts">
// 拓扑画布：账号 → 供应商(卡内模型) → 路由组/Auto → 对外模型。
// 账号/模型/组 取自网关原样视图（gw）；对外模型与绑定取自灵枢（view）。
// 拖线：从模型行或组右侧拖到空白 = 新建绑定该模型/组的对外模型；拖到已有对外模型 = 直接绑定。
import { computed } from 'vue'
import { VueFlow, type Node, type Edge } from '@vue-flow/core'
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
}>()

const X_ACCOUNT = 0
const X_PROVIDER = 220
const X_GROUP = 640
const X_EXTERNAL = 980

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
      data: { Ref: p, Name: providerName[p], Accounts: accts.length, Rows: models.map(shortModel) }, connectable: true,
    })
    accts.forEach((a, i) => {
      const aid = `a:${p}:${a.name}`
      nodes.push({ id: aid, type: 'account', position: { x: X_ACCOUNT, y: providerY + 8 + i * 42 }, data: { Account: a.name, Provider: providerName[p], Plan: a.plan }, connectable: false })
      edges.push({ id: `e:${aid}->${pid}`, source: aid, target: pid, type: 'default', animated: true, style: { stroke: '#2f3d58', strokeWidth: 1, strokeDasharray: '3 5' } })
    })
    providerY += Math.max(cardH, accts.length * 42 + 12) + 34
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
      data: { Ref: g.ref, Name: g.name, Auto: !!g.auto, Members: g.members.length, Sold: 0 }, connectable: true,
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
        type: 'default',
        animated: true,
        style: { stroke: g.auto ? '#5b47a8' : '#33415e', strokeWidth: 1.1 },
      })
    }
    groupY += 84
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
      data: { ID: e.ID, Name: e.ExternalName, Bound: bound, Enabled: e.Enabled, Dangling: dang, In: props.prices?.[e.ID]?.Input, Out: props.prices?.[e.ID]?.Output }, connectable: false,
    })
    extY += 96
    if (!bound || dang) continue
    if (groupRefs.has(bound)) {
      edges.push({ id: `e:${bound}->${xid}`, source: `g:${bound}`, target: xid, type: 'default', animated: true, style: { stroke: '#5f9e6e', strokeWidth: 1.4 } })
    } else {
      const prov = bound.split('/')[0]
      if (byProvider.has(prov)) {
        edges.push({ id: `e:${bound}->${xid}`, source: `p:${prov}`, sourceHandle: `m:${bound}`, target: xid, type: 'default', animated: true, style: { stroke: '#5f9e6e', strokeWidth: 1.4 } })
      }
    }
  }

  return { nodes, edges }
})

function pick(kind: string, ref: string, data: Record<string, unknown>) {
  emit('select', { kind, ref, data })
}
</script>

<template>
  <div class="canvas-wrap">
    <VueFlow :nodes="graph.nodes" :edges="graph.edges" :min-zoom="0.2" :max-zoom="1.5"
      :elements-selectable="true"
      fit-view-on-init class="canvas">
      <Background pattern-color="#141b28" :gap="22" :size="1" />
      <Controls />

      <template #node-account="p">
        <div class="n acct" @click="pick('account', p.data.Account, p.data)">
          <div class="ic"><svg viewBox="0 0 16 16" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"><circle cx="8" cy="5.4" r="2.6"></circle><path d="M2.8 13.6c.6-2.6 2.7-4 5.2-4s4.6 1.4 5.2 4"></path></svg></div>
          <div class="bd">
            <div class="hd"><b>{{ p.data.Account }}</b><span class="ro">只读</span></div>
            <div class="sub"><span>{{ p.data.Provider }}</span><span v-if="p.data.Plan">· {{ p.data.Plan }}</span></div>
          </div>
        </div>
      </template>

      <template #node-provider="p">
        <div class="prov">
          <div class="prov-hd" @click="pick('provider', p.data.Ref, p.data)">
            <div class="ic"><svg viewBox="0 0 16 16" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"><circle cx="8" cy="8" r="5.6"></circle><path d="M2.4 8h11.2M8 2.4c1.8 1.8 1.8 9.4 0 11.2M8 2.4c-1.8 1.8-1.8 9.4 0 11.2"></path></svg></div>
            <div class="ti"><b>{{ p.data.Name }}</b><span>{{ p.data.Accounts }} 账号</span></div>
            <span class="ro">只读</span>
          </div>
          <div class="rows">
            <div v-for="r in p.data.Rows" :key="r.id" class="row">
              <span class="rn"><svg viewBox="0 0 16 16" width="11" height="11" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"><rect x="4.4" y="4.4" width="7.2" height="7.2" rx="1.5"></rect><path d="M6.4 1.8v1.8M9.6 1.8v1.8M6.4 12.4v1.8M9.6 12.4v1.8"></path></svg></span>
              <b @click="pick('model', r.id, r)">{{ r.name }}</b>
              <em>{{ r.context ? fmtCompact(r.context) : '—' }}</em>
            </div>
          </div>
        </div>
      </template>

      <template #node-group="p">
        <div class="n grp" @click="pick('group', p.data.Ref, p.data)">
          <div class="ic"><svg viewBox="0 0 16 16" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"><path d="M2 4.4h3.6M8.2 4.4H14"></path><circle cx="6.9" cy="4.4" r="1.5"></circle><path d="M2 11.6h4.4M8.9 11.6H14"></path><circle cx="7.7" cy="11.6" r="1.5"></circle></svg></div>
          <div class="bd">
            <div class="hd"><b>{{ p.data.Name }}</b><span class="bg group">{{ p.data.Auto ? 'AUTO' : '路由组' }}</span></div>
            <div class="sub"><span>{{ p.data.Members }} 个成员</span></div>
          </div>
        </div>
      </template>

      <template #node-external="p">
        <div class="n ext" :class="{ off: p.data.Enabled === false, dashed: p.data.Dangling }" @click="pick('external', p.data.ID, p.data)">
          <div class="ic"><svg viewBox="0 0 16 16" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"><path d="M7.6 2H2.6A.6.6 0 0 0 2 2.6v10.8a.6.6 0 0 0 .6.6h10.8a.6.6 0 0 0 .6-.6V8.4"></path><path d="M9.4 2h4.6v4.6"></path><path d="M14 2 7.8 8.2"></path></svg></div>
          <div class="bd">
            <div class="hd"><b>{{ p.data.Name || '(未命名)' }}</b><span :class="p.data.Enabled === false ? 'warn' : 'ok'">{{ p.data.Enabled === false ? '停用' : '在售' }}</span></div>
            <div class="sub"><span>i/o {{ p.data.In ?? 0 }} / {{ p.data.Out ?? 0 }}</span><span v-if="p.data.Dangling" class="warn">绑定悬空</span></div>
          </div>
        </div>
      </template>
    </VueFlow>
  </div>
</template>

<style scoped>
.canvas-wrap { height: 100%; min-height: 480px; }
.canvas { height: 100%; background: radial-gradient(120% 90% at 76% -12%, #101a30, #080c17 56%, #06080f); }
.n { display: flex; align-items: center; gap: 8px; padding: 7px 10px; border-radius: 9px; border: 1px solid #1d2534; background: linear-gradient(175deg, #101622, #0a0e17); color: #a2adc6; cursor: pointer; }
.n .ic { flex: none; width: 20px; height: 20px; border-radius: 5px; display: flex; align-items: center; justify-content: center; border: 1px solid #253049; background: #111828; color: #7f93b8; }
.n .bd { min-width: 0; flex: 1; }
.n .hd { display: flex; align-items: center; gap: 6px; }
.n .hd b { font-size: 11.8px; font-weight: 600; color: #dbe2ef; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.n .sub { margin-top: 1px; display: flex; gap: 7px; font-size: 10.4px; color: #65708a; white-space: nowrap; }
.n .ro { flex: none; font-size: 9px; letter-spacing: .5px; color: #5a6377; border: 1px solid #232c3e; border-radius: 7px; padding: 0 4px; }
.n .hd .ok { color: #8fbf9f; font-size: 9.5px; }
.n .hd .warn { color: #c9a227; font-size: 9.5px; }
.n .bg { flex: none; height: 15px; padding: 0 5px; border-radius: 8px; font-size: 9.5px; display: inline-flex; align-items: center; border: 1px solid #26402f; color: #8fbf9f; }
.n .bg.group { border-color: #3a3358; color: #b3a4f0; background: #a78bfa17; }
.acct { width: 186px; }
.acct .ic { color: #8d9cc0; }
.grp { width: 268px; }
.grp .ic { color: #9d8be0; border-color: #2f3750; background: #14182a; }
.ext { width: 220px; border-color: #1f2a3b; background: linear-gradient(175deg, #0f1421, #090d15); }
.ext .ic { color: #7fbf95; border-color: #2a3b31; background: #101a16; }
.ext.off { opacity: .62; border-style: dashed; }
.ext.dashed { border-color: #4a3d18; }
.prov { width: 300px; border-radius: 11px; border: 1px solid #22304a; background: linear-gradient(175deg, #111a2bf7, #090e18fa); box-shadow: inset 0 1px #ffffff08, 0 20px 44px -34px #4c6fe8e6; }
.prov-hd { display: flex; align-items: center; gap: 8px; padding: 8px 10px 6px; cursor: pointer; }
.prov-hd .ic { flex: none; width: 21px; height: 21px; border-radius: 5px; display: flex; align-items: center; justify-content: center; border: 1px solid #2b3a55; background: #121b2c; color: #88a8d4; }
.prov-hd .ti { min-width: 0; flex: 1; }
.prov-hd b { display: block; font-size: 12px; font-weight: 600; color: #dbe2ef; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; }
.prov-hd .ti span { font-size: 10px; color: #65708a; }
.prov-hd .ro { flex: none; font-size: 9px; letter-spacing: .5px; color: #5a6377; border: 1px solid #232c3e; border-radius: 7px; padding: 0 4px; }
.prov .rows { display: flex; flex-direction: column; gap: 4px; padding: 0 8px 8px; }
.prov .row { position: relative; display: flex; align-items: center; gap: 6px; padding: 5px 9px; border-radius: 7px; border: 1px solid #1c2433; background: #0c111c; font-size: 11px; color: #c3ccdd; }
.prov .row .rn { display: flex; flex: none; color: #8d7fd0; }
.prov .row b { flex: 1; min-width: 0; font-weight: 500; overflow: hidden; text-overflow: ellipsis; white-space: nowrap; cursor: pointer; }
.prov .row em { font-style: normal; font-family: ui-monospace, Menlo, monospace; font-size: 9.5px; color: #5f6a80; }
:deep(.vue-flow__handle) { width: 6px; height: 6px; background: #2a3346; border: 1px solid #3a4a6b; }
:deep(.vue-flow__edge-path) { stroke-linecap: round; }
</style>
