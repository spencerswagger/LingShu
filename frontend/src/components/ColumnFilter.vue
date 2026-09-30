<script setup lang="ts">
// 列头筛选入口（对齐成熟组件库的表格筛选交互）：
// 表头正常显示文本，右侧过滤图标；点击图标弹出浮窗承载筛选控件。
// active 由父组件按对应筛选条件是否有值驱动（高亮 + title 提示）。
import { Filter } from '@element-plus/icons-vue'

defineProps<{
  label: string
  active?: boolean
  width?: number
}>()
const emit = defineEmits<{ (e: 'clear'): void }>()
</script>

<template>
  <div class="cf">
    <span class="cf-label">{{ label }}</span>
    <el-popover placement="bottom-start" :width="width ?? 280" trigger="click" :show-arrow="false">
      <template #reference>
        <el-icon
          class="cf-icon"
          :class="{ active }"
          role="button"
          :aria-label="`筛选${label}`"
          :title="active ? `${label}已设置筛选条件` : `筛选${label}`"
        >
          <Filter />
        </el-icon>
      </template>
      <div class="cf-body">
        <slot />
        <div class="cf-actions">
          <el-button size="small" type="primary" link @click="emit('clear')">清除</el-button>
        </div>
      </div>
    </el-popover>
  </div>
</template>

<style scoped>
.cf {
  display: inline-flex;
  align-items: center;
  gap: 4px;
}
.cf-label {
  font-weight: 600;
  color: var(--color-text-regular, #606266);
}
.cf-icon {
  cursor: pointer;
  color: var(--color-text-tertiary);
  font-size: 13px;
  transition: color 0.15s;
}
.cf-icon:hover {
  color: var(--brand-primary);
}
.cf-icon.active {
  color: var(--brand-primary);
}
.cf-body {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.cf-actions {
  display: flex;
  justify-content: flex-end;
}
</style>