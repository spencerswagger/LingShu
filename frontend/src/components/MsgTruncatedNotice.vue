<script setup lang="ts">
import { computed } from 'vue'
import { fmtBytes, type MsgTruncInfo } from '@/utils/reqParams'

// 「消息不是全量」的显式提示（后端 req_params.messages_truncated）。
//
// 后端策略：req_messages 超过 256KB 时**保 system + 保最新**、从最旧的开始裁；单条超长时
// 先按 rune 边界截断正文。所以看到的条数/正文都可能少于原始请求——必须紧贴消息区提示，
// 否则读日志的人会把「被裁掉的历史」误判成「用户当初就没发」。
const props = defineProps<{ info: MsgTruncInfo | null | undefined }>()

const show = computed(() => !!props.info)

const numbers = computed(() => {
  const i = props.info
  if (!i) return ''
  const parts: string[] = []
  if (i.OriginalCount != null && i.KeptCount != null) {
    parts.push(`原始 ${i.OriginalCount} 条 → 保留 ${i.KeptCount} 条`)
  }
  if (i.DroppedCount != null && i.DroppedCount > 0) parts.push(`省略 ${i.DroppedCount} 条`)
  if (i.OriginalBytes != null) parts.push(`原始体积 ${fmtBytes(i.OriginalBytes)}`)
  if (i.LimitBytes != null) parts.push(`上限 ${fmtBytes(i.LimitBytes)}`)
  if (i.ContentCut != null && i.ContentCut > 0) parts.push(`${i.ContentCut} 条超长消息正文被截断`)
  return parts.join(" · ")
})
</script>

<template>
  <div v-if="show" class="mtn">
    <el-alert type="warning" :closable="false" show-icon>
      <template #title>消息过长，最早的部分已被省略（不是全量）</template>
      <div class="mtn-body">
        <div class="mtn-line">{{ numbers || '后端已按体积上限裁剪消息列表' }}</div>
        <div class="mtn-line mtn-dim">
          裁剪方向：<b>从最旧的消息开始裁</b>，system 与最新消息优先保留；因此缺失的是更早的上下文，
          而不是用户当初没发。
        </div>
      </div>
    </el-alert>
  </div>
</template>

<style scoped>
.mtn {
  margin-bottom: 10px;
}
.mtn-body {
  display: grid;
  gap: 4px;
  margin-top: 2px;
}
.mtn-line {
  font-size: 12px;
  line-height: 1.5;
  color: var(--color-text-secondary);
  word-break: break-word;
  overflow-wrap: anywhere;
}
.mtn-dim {
  color: var(--color-text-tertiary);
}
@media (max-width: 768px) {
  .mtn-line {
    font-size: 14px;
  }
}
</style>
