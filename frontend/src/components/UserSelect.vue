<script setup lang="ts">
// 用户选择器：远程按用户名搜索，对外只暴露 user_id（无业务意义 ID 不对用户展示）。
// 支持通过已有 user_id 回显用户名（表单回填/跳转带参场景）。
import { onMounted, ref, watch } from 'vue'
import { listUsers, getUser, type AdminUser } from '@/api/admin'

const props = defineProps<{
  modelValue: string | null | undefined
  placeholder?: string
  clearable?: boolean
}>()
const emit = defineEmits<{ (e: 'update:modelValue', v: string | null): void }>()

const loading = ref(false)
const options = ref<AdminUser[]>([])
const kw = ref('')

// 当前已选项（用于回显用户名）。选中的候选项或经 getUser 查回的项。
const selected = ref<AdminUser | null>(null)

async function search(q: string) {
  loading.value = true
  try {
    const res = await listUsers({ Q: q, Page: 1, Size: 50 })
    options.value = res.Data.List || []
  } catch {
    options.value = []
  } finally {
    loading.value = false
  }
}

function onChange(v: string | null | undefined) {
  const hit = options.value.find((u) => u.ID === v) || null
  selected.value = hit
  // 保留用户名到输入框便于展示
  if (hit) kw.value = hit.Username
  emit('update:modelValue', v ?? null)
}

// 外部给到 id（编辑回填/跳转筛选）时，若选项里没有，按 id 查回用户名展示
watch(
  () => props.modelValue,
  async (v) => {
    if (v == null || selected.value?.ID === v) return
    if (options.value.some((u) => u.ID === v)) {
      selected.value = options.value.find((u) => u.ID === v) || null
      return
    }
    try {
      const res = await getUser(v)
      const u = res.Data?.User
      if (u) {
        selected.value = u
        kw.value = u.Username
        if (!options.value.some((x) => x.ID === u.ID)) options.value.unshift(u)
      }
    } catch {
      /* 保持现状 */
    }
  },
  { immediate: true },
)

onMounted(() => search(''))
</script>

<template>
  <el-select
    :model-value="props.modelValue ?? null"
    filterable
    remote
    clearable
    :remote-method="search"
    :loading="loading"
    :placeholder="placeholder || '选择用户'"
    :class="$attrs.class"
    style="width: 100%"
    @update:model-value="onChange"
    @clear="onChange(null)"
  >
    <el-option
      v-for="u in options"
      :key="u.ID"
      :value="u.ID"
      :label="u.Nickname || u.Username"
    >
      <div class="user-opt">
        <span class="user-opt-name">{{ u.Nickname || u.Username }}</span>
        <span class="user-opt-sub">@{{ u.Username }}</span>
      </div>
    </el-option>
  </el-select>
</template>

<style>
.user-opt {
  display: flex;
  flex-direction: column;
  line-height: 1.35;
  padding: 2px 0;
}
.user-opt-name {
  font-size: 13px;
  color: var(--color-text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.user-opt-sub {
  font-size: 12px;
  color: var(--color-text-tertiary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
</style>