<script setup lang="ts">
import { CircleCheck, CircleClose, Refresh, VideoPause } from '@element-plus/icons-vue'

// 渠道内部模型 / 渠道密钥的「三态操作」按钮组（统一图标样式）。
// 当前状态对应的按钮自动置灰；可选前置一个「重新探测」。
const props = withDefaults(
  defineProps<{
    state?: string
    showProbe?: boolean
    probeTitle?: string
    loading?: boolean
  }>(),
  { state: 'NORMAL', showProbe: false, probeTitle: '重新探测', loading: false },
)

const emit = defineEmits<{
  (e: 'probe'): void
  (e: 'set', action: 'normal' | 'drain' | 'disable'): void
}>()
</script>

<template>
  <span class="icon-ops">
    <el-tooltip v-if="showProbe" :content="probeTitle" placement="top">
      <el-button text circle size="small" :icon="Refresh" :loading="loading" :disabled="loading" @click="emit('probe')" />
    </el-tooltip>
    <el-tooltip content="置为正常" placement="top">
      <el-button text circle size="small" class="ico-ok" :icon="CircleCheck" :disabled="loading || props.state === 'NORMAL'" @click="emit('set', 'normal')" />
    </el-tooltip>
    <el-tooltip content="排空" placement="top">
      <el-button text circle size="small" class="ico-warn" :icon="VideoPause" :disabled="loading || props.state === 'DRAIN'" @click="emit('set', 'drain')" />
    </el-tooltip>
    <el-tooltip content="禁用" placement="top">
      <el-button text circle size="small" class="ico-bad" :icon="CircleClose" :disabled="loading || props.state === 'DISABLED'" @click="emit('set', 'disable')" />
    </el-tooltip>
  </span>
</template>

<style scoped>
.icon-ops {
  display: inline-flex;
  align-items: center;
  gap: 2px;
}
.icon-ops .el-button {
  margin-left: 0 !important;
  width: 30px;
  height: 30px;
  padding: 0;
  font-size: 17px;
  color: var(--color-text-secondary);
}
.icon-ops .el-button .el-icon {
  font-size: 17px;
}
.icon-ops .el-button:hover {
  background: rgba(99, 102, 241, 0.1);
}
.icon-ops .ico-ok {
  color: var(--color-success);
}
.icon-ops .ico-warn {
  color: var(--color-warning);
}
.icon-ops .ico-bad {
  color: var(--color-danger);
}
.icon-ops .el-button.is-disabled {
  color: var(--el-text-color-disabled) !important;
  background: transparent;
}
</style>