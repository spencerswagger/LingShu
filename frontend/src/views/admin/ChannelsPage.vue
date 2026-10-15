<script setup lang="ts">
// 渠道页：同一路由内切换「渠道 / 渠道协议」两个列表（组件重载，不跳转路由）。
// 选中项用 sessionStorage 记忆，便于从渠道协议子页（新建/编辑）返回时保持原视图。
import { ref } from 'vue'
import ChannelList from './ChannelList.vue'
import ChannelProtocolList from './ChannelProtocolList.vue'

type Tab = 'channels' | 'protocols'
const STORAGE_KEY = 'admin-channels-tab'

const segOptions = [
  { label: '渠道', value: 'channels' },
  { label: '渠道协议', value: 'protocols' },
]

const tab = ref<Tab>(sessionStorage.getItem(STORAGE_KEY) === 'protocols' ? 'protocols' : 'channels')

function onSegChange(v: string | number | boolean) {
  tab.value = v === 'protocols' ? 'protocols' : 'channels'
  sessionStorage.setItem(STORAGE_KEY, tab.value)
}
</script>

<template>
  <ChannelList v-if="tab === 'channels'">
    <template #header-left>
      <el-segmented :model-value="tab" :options="segOptions" @change="onSegChange" />
    </template>
  </ChannelList>
  <ChannelProtocolList v-else>
    <template #header-left>
      <el-segmented :model-value="tab" :options="segOptions" @change="onSegChange" />
    </template>
  </ChannelProtocolList>
</template>
