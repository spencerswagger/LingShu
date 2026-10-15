<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { Plus } from '@element-plus/icons-vue'
import {
  createChannelProtocol,
  getChannelProtocol,
  updateChannelProtocol,
  type ChannelProtocolInput,
  type ChannelProtocolModelOverride,
  type ProtocolOption,
} from '@/api/admin'
import KvpEditor from '@/components/KvpEditor.vue'
import StringListEditor from '@/components/StringListEditor.vue'
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
  passthrough_mode: 'none',
  whitelist: [] as string[],
  blacklist: [] as string[],
  set: {} as Record<string, string>,
})

// 模型清单：每行一个内部模型，可覆盖可达协议子集与 BaseURL（留空 = 继承渠道协议级）
type ModelRow = { id: string; protocols: string[]; baseUrl: string }
const modelRows = ref<ModelRow[]>([])

const enabledProtocols = computed(() => baseProtocols.filter((p) => protocols[p].enabled))

// 来源预设 ID（只读回显）
const presetSourceId = ref('')

// 请求头候选：仅 OpenAI / Anthropic 常用头，作为下拉提示（可自行输入任意头名）
const headerSuggestions = [
  'anthropic-version',
  'anthropic-beta',
  'OpenAI-Organization',
  'OpenAI-Project',
  'OpenAI-Beta',
]

const passthroughOptions = [
  { label: '不透传', value: 'none' },
  { label: '白名单', value: 'whitelist' },
  { label: '黑名单', value: 'blacklist' },
  { label: '全部透传', value: 'all' },
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
    form.passthrough_mode = v.RequestHeaders?.passthroughMode || 'none'
    form.whitelist = v.RequestHeaders?.whitelist || []
    form.blacklist = v.RequestHeaders?.blacklist || []
    form.set = v.RequestHeaders?.set || {}
    modelRows.value = Object.entries(v.Models || {}).map(([mid, ov]) => ({
      id: mid,
      protocols: Object.keys(ov?.protocols || {}),
      baseUrl: ov?.baseUrl || '',
    }))
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
onMounted(() => {
  // 返回「渠道」合并页时保持选中「渠道协议」视图
  sessionStorage.setItem('admin-channels-tab', 'protocols')
  load()
})

// 模型清单 增/删行
function addModelRow() {
  modelRows.value.push({ id: '', protocols: [], baseUrl: '' })
}
function removeModelRow(index: number) {
  modelRows.value.splice(index, 1)
}

// 由行数据组装后端 Models（{ [内部模型 ID]: { baseUrl?, protocols? } }）
function buildModels(): Record<string, ChannelProtocolModelOverride> {
  const out: Record<string, ChannelProtocolModelOverride> = {}
  for (const r of modelRows.value) {
    const mid = r.id.trim()
    if (!mid) continue
    const ov: ChannelProtocolModelOverride = {}
    if (r.baseUrl.trim()) ov.baseUrl = r.baseUrl.trim()
    if (r.protocols.length) {
      const p: Record<string, ProtocolOption> = {}
      for (const name of r.protocols) p[name] = {}
      ov.protocols = p
    }
    out[mid] = ov
  }
  return out
}

async function onSubmit() {
  if (!form.name.trim()) return ElMessage.warning('请填写名称')
  if (form.base_url.trim() && !/^https?:\/\//.test(form.base_url.trim()))
    return ElMessage.warning('BaseURL 必须以 http(s):// 开头')
  if (!enabledProtocols.value.length) return ElMessage.warning('请至少启用一个基础协议')

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
    RequestHeaders: {
      passthroughMode: form.passthrough_mode,
      whitelist: form.whitelist,
      blacklist: form.blacklist,
      set: form.set,
    },
    Models: buildModels(),
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
      router.push('/admin/channels')
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
          <div class="block-desc">选择该协议模板原生支持的上游基础协议；键存在 = 支持。可逐协议覆盖路径与上游地址。鉴权由协议隐含（openai-* → Authorization Bearer；anthropic-messages → x-api-key），如需其它方式请在「请求头策略 → 覆盖」中显式写入。</div>
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

      <el-tab-pane label="请求头策略" name="headers">
        <div class="card">
          <div class="block-desc">控制下游请求头如何透传到上游；鉴权由基础协议隐含注入，「覆盖」在其之后应用，可显式覆盖鉴权头。</div>
          <el-form label-width="130px" class="form-grid">
            <el-form-item class="span-2" label="透传模式">
              <el-segmented :model-value="form.passthrough_mode" :options="passthroughOptions" @change="(v: string | number | boolean) => (form.passthrough_mode = String(v))" />
            </el-form-item>
            <el-form-item label="白名单">
              <StringListEditor
                v-model="form.whitelist"
                :suggestions="headerSuggestions"
                placeholder="仅这些下游头透传"
              />
              <div class="hint">下拉仅为常用头提示，可自行输入；仅在「白名单」模式下生效</div>
            </el-form-item>
            <el-form-item label="黑名单">
              <StringListEditor
                v-model="form.blacklist"
                :suggestions="headerSuggestions"
                placeholder="除这些下游头外透传"
              />
              <div class="hint">下拉仅为常用头提示，可自行输入；仅在「黑名单」模式下生效</div>
            </el-form-item>
            <el-form-item class="span-2" label="覆盖">
              <KvpEditor v-model="form.set" key-placeholder="请求头名" value-placeholder="值（支持变量）" />
              <div class="hint">覆盖语义（非合并），在协议隐含鉴权之后应用；支持变量 {api_key} / {session_id} / {request_id} / {model} / {client_header:NAME}</div>
            </el-form-item>
          </el-form>
        </div>
      </el-tab-pane>

      <el-tab-pane label="模型清单" name="models">
        <div class="card">
          <div class="block-desc">列出该协议模板下的内部模型；可为每个模型收窄可达协议子集并覆盖 BaseURL（留空 = 继承渠道协议级配置）。</div>
          <el-table :data="modelRows" border class="model-table">
            <el-table-column label="内部模型 ID" min-width="200">
              <template #default="{ row }">
                <el-input v-model="row.id" placeholder="上游模型名（请求体 model 字段）" />
              </template>
            </el-table-column>
            <el-table-column label="可达协议" min-width="260">
              <template #default="{ row }">
                <el-select v-model="row.protocols" multiple clearable placeholder="留空 = 继承渠道协议" style="width: 100%">
                  <el-option v-for="p in baseProtocols" :key="p" :label="protocolLabels[p]" :value="p" />
                </el-select>
              </template>
            </el-table-column>
            <el-table-column label="BaseURL 覆盖" min-width="200">
              <template #default="{ row }">
                <el-input v-model="row.baseUrl" placeholder="留空 = 继承协议级" />
              </template>
            </el-table-column>
            <el-table-column label="操作" width="80" align="center">
              <template #default="{ $index }">
                <el-button link type="danger" @click="removeModelRow($index)">删除</el-button>
              </template>
            </el-table-column>
          </el-table>
          <el-button class="add-tier-btn" :icon="Plus" @click="addModelRow">添加模型</el-button>
        </div>
      </el-tab-pane>
    </el-tabs>

    <div class="actions">
      <el-button type="primary" :loading="saving" @click="onSubmit">{{ isEdit ? '保存' : '创建' }}</el-button>
      <el-button @click="router.push('/admin/channels')">取消</el-button>
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
  align-items: start;
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
.add-tier-btn {
  margin-top: 10px;
}
.actions {
  margin-top: 20px;
  display: flex;
  align-items: center;
  gap: 8px;
}
</style>
