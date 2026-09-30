<script setup lang="ts">
import { ElMessage } from 'element-plus'

// 小字错误提示：展示 message + 可复制 requestId（供排查关联日志）
const props = defineProps<{
  message?: string
  requestId?: string
}>()

// 复制 requestId 到剪贴板
async function copyReqId() {
  if (!props.requestId) return
  try {
    await navigator.clipboard.writeText(props.requestId)
    ElMessage.success('requestId 已复制')
  } catch {
    ElMessage.warning('复制失败，请手动选择复制')
  }
}
</script>

<template>
  <div class="error-tip">
    <span>{{ message || '请求失败，请重试' }}</span>
    <span v-if="requestId" class="req-id" title="点击复制" @click="copyReqId">
      reqId:{{ requestId }}
    </span>
  </div>
</template>