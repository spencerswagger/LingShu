<script setup lang="ts">
import { ref, watch } from 'vue'
import { Plus, Delete, ArrowDown } from '@element-plus/icons-vue'

// 键值对行内编辑器（非弹窗）：以 Record<string,string> 作为 v-model，
// 内部转为行数组以便行内增删编辑。空的 key 行在提交时自动过滤。
// keySuggestions / valueSuggestions 作为下拉候选：键可选项 + 值（变量）可插入项，用户仍可自行输入。
const props = defineProps<{
  modelValue?: Record<string, string>
  keyPlaceholder?: string
  valuePlaceholder?: string
  keySuggestions?: string[]
  valueSuggestions?: string[]
}>()
const emit = defineEmits<{
  (e: 'update:modelValue', v: Record<string, string>): void
}>()

// 内部行；0 表示占位空行（仅当存在且 key/value 均空时不提交）
const rows = ref<Array<{ key: string; value: string }>>([])
// synced 记录最近一次本地提交的映射：父组件回写一致时不重建行，
// 避免「正在输入的空行 / 尚未填写 key 的新行」被回写覆盖而丢失（与 StringListEditor 同思路）。
let synced: Record<string, string> = {}
let initialized = false

function sameMap(a: Record<string, string>, b: Record<string, string>): boolean {
  const ka = Object.keys(a)
  const kb = Object.keys(b)
  if (ka.length !== kb.length) return false
  return ka.every((k) => b[k] === a[k])
}

function fromMap() {
  const map = props.modelValue || {}
  if (initialized && sameMap(map, synced)) return
  rows.value = Object.entries(map).map(([k, v]) => ({ key: k, value: String(v) }))
  if (rows.value.length === 0) rows.value.push({ key: '', value: '' })
  initialized = true
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
  synced = map
  emit('update:modelValue', map)
}
// 变量插入：把所选变量追加到当前值末尾
function insertValue(index: number, token: string) {
  const r = rows.value[index]
  r.value = r.value ? r.value + token : token
  emitChange()
}
</script>

<template>
  <div class="kvp-editor">
    <div v-for="(row, i) in rows" :key="i" class="kvp-row">
      <el-select
        v-if="keySuggestions && keySuggestions.length"
        :model-value="row.key"
        filterable
        allow-create
        default-first-option
        clearable
        class="kvp-key"
        :placeholder="keyPlaceholder || '键'"
        @update:model-value="(v: string) => { row.key = v || ''; emitChange() }"
      >
        <el-option v-for="s in keySuggestions" :key="s" :label="s" :value="s" />
      </el-select>
      <el-input
        v-else
        :model-value="row.key"
        :placeholder="keyPlaceholder || '键'"
        class="kvp-key"
        :input-style="{ fontFamily: 'monospace' }"
        @update:model-value="row.key = $event; emitChange()"
      />
      <span class="kvp-eq">=</span>
      <el-input
        :model-value="row.value"
        :placeholder="valuePlaceholder || '值'"
        :input-style="{ fontFamily: 'monospace' }"
        @update:model-value="row.value = $event; emitChange()"
      >
        <template v-if="valueSuggestions && valueSuggestions.length" #append>
          <el-dropdown trigger="click" @command="(v: string) => insertValue(i, v)">
            <el-button :icon="ArrowDown" />
            <template #dropdown>
              <el-dropdown-menu>
                <el-dropdown-item v-for="v in valueSuggestions" :key="v" :command="v">{{ v }}</el-dropdown-item>
              </el-dropdown-menu>
            </template>
          </el-dropdown>
        </template>
      </el-input>
      <el-button
        type="danger"
        text
        class="kvp-del"
        :icon="Delete"
        @click="removeRow(i)"
      />
    </div>
    <el-button :icon="Plus" class="kvp-add" @click="addRow">添加键值对</el-button>
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
