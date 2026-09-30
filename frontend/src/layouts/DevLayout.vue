<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '@/stores/auth'
import {
  Odometer,
  Key,
  TrendCharts,
  Tickets,
  Bell,
  Wallet,
  Lock,
  ArrowDown,
  SwitchButton,
} from '@element-plus/icons-vue'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

// 开发者端菜单
const menus = [
  { path: '/dev/dashboard', title: '统计', icon: Odometer },
  { path: '/dev/tokens', title: '令牌', icon: Key },
  { path: '/dev/usage', title: '用量统计', icon: TrendCharts },
  { path: '/dev/wallet', title: '我的钱包', icon: Wallet },
  { path: '/dev/billings', title: '消费账单', icon: Tickets },
  { path: '/dev/announcements', title: '系统公告', icon: Bell },
  { path: '/account/security', title: '账号安全', icon: Lock },
]

function activeMenu() {
  const p = route.path
  if (p.startsWith('/dev/tokens')) return '/dev/tokens'
  if (p.startsWith('/dev/billings')) return '/dev/billings'
  return p
}

const avatarText = computed(() => (auth.displayName() || 'D').slice(0, 1).toUpperCase())

const roleLabel = computed(() => (auth.role === 'ADMIN' ? '管理员' : '开发者'))
const balanceText = computed(() => (auth.balanceLoaded ? auth.balance.toFixed(5) : '--'))

// 浮窗内修改本人昵称
const editName = ref('')
const saving = ref(false)
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
  } catch (e: any) {
    ElMessage.error(e?.message || '修改失败')
  } finally {
    saving.value = false
  }
}
function startEditName() {
  editName.value = auth.nickname || auth.username || ''
}
onMounted(() => {
  auth.fetchProfile()
})

// 子页（新建/编辑/详情）时，当前页标题可作为「返回父列表」的链接
const crumbBack = computed(() => {
  const parent = activeMenu()
  if (route.path !== parent && parent.startsWith('/dev/')) return parent
  return ''
})

function logout() {
  auth.logout()
  router.push('/login')
}
</script>

<template>
  <el-container class="layout">
    <el-aside width="160px" class="aside">
      <div class="logo">
        <span class="logo-text">
          <span class="logo-title">AI网关</span>
          <span class="logo-sub">AI Gateway</span>
        </span>
      </div>

      <div class="menu-wrap">
        <el-menu :default-active="activeMenu()" router class="menu">
          <el-menu-item v-for="m in menus" :key="m.path" :index="m.path">
            <el-icon><component :is="m.icon" /></el-icon>
            <span>{{ m.title }}</span>
          </el-menu-item>
        </el-menu>
      </div>
    </el-aside>

    <el-container class="body">
      <el-header class="header">
        <div class="crumb">
          <span class="crumb-home crumb-link" @click="router.push('/dev/dashboard')">首页</span>
          <span class="crumb-sep">/</span>
          <span v-if="crumbBack" class="crumb-cur crumb-link" @click="router.push(crumbBack)">{{ route.meta.title || '' }}</span>
          <span v-else class="crumb-cur">{{ route.meta.title || '' }}</span>
        </div>
        <div class="user-box">
          <el-popover
            placement="bottom-end"
            :width="280"
            trigger="hover"
            popper-class="user-popover"
            @show="startEditName"
            @before-enter="auth.fetchProfile()"
          >
            <template #reference>
              <span class="user-trigger">
                <span class="avatar">{{ avatarText }}</span>
                <span class="username">{{ auth.displayName() }}</span>
                <el-icon class="arrow"><ArrowDown /></el-icon>
              </span>
            </template>
            <div class="pop-body">
              <div class="pop-name">
                <span class="pop-nick">{{ auth.displayName() }}</span>
                <el-tag size="small" effect="plain">{{ roleLabel }}</el-tag>
              </div>
              <div class="pop-sub">@{{ auth.username }}</div>
              <div class="pop-balance">
                余额
                <b class="pop-balance-num">{{ balanceText }}</b>
                积分
              </div>
              <div class="pop-edit">
                <el-input v-model="editName" size="small" maxlength="40" placeholder="设置昵称" @keyup.enter="saveName" />
                <el-button size="small" type="primary" :loading="saving" @click="saveName">保存</el-button>
              </div>
              <div class="pop-foot">
                <el-button link type="danger" class="pop-logout" @click="logout">
                  <el-icon><SwitchButton /></el-icon>退出登录
                </el-button>
              </div>
            </div>
          </el-popover>
        </div>
      </el-header>
      <el-main class="main">
        <router-view />
      </el-main>
    </el-container>
  </el-container>
</template>

<style scoped>
.layout {
  height: 100vh;
}

/* ===== 侧边栏 ===== */
.aside {
  background: linear-gradient(180deg, #171c3b 0%, #10142b 100%);
  display: flex;
  flex-direction: column;
}

.logo {
  height: 48px;
  display: flex;
  align-items: center;
  padding: 0 14px;
  flex-shrink: 0;
}

.logo-text {
  display: flex;
  flex-direction: column;
  line-height: 1.2;
}

.logo-title {
  color: #fff;
  font-size: 15px;
  font-weight: 700;
  letter-spacing: 0.4px;
}

.logo-sub {
  color: rgba(255, 255, 255, 0.45);
  font-size: 10px;
  letter-spacing: 0.6px;
}

.menu-wrap {
  flex: 1;
  overflow-y: auto;
  padding: 6px 8px;
}

.menu {
  background: transparent;
  border-right: none;
  --el-menu-bg-color: transparent;
  --el-menu-text-color: rgba(255, 255, 255, 0.62);
  --el-menu-hover-text-color: #fff;
  --el-menu-active-color: #fff;
  --el-menu-hover-bg-color: rgba(255, 255, 255, 0.07);
  --el-menu-item-height: 38px;
  --el-menu-base-level-padding: 12px;
}

.menu :deep(.el-menu-item) {
  border-radius: 8px;
  margin-bottom: 2px;
  font-size: 14px;
  padding-right: 12px;
}

.menu :deep(.el-menu-item.is-active) {
  background: linear-gradient(90deg, rgba(99, 102, 241, 0.95), rgba(139, 92, 246, 0.75));
  color: #fff;
  font-weight: 600;
  box-shadow: 0 4px 12px rgba(99, 102, 241, 0.35);
}

.body {
  background: var(--color-bg);
}

.header {
  background: var(--color-surface);
  display: flex;
  align-items: center;
  justify-content: space-between;
  border-bottom: 1px solid var(--color-border);
  height: 64px;
  padding: 0 24px;
}

.crumb {
  font-size: 13px;
  color: var(--color-text-secondary);
  display: flex;
  align-items: center;
  gap: 8px;
}

.crumb-home {
  color: var(--color-text-tertiary);
}

.crumb-link {
  cursor: pointer;
  border-radius: 6px;
  padding: 2px 6px;
  transition: background 0.15s, color 0.15s;
}

.crumb-link:hover {
  background: var(--color-fill, #f6f7fa);
  color: var(--brand-primary, #5a5ce8);
}

.crumb-sep {
  color: var(--color-border-dark, #d0d3da);
}

.crumb-cur {
  color: var(--color-text);
  font-weight: 600;
}

.user-box {
  display: flex;
  align-items: center;
}

/* 整块（头像+用户名+箭头）作为悬停触发器 */
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

/* 用户信息浮窗 */
.pop-body {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.pop-name {
  display: flex;
  align-items: center;
  gap: 8px;
}
.pop-nick {
  font-size: 15px;
  font-weight: 600;
  color: var(--color-text);
}
.pop-sub {
  font-size: 12px;
  color: var(--color-text-secondary);
  font-family: 'SF Mono', Menlo, Consolas, monospace;
}
.pop-balance {
  display: flex;
  align-items: baseline;
  gap: 4px;
  font-size: 13px;
  color: var(--color-text-secondary);
  background: var(--color-fill, #f6f7fa);
  border-radius: 8px;
  padding: 8px 10px;
}
.pop-balance-num {
  font-size: 16px;
  font-weight: 600;
  color: var(--color-primary);
  font-variant-numeric: tabular-nums;
}
.pop-edit {
  display: flex;
  gap: 8px;
}
.pop-foot {
  border-top: 1px solid var(--color-border, #eee);
  padding-top: 6px;
  display: flex;
  justify-content: center;
}
.pop-logout {
  width: 100%;
}
.pop-logout .el-icon {
  margin-right: 4px;
}

.main {
  padding: 0;
  overflow: auto;
}
</style>