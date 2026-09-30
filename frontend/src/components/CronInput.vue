<script setup lang="ts">
// CronInput：正常态探测频率编辑器（始终使用 6 段 cron 表达式）。
// 右侧「常用」下拉提供常用频率快捷项，输入框校验 6 段格式。
import { computed } from 'vue'
import { Clock } from '@element-plus/icons-vue'

const props = withDefaults(
  defineProps<{
    modelValue: string
    placeholder?: string
  }>(),
  { placeholder: '0 * * * * *' },
)
const emit = defineEmits<{ (e: 'update:modelValue', v: string): void }>()

const text = computed({
  get: () => props.modelValue,
  set: (v: string) => emit('update:modelValue', v),
})

const valid = computed(() => props.modelValue.trim().split(/\s+/).length === 6)

// 常用频率快捷项（6 段 cron：秒 分 时 日 月 周）
const presets = [
  { label: '30 分钟', expr: '0 */30 * * * *' },
  { label: '5 小时', expr: '0 0 */5 * * *' },
  { label: '1 天', expr: '0 0 0 * * *' },
  { label: '1 周', expr: '0 0 0 * * 1' },
  { label: '1 月', expr: '0 0 0 1 * *' },
]

function applyPreset(p: { label: string; expr: string }) {
  emit('update:modelValue', p.expr)
}
</script>

<template>
  <div class="cron-simple">
    <el-input :model-value="text" :placeholder="placeholder" class="cron-expr" />
    <el-tooltip content="常用频率" placement="top">
      <el-dropdown trigger="click">
        <el-button plain><el-icon><Clock /></el-icon></el-button>
        <template #dropdown>
          <el-dropdown-menu>
            <el-dropdown-item v-for="p in presets" :key="p.label" @click="applyPreset(p)">
              {{ p.label }}
            </el-dropdown-item>
          </el-dropdown-menu>
        </template>
      </el-dropdown>
    </el-tooltip>
    <span v-if="!valid" class="cron-invalid">需 6 段（秒 分 时 日 月 周）</span>
  </div>
</template>

<style scoped>
.cron-simple {
  display: flex;
  align-items: center;
  gap: 6px;
  width: 100%;
}
.cron-expr {
  width: 230px;
  flex: none;
}
.cron-invalid {
  font-size: 12px;
  color: #f56c6c;
}
</style>