<script setup lang="ts">
// 公告（开发者端 P13，只读）—— Spec `2026-11-16-stage3-13-pages-acceptance.md` P13。
//
// 为什么单独一页：Spec A-5 指出「开发者端只有 api/dev.ts 的封装，路由与菜单都没有公告入口」，
// 于是「发布后开发端可见」这条验收在页面上根本不可达。这里补齐入口，口径三条：
//   1. **只读**：本页不发起任何 POST/PUT/DELETE；后端 `/dev/announcements` 也只有 GET。
//      负向断言靠这一点——页面代码里出现写接口就是 bug，不是「以后再说」。
//   2. 空态不装作有内容：先「未发布公告」再给一句「有新公告时管理员会在这里发布」。
//   3. 错误态必须带 request_id + 重试（G1）；40101 由 http 层跳登录，本页不自己造跳转。
import { computed, onMounted, ref } from 'vue'
import { BellFilled, Warning, CircleCheck, InfoFilled } from '@element-plus/icons-vue'
import { listDevAnnouncements, type DevAnnouncement } from '@/api/dev'
import ErrorBubble from '@/components/ErrorBubble.vue'

const loading = ref(false)
const list = ref<DevAnnouncement[]>([])
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

const LEVEL: Record<string, { label: string; type: 'primary' | 'warning' | 'danger'; icon: unknown }> = {
  info: { label: '通知', type: 'primary', icon: InfoFilled },
  warning: { label: '注意', type: 'warning', icon: Warning },
  danger: { label: '重要', type: 'danger', icon: CircleCheck },
}

function levelOf(lv: string) {
  return LEVEL[lv] || LEVEL.info
}

// 后端只下发「当前有效」的公告（Enabled + 发布/过期窗口在服务端过滤），
// 前端不再二次过滤，避免两边口径不一致时出现「后端发了、页面看不到」。
const visible = computed(() => list.value)

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await listDevAnnouncements()
    list.value = res.Data?.List || []
  } catch (e: any) {
    errInfo.value = { message: e?.message || '加载失败', requestId: e?.requestId || '' }
  } finally {
    loading.value = false
  }
}
onMounted(load)

function fmt(t?: string) {
  if (!t) return ''
  const d = new Date(t)
  if (Number.isNaN(d.getTime())) return t
  return d.toLocaleString('zh-CN', { hour12: false })
}
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">公告</div>
      <span class="page-count">共 {{ visible.length }} 条</span>
      <div class="page-header-spacer" />
      <el-button size="small" :loading="loading" @click="load">刷新</el-button>
    </div>

    <ErrorBubble
      v-if="errInfo.message"
      :message="errInfo.message"
      :request-id="errInfo.requestId"
      class="err"
    />

    <div v-loading="loading" class="list">
      <el-empty v-if="!loading && !visible.length && !errInfo.message" description="暂无公告">
        <span class="empty-hint">有新公告时，管理员会在这里发布。</span>
      </el-empty>

      <article v-for="a in visible" :key="a.ID" class="item" :class="`is-${a.Level}`">
        <header class="item-head">
          <el-icon class="item-icon"><BellFilled /></el-icon>
          <h3 class="item-title">{{ a.Title }}</h3>
          <el-tag :type="levelOf(a.Level).type" size="small" effect="light">{{ levelOf(a.Level).label }}</el-tag>
          <span class="item-time">{{ fmt(a.PublishAt || a.CreatedAt) }}</span>
        </header>
        <div class="item-body">{{ a.Content }}</div>
        <footer v-if="a.ExpireAt" class="item-foot">有效期至 {{ fmt(a.ExpireAt) }}</footer>
      </article>
    </div>
  </div>
</template>

<style scoped>
.err {
  margin: 0;
}
.list {
  display: flex;
  flex-direction: column;
  gap: 12px;
  min-height: 80px;
}
.empty-hint {
  font-size: 13px;
  color: var(--color-text-tertiary);
}
.item {
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-left: 3px solid var(--color-primary);
  border-radius: var(--radius-s);
  padding: 12px 14px;
}
.item.is-warning {
  border-left-color: var(--color-warning);
}
.item.is-danger {
  border-left-color: var(--color-danger, #e6a23c);
}
.item-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.item-icon {
  color: var(--color-text-tertiary);
}
.item-title {
  margin: 0;
  font-size: 15px;
  font-weight: 600;
  color: var(--color-text);
}
.item-time {
  margin-left: auto;
  font-size: 12px;
  color: var(--color-text-tertiary);
  font-variant-numeric: tabular-nums;
}
.item-body {
  margin-top: 8px;
  font-size: 14px;
  line-height: 1.7;
  color: var(--color-text-secondary);
  white-space: pre-wrap;
  word-break: break-word;
}
.item-foot {
  margin-top: 8px;
  font-size: 12px;
  color: var(--color-text-tertiary);
}

@media (max-width: 640px) {
  .item-body {
    font-size: 14px;
  }
  .item-time {
    margin-left: 0;
    width: 100%;
  }
}
</style>
