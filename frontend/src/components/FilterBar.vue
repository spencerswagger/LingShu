<script setup lang="ts">
// 列表统一工具栏（会话 / 账单等列表页复用）：
// 左侧展示生效中的筛选条件标签（可关闭），右侧为图标化的重置、刷新与定时刷新。
import { computed, onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { Delete, Refresh, Timer } from '@element-plus/icons-vue'

interface FilterTag {
  key: string
  label: string
}

const props = withDefaults(
  defineProps<{
    tags?: FilterTag[]
    loading?: boolean
    /** 定时刷新间隔（秒） */
    interval?: number
  }>(),
  { tags: () => [], loading: false, interval: 30 },
)

const emit = defineEmits<{
  (e: 'clear', key: string): void
  (e: 'reset'): void
  (e: 'refresh'): void
}>()

const hasTags = computed(() => props.tags.length > 0)

// ===== 定时刷新（默认开启）=====
const INTERVAL_OPTIONS = [5, 10, 30, 60]
const autoRefresh = ref(true)
const seconds = ref(props.interval)
let timer: number | undefined

function stopTimer() {
  if (timer !== undefined) {
    window.clearInterval(timer)
    timer = undefined
  }
}
function startTimer() {
  stopTimer()
  timer = window.setInterval(() => {
    // 页面不可见或上次请求未结束时跳过本次
    if (document.hidden || props.loading) return
    emit('refresh')
  }, seconds.value * 1000)
}
watch([autoRefresh, seconds], () => (autoRefresh.value ? startTimer() : stopTimer()))

function pickInterval(s: number) {
  seconds.value = s
  autoRefresh.value = true
}

// 页面切到后台时暂停，回到前台时若仍开启则恢复
function onVisibility() {
  if (!autoRefresh.value) return
  if (document.hidden) stopTimer()
  else startTimer()
}

onMounted(() => {
  if (autoRefresh.value) startTimer()
  document.addEventListener('visibilitychange', onVisibility)
})
onBeforeUnmount(() => {
  stopTimer()
  document.removeEventListener('visibilitychange', onVisibility)
})
</script>

<template>
  <div class="filter-bar">
    <div class="fb-tags">
      <el-tag
        v-for="t in tags"
        :key="t.key"
        class="fb-tag"
        size="small"
        effect="light"
        closable
        @close="emit('clear', t.key)"
      >
        {{ t.label }}
      </el-tag>
    </div>
    <div class="fb-actions">
      <slot name="actions" />
      <el-tooltip content="重置筛选" placement="top" :show-after="200">
        <span
          class="op-icon"
          :class="{ disabled: !hasTags }"
          role="button"
          aria-label="重置筛选"
          @click="hasTags && emit('reset')"
        >
          <el-icon><Delete /></el-icon>
        </span>
      </el-tooltip>
      <el-tooltip content="刷新" placement="top" :show-after="200">
        <span class="op-icon" role="button" aria-label="刷新" @click="emit('refresh')">
          <el-icon :class="{ spinning: loading }"><Refresh /></el-icon>
        </span>
      </el-tooltip>
      <el-popover placement="top" trigger="hover" :width="132" :show-arrow="false" :offset="8" popper-class="fb-timer-pop">
        <template #reference>
          <span
            class="op-icon"
            :class="{ on: autoRefresh }"
            role="button"
            aria-label="定时刷新开关"
            :title="autoRefresh ? `定时刷新已开启（每 ${seconds} 秒）` : '定时刷新已关闭'"
            @click="autoRefresh = !autoRefresh"
          >
            <el-icon><Timer /></el-icon>
          </span>
        </template>
        <div class="fb-timer">
          <div class="fb-timer-head">{{ autoRefresh ? `定时刷新 · 每 ${seconds} 秒` : '定时刷新已关闭' }}</div>
          <div
            v-for="opt in INTERVAL_OPTIONS"
            :key="opt"
            class="fb-timer-item"
            :class="{ active: seconds === opt }"
            role="button"
            :aria-pressed="seconds === opt"
            @click="pickInterval(opt)"
          >
            {{ opt }} 秒
          </div>
        </div>
      </el-popover>
    </div>
  </div>
</template>

<style scoped>
.filter-bar {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 12px;
  flex-wrap: wrap;
}
.fb-tags {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
  flex: 1;
  min-height: 24px;
}
.fb-tag {
  --el-tag-bg-color: var(--brand-primary-soft);
  --el-tag-text-color: var(--brand-primary-deep);
  --el-tag-border-color: transparent;
}
.fb-actions {
  display: flex;
  align-items: center;
  gap: 4px;
  margin-left: auto;
}
.op-icon.on {
  color: var(--brand-primary);
  background: rgba(90, 92, 232, 0.1);
}
.op-icon.disabled {
  color: var(--color-text-tertiary);
  cursor: not-allowed;
  opacity: 0.45;
}
.op-icon.disabled:hover {
  background: transparent;
  color: var(--color-text-tertiary);
}
.spinning {
  animation: fb-spin 0.8s linear infinite;
}
@keyframes fb-spin {
  to {
    transform: rotate(360deg);
  }
}
</style>

<style>
/* 定时刷新下拉（悬停浮层） */
.fb-timer-pop .fb-timer-head {
  font-size: 12px;
  color: var(--color-text-tertiary);
  margin-bottom: 6px;
}
.fb-timer-pop .fb-timer-item {
  padding: 5px 8px;
  border-radius: 6px;
  font-size: 13px;
  color: var(--color-text);
  cursor: pointer;
}
.fb-timer-pop .fb-timer-item:hover {
  background: var(--brand-primary-soft);
}
.fb-timer-pop .fb-timer-item.active {
  color: var(--brand-primary);
  font-weight: 600;
  background: var(--brand-primary-soft);
}
</style>
