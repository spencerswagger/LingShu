import { createRouter, createWebHistory } from 'vue-router'
import { useAuthStore } from '@/stores/auth'

// 路由表：/login、/register 公开；/admin/* 仅 ADMIN；/dev/* 仅 DEVELOPER
const router = createRouter({
  history: createWebHistory(),
  routes: [
    {
      path: '/login',
      name: 'login',
      component: () => import('@/views/Login.vue'),
    },
    {
      path: '/register',
      name: 'register',
      component: () => import('@/views/Register.vue'),
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
        // ===== V2 运营台（阶段三）=====
        // 与阶段二后端接口一一对应；接口未上线时页面自行出空态（不弹红条、不显示 0）。
        { path: 'topology', name: 'admin-topology', component: () => import('@/views/admin/v2/Topology.vue'), meta: { title: '拓扑' } },
        { path: 'subscriptions', name: 'admin-subscriptions', component: () => import('@/views/admin/v2/Subscriptions.vue'), meta: { title: '订阅' } },
        { path: 'ledger', name: 'admin-ledger', component: () => import('@/views/admin/v2/Ledger.vue'), meta: { title: '账本' } },
        { path: 'reconcile', name: 'admin-reconcile', component: () => import('@/views/admin/v2/Reconcile.vue'), meta: { title: '对账' } },
        { path: 'pnl', redirect: '/admin/reconcile' },
        // 渠道族（渠道 / 渠道协议 / 渠道密钥 / 渠道模型）与标签、服务状态页在 V2 整体下线：
        // 后端接口 410、表已删（0024），页面文件已删除。这里只保留 **1 行老书签兜底**，
        // 让历史链接落到「上游拓扑」而不是撞登录页（无页面、无接口调用）。
        { path: 'channels/:pathMatch(.*)*', redirect: '/admin/topology' },
        { path: 'channel-protocols/:pathMatch(.*)*', redirect: '/admin/topology' },
        { path: 'tags/:pathMatch(.*)*', redirect: '/admin/topology' },
        { path: 'service-status', redirect: '/admin/topology' },
        // 模型（Auto 模型族已与渠道族同批退役：页内切换与 Auto 页一并删除，这里只剩对外模型列表）
        { path: 'models', name: 'admin-models', component: () => import('@/views/admin/ModelList.vue'), meta: { title: '模型' } },
        { path: 'models/create', name: 'admin-model-create', component: () => import('@/views/admin/ModelCreate.vue'), meta: { title: '新建模型' } },
        { path: 'models/:id/edit', name: 'admin-model-edit', component: () => import('@/views/admin/ModelEdit.vue'), meta: { title: '编辑模型' } },
        // Auto 模型（auto_models / auto_model_members，0024 删表、接口 410）整体下线：
        // 与渠道族同一套「1 行老书签兜底」，落到对外模型列表，不留死链。
        { path: 'auto-models/:pathMatch(.*)*', redirect: '/admin/models' },
        // 用户管理
        { path: 'users', name: 'admin-users', component: () => import('@/views/admin/UserList.vue'), meta: { title: '用户' } },
        { path: 'users/create', name: 'admin-user-create', component: () => import('@/views/admin/UserCreate.vue'), meta: { title: '新建用户' } },
        { path: 'users/:id', name: 'admin-user-detail', component: () => import('@/views/admin/UserDetail.vue'), meta: { title: '用户详情' } },
        // 令牌管理
        { path: 'tokens', name: 'admin-tokens', component: () => import('@/views/admin/AdminTokenList.vue'), meta: { title: '令牌' } },
        // 会话管理（V2 下线：新链路不写 sessions，Spec §1.2）——只留老书签兜底，菜单已摘除。
        { path: 'sessions/:pathMatch(.*)*', redirect: '/admin/dashboard' },
        // 账单
        { path: 'billings', name: 'admin-billings', component: () => import('@/views/admin/BillingQuery.vue'), meta: { title: '账单' } },
        { path: 'billings/:id', name: 'admin-billing-detail', component: () => import('@/views/admin/BillingDetail.vue'), meta: { title: '账单详情' } },
        // 系统管理（系统级全局配置统一铺在这一页）
        { path: 'system', name: 'admin-system', component: () => import('@/views/admin/BillingConfig.vue'), meta: { title: '系统' } },
        { path: 'system/config', redirect: '/admin/system' },
        { path: 'system/gateway', redirect: '/admin/system' },
        // 成本（V1 页 JOIN channel_keys/channels，0024 后必 500）：由盈亏页取代（Spec §1.2），
        // 老书签兜底到盈亏，菜单已摘除。
        { path: 'cost/:pathMatch(.*)*', redirect: '/admin/pnl' },
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
          path: 'models',
          name: 'dev-model-list',
          component: () => import('@/views/admin/ModelList.vue'),
          meta: { title: '模型' },
          props: { mode: 'self' },
        },
        // Auto 模型族整体下线（同管理端）：老书签兜底到模型列表。
        { path: 'auto-models/:pathMatch(.*)*', redirect: '/dev/models' },
        // 会话页在 V2 下线（Spec §1.2）：老书签兜底到统计，菜单已摘除。
        { path: 'sessions/:pathMatch(.*)*', redirect: '/dev/dashboard' },
        // 公告：开发者端**只读**一页（Spec P13 / A-5 —— 之前只有 api/dev.ts 的封装，
        // 路由与入口都没有，导致「发布后开发端可见」在页面上不可达）。
        {
          path: 'announcements',
          name: 'dev-announcements',
          component: () => import('@/views/dev/AnnouncementList.vue'),
          meta: { title: '公告' },
        },
        {
          path: 'billings',
          name: 'dev-billings',
          component: () => import('@/views/admin/BillingQuery.vue'),
          meta: { title: '账单' },
          props: { mode: 'self' },
        },
        {
          path: 'billings/:id',
          name: 'dev-billing-detail',
          component: () => import('@/views/admin/BillingDetail.vue'),
          meta: { title: '账单详情' },
          props: { mode: 'self' },
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
    // 403 / 兜底
    { path: '/403', name: 'forbidden', component: () => import('@/views/Forbidden.vue') },
    { path: '/:pathMatch(.*)*', redirect: '/login' },
  ],
})

router.beforeEach((to) => {
  const auth = useAuthStore()
  // 未登录：除登录页、注册页外均跳登录
  if (!auth.token && to.name !== 'login' && to.name !== 'register') {
    return { name: 'login', query: { redirect: to.fullPath } }
  }
  // 已登录访问登录/注册页：按角色回首页
  if (auth.token && (to.name === 'login' || to.name === 'register')) {
    return auth.role === 'ADMIN' ? '/admin' : '/dev'
  }
  // 已登录访问受保护页：不再每次导航拉取 /auth/me（150ms+ 的换页卡顿来源）。
  // 登录态判断只用本地状态（store + token 有效性）；/auth/me 只在首屏（布局挂载）、
  // 401 拦截器、用户面板/改密等手动刷新时拉取。token 与 role 始终由 setAuth 成对写入，
  // 401 时由拦截器成对清空，故本地状态可信；角色降权在下次手动刷新时同步。
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