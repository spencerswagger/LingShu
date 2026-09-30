<script setup lang="ts">
// 钱包充值统一弹窗：用户列表与用户详情共用。
// 三种作用方式：增加（当前值+输入，默认）、减少（当前值-输入）、覆盖（直接 set 目标值）。
// 增加/减少基于当前值增减，覆盖直接写入，均为单条原子 UPDATE。
import { computed, ref, watch } from 'vue'
import { ElMessage } from 'element-plus'
import { rechargeWallet, adjustWallet, setWallet } from '@/api/admin'

type OpMode = 'add' | 'sub' | 'set'

const props = defineProps<{
  modelValue: boolean
  userId: number
  username: string
  nickname?: string
  balance: number
}>()
const emit = defineEmits<{ (e: 'update:modelValue', v: boolean): void; (e: 'success'): void }>()

const visible = computed({
  get: () => props.modelValue,
  set: (v) => emit('update:modelValue', v),
})

const QUICK = [10, 50, 100, 500]
const opMode = ref<OpMode>('add')
const amount = ref(0)
const remark = ref('')
const loading = ref(false)

watch(visible, (v) => {
  if (v) {
    opMode.value = 'add'
    amount.value = 0
    remark.value = ''
  }
})

const balanceText = computed(() =>
  (props.balance ?? 0).toLocaleString(undefined, { maximumFractionDigits: 4 }),
)

// 操作后预览余额。
const preview = computed(() => {
  const b = props.balance ?? 0
  if (opMode.value === 'add') return b + amount.value
  if (opMode.value === 'sub') return b - amount.value
  return amount.value
})
const previewValid = computed(() => preview.value >= 0)

const MODES: { value: OpMode; label: string }[] = [
  { value: 'add', label: '增加' },
  { value: 'sub', label: '减少' },
  { value: 'set', label: '覆盖' },
]

async function submit() {
  if (!amount.value && amount.value !== 0) return ElMessage.warning('请输入积分数')
  if (opMode.value !== 'set' && amount.value <= 0)
    return ElMessage.warning('请输入大于 0 的积分数')
  if (opMode.value === 'set' && amount.value < 0)
    return ElMessage.warning('目标余额不能为负')
  if (opMode.value === 'sub' && !previewValid)
    return ElMessage.warning('减少后余额不能为负')
  loading.value = true
  try {
    if (opMode.value === 'add') {
      await rechargeWallet(props.userId, amount.value, remark.value)
    } else if (opMode.value === 'sub') {
      await adjustWallet(props.userId, -amount.value, remark.value)
    } else {
      await setWallet(props.userId, amount.value, remark.value)
    }
    ElMessage.success('操作成功')
    visible.value = false
    emit('success')
  } catch (e: any) {
    ElMessage.error(e?.message || '操作失败')
  } finally {
    loading.value = false
  }
}
</script>

<template>
  <el-dialog v-model="visible" title="充值积分" width="440px" :close-on-click-modal="false">
    <div class="wc-user">
      <span class="wc-name">{{ nickname || username }}</span>
      <span v-if="nickname" class="wc-sub">@{{ username }}</span>
      <span class="wc-balance">
        当前余额 <b>{{ balanceText }}</b> 积分
      </span>
    </div>
    <el-form label-width="80px">
      <el-form-item label="方式">
        <el-radio-group v-model="opMode">
          <el-radio-button v-for="m in MODES" :key="m.value" :value="m.value">{{ m.label }}</el-radio-button>
        </el-radio-group>
      </el-form-item>
      <el-form-item label="积分数">
        <el-input-number v-model="amount" :precision="2" :min="0" style="width: 100%" />
        <div v-if="opMode === 'set'" class="wc-tip">直接设置为目标余额</div>
        <div v-else class="wc-quick">
          <el-button v-for="q in QUICK" :key="q" size="small" plain @click="amount = q">
            {{ opMode === 'sub' ? '-' : '+' }}{{ q }}
          </el-button>
        </div>
      </el-form-item>
      <el-form-item label="结果余额">
        <span class="wc-preview" :class="{ invalid: !previewValid }">
          {{ preview.toLocaleString(undefined, { maximumFractionDigits: 4 }) }} 积分
        </span>
      </el-form-item>
      <el-form-item label="备注">
        <el-input v-model="remark" type="textarea" :rows="2" placeholder="选填" />
      </el-form-item>
    </el-form>
    <template #footer>
      <el-button @click="visible = false">取消</el-button>
      <el-button type="primary" :loading="loading" @click="submit">确认</el-button>
    </template>
  </el-dialog>
</template>

<style scoped>
.wc-user {
  display: flex;
  align-items: baseline;
  gap: 6px;
  margin-bottom: 14px;
  padding: 10px 14px;
  border-radius: 8px;
  background: var(--color-primary-light);
}
.wc-name {
  font-size: 14px;
  font-weight: 600;
  color: var(--color-text);
}
.wc-sub {
  font-size: 12px;
  color: var(--color-text-tertiary);
}
.wc-balance {
  margin-left: auto;
  font-size: 12px;
  color: var(--color-text-secondary);
  font-variant-numeric: tabular-nums;
}
.wc-balance b {
  font-size: 15px;
  color: var(--color-primary);
}
.wc-tip {
  font-size: 12px;
  color: var(--color-text-secondary);
  margin-top: 4px;
}
.wc-quick {
  display: flex;
  gap: 8px;
  margin-top: 8px;
  flex-wrap: wrap;
}
.wc-preview {
  font-size: 14px;
  font-weight: 600;
  color: var(--color-primary);
  font-variant-numeric: tabular-nums;
}
.wc-preview.invalid {
  color: var(--color-danger);
}
</style>