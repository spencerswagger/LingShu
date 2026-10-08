<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Edit, Delete } from '@element-plus/icons-vue'
import { listTags, updateTag, batchDeleteTags, type AdminTag } from '@/api/admin'
import ErrorBubble from '@/components/ErrorBubble.vue'
import ColumnFilter from '@/components/ColumnFilter.vue'

const router = useRouter()
const loading = ref(false)
const list = ref<AdminTag[]>([])
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const selected = ref<AdminTag[]>([])

const filters = reactive({ name: '', kv: '' })

// 前端本地过滤（全量加载）：名称/描述 与 KV 摘要
const displayList = computed(() => {
  const n = filters.name.trim().toLowerCase()
  const k = filters.kv.trim().toLowerCase()
  if (!n && !k) return list.value
  return list.value.filter((t) => {
    const hitName = !n || (t.Name || '').toLowerCase().includes(n) || (t.Description || '').toLowerCase().includes(n)
    const hitKv = !k || kvSummary(t).toLowerCase().includes(k)
    return hitName && hitKv
  })
})

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await listTags()
    list.value = res.data || []
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
onMounted(load)

function kvSummary(t: AdminTag): string {
  if (!t.KVPairs) return '-'
  return Object.entries(t.KVPairs)
    .map(([k, v]) => `${k}=${v}`)
    .join(', ')
}

async function onDelete(row: AdminTag) {
  try {
    await ElMessageBox.confirm(
      `确认删除标签「${row.Name}」吗？`,
      '删除确认',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    await batchDeleteTags([row.ID])
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
      `确认删除选中的 ${ids.length} 个标签吗？`,
      '批量删除确认',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    await batchDeleteTags(ids)
    ElMessage.success('批量删除成功')
    selected.value = []
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '批量删除失败')
  }
}

async function onToggle(row: AdminTag) {
  try {
    await updateTag(row.ID, {
      Name: row.Name,
      Description: row.Description,
      KVPairs: row.KVPairs,
      Enabled: !row.Enabled,
    })
    row.Enabled = !row.Enabled
    ElMessage.success(row.Enabled ? '已启用' : '已停用')
  } catch (e: any) {
    ElMessage.error(e?.message || '操作失败')
  }
}
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">标签</div>
      <div class="page-header-spacer" />
      <el-button v-if="selected.length" type="danger" @click="onBatchDelete">
        批量删除
      </el-button>
      <el-button type="primary" @click="router.push('/admin/tags/create')">新建标签</el-button>
    </div>

    <div v-loading="loading" class="card">
      <el-table
        :data="displayList"
        border
        stripe
        class="table-nowrap clickable-rows"
        row-key="ID"
        @selection-change="(rows: any[]) => (selected = rows)"
        @row-click="(row: AdminTag, _c: unknown, e: Event) => !(e.target as HTMLElement)?.closest('.op-cell') && router.push(`/admin/tags/${row.ID}/edit`)"
      >
        <el-table-column type="selection" width="44" />
        <el-table-column label="名称 / 描述" min-width="210">
          <template #header>
            <ColumnFilter label="名称 / 描述" :active="!!filters.name" @clear="() => (filters.name = '')">
              <el-input
                v-model="filters.name"
                placeholder="按名称或描述搜索"
                clearable
                size="small"
                @keyup.enter="(e: KeyboardEvent) => (e.target as HTMLInputElement)?.blur()"
              />
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ row.Name }}</span>
              <span class="t-clock">{{ row.Description || '-' }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="KV 摘要" min-width="180">
          <template #header>
            <ColumnFilter label="KV 摘要" :active="!!filters.kv" @clear="() => (filters.kv = '')">
              <el-input
                v-model="filters.kv"
                placeholder="按 k=v 搜索"
                clearable
                size="small"
                @keyup.enter="(e: KeyboardEvent) => (e.target as HTMLInputElement)?.blur()"
              />
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <el-tooltip :content="kvSummary(row)" placement="top">
              <span class="kv">{{ kvSummary(row) }}</span>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="130" align="right" fixed="right">
          <template #default="{ row }">
            <div class="op-cell" @click.stop>
              <el-switch
                :model-value="row.Enabled"
                inline-prompt
                active-text="启"
                inactive-text="禁"
                @change="onToggle(row)"
              />
              <el-tooltip content="编辑" placement="top">
                <el-icon class="op-icon" @click="router.push(`/admin/tags/${row.ID}/edit`)"><Edit /></el-icon>
              </el-tooltip>
              <el-tooltip content="删除" placement="top">
                <el-icon class="op-icon danger" @click="onDelete(row)"><Delete /></el-icon>
              </el-tooltip>
            </div>
          </template>
        </el-table-column>
      </el-table>

      <el-empty v-if="!loading && !list.length" description="暂无标签" />
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
.kv {
  font-size: 12px;
  color: var(--color-text-secondary);
  font-family: 'SF Mono', Menlo, Consolas, monospace;
}
</style>