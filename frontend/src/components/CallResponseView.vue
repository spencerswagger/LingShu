<script setup lang="ts">
import { computed } from 'vue'
import MessageBubbleGroup from '@/components/MessageBubbleGroup.vue'
import { parseRespBody } from '@/utils/messageParts'

// 响应 / 上游返回的统一渲染（账单详情、会话详情、调用日志面板共用）。
// 三种形态：
//   1. 有错误原因（网关侧失败）→ 独立红字，不吞；
//   2. 响应体能解析成消息（非流式完整响应 / 流式 assistant 增量）→ 气泡 + 原始 JSON 折叠；
//   3. 非消息形状（如上游错误体）→ 原样展示，绝不吞原文。
const props = withDefaults(
  defineProps<{
    body?: string
    respKind?: string // non_stream | stream | error | none
    errorMessage?: string
    // 是否在本组件内展示 errorMessage（错误信息已单独成块时置 false，避免重复叙述）
    showError?: boolean
  }>(),
  { body: '', respKind: '', errorMessage: '', showError: true },
)

const resp = computed(() => parseRespBody(props.body || ''))
const isMessage = computed(() => !!(resp.value.text || resp.value.reasoning || resp.value.toolCalls.length))
</script>

<template>
  <div class="crv">
    <div v-if="showError && errorMessage" class="crv-error">{{ errorMessage }}</div>
    <template v-if="!body">
      <div v-if="!errorMessage" class="crv-empty">
        {{ respKind === 'error' ? '未收到上游响应（原因见「决策轨迹」）' : '—' }}
      </div>
    </template>
    <template v-else-if="isMessage">
      <MessageBubbleGroup
        role="assistant 响应"
        :content="resp.text"
        :reasoning="resp.reasoning"
        :tool-calls="resp.toolCalls"
      />
      <el-collapse class="crv-raw">
        <el-collapse-item name="resp-raw" title="原始响应">
          <pre class="mono crv-pre">{{ body }}</pre>
        </el-collapse-item>
      </el-collapse>
    </template>
    <pre v-else class="mono crv-pre">{{ body }}</pre>
  </div>
</template>

<style scoped>
.crv {
  min-width: 0;
}
.crv-error {
  font-size: 13px;
  font-weight: 600;
  color: var(--color-danger);
  margin-bottom: 6px;
  word-break: break-word;
  overflow-wrap: anywhere;
}
.crv-empty {
  font-size: 13px;
  color: var(--color-text-tertiary);
  padding: 4px 2px;
}
.crv-raw {
  margin-top: 10px;
}
.crv-pre {
  margin: 0;
  max-height: 360px;
  overflow: auto;
  padding: 10px 12px;
  border-radius: 6px;
  background: #101828;
  color: #d0d5dd;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
}
.mono {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
  font-size: 12px;
}
@media (max-width: 768px) {
  .crv-error,
  .crv-empty {
    font-size: 14px;
  }
}
</style>
