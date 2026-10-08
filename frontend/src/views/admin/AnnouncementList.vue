<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Edit, Delete } from '@element-plus/icons-vue'
import { listAnnouncements, batchDeleteAnnouncements, type AdminAnnouncement } from '@/api/admin'
import ErrorBubble from '@/components/ErrorBubble.vue'
import ColumnFilter from '@/components/ColumnFilter.vue'

const router = useRouter()
const loading = ref(false)
const list = ref<AdminAnnouncement[]>([])
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const selected = ref<AdminAnnouncement[]>([])

const filters = reactive({
  q: '',
  level: '' as string | undefined,
  enabled: undefined as string | undefined,
})

// 前端本地过滤（全量加载）：标题/内容、级别、启用
const displayList = computed(() => {
  const q = filters.q.trim().toLowerCase()
  return list.value.filter((a) => {
    const hitQ = !q || (a.Title || '').toLowerCase().includes(q) || (a.Content || '').toLowerCase().includes(q)
    const hitLv = !filters.level || a.Level === filters.level
    const hitEn = filters.enabled === undefined || String(a.Enabled) === filters.enabled
    return hitQ && hitLv && hitEn
  })
})

const levelMap: Record<string, { label: string; type: string }> = {
  info: { label: 'Info', type: 'primary' },
  warning: { label: 'Warning', type: 'warning' },
  danger: { label: 'Danger', type: 'danger' },
}

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await listAnnouncements()
    list.value = res.data.list || []
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
onMounted(load)

async function onDelete(row: AdminAnnouncement) {
  try {
    await ElMessageBox.confirm(
      `确认删除公告「${row.Title}」吗？`,
      '删除确认',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    await batchDeleteAnnouncements([row.ID])
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
      `确认删除选中的 ${ids.length} 条公告吗？`,
      '批量删除确认',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    await batchDeleteAnnouncements(ids)
    ElMessage.success('批量删除成功')
    selected.value = []
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '批量删除失败')
  }
}
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">公告</div>
      <span class="page-count">共 {{ list.length }} 条公告</span>
      <div class="page-header-spacer" />
      <el-button v-if="selected.length" type="danger" @click="onBatchDelete">
        批量删除
      </el-button>
      <el-button type="primary" @click="router.push('/admin/announcements/create')">新建公告</el-button>
    </div>

    <div v-loading="loading" class="card">
      <el-table
        :data="displayList"
        border
        stripe
        class="table-nowrap clickable-rows"
        row-key="ID"
        @selection-change="(rows: any[]) => (selected = rows)"
        @row-click="(row: AdminAnnouncement, _c: unknown, e: Event) => !(e.target as HTMLElement)?.closest('.op-cell') && router.push(`/admin/announcements/${row.ID}`)"
      >
        <el-table-column type="selection" width="44" />
        <el-table-column label="标题 / 内容" min-width="240">
          <template #header>
            <ColumnFilter label="标题 / 内容" :active="!!filters.q" @clear="() => (filters.q = '')">
              <el-input
                v-model="filters.q"
                placeholder="按标题或内容搜索"
                clearable
                size="small"
                @keyup.enter="(e: KeyboardEvent) => (e.target as HTMLInputElement)?.blur()"
              />
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ row.Title }}</span>
              <span class="t-clock ellipsis">{{ row.Content || '-' }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="级别" min-width="100" align="center">
          <template #header>
            <ColumnFilter label="级别" :active="!!filters.level" @clear="() => (filters.level = undefined)">
              <el-select v-model="filters.level" placeholder="级别" clearable size="small" style="width: 100%">
                <el-option v-for="(v, k) in levelMap" :key="k" :label="v.label" :value="k" />
              </el-select>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <el-tag :type="(levelMap[row.Level]?.type as any) || 'info'" size="small" effect="light">
              {{ levelMap[row.Level]?.label || row.Level }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="发布 / 过期" min-width="200">
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ row.PublishAt || '立即' }}</span>
              <span class="t-clock">{{ row.ExpireAt || '永不过期' }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="启用" min-width="76" align="center">
          <template #header>
            <ColumnFilter label="启用" :active="filters.enabled !== undefined" @clear="() => (filters.enabled = undefined)">
              <el-select v-model="filters.enabled" placeholder="启用状态" clearable size="small" style="width: 100%">
                <el-option label="启用" value="true" />
                <el-option label="停用" value="false" />
              </el-select>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <el-tag :type="row.Enabled ? 'success' : 'info'" size="small" effect="plain">
              {{ row.Enabled ? '是' : '否' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="90" align="right" fixed="right">
          <template #default="{ row }">
            <div class="op-cell" @click.stop>
              <el-tooltip content="编辑" placement="top">
                <el-icon class="op-icon" @click="router.push(`/admin/announcements/${row.ID}`)"><Edit /></el-icon>
              </el-tooltip>
              <el-tooltip content="删除" placement="top">
                <el-icon class="op-icon danger" @click="onDelete(row)"><Delete /></el-icon>
              </el-tooltip>
            </div>
          </template>
        </el-table-column>
      </el-table>

      <el-empty v-if="!loading && !displayList.length" description="暂无公告" />
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
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>