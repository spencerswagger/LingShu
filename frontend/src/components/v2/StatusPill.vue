<script setup lang="ts">
// 状态标签：把后端枚举翻成中文 + 颜色，一处定义。
// 枚举取值域来自口径文档，前端**不新增取值**：
//   对账 status   ok / alarm / error / blocked / skipped
//   预扣 status   held / settled / refunded / expired
//   成本来源      ledger / amortized / none（NULL = 历史行，口径未知）
//   内部模型 kind group / direct
import { computed } from 'vue'

export type StatusDomain = 'reconcile' | 'hold' | 'costSource' | 'kind'

const props = defineProps<{ domain: StatusDomain; value?: string | null }>()

type Tone = 'success' | 'warning' | 'danger' | 'info'

interface Meta {
  label: string
  tone: Tone
  title: string
}

const RECONCILE: Record<string, Meta> = {
  ok: { label: '对账通过', tone: 'success', title: '本窗已拉完且偏差在阈值内' },
  alarm: { label: '偏差超阈', tone: 'warning', title: '已对账但偏差超阈值，连续 3 窗升级为回退判据' },
  error: { label: '拉取失败', tone: 'danger', title: '本窗没拉完（拉取或落库失败），不计对账结论' },
  blocked: { label: '待补拉', tone: 'warning', title: '单轮 4000 条预算用尽，本窗没拉完，offset 有待补进度' },
  skipped: { label: '无结论', tone: 'info', title: '账本关闭/不可用/本窗仍 blocked → 没有对账结论（不是「对上了」）' },
}

const HOLD: Record<string, Meta> = {
  held: { label: '预扣中', tone: 'warning', title: '已预扣、未结算：15 分钟内未结算由扫描器置 expired' },
  settled: { label: '已结算', tone: 'success', title: '已按实际用量结算' },
  refunded: { label: '已退预扣', tone: 'info', title: '失败或无 usage，整笔退回' },
  expired: { label: '已过期', tone: 'danger', title: '超时未结算，由过期扫描器终结' },
}

const COST_SOURCE: Record<string, Meta> = {
  ledger: { label: '账本实付', tone: 'success', title: '成本取自上游账本条目（变动成本）' },
  amortized: { label: '订阅摊销', tone: 'warning', title: '成本由订阅月费按窗口摊销（固定成本）' },
  none: { label: '无成本口径', tone: 'danger', title: '成本为空：账本没拉到或订阅没配 → 毛利此刻是虚高的' },
  // 空值不是 'none'：Spec P08/G2 要求「历史行（口径未记录）」与「明确无口径」文案与颜色都不相同，
  // 用 `|| 'none'` 兜底会把两种账务结论合成一种。
  '': { label: '历史未知', tone: 'info', title: 'cost_source 为空：这条账单没有成本口径记录，别当成「无成本口径」' },
}

const KIND: Record<string, Meta> = {
  group: { label: '组', tone: 'info', title: '内部模型是组（一次请求命中组内一个成员）' },
  direct: { label: '直通', tone: 'success', title: '内部模型是直通模型' },
}

const table = computed<Record<string, Meta>>(() => {
  if (props.domain === 'reconcile') return RECONCILE
  if (props.domain === 'hold') return HOLD
  if (props.domain === 'costSource') return COST_SOURCE
  return KIND
})

const meta = computed<Meta>(() => {
  const v = props.value == null ? '' : String(props.value)
  return table.value[v] || { label: v || '—', tone: 'info', title: v ? '未识别的取值' : '没有值' }
})
</script>

<template>
  <el-tag :type="meta.tone" size="small" effect="plain" :title="meta.title">{{ meta.label }}</el-tag>
</template>
