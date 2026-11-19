<script setup lang="ts">
// 自定义边：贝塞尔曲线 + 双粒子（两个发光圆点沿路径错相循环）。
// 颜色跟随边的 style.stroke；粒子用 SVG animateMotion 沿同一贝塞尔路径运动。
import { computed } from 'vue'
import { BaseEdge, getBezierPath, type EdgeProps } from '@vue-flow/core'

const props = defineProps<EdgeProps>()

const path = computed(() => {
  const [p] = getBezierPath({
    sourceX: props.sourceX,
    sourceY: props.sourceY,
    targetX: props.targetX,
    targetY: props.targetY,
  })
  return p
})

const stroke = computed(() => (props.style?.stroke as string) || '#33415e')
</script>

<template>
  <BaseEdge :id="props.id" :path="path" :style="props.style" />
  <circle r="2.4" :fill="stroke">
    <animateMotion :path="path" dur="1.8s" repeatCount="indefinite" />
  </circle>
  <circle r="1.5" :fill="stroke" opacity="0.55">
    <animateMotion :path="path" dur="1.8s" begin="-0.9s" repeatCount="indefinite" />
  </circle>
</template>
