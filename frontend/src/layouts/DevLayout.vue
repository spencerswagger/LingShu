<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import UserPanel from '@/components/UserPanel.vue'
import {
  Odometer,
  Key,
  Cpu,
  ChatDotRound,
  Tickets,
} from '@element-plus/icons-vue'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

// 开发者端菜单
const menus = [
  { path: '/dev/dashboard', title: '统计', icon: Odometer },
  { path: '/dev/tokens', title: '令牌', icon: Key },
  { path: '/dev/models', title: '模型', icon: Cpu },
  { path: '/dev/sessions', title: '会话', icon: ChatDotRound },
  { path: '/dev/billings', title: '账单', icon: Tickets },
]

function activeMenu() {
  const p = route.path
  if (p.startsWith('/dev/tokens')) return '/dev/tokens'
  if (p.startsWith('/dev/models')) return '/dev/models'
  if (p.startsWith('/dev/sessions')) return '/dev/sessions'
  if (p.startsWith('/dev/billings')) return '/dev/billings'
  return p
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