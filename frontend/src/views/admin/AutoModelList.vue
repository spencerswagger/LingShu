<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Edit, Delete } from '@element-plus/icons-vue'
import { listAutoModels, deleteAutoModel, type AutoModel } from '@/api/admin'
import ErrorBubble from '@/components/ErrorBubble.vue'
import ColumnFilter from '@/components/ColumnFilter.vue'

const router = useRouter()
// mode='self'：开发者端（走 /dev 前缀接口），仅可写本人 private Auto；global 只读
const props = withDefaults(defineProps<{ mode?: 'admin' | 'self' }>(), { mode: 'admin' })
const isSelf = computed(() => props.mode === 'self')
const base = computed(() => (isSelf.value ? '/dev' : '/admin'))
const listBase = computed(() => (isSelf.value ? '/dev/auto-models' : '/admin/auto-models'))
const loading = ref(false)
const list = ref<AutoModel[]>([])
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

const filters = reactive({
  q: '',
  scope: undefined as string | undefined,
})

// 前端本地过滤（全量加载）：名称/描述 + 类型
const displayList = computed(() => {
  const q = filters.q.trim().toLowerCase()
  return list.value.filter((m) => {
    const hitQ =
      !q ||
      (m.Name || '').toLowerCase().includes(q) ||
      (m.Description || '').toLowerCase().includes(q)
    const hitScope = filters.scope === undefined || m.Scope === filters.scope
    return hitQ && hitScope
  })
})

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await listAutoModels({}, base.value)
    list.value = res.Data || []
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
onMounted(load)

// 成员名按 position 升序拼接（后端已保证顺序）
function memberNames(m: AutoModel): string[] {
  return (m.Members || [])
    .slice()
    .sort((a, b) => a.Position - b.Position)
    .map((x) => x.ExternalName || `#${x.ExternalModelID}`)
}
function memberPreview(m: AutoModel): string {
  const names = memberNames(m)
  return names.length ? names.join(' → ') : '-'
}
function memberDetail(m: AutoModel): string {
  const names = memberNames(m)
  if (!names.length) return '无成员'
  return names.map((n, i) => `${i + 1}. ${n}`).join('\n')
}

// 仅本人 private 可编辑：开发者端 global 只读，admin 端全部可写
function canWrite(row: AutoModel): boolean {
  return !isSelf.value || row.Scope === 'private'
}

function onRowClick(row: AutoModel, _c: unknown, e: Event) {
  if (!canWrite(row)) return
  if ((e.target as HTMLElement)?.closest('.op-cell')) return
  router.push(`${listBase.value}/${row.ID}/edit`)
}

async function onDelete(row: AutoModel) {
  try {
    await ElMessageBox.confirm(`确认删除 Auto 模型「${row.Name}」吗？`, '删除确认', {
      type: 'warning',
      confirmButtonText: '删除',
      cancelButtonText: '取消',
    })
  } catch {
    return
  }
  try {
    await deleteAutoModel(row.ID, base.value)
    ElMessage.success('已删除')
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '删除失败')
  }
}
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">Auto 模型</div>
      <span class="page-count">{{ list.length }} 个 Auto 模型</span>
      <div class="page-header-spacer" />
      <el-button type="primary" @click="router.push(`${listBase}/create`)">新建</el-button>
    </div>

    <div v-loading="loading" class="card">
      <el-table
        :data="displayList"
        border
        stripe
        class="table-nowrap clickable-rows"
        row-key="ID"
        @row-click="onRowClick"
      >
        <el-table-column label="名称 / 描述" min-width="200">
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
              <span class="t-date">{{ row.Name }}</span>
              <span class="t-clock ellipsis">{{ row.Description || '-' }}</span>
            </div>
          </template>
        </el-table-column>

        <el-table-column label="类型" width="100" align="center">
          <template #header>
            <ColumnFilter
              label="类型"
              :active="filters.scope !== undefined"
              @clear="() => (filters.scope = undefined)"
            >
              <el-select v-model="filters.scope" placeholder="作用域" clearable size="small" style="width: 100%">
                <el-option label="全局" value="global" />
                <el-option label="私有" value="private" />
              </el-select>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <el-tag :type="row.Scope === 'global' ? 'success' : 'info'" effect="plain" size="small">
              {{ row.Scope === 'global' ? '全局' : '私有' }}
            </el-tag>
          </template>
        </el-table-column>

        <el-table-column label="成员" min-width="240">
          <template #header>
            <span class="members-head">成员（按优先级）</span>
          </template>
          <template #default="{ row }">
            <el-tooltip placement="top" effect="dark">
              <template #content>
                <div class="member-tip">{{ memberDetail(row) }}</div>
              </template>
              <div class="member-cell">
                <span class="member-count">{{ (row.Members || []).length }} 个</span>
                <span class="member-names ellipsis">{{ memberPreview(row) }}</span>
              </div>
            </el-tooltip>
          </template>
        </el-table-column>

        <el-table-column label="启用" min-width="80" align="center">
          <template #default="{ row }">
            <el-tag :type="row.Enabled ? 'success' : 'info'" effect="plain" size="small">
              {{ row.Enabled ? '启用' : '停用' }}
            </el-tag>
          </template>
        </el-table-column>

        <el-table-column label="操作" width="90" align="right" fixed="right">
          <template #default="{ row }">
            <div v-if="canWrite(row)" class="op-cell" @click.stop>
              <el-tooltip content="编辑" placement="top">
                <el-icon class="op-icon" @click="router.push(`${listBase}/${row.ID}/edit`)"><Edit /></el-icon>
              </el-tooltip>
              <el-tooltip content="删除" placement="top">
                <el-icon class="op-icon danger" @click="onDelete(row)"><Delete /></el-icon>
              </el-tooltip>
            </div>
          </template>
        </el-table-column>
      </el-table>

      <el-empty v-if="!loading && !displayList.length" description="暂无 Auto 模型" />
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
}
.ellipsis {
  max-width: 220px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.members-head {
  font-weight: 600;
}
.member-cell {
  display: flex;
  align-items: center;
  gap: 8px;
  min-width: 0;
}
.member-count {
  flex: none;
  font-size: 12px;
  color: var(--color-text-secondary);
  font-variant-numeric: tabular-nums;
}
.member-names {
  font-size: 12px;
  color: var(--color-text-tertiary);
}
.member-tip {
  white-space: pre-line;
  line-height: 1.6;
  font-size: 12px;
}
</style>