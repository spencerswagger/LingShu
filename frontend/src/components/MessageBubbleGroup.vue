<script setup lang="ts">
import { computed } from 'vue'
import ChatBubble from '@/components/ChatBubble.vue'
import { contentToBubbles, fmtJson, type MsgBubble } from '@/utils/messageParts'

// 统一的消息气泡组：会话时间线、账单详情的「请求增量」与「响应」三处共用。
// 结构 = 角色标签 + 气泡（正文，含 XML 标签拆分）+ 推理气泡 + 工具调用气泡。
// 气泡外观（内边距/圆角/底色）由本组件统一提供，tone 区分用户侧与助手侧配色。
const props = defineProps<{
  role?: string
  content?: unknown
  reasoning?: string
  toolCalls?: unknown[]
  // 气泡配色：user=用户侧（浅主色），assistant/未传=通用底色
  tone?: 'user' | 'assistant'
  // 无任何内容时的占位文案；不传则不显示占位（由调用方自行展示空态）
  emptyText?: string
}>()

const bubbles = computed<MsgBubble[]>(() => contentToBubbles(props.content))
const hasReasoning = computed(() => !!props.reasoning)
const hasTools = computed(() => (props.toolCalls?.length || 0) > 0)
const isEmpty = computed(() => !bubbles.value.length && !hasReasoning.value && !hasTools.value)
</script>

<template>
  <div class="mbg" :class="`tone-${tone || 'assistant'}`">
    <div v-if="role" class="mbg-role">{{ role }}</div>
    <div v-for="(b, i) in bubbles" :key="i" class="mbg-bubble" :class="{ 'is-tool': b.kind === 'tool' }">
      <ChatBubble :bubble="b" />
    </div>
    <div v-if="hasReasoning" class="mbg-bubble">
      <ChatBubble :bubble="{ kind: 'xml', tag: 'reasoning', text: reasoning || '' }" />
    </div>
    <div v-for="(tc, i) in toolCalls || []" :key="'t' + i" class="mbg-bubble is-tool">
      <ChatBubble :bubble="{ kind: 'tool', text: fmtJson(tc) }" />
    </div>
    <div v-if="isEmpty && emptyText" class="mbg-empty">{{ emptyText }}</div>
  </div>
</template>

<style scoped>
.mbg-role {
  font-size: 11px;
  color: var(--color-text-tertiary);
  margin-bottom: 2px;
}
.mbg-bubble {
  max-width: 100%;
  padding: 8px 12px;
  border-radius: 10px;
  background: var(--color-fill, #f6f7fa);
  overflow-wrap: break-word;
}
.mbg.tone-user .mbg-bubble {
  background: var(--el-color-primary-light-9, #eef0fe);
}
.mbg-bubble + .mbg-bubble {
  margin-top: 6px;
}
/* 工具调用气泡占满气泡簇宽度（迁移前会话页行为） */
.mbg-bubble.is-tool {
  width: 100%;
}
.mbg-empty {
  font-size: 12px;
  color: var(--color-text-tertiary);
}
</style>
