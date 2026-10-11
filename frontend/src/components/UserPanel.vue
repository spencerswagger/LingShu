<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { ArrowDown, Edit, SwitchButton } from '@element-plus/icons-vue'
import { useAuthStore } from '@/stores/auth'
import { getMeSummary, type MeSummary } from '@/api/auth'
import ChangePasswordDialog from '@/components/account/ChangePasswordDialog.vue'
import MfaDialog from '@/components/account/MfaDialog.vue'

const router = useRouter()
const auth = useAuthStore()

const isAdmin = computed(() => auth.role === 'ADMIN')
const avatarText = computed(() => (auth.displayName() || 'A').slice(0, 1).toUpperCase())
const roleLabel = computed(() => (isAdmin.value ? '管理员' : '开发者'))
const balanceText = computed(() =>
  auth.balanceLoaded ? Math.round(auth.balance).toLocaleString() : '--',
)

// ===== 摘要指标（悬停展开时拉取，失败静默置空） =====
const summary = ref<MeSummary | null>(null)

async function loadSummary() {
  try {
    const res = await getMeSummary()
    summary.value = res.Data
  } catch {
    summary.value = null
  }
}

function onShow() {
  auth.fetchProfile()
  loadSummary()
}

function compact(v: number) {
  if (v >= 1e6) return `${(v / 1e6).toFixed(1)}M`
  if (v >= 1e3) return `${(v / 1e3).toFixed(1)}k`
  return String(v)
}

const throughputText = computed(() => {
  const s = summary.value
  if (!s) return '--'
  return `RPM ${Math.round(s.RPM)} · TPM ${compact(s.TPM)}`
})

// 缓存命中率（本月）= 缓存读 ÷ 输入(含缓存读)
const cacheHitText = computed(() => {
  const s = summary.value
  if (!s || !s.InputTokens) return '--'
  return `${((s.CacheReadTokens / s.InputTokens) * 100).toFixed(1)}%`
})

function goBilling(range: 'today' | 'yesterday' | 'month') {
  const path = isAdmin.value ? '/admin/billings' : '/dev/billings'
  const query: Record<string, string> = { range }
  if (isAdmin.value && auth.userId) query.user_id = auth.userId
  router.push({ path, query })
}

interface MetricItem {
  label: string
  value: string
  click?: () => void
  span?: boolean
}

const metrics = computed<MetricItem[]>(() => {
  const s = summary.value
  const fmt = (v: number | undefined) => (v == null ? '--' : Math.round(v).toLocaleString())
  const items: MetricItem[] = [
    {
      label: '令牌总数',
      value: fmt(s?.TokenCount),
      click: () => router.push(isAdmin.value ? '/admin/tokens' : '/dev/tokens'),
    },
  ]
  if (isAdmin.value) {
    items.push({
      label: '会话总数',
      value: fmt(s?.SessionCount),
      click: () => router.push('/admin/sessions'),
    })
  }
  items.push(
    { label: '今日消耗', value: fmt(s?.TodayCredits), click: () => goBilling('today') },
    { label: '昨日消耗', value: fmt(s?.YesterdayCredits), click: () => goBilling('yesterday') },
    { label: '本月消耗', value: fmt(s?.MonthCredits), click: () => goBilling('month') },
    {
      label: '缓存命中率(本月)',
      value: cacheHitText.value,
      click: () => router.push(isAdmin.value ? '/admin/dashboard' : '/dev/usage'),
    },
    {
      label: '吞吐速率',
      value: throughputText.value,
      click: () => router.push(isAdmin.value ? '/admin/dashboard' : '/dev/usage'),
      span: true,
    },
  )
  return items
})

// ===== 昵称编辑小弹窗 =====
const nameDialogVisible = ref(false)
const editName = ref('')
const saving = ref(false)

function openNameDialog() {
  editName.value = auth.nickname || auth.username || ''
  nameDialogVisible.value = true
}

async function saveName() {
  const name = editName.value.trim()
  if (!name) {
    ElMessage.warning('昵称不能为空')
    return
  }
  saving.value = true
  try {
    await auth.changeNickname(name)
    ElMessage.success('昵称已更新')
    nameDialogVisible.value = false
  } catch (e: any) {
    ElMessage.error(e?.message || '修改失败')
  } finally {
    saving.value = false
  }
}

// ===== 账号操作 =====
const pwdVisible = ref(false)
const mfaVisible = ref(false)

function onLogout() {
  auth.logout()
  router.push('/login')
}
</script>

<template>
  <el-popover
    placement="bottom-end"
    :width="360"
    trigger="hover"
    popper-class="user-popover"
    @show="onShow"
  >
    <template #reference>
      <span class="user-trigger">
        <span class="avatar">{{ avatarText }}</span>
        <span class="username">{{ auth.displayName() }}</span>
        <el-icon class="arrow"><ArrowDown /></el-icon>
      </span>
    </template>

    <div class="panel">
      <div class="panel-head">
        <span class="panel-nick">{{ auth.displayName() }}</span>
        <el-tag size="small" effect="plain">{{ roleLabel }}</el-tag>
        <el-icon class="edit-icon" @click="openNameDialog"><Edit /></el-icon>
      </div>
      <div class="panel-sub">@{{ auth.username }}</div>

      <div class="panel-balance">
        <span>积分余额</span>
        <b class="panel-balance-num">{{ balanceText }}</b>
      </div>

      <div class="metric-grid">
        <div
          v-for="m in metrics"
          :key="m.label"
          class="metric"
          :class="{ 'metric-wide': m.span, 'metric-clickable': !!m.click }"
          @click="m.click && m.click()"
        >
          <span class="metric-label">{{ m.label }}</span>
          <span class="metric-value">{{ m.value }}</span>
        </div>
      </div>

      <div class="panel-actions">
        <div class="action-row" @click="pwdVisible = true">
          <span>修改密码</span>
        </div>
        <div class="action-row" @click="mfaVisible = true">
          <span>多因素认证</span>
          <span class="action-side">{{ auth.totpEnabled ? '已开启' : '未开启' }}</span>
        </div>
      </div>

      <div class="panel-foot">
        <el-button link type="danger" class="logout-btn" @click="onLogout">
          <el-icon><SwitchButton /></el-icon>退出登录
        </el-button>
      </div>
    </div>
  </el-popover>

  <el-dialog v-model="nameDialogVisible" title="修改昵称" width="360px" append-to-body destroy-on-close>
    <el-input v-model="editName" maxlength="40" placeholder="请输入昵称" @keyup.enter="saveName" />
    <template #footer>
      <el-button @click="nameDialogVisible = false">取消</el-button>
      <el-button type="primary" :loading="saving" @click="saveName">保存</el-button>
    </template>
  </el-dialog>

  <ChangePasswordDialog v-model="pwdVisible" />
  <MfaDialog v-model="mfaVisible" />
</template>

<style scoped>
/* 触发器：整块（头像+用户名+箭头）作为悬停入口 */
.user-trigger {
  display: inline-flex;
  align-items: center;
  gap: 8px;
  padding: 6px 10px;
  border-radius: 10px;
  cursor: pointer;
  transition: background 0.15s;
}

.user-trigger:hover {
  background: var(--color-fill, #f6f7fa);
}

.user-trigger .arrow {
  font-size: 12px;
  color: var(--color-text-tertiary);
  transition: transform 0.15s;
}

.user-trigger:hover .arrow {
  transform: rotate(180deg);
}

.avatar {
  width: 30px;
  height: 30px;
  border-radius: 50%;
  background: var(--brand-gradient);
  color: #fff;
  font-size: 14px;
  font-weight: 600;
  display: inline-flex;
  align-items: center;
  justify-content: center;
}

.username {
  color: var(--color-text);
  font-weight: 500;
  font-size: 14px;
}

/* 浮窗面板 */
.panel {
  display: flex;
  flex-direction: column;
  gap: 10px;
}

.panel-head {
  display: flex;
  align-items: center;
  gap: 8px;
}

.panel-nick {
  font-size: 15px;
  font-weight: 600;
  color: var(--color-text);
}

.edit-icon {
  font-size: 14px;
  color: var(--color-text-tertiary);
  cursor: pointer;
  transition: color 0.15s;
}

.edit-icon:hover {
  color: var(--brand-primary);
}

.panel-sub {
  font-size: 12px;
  color: var(--color-text-secondary);
  font-family: 'SF Mono', Menlo, Consolas, monospace;
}

.panel-balance {
  display: flex;
  align-items: baseline;
  justify-content: space-between;
  font-size: 13px;
  color: var(--color-text-secondary);
  background: var(--color-fill, #f6f7fa);
  border-radius: 8px;
  padding: 8px 10px;
}

.panel-balance-num {
  font-size: 16px;
  font-weight: 600;
  color: var(--brand-primary);
  font-variant-numeric: tabular-nums;
}

.metric-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 8px;
}

.metric {
  display: flex;
  flex-direction: column;
  gap: 2px;
  background: var(--color-fill, #f6f7fa);
  border-radius: 8px;
  padding: 8px 10px;
}

.metric-wide {
  grid-column: span 2;
}

.metric-clickable {
  cursor: pointer;
  transition: background 0.15s;
}

.metric-clickable:hover {
  background: var(--brand-primary-soft, #eef0fe);
}

.metric-label {
  font-size: 12px;
  color: var(--color-text-secondary);
}

.metric-value {
  font-size: 15px;
  font-weight: 600;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
}

.panel-actions {
  display: flex;
  flex-direction: column;
  border-top: 1px solid var(--color-border);
  padding-top: 4px;
}

.action-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 8px 4px;
  font-size: 13px;
  color: var(--color-text);
  cursor: pointer;
  border-radius: 6px;
  transition: background 0.15s, color 0.15s;
}

.action-row:hover {
  background: var(--color-fill, #f6f7fa);
  color: var(--brand-primary);
}

.action-side {
  font-size: 12px;
  color: var(--color-text-secondary);
}

.panel-foot {
  border-top: 1px solid var(--color-border);
  padding-top: 6px;
  display: flex;
  justify-content: center;
}

.logout-btn {
  width: 100%;
}

.logout-btn .el-icon {
  margin-right: 4px;
}
</style>
