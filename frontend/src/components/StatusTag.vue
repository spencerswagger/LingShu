<script setup lang="ts">
import { computed } from 'vue'

// 通用状态徽标：根据 value 映射为不同颜色的 el-tag。
// 可通过 props.options 自定义映射；未命中时显示原始值（灰色）。
type Option = { label?: string; type?: 'success' | 'warning' | 'danger' | 'info' | 'primary' }

const props = withDefaults(
  defineProps<{
    value?: string
    // 自定义映射表，如 { ACTIVE: '启用', DISABLED: '禁用' }
    labelMap?: Record<string, string>
    typeMap?: Record<string, string>
    options?: Record<string, Option>
  }>(),
  {
    value: '',
    labelMap: () => ({}),
    typeMap: () => ({}),
    options: () => ({}),
  },
)

interface Resolved {
  label: string
  type: string
}

// 内置映射：令牌 / 账单 / 公告级别
const builtin: Record<string, Resolved> = {
  // 令牌
  ACTIVE: { label: '启用', type: 'success' },
  DISABLED: { label: '禁用', type: 'danger' },
  // 账单
  completed: { label: '成功', type: 'success' },
  failed: { label: '失败', type: 'danger' },
  // 公告级别
  info: { label: 'Info', type: 'primary' },
  warning: { label: 'Warning', type: 'warning' },
  danger: { label: 'Danger', type: 'danger' },
  // 用户状态
  ENABLED: { label: '正常', type: 'success' },
  // 渠道/内部模型三态（含历史旧值，颜色全局统一；DISABLED 已在上方令牌段映射）
  NORMAL: { label: '正常', type: 'success' },
  HEALTHY: { label: '正常', type: 'success' },
  DRAIN: { label: '排空', type: 'warning' },
  DRAIN_ONLY: { label: '排空', type: 'warning' },
  UNAVAILABLE: { label: '禁用', type: 'danger' },
}

const resolved = computed<Resolved>(() => {
  const v = props.value
  if (props.options[v]) {
    const o = props.options[v]
    return { label: o.label || v, type: o.type || 'info' }
  }
  if (builtin[v]) return builtin[v]
  return {
    label: props.labelMap[v] || v || '-',
    type: props.typeMap[v] || 'info',
  }
})
</script>

<template>
  <el-tag :type="(resolved.type as any)" size="small" effect="light">{{ resolved.label }}</el-tag>
</template>