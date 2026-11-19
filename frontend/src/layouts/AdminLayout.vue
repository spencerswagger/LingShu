<script setup lang="ts">
import { computed, onMounted } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuthStore } from '@/stores/auth'
import UserPanel from '@/components/UserPanel.vue'

const route = useRoute()
const router = useRouter()
const auth = useAuthStore()

// 管理端菜单：分「运营 / 系统」两组，与已确认原型一致。
// 会话页与 V1 成本页在 V2 下线，渠道族/标签/Auto 模型/服务状态同理（页面已删）。
type Item = { path: string; title: string }
const groups: { label: string; items: Item[] }[] = [
  {
    label: '运营',
    items: [
      { path: '/admin/dashboard', title: '统计' },
      { path: '/admin/topology', title: '拓扑' },
      { path: '/admin/ledger', title: '账本' },
    ],
  },
  {
    label: '系统',
    items: [
      { path: '/admin/users', title: '用户' },
      { path: '/admin/tokens', title: '令牌' },
      { path: '/admin/billings', title: '账单' },
      { path: '/admin/announcements', title: '公告' },
      { path: '/admin/system', title: '系统' },
    ],
  },
]

function activeMenu() {
  const p = route.path
  for (const g of groups) {
    for (const m of g.items) {
      if (p === m.path || p.startsWith(m.path + '/')) return m.path
    }
  }
  return p
}

onMounted(() => {
  auth.fetchProfile()
})

const crumbBack = computed(() => {
  const parent = activeMenu()
  if (route.path !== parent && parent.startsWith('/admin/')) return parent
  return ''
})
</script>

<template>
  <div class="layout">
    <aside class="side">
      <div class="brand">
        <img src="/brand/lockup.svg" alt="灵枢" />
      </div>
      <nav class="nav">
        <template v-for="g in groups" :key="g.label">
          <div class="grp">{{ g.label }}</div>
          <router-link
            v-for="m in g.items"
            :key="m.path"
            :to="m.path"
            class="it"
            :class="{ on: activeMenu() === m.path }"
          >{{ m.title }}</router-link>
        </template>
      </nav>
      <div class="side-foot">v0.14 · 网关</div>
    </aside>

    <div class="main">
      <header class="top">
        <div class="crumb">
          <span class="crumb-link" @click="router.push('/admin/dashboard')">首页</span>
          <span class="sep">/</span>
          <span v-if="crumbBack" class="crumb-link" @click="router.push(crumbBack)">{{ route.meta.title || '' }}</span>
          <span v-else class="cur">{{ route.meta.title || '' }}</span>
        </div>
        <div class="sp" />
        <UserPanel />
      </header>
      <main class="body">
        <router-view />
      </main>
    </div>
  </div>
</template>

<style scoped>
.layout {
  display: flex;
  min-height: 100vh;
}

/* ---------- 侧栏 ---------- */
.side {
  width: 176px;
  flex: none;
  border-right: 1px solid var(--line);
  background: var(--bg-2);
  display: flex;
  flex-direction: column;
  position: sticky;
  top: 0;
  height: 100vh;
}
.brand {
  display: flex;
  align-items: center;
  padding: 16px 16px 14px;
}
.brand img {
  width: 112px;
  height: auto;
  display: block;
}
.nav {
  padding: 4px 10px;
  overflow: auto;
  flex: 1;
}
.grp {
  font-size: 10px;
  letter-spacing: 1px;
  color: var(--ink-4);
  padding: 12px 8px 5px;
}
.it {
  display: flex;
  align-items: center;
  height: 33px;
  padding: 0 9px 0 12px;
  border-radius: 8px;
  color: var(--ink-2);
  font-size: 12.5px;
  margin-bottom: 2px;
  border: 1px solid transparent;
  position: relative;
}
.it:hover {
  background: #0f1626;
  color: var(--ink);
}
.it.on {
  background: #101728;
  border-color: #243357;
  color: #dbe6ff;
}
.it.on::before {
  content: '';
  position: absolute;
  left: 4px;
  width: 3px;
  height: 14px;
  border-radius: 2px;
  background: var(--acc-2);
}
.side-foot {
  padding: 11px 14px;
  border-top: 1px solid var(--line);
  font-size: 10.5px;
  color: var(--ink-4);
}

/* ---------- 主区 ---------- */
.main {
  flex: 1;
  min-width: 0;
  display: flex;
  flex-direction: column;
  min-height: 100vh;
}
.top {
  display: flex;
  align-items: center;
  gap: 12px;
  padding: 13px 22px;
  border-bottom: 1px solid var(--line);
  background: rgba(8, 12, 23, 0.86);
  position: sticky;
  top: 0;
  z-index: 5;
}
.crumb {
  font-size: 12px;
  color: var(--ink-3);
  display: flex;
  align-items: center;
  gap: 7px;
}
.crumb-link {
  cursor: pointer;
}
.crumb-link:hover {
  color: var(--ink);
}
.crumb .cur {
  color: var(--ink);
}
.crumb .sep {
  color: var(--ink-4);
}
.sp {
  flex: 1;
}
.body {
  /* 宽屏吃满可用宽度：不再固定 1180，只留随视口变化的边距。 */
  padding: 20px clamp(16px, 2.2vw, 40px) 40px;
  width: 100%;
  max-width: none;
  box-sizing: border-box;
}
</style>
