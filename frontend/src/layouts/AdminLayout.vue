<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import UserPanel from '@/components/UserPanel.vue'
import {
  Odometer,
  Connection,
  Monitor,
  Cpu,
  PriceTag,
  User,
  Key,
  ChatDotRound,
  Tickets,
  Setting,
  Bell,
  DataAnalysis,
} from '@element-plus/icons-vue'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

// 管理端菜单
const menus = [
  { path: '/admin/dashboard', title: '统计', icon: Odometer },
  { path: '/admin/service-status', title: '状态', icon: Monitor },
  { path: '/admin/channels', title: '渠道', icon: Connection },
  { path: '/admin/models', title: '模型', icon: Cpu },
  { path: '/admin/tags', title: '标签', icon: PriceTag },
  { path: '/admin/users', title: '用户', icon: User },
  { path: '/admin/tokens', title: '令牌', icon: Key },
  { path: '/admin/sessions', title: '会话', icon: ChatDotRound },
  { path: '/admin/billings', title: '账单', icon: Tickets },
  { path: '/admin/session-cost-routing', title: '会话成本', icon: DataAnalysis },
  { path: '/admin/announcements', title: '公告', icon: Bell },
  { path: '/admin/system', title: '系统', icon: Setting },
]

function activeMenu() {
  // 子页（新建/编辑/详情）高亮所属列表菜单
  const p = route.path
  for (const m of ['/admin/channels', '/admin/service-status', '/admin/models', '/admin/tags', '/admin/users', '/admin/tokens', '/admin/billings', '/admin/announcements']) {
    if (p.startsWith(m)) return m
  }
  // 渠道协议为「渠道」页内切换的视图，归入「渠道」菜单高亮
  if (p.startsWith('/admin/channel-protocols')) return '/admin/channels'
  // Auto 模型为「模型」页内切换的视图，归入「模型」菜单高亮
  if (p.startsWith('/admin/auto-models')) return '/admin/models'
  if (p.startsWith('/admin/system')) return '/admin/system'
  return p
}

onMounted(() => {
  auth.fetchProfile()
})

// 子页（新建/编辑/详情）时，当前页标题可作为「返回父列表」的链接
const crumbBack = computed(() => {
  const parent = activeMenu()
  // 非列表页本身且属于某父菜单 → 提供返回父列表路径
  if (route.path !== parent && parent.startsWith('/admin/')) return parent
  return ''
})
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

      <div class="aside-foot">
        <span class="dot" /> 系统运行正常
      </div>
    </el-aside>

    <el-container class="body">
      <el-header class="header">
        <div class="crumb">
          <span class="crumb-home crumb-link" @click="router.push('/admin/dashboard')">首页</span>
          <span class="crumb-sep">/</span>
          <span v-if="crumbBack" class="crumb-cur crumb-link" @click="router.push(crumbBack)">{{ route.meta.title || '' }}</span>
          <span v-else class="crumb-cur">{{ route.meta.title || '' }}</span>
        </div>
        <div class="user-box">
          <UserPanel />
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

.aside-foot {
  flex-shrink: 0;
  padding: 10px 14px;
  font-size: 12px;
  color: rgba(255, 255, 255, 0.4);
  display: flex;
  align-items: center;
  gap: 8px;
  border-top: 1px solid rgba(255, 255, 255, 0.06);
}

.dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: #34d399;
  box-shadow: 0 0 0 3px rgba(52, 211, 153, 0.18);
}

/* ===== 主体 ===== */
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

.main {
  padding: 0;
  overflow: auto;
}
</style>