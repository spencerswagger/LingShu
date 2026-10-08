<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Edit, Delete } from '@element-plus/icons-vue'
import {
  listExternalModels,
  batchDeleteExternalModels,
  syncModelsPrices,
  rateLabels,
  rateKeys,
  type ExternalModel,
} from '@/api/admin'
import ErrorBubble from '@/components/ErrorBubble.vue'
import ColumnFilter from '@/components/ColumnFilter.vue'

const router = useRouter()
const loading = ref(false)
const list = ref<ExternalModel[]>([])
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const selected = ref<ExternalModel[]>([])

const filters = reactive({
  q: '',
  enabled: undefined as string | undefined,
})

// 前端本地过滤（全量加载）：名称/描述 + 启用
const displayList = computed(() => {
  const q = filters.q.trim().toLowerCase()
  return list.value.filter((m) => {
    const hitQ =
      !q ||
      (m.ExternalName || '').toLowerCase().includes(q) ||
      (m.Description || '').toLowerCase().includes(q)
    const hitEn = filters.enabled === undefined || String(m.Enabled) === filters.enabled
    return hitQ && hitEn
  })
})

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await listExternalModels()
    list.value = res.Data || []
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
onMounted(load)

function fmtRate(v: number | null | undefined): string {
  if (v == null) return '-'
  const n = Math.round(v * 1e6) / 1e6
  return String(n)
}

function ratesDetail(m: ExternalModel): string {
  const r = m.SaleRates || {}
  const parts = rateKeys.map((k) => `${rateLabels[k]} ${fmtRate(r[k])}`)
  return `${parts.join('，')}（单位：人民币元/百万 token）`
}

async function onDelete(row: ExternalModel) {
  try {
    await ElMessageBox.confirm(
      `确认删除对外模型「${row.ExternalName}」吗？相关渠道内部模型的绑定关系将受影响。`,
      '删除确认',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    await batchDeleteExternalModels([row.ID])
    ElMessage.success('已删除')
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '删除失败')
  }
}

async function onBatchDelete() {
  if (!selected.value.length) return
  const ids = selected.value.map((r) => r.ID)
  try {
    await ElMessageBox.confirm(
      `确认删除选中的 ${ids.length} 个对外模型吗？`,
      '批量删除确认',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    await batchDeleteExternalModels(ids)
    ElMessage.success('批量删除成功')
    selected.value = []
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '批量删除失败')
  }
}

// 批量用 models.dev 参考价刷新全部售价
const syncing = ref(false)
async function onSyncPrices() {
  if (!list.value.length) return
  try {
    await ElMessageBox.confirm(
      '将用 models.dev 参考价批量刷新所有对外模型的售价（input/output 两段），无参考价的模型自动跳过。已配置的时段/分档与其余费率段不受影响。',
      '从 models.dev 同步售价',
      { type: 'info', confirmButtonText: '同步', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  syncing.value = true
  try {
    const res = await syncModelsPrices()
    const { Updated, Skipped } = res.Data
    const esc = (s: string) =>
      s.replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c] as string)
    const lines: string[] = []
    if (Updated.length) {
      lines.push(`已更新 <b>${Updated.length}</b> 个模型的售价：<br/>${Updated.map(esc).join('、')}`)
    } else {
      lines.push('没有模型被更新。')
    }
    if (Skipped.length) {
      lines.push(`跳过 <b>${Skipped.length}</b> 个（models.dev 无参考价）：<br/>${Skipped.map(esc).join('、')}`)
    }
    // 结果常驻弹窗，避免 toast 一闪而过来不及查看同步明细
    try {
      await ElMessageBox.alert(lines.join('<br/><br/>'), '同步售价完成', {
        confirmButtonText: '知道了',
        dangerouslyUseHTMLString: true,
      })
    } catch {
      /* 用户直接关闭结果弹窗，忽略；随后仍刷新列表 */
    }
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '同步失败')
  } finally {
    syncing.value = false
  }
}
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">模型</div>
      <span class="page-count">{{ list.length }} 个模型</span>
      <div class="page-header-spacer" />
      <el-button :loading="syncing" @click="onSyncPrices">同步售价</el-button>
      <el-button v-if="selected.length" type="danger" @click="onBatchDelete">
        批量删除
      </el-button>
      <el-button type="primary" @click="router.push('/admin/models/create')">新建</el-button>
    </div>

    <div v-loading="loading" class="card">
      <el-table
        :data="displayList"
        border
        stripe
        class="table-nowrap clickable-rows"
        row-key="ID"
        @selection-change="(rows: any[]) => (selected = rows)"
        @row-click="(row: ExternalModel, _c: unknown, e: Event) => !(e.target as HTMLElement)?.closest('.op-cell') && router.push(`/admin/models/${row.ID}/edit`)"
      >
        <el-table-column type="selection" width="44" />
        <el-table-column label="对外名称 / 描述" min-width="200">
          <template #header>
            <ColumnFilter label="名称 / 描述" :active="!!filters.q" @clear="() => (filters.q = '')">
              <el-input
                v-model="filters.q"
                placeholder="按名称或描述搜索"
                clearable
                size="small"
                @keyup.enter="(e: KeyboardEvent) => (e.target as HTMLInputElement)?.blur()"
              />
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ row.ExternalName }}</span>
              <span class="t-clock ellipsis">{{ row.Description || '-' }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="售价·输入" width="110" align="right">
          <template #default="{ row }">
            <el-tooltip :content="ratesDetail(row)" placement="top">
              <span class="rate-cell">{{ fmtRate(row.SaleRates?.Input) }}</span>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="售价·输出" width="110" align="right">
          <template #default="{ row }">
            <el-tooltip :content="ratesDetail(row)" placement="top">
              <span class="rate-cell">{{ fmtRate(row.SaleRates?.Output) }}</span>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="售价·缓存读" width="120" align="right">
          <template #default="{ row }">
            <el-tooltip :content="ratesDetail(row)" placement="top">
              <span class="rate-cell">{{ fmtRate(row.SaleRates?.CacheRead) }}</span>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="启用" min-width="80" align="center">
          <template #header>
            <ColumnFilter label="启用" :active="filters.enabled !== undefined" @clear="() => (filters.enabled = undefined)">
              <el-select v-model="filters.enabled" placeholder="启用状态" clearable size="small" style="width: 100%">
                <el-option label="启用" value="true" />
                <el-option label="停用" value="false" />
              </el-select>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <el-tag :type="row.Enabled ? 'success' : 'info'" effect="plain" size="small">
              {{ row.Enabled ? '启用' : '停用' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="90" align="right" fixed="right">
          <template #default="{ row }">
            <div class="op-cell" @click.stop>
              <el-tooltip content="编辑" placement="top">
                <el-icon class="op-icon" @click="router.push(`/admin/models/${row.ID}/edit`)"><Edit /></el-icon>
              </el-tooltip>
              <el-tooltip content="删除" placement="top">
                <el-icon class="op-icon danger" @click="onDelete(row)"><Delete /></el-icon>
              </el-tooltip>
            </div>
          </template>
        </el-table-column>
      </el-table>

      <el-empty v-if="!loading && !displayList.length" description="暂无对外模型" />
      <ErrorBubble
        v-if="errInfo.message || errInfo.requestId"
        :message="errInfo.message"
        :request-id="errInfo.requestId"
      />
    </div>
  </div>
</template>

<style scoped>
.t-time {
  display: flex;
  flex-direction: column;
  line-height: 1.35;
}
.t-date {
  font-size: 12px;
  color: var(--color-text);
}
.t-clock {
  font-size: 12px;
  color: var(--color-text-tertiary);
  font-variant-numeric: tabular-nums;
}
.ellipsis {
  max-width: 180px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.rates {
  font-size: 12px;
  color: var(--color-text-secondary);
  cursor: help;
}
.rate-cell {
  cursor: help;
  font-variant-numeric: tabular-nums;
}
</style>