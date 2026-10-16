<script setup lang="ts">
// 模型页：同一路由内切换「模型 / Auto 模型」两个列表（组件切换，不跳转路由）。
// 选中项用 sessionStorage 记忆，便于从 Auto 模型子页（新建/编辑）返回时保持原视图。
import { computed, ref } from 'vue'
import ModelList from './ModelList.vue'
import AutoModelList from './AutoModelList.vue'

type Tab = 'models' | 'auto'

const props = withDefaults(defineProps<{ mode?: 'admin' | 'self' }>(), { mode: 'admin' })

const segOptions = [
  { label: '模型', value: 'models' },
  { label: 'Auto 模型', value: 'auto' },
]

// 管理端与开发者端各自记忆，互不干扰
const storageKey = computed(() => (props.mode === 'self' ? 'dev-models-tab' : 'admin-models-tab'))

const tab = ref<Tab>(sessionStorage.getItem(storageKey.value) === 'auto' ? 'auto' : 'models')

function onSegChange(v: string | number | boolean) {
  tab.value = v === 'auto' ? 'auto' : 'models'
  sessionStorage.setItem(storageKey.value, tab.value)
}
</script>

<template>
  <ModelList v-if="tab === 'models'" :mode="mode">
    <template #header-left>
      <el-segmented :model-value="tab" :options="segOptions" @change="onSegChange" />
    </template>
  </ModelList>
  <AutoModelList v-else :mode="mode">
    <template #header-left>
      <el-segmented :model-value="tab" :options="segOptions" @change="onSegChange" />
    </template>
  </AutoModelList>
</template>
