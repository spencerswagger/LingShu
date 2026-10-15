<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import {
  createChannelProtocol,
  getChannelProtocol,
  updateChannelProtocol,
  type ChannelProtocolInput,
  type ProtocolOption,
} from '@/api/admin'
import KvpEditor from '@/components/KvpEditor.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'

const route = useRoute()
const router = useRouter()
const id = route.params.id ? String(route.params.id) : ''
const isEdit = computed(() => !!id)

const loading = ref(false)
const saving = ref(false)
const activeTab = ref('base')
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

// 三个代码固定的基础协议
const baseProtocols = ['openai-chat', 'openai-responses', 'anthropic-messages'] as const
const protocolLabels: Record<string, string> = {
  'openai-chat': 'OpenAI Chat',
  'openai-responses': 'OpenAI Responses',
  'anthropic-messages': 'Anthropic Messages',
}

type ProtoState = { enabled: boolean; path: string; baseUrl: string }
const protocols = reactive<Record<string, ProtoState>>({
  'openai-chat': { enabled: false, path: '', baseUrl: '' },
  'openai-responses': { enabled: false, path: '', baseUrl: '' },
  'anthropic-messages': { enabled: false, path: '', baseUrl: '' },
})
// 首选协议（default）：同一作用域内至多一个，用单选下拉保证互斥
const defaultProtocol = ref<string>('')

const form = reactive({
  name: '',
  base_url: '',
  auth_header: 'Authorization',
  auth_scheme: 'bearer',
  auth_query: {} as Record<string, string>,
  passthrough_mode: 'none',
  whitelist: [] as string[],
  blacklist: [] as string[],
  set: {} as Record<string, string>,
  models_text: '{}',
})

const enabledProtocols = computed(() => baseProtocols.filter((p) => protocols[p].enabled))

// 来源预设 ID（只读回显）
const presetSourceId = ref('')

// 建议透传的白名单候选（来自预设包格式约定）
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
  if (!isEdit.value) return
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await getChannelProtocol(id)
    const v = res.Data
    form.name = v.Name
    form.base_url = v.BaseURL || ''
    presetSourceId.value = v.PresetSourceID || ''
    for (const p of baseProtocols) {
      const opt = v.Protocols?.[p]
      protocols[p].enabled = !!opt
      protocols[p].path = opt?.path || ''
      protocols[p].baseUrl = opt?.baseUrl || ''
      if (opt?.default) defaultProtocol.value = p
    }
    form.auth_header = v.Auth?.header || 'Authorization'
    form.auth_scheme = v.Auth?.scheme || 'bearer'
    form.auth_query = v.Auth?.query || {}
    form.passthrough_mode = v.RequestHeaders?.passthroughMode || 'none'
    form.whitelist = v.RequestHeaders?.whitelist || []
    form.blacklist = v.RequestHeaders?.blacklist || []
    form.set = v.RequestHeaders?.set || {}
    form.models_text = JSON.stringify(v.Models ?? {}, null, 2)
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
onMounted(load)

async function onSubmit() {
  if (!form.name.trim()) return ElMessage.warning('请填写名称')
  if (form.base_url.trim() && !/^https?:\/\//.test(form.base_url.trim()))
    return ElMessage.warning('BaseURL 必须以 http(s):// 开头')
  if (!enabledProtocols.value.length) return ElMessage.warning('请至少启用一个基础协议')

  // models 以 JSON 文本域录入，提交前解析校验（必须为 JSON 对象）
  let models: Record<string, unknown>
  try {
    const parsed = JSON.parse(form.models_text || '{}')
    if (parsed === null || typeof parsed !== 'object' || Array.isArray(parsed)) {
      return ElMessage.warning('模型清单必须为 JSON 对象')
    }
    models = parsed as Record<string, unknown>
  } catch {
    return ElMessage.warning('模型清单 JSON 格式错误')
  }

  const protoOut: Record<string, ProtocolOption> = {}
  for (const p of baseProtocols) {
    if (!protocols[p].enabled) continue
    const opt: ProtocolOption = {}
    if (protocols[p].path.trim()) opt.path = protocols[p].path.trim()
    if (protocols[p].baseUrl.trim()) opt.baseUrl = protocols[p].baseUrl.trim()
    if (defaultProtocol.value === p) opt.default = true
    protoOut[p] = opt
  }

  const payload: ChannelProtocolInput = {
    Name: form.name.trim(),
    BaseURL: form.base_url.trim(),
    Protocols: protoOut,
    Auth: {
      header: form.auth_header,
      scheme: form.auth_scheme,
      query: form.auth_query,
    },
    RequestHeaders: {
      passthroughMode: form.passthrough_mode,
      whitelist: form.whitelist,
      blacklist: form.blacklist,
      set: form.set,
    },
    Models: models,
  }

  saving.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    if (isEdit.value) {
      await updateChannelProtocol(id, payload)
      ElMessage.success('渠道协议已更新')
      await load()
    } else {
      await createChannelProtocol(payload)
      ElMessage.success('渠道协议已创建')
      router.push('/admin/channel-protocols')
    }
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    saving.value = false
  }
}
</script>

<template>
  <div class="page-container">
    <el-tabs v-model="activeTab" class="edit-tabs">
      <el-tab-pane label="基本信息" name="base">
        <div v-loading="loading" class="card">
          <el-form label-width="130px" class="form-grid">
            <el-form-item label="名称" required>
              <el-input v-model="form.name" maxlength="64" placeholder="如 OpenCode Zen" />
            </el-form-item>
            <el-form-item label="来源预设 ID">
              <el-input :model-value="presetSourceId || '—'" disabled placeholder="—" />
              <div class="hint">来源预设 ID 由同步流程写入；手动新建时为空</div>
            </el-form-item>
            <el-form-item class="span-2" label="Base URL">
              <el-input v-model="form.base_url" placeholder="https://api.example.com/v1" />
              <div class="hint">渠道协议级默认上游地址；各协议可单独覆盖</div>
            </el-form-item>
          </el-form>
        </div>
      </el-tab-pane>

      <el-tab-pane label="协议配置" name="protocols">
        <div class="card">
          <div class="block-desc">选择该协议模板原生支持的上游基础协议；键存在 = 支持。可逐协议覆盖路径与上游地址。</div>
          <div v-for="p in baseProtocols" :key="p" class="proto-row">
            <el-checkbox v-model="protocols[p].enabled" class="proto-check">{{ protocolLabels[p] }}</el-checkbox>
            <div class="proto-fields">
              <el-input v-model="protocols[p].path" size="small" placeholder="路径，留空用内置默认（如 /chat/completions）" :disabled="!protocols[p].enabled" />
              <el-input v-model="protocols[p].baseUrl" size="small" placeholder="该协议专属 BaseURL，留空回退协议级" :disabled="!protocols[p].enabled" />
            </div>
          </div>
          <el-form label-width="130px" class="init-form">
            <el-form-item label="首选协议">
              <el-select v-model="defaultProtocol" clearable placeholder="不指定" style="width: 320px">
                <el-option v-for="p in enabledProtocols" :key="p" :label="protocolLabels[p]" :value="p" />
              </el-select>
              <div class="hint">标记 default，同一作用域内至多一个；用于二期跨协议转换的目标协议</div>
            </el-form-item>
          </el-form>
        </div>
      </el-tab-pane>

      <el-tab-pane label="鉴权" name="auth">
        <div class="card">
          <div class="block-desc">凭据仍存于渠道密钥（channel_keys）；此处仅描述鉴权注入方式。</div>
          <el-form label-width="130px" class="form-grid">
            <el-form-item label="Header">
              <el-select v-model="form.auth_header" style="width: 100%">
                <el-option label="Authorization" value="Authorization" />
                <el-option label="x-api-key" value="x-api-key" />
                <el-option label="api-key" value="api-key" />
              </el-select>
            </el-form-item>
            <el-form-item label="Scheme">
              <el-select v-model="form.auth_scheme" style="width: 100%">
                <el-option label="bearer" value="bearer" />
                <el-option label="raw" value="raw" />
              </el-select>
              <div class="hint">bearer = 注入 `Bearer &lt;凭据&gt;`；raw = 原样注入凭据</div>
            </el-form-item>
            <el-form-item class="span-2" label="Query 参数">
              <KvpEditor v-model="form.auth_query" key-placeholder="参数名" value-placeholder="参数值（如 api-version）" />
              <div class="hint">需放 querystring 的鉴权 / 版本参数（如 Azure api-version）</div>
            </el-form-item>
          </el-form>
        </div>
      </el-tab-pane>

      <el-tab-pane label="请求头策略" name="headers">
        <div class="card">
          <div class="block-desc">控制下游请求头如何透传到上游；鉴权头由「鉴权」注入，不应写入 set。</div>
          <el-form label-width="130px" class="form-grid">
            <el-form-item label="透传模式">
              <el-select v-model="form.passthrough_mode" style="width: 100%">
                <el-option label="none（不透传）" value="none" />
                <el-option label="whitelist（白名单）" value="whitelist" />
                <el-option label="blacklist（黑名单）" value="blacklist" />
                <el-option label="all（全部透传）" value="all" />
              </el-select>
            </el-form-item>
            <el-form-item label="白名单">
              <el-select
                v-model="form.whitelist"
                multiple
                filterable
                allow-create
                default-first-option
                style="width: 100%"
                placeholder="仅这些下游头透传"
                :disabled="form.passthrough_mode !== 'whitelist'"
              >
                <el-option v-for="h in headerSuggestions" :key="h" :label="h" :value="h" />
              </el-select>
            </el-form-item>
            <el-form-item label="黑名单">
              <el-select
                v-model="form.blacklist"
                multiple
                filterable
                allow-create
                default-first-option
                style="width: 100%"
                placeholder="除这些下游头外透传"
                :disabled="form.passthrough_mode !== 'blacklist'"
              >
                <el-option v-for="h in headerSuggestions" :key="h" :label="h" :value="h" />
              </el-select>
            </el-form-item>
            <el-form-item class="span-2" label="Set 覆盖">
              <KvpEditor v-model="form.set" key-placeholder="请求头名" value-placeholder="值（支持变量）" />
              <div class="hint">覆盖语义（非合并）；支持变量 {api_key} / {session_id} / {request_id} / {model} / {client_header:NAME}</div>
            </el-form-item>
          </el-form>
        </div>
      </el-tab-pane>

      <el-tab-pane label="模型清单" name="models">
        <div class="card">
          <div class="block-desc">模型清单以 JSON 对象录入（对齐预设包 models 字段）；提交时解析校验。</div>
          <el-input v-model="form.models_text" type="textarea" :rows="16" class="code-area" />
        </div>
      </el-tab-pane>
    </el-tabs>

    <div class="actions">
      <el-button type="primary" :loading="saving" @click="onSubmit">{{ isEdit ? '保存' : '创建' }}</el-button>
      <el-button @click="router.push('/admin/channel-protocols')">取消</el-button>
      <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
    </div>
  </div>
</template>

<style scoped>
.edit-tabs {
  width: 100%;
}
.edit-tabs :deep(.el-tabs__header) {
  margin-bottom: 18px;
}
.form-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 0 28px;
}
.form-grid .span-2 {
  grid-column: 1 / -1;
}
@media (max-width: 900px) {
  .form-grid {
    grid-template-columns: 1fr;
  }
}
.hint {
  margin-top: 4px;
  font-size: 12px;
  line-height: 1.5;
  color: var(--color-text-secondary);
}
.block-desc {
  color: var(--color-text-secondary);
  font-size: 13px;
  margin-bottom: 14px;
}
.proto-row {
  display: flex;
  align-items: flex-start;
  gap: 16px;
  padding: 10px 0;
  border-bottom: 1px solid var(--color-border);
}
.proto-check {
  width: 160px;
  flex: none;
  margin-top: 4px;
}
.proto-fields {
  flex: 1;
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.init-form {
  margin-top: 16px;
}
.actions {
  margin-top: 20px;
  display: flex;
  align-items: center;
  gap: 8px;
}
.code-area :deep(textarea) {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
  font-size: 12px;
}
</style>
