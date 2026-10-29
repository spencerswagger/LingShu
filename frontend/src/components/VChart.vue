<script setup lang="ts">
// VChart 通用图表容器：按需引入 ECharts 模块（本地打包，不走外网），
// 负责 init / setOption / 自适应 / 销毁。
// 自适应双保险：ResizeObserver 跟容器尺寸（侧栏折叠、卡片流式布局），
// window resize 兜底（容器尺寸没变但视口变了，如浏览器缩放手势、旋屏）。
import { onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import * as echarts from 'echarts/core'
import type { EChartsCoreOption } from 'echarts/core'
import { LineChart, BarChart, PieChart } from 'echarts/charts'
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
let raf = 0

// 视口 resize 兜底：用 rAF 合并同一帧内的多次触发，避免 resize 抖动时反复重排。
function onWindowResize() {
  if (raf) return
  raf = requestAnimationFrame(() => {
    raf = 0
    chart.value?.resize()
  })
}

onMounted(() => {
  if (!el.value) return
  chart.value = echarts.init(el.value)
  chart.value.setOption(props.option)
  ro = new ResizeObserver(() => chart.value?.resize())
  ro.observe(el.value)
  window.addEventListener('resize', onWindowResize)
})

watch(
  () => props.option,
  (o) => chart.value?.setOption(o, true),
  { deep: true },
)

onBeforeUnmount(() => {
  window.removeEventListener('resize', onWindowResize)
  if (raf) {
    cancelAnimationFrame(raf)
    raf = 0
  }
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