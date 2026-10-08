<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { RefreshRight, Key, Link, Plus } from '@element-plus/icons-vue'
import { listDevTokens, toggleDevToken, rotateDevToken, createDevToken, getDevTokenSecret, listDevTags, type DevToken, type DevTag } from '@/api/dev'
import ErrorBubble from '@/components/ErrorBubble.vue'

const router = useRouter()
const loading = ref(false)
const list = ref<DevToken[]>([])
const total = ref(0)
const page = ref(1)
const size = ref(20)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

// 标签名映射：tag_id → name（令牌列表展示用）
const tagNameMap = ref<Record<string, string>>({})

// 新建令牌弹窗
const createOpen = ref(false)
const createSaving = ref(false)
const createErr = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })
const tagOptions = ref<{ value: string | null; label: string }[]>([{ value: null, label: '不选择（走默认路由）' }])
const createForm = ref({ DisplayName: '', TagID: null as string | null, ExpiresAt: null as string | null })
const createdPlain = ref<{ Plain: string; Display: string } | null>(null)

// 查看密钥弹窗
const secretOpen = ref(false)
const secretLoading = ref(false)
const secretPlain = ref<{ Plain: string; Display: string } | null>(null)
const secretName = ref('')

// 轮换后的明文展示
const rotateDialog = ref(false)
const rotatePlain = ref('')
const rotateDisplay = ref('')

async function loadTags() {
  try {
    const res = await listDevTags()
    const tags: DevTag[] = res.Data || []
    tagNameMap.value = Object.fromEntries(tags.map((t) => [t.ID, t.Name]))
    tagOptions.value = [
      { value: null, label: '不选择（走默认路由）' },
      ...tags.filter((t) => t.Enabled !== false).map((t) => ({ value: t.ID, label: t.Name })),
    ]
  } catch {
    // 忽略，仅影响标签列展示
  }
}

function tagLabel(row: DevToken): string {
  if (!row.TagID) return '默认路由'
  return tagNameMap.value[row.TagID] ? `标签：${tagNameMap.value[row.TagID]}` : '标签'
}

async function load() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await listDevTokens(page.value, size.value)
    list.value = res.Data.List
    total.value = res.Data.Total
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}

onMounted(() => {
  load()
  loadTags()
})

// 行内状态切换（实时反馈，无需刷新整页）
async function onToggle(row: DevToken) {
  try {
    const res = await toggleDevToken(row.ID)
    row.Status = res.Data.Status
    ElMessage.success(res.Data.Status === 'ACTIVE' ? '已启用' : '已禁用')
  } catch (e: any) {
    ElMessage.error(e?.message || '操作失败')
  }
}

// 轮换：作废旧令牌并换发新令牌
async function onRotate(row: DevToken) {
  try {
    const res = await rotateDevToken(row.ID)
    rotatePlain.value = res.Data.Plain
    rotateDisplay.value = res.Data.Display
    rotateDialog.value = true
    await load()
  } catch (e: any) {
    ElMessage.error(e?.message || '轮换失败')
  }
}

// ---- 查看密钥（可反复复制；敏感操作需口令二次验证） ----
async function onViewSecret(row: DevToken) {
  let pwd = ''
  try {
    const r = await ElMessageBox.prompt('查看明文密钥需要验证当前登录口令', '口令验证', {
      inputType: 'password',
      inputPlaceholder: '当前口令',
    })
    pwd = r.value
  } catch {
    return
  }
  secretOpen.value = true
  secretLoading.value = true
  secretName.value = row.DisplayName
  secretPlain.value = null
  try {
    const res = await getDevTokenSecret(row.ID, pwd)
    secretPlain.value = res.Data
  } catch (e: any) {
    secretPlain.value = null
    ElMessage.error(e?.message || '查看密钥失败')
  } finally {
    secretLoading.value = false
  }
}

// ---- 新建令牌（弹窗） ----
function openCreate() {
  createOpen.value = true
  createdPlain.value = null
  createForm.value = { DisplayName: '', TagID: null, ExpiresAt: null }
  createErr.value = { message: '', requestId: '' }
}

async function submitCreate() {
  if (!createForm.value.DisplayName.trim()) return ElMessage.warning('请输入令牌名称')
  createSaving.value = true
  createErr.value = { message: '', requestId: '' }
  try {
    const res = await createDevToken({
      DisplayName: createForm.value.DisplayName.trim(),
      TagID: createForm.value.TagID,
      ExpiresAt: createForm.value.ExpiresAt,
    })
    createdPlain.value = res.Data
    ElMessage.success('令牌已创建')
    await load()
  } catch (e: any) {
    createErr.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    createSaving.value = false
  }
}

async function copyText(text: string) {
  try {
    await navigator.clipboard.writeText(text)
    ElMessage.success('密钥已复制')
  } catch {
    ElMessage.warning('复制失败，请手动选择复制')
  }
}

// 复制 API Base URL（网关 OpenAI 兼容入口，前端同域时取 location.origin）
function copyBaseUrl() {
  const url = `${window.location.origin}/v1`
  navigator.clipboard.writeText(url)
  ElMessage.success('Base URL 已复制：' + url)
}
</script>

<template>
  <div class="page-container">
    <div class="page-header">
      <div class="page-title">令牌</div>
      <span class="page-count">共 {{ total }} 个令牌</span>
      <div class="page-header-spacer" />
      <el-tooltip content="复制本网关 OpenAI 兼容 API 地址（/v1），开发者按此地址拼接令牌调用" placement="top">
        <el-button :icon="Link" plain @click="copyBaseUrl">复制 Base URL</el-button>
      </el-tooltip>
      <el-button type="primary" :icon="Plus" @click="openCreate">新建令牌</el-button>
    </div>

    <div v-loading="loading" class="card">
      <el-table
        :data="list"
        border
        stripe
        class="table-nowrap clickable-rows"
        row-key="ID"
        @selection-change="() => {}"
        @row-click="(row: DevToken, _c: unknown, e: Event) => !(e.target as HTMLElement)?.closest('.op-cell') && router.push(`/dev/tokens/${row.ID}`)"
      >
        <el-table-column label="名称" prop="DisplayName" min-width="140" show-overflow-tooltip />
        <el-table-column label="标识" min-width="200" show-overflow-tooltip>
          <template #default="{ row }">
            <el-tooltip :content="row.TokenDisplay" placement="top">
              <span class="mono">{{ row.TokenDisplay }}</span>
            </el-tooltip>
          </template>
        </el-table-column>
        <el-table-column label="路由" min-width="120">
          <template #default="{ row }">
            <el-tag v-if="!row.TagID" size="small" type="info" effect="plain">默认路由</el-tag>
            <el-tag v-else size="small" effect="plain">{{ tagLabel(row) }}</el-tag>
          </template>
        </el-table-column>
        <el-table-column label="状态" width="90" align="center">
          <template #default="{ row }">
            <el-switch
              :model-value="row.Status === 'ACTIVE'"
              inline-prompt
              active-text="启"
              inactive-text="禁"
              @change="onToggle(row)"
            />
          </template>
        </el-table-column>
        <el-table-column label="最近使用" min-width="170">
          <template #default="{ row }">
            <template v-if="row.LastUsedAt">{{ row.LastUsedAt }}</template>
            <span v-else class="never">从未使用</span>
          </template>
        </el-table-column>
        <el-table-column label="过期时间" min-width="150">
          <template #default="{ row }">
            {{ row.ExpiresAt || '永不过期' }}
          </template>
        </el-table-column>
        <el-table-column label="操作" width="130" align="right" fixed="right">
          <template #default="{ row }">
            <div class="op-cell" @click.stop>
              <el-tooltip content="查看密钥" placement="top">
                <el-icon class="op-icon" @click="onViewSecret(row)"><Key /></el-icon>
              </el-tooltip>
              <el-tooltip content="轮换密钥" placement="top">
                <el-icon class="op-icon" @click="onRotate(row)"><RefreshRight /></el-icon>
              </el-tooltip>
            </div>
          </template>
        </el-table-column>
      </el-table>

      <div class="pager">
        <el-pagination
          layout="total, prev, pager, next"
          :total="total"
          :page-size="size"
          :current-page="page"
          @current-change="(p: number) => { page = p; load() }"
        />
      </div>

      <ErrorBubble
        v-if="errInfo.message || errInfo.requestId"
        :message="errInfo.message"
        :request-id="errInfo.requestId"
      />
    </div>

    <!-- 新建令牌弹窗 -->
    <el-dialog v-model="createOpen" title="新建令牌" width="520px" :close-on-click-modal="false" destroy-on-close>
      <template v-if="createdPlain">
        <div class="plain-warning">令牌已创建，请复制并妥善保存。关闭后仍可在列表中随时查看/复制。</div>
        <el-input :model-value="createdPlain.Plain" readonly class="mono">
          <template #append>
            <el-button text @click="copyText(createdPlain.Plain)">复制</el-button>
          </template>
        </el-input>
        <div class="plain-tip">标识：<span class="mono">{{ createdPlain.Display }}</span></div>
      </template>
      <el-form v-else label-width="110px">
        <el-form-item label="名称" required>
          <el-input v-model="createForm.DisplayName" placeholder="例：生产环境 Key" maxlength="50" show-word-limit />
        </el-form-item>
        <el-form-item label="语义标签">
          <el-select v-model="createForm.TagID" placeholder="选择标签" style="width: 100%">
            <el-option
              v-for="opt in tagOptions"
              :key="String(opt.value)"
              :label="opt.label"
              :value="opt.value"
            />
          </el-select>
          <div class="form-tip">未选择时令牌走默认路由。</div>
        </el-form-item>
        <el-form-item label="过期时间">
          <el-date-picker
            v-model="createForm.ExpiresAt"
            type="datetime"
            placeholder="留空则永不过期"
            value-format="YYYY-MM-DDTHH:mm:00Z"
            format="YYYY-MM-DD HH:mm"
            style="width: 100%"
          />
        </el-form-item>
      </el-form>
      <div v-if="createErr.message || createErr.requestId" style="margin-bottom: 8px">
        <ErrorBubble :message="createErr.message" :request-id="createErr.requestId" />
      </div>
      <template #footer>
        <template v-if="createdPlain">
          <el-button type="primary" @click="createOpen = false">完成</el-button>
        </template>
        <template v-else>
          <el-button @click="createOpen = false">取消</el-button>
          <el-button type="primary" :loading="createSaving" @click="submitCreate">创建令牌</el-button>
        </template>
      </template>
    </el-dialog>

    <!-- 查看密钥弹窗 -->
    <el-dialog v-model="secretOpen" :title="`查看密钥 - ${secretName}`" width="520px" :close-on-click-modal="false">
      <div v-loading="secretLoading">
        <template v-if="secretPlain">
          <el-input :model-value="secretPlain.Plain" readonly class="mono">
            <template #append>
              <el-button text @click="copyText(secretPlain.Plain)">复制</el-button>
            </template>
          </el-input>
          <div class="plain-tip">密钥可随时反复查看/复制。如怀疑泄露，可轮换或删除后新建。</div>
        </template>
        <el-empty v-else-if="!secretLoading" description="该令牌未备份密钥，可轮换后查看新密钥" :image-size="60" />
      </div>
      <template #footer>
        <el-button @click="secretOpen = false">关闭</el-button>
      </template>
    </el-dialog>

    <!-- 轮换新明文 -->
    <el-dialog v-model="rotateDialog" title="令牌已轮换" width="480px" :close-on-click-modal="false">
      <div class="plain-warning">
        新令牌已生效，请复制保存。之后可在列表中随时查看/复制。
      </div>
      <el-input :model-value="rotatePlain" readonly class="mono">
        <template #append>
          <el-button text @click="copyText(rotatePlain)">复制</el-button>
        </template>
      </el-input>
      <template #footer>
        <el-button type="primary" @click="rotateDialog = false">完成</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.mono {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
  font-size: 12px;
}
.pager {
  display: flex;
  justify-content: flex-end;
  margin-top: 16px;
}
.plain-warning {
  color: var(--color-warning);
  font-size: 13px;
  margin-bottom: 12px;
}
.plain-tip {
  font-size: 12px;
  color: var(--color-text-secondary);
  margin-top: 8px;
}
.form-tip {
  font-size: 12px;
  color: var(--color-text-secondary);
  line-height: 1.6;
  margin-top: 4px;
}
.never {
  color: var(--color-text-tertiary);
}
</style>