<script setup lang="ts">
// Auto 成员编辑器（全局/私有 Auto 共用）：
// 有序成员列表，顺序即优先级（数组下标 = position）。从「启用的对外模型」中选择，禁止重复。
import { computed } from 'vue'
import { Top, Bottom, Delete, Plus } from '@element-plus/icons-vue'

// 候选对外模型（仅需 ID 与展示名，便于回显已停用成员）
interface AutoMemberCandidate {
  ID: string
  ExternalName: string
}
// 成员行：仅承载对外模型选择，顺序即优先级，提交时 Position 取数组下标
type AutoMemberRow = { ExternalModelID: string }

const props = defineProps<{
  modelValue: AutoMemberRow[]
  candidates: AutoMemberCandidate[]
}>()
const emit = defineEmits<{ (e: 'update:modelValue', v: AutoMemberRow[]): void }>()

const rows = computed(() => props.modelValue || [])

// 已被其它行占用的对外模型 ID（选中项可选但不可重复选择）
const usedIds = computed(() => new Set(rows.value.map((r) => String(r.ExternalModelID)).filter(Boolean)))

function candidateOptions(currentId: string) {
  return props.candidates.filter((m) => String(m.ID) === currentId || !usedIds.value.has(String(m.ID)))
}

function update(next: AutoMemberRow[]) {
  emit('update:modelValue', next)
}

function onSelect(index: number, id: string) {
  const next = rows.value.map((r) => ({ ...r }))
  next[index].ExternalModelID = id
  update(next)
}

function onAdd() {
  update([...rows.value.map((r) => ({ ...r })), { ExternalModelID: '' }])
}

function onRemove(index: number) {
  const next = rows.value.map((r) => ({ ...r }))
  next.splice(index, 1)
  update(next)
}

// 上移/下移：交换相邻项，顺序变化即优先级变化
function onMove(index: number, delta: number) {
  const j = index + delta
  if (j < 0 || j >= rows.value.length) return
  const next = rows.value.map((r) => ({ ...r }))
  ;[next[index], next[j]] = [next[j], next[index]]
  update(next)
}
</script>

<template>
  <div class="am-members">
    <el-table :data="rows" border size="small" class="table-nowrap">
      <el-table-column label="顺序" width="60" align="center">
        <template #default="{ $index }">{{ $index + 1 }}</template>
      </el-table-column>
      <el-table-column label="对外模型" min-width="220">
        <template #default="{ row, $index }">
          <el-select
            :model-value="row.ExternalModelID || null"
            filterable
            placeholder="选择对外模型"
            style="width: 100%"
            @update:model-value="(v: string) => onSelect($index, v)"
          >
            <el-option
              v-for="m in candidateOptions(row.ExternalModelID)"
              :key="m.ID"
              :label="m.ExternalName"
              :value="String(m.ID)"
            />
          </el-select>
        </template>
      </el-table-column>
      <el-table-column label="操作" width="120" align="right">
        <template #default="{ $index }">
          <div class="op-cell">
            <el-tooltip content="上移" placement="top">
              <el-icon
                class="op-icon"
                :class="{ disabled: $index === 0 }"
                @click="onMove($index, -1)"
              >
                <Top />
              </el-icon>
            </el-tooltip>
            <el-tooltip content="下移" placement="top">
              <el-icon
                class="op-icon"
                :class="{ disabled: $index === rows.length - 1 }"
                @click="onMove($index, 1)"
              >
                <Bottom />
              </el-icon>
            </el-tooltip>
            <el-tooltip content="移除" placement="top">
              <el-icon class="op-icon danger" @click="onRemove($index)"><Delete /></el-icon>
            </el-tooltip>
          </div>
        </template>
      </el-table-column>
    </el-table>

    <div class="am-foot">
      <el-button size="small" :icon="Plus" @click="onAdd">添加成员</el-button>
      <span class="am-hint">按从上到下的顺序选择成员，越靠前优先级越高。</span>
    </div>

    <el-empty v-if="!rows.length" description="暂无成员，请至少添加一个" :image-size="60" />
  </div>
</template>

<style scoped>
.am-foot {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-top: 10px;
}
.am-hint {
  font-size: 12px;
  color: var(--color-text-tertiary);
}
.op-icon.disabled {
  opacity: 0.35;
  pointer-events: none;
}
</style>