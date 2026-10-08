import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'

// 路由表：/login 公开；/admin/* 仅 ADMIN；/dev/* 仅 DEVELOPER
const router = createRouter({
  history: createWebHistory(),
  routes: [
    {
      path: '/login',
      name: 'login',
      component: () => import('@/views/Login.vue'),
    },
    {
      path: '/admin',
      component: () => import('@/layouts/AdminLayout.vue'),
      meta: { roles: ['ADMIN'] },
      children: [
        { path: '', redirect: '/admin/dashboard' },
        {
          path: 'dashboard',
          name: 'admin-dashboard',
          component: () => import('@/views/StatsDashboard.vue'),
          meta: { title: '统计' },
        },
        // 渠道
        { path: 'channels', name: 'admin-channels', component: () => import('@/views/admin/ChannelList.vue'), meta: { title: '渠道' } },
        { path: 'channels/create', name: 'admin-channel-create', component: () => import('@/views/admin/ChannelCreate.vue'), meta: { title: '新建渠道' } },
        { path: 'channels/:id', name: 'admin-channel-edit', component: () => import('@/views/admin/ChannelEdit.vue'), meta: { title: '渠道' } },
        // 模型
        { path: 'models', name: 'admin-models', component: () => import('@/views/admin/ModelList.vue'), meta: { title: '模型' } },
        { path: 'models/create', name: 'admin-model-create', component: () => import('@/views/admin/ModelCreate.vue'), meta: { title: '新建模型' } },
        { path: 'models/:id/edit', name: 'admin-model-edit', component: () => import('@/views/admin/ModelEdit.vue'), meta: { title: '编辑模型' } },
        // 语义标签
        { path: 'tags', name: 'admin-tags', component: () => import('@/views/admin/TagList.vue'), meta: { title: '标签' } },
        { path: 'tags/create', name: 'admin-tag-create', component: () => import('@/views/admin/TagCreate.vue'), meta: { title: '新建标签' } },
        { path: 'tags/:id/edit', name: 'admin-tag-edit', component: () => import('@/views/admin/TagEdit.vue'), meta: { title: '编辑标签' } },
        // 用户管理
        { path: 'users', name: 'admin-users', component: () => import('@/views/admin/UserList.vue'), meta: { title: '用户' } },
        { path: 'users/create', name: 'admin-user-create', component: () => import('@/views/admin/UserCreate.vue'), meta: { title: '新建用户' } },
        { path: 'users/:id', name: 'admin-user-detail', component: () => import('@/views/admin/UserDetail.vue'), meta: { title: '用户详情' } },
        // 令牌管理
        { path: 'tokens', name: 'admin-tokens', component: () => import('@/views/admin/AdminTokenList.vue'), meta: { title: '令牌' } },
        // 会话管理
        { path: 'sessions', name: 'admin-sessions', component: () => import('@/views/admin/SessionList.vue'), meta: { title: '会话' } },
        // 账单
        { path: 'billings', name: 'admin-billings', component: () => import('@/views/admin/BillingQuery.vue'), meta: { title: '账单' } },
        { path: 'billings/:id', name: 'admin-billing-detail', component: () => import('@/views/admin/BillingDetail.vue'), meta: { title: '账单详情' } },
        // 系统管理（系统级全局配置统一放这里）
        { path: 'system', redirect: '/admin/system/config' },
        { path: 'system/config', name: 'admin-system-config', component: () => import('@/views/admin/BillingConfig.vue'), meta: { title: '系统' } },
        // 公告
        { path: 'announcements', name: 'admin-announcements', component: () => import('@/views/admin/AnnouncementList.vue'), meta: { title: '公告' } },
        { path: 'announcements/create', name: 'admin-announcement-create', component: () => import('@/views/admin/AnnouncementEdit.vue'), meta: { title: '新建公告' } },
        { path: 'announcements/:id', name: 'admin-announcement-edit', component: () => import('@/views/admin/AnnouncementEdit.vue'), meta: { title: '编辑公告' } },
      ],
    },
    {
      path: '/dev',
      component: () => import('@/layouts/DevLayout.vue'),
      meta: { roles: ['DEVELOPER'] },
      children: [
        { path: '', redirect: '/dev/dashboard' },
        {
          path: 'dashboard',
          name: 'dev-dashboard',
          component: () => import('@/views/StatsDashboard.vue'),
          meta: { title: '统计' },
        },
        {
          path: 'tokens',
          name: 'dev-tokens',
          component: () => import('@/views/dev/TokenList.vue'),
          meta: { title: '令牌' },
        },
        {
          path: 'tokens/:id',
          name: 'dev-token-detail',
          component: () => import('@/views/dev/TokenDetail.vue'),
          meta: { title: '令牌详情' },
        },
        {
          path: 'usage',
          name: 'dev-usage',
          component: () => import('@/views/dev/Usage.vue'),
          meta: { title: '用量统计' },
        },
        {
          path: 'wallet',
          name: 'dev-wallet',
          component: () => import('@/views/dev/Wallet.vue'),
          meta: { title: '我的钱包' },
        },
        {
          path: 'billings',
          name: 'dev-billings',
          component: () => import('@/views/dev/Billings.vue'),
          meta: { title: '消费账单' },
        },
        {
          path: 'billings/:id',
          name: 'dev-billing-detail',
          component: () => import('@/views/dev/BillingDetail.vue'),
          meta: { title: '账单详情' },
        },
        {
          path: 'announcements',
          name: 'dev-announcements',
          component: () => import('@/views/dev/Announcements.vue'),
          meta: { title: '系统公告' },
        },
      ],
    },
    // 根路径按角色重定向
    { path: '/', redirect: '/login' },
    // 改密页（强制改密守卫白名单，登录后 ADMIN/DEVELOPER 均可访问）
    {
      path: '/change-password',
      name: 'change-password',
      component: () => import('@/views/ChangePassword.vue'),
    },
    // 账号安全页（TOTP 管理，登录后 ADMIN/DEVELOPER 均可访问）
    {
      path: '/account/security',
      name: 'account-security',
      component: () => import('@/views/AccountSecurity.vue'),
    },
    // 403 / 兜底
    { path: '/403', name: 'forbidden', component: () => import('@/views/Forbidden.vue') },
    { path: '/:pathMatch(.*)*', redirect: '/login' },
  ],
})

router.beforeEach(async (to) => {
  const auth = useAuthStore()
  // 未登录：除登录页外均跳登录
  if (!auth.token && to.name !== 'login') {
    return { name: 'login', query: { redirect: to.fullPath } }
  }
  // 已登录访问登录页：按角色回首页
  if (auth.token && to.name === 'login') {
    return auth.role === 'ADMIN' ? '/admin' : '/dev'
  }
  // 已登录访问受保护页：先拉取最新用户信息，同步本地角色（堵住本地清 role 绕过，
  // 并解决管理员降权后本地角色陈旧的问题——后端 /auth/me 始终返回权威角色）。
  if (auth.token) {
    await auth.fetchProfile()
    if (!auth.token) return { name: 'login', query: { redirect: to.fullPath } }
  }
  // 角色校验：角色缺失或与页面要求不符一律拦截（不再用 `auth.role &&` 短路跳过）
  const need = to.meta.roles as string[] | undefined
  if (need && need.length && !need.includes(auth.role)) {
    return { name: 'forbidden' }
  }
  // 强制改密：非白名单页一律跳改密页
  if (auth.mustChangePassword && !['login', 'change-password'].includes(to.name as string)) {
    return { name: 'change-password' }
  }
  return true
})

export default router