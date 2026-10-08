<script setup lang="ts">
import { reactive, ref, watch } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { createChannel, cronPreview, listTags } from '@/api/admin'
import CronInput from '@/components/CronInput.vue'
import ErrorBubble from '@/components/ErrorBubble.vue'

const router = useRouter()
const activeTab = ref('base')
const saving = ref(false)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

const tagOptions = ref<{ ID: string; Name: string }[]>([])
async function loadTags() {
  try {
    const res = await listTags({ Enabled: true })
    tagOptions.value = res.Data || []
  } catch {
    tagOptions.value = []
  }
}
loadTags()

// TPM 单位切换：后端仍保存原始 token 数，仅前端以 百万/K/无 展示辅助录入
// 注意：el-select 的 @change 触发时 v-model 已更新为新单位，须用 tpmPrevUnit 记录切换前单位换算
const tpmUnit = ref<'M' | 'K' | '1'>('M')
const tpmPrevUnit = ref<'M' | 'K' | '1'>('M')
const tpmInput = ref<number>(1) // 默认 1000000 = 1 百万
const tpmFactor: Record<'M' | 'K' | '1', number> = { M: 1000000, K: 1000, 1: 1 }
function onTpmUnitChange() {
  const raw = (tpmInput.value || 0) * tpmFactor[tpmPrevUnit.value]
  tpmInput.value = +(raw / tpmFactor[tpmUnit.value]).toFixed(2)
  tpmPrevUnit.value = tpmUnit.value
}
function tpmRaw(): number {
  return Math.round((tpmInput.value || 0) * tpmFactor[tpmUnit.value])
}

const form = reactive({
  name: '',
  protocol: 'openai-compat',
  base_url: '',
  tag_ids: [] as string[],
  priority: 100,
  weight: 1,
  session_ttl_minutes: 60,
  rpm: 1000,
  tpm: 1000000,
  burst_multiplier: 1.2,
  on_exceed: 'QUEUE',
  queue_timeout_ms: 5000,
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

async function onSubmit() {
  if (!form.name.trim()) return ElMessage.warning('请填写渠道名称')
  if (!/^https?:\/\//.test(form.base_url)) return ElMessage.warning('Base URL 必须以 http(s):// 开头')

  saving.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await createChannel({
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
        QueueSize: 100,
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
    ElMessage.success('渠道已创建，请先添加密钥')
    router.push(`/admin/channels/${res.Data.ID}`)
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
        <div v-loading="saving" class="card">
          <el-form label-width="130px" class="form-grid">
            <el-form-item label="名称" required>
              <el-input v-model="form.name" placeholder="如 主渠道-DeepSeek" maxlength="64" />
            </el-form-item>
            <el-form-item label="协议">
              <el-select v-model="form.protocol" style="width: 100%">
                <el-option label="openai-compat" value="openai-compat" />
              </el-select>
            </el-form-item>
            <el-form-item class="span-2" label="Base URL">
          <el-input v-model="form.base_url" placeholder="https://api.example.com/v1" />
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
          <div class="actions">
            <el-button type="primary" :loading="saving" @click="onSubmit">保存</el-button>
            <el-button @click="router.push('/admin/channels')">取消</el-button>
          </div>
          <ErrorBubble v-if="errInfo.message || errInfo.requestId" :message="errInfo.message" :request-id="errInfo.requestId" />
        </div>
      </el-tab-pane>

      <el-tab-pane label="健康探测" name="health">
        <div class="card">
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
                <el-option v-for="m in []" :key="m" :label="m" :value="m" />
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
</style>