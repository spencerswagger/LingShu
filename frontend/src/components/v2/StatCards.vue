<script setup lang="ts">
// 指标卡网格：V2 运营台各页顶部统一用它，保证「同一类数字长一个样」。
// value 一律由调用方格式化好再传（金额/百分比/token 的格式化各有口径，别在这里猜）。
// 类型定义放在 components/v2/types.ts（script setup 不能导出类型）。
import type { StatItem } from './types'

defineProps<{ items: StatItem[]; columns?: number }>()
</script>

<template>
  <div class="stat-grid" :style="{ '--cols': String(columns || 4) }">
    <div v-for="it in items" :key="it.label" class="stat" :class="`tone-${it.tone || 'default'}`">
      <div class="stat-label">{{ it.label }}</div>
      <div class="stat-value">{{ it.value }}</div>
      <div v-if="it.hint" class="stat-hint">{{ it.hint }}</div>
    </div>
  </div>
</template>

<style scoped>
.stat-grid {
  display: grid;
  grid-template-columns: repeat(var(--cols), minmax(0, 1fr));
  gap: 10px;
}
.stat {
  background: var(--color-surface);
  border: 1px solid var(--color-border);
  border-radius: var(--radius-m);
  box-shadow: var(--shadow-card);
  padding: 12px 14px;
  min-width: 0;
}
.stat-label {
  font-size: 12px;
  color: var(--color-text-secondary);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}
.stat-value {
  margin-top: 6px;
  font-size: 22px;
  font-weight: 600;
  line-height: 1.2;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
  word-break: break-all;
}
.stat-hint {
  margin-top: 4px;
  font-size: 11px;
  line-height: 1.5;
  color: var(--color-text-tertiary);
}
.tone-ok .stat-value {
  color: var(--color-success);
}
.tone-warn .stat-value {
  color: var(--color-warning);
}
.tone-danger .stat-value {
  color: var(--color-danger);
}

/* 移动端：单列（全站铁律 ≤640px 一律单列） */
@media (max-width: 900px) {
  .stat-grid {
    grid-template-columns: repeat(2, minmax(0, 1fr));
  }
}
@media (max-width: 640px) {
  .stat-grid {
    grid-template-columns: 1fr;
  }
  .stat-value {
    font-size: 20px;
  }
}
</style>
