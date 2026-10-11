<script setup lang="ts">
import { onMounted, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { listDevBillings, type BillingItem } from '@/api/dev'
import StatusTag from '@/components/StatusTag.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'
import { resolveBillingRange } from '@/utils/range'

const route = useRoute()
const router = useRouter()
const loading = ref(false)
const list = ref<BillingItem[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

// 时间区间 [开始, 结束]（datetimerange，格式 YYYY-MM-DDTHH:mm:ss±HH:mm）
const range = ref<[string, string] | null>(resolveBillingRange(route.query.range))

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await listDevBillings({
      Page: page.value,
      Size: size.value,
      From: range.value?.[0],
      To: range.value?.[1],
    })
    list.value = res.Data.List
    total.value = res.Data.Total
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}

// 时间区间变更：回第一页并重新加载
function onRangeChange() {
  page.value = 1
  load()
}

// 同一页面内再次从用户面板跳转（URL query 变化）时同步时间区间并重载
watch(
  () => route.query.range,
  (v) => {
    const rng = resolveBillingRange(v)
    if (!rng) return
    range.value = rng
    page.value = 1
    load()
  },
)

onMounted(load)
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">消费账单</div>
      <el-date-picker
        v-model="range"
        type="datetimerange"
        value-format="YYYY-MM-DDTHH:mm:ssZ"
        start-placeholder="开始时间"
        end-placeholder="结束时间"
        size="small"
        @change="onRangeChange"
      />
    </div>

    <div v-loading="loading" class="card">
      <el-table
        :data="list"
        border
        stripe
        class="table-nowrap clickable-rows"
        @row-click="(row: BillingItem) => router.push(`/dev/billings/${row.BillingID}`)"
      >
        <el-table-column label="时间" min-width="170">
          <template #default="{ row }">{{ row.CallTime }}</template>
        </el-table-column>
        <el-table-column label="模型" prop="ExternalModel" min-width="160" show-overflow-tooltip />
        <el-table-column label="模式" width="100" align="center">
          <template #default="{ row }">
            <el-tag :type="row.PricingMode === 'cost' ? 'warning' : 'primary'" size="small" effect="plain">
              {{ row.PricingMode === 'cost' ? '按成本' : '按售价' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="积分" width="110" align="right">
          <template #default="{ row }">{{ row.CreditsConsumed }}</template>
        </el-table-column>
        <el-table-column label="状态" width="90" align="center">
          <template #default="{ row }"><StatusTag :value="row.Status" /></template>
        </el-table-column>
      </el-table>

      <div class="pager">
        <el-pagination
          layout="total, prev, pager, next"
          :total="total"
          :page-size="size"
          :current-page="page"
          @current-change="(p: number) => { page = p; load() }"
        />
      </div>

      <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
    </div>
  </div>
</template>

<style scoped>
.mono {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
  font-size: 12px;
}
.pager {
  display: flex;
  justify-content: flex-end;
  margin-top: 16px;
}
</style>