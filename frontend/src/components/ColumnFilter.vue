<script setup lang="ts">
// 列头筛选入口（全站统一）：
// 表头正常显示文本 + 右侧过滤图标；点击图标弹出浮窗承载筛选控件。
// 交互要点：浮窗采用受控显隐；浮窗内的下拉/日期选择器等控件会把弹层 teleport 到 body，
// 点击这些弹层不应关闭浮窗（否则会出现“点下拉框就关闭浮窗并刷新列表”的问题），
// 仅在点击浮窗本身、触发图标及其它弹层之外时才关闭。
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { Filter } from '@element-plus/icons-vue'

withDefaults(
  defineProps<{
    label: string
    active?: boolean
    width?: number
  }>(),
  { active: false, width: 300 },
)
const emit = defineEmits<{ (e: 'clear'): void }>()

const visible = ref(false)
const rootRef = ref<HTMLElement | null>(null)
const contentRef = ref<HTMLElement | null>(null)

function toggle() {
  visible.value = !visible.value
}

// 点击落在当前浮窗或其内部控件弹出的覆盖层（均为 .el-popper）时，不关闭浮窗。
// 同时监听 pointerdown（鼠标/触摸）与 click（合成点击/键盘触发），保证各输入方式一致。
function onDocPointerDown(e: Event) {
  if (!visible.value) return
  const t = e.target as HTMLElement | null
  if (!t || typeof t.closest !== 'function') return
  if (rootRef.value?.contains(t)) return // 触发图标/列名区域
  if (contentRef.value?.contains(t)) return // 浮窗内容
  if (t.closest('.el-popper')) return // 浮窗内控件 teleport 出的下拉/日期面板等
  visible.value = false
}

function onKeydown(e: KeyboardEvent) {
  if (e.key === 'Escape') visible.value = false
}

onMounted(() => {
  document.addEventListener('pointerdown', onDocPointerDown, true)
  document.addEventListener('click', onDocPointerDown, true)
  document.addEventListener('keydown', onKeydown)
})
onBeforeUnmount(() => {
  document.removeEventListener('pointerdown', onDocPointerDown, true)
  document.removeEventListener('click', onDocPointerDown, true)
  document.removeEventListener('keydown', onKeydown)
})
</script>

<template>
  <div ref="rootRef" class="cf">
    <span class="cf-label">{{ label }}</span>
    <el-popover
      :visible="visible"
      :width="width"
      placement="bottom-start"
      trigger="manual"
      :show-arrow="false"
      :offset="6"
      popper-class="cf-popper"
    >
      <template #reference>
        <span
          class="cf-icon-wrap"
          role="button"
          :aria-label="`筛选${label}`"
          :title="active ? `${label}已设置筛选条件` : `筛选${label}`"
          @click="toggle"
        >
          <el-icon class="cf-icon" :class="{ active }"><Filter /></el-icon>
        </span>
      </template>
      <div ref="contentRef" class="cf-body">
        <div class="cf-head">{{ label }}</div>
        <div class="cf-fields"><slot /></div>
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
.cf-icon-wrap {
  display: inline-flex;
  align-items: center;
  cursor: pointer;
  border-radius: 4px;
}
.cf-icon {
  color: var(--color-text-tertiary);
  font-size: 13px;
  transition: color 0.15s;
}
.cf-icon-wrap:hover .cf-icon {
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
.cf-head {
  font-size: 12px;
  font-weight: 600;
  color: var(--color-text-secondary);
}
.cf-fields {
  display: flex;
  flex-direction: column;
  gap: 10px;
}
.cf-actions {
  display: flex;
  justify-content: flex-end;
  margin-top: -2px;
}
</style>

<style>
/* 浮窗统一：所有筛选控件撑满浮窗宽度，避免长下拉框溢出 */
.cf-popper .cf-fields > * {
  width: 100%;
}
.cf-popper .cf-fields .el-select,
.cf-popper .cf-fields .el-date-editor,
.cf-popper .cf-fields .el-input,
.cf-popper .cf-fields .el-input-number {
  width: 100%;
}
</style>
