<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { listDevTokens, toggleDevToken, rotateDevToken, type DevToken } from '@/api/dev'
import StatusTag from '@/components/StatusTag.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'

const route = useRoute()
const router = useRouter()
const tokenId = String(route.params.id)

const loading = ref(false)
const token = ref<DevToken | null>(null)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

// 轮换新明文（一次展示）
const rotateDialog = ref(false)
const rotatePlain = ref('')
const rotateDisplay = ref('')

// 复制轮换明文
async function copyPlain() {
  await navigator.clipboard.writeText(rotatePlain.value)
  ElMessage.success('已复制')
}

const statusDesc = computed(() => token.value?.Status || '')

// 开发端未提供单条查询，复用列表按 id 定位
async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await listDevTokens(1, 500)
    token.value = res.data.list.find((t) => t.ID === tokenId) || null
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
onMounted(load)

async function onToggle() {
  if (!token.value) return
  try {
    const res = await toggleDevToken(token.value.ID)
    token.value.Status = res.data.Status
    ElMessage.success(res.data.Status === 'ACTIVE' ? '已启用' : '已禁用')
  } catch (e: any) {
    ElMessage.error(e?.message || '操作失败')
  }
}

async function onRotate() {
  if (!token.value) return
  try {
    const res = await rotateDevToken(token.value.ID)
    rotatePlain.value = res.data.Plain
    rotateDisplay.value = res.data.Display
    rotateDialog.value = true
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '轮换失败')
  }
}
</script>

<template>
  <div class="page-container">
    <div v-loading="loading" class="card" style="max-width: 720px">
      <el-empty v-if="!loading && !token" description="令牌不存在" />
      <template v-else-if="token">
        <div class="switch-row">
          <span>启用状态</span>
          <el-switch
            :model-value="token.Status === 'ACTIVE'"
            inline-prompt
            active-text="启"
            inactive-text="禁"
            @change="onToggle"
          />
        </div>
        <el-descriptions :column="1" border>
          <el-descriptions-item label="名称">{{ token.DisplayName }}</el-descriptions-item>
          <el-descriptions-item label="标识">
            <span class="mono">{{ token.TokenDisplay }}</span>
          </el-descriptions-item>
          <el-descriptions-item label="路由">
            <el-tag v-if="!token.TagID" size="small" type="info" effect="plain">默认路由</el-tag>
            <el-tag v-else size="small" effect="plain">已绑定标签</el-tag>
          </el-descriptions-item>
          <el-descriptions-item label="状态"><StatusTag :value="statusDesc" /></el-descriptions-item>
          <el-descriptions-item label="创建时间">{{ token.CreatedAt }}</el-descriptions-item>
          <el-descriptions-item label="最近使用">{{ token.LastUsedAt || '-' }}</el-descriptions-item>
          <el-descriptions-item label="过期时间">{{ token.ExpiresAt || '永不过期' }}</el-descriptions-item>
        </el-descriptions>

        <div class="ops">
          <el-button type="warning" @click="onRotate">轮换令牌</el-button>
        </div>
      </template>
      <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
    </div>

    <el-dialog v-model="rotateDialog" title="令牌已轮换" width="480px" :close-on-click-modal="false">
      <div class="plain-warning">新令牌仅展示一次，请立即复制保存。</div>
      <el-input :model-value="rotatePlain" readonly class="mono">
        <template #append>
          <el-button text @click="copyPlain">复制</el-button>
        </template>
      </el-input>
      <template #footer>
        <el-button type="primary" @click="router.push('/dev/tokens')">完成</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.mono {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
  font-size: 12px;
}
.switch-row {
  display: flex;
  justify-content: space-between;
  align-items: center;
  background: var(--color-primary-light);
  border-radius: 6px;
  padding: 10px 14px;
  margin-bottom: 16px;
}
.ops {
  margin-top: 16px;
}
.plain-warning {
  color: var(--color-warning);
  font-size: 13px;
  margin-bottom: 12px;
}
</style>