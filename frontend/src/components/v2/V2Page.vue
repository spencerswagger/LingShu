<script setup lang="ts">
// V2 页面通用壳：标题 + 口径说明 + 依赖接口徽标 + 四态（加载 / 真实数据 / 接口未就绪 / 错误 / 演示数据）。
//
// 为什么统一壳：阶段三这十几页里有相当一部分依赖后端分批上线的接口。如果每页自己写
// 「未就绪怎么办」，就会出现有的页弹红条、有的页显示 0、有的页直接白屏——运营分不清
// 「真没有数据」和「接口没上线」。这一层把三种情况固定成三种视觉：
//   pending → 灰底说明 + 「预览演示数据」按钮（默认不显示任何数字）
//   demo    → 橙色横幅「演示数据」，明确不可用于任何结论
//   error   → ErrorBubble（带 requestId，可复制）
import { computed } from 'vue'
import { Refresh, Warning } from '@element-plus/icons-vue'
import ErrorBubble from '@/components/ErrorBubble.vue'
import type { V2Dep, V2PageState } from './types'

const props = withDefaults(
  defineProps<{
    title: string
    intro?: string
    // 页面口径出处（文档小节 / 后端文件）
    source?: string
    deps?: V2Dep[]
    state: V2PageState
    error?: { message: string; requestId: string }
    // 是否允许「预览演示数据」（财务类页面也允许，但默认必须空态）
    demoable?: boolean
    // 未就绪时的原因文案（谁知道谁写，别写「敬请期待」）
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
</script>

<template>
  <div class="v2-page">
    <header class="v2-head">
      <div class="v2-head-main">
        <h2 class="v2-title">
          {{ title }}
          <el-tag v-if="state === 'demo'" type="warning" size="small" effect="dark" class="tag">演示数据</el-tag>
        </h2>
        <p v-if="intro" class="v2-intro">{{ intro }}</p>
        <div v-if="deps.length" class="v2-deps">
          <span class="deps-label">依赖接口</span>
          <span v-for="d in deps" :key="d.path" class="dep" :class="d.state === 'ready' ? 'is-ready' : 'is-pending'">
            <code>{{ d.path }}</code>
            <em>{{ d.state === 'ready' ? '已定型' : '待上线' }}</em>
          </span>
        </div>
        <p v-if="source" class="v2-source">口径：{{ source }}</p>
      </div>
      <div class="v2-head-actions">
        <slot name="actions" />
        <el-button :icon="Refresh" size="small" :loading="state === 'loading'" @click="emit('reload')">刷新</el-button>
      </div>
    </header>

    <div v-if="bannerKind === 'demo'" class="v2-banner is-demo">
      <el-icon><Warning /></el-icon>
      <span>当前显示的是<strong>演示数据</strong>，不是真实账务结果，不能用于对账、结算或对外汇报。</span>
      <el-button size="small" text @click="emit('exitDemo')">退出预览</el-button>
    </div>

    <div v-else-if="bannerKind === 'pending'" class="v2-banner is-pending">
      <el-icon><Warning /></el-icon>
      <span>
        接口未就绪<template v-if="pendingDeps.length">（{{ pendingDeps.map((d) => d.path).join('、') }}）</template
        >。{{ pendingHint || '页面结构与校验已就位，等后端接口上线后自动切到真实数据。' }}
      </span>
      <el-button v-if="demoable" size="small" @click="emit('demo')">预览演示数据</el-button>
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
  gap: 12px;
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
  font-size: 18px;
  font-weight: 600;
  color: var(--color-text);
  display: flex;
  align-items: center;
  gap: 8px;
}
.tag {
  font-weight: 500;
}
.v2-intro {
  margin: 4px 0 0;
  font-size: 13px;
  line-height: 1.6;
  color: var(--color-text-secondary);
  max-width: 900px;
}
.v2-deps {
  margin-top: 8px;
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 6px;
  font-size: 12px;
}
.deps-label {
  color: var(--color-text-tertiary);
}
.dep {
  display: inline-flex;
  align-items: center;
  gap: 4px;
  padding: 1px 8px;
  border-radius: 999px;
  border: 1px solid var(--color-border);
  background: var(--color-surface);
}
.dep code {
  font-size: 11px;
  color: var(--color-text-secondary);
}
.dep em {
  font-style: normal;
  font-size: 11px;
}
.dep.is-ready em {
  color: var(--color-success);
}
.dep.is-pending em {
  color: var(--color-warning);
}
.v2-source {
  margin: 6px 0 0;
  font-size: 12px;
  color: var(--color-text-tertiary);
}
.v2-head-actions {
  display: flex;
  align-items: center;
  gap: 8px;
}
.v2-banner {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 8px 12px;
  border-radius: var(--radius-s);
  font-size: 13px;
  line-height: 1.5;
  flex-wrap: wrap;
}
.v2-banner.is-demo {
  background: #fef4e5;
  border: 1px solid #fcdba3;
  color: #b25c05;
}
.v2-banner.is-pending {
  background: #f7f8fb;
  border: 1px dashed var(--color-border-strong);
  color: var(--color-text-secondary);
}
.v2-banner strong {
  font-weight: 600;
}
.v2-body {
  display: flex;
  flex-direction: column;
  gap: 12px;
}
.v2-error {
  margin: 0;
}

@media (max-width: 640px) {
  .v2-title {
    font-size: 16px;
  }
  .v2-intro,
  .v2-banner {
    font-size: 14px;
  }
  .v2-head-actions {
    width: 100%;
    justify-content: flex-end;
  }
}
</style>
