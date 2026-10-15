<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { ElMessage } from 'element-plus'
import { QuestionFilled } from '@element-plus/icons-vue'
import { getGatewayConfig, putGatewayConfig } from '@/api/admin'
import ErrorBubble from '@/components/ErrorBubble.vue'

const loading = ref(false)
const saving = ref(false)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

// 全局上游响应头白名单（sys_configs 键 gateway.upstream_response_header_whitelist）
const whitelist = ref<string[]>([])

// 建议透传的响应头候选（与预设包格式约定保持一致）
const headerSuggestions = [
  'anthropic-version',
  'anthropic-beta',
  'OpenAI-Organization',
  'OpenAI-Project',
  'OpenAI-Beta',
  'traceparent',
  'tracestate',
  'x-request-id',
  'x-session-id',
  'x-client-request-id',
  'x-opencode-session',
]

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await getGatewayConfig()
    whitelist.value = res.Data?.UpstreamResponseHeaderWhitelist || []
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
onMounted(load)

async function onSave() {
  saving.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    await putGatewayConfig({ UpstreamResponseHeaderWhitelist: whitelist.value })
    ElMessage.success('网关设置已保存')
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">网关设置</div>
      <el-button type="primary" :loading="saving" @click="onSave">保存</el-button>
    </div>

    <div v-loading="loading" class="card config-card">
      <div class="config-block">
        <div class="block-title">
          全局上游响应头白名单
          <el-tooltip
            content="上游响应头默认不透传（维持渠道透明）。仅白名单中的头会被透传给客户端，用于排查链路。"
            placement="top"
          >
            <el-icon class="hint-icon"><QuestionFilled /></el-icon>
          </el-tooltip>
        </div>
        <div class="block-desc">
          上游响应头默认不透传给客户端。在此列出需要透传的响应头名称后，网关才会把对应头原样回传（全局生效，对所有渠道一致）。
        </div>

        <el-form label-width="130px">
          <el-form-item label="白名单头">
            <el-select
              v-model="whitelist"
              multiple
              filterable
              allow-create
              default-first-option
              style="width: 100%"
              placeholder="输入或选择需透传的上游响应头名（留空 = 不透传任何上游响应头）"
            >
              <el-option v-for="h in headerSuggestions" :key="h" :label="h" :value="h" />
            </el-select>
            <div class="hint">支持直接输入自定义头名回车添加；候选项为常见可透传头。</div>
          </el-form-item>
        </el-form>
      </div>

      <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
    </div>
  </div>
</template>

<style scoped>
.config-card {
  padding: 20px 24px;
}
.config-block {
  margin-bottom: 4px;
}
.block-desc {
  color: var(--color-text-secondary);
  font-size: 13px;
  margin-bottom: 14px;
  line-height: 1.6;
}
.hint {
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.5;
  color: var(--color-text-secondary);
}
.hint-icon {
  cursor: pointer;
  color: var(--color-text-secondary);
}
</style>
