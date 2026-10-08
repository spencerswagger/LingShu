<script setup lang="ts">
import { onMounted, reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { EditPen, Lock } from '@element-plus/icons-vue'
import { listUsers, updateUser, resetUserPassword, batchDeleteUsers, type AdminUser } from '@/api/admin'
import ErrorBubble from '@/components/ErrorBubble.vue'
import ColumnFilter from '@/components/ColumnFilter.vue'
import WalletChargeDlg from '@/components/WalletChargeDlg.vue'

const router = useRouter()
const loading = ref(false)
const list = ref<AdminUser[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const selected = ref<AdminUser[]>([])

const filters = reactive({
  q: '',
  role: undefined as string | undefined,
  status: undefined as string | undefined,
})

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  const params: Record<string, unknown> = { page: page.value, size: size.value }
  if (filters.q.trim()) params.q = filters.q.trim()
  if (filters.role) params.role = filters.role
  if (filters.status) params.status = filters.status
  try {
    const res = await listUsers(params)
    list.value = res.data.list
    total.value = res.data.total
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
// 角色/状态下拉变化自动刷新；用户名搜索由输入框 change 触发
watch(
  () => `${filters.role}|${filters.status}`,
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

// 积分数值（千分位，最多 4 位小数）
function fmtCredits(n?: number): string {
  if (n == null || isNaN(n)) return '-'
  return n.toLocaleString(undefined, { maximumFractionDigits: 4 })
}

// 余额累计进度条：总额 = 余额 + 累计消费，进度 = 余额占比，按占比配色
function barPct(row: AdminUser): number {
  const total = (row.Balance || 0) + (row.TotalSpent || 0)
  if (total <= 0) return 0
  return Math.max(2, Math.round((row.Balance / total) * 100))
}
function barClass(row: AdminUser): string {
  const total = (row.Balance || 0) + (row.TotalSpent || 0)
  if (total <= 0) return 'off'
  const pct = row.Balance / total
  if (pct >= 0.5) return 'ok'
  if (pct >= 0.2) return 'warn'
  return 'danger'
}

// 通用保存：角色/状态/计价模式 hover 修改入口（PUT 需携带全部字段）
async function saveAttr(row: AdminUser, payload: { Role?: string; Status?: string; PricingMode?: string }, msg: string) {
  try {
    const res = await updateUser(row.ID, {
      Role: row.Role,
      Status: row.Status,
      PricingMode: row.PricingMode,
      Nickname: row.Nickname || '',
      ...payload,
    })
    if (payload.Role) row.Role = res.data.Role ?? payload.Role
    if (payload.Status) row.Status = res.data.Status ?? payload.Status
    if (payload.PricingMode) row.PricingMode = res.data.PricingMode ?? payload.PricingMode
    ElMessage.success(msg)
  } catch (e: any) {
    ElMessage.error(e?.message || '更新失败')
  }
}

// 昵称编辑（用户名旁铅笔 → popover）
const nickEditId = ref<string | null>(null)
const nickDraft = ref('')
function openNickEdit(row: AdminUser) {
  nickEditId.value = nickEditId.value === row.ID ? null : row.ID
  nickDraft.value = row.Nickname || ''
}
async function saveNick(row: AdminUser) {
  try {
    const res = await updateUser(row.ID, {
      Role: row.Role,
      Status: row.Status,
      PricingMode: row.PricingMode,
      Nickname: nickDraft.value.trim(),
    })
    row.Nickname = res.data.Nickname ?? nickDraft.value.trim()
    nickEditId.value = null
    ElMessage.success('昵称已更新')
  } catch (e: any) {
    ElMessage.error(e?.message || '修改昵称失败')
  }
}

// 充值弹窗（统一组件）
const chargeDlg = ref(false)
const chargeUser = ref<AdminUser | null>(null)
function openRecharge(row: AdminUser) {
  chargeUser.value = row
  chargeDlg.value = true
}

// 重置密码弹窗（管理员输入新密码）
const resetDlg = ref(false)
const resetUser = ref<AdminUser | null>(null)
const resetPwd = ref('')
const resetPwd2 = ref('')
const resetLoading = ref(false)
function openReset(row: AdminUser) {
  resetUser.value = row
  resetPwd.value = ''
  resetPwd2.value = ''
  resetDlg.value = true
}
async function submitReset() {
  const u = resetUser.value
  if (!u) return
  if (!resetPwd.value) return ElMessage.warning('请输入新密码')
  if (resetPwd.value !== resetPwd2.value) return ElMessage.warning('两次输入的密码不一致')
  resetLoading.value = true
  try {
    await resetUserPassword(u.ID, resetPwd.value)
    ElMessage.success(`已重置「${u.Username}」的密码`)
    resetDlg.value = false
  } catch (e: any) {
    ElMessage.error(e?.message || '重置密码失败')
  } finally {
    resetLoading.value = false
  }
}

// 徽标映射
const roleOptions: Record<string, { label: string; type: 'success' | 'warning' | 'danger' | 'info' | 'primary' }> = {
  ADMIN: { label: '管理员', type: 'danger' },
  DEVELOPER: { label: '开发者', type: 'primary' },
}
const statusOptions: Record<string, { label: string; type: 'success' | 'warning' | 'danger' | 'info' | 'primary' }> = {
  ACTIVE: { label: '正常', type: 'success' },
  DISABLED: { label: '禁用', type: 'danger' },
}
const pricingOptions: Record<string, { label: string; type: 'success' | 'warning' | 'danger' | 'info' | 'primary' }> = {
  sale: { label: '按售价', type: 'success' },
  cost: { label: '按成本', type: 'warning' },
}

async function onBatchDelete() {
  if (!selected.value.length) return
  const ids = selected.value.map((r) => r.ID)
  try {
    await ElMessageBox.confirm(
      `确认删除选中的 ${ids.length} 个用户吗？存在账单记录的用户将无法删除。`,
      '批量删除确认',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    await batchDeleteUsers(ids)
    ElMessage.success('批量删除成功')
    selected.value = []
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '批量删除失败')
  }
}

// 点击整行进入用户详情；点击内联控件（select/hover tag/操作列）不跳转
function onRowClick(row: AdminUser, _column: unknown, event: Event) {
  const el = event?.target as HTMLElement | null
  if (el?.closest('.no-nav')) return
  router.push(`/admin/users/${row.ID}`)
}
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">用户</div>
      <span class="page-count">共 {{ total }} 个用户</span>
      <div class="page-header-spacer" />
      <el-button v-if="selected.length" type="danger" @click="onBatchDelete">
        批量删除
      </el-button>
      <el-button type="primary" @click="router.push('/admin/users/create')">新建用户</el-button>
    </div>

    <div v-loading="loading" class="card">
      <el-table
        :data="list"
        border
        stripe
        class="table-nowrap clickable-rows"
        row-key="ID"
        @selection-change="(rows: any[]) => (selected = rows)"
        @row-click="onRowClick"
      >
        <el-table-column type="selection" width="44" />
        <el-table-column label="用户" min-width="180">
          <template #header>
            <ColumnFilter label="用户名" :active="!!filters.q" @clear="() => { filters.q = ''; onSearch() }">
              <el-input
                v-model="filters.q"
                placeholder="按用户名搜索"
                clearable
                size="small"
                @change="onSearch"
                @keyup.enter="(e: KeyboardEvent) => (e.target as HTMLInputElement)?.blur()"
              />
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <div class="t-time">
              <span class="t-date">
                {{ row.Nickname || row.Username }}
                <el-popover
                  :visible="nickEditId === row.ID"
                  trigger="click"
                  placement="bottom-start"
                  :width="220"
                  @hide="nickEditId = null"
                >
                  <template #reference>
                    <el-icon class="nick-edit no-nav" @click.stop="openNickEdit(row)"><EditPen /></el-icon>
                  </template>
                  <el-input v-model="nickDraft" maxlength="40" placeholder="设置昵称" @keyup.enter="saveNick(row)" />
                  <div class="nick-actions">
                    <el-button size="small" type="primary" @click="saveNick(row)">保存</el-button>
                  </div>
                </el-popover>
              </span>
              <span class="t-clock">@{{ row.Username }}</span>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="角色" width="96" align="center">
          <template #header>
            <ColumnFilter label="角色" :active="!!filters.role" @clear="() => (filters.role = undefined)">
              <el-select v-model="filters.role" placeholder="角色" clearable size="small" style="width: 100%">
                <el-option v-for="(v, k) in roleOptions" :key="k" :label="v.label" :value="k" />
              </el-select>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <el-popover trigger="hover" placement="left" :width="180">
              <template #reference>
                <el-tag
                  class="hover-tag no-nav"
                  :type="(roleOptions[row.Role]?.type as any) || 'info'"
                  size="small"
                  effect="light"
                  @click.stop
                >
                  {{ roleOptions[row.Role]?.label || row.Role }}
                </el-tag>
              </template>
              <div class="pop-section">
                <span class="pop-label">修改角色</span>
                <div class="pop-actions">
                  <el-button v-if="row.Role !== 'ADMIN'" size="small" type="danger" plain @click="saveAttr(row, { Role: 'ADMIN' }, '已设为管理员')">管理员</el-button>
                  <el-button v-if="row.Role !== 'DEVELOPER'" size="small" type="primary" plain @click="saveAttr(row, { Role: 'DEVELOPER' }, '已设为开发者')">开发者</el-button>
                </div>
              </div>
            </el-popover>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="92" align="center">
          <template #header>
            <ColumnFilter label="状态" :active="!!filters.status" @clear="() => (filters.status = undefined)">
              <el-select v-model="filters.status" placeholder="状态" clearable size="small" style="width: 100%">
                <el-option v-for="(v, k) in statusOptions" :key="k" :label="v.label" :value="k" />
              </el-select>
            </ColumnFilter>
          </template>
          <template #default="{ row }">
            <el-popover trigger="hover" placement="left" :width="180">
              <template #reference>
                <el-tag
                  class="hover-tag no-nav"
                  :type="(statusOptions[row.Status]?.type as any) || 'info'"
                  size="small"
                  effect="light"
                  @click.stop
                >
                  {{ statusOptions[row.Status]?.label || row.Status }}
                </el-tag>
              </template>
              <div class="pop-section">
                <span class="pop-label">修改状态</span>
                <div class="pop-actions">
                  <el-button v-if="row.Status !== 'ACTIVE'" size="small" type="success" plain @click="saveAttr(row, { Status: 'ACTIVE' }, '已启用')">正常</el-button>
                  <el-button v-if="row.Status !== 'DISABLED'" size="small" type="danger" plain @click="saveAttr(row, { Status: 'DISABLED' }, '已禁用')">禁用</el-button>
                </div>
              </div>
            </el-popover>
          </template>
        </el-table-column>
        <el-table-column label="余额 / 累计消费" min-width="220">
          <template #default="{ row }">
            <div class="bal-cell no-nav" @click.stop>
              <div class="bal-head">
                <div class="bal-col">
                  <span class="bal-label">余额</span>
                  <span class="bal-num">{{ fmtCredits(row.Balance) }}</span>
                </div>
                <div class="bal-col right">
                  <span class="bal-label">累计消费</span>
                  <span class="bal-num spent">{{ fmtCredits(row.TotalSpent) }}</span>
                </div>
                <el-button link type="primary" size="small" class="bal-recharge" @click.stop="openRecharge(row)">充值</el-button>
              </div>
              <div class="bal-track">
                <div class="bal-bar" :class="barClass(row)" :style="{ width: barPct(row) + '%' }" />
              </div>
            </div>
          </template>
        </el-table-column>
        <el-table-column label="计价模式" width="100" align="center">
          <template #default="{ row }">
            <el-popover trigger="hover" placement="left" :width="180">
              <template #reference>
                <el-tag
                  class="hover-tag no-nav"
                  :type="(pricingOptions[row.PricingMode]?.type as any) || 'info'"
                  size="small"
                  effect="light"
                  @click.stop
                >
                  {{ pricingOptions[row.PricingMode]?.label || row.PricingMode }}
                </el-tag>
              </template>
              <div class="pop-section">
                <span class="pop-label">修改计价模式</span>
                <div class="pop-actions">
                  <el-button v-if="row.PricingMode !== 'sale'" size="small" type="success" plain @click="saveAttr(row, { PricingMode: 'sale' }, '已切换为按售价')">按售价</el-button>
                  <el-button v-if="row.PricingMode !== 'cost'" size="small" type="warning" plain @click="saveAttr(row, { PricingMode: 'cost' }, '已切换为按成本')">按成本</el-button>
                </div>
              </div>
            </el-popover>
          </template>
        </el-table-column>
        <el-table-column label="操作" width="80" align="center" class-name="op-cell">
          <template #default="{ row }">
            <div class="op-cell" @click.stop>
              <el-tooltip content="重置密码" placement="top">
                <el-icon class="op-icon" @click="openReset(row)"><Lock /></el-icon>
              </el-tooltip>
            </div>
          </template>
        </el-table-column>
      </el-table>

      <!-- 充值弹窗（统一组件） -->
      <WalletChargeDlg
        v-model="chargeDlg"
        :user-id="chargeUser?.ID ?? ''"
        :username="chargeUser?.Username || ''"
        :nickname="chargeUser?.Nickname"
        :balance="chargeUser?.Balance ?? 0"
        @success="load"
      />

      <!-- 重置密码弹窗 -->
      <el-dialog v-model="resetDlg" title="重置密码" width="420px" :close-on-click-modal="false">
        <el-form label-width="90px">
          <el-form-item label="用户">
            <span>{{ resetUser?.Nickname || resetUser?.Username }}</span>
            <span v-if="resetUser?.Nickname" class="dlg-sub">@{{ resetUser?.Username }}</span>
          </el-form-item>
          <el-form-item label="新密码">
            <el-input v-model="resetPwd" type="password" show-password placeholder="输入新登录密码" />
          </el-form-item>
          <el-form-item label="确认密码">
            <el-input v-model="resetPwd2" type="password" show-password placeholder="再次输入新密码" @keyup.enter="submitReset" />
          </el-form-item>
        </el-form>
        <template #footer>
          <el-button @click="resetDlg = false">取消</el-button>
          <el-button type="primary" :loading="resetLoading" @click="submitReset">确认重置</el-button>
        </template>
      </el-dialog>

      <div class="pager">
        <el-pagination
          layout="total, prev, pager, next"
          :total="total"
          :page-size="size"
          :current-page="page"
          @current-change="(p: number) => { page = p; load() }"
        />
      </div>
      <el-empty v-if="!loading && !list.length" description="暂无用户" />
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
  display: inline-flex;
  align-items: center;
  gap: 4px;
  font-size: 12px;
  color: var(--color-text);
}
.t-clock {
  font-size: 12px;
  color: var(--color-text-tertiary);
  font-variant-numeric: tabular-nums;
}
.nick-edit {
  cursor: pointer;
  color: var(--color-text-tertiary);
  font-size: 12px;
  transition: color 0.15s;
}
.nick-edit:hover {
  color: var(--brand-primary);
}
.nick-actions {
  display: flex;
  justify-content: flex-end;
  margin-top: 8px;
}
.hover-tag {
  cursor: pointer;
}
.pop-section {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.pop-label {
  font-size: 12px;
  color: var(--color-text-secondary);
}
.pop-actions {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}
.bal-cell {
  display: flex;
  flex-direction: column;
  gap: 6px;
  padding: 2px 0;
}
.bal-head {
  display: flex;
  align-items: baseline;
  gap: 14px;
}
.bal-col {
  display: flex;
  flex-direction: column;
}
.bal-col.right {
  margin-right: auto;
  text-align: right;
}
.bal-label {
  font-size: 11px;
  color: var(--color-text-tertiary);
}
.bal-num {
  font-size: 13px;
  font-weight: 600;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
}
.bal-num.spent {
  color: var(--color-text-secondary);
}
.bal-recharge {
  padding: 0;
  font-size: 12px;
}
.bal-track {
  height: 5px;
  border-radius: 3px;
  background: var(--color-bg-muted, #ececec);
  overflow: hidden;
}
.bal-bar {
  height: 100%;
  border-radius: 3px;
  transition: width 0.2s;
}
.bal-bar.off {
  background: transparent;
}
.bal-bar.ok {
  background: var(--color-success);
}
.bal-bar.warn {
  background: var(--color-warning);
}
.bal-bar.danger {
  background: var(--color-danger);
}
.op-cell {
  display: flex;
  align-items: center;
  justify-content: center;
}
.dlg-sub {
  font-size: 12px;
  color: var(--color-text-tertiary);
  margin-left: 4px;
}
.pager {
  display: flex;
  justify-content: flex-end;
  margin-top: 16px;
}
</style>