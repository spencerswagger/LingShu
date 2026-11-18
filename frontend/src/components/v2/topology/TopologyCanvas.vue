<script setup lang="ts">
// 拓扑画布（四层：账号 → 供应商 → 实际模型 → 路由组 / 外部模型），基于 @vue-flow/core。
//
// 三条硬口径：
//   1. **只画真实数据**。数据全部来自 `GET /admin/topology`（T1）；接口未上线时这一块不出假节点，
//      由页面外层出空态（Spec §5 规则 3：接口就绪当轮删 mock）。
//   2. **连线只表达两种关系**：账号→供应商（网关侧事实）、实际模型→对外模型（`external_models.bound_internal_ref`，
//      灵枢唯一可写的映射）。路由组的成员关系在网关侧维护，灵枢没有镜像，所以不画「模型→组」的线。
//   3. **坐标不落库**就自动布局（T2 可延后）。画布拖动只影响本地视图，刷新按自动布局重排——
//      页面不去暗示「拖完会保存」。
//
// 节点身份（与后端 T1 的字段逐字对齐，别自己发明 ref）：
//   供应商 → `p:${Provider}`（Providers[] 没有 ref 字段，且 Items[].ProviderRef 与它同一套取值）
//   账号   → `a:${Provider}:${Account}`（Accounts[] 没有 ref 字段，(Provider, Account) 就是唯一键）
//   路由组 → `g:${Ref}`（Items[] 里有 ref；组的 ProviderRef 恒为空串）
//   对外模型 → `x:${ID}`（Externals[] 的 ID 是雪花字符串）
import { computed } from 'vue'
import { VueFlow, Handle, Position, type Node, type Edge } from '@vue-flow/core'
import { Background } from '@vue-flow/background'
import { Controls } from '@vue-flow/controls'
import '@vue-flow/core/dist/style.css'
import '@vue-flow/core/dist/theme-default.css'
import '@vue-flow/controls/dist/style.css'
import type { InternalModel, InternalModelView } from '@/api/v2'
import { fmtCompact } from '@/utils/format'

const props = defineProps<{ view: InternalModelView }>()
const emit = defineEmits<{ selectExternal: [id: string] }>()

// 列坐标（px）：账号 / 供应商大卡 / 路由组 / 对外模型
const X_ACCOUNT = 0
const X_PROVIDER = 210
const X_GROUP = 570
const X_EXTERNAL = 830

interface ModelRow {
  Ref: string
  Name: string
  ContextLen: number
  Stale: boolean
  Sold: number
}

// 每个供应商大卡里列出的「实际模型」行（直通模型；路由组单独立节点，和原型一致）
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

// 绑定计数：一个内部模型可以挂多个对外模型（不同价格档）
const soldMap = computed(() => {
  const map = new Map<string, number>()
  for (const e of props.view.Externals) {
    if (!e.BoundInternalRef) continue
    map.set(e.BoundInternalRef, (map.get(e.BoundInternalRef) || 0) + 1)
  }
  return map
})

// 路由组独立成层：网关侧它是**跨供应商的聚合层**，后端刻意给 `ProviderRef=""`
// （backend/internal/domain/topology/source.go `parseGroups`），所以组的归属只认它自己的 ref，
// 不按 ProviderRef 挂到某张供应商卡下面——否则真数据里组既进不了任何卡、还会被聚成一张空的「(未知)」卡。
const groupItems = computed(() => props.view.Items.filter((m) => m.Kind === 'group'))

// 自动布局：三列各自纵向堆叠；供应商大卡的高度随「实际模型行数」增长。
const graph = computed<{ nodes: Node[]; edges: Edge[] }>(() => {
  const view = props.view
  const nodes: Node[] = []
  const edges: Edge[] = []

  let providerY = 0
  let groupY = 0
  let externalY = 0

  const providerTops = new Map<string, number>()

  for (const p of view.Providers) {
    // 供应商身份 = Provider 名（与 Items[].ProviderRef 同一套取值）；空名不该有卡，后端聚合出的
    // `provider_ref=''` 那一行是路由组，画布单列渲染，跳过。
    if (!p.Provider) continue
    const rows = rowsOf(p.Provider, view.Items, soldMap.value)
    const accounts = view.Accounts.filter((a) => a.Provider === p.Provider)

    const cardHeight = 52 + rows.length * 30 + 10
    const providerId = `p:${p.Provider}`
    nodes.push({
      id: providerId,
      type: 'provider',
      position: { x: X_PROVIDER, y: providerY },
      // Stale 是**条数**不是布尔；Accounts 也是计数，缺省时退到账号层实际条数
      data: {
        Ref: p.Provider,
        Accounts: p.Accounts || accounts.length,
        Models: p.Models,
        StaleCount: p.Stale || 0,
        Rows: rows,
      },
      draggable: false,
    })
    providerTops.set(p.Provider, providerY)

    // 账号：贴着自己供应商的纵向位置散开，避免所有账号挤在顶部
    accounts.forEach((a, i) => {
      const accId = `a:${a.Provider}:${a.Account}`
      nodes.push({
        id: accId,
        type: 'account',
        position: { x: X_ACCOUNT, y: providerY + 10 + i * 44 },
        data: { Account: a.Account, Provider: a.Provider, Subscribed: a.Subscribed, LastSeenAt: a.LastSeenAt },
        draggable: false,
      })
      edges.push({
        id: `e:${accId}->${providerId}`,
        source: accId,
        target: providerId,
        type: 'smoothstep',
        animated: false,
        style: { stroke: '#b9c2d8', strokeWidth: 1.2 },
      })
    })

    providerY += Math.max(cardHeight, accounts.length * 44 + 12) + 28
  }

  // 路由组：单独一列（组的路由策略/成员由网关维护，灵枢只读）。
  // 不挂在供应商卡下面：后端对组恒给 ProviderRef=""，按供应商归属遍历会让这一层整个不渲染。
  for (const g of groupItems.value) {
    const gid = `g:${g.Ref}`
    nodes.push({
      id: gid,
      type: 'group',
      position: { x: X_GROUP, y: groupY },
      data: {
        Ref: g.Ref,
        Name: g.DisplayName || g.Ref,
        ContextLen: g.ContextLen,
        Stale: g.Stale,
        Sold: soldMap.value.get(g.Ref) || 0,
      },
      draggable: false,
    })
    groupY += 96
  }

  // 对外模型：按绑定目标分组，绑定到直通模型的从左列（供应商）连，绑定到组的从组节点连，
  // 未绑定的照常出节点（灰化 + 「不售卖」），别让运营看不见「我建了但没卖」的模型。
  // `InternalModelHit=false` 且有绑定 = 绑定悬空（目标已从网关下线/改名）：节点照常出，打「悬空」标，
  // 不画线（画到不存在的节点上会骗人）。
  const externals = [...view.Externals].sort((a, b) => Number(!!a.BoundInternalRef) - Number(!!b.BoundInternalRef))
  for (const e of externals) {
    const xid = `x:${e.ID}`
    const dangling = e.BoundInternalRef !== '' && !e.InternalModelHit
    nodes.push({
      id: xid,
      type: 'external',
      position: { x: X_EXTERNAL, y: externalY },
      data: {
        ID: e.ID,
        Name: e.ExternalName,
        Bound: e.BoundInternalRef,
        Enabled: e.Enabled,
        Dangling: dangling,
      },
      draggable: false,
    })
    externalY += 104

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
      style: { stroke: '#5f9e6e', strokeWidth: 1.4 },
    })
  }

  return { nodes, edges }
})

defineExpose({ fit: () => undefined })

function openExternal(id: string) {
  emit('selectExternal', id)
}
</script>

<template>
  <div class="canvas-wrap">
    <VueFlow
      :nodes="graph.nodes"
      :edges="graph.edges"
      :min-zoom="0.2"
      :max-zoom="1.6"
      :nodes-draggable="false"
      :nodes-connectable="false"
      :elements-selectable="true"
      fit-view-on-init
      class="canvas"
    >
      <!-- 账号（只读）：V2 没有账号镜像表，这一层是账单/账本/订阅三处并集现算的，
           后端只给 Provider / Account / Subscribed / LastSeenAt —— 额度窗口那类网关字段没有就不显示 -->
      <template #node-account="p">
        <div class="nd nd-account">
          <div class="nd-tag">账号</div>
          <div class="nd-title">{{ p.data.Account }}</div>
          <div class="nd-sub">
            <span v-if="p.data.Subscribed" class="chip chip-plan">已录订阅</span>
            <span v-else class="chip">按量</span>
            <span class="txt">{{ p.data.Provider }}</span>
          </div>
          <Handle type="source" :position="Position.Right" />
        </div>
      </template>

      <!-- 供应商大卡：卡内是「实际模型」行；每行右侧一个句柄，供绑定的对外模型连线 -->
      <template #node-provider="p">
        <div class="nd nd-provider">
          <Handle type="target" :position="Position.Left" />
          <div class="pv-head">
            <div>
              <div class="nd-tag">供应商</div>
              <div class="nd-title">{{ p.data.Ref }}</div>
            </div>
            <div class="pv-meta">
              <span class="txt">{{ p.data.Accounts }} 个账号 · {{ p.data.Rows.length }} 个直通模型</span>
              <span v-if="p.data.StaleCount" class="chip chip-warn">本轮未见 {{ p.data.StaleCount }}</span>
            </div>
          </div>
          <div v-if="!p.data.Rows.length" class="pv-empty">本轮没有直通模型</div>
          <div v-for="r in p.data.Rows" :key="r.Ref" class="pv-row" :class="{ 'is-stale': r.Stale }">
            <div class="pv-row-main">
              <span class="pv-row-name">{{ r.Name }}</span>
              <span class="pv-row-meta">
                {{ r.ContextLen ? fmtCompact(r.ContextLen) + ' 上下文' : '上下文未知' }}
                <template v-if="r.Stale"> · 本轮未见</template>
              </span>
            </div>
            <span v-if="r.Sold" class="chip chip-sold">对外 {{ r.Sold }}</span>
            <span v-else class="chip chip-idle">未售卖</span>
            <Handle :id="`m:${r.Ref}`" type="source" :position="Position.Right" />
          </div>
          <div class="pv-foot">实际模型清单随网关同步，灵枢只读；要对外卖，把某个对外模型绑到这一行即可</div>
        </div>
      </template>

      <!-- 路由组（内部模型的一种）：策略与成员由网关维护 -->
      <template #node-group="p">
        <div class="nd nd-group" :class="{ 'is-stale': p.data.Stale }">
          <Handle type="target" :position="Position.Left" />
          <div class="nd-tag">路由组</div>
          <div class="nd-title">{{ p.data.Name }}</div>
          <div class="nd-sub">
            <span class="txt">{{ p.data.ContextLen ? fmtCompact(p.data.ContextLen) + ' 上下文' : '上下文未知' }}</span>
          </div>
          <div class="nd-sub">
            <span v-if="p.data.Sold" class="chip chip-sold">对外 {{ p.data.Sold }}</span>
            <span v-else class="chip chip-idle">未售卖</span>
          </div>
          <div class="nd-foot">成员与策略在网关侧维护</div>
          <Handle type="source" :position="Position.Right" />
        </div>
      </template>

      <!-- 对外模型（灵枢售卖面）：点击进模型编辑。T1 只给绑定状态，不给价格档（价格在模型编辑页），
           所以这里不显示价格——宁可少一行，也不拿 0 冒充 -->
      <template #node-external="p">
        <div
          class="nd nd-external"
          :class="{
            'is-unbound': !p.data.Bound,
            'is-off': p.data.Enabled === false,
            'is-dangling': p.data.Dangling,
          }"
          role="button"
          tabindex="0"
          @click="openExternal(p.data.ID)"
          @keydown.enter="openExternal(p.data.ID)"
        >
          <Handle type="target" :position="Position.Left" />
          <div class="nd-tag">对外模型</div>
          <div class="nd-title">{{ p.data.Name || '(未命名)' }}</div>
          <div class="nd-sub">
            <span v-if="p.data.Dangling" class="chip chip-warn">绑定悬空 · 目标已下线</span>
            <span v-else-if="p.data.Bound" class="chip chip-sold">已绑 {{ p.data.Bound }}</span>
            <span v-else class="chip chip-warn">未绑定 · 不售卖</span>
            <span v-if="p.data.Enabled === false" class="chip chip-idle">已停用</span>
          </div>
        </div>
      </template>

      <Background :gap="16" :size="1" pattern-color="#e3e8f2" />
      <Controls position="bottom-right" :show-interactive="false" />
    </VueFlow>
  </div>
</template>

<style scoped>
.canvas-wrap {
  position: relative;
  width: 100%;
  height: 560px;
  border: 1px solid var(--color-border);
  border-radius: var(--radius-m);
  background: var(--bg);
  overflow: hidden;
}
.canvas {
  width: 100%;
  height: 100%;
}

/* ===== 节点通用 ===== */
.nd {
  background: var(--panel);
  border: 1px solid var(--color-border-strong);
  border-radius: 10px;
  padding: 8px 10px;
  font-size: 12px;
  line-height: 1.45;
  box-shadow: none;
  color: var(--color-text);
}
.nd.is-stale {
  opacity: 0.55;
  border-style: dashed;
  background: #0b101c;
}
.nd-tag {
  font-size: 10.5px;
  letter-spacing: 0.3px;
  color: var(--color-text-tertiary);
}
.nd-title {
  font-size: 13px;
  font-weight: 600;
  word-break: break-all;
}
.nd-sub {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
  margin-top: 2px;
  color: var(--color-text-secondary);
}
.nd-foot {
  margin-top: 4px;
  font-size: 10.5px;
  color: var(--color-text-tertiary);
  border-top: 1px dashed var(--color-border);
  padding-top: 4px;
}
.txt {
  font-size: 11px;
}
.chip {
  display: inline-flex;
  align-items: center;
  padding: 0 6px;
  height: 17px;
  border-radius: 999px;
  font-size: 10.5px;
  background: #161d29;
  color: var(--ink-2);
  white-space: nowrap;
}
.chip-plan {
  background: rgba(76,111,232,.14);
  color: #4b4dd6;
}
.chip-sold {
  background: rgba(95,158,110,.14);
  color: #8fbf9f;
}
.chip-idle {
  background: #141b26;
  color: var(--ink-3);
}
.chip-warn {
  background: rgba(201,162,39,.1);
  color: #d6bd80;
}

/* 账号：蓝 */
.nd-account {
  width: 168px;
  border-left: 3px solid #4c6fe8;
}

/* 供应商大卡：深色标题区 + 卡内模型行 */
.nd-provider {
  width: 300px;
  padding: 0;
  overflow: hidden;
}
.pv-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 8px;
  padding: 8px 10px;
  background: #0b101c;
  border-bottom: 1px solid var(--color-border);
}
.pv-meta {
  display: flex;
  flex-direction: column;
  align-items: flex-end;
  gap: 2px;
  font-size: 10.5px;
  color: var(--color-text-tertiary);
  text-align: right;
}
.pv-row {
  position: relative;
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 5px 10px;
  border-bottom: 1px dashed var(--color-border);
}
.pv-row.is-stale {
  opacity: 0.5;
}
.pv-row-main {
  flex: 1;
  min-width: 0;
}
.pv-row-name {
  display: block;
  font-size: 12px;
  font-weight: 500;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.pv-row-meta {
  font-size: 10.5px;
  color: var(--color-text-tertiary);
}
.pv-empty {
  padding: 8px 10px;
  font-size: 11px;
  color: var(--color-text-tertiary);
}
.pv-foot {
  padding: 6px 10px 8px;
  font-size: 10.5px;
  color: var(--color-text-tertiary);
  background: #0c1220;
}

/* 路由组：紫 */
.nd-group {
  width: 210px;
  border-left: 3px solid #a78bfa;
}

/* 对外模型：绿 */
.nd-external {
  width: 210px;
  border-left: 3px solid #5f9e6e;
  cursor: pointer;
}
.nd-external:hover {
  border-color: #5f9e6e;
  box-shadow: var(--shadow-pop);
}
.nd-external.is-unbound {
  border-left-color: var(--line-2);
  background: #0b101c;
}
.nd-external.is-off {
  opacity: 0.6;
}
/* 绑定悬空：目标已从网关下线/改名 —— 橙边，别和「正常在售」混在一起 */
.nd-external.is-dangling {
  border-left-color: var(--warn);
  background: rgba(201,162,39,.06);
}

/* vue-flow 默认节点内边距会挤压卡片，这里清掉 */
:deep(.vue-flow__node) {
  padding: 0;
  border: none;
  background: transparent;
  font-size: 12px;
}

@media (max-width: 640px) {
  .canvas-wrap {
    height: 420px;
  }
}
</style>
