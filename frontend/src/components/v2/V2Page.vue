<script setup lang="ts">
// V2 页面通用壳：标题 + 一句话说明 + 四态（加载 / 真实数据 / 接口未就绪 / 错误 / 演示数据）。
// 口径出处与依赖接口不再平铺成正文，收进标题的悬浮说明，页面只留一句人能读的说明。
import { computed } from 'vue'
import { Refresh } from '@element-plus/icons-vue'
import ErrorBubble from '@/components/ErrorBubble.vue'
import type { V2Dep, V2PageState } from './types'

const props = withDefaults(
  defineProps<{
    title: string
    intro?: string
    source?: string
    deps?: V2Dep[]
    state: V2PageState
    error?: { message: string; requestId: string }
    demoable?: boolean
    pendingHint?: string
  }>(),
  { deps: () => [], demoable: true },
)

const emit = defineEmits<{ reload: []; demo: []; exitDemo: [] }>()

const pendingDeps = computed(() => props.deps.filter((d) => d.state === 'pending'))
const bannerKind = computed(() => {
  if (props.state === 'demo') return 'demo'
  if (props.state === 'pending') return 'pending'
  return ''
})
const metaTip = computed(() => {
  const bits: string[] = []
  if (props.source) bits.push(`口径：${props.source}`)
  if (props.deps.length) bits.push(`接口：${props.deps.map((d) => `${d.path}(${d.state === 'ready' ? '已定型' : '待上线'})`).join('、')}`)
  return bits.join('　')
})
</script>

<template>
  <div class="v2-page">
    <header class="v2-head">
      <div class="v2-head-main">
        <h2 class="v2-title" :title="metaTip || undefined">
          {{ title }}
          <span v-if="state === 'demo'" class="pill">演示</span>
        </h2>
        <p v-if="intro" class="v2-intro">{{ intro }}</p>
      </div>
      <div class="v2-head-actions">
        <slot name="actions" />
        <el-button :icon="Refresh" size="small" :loading="state === 'loading'" @click="emit('reload')">刷新</el-button>
      </div>
    </header>

    <div v-if="bannerKind === 'demo'" class="v2-banner is-demo">
      <span>演示数据，不能用于对账或对外汇报</span>
      <el-button size="small" text @click="emit('exitDemo')">退出</el-button>
    </div>
    <div v-else-if="bannerKind === 'pending'" class="v2-banner is-pending">
      <span>接口未就绪{{ pendingDeps.length ? '：' + pendingDeps.map((d) => d.path).join('、') : '' }}</span>
      <el-button v-if="demoable" size="small" text @click="emit('demo')">看演示数据</el-button>
    </div>

    <ErrorBubble
      v-if="state === 'error'"
      class="v2-error"
      :message="error?.message || '请求失败'"
      :request-id="error?.requestId"
    />

    <div class="v2-body">
      <slot />
    </div>
  </div>
</template>

<style scoped>
.v2-page {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.v2-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 12px;
  flex-wrap: wrap;
}
.v2-head-main {
  min-width: 0;
}
.v2-title {
  margin: 0;
  font-size: 15px;
  font-weight: 600;
  color: var(--ink);
  display: flex;
  align-items: center;
  gap: 8px;
}
.pill {
  font-size: 10.5px;
  color: var(--warn);
  border: 1px solid #443718;
  border-radius: 9px;
  padding: 0 7px;
  line-height: 17px;
}
.v2-intro {
  margin: 4px 0 0;
  font-size: 12px;
  line-height: 1.5;
  color: var(--ink-3);
  max-width: 860px;
}
.v2-head-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}
.v2-banner {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 7px 12px;
  border-radius: var(--radius-s);
  font-size: 12px;
  flex-wrap: wrap;
}
.v2-banner.is-demo {
  background: rgba(201, 162, 39, 0.08);
  border: 1px solid #443718;
  color: #d6bd80;
}
.v2-banner.is-pending {
  background: #0b101c;
  border: 1px solid #2a3344;
  color: var(--ink-2);
}
.v2-body {
  display: flex;
  flex-direction: column;
  gap: 14px;
}
.v2-error {
  margin: 0;
}

@media (max-width: 640px) {
  .v2-head-actions {
    width: 100%;
    justify-content: flex-end;
  }
}
</style>
