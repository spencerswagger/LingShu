<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { listDevBillings, type BillingItem } from '@/api/dev'
import StatusTag from '@/components/StatusTag.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'

const router = useRouter()
const loading = ref(false)
const list = ref<BillingItem[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await listDevBillings(page.value, size.value)
    list.value = res.data.list
    total.value = res.data.total
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
onMounted(load)
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">消费账单</div>
    </div>

    <div v-loading="loading" class="card">
      <el-table
        :data="list"
        border
        stripe
        class="table-nowrap clickable-rows"
        @row-click="(row: BillingItem) => router.push(`/dev/billings/${row.billing_id}`)"
      >
        <el-table-column label="账单 ID" prop="billing_id" min-width="260" show-overflow-tooltip>
          <template #default="{ row }">
            <span class="mono">{{ row.billing_id }}</span>
          </template>
        </el-table-column>
        <el-table-column label="时间" min-width="170">
          <template #default="{ row }">{{ row.call_time }}</template>
        </el-table-column>
        <el-table-column label="模型" prop="model" min-width="160" show-overflow-tooltip />
        <el-table-column label="模式" width="100" align="center">
          <template #default="{ row }">
            <el-tag :type="row.pricing_mode === 'cost' ? 'warning' : 'primary'" size="small" effect="plain">
              {{ row.pricing_mode === 'cost' ? '按成本' : '按售价' }}
            </el-tag>
          </template>
        </el-table-column>
        <el-table-column label="积分" width="110" align="right">
          <template #default="{ row }">{{ row.credits_consumed }}</template>
        </el-table-column>
        <el-table-column label="状态" width="90" align="center">
          <template #default="{ row }"><StatusTag :value="row.status" /></template>
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