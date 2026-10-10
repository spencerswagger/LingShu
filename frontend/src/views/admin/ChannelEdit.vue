<script setup lang="ts">
import { onMounted, reactive, ref, watch } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Plus, Edit, Delete } from '@element-plus/icons-vue'
import {
  listChannels,
  updateChannel,
  channelState,
  listChannelModels,
  listChannelEvents,
  listModelEvents,
  listProbeLogs,
  listChannelKeys,
  createChannelKey,
  updateChannelKey,
  deleteChannelKey,
  channelKeyState,
  probeChannelKey,
  cronPreview,
  listTags,
  type AdminChannel,
  type ChannelEvent,
  type ChannelModelEvent,
  type ProbeLog,
  type ChannelKey,
} from '@/api/admin'
import ChannelModels from '@/components/ChannelModels.vue'
import StatusTag from '@/components/StatusTag.vue'
import CronInput from '@/components/CronInput.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'

const route = useRoute()
const router = useRouter()
const id = String(route.params.id)

const loading = ref(false)
const saving = ref(false)
const activeTab = ref('base')
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

const channel = ref<AdminChannel | null>(null)
const events = ref<ChannelEvent[]>([])
const modelEvents = ref<ChannelModelEvent[]>([])
const probeLogs = ref<ProbeLog[]>([])
const actionLoading = ref(false)
// 探测模型下拉：渠道内部模型
const probeModels = ref<string[]>([])
// 渠道密钥（独立运行时实体）
const keys = ref<ChannelKey[]>([])
const keysLoading = ref(false)
const keyDrawerOpen = ref(false)
const keySaving = ref(false)
const keyEditingId = ref<string | null>(null) // null = 新增
const keyForm = reactive({ name: '', credential: '' })
const probingKeyId = ref<string | null>(null) // 正在手动探测的密钥 ID

const form = reactive({
  name: '',
  protocol: 'openai-compat',
  base_url: '',
  tag_ids: [] as string[],
  priority: 100,
  weight: 1,
  session_ttl_minutes: 60,
  rpm: 0,
  tpm: 0,
  burst_multiplier: 1.2,
  on_exceed: 'QUEUE' as string,
  queue_timeout_ms: 0,
  max_concurrent: 16,
  interval: '0 * * * * *',
  drain_interval_seconds: 15,
  timeout_ms: 15000,
  fail_threshold: 1,
  recovery_threshold: 2,
  probe_model: '',
  window_seconds: 60,
  min_samples: 10,
  error_rate_pct: 10,
  rate_429_pct: 20,
  p99_latency_ms: 5000,
  auth_fail_threshold: 3,
})

const stateLabel: Record<string, string> = { NORMAL: '正常', DRAIN: '排空', DISABLED: '禁用' }

const tagOptions = ref<{ ID: string; Name: string }[]>([])
async function loadTags() {
  try {
    const res = await listTags({ Enabled: true })
    tagOptions.value = res.Data || []
  } catch {
    tagOptions.value = []
  }
}

// TPM 单位切换：后端仍保存原始 token 数，仅前端以 百万/K/无 展示辅助录入
// 注意：el-select 的 @change 触发时 v-model 已更新为新单位，须用 tpmPrevUnit 记录切换前单位换算
const tpmUnit = ref<'M' | 'K' | '1'>('M')
const tpmPrevUnit = ref<'M' | 'K' | '1'>('M')
const tpmInput = ref<number>(0)
const tpmFactor: Record<'M' | 'K' | '1', number> = { M: 1000000, K: 1000, 1: 1 }
function setTpmDisplay(v: unknown) {
  const n = Number(v) || 0
  if (n >= 1000000) {
    tpmUnit.value = 'M'
    tpmPrevUnit.value = 'M'
    tpmInput.value = +(n / 1000000).toFixed(2)
  } else if (n >= 1000) {
    tpmUnit.value = 'K'
    tpmPrevUnit.value = 'K'
    tpmInput.value = +(n / 1000).toFixed(2)
  } else {
    tpmUnit.value = '1'
    tpmPrevUnit.value = '1'
    tpmInput.value = n
  }
}
function onTpmUnitChange() {
  const raw = (tpmInput.value || 0) * tpmFactor[tpmPrevUnit.value]
  tpmInput.value = +(raw / tpmFactor[tpmUnit.value]).toFixed(2)
  tpmPrevUnit.value = tpmUnit.value
}
function tpmRaw(): number {
  return Math.round((tpmInput.value || 0) * tpmFactor[tpmUnit.value])
}

// 流转原因中文
function reasonZh(s: string): string {
  const map: Record<string, string> = {
    probe_recovered: '探测恢复',
    probe_failed: '探测失败',
    error_rate_exceeded: '错误率超限',
    http_429_exceeded: '429 占比超限',
    p99_latency_exceeded: 'P99 耗时超限',
    auth_failure: '凭据失效',
    manual_normal: '手动置正常',
    'manual recover': '手动置正常',
    manual_drain: '手动置排空',
    manual_disable: '手动置禁用',
    'manual disable': '手动置禁用',
  }
  return map[s] || s
}

// 时间双行展示（与账单页一致）
function fmtDate(v: string): string {
  return v ? v.slice(0, 10) : '-'
}
function fmtTime(v: string): string {
  return v ? v.slice(11, 19) : ''
}

// 正常态 cron 后续执行时间预览（防写错）
const cronFutures = ref<string[]>([])
let cronTimer: ReturnType<typeof setTimeout> | null = null
async function refreshCronPreview() {
  if (cronTimer) clearTimeout(cronTimer)
  cronTimer = setTimeout(async () => {
    const parts = (form.interval.trim().match(/\S+/g) || []).length
    if (parts !== 6) {
      cronFutures.value = []
      return
    }
    try {
      const res = await cronPreview(form.interval, 5)
      cronFutures.value = res.Data?.Times || []
    } catch {
      cronFutures.value = []
    }
  }, 300)
}
watch(() => form.interval, refreshCronPreview, { immediate: true })

function num(v: unknown, def: number): number {
  const n = Number(v)
  return Number.isFinite(n) && n > 0 ? n : def
}

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const [res, cmRes] = await Promise.all([listChannels(), listChannelModels(id).catch(() => ({ Data: [] as never[] }))])
    probeModels.value = (cmRes.Data || []).map((m: { InternalModelID: string }) => m.InternalModelID)
    const row = (res.Data || []).find((c: AdminChannel) => c.ID === id)
    if (!row) {
      ElMessage.error('渠道不存在')
      router.push('/admin/channels')
      return
    }
    channel.value = row
    form.name = row.Name
    form.protocol = row.Protocol
    form.base_url = row.BaseURL
    form.tag_ids = row.TagIDs || []
    form.priority = row.Priority
    form.weight = row.Weight || 1
    form.session_ttl_minutes = row.SessionTTLMinutes || 60
    form.rpm = row.RateLimit?.RPM || 0
    setTpmDisplay(row.RateLimit?.TPM || 0)
    form.burst_multiplier = row.RateLimit?.BurstMultiplier || 1.2
    form.on_exceed = row.RateLimit?.OnExceed || 'QUEUE'
    form.queue_timeout_ms = row.RateLimit?.QueueTimeoutMS || 0
    form.max_concurrent = row.RateLimit?.MaxConcurrent || 16
    form.interval = row.HealthProbe?.Interval || '0 * * * * *'
    form.drain_interval_seconds = num(row.HealthProbe?.DrainIntervalSeconds, 15)
    form.timeout_ms = num(row.HealthProbe?.TimeoutMS, 15000)
    form.fail_threshold = num(row.HealthProbe?.FailThreshold, 1)
    form.recovery_threshold = num(row.HealthProbe?.RecoveryThreshold, 2)
    form.probe_model = row.HealthProbe?.ProbeModel || ''
    form.window_seconds = num(row.Reliability?.WindowSeconds, 60)
    form.min_samples = num(row.Reliability?.MinSamples, 10)
    form.error_rate_pct = num(row.Reliability?.ErrorRatePct, 10)
    form.rate_429_pct = num(row.Reliability?.Rate429Pct, 20)
    form.p99_latency_ms = num(row.Reliability?.P99LatencyMS, 5000)
    form.auth_fail_threshold = num(row.Reliability?.AuthFailThreshold, 3)
    await loadStateData()
    await loadKeys()
    await loadTags()
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}
onMounted(load)

async function loadStateData() {
  const [evRes, meRes, plRes] = await Promise.all([
    listChannelEvents(id).catch(() => ({ Data: [] as ChannelEvent[] })),
    listModelEvents(id).catch(() => ({ Data: [] as ChannelModelEvent[] })),
    listProbeLogs(id, 50).catch(() => ({ Data: [] as ProbeLog[] })),
  ])
  events.value = evRes.Data || []
  modelEvents.value = meRes.Data || []
  probeLogs.value = plRes.Data || []
}

async function loadKeys() {
  keysLoading.value = true
  try {
    const res = await listChannelKeys(id)
    keys.value = res.Data || []
  } catch (e: any) {
    ElMessage.error(e?.message || '加载渠道密钥失败')
  } finally {
    keysLoading.value = false
  }
}
// 切到涉及密钥的 tab 时刷新（onMounted 时 load() 已加载一次）
watch(activeTab, (tab) => {
  if (tab === 'base' || tab === 'status') loadKeys()
})

function openKeyCreate() {
  keyEditingId.value = null
  keyForm.name = ''
  keyForm.credential = ''
  keyDrawerOpen.value = true
}
function openKeyEdit(k: ChannelKey) {
  keyEditingId.value = k.ID
  keyForm.name = k.Name
  keyForm.credential = ''
  keyDrawerOpen.value = true
}
async function saveKey() {
  if (!keyForm.name.trim()) return ElMessage.warning('请填写密钥名称')
  if (keyEditingId.value == null && !keyForm.credential.trim())
    return ElMessage.warning('请填写密钥凭据')
  keySaving.value = true
  try {
    if (keyEditingId.value != null) {
      const payload: { Name: string; Credential?: string } = { Name: keyForm.name.trim() }
      if (keyForm.credential.trim()) payload.Credential = keyForm.credential.trim()
      await updateChannelKey(id, keyEditingId.value, payload)
      ElMessage.success('密钥已更新')
    } else {
      await createChannelKey(id, {
        Name: keyForm.name.trim(),
        Credential: keyForm.credential.trim(),
      })
      ElMessage.success('密钥已添加')
    }
    keyDrawerOpen.value = false
    await loadKeys()
  } catch (e: any) {
    ElMessage.error(e?.message || '保存失败')
  } finally {
    keySaving.value = false
  }
}
async function onDeleteKey(k: ChannelKey) {
  try {
    await ElMessageBox.confirm(
      `确认删除密钥「${k.Name}」吗？删除后该密钥立即停止路由。`,
      '删除确认',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    await deleteChannelKey(id, k.ID)
    ElMessage.success('已删除')
    await loadKeys()
  } catch (e: any) {
    ElMessage.error(e?.message || '删除失败')
  }
}
async function onKeyState(k: ChannelKey, action: 'normal' | 'drain' | 'disable') {
  const label = stateLabel[action.toUpperCase()] || action
  try {
    await channelKeyState(id, k.ID, action)
    ElMessage.success(`密钥「${k.Name}」已置为 ${label}`)
    await loadKeys()
  } catch (e: any) {
    ElMessage.error(e?.message || '状态操作失败')
  }
}

// 手动触发一轮密钥健康探测：结果按定时探测同规则驱动密钥状态机，随后刷新列表。
async function onProbeKey(k: ChannelKey) {
  probingKeyId.value = k.ID
  try {
    const res = await probeChannelKey(id, k.ID)
    const o = res.Data
    if (o.OK) {
      ElMessage.success(`密钥「${k.Name}」探测成功（${o.DurationMS}ms）`)
    } else {
      ElMessage.error(`密钥「${k.Name}」探测失败：${o.Error || '未知错误'}`)
    }
    await loadKeys()
  } catch (e: any) {
    ElMessage.error(e?.message || '探测失败')
  } finally {
    probingKeyId.value = null
  }
}

async function onToggleState(action: 'normal' | 'drain' | 'disable') {
  const label = stateLabel[action.toUpperCase()] || action
  try {
    await ElMessageBox.confirm(`确认手动将渠道置为「${label}」？`, '状态操作', {
      type: 'warning',
      confirmButtonText: '确认',
      cancelButtonText: '取消',
    })
  } catch {
    return
  }
  actionLoading.value = true
  try {
    const res = await channelState(id, action)
    channel.value = res.Data
    ElMessage.success(`已置为 ${label}`)
    await loadStateData()
  } catch (e: any) {
    ElMessage.error(e?.message || '操作失败')
  } finally {
    actionLoading.value = false
  }
}

async function onSubmit() {
  if (!form.name.trim()) return ElMessage.warning('请填写渠道名称')
  if (!/^https?:\/\//.test(form.base_url)) return ElMessage.warning('Base URL 必须以 http(s):// 开头')

  saving.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    await updateChannel(id, {
      Name: form.name.trim(),
      Protocol: form.protocol,
      BaseURL: form.base_url.trim(),
      TagIDs: form.tag_ids,
      Priority: form.priority,
      Weight: Number(form.weight) || 1,
      SessionTTLMinutes: Number(form.session_ttl_minutes) || 60,
      RateLimit: {
        RPM: Number(form.rpm),
        TPM: tpmRaw(),
        BurstMultiplier: Number(form.burst_multiplier),
        OnExceed: form.on_exceed,
        QueueSize: 0,
        QueueTimeoutMS: Number(form.queue_timeout_ms),
        MaxConcurrent: Number(form.max_concurrent),
      },
      HealthProbe: {
        Interval: form.interval || '0 * * * * *',
        DrainIntervalSeconds: Number(form.drain_interval_seconds),
        TimeoutMS: Number(form.timeout_ms),
        FailThreshold: Number(form.fail_threshold),
        RecoveryThreshold: Number(form.recovery_threshold),
        ProbeModel: form.probe_model,
      },
      Reliability: {
        WindowSeconds: Number(form.window_seconds),
        MinSamples: Number(form.min_samples),
        ErrorRatePct: Number(form.error_rate_pct),
        Rate429Pct: Number(form.rate_429_pct),
        P99LatencyMS: Number(form.p99_latency_ms),
        AuthFailThreshold: Number(form.auth_fail_threshold),
      },
    })
    ElMessage.success('渠道已更新')
    await load()
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
              <el-input v-model="form.name" maxlength="64" />
            </el-form-item>
            <el-form-item label="协议">
              <el-select v-model="form.protocol" style="width: 100%">
                <el-option label="openai-compat" value="openai-compat" />
              </el-select>
            </el-form-item>
            <el-form-item class="span-2" label="Base URL">
          <el-input v-model="form.base_url" />
          <div class="hint">上游供应商地址，网关请求将转发到该地址下的 /chat/completions 等路径</div>
        </el-form-item>
            <el-form-item class="span-2" label="标签">
              <el-select v-model="form.tag_ids" multiple filterable style="width: 100%"
                placeholder="选择该渠道绑定的语义标签">
                <el-option v-for="t in tagOptions" :key="t.ID" :label="t.Name" :value="t.ID" />
              </el-select>
              <div class="hint">令牌的语义标签命中本渠道已绑任一标签时才路由到此渠道</div>
            </el-form-item>
            <el-form-item label="优先级">
              <el-input-number v-model="form.priority" :min="1" :max="999" style="width: 100%" />
              <div class="hint">优先级数字越大越先被路由；同一优先级的渠道按权重比例分配流量（权重越大流量越多）；已有会话固定路由到原渠道</div>
            </el-form-item>
            <el-form-item label="权重">
              <el-input-number v-model="form.weight" :min="1" :max="10000" style="width: 100%" />
              <div class="hint">仅在同一优先级内生效，权重越大被选中的概率越高；默认 1 表示等权</div>
            </el-form-item>
          </el-form>
          <div class="key-section">
            <div class="key-head">
              <span class="state-label">渠道密钥</span>
              <el-tooltip content="密钥为独立运行时实体：探测、限流、会话计数按密钥各自独立；渠道级状态为批量操作" placement="top">
                <span class="key-tip">?</span>
              </el-tooltip>
              <el-button size="small" type="primary" :icon="Plus" @click="openKeyCreate">新增密钥</el-button>
            </div>
            <div v-loading="keysLoading">
              <el-table :data="keys" border stripe class="table-nowrap small">
                <el-table-column label="名称" prop="Name" min-width="130" show-overflow-tooltip />
                <el-table-column label="凭据尾号" min-width="110">
                  <template #default="{ row }">
                    <span class="tail">••••••{{ row.CredentialTail }}</span>
                  </template>
                </el-table-column>
                <el-table-column label="状态" width="110" align="center">
                  <template #default="{ row }">
                    <el-popover trigger="hover" placement="left" :width="170">
                      <template #reference>
                        <StatusTag :value="row.State" style="cursor: pointer" />
                      </template>
                      <div class="pop-actions">
                        <el-button v-if="row.State !== 'NORMAL'" size="small" type="success" @click="onKeyState(row, 'normal')">正常</el-button>
                        <el-button v-if="row.State !== 'DRAIN'" size="small" type="warning" @click="onKeyState(row, 'drain')">排空</el-button>
                        <el-button v-if="row.State !== 'DISABLED'" size="small" type="danger" @click="onKeyState(row, 'disable')">禁用</el-button>
                      </div>
                    </el-popover>
                  </template>
                </el-table-column>
                <el-table-column label="最近错误" min-width="160">
                  <template #default="{ row }">
                    <span class="last-err" :class="{ on: row.LastErr }">{{ row.LastErr || '无' }}</span>
                  </template>
                </el-table-column>
                <el-table-column label="操作" width="140" align="right">
                  <template #default="{ row }">
                    <div class="op-cell" @click.stop>
                      <el-button
                        size="small"
                        text
                        type="primary"
                        :loading="probingKeyId === row.ID"
                        @click="onProbeKey(row)"
                      >探测</el-button>
                      <el-tooltip content="编辑" placement="top">
                        <el-icon class="op-icon" @click="openKeyEdit(row)"><Edit /></el-icon>
                      </el-tooltip>
                      <el-tooltip content="删除" placement="top">
                        <el-icon class="op-icon danger" @click="onDeleteKey(row)"><Delete /></el-icon>
                      </el-tooltip>
                    </div>
                  </template>
                </el-table-column>
              </el-table>
              <el-empty v-if="!keysLoading && !keys.length" description="该渠道暂无密钥，新增后路由才会生效" :image-size="48" />
            </div>
          </div>
          <div class="actions">
            <el-button type="primary" :loading="saving" @click="onSubmit">保存</el-button>
            <el-button @click="router.push('/admin/channels')">取消</el-button>
          </div>
          <el-drawer
            v-model="keyDrawerOpen"
            :title="keyEditingId == null ? '新增密钥' : '编辑密钥'"
            size="460"
            destroy-on-close
          >
            <el-form label-width="96px" class="key-form">
              <el-form-item label="名称" required>
                <el-input v-model="keyForm.name" maxlength="64" placeholder="如 默认密钥 / 备用密钥" />
              </el-form-item>
              <el-form-item label="凭据">
                <el-input
                  v-model="keyForm.credential"
                  type="password"
                  show-password
                  :placeholder="keyEditingId == null ? '上游 API Key' : '留空 = 保持不变'"
                />
                <div class="hint">{{ keyEditingId == null ? '该密钥为渠道上游的真实 API Key，加密存储，仅回显尾号' : '修改凭据时填写新值；留空表示不修改' }}</div>
              </el-form-item>
            </el-form>
            <template #footer>
              <el-button @click="keyDrawerOpen = false">取消</el-button>
              <el-button type="primary" :loading="keySaving" @click="saveKey">保存</el-button>
            </template>
          </el-drawer>
          <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
        </div>
      </el-tab-pane>

      <el-tab-pane label="模型" name="models">
        <div class="card">
          <ChannelModels :channel-id="id" :readonly="false" />
        </div>
      </el-tab-pane>

      <el-tab-pane label="健康探测" name="health">
        <div v-loading="loading" class="card">
          <el-form label-width="150px" class="form-grid">
            <el-form-item class="span-2" label="探测模型">
              <el-select
                v-model="form.probe_model"
                filterable
                allow-create
                default-first-option
                clearable
                placeholder="留空自动选型"
                style="width: 100%"
              >
                <el-option v-for="m in probeModels" :key="m" :label="m" :value="m" />
              </el-select>
              <div class="hint">选渠道内的内部模型作为探测目标（真实最小对话请求）；留空自动选型</div>
            </el-form-item>
            <el-form-item label="探测超时">
              <div class="with-unit">
                <el-input-number v-model="form.timeout_ms" :min="0" style="flex: 1" />
                <span class="unit">ms</span>
              </div>
              <div class="hint">单次探测的最长等待时间，超时计一次失败</div>
            </el-form-item>

            <div class="probe-group-title">正常状态下</div>
            <el-form-item class="span-2" label="频率">
              <CronInput v-model="form.interval" />
              <div v-if="cronFutures.length" class="cron-table-wrap">
                <el-table :data="cronFutures" size="small" border stripe class="table-nowrap">
                  <el-table-column label="次数" width="90" align="center">
                    <template #default="{ $index }">第 {{ $index + 1 }} 次</template>
                  </el-table-column>
                  <el-table-column label="执行时间" min-width="170">
                    <template #default="{ row }">{{ row.slice(5, 19).replace('T', ' ') }}</template>
                  </el-table-column>
                </el-table>
              </div>
              <div class="hint">正常状态下按此 cron 频率发起健康探测</div>
            </el-form-item>
            <el-form-item label="失败阈值">
              <div class="with-unit">
                <el-input-number v-model="form.fail_threshold" :min="1" style="flex: 1" />
                <span class="unit">次</span>
              </div>
              <div class="hint">连续探测失败达到该次数即排空</div>
            </el-form-item>
            <el-form-item label="恢复阈值">
              <div class="with-unit">
                <el-input-number v-model="form.recovery_threshold" :min="1" style="flex: 1" />
                <span class="unit">次</span>
              </div>
              <div class="hint">连续探测成功达到该次数后自动恢复正常</div>
            </el-form-item>

            <div class="probe-group-title">排空状态下</div>
            <el-form-item label="频率">
              <div class="with-unit">
                <el-input-number v-model="form.drain_interval_seconds" :min="1" style="flex: 1" />
                <span class="unit">秒</span>
              </div>
              <div class="hint">排空状态下更勤地探测，加速恢复证据采集</div>
            </el-form-item>
          </el-form>
          <div class="actions">
            <el-button type="primary" :loading="saving" @click="onSubmit">保存</el-button>
            <el-button @click="router.push('/admin/channels')">取消</el-button>
          </div>
          <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
        </div>
      </el-tab-pane>

      <el-tab-pane label="可靠性" name="reliability">
        <div class="card">
          <el-form label-width="150px" class="form-grid">
            <el-form-item label="窗口时长">
          <div class="with-unit">
                <el-input-number v-model="form.window_seconds" :min="1"  style="flex: 1" />
                <span class="unit">秒</span>
              </div>
          <div class="hint">统计真实调用指标的滑动窗口长度（秒）</div>
        </el-form-item>
            <el-form-item label="最小样本数">
          <div class="with-unit">
                <el-input-number v-model="form.min_samples" :min="1"  style="flex: 1" />
                <span class="unit">个</span>
              </div>
          <div class="hint">窗口内样本不足该值时不做评估，避免少样本误判</div>
        </el-form-item>
            <el-form-item label="错误率阈值">
          <div class="with-unit">
                <el-input-number v-model="form.error_rate_pct" :min="0" :max="100"  style="flex: 1" />
                <span class="unit">%</span>
              </div>
          <div class="hint">窗口内 (失败+超时)/总数 超过该百分比即排空</div>
        </el-form-item>
            <el-form-item label="429 占比">
          <div class="with-unit">
                <el-input-number v-model="form.rate_429_pct" :min="0" :max="100"  style="flex: 1" />
                <span class="unit">%</span>
              </div>
          <div class="hint">429 是上游返回的 HTTP 429（Too Many Requests，限流/过载）状态码；窗口内该占比超过此百分比即排空</div>
        </el-form-item>
            <el-form-item label="P99 耗时">
          <div class="with-unit">
                <el-input-number v-model="form.p99_latency_ms" :min="0"  style="flex: 1" />
                <span class="unit">ms</span>
              </div>
          <div class="hint">P99 = 窗口内成功样本按时长升序排列的第 99 百分位耗时（99% 的请求快于该值），衡量长尾延迟；超过该毫秒数即排空</div>
        </el-form-item>
            <el-form-item label="鉴权失败熔断">
          <div class="with-unit">
                <el-input-number v-model="form.auth_fail_threshold" :min="0"  style="flex: 1" />
                <span class="unit">次</span>
              </div>
          <div class="hint">上游连续返回 401/403（凭据失效）达到此次数才禁用该渠道/模型；单次失败只转移到下一候选，避免抖动误杀。健康探测的 401/403 仍一次即禁用</div>
        </el-form-item>
          </el-form>
          <div class="actions">
            <el-button type="primary" :loading="saving" @click="onSubmit">保存</el-button>
            <el-button @click="router.push('/admin/channels')">取消</el-button>
          </div>
          <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
        </div>
      </el-tab-pane>

      <el-tab-pane label="限流" name="limit">
        <div class="card">
          <el-form label-width="150px" class="form-grid">
            <el-form-item label="RPM">
          <div class="with-unit">
                <el-input-number v-model="form.rpm" :min="0"  style="flex: 1" />
                <span class="unit">次/分</span>
              </div>
          <div class="hint">每分钟最大请求数；0 不限。超限自动切换下一候选渠道</div>
        </el-form-item>
            <el-form-item label="TPM">
          <div class="with-unit">
                <el-input-number v-model="tpmInput" :min="0" style="flex: 1" />
                <el-select v-model="tpmUnit" style="width: 96px" @change="onTpmUnitChange">
                  <el-option label="百万" value="M" />
                  <el-option label="K" value="K" />
                  <el-option label="无" value="1" />
                </el-select>
                <span class="unit">tokens/分</span>
              </div>
          <div class="hint">每分钟最大 token 数，按请求体字节估算；0 不限。数值过大时可切换到百万 / K 单位录入</div>
        </el-form-item>
            <el-form-item label="突发系数">
          <div class="with-unit">
                <el-input-number v-model="form.burst_multiplier" :min="1" :max="10" :step="0.5"  style="flex: 1" />
                <span class="unit">×</span>
              </div>
          <div class="hint">允许瞬时超发的额度倍数</div>
        </el-form-item>
            <el-form-item label="并发会话数">
          <div class="with-unit">
                <el-input-number v-model="form.max_concurrent" :min="0"  style="flex: 1" />
                <span class="unit">个</span>
              </div>
          <div class="hint">每渠道存活会话数上限；新会话超限自动切换到下一候选</div>
        </el-form-item>
            <el-form-item label="会话存活时长">
          <div style="display: flex; align-items: center; gap: 6px; width: 100%">
                <el-input-number v-model="form.session_ttl_minutes" :min="1" style="flex: 1" />
                <span>分钟</span>
              </div>
          <div class="hint">会话无活动超过该时长后过期释放</div>
        </el-form-item>
            <el-form-item label="超限策略">
          <el-radio-group v-model="form.on_exceed">
                <el-radio-button label="QUEUE">排队</el-radio-button>
                <el-radio-button label="REJECT">拒绝</el-radio-button>
              </el-radio-group>
          <div class="hint">排队 = 等待窗口容量；拒绝 = 立即超时并切换下一渠道</div>
        </el-form-item>
            <el-form-item label="队列超时">
          <div style="display: flex; align-items: center; gap: 6px; width: 100%">
                <el-input-number v-model="form.queue_timeout_ms" :min="0" style="flex: 1" />
                <span>ms</span>
              </div>
          <div class="hint">排队等待窗口容量的最大时长，超过则切换下一渠道</div>
        </el-form-item>
          </el-form>
          <div class="actions">
            <el-button type="primary" :loading="saving" @click="onSubmit">保存</el-button>
            <el-button @click="router.push('/admin/channels')">取消</el-button>
          </div>
          <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
        </div>
      </el-tab-pane>

      <el-tab-pane label="状态" name="status">
        <div v-loading="loading" class="card">
          <template v-if="channel">
            <div class="state-head">
              <span class="state-label">渠道状态</span>
              <StatusTag :value="channel.State" />
              <div class="state-actions">
                <template v-if="channel.State !== 'NORMAL'">
                  <el-button size="small" type="success" :loading="actionLoading" @click="onToggleState('normal')">正常</el-button>
                </template>
                <template v-if="channel.State !== 'DRAIN'">
                  <el-button size="small" type="warning" :loading="actionLoading" @click="onToggleState('drain')">排空</el-button>
                </template>
                <template v-if="channel.State !== 'DISABLED'">
                  <el-button size="small" type="danger" :loading="actionLoading" @click="onToggleState('disable')">禁用</el-button>
                </template>
              </div>
            </div>
            <div class="sub-title">渠道状态流转记录</div>
            <el-table :data="events" border stripe class="table-nowrap small">
              <el-table-column label="变更" width="150">
                <template #default="{ row }">
                  <StatusTag :value="row.FromState" />
                  <span class="arrow">→</span>
                  <StatusTag :value="row.ToState" />
                </template>
              </el-table-column>
              <el-table-column label="原因" min-width="200" show-overflow-tooltip>
                <template #default="{ row }">{{ reasonZh(row.Reason) }}</template>
              </el-table-column>
              <el-table-column label="时间" min-width="150">
                <template #default="{ row }">
                  <div class="t-date">{{ fmtDate(row.CreatedAt) }}</div>
                  <div class="t-time">{{ fmtTime(row.CreatedAt) }}</div>
                </template>
              </el-table-column>
            </el-table>
            <el-empty v-if="!events.length" description="暂无渠道状态流转记录" :image-size="48" />

            <div class="sub-title">密钥状态</div>
            <div v-loading="keysLoading">
              <el-table :data="keys" border stripe class="table-nowrap small">
                <el-table-column label="名称" prop="Name" min-width="130" show-overflow-tooltip />
                <el-table-column label="状态" width="110" align="center">
                  <template #default="{ row }">
                    <el-popover trigger="hover" placement="left" :width="170">
                      <template #reference>
                        <StatusTag :value="row.State" style="cursor: pointer" />
                      </template>
                      <div class="pop-actions">
                        <el-button v-if="row.State !== 'NORMAL'" size="small" type="success" @click="onKeyState(row, 'normal')">正常</el-button>
                        <el-button v-if="row.State !== 'DRAIN'" size="small" type="warning" @click="onKeyState(row, 'drain')">排空</el-button>
                        <el-button v-if="row.State !== 'DISABLED'" size="small" type="danger" @click="onKeyState(row, 'disable')">禁用</el-button>
                      </div>
                    </el-popover>
                  </template>
                </el-table-column>
                <el-table-column label="最近错误" min-width="180">
                  <template #default="{ row }">
                    <span class="last-err" :class="{ on: row.LastErr }">{{ row.LastErr || '无' }}</span>
                  </template>
                </el-table-column>
                <el-table-column label="活跃会话数" width="100" align="right">
                  <template #default="{ row }">{{ row.ActiveSessions }}</template>
                </el-table-column>
                <el-table-column label="操作" width="150" align="right">
                  <template #default="{ row }">
                    <div class="pop-actions key-state-ops">
                      <el-button v-if="row.State !== 'NORMAL'" size="small" type="success" text @click="onKeyState(row, 'normal')">正常</el-button>
                      <el-button v-if="row.State !== 'DRAIN'" size="small" type="warning" text @click="onKeyState(row, 'drain')">排空</el-button>
                      <el-button v-if="row.State !== 'DISABLED'" size="small" type="danger" text @click="onKeyState(row, 'disable')">禁用</el-button>
                    </div>
                  </template>
                </el-table-column>
              </el-table>
              <el-empty v-if="!keysLoading && !keys.length" description="该渠道暂无密钥，新增后路由才会生效" :image-size="48" />
            </div>

            <div class="sub-title">内部模型状态流转记录</div>
            <el-table :data="modelEvents" border stripe class="table-nowrap small">
              <el-table-column label="模型" min-width="150" prop="ModelID" show-overflow-tooltip />
              <el-table-column label="变更" width="150">
                <template #default="{ row }">
                  <StatusTag :value="row.FromState" />
                  <span class="arrow">→</span>
                  <StatusTag :value="row.ToState" />
                </template>
              </el-table-column>
              <el-table-column label="原因" min-width="200" show-overflow-tooltip>
                <template #default="{ row }">{{ reasonZh(row.Reason) }}</template>
              </el-table-column>
              <el-table-column label="时间" min-width="150">
                <template #default="{ row }">
                  <div class="t-date">{{ fmtDate(row.CreatedAt) }}</div>
                  <div class="t-time">{{ fmtTime(row.CreatedAt) }}</div>
                </template>
              </el-table-column>
            </el-table>
            <el-empty v-if="!modelEvents.length" description="暂无模型状态流转记录" :image-size="48" />

            <div class="sub-title">健康探测历史（最近 50 次）</div>
            <el-table :data="probeLogs" border stripe class="table-nowrap small">
              <el-table-column label="层级" width="80" align="center">
                <template #default="{ row }">
                  <el-tag size="small" effect="plain" :type="row.Level === 'key' ? 'info' : 'primary'">
                    {{ row.Level === 'key' ? '密钥' : '模型' }}
                  </el-tag>
                </template>
              </el-table-column>
              <el-table-column label="模型" min-width="130" prop="ModelID" show-overflow-tooltip>
                <template #default="{ row }">{{ row.ModelID || '—' }}</template>
              </el-table-column>
              <el-table-column label="结果" width="80" align="center">
                <template #default="{ row }">
                  <el-tooltip v-if="!row.OK && row.Error" :content="row.Error" placement="top">
                    <el-tag :type="row.OK ? 'success' : 'danger'" effect="plain" size="small" style="cursor: help">
                      {{ row.OK ? '成功' : '失败' }}
                    </el-tag>
                  </el-tooltip>
                  <el-tag v-else :type="row.OK ? 'success' : 'danger'" effect="plain" size="small">
                    {{ row.OK ? '成功' : '失败' }}
                  </el-tag>
                </template>
              </el-table-column>
              <el-table-column label="Token" width="130" align="right">
                <template #default="{ row }">入{{ row.InputTokens }}·出{{ row.OutputTokens }}</template>
              </el-table-column>
              <el-table-column label="耗时" width="90" align="right">
                <template #default="{ row }">{{ row.DurationMS }}ms</template>
              </el-table-column>
              <el-table-column label="时间" min-width="150">
                <template #default="{ row }">
                  <div class="t-date">{{ fmtDate(row.CreatedAt) }}</div>
                  <div class="t-time">{{ fmtTime(row.CreatedAt) }}</div>
                </template>
              </el-table-column>
            </el-table>
            <el-empty v-if="!probeLogs.length" description="暂无探测记录" :image-size="48" />
          </template>
          <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
        </div>
      </el-tab-pane>
    </el-tabs>
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
.with-unit {
  display: flex;
  align-items: center;
  gap: 6px;
  width: 100%;
}
.unit {
  flex-shrink: 0;
  color: var(--color-text-secondary);
  font-size: 13px;
}
.actions {
  margin-top: 20px;
}
.state-head {
  display: flex;
  align-items: center;
  gap: 12px;
  margin-bottom: 16px;
}
.state-actions {
  margin-left: auto;
  display: flex;
  align-items: center;
  gap: 8px;
}
.state-label {
  font-weight: 600;
}
.sub-title {
  font-size: 15px;
  font-weight: 600;
  margin: 18px 0 10px;
}
.arrow {
  margin: 0 6px;
  color: var(--color-text-secondary);
}
.t-date {
  color: var(--color-text);
  line-height: 1.3;
}
.t-time {
  font-size: 12px;
  color: var(--color-text-secondary);
}
.cron-table-wrap {
  margin-top: 8px;
  width: 100%;
  max-width: 480px;
}
.probe-group-title {
  grid-column: 1 / -1;
  font-size: 14px;
  font-weight: 600;
  color: var(--color-text);
  background: #f6f8ff;
  border: 1px solid #e4e9f2;
  border-radius: 8px;
  padding: 7px 12px;
  margin: 2px 0 6px;
}
.cron-table-wrap :deep(.el-table__cell) {
  padding: 4px 0;
}
.small :deep(.el-table__cell) {
  padding: 6px 0;
}
.key-section {
  margin-top: 22px;
}
.key-head {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 10px;
}
.key-tip {
  width: 16px;
  height: 16px;
  line-height: 16px;
  text-align: center;
  border-radius: 50%;
  background: #eef2fb;
  color: #4a7bd9;
  font-size: 11px;
  cursor: help;
}
.tail {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
  font-size: 12px;
  color: var(--color-text-secondary);
}
.last-err {
  font-size: 12px;
  color: var(--color-text-secondary);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}
.last-err.on {
  color: var(--el-color-danger);
}
.key-state-ops {
  justify-content: flex-end;
}
.key-form :deep(.el-form-item__content) {
  display: block;
}
</style>