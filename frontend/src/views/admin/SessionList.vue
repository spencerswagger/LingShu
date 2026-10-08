<script setup lang="ts">
import { nextTick, onMounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Edit } from '@element-plus/icons-vue'
import { listSessions, kickSessions, renameSession, type AdminSession } from '@/api/admin'
import { fmtDate, fmtTime } from '@/utils/format'
import ErrorBubble from '@/components/ErrorBubble.vue'
import UserSelect from '@/components/UserSelect.vue'
import ColumnFilter from '@/components/ColumnFilter.vue'

const loading = ref(false)
const list = ref<AdminSession[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const selected = ref<AdminSession[]>([])
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const editingId = ref('')
const editingName = ref('')
const editingInputs = ref<Record<string, any>>({})

const filters = reactive({
  q: '',
  user_id: undefined as string | undefined,
  token_id: undefined as string | undefined,
  channel_key_id: undefined as string | undefined,
  status: undefined as string | undefined, // active | expired | closed
})

function setInputRef(el: any, id: string) {
  if (el) editingInputs.value[id] = el
}

function startRename(row: AdminSession) {
  editingId.value = row.SessionID
  editingName.value = row.SessionName || ''
  nextTick(() => {
    editingInputs.value[row.SessionID]?.focus()
  })
}

async function saveRename(row: AdminSession) {
  const id = editingId.value
  if (!id) return
  editingId.value = ''
  const name = editingName.value.trim()
  if (!name) {
    load()
    return
  }
  if (name === row.SessionName) return
  try {
    await renameSession(id, name)
    row.SessionName = name
    ElMessage.success('已重命名')
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
    load()
  }
}

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  const params: Record<string, any> = { Page: page.value, Size: size.value }
  if (filters.q.trim()) params.Q = filters.q.trim()
  if (filters.user_id) params.UserID = filters.user_id
  if (filters.token_id && filters.token_id.trim()) params.TokenID = filters.token_id.trim()
  if (filters.channel_key_id && filters.channel_key_id.trim()) params.ChannelKeyID = filters.channel_key_id.trim()
  if (filters.status === 'active') params.Expired = 'active'
  if (filters.status === 'expired') params.Expired = 'expired'
  if (filters.status === 'closed') params.Closed = '1'
  try {
    const res = await listSessions(params)
    list.value = res.Data.List || []
    total.value = res.Data.Total || 0
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
// 用户/令牌/密钥/状态筛选变化自动刷新；会话 ID 由输入框 change 触发
watch(
  () => `${filters.user_id}|${filters.token_id}|${filters.channel_key_id}|${filters.status}`,
  () => {
    page.value = 1
    load()
  },
)
function onSearch() {
  page.value = 1
  load()
}
onMounted(load)

async function close(ids: string[], label: string) {
  try {
    await ElMessageBox.confirm(
      `确认关闭选中的 ${label} 会话吗？关闭后该会话不再被请求命中（并发槽释放），记录保留在列表中。`,
      '关闭确认',
      { type: 'warning', confirmButtonText: '关闭', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    const res = await kickSessions({ SessionIDs: ids })
    ElMessage.success(`已关闭 ${res.Data.Affected} 个会话`)
    selected.value = []
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '关闭失败')
  }
}

function onCloseOne(row: AdminSession) {
  close([row.SessionID], '该')
}
function onCloseBatch() {
  if (!selected.value.length) return
  close(selected.value.map((r) => r.SessionID), `${selected.value.length} 个`)
}
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">会话</div>
      <span class="page-count">共 {{ total }} 个会话</span>
      <div class="page-header-spacer" />
      <el-button v-if="selected.length" type="danger" @click="onCloseBatch">批量关闭</el-button>
    </div>

    <div v-loading="loading" class="card">
      <el-table
        :data="list"
        border
        stripe
        class="table-nowrap"
        row-key="SessionID"
        @selection-change="(rows: AdminSession[]) => (selected = rows)"
      >
        <el-table-column type="selection" width="44" />
        <el-table-column label="会话名称 / 模型" min-width="200">
          <template #header>
            <ColumnFilter label="会话 ID" :active="!!filters.q" @clear="() => { filters.q = ''; onSearch() }">
              <el-input
                v-model="filters.q"
                placeholder="会话 ID 前缀"
                clearable
                size="small"
                @change="onSearch"
                @keyup.enter="(e: KeyboardEvent) => (e.target as HTMLInputElement)?.blur()"
              />
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <el-input
                v-if="editingId === row.SessionID"
                v-model="editingName"
                :ref="(el: any) => setInputRef(el, row.SessionID)"
                size="small"
                maxlength="100"
                placeholder="输入会话名称"
                @click.stop
                @keyup.enter="saveRename(row)"
                @blur="saveRename(row)"
              />
              <span v-else class="cell-name" @click="startRename(row)">
                <span class="name-text">{{ row.SessionName || '未命名' }}</span>
                <el-icon class="name-edit"><Edit /></el-icon>
              </span>
              <span class="t-clock">{{ row.Model || '-' }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="用户 / 令牌" min-width="150">
          <template #header>
            <ColumnFilter label="用户" :active="!!filters.user_id" @clear="() => (filters.user_id = undefined)">
              <UserSelect v-model="filters.user_id" placeholder="按用户名搜索" />
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ row.UserNickname || row.UserName || '-' }}</span>
              <span class="t-clock">{{ row.UserNickname && row.UserName ? '@' + row.UserName + ' · ' : '' }}{{ row.TokenName || '-' }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="渠道-密钥" min-width="120" show-overflow-tooltip>
          <template #header>
            <ColumnFilter
              label="渠道-密钥"
              :active="!!filters.channel_key_id"
              @clear="() => (filters.channel_key_id = undefined)"
            >
              <el-input
                v-model="filters.channel_key_id"
                placeholder="渠道密钥 ID"
                clearable
                size="small"
                @change="onSearch"
              />
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <span class="cell">{{ row.ChannelKeyName || '-' }}</span>
          </template>
        </el-table-column>
        <el-table-column label="创建 / 最近活跃" min-width="170">
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ fmtDate(row.CreatedAt) }} {{ fmtTime(row.CreatedAt) }}</span>
              <span class="t-clock">{{ fmtDate(row.LastActive) }} {{ fmtTime(row.LastActive) }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="过期时间" min-width="110">
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ fmtDate(row.ExpireAt) }}</span>
              <span class="t-clock">{{ fmtTime(row.ExpireAt) }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="状态" min-width="86" align="center">
          <template #header>
            <ColumnFilter label="状态" :active="!!filters.status" @clear="() => (filters.status = undefined)">
              <el-select v-model="filters.status" placeholder="会话状态" clearable size="small" style="width: 100%">
                <el-option label="进行中" value="active" />
                <el-option label="已过期" value="expired" />
                <el-option label="已关闭" value="closed" />
              </el-select>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <el-tag v-if="row.Closed" type="info" size="small" effect="light">已关闭</el-tag>
            <el-tag v-else-if="row.Expired" type="danger" size="small" effect="light">已过期</el-tag>
            <el-tag v-else type="success" size="small" effect="light">进行中</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="90" align="right" fixed="right">
          <template #default="{ row }">
            <el-button link type="danger" size="small" @click="onCloseOne(row)">关闭</el-button>
          </template>
        </el-table-column>
      </el-table>

      <div class="pager">
        <el-pagination
          layout="total, prev, pager, next"
          :total="total"
          :page-size="size"
          :current-page="page"
          @current-change="(p: number) => { page = p; load() }"
        />
      </div>
      <el-empty v-if="!loading && !list.length" description="暂无会话" />
      <ErrorBubble
        v-if="errInfo.message || errInfo.requestId"
        :message="errInfo.message"
        :request-id="errInfo.requestId"
      />
    </div>
  </div>
</template>

<style scoped>
.cell {
  font-size: 12px;
  color: var(--color-text-secondary);
}
.mono {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
  font-size: 12px;
}
.pager {
  display: flex;
  justify-content: flex-end;
  margin-top: 16px;
}
.t-time {
  display: flex;
  flex-direction: column;
  line-height: 1.35;
}
.t-date {
  font-size: 12px;
  color: var(--color-text);
  line-height: 1.3;
  font-variant-numeric: tabular-nums;
}
.t-clock {
  font-size: 12px;
  color: var(--color-text-tertiary);
  font-variant-numeric: tabular-nums;
}
.cell-name {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  cursor: pointer;
  max-width: 100%;
  color: var(--color-primary, #409eff);
}
.name-text {
  display: inline-block;
  max-width: 200px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.name-edit {
  font-size: 13px;
  color: var(--color-text-secondary);
}
</style>