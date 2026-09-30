<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { getDevUsage, type UsageDay } from '@/api/dev'
import ErrorBubble from '@/components/ErrorBubble.vue'

const loading = ref(false)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const list = ref<UsageDay[]>([])

// 柱状图最大刻度，用于相对高度
const maxCredits = computed(() => Math.max(...list.value.map((d) => d.credits), 1))

// 将时间与量格式化展示（tooltip 真实值）
function toLocal(d: string) {
  return d // 后端返回 ISO 日期 (YYYY-MM-DD)
}

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await getDevUsage(30)
    list.value = res.data.list
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
onMounted(load)

// 汇总：总积分、总调用量
const totals = computed(() =>
  list.value.reduce(
    (acc, d) => ({ credits: acc.credits + d.credits, calls: acc.calls + d.calls }),
    { credits: 0, calls: 0 },
  ),
)

// 横轴标签抽稀：30 根柱子若全部显示日期会重叠，按步长只保留约 10 个刻度
const labelStep = computed(() => Math.max(1, Math.ceil(list.value.length / 10)))

// 后端会补零返回近 30 天（每天都有条目），因此「有无用量」不能看 list.length，
// 需判断是否存在真实消耗/调用，否则会渲染出一片空白柱状区 + 密集刻度。
const hasUsage = computed(() => list.value.some((d) => d.credits > 0 || d.calls > 0))
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">用量统计</div>
      <el-button :loading="loading" @click="load">刷新</el-button>
    </div>

    <div v-loading="loading" class="card">
      <div class="sum-row">
        <span>近 30 天总计消耗：<b class="blue">{{ totals.credits.toFixed(2) }}</b> 积分 / <b>{{ totals.calls }}</b> 次调用</span>
      </div>

      <!-- 按日柱状图：简单 div 柱条，避免引入大体积图表依赖 -->
      <div v-if="hasUsage" class="chart">
        <div v-for="(d, i) in list" :key="i" class="bar-col" :title="`${toLocal(d.date)}：${d.credits.toFixed(2)} 积分 · ${d.calls} 次`">
          <div class="bar-wrap">
            <div class="bar" :style="{ height: Math.max((d.credits / maxCredits) * 100, d.credits > 0 ? 2 : 0) + '%' }"></div>
          </div>
          <div class="bar-label" :class="{ 'is-hidden': i % labelStep !== 0 }">{{ toLocal(d.date).slice(5) }}</div>
        </div>
      </div>

      <el-empty v-if="!loading && !hasUsage" description="近 30 天暂无用量" />
      <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
    </div>

    <!-- 按模型汇总：开发端暂未提供按模型聚合接口，保留占位 -->
    <div class="card" style="margin-top: 16px">
      <div class="block-title">按模型汇总</div>
      <el-empty description="开发端接口未提供按模型聚合数据" :image-size="60" />
    </div>
  </div>
</template>

<style scoped>
.sum-row {
  color: #666;
  margin-bottom: 16px;
  font-size: 14px;
}
.sum-row .blue {
  color: var(--color-primary);
}
.chart {
  display: flex;
  align-items: flex-end;
  gap: 3px;
  height: 220px;
  padding: 8px 4px 0;
  border-bottom: 1px solid #eee;
  overflow-x: auto;
}
.bar-col {
  flex: 1 0 18px;
  display: flex;
  flex-direction: column;
  align-items: center;
  height: 100%;
  gap: 4px;
}
.bar-wrap {
  flex: 1;
  width: 100%;
  display: flex;
  align-items: flex-end;
  justify-content: center;
}
.bar {
  width: 70%;
  background: linear-gradient(180deg, var(--color-accent), var(--color-primary));
  border-radius: 2px 2px 0 0;
  min-height: 0;
}
.bar-label {
  font-size: 10px;
  color: var(--color-text-secondary);
  margin-top: 2px;
  white-space: nowrap;
}
/* 抽稀后的隐藏刻度：保留占位以维持柱体对齐 */
.bar-label.is-hidden {
  visibility: hidden;
}
</style>