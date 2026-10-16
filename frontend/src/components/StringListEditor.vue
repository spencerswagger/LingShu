<script setup lang="ts">
import { ref, watch } from 'vue'
import { Plus, Delete } from '@element-plus/icons-vue'

// 字符串列表行内编辑器（非弹窗）：以 string[] 作为 v-model。
// 每行一个值；suggestions 仅作下拉候选提示，用户可自行输入任意值。空行在提交时自动过滤。
const props = defineProps<{
  modelValue?: string[]
  suggestions?: string[]
  placeholder?: string
}>()
const emit = defineEmits<{
  (e: 'update:modelValue', v: string[]): void
}>()

const rows = ref<string[]>([])
let synced: string[] = []
let initialized = false

function sameList(a: string[], b: string[]): boolean {
  return a.length === b.length && a.every((v, i) => v === b[i])
}

function fromList() {
  const next = (props.modelValue || []).map((s) => String(s))
  // 与本地已提交值一致时不重建（避免正在输入的空行被覆盖）
  if (initialized && sameList(next, synced)) return
  rows.value = next.length ? next : ['']
  initialized = true
}

watch(() => props.modelValue, fromList, { immediate: true })

function emitChange() {
  const list = rows.value.map((s) => s.trim()).filter((s) => s)
  synced = list
  emit('update:modelValue', list)
}
function addRow() {
  rows.value.push('')
}
function removeRow(index: number) {
  rows.value.splice(index, 1)
  if (rows.value.length === 0) rows.value.push('')
  emitChange()
}
</script>

<template>
  <div class="sle-editor">
    <div v-for="(row, i) in rows" :key="i" class="sle-row">
      <el-select
        :model-value="row"
        filterable
        allow-create
        default-first-option
        clearable
        class="sle-input"
        :placeholder="placeholder || '输入或选择'"
        @update:model-value="(v: string) => { rows[i] = v || ''; emitChange() }"
      >
        <el-option v-for="s in suggestions" :key="s" :label="s" :value="s" />
      </el-select>
      <el-button type="danger" text class="sle-del" :icon="Delete" @click="removeRow(i)" />
    </div>
    <el-button :icon="Plus" class="sle-add" @click="addRow">添加一项</el-button>
  </div>
</template>

<style scoped>
.sle-editor {
  display: flex;
  flex-direction: column;
  gap: 6px;
  width: 100%;
}
.sle-row {
  display: flex;
  align-items: center;
  gap: 6px;
}
.sle-input {
  width: 280px;
  flex: none;
}
.sle-del {
  flex: none;
  margin-left: 0;
}
.sle-add {
  align-self: flex-start;
  margin-top: 2px;
}
</style>
