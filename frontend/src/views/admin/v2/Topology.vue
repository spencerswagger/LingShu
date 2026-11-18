<script setup lang="ts">
// 拓扑（P02）：网关侧「账号 → 供应商 → 实际模型 → 路由组 / 对外模型」四层关系 + 同步新鲜度。
//
// 数据层契约（0019_v2_topology.sql / 数据层契约 §2.1–§2.3）：
//   1. `internal_models` 是**只读镜像**，没有软删列；`stale=true` 只表示「本轮同步没见到」，
//      仍展示但标灰，删除判定交给网关（它才是真源）。
//   2. `model_links` 只存画布坐标，不当关系真源；绑定真源是 `external_models.bound_internal_ref`。
//   3. 账号层没有镜像表：它是「账单 / 账本 / 订阅」三处来源按 (供应商, 账号) 并集现算的，只读。
// 字段名以后端 T1 为准（backend/internal/domain/topology/handler.go 的 view，PR #88 已合入 v2）：
//   Providers[] 没有 ref 字段（身份 = Provider 名），Accounts[] 也没有（身份 = Provider + Account），
//   Externals[] 用 ID / ExternalName / BoundInternalRef / InternalModelHit。
// 页面不展示任何凭据/密钥/上游 URL（Spec P02 验收 5）。
import { computed, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { getTopology, syncTopology, type InternalModel, type InternalModelView } from '@/api/v2'
import { mockTopology } from '@/mock/topology'
import { useV2Load } from '@/composables/useV2Load'
import V2Page from '@/components/v2/V2Page.vue'
import StatCards from '@/components/v2/StatCards.vue'
import StatusPill from '@/components/v2/StatusPill.vue'
import TopologyCanvas from '@/components/v2/topology/TopologyCanvas.vue'
import type { StatItem, V2Dep } from '@/components/v2/types'
import { fmtCompact, fmtDateTime } from '@/utils/format'

const router = useRouter()
const tab = ref<'canvas' | 'snapshot' | 'accounts'>('canvas')
const keyword = ref('')

const { state, data, error, load, showDemo, exitDemo } = useV2Load<InternalModelView>(getTopology, mockTopology)

onMounted(() => void load())

// 手动同步（T3）：与后台定时同步共用实现；接口未上线时按钮照样能点，但结果如实说明（Spec §5 规则 5）。
const syncing = ref(false)
const syncResult = ref<{ tone: 'ok' | 'warn' | 'danger'; text: string } | null>(null)
const syncReady = ref(false)

async function runSync() {
  if (syncing.value) return
  syncing.value = true
  syncResult.value = null
  const res = await syncTopology()
  if (res.state === 'live' && res.data) {
    syncReady.value = true
    const d = res.data
    const counts = `拉取 ${d.Fetched} / 新增 ${d.Created} / 失效 ${d.Stale}`
    // T3 失败同样回 200（业务结果），成败只能看 State —— 只认 HTTP 状态会给出假的绿字「同步完成」。
    if (d.State === 'failed') {
      syncResult.value = {
        tone: 'danger',
        text: `同步失败：${d.Message || '网关未给出原因'}（旧快照已保留，画布与列表仍是上一轮成功的数据）`,
      }
    } else if (d.State === 'ok') {
      syncResult.value = { tone: 'ok', text: `同步完成：${counts}` }
    } else {
      syncResult.value = { tone: 'warn', text: `同步已执行，但响应未给结果状态（State 缺失），无法确认成败：${counts}` }
    }
    // 失败也要重载：后端已把 failed 状态与原因落库，状态条要跟着翻成失败态
    await load()
  } else if (res.state === 'pending') {
    syncResult.value = {
      tone: 'warn',
      text: '手动同步接口（POST /admin/topology/sync）尚未上线，当前只能等后台定时同步；按钮保留，接口上线后同一按钮即生效。',
    }
  } else {
    syncResult.value = { tone: 'danger', text: `同步失败：${res.error?.message || '请求失败'}（${res.error?.requestId || '无 request_id'}）` }
  }
  syncing.value = false
}

const deps = computed<V2Dep[]>(() => [
  { path: 'GET /admin/topology', state: state.value === 'live' ? 'ready' : 'pending' },
  { path: 'POST /admin/topology/sync', state: syncReady.value ? 'ready' : 'pending' },
  // 坐标保存是可延后项（T2）：未就绪时画布走自动布局、坐标不持久化，不影响售卖与计费
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

// 计数一律显示**精确整数**（Spec P02 验收 1：未绑定计数必须与列表条数相等，不能被 3.0 这种缩写糊过去）
function count(n: number): string {
  return (n || 0).toLocaleString('zh-CN')
}

const stats = computed<StatItem[]>(() => {
  const d = data.value
  if (!d) return []
  const groups = d.Items.filter((m) => m.Kind === 'group').length
  return [
    { label: '供应商', value: count(d.Providers.length), hint: `账号 ${d.Accounts.length} 个（网关只读）` },
    {
      label: '内部模型',
      value: count(d.Items.length),
      hint: `路由组 ${groups} / 直通 ${d.Items.length - groups}`,
    },
    {
      label: '本轮未见（stale）',
      value: count(d.StaleCount),
      tone: d.StaleCount > 0 ? 'warn' : 'ok',
      hint: '标灰但仍在售；删除由网关判定',
    },
    {
      label: '未绑定的对外模型',
      value: count(d.UnboundExternal),
      tone: d.UnboundExternal > 0 ? 'warn' : 'ok',
      hint: '未绑定 = 不售卖；绑定在模型页设置',
    },
    { label: '最后同步', value: d.SyncedAt ? fmtDateTime(d.SyncedAt) : '—', hint: '镜像每轮同步刷新 last_seen_at' },
  ]
})

const syncState = computed(() => data.value?.Sync || null)
const syncTone = computed(() => {
  const st = syncState.value?.State
  if (st === 'ok') return 'ok'
  if (st === 'failed') return 'danger'
  return 'warn'
})

function openExternal(id: string) {
  if (!id) return
  void router.push(`/admin/models/${id}/edit`)
}
</script>

<template>
  <V2Page
    title="拓扑"
    intro="账号 → 供应商 → 实际模型 → 对外模型，实际模型随网关同步。"
    source="数据层契约 §2.1–§2.3（internal_models / model_links / external_models.bound_internal_ref）"
    :deps="deps"
    :state="state"
    :error="error"
    pending-hint="拓扑聚合（T1）与手动同步（T3）待后端上线。画布、同步状态条、快照/账号列表已按 0019 表结构就位，接口上线即接真数据。"
    @reload="load()"
    @demo="showDemo"
    @exit-demo="exitDemo"
  >
    <template #actions>
      <el-input v-model="keyword" size="small" placeholder="搜索 ref / 供应商" clearable style="width: 170px" />
      <el-button size="small" type="primary" plain :loading="syncing" @click="runSync">立即同步</el-button>
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
          <el-tag
            :type="syncTone === 'ok' ? 'success' : syncTone === 'danger' ? 'danger' : 'warning'"
            size="small"
            effect="plain"
          >
            {{ syncState?.State === 'ok' ? '成功' : syncState?.State === 'failed' ? '失败' : '未运行' }}
          </el-tag>
          <span v-if="syncState?.Message" class="sync-msg">{{ syncState.Message }}</span>
        </span>
        <span class="sync-item">本轮新增 <strong>{{ syncState?.Created ?? 0 }}</strong></span>
        <span class="sync-item">本轮失效 <strong>{{ syncState?.Stale ?? 0 }}</strong></span>
      </div>

      <p v-if="syncResult" class="sync-result" :class="`is-${syncResult.tone}`">{{ syncResult.text }}</p>

      <StatCards :items="stats" :columns="5" />

      <el-tabs v-model="tab">
        <el-tab-pane label="画布" name="canvas">
          <div class="card">
            <div class="card-head">
              <b>四层关系</b>
              <span class="legend">
                <i class="lg lg-account" />账号
                <i class="lg lg-provider" />供应商
                <i class="lg lg-group" />路由组
                <i class="lg lg-external" />对外模型
              </span>
              <span class="sp" />
              <span class="hint">拖动平移 / 滚轮缩放；点对外模型进编辑</span>
            </div>
            <TopologyCanvas :view="data" @select-external="openExternal" />
            <p class="foot">
              连线只有两种：账号→供应商（网关事实）、实际模型→对外模型（灵枢唯一可写的映射）。路由组的成员关系在网关侧维护，
              灵枢没有镜像，所以不画「模型→组」的线；画布坐标（T2）未上线前按自动布局排，刷新重排、不写库。
            </p>
          </div>
        </el-tab-pane>

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
            <p class="foot">镜像表没有软删列：删除由网关侧决定；本轮没见到的模型标 stale，仍展示、仍可绑定。</p>
          </div>
        </el-tab-pane>

        <el-tab-pane label="账号" name="accounts">
          <div class="card">
            <div class="table-wrap">
              <el-table :data="data.Accounts" size="small" border stripe empty-text="还没有上游账号：账单/账本/订阅里出现过的账号会在这里自动汇总">
                <el-table-column prop="Provider" label="供应商" width="140" show-overflow-tooltip />
                <el-table-column prop="Account" label="账号" min-width="180" show-overflow-tooltip />
                <el-table-column label="订阅" width="110">
                  <template #default="{ row }">
                    <el-tag v-if="row.Subscribed" size="small" effect="plain">已录订阅</el-tag>
                    <el-tag v-else size="small" type="info" effect="plain">按量</el-tag>
                  </template>
                </el-table-column>
                <el-table-column label="最后出现" width="180">
                  <template #default="{ row }">{{ row.LastSeenAt ? fmtDateTime(row.LastSeenAt) : '—' }}</template>
                </el-table-column>
              </el-table>
            </div>
            <p class="foot">
              账号层是**现算**的：账单 / 账本 / 订阅三处来源按 (供应商, 账号) 并集去重，灵枢没有账号镜像表。
              所以只有「有没有录订阅」和「最后一次出现的时刻」——额度窗口这类网关字段后端不返回，页面不显示。
              实付价 / 账期在「订阅」页维护。
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
  box-shadow: none;
  padding: 12px 14px;
}
.card-head {
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 12px;
  margin-bottom: 10px;
  font-size: 13px;
}
.card-head .sp {
  flex: 1;
}
.legend {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  font-size: 12px;
  color: var(--color-text-secondary);
}
.lg {
  display: inline-block;
  width: 9px;
  height: 9px;
  border-radius: 2px;
  margin-left: 6px;
}
.lg-account {
  background: #4c6fe8;
}
.lg-provider {
  background: #2b3a55;
}
.lg-group {
  background: #a78bfa;
}
.lg-external {
  background: #5f9e6e;
}
.hint {
  font-size: 12px;
  color: var(--color-text-tertiary);
}
.sync-bar {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 14px;
  padding: 8px 12px;
  border-radius: var(--radius-s);
  border: 1px solid var(--color-border);
  background: #0b101c;
  font-size: 13px;
}
.sync-bar.is-warn {
  background: rgba(201,162,39,.08);
  border-color: #ffe0a3;
}
.sync-bar.is-danger {
  background: rgba(196,85,63,.1);
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
.sync-result {
  margin: 8px 0 0;
  padding: 7px 12px;
  border-radius: var(--radius-s);
  font-size: 12.5px;
  line-height: 1.5;
}
.sync-result.is-ok {
  background: rgba(95,158,110,.12);
  color: #0e7a45;
}
.sync-result.is-warn {
  background: rgba(201,162,39,.08);
  color: #8a5a00;
}
.sync-result.is-danger {
  background: rgba(196,85,63,.1);
  color: #b42318;
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
