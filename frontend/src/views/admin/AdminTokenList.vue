<script setup lang="ts">
import { onMounted, reactive, ref, watch } from 'vue'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Delete, Key, Link, Plus } from '@element-plus/icons-vue'
import { listTokens, batchDeleteTokens, createToken, getTokenSecret, listTags, type AdminToken, type AdminTag } from '@/api/admin'
import { fmtDate, fmtTime } from '@/utils/format'
import ErrorBubble from '@/components/ErrorBubble.vue'
import UserSelect from '@/components/UserSelect.vue'
import ColumnFilter from '@/components/ColumnFilter.vue'

const loading = ref(false)
const list = ref<AdminToken[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const filters = reactive({
  user_id: undefined as string | undefined,
  status: undefined as string | undefined,
})
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const selected = ref<AdminToken[]>([])

// 新建令牌弹窗
const createOpen = ref(false)
const createSaving = ref(false)
const createErr = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const createForm = ref({ UserID: null as string | null, DisplayName: '', TagID: null as string | null, ExpiresAt: null as string | null })
const tags = ref<AdminTag[]>([])
const createdPlain = ref<{ Plain: string; Display: string } | null>(null)

// 查看密钥弹窗
const secretOpen = ref(false)
const secretLoading = ref(false)
const secretPlain = ref<{ Plain: string; Display: string } | null>(null)
const secretName = ref('')

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  const params: Record<string, unknown> = { Page: page.value, Size: size.value }
  if (filters.user_id) params.UserID = filters.user_id
  if (filters.status) params.Status = filters.status
  try {
    const res = await listTokens(params)
    list.value = res.Data.List
    total.value = res.Data.Total
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
// 状态/用户筛选变化自动刷新（用户选择器以 update 驱动）
watch(() => `${filters.user_id}|${filters.status}`, (v, o) => {
  if (v === o) return
  page.value = 1
  load()
})
onMounted(() => {
  load()
  listTags({ Enabled: true }).then((r) => (tags.value = (r.Data || []).filter((t) => t.Enabled))).catch(() => (tags.value = []))
})

// ---- 新建令牌（弹窗） ----
function openCreate() {
  createOpen.value = true
  createdPlain.value = null
  createForm.value = { UserID: null, DisplayName: '', TagID: null, ExpiresAt: null }
  createErr.value = { message: '', requestId: '' }
}

async function submitCreate() {
  const uid = createForm.value.UserID
  if (!uid) return ElMessage.warning('请选择所属用户')
  if (!createForm.value.DisplayName.trim()) return ElMessage.warning('请填写令牌名称')
  createSaving.value = true
  createErr.value = { message: '', requestId: '' }
  try {
    const res = await createToken({
      UserID: uid,
      DisplayName: createForm.value.DisplayName.trim(),
      TagID: createForm.value.TagID,
      ExpiresAt: createForm.value.ExpiresAt,
    })
    createdPlain.value = res.Data
    ElMessage.success('令牌已创建')
    await load()
  } catch (e: any) {
    createErr.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    createSaving.value = false
  }
}

async function copyText(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success('密钥已复制')
  } catch {
    ElMessage.warning('复制失败，请手动选择复制')
  }
}

// ---- 查看密钥（可反复复制；敏感操作需口令二次验证） ----
async function onViewSecret(row: AdminToken) {
  let pwd = ''
  try {
    const r = await ElMessageBox.prompt('查看明文密钥需要验证当前登录口令', '口令验证', {
      inputType: 'password',
      inputPlaceholder: '当前口令',
    })
    pwd = r.value
  } catch {
    return
  }
  secretOpen.value = true
  secretLoading.value = true
  secretName.value = row.DisplayName
  secretPlain.value = null
  try {
    const res = await getTokenSecret(row.ID, pwd)
    secretPlain.value = res.Data
  } catch (e: any) {
    secretPlain.value = null
    ElMessage.error(e?.message || '查看密钥失败')
  } finally {
    secretLoading.value = false
  }
}

async function onDelete(row: AdminToken) {
  try {
    await ElMessageBox.confirm(
      `确认删除令牌「${row.DisplayName}」吗？删除后立即失效不可恢复。`,
      '删除确认',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    await batchDeleteTokens([row.ID])
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
      `确认删除选中的 ${ids.length} 个令牌吗？此操作不可恢复。`,
      '批量删除确认',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    await batchDeleteTokens(ids)
    ElMessage.success('批量删除成功')
    selected.value = []
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '批量删除失败')
  }
}

// 复制 API Base URL（网关 OpenAI 兼容入口，前端同域时取 location.origin）
function copyBaseUrl() {
  const url = `${window.location.origin}/v1`
  navigator.clipboard.writeText(url)
  ElMessage.success('Base URL 已复制：' + url)
}
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">令牌</div>
      <span class="page-count">共 {{ total }} 个令牌</span>
      <div class="page-header-spacer" />
      <el-tooltip content="复制本网关 OpenAI 兼容 API 地址（/v1），开发者按此地址拼接令牌调用" placement="top">
        <el-button :icon="Link" plain @click="copyBaseUrl">复制 Base URL</el-button>
      </el-tooltip>
      <el-button v-if="selected.length" type="danger" @click="onBatchDelete">
        批量删除
      </el-button>
      <el-button type="primary" :icon="Plus" @click="openCreate">新建令牌</el-button>
    </div>

    <div v-loading="loading" class="card">
      <el-table
        :data="list"
        border
        stripe
        class="table-nowrap"
        row-key="ID"
        @selection-change="(rows: any[]) => (selected = rows)"
      >
        <el-table-column type="selection" width="44" />
        <el-table-column label="名称 / 标识" min-width="190">
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ row.DisplayName }}</span>
              <span class="t-clock mono">{{ row.TokenDisplay }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="所属用户" min-width="100">
          <template #header>
            <ColumnFilter label="所属用户" :active="!!filters.user_id" @clear="() => (filters.user_id = undefined)">
              <UserSelect v-model="filters.user_id" placeholder="按用户名搜索" />
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ row.UserNickname || row.Username || '-' }}</span>
              <span v-if="row.UserNickname" class="t-clock">@{{ row.Username }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="状态" min-width="76" align="center">
          <template #header>
            <ColumnFilter label="状态" :active="!!filters.status" @clear="() => (filters.status = undefined)">
              <el-select v-model="filters.status" placeholder="状态" clearable size="small" style="width: 100%">
                <el-option label="启用" value="ACTIVE" />
                <el-option label="禁用" value="DISABLED" />
              </el-select>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <el-tag :type="row.Status === 'ACTIVE' ? 'success' : 'danger'" size="small" effect="light">
              {{ row.Status === 'ACTIVE' ? '启用' : '禁用' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="最近使用 / 过期" min-width="190">
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ row.LastUsedAt ? `${fmtDate(row.LastUsedAt)} ${fmtTime(row.LastUsedAt)}` : '从未使用' }}</span>
              <span class="t-clock">{{ row.ExpiresAt ? `${fmtDate(row.ExpiresAt)} ${fmtTime(row.ExpiresAt)}` : '永不过期' }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="创建时间" min-width="130">
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">{{ fmtDate(row.CreatedAt) }}</span>
              <span class="t-clock">{{ fmtTime(row.CreatedAt) }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="70" align="right" fixed="right">
          <template #default="{ row }">
            <div class="op-cell" @click.stop>
              <el-tooltip content="查看密钥" placement="top">
                <el-icon class="op-icon" @click="onViewSecret(row)"><Key /></el-icon>
              </el-tooltip>
              <el-tooltip content="删除" placement="top">
                <el-icon class="op-icon danger" @click="onDelete(row)"><Delete /></el-icon>
              </el-tooltip>
            </div>
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
      <el-empty v-if="!loading && !list.length" description="暂无令牌" />
      <ErrorBubble
        v-if="errInfo.message || errInfo.requestId"
        :message="errInfo.message"
        :request-id="errInfo.requestId"
      />
    </div>

    <!-- 新建令牌弹窗 -->
    <el-dialog v-model="createOpen" title="新建令牌" width="560px" :close-on-click-modal="false" destroy-on-close>
      <template v-if="createdPlain">
        <div class="plain-warning">令牌已创建，请复制并妥善保存。关闭后仍可在列表中随时查看/复制。</div>
        <el-input :model-value="createdPlain.Plain" readonly class="mono">
          <template #append>
            <el-button text @click="copyText(createdPlain.Plain)">复制</el-button>
          </template>
        </el-input>
        <div class="plain-tip">标识：<span class="mono">{{ createdPlain.Display }}</span></div>
      </template>
      <el-form v-else label-width="100px">
        <el-form-item label="所属用户" required>
          <UserSelect v-model="createForm.UserID" placeholder="按用户名搜索" />
        </el-form-item>
        <el-form-item label="令牌名称" required>
          <el-input v-model="createForm.DisplayName" maxlength="64" placeholder="如 生产环境" />
        </el-form-item>
        <el-form-item label="语义标签">
          <el-select v-model="createForm.TagID" placeholder="默认路由" clearable style="width: 100%">
            <el-option v-for="t in tags" :key="t.ID" :label="t.Name" :value="t.ID" />
          </el-select>
        </el-form-item>
        <el-form-item label="过期时间">
          <el-date-picker
            v-model="createForm.ExpiresAt"
            type="datetime"
            value-format="YYYY-MM-DDTHH:mm:ssZ"
            placeholder="留空 = 永不过期"
            style="width: 100%"
          />
        </el-form-item>
      </el-form>
      <div v-if="createErr.message || createErr.requestId" style="margin-bottom: 8px">
        <ErrorBubble :message="createErr.message" :request-id="createErr.requestId" />
      </div>
      <template #footer>
        <template v-if="createdPlain">
          <el-button type="primary" @click="createOpen = false">完成</el-button>
        </template>
        <template v-else>
          <el-button @click="createOpen = false">取消</el-button>
          <el-button type="primary" :loading="createSaving" @click="submitCreate">创建</el-button>
        </template>
      </template>
    </el-dialog>

    <!-- 查看密钥弹窗 -->
    <el-dialog v-model="secretOpen" :title="`查看密钥 - ${secretName}`" width="520px" :close-on-click-modal="false">
      <div v-loading="secretLoading">
        <template v-if="secretPlain">
          <el-input :model-value="secretPlain.Plain" readonly class="mono">
            <template #append>
              <el-button text @click="copyText(secretPlain.Plain)">复制</el-button>
            </template>
          </el-input>
          <div class="plain-tip">密钥可随时反复查看/复制。如怀疑泄露，可删除后新建。</div>
        </template>
        <el-empty v-else-if="!secretLoading" description="该令牌未备份密钥，可删除后新建" :image-size="60" />
      </div>
      <template #footer>
        <el-button @click="secretOpen = false">关闭</el-button>
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
  font-size: 12px;
  color: var(--color-text);
}
.t-clock {
  font-size: 12px;
  color: var(--color-text-tertiary);
  font-variant-numeric: tabular-nums;
}
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
.plain-warning {
  color: var(--color-warning);
  font-size: 13px;
  margin-bottom: 12px;
}
.plain-tip {
  font-size: 12px;
  color: var(--color-text-secondary);
  margin-top: 8px;
}
</style>