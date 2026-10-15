<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Edit, Delete } from '@element-plus/icons-vue'
import {
  listChannels,
  updateChannel,
  batchDeleteChannels,
  channelKeyState,
  type AdminChannel,
} from '@/api/admin'
import StatusTag from '@/components/StatusTag.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'
import ColumnFilter from '@/components/ColumnFilter.vue'

// 密钥聚合态子项（与 AdminChannel.KeyStates 对齐）
type KeyState = { KeyID: string; KeyName: string; State: string }

function keysOf(row: AdminChannel): KeyState[] {
  return row.KeyStates || []
}

const router = useRouter()
const loading = ref(false)
const list = ref<AdminChannel[]>([])
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

// 多选
const selected = ref<AdminChannel[]>([])

const filters = reactive({
  q: '',
})

// 前端本地过滤（全量加载）：名称/地址关键词
const displayList = computed(() => {
  const q = filters.q.trim().toLowerCase()
  return list.value.filter((c) => {
    return (
      !q ||
      (c.Name || '').toLowerCase().includes(q) ||
      (c.BaseURL || '').toLowerCase().includes(q) ||
      (c.Protocol || '').toLowerCase().includes(q)
    )
  })
})

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await listChannels()
    list.value = res.Data || []
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}

onMounted(load)

const savingId = ref<string | null>(null)
async function onEditPriority(row: AdminChannel, v: number) {
  const num = Number(v)
  if (!Number.isFinite(num) || num < 1) {
    ElMessage.warning('优先级需 ≥ 1')
    await load()
    return
  }
  const prev = row.Priority
  row.Priority = num
  savingId.value = row.ID
  try {
    await updateChannel(row.ID, {
      Name: row.Name,
      Protocol: row.Protocol,
      BaseURL: row.BaseURL,
      Priority: num,
    })
    ElMessage.success('已更新优先级')
  } catch (e: any) {
    row.Priority = prev
    ElMessage.error(e?.message || '更新失败，已还原')
  } finally {
    savingId.value = null
  }
}

// 逐密钥三态切换（popover 内）：调密钥级状态接口，成功后刷新列表（KeyStates 随之更新）
async function onSetKeyState(row: AdminChannel, key: KeyState, action: 'normal' | 'drain' | 'disable') {
  try {
    await channelKeyState(row.ID, key.KeyID, action)
    ElMessage.success('密钥状态已更新')
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '密钥状态操作失败')
  }
}

// 单个删除（二次确认）
async function onDelete(row: AdminChannel) {
  try {
    await ElMessageBox.confirm(`确认删除渠道「${row.Name}」吗？此操作不可恢复。`, '删除确认', {
      type: 'warning',
      confirmButtonText: '删除',
      cancelButtonText: '取消',
    })
  } catch {
    return // 用户取消
  }
  try {
    await batchDeleteChannels([row.ID])
    ElMessage.success('已删除')
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '删除失败')
  }
}

// 批量删除
async function onBatchDelete() {
  if (!selected.value.length) return
  const ids = selected.value.map((r) => r.ID)
  try {
    await ElMessageBox.confirm(
      `确认删除选中的 ${ids.length} 个渠道吗？此操作不可恢复。`,
      '批量删除确认',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    await batchDeleteChannels(ids)
    ElMessage.success('批量删除成功')
    selected.value = []
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '批量删除失败')
  }
}

// 点击整行进入详情；点击操作列（开关/图标按钮）不跳转
function onRowClick(row: AdminChannel, _column: unknown, event: Event) {
  const el = event?.target as HTMLElement | null
  if (el?.closest('.op-cell') || el?.closest('.row-stop')) return
  router.push(`/admin/channels/${row.ID}`)
}
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">渠道</div>
      <span class="page-count">{{ list.length }} 个渠道</span>
      <div class="page-header-spacer" />
      <el-button
        v-if="selected.length"
        type="danger"
        @click="onBatchDelete"
      >
        批量删除
      </el-button>
      <el-button type="primary" @click="router.push('/admin/channels/create')">新建渠道</el-button>
    </div>

    <div v-loading="loading" class="card">
      <el-table
        :data="displayList"
        border
        stripe
        class="table-nowrap clickable-rows"
        row-key="ID"
        @selection-change="(rows: any[]) => (selected = rows)"
        @row-click="onRowClick"
      >
        <el-table-column type="selection" width="44" />
        <el-table-column label="名称 / 地址" min-width="220">
          <template #header>
            <ColumnFilter label="名称 / 地址" :active="!!filters.q" @clear="() => (filters.q = '')">
              <el-input
                v-model="filters.q"
                placeholder="按名称、地址或协议搜索"
                clearable
                size="small"
                @keyup.enter="(e: KeyboardEvent) => (e.target as HTMLInputElement)?.blur()"
              />
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ row.Name }}</span>
              <span class="t-clock ellipsis">{{ row.BaseURL }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="状态（按密钥）" min-width="160" align="center">
          <template #default="{ row }">
            <el-popover trigger="hover" placement="left" :width="300">
              <template #reference>
                <div class="key-state-cell">
                  <template v-if="keysOf(row).length">
                    <span
                      v-for="k in keysOf(row)"
                      :key="k.KeyID"
                      class="key-state-chip"
                      :title="k.KeyName"
                    >
                      <StatusTag :value="k.State" />
                    </span>
                  </template>
                  <el-tag v-else type="info" size="small" effect="light">无密钥</el-tag>
                </div>
              </template>
              <div class="pop-section">
                <span class="pop-label">各密钥状态（渠道可用性以密钥为准）</span>
              </div>
              <template v-if="keysOf(row).length">
                <div v-for="k in keysOf(row)" :key="k.KeyID" class="key-row">
                  <span class="key-name" :title="k.KeyName">{{ k.KeyName }}</span>
                  <StatusTag :value="k.State" />
                  <div class="pop-actions">
                    <el-button v-if="k.State !== 'NORMAL'" size="small" type="success" @click="onSetKeyState(row, k, 'normal')">正常</el-button>
                    <el-button v-if="k.State !== 'DRAIN'" size="small" type="warning" @click="onSetKeyState(row, k, 'drain')">排空</el-button>
                    <el-button v-if="k.State !== 'DISABLED'" size="small" type="danger" @click="onSetKeyState(row, k, 'disable')">禁用</el-button>
                  </div>
                </div>
              </template>
              <div v-else class="pop-empty">该渠道暂无密钥</div>
            </el-popover>
          </template>
        </el-table-column>
        <el-table-column label="标签" min-width="150">
          <template #default="{ row }">
            <template v-if="row.BoundTags && row.BoundTags.length">
              <el-tag v-for="t in row.BoundTags" :key="t.ID" size="small" effect="plain" class="tag-chip">
                {{ t.Name }}
              </el-tag>
            </template>
            <span v-else class="tag-empty">—</span>
          </template>
        </el-table-column>
        <el-table-column label="优先级" width="130" align="center">
          <template #header>
            <el-tooltip content="数字越大越先被路由；同优先级内新会话选当前成本最低者" placement="top"><span>优先级</span></el-tooltip>
          </template>
          <template #default="{ row }">
            <div class="row-stop" @click.stop>
              <el-input-number
                :model-value="row.Priority"
                :min="1" :max="999" :step="1" size="small"
                controls-position="right"
                class="inline-num"
                @change="(v: number) => onEditPriority(row, v)"
              />
            </div>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="110" align="right" fixed="right">
          <template #default="{ row }">
            <div class="op-cell" @click.stop>
              <el-tooltip content="编辑" placement="top">
                <el-icon class="op-icon" @click="router.push(`/admin/channels/${row.ID}`)"><Edit /></el-icon>
              </el-tooltip>
              <el-tooltip content="删除" placement="top">
                <el-icon class="op-icon danger" @click="onDelete(row)"><Delete /></el-icon>
              </el-tooltip>
            </div>
          </template>
        </el-table-column>
      </el-table>

      <el-empty v-if="!loading && !displayList.length" description="暂无渠道" />
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
  max-width: 200px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.key-state-cell {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
  justify-content: center;
  cursor: pointer;
}
.key-state-chip {
  display: inline-flex;
}
.pop-actions {
  display: flex;
  gap: 6px;
  flex-wrap: wrap;
}
.pop-section {
  margin-bottom: 8px;
}
.pop-section:last-of-type {
  margin-bottom: 0;
}
.pop-label {
  display: block;
  font-size: 12px;
  color: var(--color-text-tertiary);
  margin-bottom: 4px;
}
.key-row {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 4px 0;
  border-top: 1px solid rgba(128, 128, 128, 0.15);
}
.key-row:first-of-type {
  border-top: none;
}
.key-name {
  flex: 1;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 12px;
  color: var(--color-text);
}
.pop-empty {
  font-size: 12px;
  color: var(--color-text-tertiary);
  padding: 4px 0;
}
.tag-chip {
  margin: 2px 4px 2px 0;
}
.tag-empty {
  color: var(--color-text-tertiary);
}
.inline-num {
  width: 100%;
}
.inline-num :deep(.el-input__inner) {
  text-align: center;
}
</style>