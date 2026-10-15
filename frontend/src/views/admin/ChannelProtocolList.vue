<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus, Edit, Delete } from '@element-plus/icons-vue'
import {
  listChannelProtocols,
  deleteChannelProtocol,
  syncChannelProtocols,
  type ChannelProtocolView,
  type ChannelProtocolSyncPending,
} from '@/api/admin'
import ErrorBubble from '@/components/ErrorBubble.vue'

const router = useRouter()
const loading = ref(false)
const list = ref<ChannelProtocolView[]>([])
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

// 同步预设：先以 confirm=false 试探（未编辑条目直接覆盖、已编辑条目进入 Pending），
// 若存在 Pending 则弹确认框展示差异，用户确认后再以 confirm=true 执行覆盖。
const syncing = ref(false)
const syncDialogOpen = ref(false)
const pending = ref<ChannelProtocolSyncPending[]>([])
const appliedDirect = ref(0)

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await listChannelProtocols()
    list.value = res.Data || []
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
onMounted(load)

// 状态徽标：来源失效 > 已编辑 > 同步
function statusOf(row: ChannelProtocolView): { text: string; type: 'success' | 'warning' | 'danger' | 'info' } {
  if (row.SourceStatus === 'STALE') return { text: '来源已失效', type: 'danger' }
  if (row.Edited) return { text: '已编辑', type: 'warning' }
  return { text: '同步', type: 'info' }
}

async function onSync() {
  syncing.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await syncChannelProtocols(false)
    const data = res.Data
    appliedDirect.value = data?.Applied || 0
    pending.value = data?.Pending || []
    if (pending.value.length) {
      syncDialogOpen.value = true
    } else {
      ElMessage.success(`同步完成，已覆盖 ${appliedDirect.value} 个协议`)
      await load()
    }
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    syncing.value = false
  }
}

// 确认覆盖已编辑条目
async function confirmSync() {
  syncing.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await syncChannelProtocols(true)
    const applied = res.Data?.Applied || 0
    ElMessage.success(`同步完成，共覆盖 ${appliedDirect.value + applied} 个协议`)
    syncDialogOpen.value = false
    await load()
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    syncing.value = false
  }
}

async function onDelete(row: ChannelProtocolView) {
  try {
    await ElMessageBox.confirm(`确认删除渠道协议「${row.Name}」吗？引用它的渠道将回退到内联配置。`, '删除确认', {
      type: 'warning',
      confirmButtonText: '删除',
      cancelButtonText: '取消',
    })
  } catch {
    return
  }
  try {
    await deleteChannelProtocol(row.ID)
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
      <div class="page-title">渠道协议</div>
      <span class="page-count">{{ list.length }} 个协议</span>
      <div class="page-header-spacer" />
      <el-button :loading="syncing" @click="onSync">同步预设</el-button>
      <el-button type="primary" :icon="Plus" @click="router.push('/admin/channel-protocols/create')">新建协议</el-button>
    </div>

    <div v-loading="loading" class="card">
      <el-table :data="list" border stripe class="table-nowrap" row-key="ID">
        <el-table-column label="名称" min-width="200">
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ row.Name }}</span>
              <span class="t-clock ellipsis">{{ row.BaseURL }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="来源预设 ID" min-width="150" show-overflow-tooltip>
          <template #default="{ row }">{{ row.PresetSourceID || '—' }}</template>
        </el-table-column>
        <el-table-column label="状态" width="130" align="center">
          <template #default="{ row }">
            <el-tag :type="statusOf(row).type" size="small" effect="light">{{ statusOf(row).text }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="110" align="right" fixed="right">
          <template #default="{ row }">
            <div class="op-cell">
              <el-tooltip content="编辑" placement="top">
                <el-icon class="op-icon" @click="router.push(`/admin/channel-protocols/${row.ID}/edit`)"><Edit /></el-icon>
              </el-tooltip>
              <el-tooltip content="删除" placement="top">
                <el-icon class="op-icon danger" @click="onDelete(row)"><Delete /></el-icon>
              </el-tooltip>
            </div>
          </template>
        </el-table-column>
      </el-table>

      <el-empty v-if="!loading && !list.length" description="暂无渠道协议（可在设置预设包地址后点击「同步预设」拉取）" />
      <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
    </div>

    <!-- 同步确认：展示每个待覆盖条目的差异 -->
    <el-dialog v-model="syncDialogOpen" title="同步预设确认" width="720px">
      <div class="sync-tip">
        以下 {{ pending.length }} 个协议已被管理员编辑，同步预设将<b>覆盖</b>其现有配置。请确认差异后继续。
      </div>
      <div v-for="p in pending" :key="p.Id" class="pending-item">
        <div class="pending-head">
          <span class="pending-name">{{ p.Name }}</span>
          <el-tag v-if="p.Stale" type="danger" size="small" effect="light">来源已失效</el-tag>
          <el-tag v-else-if="p.Edited" type="warning" size="small" effect="light">已编辑</el-tag>
          <el-tag v-else type="info" size="small" effect="light">同步</el-tag>
        </div>
        <pre class="pending-diff">{{ p.Diff || '（无差异描述）' }}</pre>
      </div>
      <template #footer>
        <el-button @click="syncDialogOpen = false">取消</el-button>
        <el-button type="primary" :loading="syncing" @click="confirmSync">确认覆盖</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.t-time {
  display: flex;
  flex-direction: column;
  line-height: 1.35;
}
.t-date {
  font-size: 13px;
  color: var(--color-text);
}
.t-clock {
  font-size: 12px;
  color: var(--color-text-tertiary);
}
.ellipsis {
  max-width: 320px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.sync-tip {
  font-size: 13px;
  color: var(--color-text-secondary);
  background: var(--el-color-warning-light-9);
  border: 1px dashed var(--el-color-warning-light-5);
  border-radius: 8px;
  padding: 8px 12px;
  margin-bottom: 12px;
}
.pending-item {
  border: 1px solid var(--color-border);
  border-radius: 8px;
  padding: 10px 12px;
  margin-bottom: 10px;
}
.pending-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 6px;
}
.pending-name {
  font-weight: 600;
}
.pending-diff {
  margin: 0;
  font-size: 12px;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
  color: var(--color-text-secondary);
  background: var(--color-fill, #f6f7fa);
  border-radius: 6px;
  padding: 8px 10px;
  max-height: 220px;
  overflow: auto;
}
</style>
