<script setup lang="ts">
// VChart 通用图表容器：按需引入 ECharts 模块（本地打包，不走外网），
// 负责 init / setOption / ResizeObserver 自适应 / 销毁。
import { onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import * as echarts from 'echarts/core'
import type { EChartsCoreOption } from 'echarts/core'
import { LineChart, BarChart, PieChart, ScatterChart } from 'echarts/charts'
import {
  GridComponent,
  TooltipComponent,
  LegendComponent,
  DataZoomComponent,
} from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'

echarts.use([
  LineChart,
  BarChart,
  PieChart,
  // 散点：失败轮 / 真实切换点 / 预测打平点等「标记类」序列都要它，缺了会被 ECharts 静默忽略。
  ScatterChart,
  GridComponent,
  TooltipComponent,
  LegendComponent,
  DataZoomComponent,
  CanvasRenderer,
])

const props = defineProps<{ option: EChartsCoreOption; height?: string }>()

const el = ref<HTMLDivElement | null>(null)
const chart = shallowRef<echarts.ECharts | null>(null)
let ro: ResizeObserver | null = null

onMounted(() => {
  if (!el.value) return
  chart.value = echarts.init(el.value)
  chart.value.setOption(props.option)
  ro = new ResizeObserver(() => chart.value?.resize())
  ro.observe(el.value)
})

watch(
  () => props.option,
  (o) => chart.value?.setOption(o, true),
  { deep: true },
)

onBeforeUnmount(() => {
  ro?.disconnect()
  ro = null
  chart.value?.dispose()
  chart.value = null
})
</script>

<template>
  <div ref="el" class="v-chart" :style="{ height: props.height || '280px' }"></div>
</template>

<style scoped>
.v-chart {
  width: 100%;
}
</style>