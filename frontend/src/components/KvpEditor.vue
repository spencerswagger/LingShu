<script setup lang="ts">
import { ref, watch } from 'vue'
import { Plus, Delete } from '@element-plus/icons-vue'

// 键值对行内编辑器（非弹窗）：以 Record<string,string> 作为 v-model，
// 内部转为行数组以便行内增删编辑。空的 key 行在提交时自动过滤。
const props = defineProps<{
  modelValue?: Record<string, string>
  keyPlaceholder?: string
  valuePlaceholder?: string
}>()
const emit = defineEmits<{
  (e: 'update:modelValue', v: Record<string, string>): void
}>()

// 内部行；0 表示占位空行（仅当存在且 key/value 均空时不提交）
const rows = ref<Array<{ key: string; value: string }>>([])

function fromMap() {
  const map = props.modelValue || {}
  rows.value = Object.entries(map).map(([k, v]) => ({ key: k, value: String(v) }))
  if (rows.value.length === 0) rows.value.push({ key: '', value: '' })
}

watch(() => props.modelValue, fromMap, { immediate: true })

function addRow() {
  rows.value.push({ key: '', value: '' })
}
function removeRow(index: number) {
  rows.value.splice(index, 1)
  emitChange()
}
function emitChange() {
  const map: Record<string, string> = {}
  for (const r of rows.value) {
    const k = r.key.trim()
    if (k) map[k] = r.value
  }
  emit('update:modelValue', map)
}
</script>

<template>
  <div class="kvp-editor">
    <div v-for="(row, i) in rows" :key="i" class="kvp-row">
      <el-input
        :model-value="row.key"
        placeholder="键"
        size="small"
        :class="{ 'kvp-key': true }"
        :input-style="{ fontFamily: 'monospace' }"
        @update:model-value="row.key = $event; emitChange()"
      />
      <span class="kvp-eq">=</span>
      <el-input
        :model-value="row.value"
        :placeholder="valuePlaceholder || '值'"
        size="small"
        :input-style="{ fontFamily: 'monospace' }"
        @update:model-value="row.value = $event; emitChange()"
      />
      <el-button
        type="danger"
        text
        size="small"
        class="kvp-del"
        :icon="Delete"
        @click="removeRow(i)"
      />
    </div>
    <el-button size="small" :icon="Plus" class="kvp-add" @click="addRow">添加键值对</el-button>
  </div>
</template>

<style scoped>
.kvp-editor {
  display: flex;
  flex-direction: column;
  gap: 6px;
}
.kvp-row {
  display: flex;
  align-items: center;
  gap: 6px;
}
.kvp-key {
  width: 200px;
  flex: none;
}
.kvp-eq {
  color: var(--color-text-secondary);
  flex: none;
}
.kvp-del {
  flex: none;
  margin-left: 0;
}
.kvp-add {
  align-self: flex-start;
  margin-top: 2px;
}
</style>