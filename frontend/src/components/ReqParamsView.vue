<script setup lang="ts">
import { computed } from 'vue'
import { ElMessage } from 'element-plus'
import type { ReqParamsView } from '@/utils/reqParams'

// 入站请求参数快照（call_logs.req_params）的统一渲染。
// 三组分区：请求（入站）/ 上游（出站）/ 鉴权；裁剪状态（omitted / dropped / over_budget）
// 必须有明确视觉标记——「键还在但值被省」与「字段被整个裁掉」不是一回事，不能都显示成空。
const props = withDefaults(
  defineProps<{
    params: ReqParamsView | null
    // 是否展示上游 URL（开发者端后端已脱敏，此处置 false 显式提示「不展示」而不是「未记录」）
    showInternal?: boolean
  }>(),
  { showInternal: true },
)

const p = computed(() => props.params)

const omittedCount = computed(() => (p.value?.Params || []).filter((r) => r.Status === 'omitted').length)
const droppedCount = computed(() => (p.value?.Dropped || []).length)
// 命中敏感键、值被后端置成 "redacted" 的参数（原文不落库）
const redactedKeys = computed(() => (p.value?.Params || []).filter((r) => r.Status === 'redacted').map((r) => r.Key))

// 值被「省略体积」的键名（不含 tools —— tools 本体就是摘要，单独展示）。
// 以 params 里的 omitted:NB 占位为主；后端若把某个键记进 summarized 而值没换标记，
// 也一并提示（两处都表示「值不可见」，不能只认一种）。
const omittedKeys = computed(() => {
  const fromParams = (p.value?.Params || [])
    .filter((r) => r.Status === 'omitted')
    .map((r) => `${r.Key}（${r.OmittedBytes}B）`)
  const seen = new Set((p.value?.Params || []).filter((r) => r.Status === 'omitted').map((r) => r.Key))
  const fromSummarized = (p.value?.Summarized || [])
    .filter((k) => k !== 'tools' && !seen.has(k))
    .map((k) => k)
  return [...fromParams, ...fromSummarized]
})

// 参数条数与裁剪计数（放在小标题右侧，一眼看出「有多少不可见」）
const paramsHint = computed(() => {
  const total = p.value?.Params.length || 0
  const parts: string[] = []
  if (omittedCount.value) parts.push(`${omittedCount.value} 项值已省略`)
  if (droppedCount.value) parts.push(`${droppedCount.value} 项已裁剪`)
  if (redactedKeys.value.length) parts.push(`${redactedKeys.value.length} 项已脱敏`)
  return parts.length ? `共 ${total} 项（${parts.join('，')}）` : `共 ${total} 项`
})

function fmtNum(n?: number): string {
  return n == null ? '—' : n.toLocaleString()
}

function copy(text: string) {
  navigator.clipboard
    .writeText(text)
    .then(() => ElMessage.success('已复制'))
    .catch(() => ElMessage.warning('复制失败，请手动选择复制'))
}
</script>

<template>
  <div class="rp">
    <div v-if="!p" class="rp-empty">未记录请求参数快照（该请求可能早于快照功能上线，或快照生成失败）</div>
    <template v-else>
      <!-- 裁剪警示：over_budget 最严重，单独一条；omitted/dropped 列出具体字段名 -->
      <el-alert
        v-if="p.OverBudget"
        type="warning"
        :closable="false"
        show-icon
        class="rp-alert"
        title="参数快照超出体积预算，非受保护参数已整体裁掉"
      />
      <div v-if="p.Dropped.length || omittedKeys.length || redactedKeys.length" class="rp-clip">
        <div v-if="omittedKeys.length" class="rp-clip-line">
          <span class="rp-clip-tag is-omitted">值已省略</span>
          <span class="rp-clip-text mono">{{ omittedKeys.join('、') }}</span>
        </div>
        <div v-if="p.Dropped.length" class="rp-clip-line">
          <span class="rp-clip-tag is-dropped">已裁剪</span>
          <span class="rp-clip-text mono">{{ p.Dropped.join('、') }}</span>
          <span class="rp-clip-hint">（超 8KB 预算被丢弃，键名保留）</span>
        </div>
        <div v-if="redactedKeys.length" class="rp-clip-line">
          <span class="rp-clip-tag is-redacted">已脱敏</span>
          <span class="rp-clip-text mono">{{ redactedKeys.join('、') }}</span>
          <span class="rp-clip-hint">（命中敏感键，值不落库）</span>
        </div>
      </div>

      <!-- 组一：请求（入站） -->
      <div class="rp-group">
        <div class="rp-group-title">请求</div>
        <div class="rp-grid">
          <div class="rp-row">
            <span class="rp-key">端点</span>
            <span class="rp-val mono">{{ p.Endpoint || '—' }}</span>
          </div>
          <div class="rp-row">
            <span class="rp-key">模型</span>
            <span class="rp-val">
              {{ p.Model || '—' }}
              <el-tag v-if="p.AutoModelName" size="small" effect="plain" type="success">
                Auto {{ p.AutoModelName }}
              </el-tag>
            </span>
          </div>
          <div class="rp-row">
            <span class="rp-key">流式</span>
            <span class="rp-val">{{ p.Stream ? '是' : '否' }}</span>
          </div>
          <div class="rp-row">
            <span class="rp-key">消息条数</span>
            <span class="rp-val">
              {{ p.MessageCount == null ? '—' : fmtNum(p.MessageCount) }}
              <!-- 消息体超 256KB 时后端从最旧开始裁：条数是原始条数，实际落库的少于它 -->
              <el-tag v-if="p.MessagesTruncated" size="small" effect="plain" type="warning">
                已裁剪，实际保留 {{ p.MessagesTruncated.KeptCount ?? '?' }} 条
              </el-tag>
            </span>
          </div>
        </div>

        <!-- tools 摘要：只有数量与工具名，不含 schema（后端恒为摘要） -->
        <div class="rp-tools">
          <div class="rp-sub-title">
            工具调用（摘要）
            <el-tag v-if="p.Summarized.includes('tools')" size="small" effect="plain" type="info">仅名称，无 schema</el-tag>
          </div>
          <div v-if="p.Tools" class="rp-tools-body">
            <span class="rp-tools-count">共 {{ p.Tools.Count }} 个</span>
            <span v-if="p.Tools.Names.length" class="rp-tools-names mono">
              {{ p.Tools.Names.join('、') }}
              <span v-if="p.Tools.Count > p.Tools.Names.length" class="rp-more">
                …（仅列前 {{ p.Tools.Names.length }} 个）
              </span>
            </span>
            <span v-else class="rp-more">（工具名未解析）</span>
          </div>
          <div v-else class="rp-more">本次请求未携带工具</div>
        </div>

        <!-- 其余入站参数：原值 / omitted:NB / dropped 三态 -->
        <div class="rp-sub-title">
          采样参数
          <span class="rp-count">{{ paramsHint }}</span>
        </div>
        <div v-if="!p.Params.length" class="rp-empty-line">（无其余参数）</div>
        <div v-for="row in p.Params" :key="row.Key" class="rp-param" :class="`st-${row.Status}`">
          <div class="rp-param-head">
            <span class="rp-key mono">{{ row.Key }}</span>
            <el-tag v-if="row.Status === 'omitted'" size="small" effect="plain" type="warning">
              已省略 {{ row.OmittedBytes }}B
            </el-tag>
            <el-tag v-else-if="row.Status === 'dropped'" size="small" effect="plain" type="danger">已裁剪</el-tag>
            <el-tag v-else-if="row.Status === 'redacted'" size="small" effect="plain" type="info">已脱敏</el-tag>
            <span v-else-if="row.Text.length > 1" class="rp-copy" role="button" title="复制该值" @click="copy(row.Text)">
              复制
            </span>
          </div>
          <div v-if="row.Status === 'ok'" class="rp-param-val">
            <pre v-if="row.Long" class="rp-pre mono">{{ row.Text }}</pre>
            <span v-else class="rp-val-text">{{ row.Text }}</span>
          </div>
          <div v-else-if="row.Status === 'omitted'" class="rp-param-val rp-muted">
            值超过单字段上限（2KB），落库时替换为 <span class="mono">{{ row.Text }}</span>，键本身保留
          </div>
          <div v-else-if="row.Status === 'redacted'" class="rp-param-val rp-muted">
            该键命中敏感键黑名单（凭据/身份类），值已置为 <span class="mono">redacted</span>，原文不落库
          </div>
          <div v-else class="rp-param-val rp-muted">该字段因快照超出 8KB 预算被丢弃，仅保留键名</div>
        </div>
      </div>

      <!-- 组二：上游（出站） -->
      <div class="rp-group">
        <div class="rp-group-title">上游</div>
        <div class="rp-grid">
          <div class="rp-row">
            <span class="rp-key">入站协议</span>
            <span class="rp-val mono">{{ p.InboundProtocol || '—' }}</span>
          </div>
          <div class="rp-row">
            <span class="rp-key">上游协议</span>
            <span class="rp-val mono">
              {{ p.UpstreamProtocol || '—' }}
              <el-tag v-if="p.Converted" size="small" effect="plain" type="warning">发生过跨协议转换</el-tag>
            </span>
          </div>
          <div class="rp-row rp-row-wide">
            <span class="rp-key">上游 URL</span>
            <span class="rp-val mono breakable">
              <!-- 开发者端即使后端漏了脱敏也不渲染：上游地址属内部维度（防御纵深） -->
              <template v-if="showInternal && p.UpstreamURL">{{ p.UpstreamURL }}</template>
              <span v-else-if="!showInternal" class="rp-muted">
                不展示（开发者端脱敏，含内部部署映射与供应商地址）
              </span>
              <span v-else class="rp-muted">未记录</span>
            </span>
          </div>
        </div>
      </div>

      <!-- 组三：鉴权 -->
      <div class="rp-group">
        <div class="rp-group-title">鉴权</div>
        <div class="rp-grid">
          <div class="rp-row">
            <span class="rp-key">令牌 ID</span>
            <span class="rp-val mono">{{ p.TokenID || '未记录' }}</span>
          </div>
          <div class="rp-row">
            <span class="rp-key">凭据</span>
            <span class="rp-val rp-muted">凭据不落快照（快照绝不含密钥原文）</span>
          </div>
        </div>
      </div>
    </template>
  </div>
</template>

<style scoped>
.rp {
  min-width: 0;
}
.rp-empty {
  font-size: 13px;
  color: var(--color-text-tertiary);
  padding: 6px 2px;
}
.rp-alert {
  margin-bottom: 10px;
}
.rp-clip {
  border: 1px dashed var(--color-warning);
  border-radius: 8px;
  padding: 8px 10px;
  margin-bottom: 12px;
  background: var(--el-color-warning-light-9, #fef4e5);
  display: grid;
  gap: 4px;
}
.rp-clip-line {
  display: flex;
  align-items: baseline;
  flex-wrap: wrap;
  gap: 6px;
  font-size: 12px;
}
.rp-clip-tag {
  flex-shrink: 0;
  border-radius: 4px;
  padding: 1px 6px;
  font-size: 11px;
  line-height: 1.5;
}
.rp-clip-tag.is-omitted {
  background: #fde5bc;
  color: #b35c00;
}
.rp-clip-tag.is-dropped {
  background: #fcd5d2;
  color: #b42318;
}
.rp-clip-tag.is-redacted {
  background: var(--el-color-info-light-9, #f0f1f4);
  color: var(--color-text-secondary, #667085);
}
.rp-clip-text {
  word-break: break-all;
}
.rp-clip-hint {
  color: var(--color-text-tertiary);
}
.rp-group + .rp-group {
  margin-top: 14px;
}
.rp-group-title {
  font-size: 13px;
  font-weight: 600;
  color: var(--color-text-secondary);
  margin-bottom: 8px;
  padding-left: 8px;
  border-left: 3px solid var(--brand-primary-light-7, #c1c2f8);
}
.rp-grid {
  display: grid;
  grid-template-columns: repeat(2, minmax(0, 1fr));
  gap: 6px 20px;
}
.rp-row {
  display: grid;
  grid-template-columns: 92px minmax(0, 1fr);
  gap: 8px;
  align-items: baseline;
  font-size: 13px;
  padding: 3px 0;
  min-width: 0;
}
.rp-row-wide {
  grid-column: 1 / -1;
}
.rp-key {
  color: var(--color-text-tertiary);
  font-size: 12px;
}
.rp-val {
  color: var(--color-text);
  min-width: 0;
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
}
.breakable {
  word-break: break-all;
  overflow-wrap: anywhere;
}
.rp-muted {
  color: var(--color-text-tertiary);
}
.rp-sub-title {
  font-size: 12px;
  color: var(--color-text-secondary);
  font-weight: 600;
  margin: 12px 0 6px;
  display: flex;
  align-items: center;
  flex-wrap: wrap;
  gap: 6px;
}
.rp-count {
  font-weight: 400;
  color: var(--color-text-tertiary);
}
.rp-tools {
  margin-top: 4px;
}
.rp-tools-body {
  font-size: 13px;
  display: flex;
  flex-wrap: wrap;
  gap: 8px;
  align-items: baseline;
}
.rp-tools-count {
  color: var(--color-text);
  font-weight: 600;
}
.rp-tools-names {
  font-size: 12px;
  color: var(--color-text-secondary);
  word-break: break-all;
  overflow-wrap: anywhere;
}
.rp-more {
  font-size: 12px;
  color: var(--color-text-tertiary);
}
.rp-empty-line {
  font-size: 13px;
  color: var(--color-text-tertiary);
}
.rp-param {
  border: 1px solid var(--color-border);
  border-radius: 8px;
  padding: 7px 10px;
  margin-bottom: 6px;
  min-width: 0;
}
.rp-param.st-omitted {
  border-color: var(--el-color-warning-light-5, #fbc470);
  background: var(--el-color-warning-light-9, #fef4e5);
}
.rp-param.st-dropped {
  border-color: var(--el-color-danger-light-5, #f79b94);
  background: var(--el-color-danger-light-9, #fef0ef);
}
.rp-param.st-redacted {
  border-color: var(--color-border-strong, #dce1ea);
  background: var(--el-fill-color-lighter, #fafbfd);
}
.rp-param-head {
  display: flex;
  align-items: center;
  gap: 8px;
  flex-wrap: wrap;
}
.rp-param-head .rp-key {
  font-weight: 600;
  color: var(--color-text);
  font-size: 12px;
}
.rp-copy {
  margin-left: auto;
  font-size: 11px;
  color: var(--brand-primary);
  cursor: pointer;
}
.rp-param-val {
  margin-top: 4px;
  font-size: 13px;
}
.rp-val-text {
  white-space: pre-wrap;
  word-break: break-word;
  overflow-wrap: anywhere;
}
.rp-pre {
  margin: 0;
  padding: 8px 10px;
  border-radius: 6px;
  background: #101828;
  color: #d0d5dd;
  line-height: 1.5;
  white-space: pre-wrap;
  word-break: break-all;
  max-height: 220px;
  overflow: auto;
  font-size: 12px;
}
.mono {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
}
/* 窄屏：单列、正文 14px、长值不横向撑破 */
@media (max-width: 768px) {
  .rp-grid {
    grid-template-columns: 1fr;
    gap: 2px;
  }
  .rp-row {
    grid-template-columns: 1fr;
    gap: 2px;
    font-size: 14px;
  }
  .rp-param-val,
  .rp-tools-body,
  .rp-clip-line {
    font-size: 14px;
  }
  .rp-pre {
    max-height: 260px;
  }
}
</style>
