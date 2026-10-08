<script setup lang="ts">
// 公共定价编辑器：以「同一套定价 schema」复用于售价(SaleRates)与成本(CostRates)。
// 结构分三块：
//   1) 五段费率（token 单价）
//   2) 时段系数（Periodic + Overrides + Default + Timezone）
//   3) 上下文分档（ContextTiers）
// 对齐后端 billing.Rates / TimeCoeffConfig / TierRule JSON 结构。
// 「配置模型级」开关：关闭时 TimeConfig / ContextTiers 置 null，运行时回落全局兜底。
import { computed, onMounted, reactive, ref, watch } from 'vue'
import { Plus, Delete, QuestionFilled } from '@element-plus/icons-vue'
import { getBillingConfig, rateKeys, rateLabels, rateHints, type ContextTier, type Segment, type DateOverride } from '@/api/admin'

const props = defineProps<{
  // 完整定价对象（含 showRatesKey 指定的费率键 + time_config + context_tiers）
  modelValue: Record<string, any>
  // 'sale_rates' | 'cost_rates'；为空字符串时隐藏费率区（仅编辑时段/分档）
  showRatesKey?: string
  // 区块标题（如「售价」/「成本」）
  title?: string
  // 积分换算底数 R（不传则自动从计费配置加载）。
  creditR?: number
}>()
const emit = defineEmits<{
  (e: 'update:modelValue', v: Record<string, any>): void
}>()

const r = ref<number>(10000)
onMounted(async () => {
  if (props.creditR != null) {
    r.value = Number(props.creditR) || 10000
    return
  }
  try {
    const res = await getBillingConfig()
    const v = Number(res.Data.R)
    if (v > 0) r.value = v
  } catch {
    /* 保持默认 */
  }
})

// ---- 倍率录入 ----
// 存储值 rate = 每百万 token 计费倍率（照抄官方定价数值录入，无单位）。
// 展示时在输入框右侧给出「≈ 积分/百万 token」辅助参考：积分 = 倍率 × 1e6 ÷ R。
const creditPerM = (rate: number): number => Math.round(((rate || 0) * 1e6) / r.value * 1e6) / 1e6

const showRates = computed(() => !!props.showRatesKey)

// 内部编辑态（避免直接改 prop）
const edit = reactive({
  rates: {} as Record<string, number>,
  Timezone: 'Asia/Shanghai',
  Default: 1 as number,
  Periodic: [] as Segment[],
  Overrides: [] as DateOverride[],
  Tiers: [] as ContextTier[],
  // true = 启用模型级时段/分档；false = 用全局默认（TimeConfig/ContextTiers 置 null）
  Configured: false,
})

const commonZones = [
  'Asia/Shanghai',
  'Asia/Hong_Kong',
  'Asia/Tokyo',
  'Asia/Singapore',
  'UTC',
  'Europe/London',
  'America/New_York',
]

// 默认四档上下文模板（与全局兜底默认一致，供一键填充）
function defaultTiers(): ContextTier[] {
  return [
    { Min: 0, Max: 8000, Coeff: 1 },
    { Min: 8001, Max: 32768, Coeff: 1.5 },
    { Min: 32769, Max: 131072, Coeff: 2 },
    { Min: 131073, Max: null, Coeff: 3 },
  ]
}

// 从 modelValue 同步进内部编辑态（倍率直接展示，无单位换算）
function syncFromModel() {
  const mv = props.modelValue || {}
  if (showRates.value && props.showRatesKey) {
    const rates = mv[props.showRatesKey] || {}
    for (const k of rateKeys) {
      const v = rates[k]
      edit.rates[k] = typeof v === 'number' ? round6(v) : 0
    }
  }
  const tc = mv.TimeConfig || null
  edit.Configured = !!(tc || (mv.ContextTiers && mv.ContextTiers.length))
  if (tc) {
    edit.Timezone = tc.Timezone || 'Asia/Shanghai'
    edit.Default = typeof tc.Default === 'number' ? tc.Default : 1
    edit.Periodic = (tc.Periodic || []).map((s: Segment) => ({ ...s }))
    edit.Overrides = (tc.Overrides || []).map((o: DateOverride) => ({
      ...o,
      segments: (o.Segments || []).map((s) => ({ ...s })),
    }))
  } else {
    edit.Timezone = 'Asia/Shanghai'
    edit.Default = 1
    edit.Periodic = []
    edit.Overrides = []
  }
  edit.Tiers = mv.ContextTiers && mv.ContextTiers.length
    ? mv.ContextTiers.map((t: ContextTier) => ({ ...t }))
    : defaultTiers()
}

const round6 = (n: number) => Math.round(n * 1e6) / 1e6

// 过滤掉空 name 的时段行
function cleanSegs(list: Segment[]): Segment[] {
  return list
    .filter((s) => !!s.Name)
    .map((s) => ({ Name: s.Name, Start: s.Start || '00:00', End: s.End || '24:00', Coeff: Number(s.Coeff) }))
}

// 打包为 modelValue 完整对象（倍率即存储值，无换算）
function pack(): Record<string, any> {
  const out: Record<string, any> = {}
  if (showRates.value && props.showRatesKey) {
    const rates: Record<string, number> = {}
    for (const k of rateKeys) rates[k] = round6(Number(edit.rates[k] ?? 0))
    out[props.showRatesKey] = rates
  }
  if (edit.Configured) {
    out.TimeConfig = {
      Timezone: edit.Timezone,
      Default: Number(edit.Default),
      Periodic: cleanSegs(edit.Periodic),
      Overrides: edit.Overrides.map((o) => ({ ...o, Segments: cleanSegs(o.Segments) })),
    }
    out.ContextTiers = edit.Tiers.map((t) => ({ Min: t.Min, Max: t.Max ?? null, Coeff: t.Coeff }))
  } else {
    out.TimeConfig = null
    out.ContextTiers = null
  }
  return out
}

// 在运行时用「过去一次 emit 的快照」比对，用于：内部编辑 → emit；外部（重新加载）替换 → 回填
// 防止自身 emit 回填引发死循环
let lastEmit: string
function recordEmit(o: Record<string, any>) {
  lastEmit = JSON.stringify(o)
}
// 内部编辑变化（深度监听）→ 重新打包并 emit
watch(
  edit,
  () => {
    const p = pack()
    const json = JSON.stringify(p)
    if (json === lastEmit) return
    recordEmit(p)
    emit('update:modelValue', p)
  },
  { deep: true },
)
// 外部替换 modelValue（如重新加载模型/应用参考价后）→ 重新回填内部态并对外回写，避免死循环
watch(
  () => props.modelValue,
  (nv) => {
    const json = JSON.stringify(nv)
    if (json == null || json === lastEmit) return
    syncFromModel()
    const p = pack()
    recordEmit(p)
    emit('update:modelValue', p)
  },
  { deep: true },
)

// R（积分换算底数）加载完成后：按新底数重算费率展示并对外回写
watch(r, () => {
  syncFromModel()
  const p = pack()
  recordEmit(p)
  emit('update:modelValue', p)
})

// 开关切换：开启则用内部已填值（含默认模板）；关闭则二者置空
function onConfiguredChange(v: boolean) {
  if (v && !edit.Tiers.length) edit.Tiers = defaultTiers()
  const p = pack()
  recordEmit(p)
  emit('update:modelValue', p)
}

function addTier() {
  edit.Tiers.push({ Min: 0, Max: null, Coeff: 1 })
}
function delTier(i: number) {
  edit.Tiers.splice(i, 1)
}
function addSeg(list: Segment[]) {
  list.push({ Name: '', Start: '00:00', End: '24:00', Coeff: 1 })
}
function addOverride() {
  edit.Overrides.push({ Name: '', Start: '', End: '', Segments: [] })
}
async function delOverride(i: number) {
  edit.Overrides.splice(i, 1)
}

// 初始化：先把传入值结算进内部编辑态，并记录快照（不主动 emit，
// 否则会在父组件异步加载完成前用初始值替换父组件绑定，造成售价/成本回填丢失）
syncFromModel()
recordEmit(pack())
</script>

<template>
  <div class="pricing">
    <!-- 费率（每百万 token 倍率） -->
    <template v-if="showRates">
      <div class="section-title">
        <span class="title-text">{{ title || '定价' }}（每百万 token）</span>
        <el-tooltip :content="'录入倍率（照抄官方定价数值即可）。消耗积分 = Σ(token × 倍率) × 时段/分档系数 ÷ R。'" placement="top">
          <el-icon class="hint-icon"><QuestionFilled /></el-icon>
        </el-tooltip>
        <span class="title-spacer" />
        <slot name="title-extra" />
      </div>
      <div class="rates-grid">
        <div v-for="k in rateKeys" :key="k" class="rate-item">
          <el-tooltip :content="rateHints[k]" placement="top">
            <span class="rate-label">{{ rateLabels[k] }}</span>
          </el-tooltip>
          <el-input-number
            v-model="edit.rates[k]"
            :min="0"
            :precision="6"
            :controls="false"
            class="rate-input"
          />
          <span class="credit-ref">≈ {{ creditPerM(edit.rates[k]) }} 积分</span>
        </div>
      </div>
      <div class="unit-note">
        倍率 = 每百万 token 计费数值，照抄官方定价录入（积分/百万 = 倍率 × 1e6 ÷ R）。
      </div>

      <div v-if="!edit.Configured" class="fallback-note">
        当前未配置模型级时段/分档，计费将使用「全局时段/分档默认值」。
      </div>
    </template>

    <!-- 时段系数 + 上下文分档（模型级开关） -->
    <div class="section-title">
      <span class="title-text">时段与分档</span>
      <el-switch
        v-model="edit.Configured"
        inline-prompt
        active-text="模型级"
        inactive-text="全局默认"
        style="margin-left: 8px"
        @change="onConfiguredChange"
      />
      <el-tooltip
        :content="'开启后填写该模型的模型级时段/分档；关闭则复用「全局时段/分档默认值」，修改全局默认对所有未单独配置的模型生效'"
        placement="top"
      >
        <el-icon class="hint-icon"><QuestionFilled /></el-icon>
      </el-tooltip>
    </div>

    <template v-if="edit.Configured">
      <!-- 时段配置 -->
      <div class="sub-title">
        时段系数
        <el-tooltip :content="'按小时/时段调整计费系数：命中时段内单价=费率×该段系数。系数 1 表示不加价，>1 高峰加价，<1 低谷优惠'" placement="top">
          <el-icon class="hint-icon-sm"><QuestionFilled /></el-icon>
        </el-tooltip>
      </div>
      <div class="form-row">
        <span class="label">时区</span>
        <el-select v-model="edit.Timezone" filterable allow-create default-first-option style="width: 200px">
          <el-option v-for="z in commonZones" :key="z" :label="z" :value="z" />
        </el-select>
        <span class="divider" />
        <span class="label">默认系数</span>
        <el-input-number v-model="edit.Default" :min="0" :precision="3" :step="0.1" />
        <el-tooltip :content="'未命中任何时段时采用的系数（通常为 1）。所有时段都不覆盖的时间段按此计费'" placement="top">
          <el-icon class="hint-icon-sm"><QuestionFilled /></el-icon>
        </el-tooltip>
      </div>

      <div class="sub-title">周期时段（Periodic）</div>
      <el-table :data="edit.Periodic" border class="table-nowrap">
        <el-table-column label="名称" min-width="120">
          <template #default="{ row }"><el-input v-model="row.Name" placeholder="如 高峰段" /></template>
        </el-table-column>
        <el-table-column label="开始 (HH:MM)" width="130">
          <template #default="{ row }"><el-input v-model="row.Start" placeholder="00:00" /></template>
        </el-table-column>
        <el-table-column label="结束 (HH:MM)" width="130">
          <template #default="{ row }"><el-input v-model="row.End" placeholder="24:00" /></template>
        </el-table-column>
        <el-table-column label="系数" width="120">
          <template #default="{ row }">
            <el-input-number v-model="row.Coeff" :min="0" :precision="3" :step="0.1" style="width: 100%" />
          </template>
        </el-table-column>
        <el-table-column label="操作" width="80" align="right">
          <template #default="{ $index }">
            <el-button type="danger" text size="small" :icon="Delete" @click="edit.Periodic.splice($index, 1)" />
          </template>
        </el-table-column>
      </el-table>
      <div class="inline-actions">
        <el-button size="small" :icon="Plus" @click="addSeg(edit.Periodic)">添加时段</el-button>
      </div>

      <!-- 日期覆盖 -->
      <div class="sub-title">
        特定日期区间（Overrides）
        <el-tooltip :content="'规定特定日期区间（如国庆/活动日）内使用覆盖时段，覆盖区间内的时段优先于每日周期时段。每个区间内的时段若未命中则回落周期时段，再回落默认系数'" placement="top">
          <el-icon class="hint-icon-sm"><QuestionFilled /></el-icon>
        </el-tooltip>
      </div>
      <div v-for="(ov, oi) in edit.Overrides" :key="oi" class="override-block">
        <div class="override-head">
          <span class="label">名称</span>
          <el-input v-model="ov.Name" placeholder="如 春节" style="width: 140px" />
          <span class="label">起始日期</span>
          <el-date-picker v-model="ov.Start" type="date" value-format="YYYY-MM-DD" placeholder="YYYY-MM-DD" style="width: 140px" />
          <span class="label">结束日期</span>
          <el-date-picker v-model="ov.End" type="date" value-format="YYYY-MM-DD" placeholder="YYYY-MM-DD" style="width: 140px" />
          <el-button type="danger" text size="small" :icon="Delete" @click="delOverride(oi)" />
        </div>
        <el-table :data="ov.Segments" border class="table-nowrap override-table">
          <el-table-column label="名称" min-width="120">
            <template #default="{ row }"><el-input v-model="row.Name" placeholder="时段名" /></template>
          </el-table-column>
          <el-table-column label="开始" width="120">
            <template #default="{ row }"><el-input v-model="row.Start" placeholder="00:00" /></template>
          </el-table-column>
          <el-table-column label="结束" width="120">
            <template #default="{ row }"><el-input v-model="row.End" placeholder="24:00" /></template>
          </el-table-column>
          <el-table-column label="系数" width="120">
            <template #default="{ row }">
              <el-input-number v-model="row.Coeff" :min="0" :precision="3" :step="0.1" style="width: 100%" />
            </template>
          </el-table-column>
          <el-table-column label="操作" width="70" align="right">
            <template #default="{ $index }">
              <el-button type="danger" text size="small" :icon="Delete" @click="ov.Segments.splice($index, 1)" />
            </template>
          </el-table-column>
        </el-table>
        <el-button size="small" :icon="Plus" @click="addSeg(ov.Segments)">添加覆盖时段</el-button>
      </div>
      <div class="inline-actions" style="margin-top: 8px">
        <el-button size="small" :icon="Plus" @click="addOverride">添加日期区间</el-button>
      </div>

      <!-- 上下文分档 -->
      <div class="sub-title">
        上下文分档（ContextTiers）
        <el-tooltip :content="'按单次请求输入 token 数分档调整单价：落在 [min, max] 档位的请求按该档系数计费。max 为空表示无上限。越长上下文越贵，以补偿更高算力成本'" placement="top">
          <el-icon class="hint-icon-sm"><QuestionFilled /></el-icon>
        </el-tooltip>
      </div>
      <el-table :data="edit.Tiers" border class="table-nowrap">
        <el-table-column label="下限 (min)" width="140">
          <template #default="{ row }">
            <el-input-number v-model="row.Min" :min="0" :controls="false" style="width: 100%" />
          </template>
        </el-table-column>
        <el-table-column label="上限 (max)" width="140">
          <template #default="{ row }">
            <el-input-number v-model="row.Max" :min="0" placeholder="空=无上限" :controls="false" style="width: 100%" />
          </template>
        </el-table-column>
        <el-table-column label="系数 (coeff)" width="140">
          <template #default="{ row }">
            <el-input-number v-model="row.Coeff" :min="0" :precision="3" :step="0.1" style="width: 100%" />
          </template>
        </el-table-column>
        <el-table-column label="操作" width="80" align="right">
          <template #default="{ $index }">
            <el-button type="danger" text size="small" :icon="Delete" @click="delTier($index)" />
          </template>
        </el-table-column>
      </el-table>
      <div class="inline-actions">
        <el-button size="small" :icon="Plus" @click="addTier">添加分档</el-button>
      </div>
    </template>

    <el-empty v-else :image-size="60" description="使用全局时段/分档默认值" style="padding: 12px 0" />
  </div>
</template>

<style scoped>
.pricing {
  display: flex;
  flex-direction: column;
}
.section-title {
  font-size: 15px;
  font-weight: 600;
  margin: 8px 0 12px;
  padding-top: 8px;
  border-top: 1px solid #f0f0f0;
  display: flex;
  align-items: center;
  gap: 8px;
}
.title-spacer {
  flex: 1;
}
.title-text {
  display: inline-flex;
  align-items: center;
}
.hint-icon {
  margin-left: 6px;
  cursor: pointer;
  color: var(--color-text-secondary);
}
.unit-note {
  font-size: 12px;
  color: var(--color-text-tertiary);
  margin: 6px 0 2px;
}
.credit-ref {
  font-size: 12px;
  color: var(--color-text-tertiary);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
  flex: 1;
  min-width: 0;
}
.hint-icon-sm {
  margin-left: 4px;
  cursor: pointer;
  color: var(--color-text-secondary);
  font-size: 13px;
  vertical-align: middle;
}
.rates-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(280px, 1fr));
  gap: 12px 20px;
}
.rate-item {
  display: flex;
  align-items: center;
  gap: 6px;
}
.rate-label {
  color: var(--color-text-secondary);
  font-size: 13px;
  white-space: nowrap;
  cursor: help;
  width: 70px;
  text-align: right;
  flex-shrink: 0;
}
.rate-input {
  width: 130px;
  flex-shrink: 0;
}
.fallback-note {
  background: #f6f8ff;
  border: 1px dashed #a5c0f7;
  color: #4a7bd9;
  font-size: 12px;
  padding: 8px 12px;
  border-radius: 6px;
  margin: 8px 0 0;
}
.sub-title {
  font-size: 13px;
  font-weight: 600;
  margin: 14px 0 8px;
  display: flex;
  align-items: center;
}
.form-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 12px;
}
.label {
  color: var(--color-text-secondary);
  font-size: 13px;
  margin-right: 4px;
}
.divider {
  width: 1px;
  height: 20px;
  background: #e8e8e8;
  margin: 0 12px;
}
.inline-actions {
  margin-top: 8px;
}
.override-block {
  border: 1px solid #f0f0f0;
  border-radius: 8px;
  padding: 12px;
  margin-bottom: 12px;
}
.override-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 10px;
  flex-wrap: wrap;
}
.override-table {
  margin-bottom: 8px;
}
</style>