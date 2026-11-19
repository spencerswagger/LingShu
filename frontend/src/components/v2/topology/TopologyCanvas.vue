<script setup lang="ts">
// 拓扑画布（V3 只读）：账号 → 供应商（卡内模型）→ 路由组/Auto（对外可调用）。
// 右列就是可调用的路由组（客户端直接请求组名）；不再有单独的「对外模型」节点，
// 组节点上显示售价（灵枢 external_models 绑定到组 ref 的定价）。
import { computed } from 'vue'
import { VueFlow, Handle, Position, type Node, type Edge } from '@vue-flow/core'
import { Background } from '@vue-flow/background'
import { Controls } from '@vue-flow/controls'
import '@vue-flow/core/dist/style.css'
import '@vue-flow/core/dist/theme-default.css'
import '@vue-flow/controls/dist/style.css'
import type { InternalModelView, GatewayView, GwModel } from '@/api/v2'
import { fmtCompact } from '@/utils/format'
import FlowEdgeParticle from './FlowEdgeParticle.vue'

const props = defineProps<{
  view: InternalModelView
  gw: GatewayView | null
  // groupPrice: 组 ref → 售价（外部模型 BoundInternalRef == 组 ref）
  groupPrice?: Record<string, { In: number; Out: number } | null>
}>()
const emit = defineEmits<{ select: [node: { kind: string; ref: string; data: Record<string, unknown> }] }>()

// 三列坐标（对齐原型）：账号 20 / 供应商 250 / 路由组 600；嵌套组再往右一列。
const X_ACCOUNT = 20
const X_PROVIDER = 250
const X_GROUP = 600
const X_NESTED_STEP = 320
const EDGE = 'particle' as const

function shortModel(m: GwModel) {
  return { id: m.id, name: m.name, provider: m.provider, context: m.context || 0 }
}

const graph = computed<{ nodes: Node[]; edges: Edge[] }>(() => {
  const nodes: Node[] = []
  const edges: Edge[] = []
  const gw = props.gw

  if (!gw) return { nodes, edges }

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

  const acctByProvider = new Map<string, { name: string; plan?: string }[]>()
  for (const [, list] of Object.entries(gw.accounts || {})) {
    for (const a of list) {
      if (!acctByProvider.has(a.provider)) acctByProvider.set(a.provider, [])
      acctByProvider.get(a.provider)!.push({ name: a.name, plan: a.plan })
    }
  }

  let providerY = 0
  const providerRight = new Map<string, number>()
  for (const p of providerOrder) {
    const models = byProvider.get(p)!
    const accts = acctByProvider.get(p) || []
    const pid = `p:${p}`
    const cardH = 44 + models.length * 28 + 8
    nodes.push({
      id: pid, type: 'provider', connectable: false,
      position: { x: X_PROVIDER, y: providerY },
      data: { Ref: p, Name: providerName[p], Accounts: accts.length, Rows: models.map(shortModel) },
    })
    providerRight.set(p, providerY + cardH)
    accts.forEach((a, i) => {
      const aid = `a:${p}:${a.name}`
      nodes.push({ id: aid, type: 'account', connectable: false, position: { x: X_ACCOUNT, y: providerY + 8 + i * 44 }, data: { Account: a.name, Provider: providerName[p], Plan: a.plan } })
      edges.push({ id: `e:${aid}->${pid}`, source: aid, sourceHandle: 'out', target: pid, targetHandle: 'in', type: EDGE, animated: true, style: { stroke: '#2f3d58', strokeWidth: 1 } })
    })
    providerY += Math.max(cardH, accts.length * 44 + 12) + 34
  }

  // 右列：路由组 / Auto（可直接调用）。hidden 组不渲染（节点与边一并不出）。
  const visible = gw.groups.filter((g) => !g.hidden)
  const groupRefs = new Set(visible.map((g) => g.ref))
  // 嵌套组：被其它组当成员引用的组，往右挪一列；深度 = 1 + 最深父组。
  const parentOf = new Map<string, string[]>()
  for (const g of visible) {
    for (const mid of g.members) {
      if (groupRefs.has(mid)) {
        if (!parentOf.has(mid)) parentOf.set(mid, [])
        parentOf.get(mid)!.push(g.ref)
      }
    }
  }
  const depthOf = new Map<string, number>()
  const depthOfRef = (ref: string, seen: Set<string>): number => {
    if (depthOf.has(ref)) return depthOf.get(ref)!
    if (seen.has(ref)) return 0 // 防环
    seen.add(ref)
    const parents = parentOf.get(ref) || []
    const d = parents.length ? Math.max(...parents.map((r) => depthOfRef(r, seen))) + 1 : 0
    depthOf.set(ref, d)
    return d
  }
  for (const g of visible) depthOfRef(g.ref, new Set())

  // 第 0 列组按纵向排；每个更深的嵌套列各自纵向排。
  let groupY = 0
  const nestedY = new Map<number, number>()
  for (const g of visible) {
    const gid = `g:${g.ref}`
    const price = props.groupPrice?.[g.ref] ?? null
    const d = depthOf.get(g.ref) || 0
    let y: number
    if (d === 0) {
      y = groupY
      groupY += 92
    } else {
      y = nestedY.get(d) || 0
      nestedY.set(d, y + 92)
    }
    nodes.push({
      id: gid, type: 'group', connectable: false,
      position: { x: X_GROUP + X_NESTED_STEP * d, y },
      data: { Ref: g.ref, Name: g.name, Auto: !!g.auto, Members: g.members.length, Price: price },
    })
    // 成员模型 → 组（从供应商卡的模型行右侧连到组左侧）；成员若是组（嵌套），父组 → 子组连线
    for (const mid of g.members) {
      if (groupRefs.has(mid)) {
        edges.push({
          id: `e:${gid}->g:${mid}`, source: gid, sourceHandle: 'out',
          target: `g:${mid}`, targetHandle: 'in', type: EDGE, animated: true,
          style: { stroke: '#4a4472', strokeWidth: 1.1, strokeDasharray: '4 3' },
        })
        continue
      }
      const prov = mid.split('/')[0]
      if (!byProvider.has(prov)) continue
      edges.push({
        id: `e:${mid}->${gid}`, source: `p:${prov}`, sourceHandle: `m:${mid}`,
        target: gid, targetHandle: 'in', type: EDGE, animated: true,
        style: { stroke: g.auto ? '#5b47a8' : '#33415e', strokeWidth: 1.1 },
      })
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
      :elements-selectable="true" fit-view-on-init class="canvas">
      <Background pattern-color="#141b28" :gap="22" :size="1" />
      <Controls />

      <template #edge-particle="p">
        <FlowEdgeParticle v-bind="p" />
      </template>

      <template #node-account="p">
        <div class="n acct" @click="pick('account', p.data.Account, p.data)">
          <div class="ic"><svg viewBox="0 0 16 16" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"><circle cx="8" cy="5.4" r="2.6"></circle><path d="M2.8 13.6c.6-2.6 2.7-4 5.2-4s4.6 1.4 5.2 4"></path></svg></div>
          <div class="bd">
            <div class="hd"><b>{{ p.data.Account }}</b><span class="ro">只读</span></div>
            <div class="sub"><span>{{ p.data.Provider }}</span><span v-if="p.data.Plan">· {{ p.data.Plan }}</span></div>
          </div>
          <Handle type="source" :position="Position.Right" id="out" />
        </div>
      </template>

      <template #node-provider="p">
        <div class="prov">
          <Handle type="target" :position="Position.Left" id="in" />
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
              <Handle :id="`m:${r.id}`" type="source" :position="Position.Right" />
            </div>
          </div>
        </div>
      </template>

      <template #node-group="p">
        <div class="n grp" @click="pick('group', p.data.Ref, p.data)">
          <Handle type="target" :position="Position.Left" id="in" />
          <Handle type="source" :position="Position.Right" id="out" />
          <div class="ic"><svg viewBox="0 0 16 16" width="12" height="12" fill="none" stroke="currentColor" stroke-width="1.3" stroke-linecap="round"><path d="M2 4.4h3.6M8.2 4.4H14"></path><circle cx="6.9" cy="4.4" r="1.5"></circle><path d="M2 11.6h4.4M8.9 11.6H14"></path><circle cx="7.7" cy="11.6" r="1.5"></circle></svg></div>
          <div class="bd">
            <div class="hd"><b>{{ p.data.Name }}</b><span class="bg group">{{ p.data.Auto ? 'AUTO' : '路由组' }}</span></div>
            <div class="sub">
              <span>{{ p.data.Members }} 成员</span>
              <span v-if="p.data.Price" class="priced">i/o {{ p.data.Price.In }} / {{ p.data.Price.Out }}</span>
              <span v-else class="unpriced">未定价</span>
            </div>
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
.n .bg { flex: none; height: 15px; padding: 0 5px; border-radius: 8px; font-size: 9.5px; display: inline-flex; align-items: center; border: 1px solid #26402f; color: #8fbf9f; }
.n .bg.group { border-color: #3a3358; color: #b3a4f0; background: #a78bfa17; }
.acct { width: 210px; }
.acct .ic { color: #8d9cc0; }
.grp { width: 300px; }
.grp .ic { color: #9d8be0; border-color: #2f3750; background: #14182a; }
.priced { color: #8fbf9f; font-family: ui-monospace, Menlo, monospace; }
.unpriced { color: #c9a227; }
.prov { width: 268px; border-radius: 11px; border: 1px solid #22304a; background: linear-gradient(175deg, #111a2bf7, #090e18fa); box-shadow: inset 0 1px #ffffff08, 0 20px 44px -34px #4c6fe8e6; }
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
:deep(.vue-flow__handle) { width: 6px; height: 6px; background: #2a3346; border: 1px solid #3a4a6b; pointer-events: none; }
:deep(.vue-flow__edge-path) { stroke-linecap: round; }
</style>
