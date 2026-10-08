<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ArrowDown } from '@element-plus/icons-vue'
import { listDevAnnouncements, type DevAnnouncement } from '@/api/dev'
import ErrorBubble from '@/components/ErrorBubble.vue'

const loading = ref(false)
const list = ref<DevAnnouncement[]>([])
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

// 展开内容
const expanded = ref<Record<string, boolean>>({})

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await listDevAnnouncements()
    list.value = res.Data.List
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
onMounted(load)

function toggle(id: string) {
  expanded.value[id] = !expanded.value[id]
}
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">系统公告</div>
      <el-button :loading="loading" @click="load">刷新</el-button>
    </div>

    <div v-loading="loading" class="card">
      <template v-if="list.length">
        <div v-for="a in list" :key="a.ID" class="ann-item">
          <div class="ann-head" @click="toggle(a.ID)">
            <el-tag :type="a.Level === 'danger' ? 'danger' : a.Level === 'warning' ? 'warning' : 'primary'" size="small" effect="light">
              {{ a.Level }}
            </el-tag>
            <span class="ann-title">{{ a.Title }}</span>
            <span class="ann-date">{{ a.PublishAt ? a.PublishAt.slice(0, 10) : '' }}</span>
            <el-icon class="ann-arrow" :class="{ open: expanded[a.ID] }"><ArrowDown /></el-icon>
          </div>
          <div v-if="expanded[a.ID]" class="ann-body">{{ a.Content }}</div>
        </div>
      </template>
      <el-empty v-else-if="!loading" description="暂无公告" />
      <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
    </div>
  </div>
</template>

<style scoped>
.ann-item {
  border-bottom: 1px solid #f2f2f2;
  padding: 4px 0;
}
.ann-head {
  display: flex;
  align-items: center;
  gap: 8px;
  padding: 12px 0;
  cursor: pointer;
}
.ann-title {
  flex: 1;
  font-weight: 500;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.ann-date {
  color: var(--color-text-secondary);
  font-size: 12px;
}
.ann-arrow {
  transition: transform 0.2s;
  color: var(--color-text-secondary);
}
.ann-arrow.open {
  transform: rotate(180deg);
}
.ann-body {
  padding: 4px 0 14px 0;
  color: #555;
  line-height: 1.7;
  white-space: pre-wrap;
}
</style>