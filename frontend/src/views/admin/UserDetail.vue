<script setup lang="ts">
import { onMounted, ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { ElMessage, ElMessageBox } from 'element-plus'
import { Delete, EditPen, Lock, Unlock } from '@element-plus/icons-vue'
import {
  getUser,
  listUserFlows,
  listTokens,
  batchDeleteTokens,
  updateUser,
  resetUserPassword,
  resetUserTotp,
  type AdminUser,
  type AdminToken,
} from '@/api/admin'
import type { FlowItem } from '@/api/dev'
import { fmtDate, fmtTime } from '@/utils/format'
import ErrorBubble from '@/components/ErrorBubble.vue'
import WalletChargeDlg from '@/components/WalletChargeDlg.vue'

const route = useRoute()
const router = useRouter()
const id = String(route.params.id)

const loading = ref(false)
const user = ref<AdminUser | null>(null)
const balance = ref(0)
const flows = ref<FlowItem[]>([])
const flowTotal = ref(0)
const flowPage = ref(1)
const flowSize = ref(10)
const errInfo = ref<{ message: string; requestId: string }>({ message: '', requestId: '' })

// Tab2：该用户令牌
const tokens = ref<AdminToken[]>([])
const tokenTotal = ref(0)
const tokenPage = ref(1)
const tokenSize = ref(10)
const tokenLoading = ref(false)

const roleOptions: Record<string, { label: string; type: string }> = {
  ADMIN: { label: '管理员', type: 'danger' },
  DEVELOPER: { label: '开发者', type: 'primary' },
}
const statusOptions: Record<string, { label: string; type: string }> = {
  ACTIVE: { label: '正常', type: 'success' },
  DISABLED: { label: '禁用', type: 'danger' },
}
const pricingOptions: Record<string, { label: string; type: string }> = {
  sale: { label: '按售价', type: 'success' },
  cost: { label: '按成本', type: 'warning' },
}

async function loadUser() {
  loading.value = true
  errInfo.value = { message: '', requestId: '' }
  try {
    const res = await getUser(id)
    user.value = res.Data.User
    balance.value = res.Data.Wallet?.Balance ?? 0
    // 流水表数据一律由 loadFlows() 从 /users/:id/wallet/flows 分页获取，
    // getUser 返回的 recent_flows 仅为最近流水摘要，此处不展示。
  } catch (e: any) {
    errInfo.value = { message: e?.message, requestId: e?.requestId }
  } finally {
    loading.value = false
  }
}

async function loadFlows() {
  try {
    const res = await listUserFlows(id, flowPage.value, flowSize.value)
    flows.value = res.Data.List
    flowTotal.value = res.Data.Total
  } catch {
    flows.value = []
  }
}

async function loadTokens() {
  tokenLoading.value = true
  try {
    const res = await listTokens({ UserID: id, Page: tokenPage.value, Size: tokenSize.value })
    tokens.value = res.Data.List
    tokenTotal.value = res.Data.Total
  } catch {
    tokens.value = []
  } finally {
    tokenLoading.value = false
  }
}

onMounted(() => {
  loadUser()
  loadFlows()
  loadTokens()
})

// 通用保存：角色/状态/计价模式 hover 修改（PUT 需携带全部字段）
async function saveAttr(payload: { Role?: string; Status?: string; PricingMode?: string }, msg: string) {
  const u = user.value
  if (!u) return
  try {
    const res = await updateUser(u.ID, {
      Role: u.Role,
      Status: u.Status,
      PricingMode: u.PricingMode,
      Nickname: u.Nickname || '',
      ...payload,
    })
    if (payload.Role) u.Role = res.Data.Role ?? payload.Role
    if (payload.Status) u.Status = res.Data.Status ?? payload.Status
    if (payload.PricingMode) u.PricingMode = res.Data.PricingMode ?? payload.PricingMode
    ElMessage.success(msg)
  } catch (e: any) {
    ElMessage.error(e?.message || '更新失败')
  }
}

// 昵称编辑（铅笔 → popover）
const nickEditing = ref(false)
const nickDraft = ref('')
function openNickEdit() {
  if (!user.value) return
  nickDraft.value = user.value.Nickname || ''
  nickEditing.value = true
}
async function saveNick() {
  const u = user.value
  if (!u) return
  try {
    const res = await updateUser(u.ID, {
      Role: u.Role,
      Status: u.Status,
      PricingMode: u.PricingMode,
      Nickname: nickDraft.value.trim(),
    })
    u.Nickname = res.Data.Nickname ?? nickDraft.value.trim()
    nickEditing.value = false
    ElMessage.success('昵称已更新')
  } catch (e: any) {
    ElMessage.error(e?.message || '修改昵称失败')
  }
}

// 充值弹窗（统一组件）
const chargeDlg = ref(false)
function openCharge() {
  chargeDlg.value = true
}

// 重置密码弹窗
const resetDlg = ref(false)
const resetPwd = ref('')
const resetPwd2 = ref('')
const resetLoading = ref(false)
function openReset() {
  resetPwd.value = ''
  resetPwd2.value = ''
  resetDlg.value = true
}
async function submitReset() {
  const u = user.value
  if (!u) return
  if (!resetPwd.value) return ElMessage.warning('请输入新密码')
  if (resetPwd.value !== resetPwd2.value) return ElMessage.warning('两次输入的密码不一致')
  resetLoading.value = true
  try {
    await resetUserPassword(u.ID, resetPwd.value)
    ElMessage.success(`已重置「${u.Username}」的密码`)
    resetDlg.value = false
  } catch (e: any) {
    ElMessage.error(e?.message || '重置密码失败')
  } finally {
    resetLoading.value = false
  }
}

// 积分数值（千分位，最多 4 位小数）
function fmtCredits(n?: number): string {
  if (n == null || isNaN(n)) return '-'
  return n.toLocaleString(undefined, { maximumFractionDigits: 4 })
}
// 流水类型
const flowType = (t: string) =>
  ({ recharge: { label: '充值', type: 'success' }, consume: { label: '消费', type: 'warning' }, adjust: { label: '差额', type: 'info' }, set: { label: '设置', type: 'primary' } }[t] || { label: t, type: 'info' })
// 系统生成备注规范化：新口径为「实际调用差额」「预扣费」；同时兼容历史英文与旧中文 remark。
const FLOW_REMARK_MAP: Record<string, string> = {
  '实际调用差额': '实际调用差额',
  '预扣费': '预扣费',
  gateway: '实际调用差额',
  '网关调用': '实际调用差额',
  'gateway preconsume': '预扣费',
  '网关预扣费': '预扣费',
  probe: '健康探测',
  'gateway refund settle': '结算退回',
  'gateway refund settle hold': '结算退回',
  '结算退回': '结算退回',
  'gateway refund rate limited, all candidates failed': '请求失败退回（限流）',
  'gateway refund no available channel': '请求失败退回（无可用渠道）',
}
function flowRemark(row: FlowItem): string {
  if (!row.Remark) return '-'
  return FLOW_REMARK_MAP[row.Remark] ?? row.Remark
}

// 删除令牌（无单令牌禁用接口，用删除管理即可）
async function onDelToken(row: AdminToken) {
  try {
    await ElMessageBox.confirm(
      `确认删除令牌「${row.DisplayName}」吗？删除后该令牌立即失效。`,
      '删除确认',
      { type: 'warning', confirmButtonText: '删除', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  try {
    await batchDeleteTokens([row.ID])
    ElMessage.success('已删除')
    await loadTokens()
  } catch (e: any) {
    ElMessage.error(e?.message || '删除失败')
  }
}

// 强制解绑 2FA（管理员操作，无需验证码）
const totpResetLoading = ref(false)
async function onResetTotp() {
  const u = user.value
  if (!u) return
  try {
    await ElMessageBox.confirm(
      `确认强制解绑「${u.Nickname || u.Username}」的两步验证（2FA）吗？解绑后该用户可用密码直接登录。`,
      '强制解绑 2FA',
      { type: 'warning', confirmButtonText: '解绑', cancelButtonText: '取消' },
    )
  } catch {
    return
  }
  totpResetLoading.value = true
  try {
    await resetUserTotp(u.ID)
    ElMessage.success('已强制解绑两步验证')
  } catch (e: any) {
    ElMessage.error(e?.message || '解绑失败')
  } finally {
    totpResetLoading.value = false
  }
}
</script>

<template>
  <div class="page-container">
    <div v-loading="loading" class="card">
      <div v-if="user">
        <div class="detail-head">
          <el-tabs>
            <!-- Tab1 基本信息 + 钱包 -->
            <el-tab-pane label="基本信息 / 钱包">
              <el-descriptions :column="2" border>
                <el-descriptions-item label="昵称">
                  <span class="nick-cell">
                    {{ user.Nickname || '-' }}
                    <el-popover
                      :visible="nickEditing"
                      trigger="click"
                      placement="bottom-start"
                      :width="220"
                      @hide="nickEditing = false"
                    >
                      <template #reference>
                        <el-icon class="nick-edit" @click="openNickEdit()"><EditPen /></el-icon>
                      </template>
                      <el-input v-model="nickDraft" maxlength="40" placeholder="设置昵称" @keyup.enter="saveNick" />
                      <div class="nick-actions">
                        <el-button size="small" type="primary" @click="saveNick">保存</el-button>
                      </div>
                    </el-popover>
                  </span>
                </el-descriptions-item>
                <el-descriptions-item label="用户名">{{ user.Username }}</el-descriptions-item>
                <el-descriptions-item label="角色">
                  <el-popover trigger="hover" placement="bottom-start" :width="180">
                    <template #reference>
                      <el-tag :type="(roleOptions[user.Role]?.type as any) || 'info'" size="small" effect="light" class="hover-tag">
                        {{ roleOptions[user.Role]?.label || user.Role }}
                      </el-tag>
                    </template>
                    <div class="pop-section">
                      <span class="pop-label">修改角色</span>
                      <div class="pop-actions">
                        <el-button v-if="user.Role !== 'ADMIN'" size="small" type="danger" plain @click="saveAttr({ Role: 'ADMIN' }, '已设为管理员')">管理员</el-button>
                        <el-button v-if="user.Role !== 'DEVELOPER'" size="small" type="primary" plain @click="saveAttr({ Role: 'DEVELOPER' }, '已设为开发者')">开发者</el-button>
                      </div>
                    </div>
                  </el-popover>
                </el-descriptions-item>
                <el-descriptions-item label="状态">
                  <el-popover trigger="hover" placement="bottom-start" :width="180">
                    <template #reference>
                      <el-tag :type="(statusOptions[user.Status]?.type as any) || 'info'" size="small" effect="light" class="hover-tag">
                        {{ statusOptions[user.Status]?.label || user.Status }}
                      </el-tag>
                    </template>
                    <div class="pop-section">
                      <span class="pop-label">修改状态</span>
                      <div class="pop-actions">
                        <el-button v-if="user.Status !== 'ACTIVE'" size="small" type="success" plain @click="saveAttr({ Status: 'ACTIVE' }, '已启用')">正常</el-button>
                        <el-button v-if="user.Status !== 'DISABLED'" size="small" type="danger" plain @click="saveAttr({ Status: 'DISABLED' }, '已禁用')">禁用</el-button>
                      </div>
                    </div>
                  </el-popover>
                </el-descriptions-item>
                <el-descriptions-item label="计价模式">
                  <el-popover trigger="hover" placement="bottom-start" :width="180">
                    <template #reference>
                      <el-tag :type="(pricingOptions[user.PricingMode]?.type as any) || 'info'" size="small" effect="light" class="hover-tag">
                        {{ pricingOptions[user.PricingMode]?.label || user.PricingMode }}
                      </el-tag>
                    </template>
                    <div class="pop-section">
                      <span class="pop-label">修改计价模式</span>
                      <div class="pop-actions">
                        <el-button v-if="user.PricingMode !== 'sale'" size="small" type="success" plain @click="saveAttr({ PricingMode: 'sale' }, '已切换为按售价')">按售价</el-button>
                        <el-button v-if="user.PricingMode !== 'cost'" size="small" type="warning" plain @click="saveAttr({ PricingMode: 'cost' }, '已切换为按成本')">按成本</el-button>
                      </div>
                    </div>
                  </el-popover>
                </el-descriptions-item>
                <el-descriptions-item label="操作">
                  <el-tooltip content="重置密码" placement="top">
                    <el-icon class="op-icon" @click="openReset"><Lock /></el-icon>
                  </el-tooltip>
                  <el-tooltip content="强制解绑两步验证（2FA）" placement="top">
                    <el-icon class="op-icon" :class="{ 'is-loading': totpResetLoading }" @click="onResetTotp"><Unlock /></el-icon>
                  </el-tooltip>
                </el-descriptions-item>
              </el-descriptions>

              <!-- 钱包卡 -->
              <div class="wallet-head">
                <div class="balance-card">
                  <div class="balance-label">当前余额（积分）</div>
                  <div class="balance-value">{{ fmtCredits(balance) }}</div>
                </div>
                <div class="balance-card spent">
                  <div class="balance-label">累计消费（积分）</div>
                  <div class="balance-value">{{ fmtCredits(user.TotalSpent) }}</div>
                </div>
                <div class="wallet-ops">
                  <el-button type="primary" @click="openCharge()">充值</el-button>
                </div>
              </div>

              <!-- 流水表：时间 / 类型 / 会话 / 备注 / 金额（金额+余额双行） -->
              <div class="sub-title">积分流水（最近 {{ flowSize }} 条）</div>
              <el-table :data="flows" border stripe class="table-nowrap">
                <el-table-column label="时间" min-width="150">
                  <template #default="{ row }">
                    <div class="t-time">
                      <span class="t-date">{{ fmtDate(row.CreatedAt) }}</span>
                      <span class="t-clock">{{ fmtTime(row.CreatedAt) }}</span>
                    </div>
                  </template>
                </el-table-column>
                <el-table-column label="类型" width="90" align="center">
                  <template #default="{ row }">
                    <el-tag :type="(flowType(row.Type).type as any)" size="small" effect="light">
                      {{ flowType(row.Type).label }}
                    </el-tag>
                  </template>
                </el-table-column>
                <el-table-column label="会话" min-width="140" show-overflow-tooltip>
                  <template #default="{ row }">
                    <span v-if="row.SessionName">{{ row.SessionName }}</span>
                    <span v-else class="t-clock">-</span>
                  </template>
                </el-table-column>
                <el-table-column label="备注" min-width="140" show-overflow-tooltip>
                  <template #default="{ row }">{{ flowRemark(row) }}</template>
                </el-table-column>
                <el-table-column label="金额" width="140" align="right">
                  <template #default="{ row }">
                    <div class="t-time amount-cell">
                      <span :class="row.Amount >= 0 ? 'pos' : 'neg'">
                        {{ row.Amount >= 0 ? '+' : '' }}{{ fmtCredits(row.Amount) }}
                      </span>
                      <span class="t-clock">余额 {{ fmtCredits(row.Balance) }}</span>
                    </div>
                  </template>
                </el-table-column>
              </el-table>
              <el-empty v-if="!flows.length" description="暂无流水" />
              <div class="pager">
                <el-pagination
                  layout="total, prev, pager, next"
                  :total="flowTotal"
                  :page-size="flowSize"
                  :current-page="flowPage"
                  @current-change="(p: number) => { flowPage = p; loadFlows() }"
                />
              </div>
            </el-tab-pane>

            <!-- Tab2 该用户令牌 -->
            <el-tab-pane label="该用户令牌">
              <div v-loading="tokenLoading">
                <el-table :data="tokens" border stripe class="table-nowrap">
                  <el-table-column label="名称" prop="DisplayName" min-width="130" show-overflow-tooltip />
                  <el-table-column label="标识" min-width="180">
                    <template #default="{ row }">
                      <el-tooltip :content="row.TokenDisplay" placement="top">
                        <span class="mono">{{ row.TokenDisplay }}</span>
                      </el-tooltip>
                    </template>
                  </el-table-column>
                  <el-table-column label="状态" width="90" align="center">
                    <template #default="{ row }">
                      <el-tag :type="row.Status === 'ACTIVE' ? 'success' : 'danger'" size="small" effect="light">
                        {{ row.Status === 'ACTIVE' ? '启用' : '禁用' }}
                      </el-tag>
                    </template>
                  </el-table-column>
                  <el-table-column label="过期时间" min-width="150">
                    <template #default="{ row }">{{ row.ExpiresAt || '永不过期' }}</template>
                  </el-table-column>
                  <el-table-column label="操作" width="90" align="right" fixed="right">
                    <template #default="{ row }">
                      <div class="op-cell" @click.stop>
                        <el-tooltip content="删除" placement="top">
                          <el-icon class="op-icon danger" @click="onDelToken(row)"><Delete /></el-icon>
                        </el-tooltip>
                      </div>
                    </template>
                  </el-table-column>
                </el-table>
                <el-empty v-if="!tokenLoading && !tokens.length" description="该用户暂无令牌" />
                <div class="pager">
                  <el-pagination
                    layout="total, prev, pager, next"
                    :total="tokenTotal"
                    :page-size="tokenSize"
                    :current-page="tokenPage"
                    @current-change="(p: number) => { tokenPage = p; loadTokens() }"
                  />
                </div>
              </div>
            </el-tab-pane>

            <!-- Tab3 该用户账单 -->
            <el-tab-pane label="该用户账单">
              <el-result icon="info" title="跳转账单查询" sub-title="查看该用户的全部调用账单记录。">
                <template #extra>
                  <el-button type="primary" @click="router.push({ path: '/admin/billings', query: { user_id: String(user.ID) } })">
                    前往账单查询
                  </el-button>
                </template>
              </el-result>
            </el-tab-pane>
          </el-tabs>
        </div>
      </div>

      <ErrorBubble
        v-if="errInfo.message || errInfo.requestId"
        :message="errInfo.message"
        :request-id="errInfo.requestId"
      />
    </div>

    <!-- 充值弹窗（统一组件） -->
    <WalletChargeDlg
      v-model="chargeDlg"
      :user-id="user?.ID ?? ''"
      :username="user?.Username || ''"
      :nickname="user?.Nickname"
      :balance="balance"
      @success="() => { loadUser(); loadFlows() }"
    />

    <!-- 重置密码弹窗 -->
    <el-dialog v-model="resetDlg" title="重置密码" width="420px" :close-on-click-modal="false">
      <el-form label-width="90px">
        <el-form-item label="用户">
          <span>{{ user?.Nickname || user?.Username }}</span>
          <span v-if="user?.Nickname" class="dlg-sub">@{{ user?.Username }}</span>
        </el-form-item>
        <el-form-item label="新密码">
          <el-input v-model="resetPwd" type="password" show-password placeholder="输入新登录密码" />
        </el-form-item>
        <el-form-item label="确认密码">
          <el-input v-model="resetPwd2" type="password" show-password placeholder="再次输入新密码" @keyup.enter="submitReset" />
        </el-form-item>
      </el-form>
      <template #footer>
        <el-button @click="resetDlg = false">取消</el-button>
        <el-button type="primary" :loading="resetLoading" @click="submitReset">确认重置</el-button>
      </template>
    </el-dialog>
  </div>
</template>

<style scoped>
.mono {
  font-family: 'SF Mono', Menlo, Consolas, monospace;
  font-size: 12px;
}
.nick-cell {
  display: inline-flex;
  align-items: center;
  gap: 6px;
}
.nick-edit {
  cursor: pointer;
  color: var(--color-text-tertiary);
  font-size: 13px;
  transition: color 0.15s;
}
.nick-edit:hover {
  color: var(--brand-primary);
}
.nick-actions {
  display: flex;
  justify-content: flex-end;
  margin-top: 8px;
}
.hover-tag {
  cursor: pointer;
}
.pop-section {
  display: flex;
  flex-direction: column;
  gap: 8px;
}
.pop-label {
  font-size: 12px;
  color: var(--color-text-secondary);
}
.pop-actions {
  display: flex;
  gap: 8px;
  flex-wrap: wrap;
}
.op-icon {
  cursor: pointer;
  color: var(--color-text-secondary);
  transition: color 0.15s;
}
.op-icon.is-loading {
  opacity: 0.5;
  pointer-events: none;
  animation: op-spin 1s linear infinite;
}
@keyframes op-spin {
  from { transform: rotate(0deg); }
  to { transform: rotate(360deg); }
}
.op-icon:hover {
  color: var(--brand-primary);
}
.op-icon.danger:hover {
  color: var(--color-danger);
}
.op-cell {
  display: flex;
  align-items: center;
  justify-content: flex-end;
}
.wallet-head {
  display: flex;
  align-items: center;
  gap: 24px;
  margin: 16px 0;
}
.balance-card {
  background: var(--color-primary-light);
  border-radius: 8px;
  padding: 18px 24px;
  min-width: 200px;
}
.balance-card.spent {
  background: var(--color-bg-muted, #f5f5f5);
  color: var(--color-text);
}
.balance-label {
  font-size: 13px;
  color: var(--color-text-secondary);
}
.balance-value {
  font-size: 30px;
  font-weight: 700;
  color: var(--color-primary);
  font-variant-numeric: tabular-nums;
}
.wallet-ops {
  display: flex;
  gap: 10px;
}
.sub-title {
  font-size: 15px;
  font-weight: 600;
  margin: 12px 0;
}
.t-time {
  display: flex;
  flex-direction: column;
  line-height: 1.35;
}
.t-date {
  font-size: 12px;
  color: var(--color-text);
  font-variant-numeric: tabular-nums;
}
.t-clock {
  font-size: 12px;
  color: var(--color-text-tertiary);
  font-variant-numeric: tabular-nums;
}
.amount-cell {
  align-items: flex-end;
}
.pos {
  color: var(--color-success);
}
.neg {
  color: var(--color-danger);
}
.dlg-sub {
  font-size: 12px;
  color: var(--color-text-tertiary);
  margin-left: 4px;
}
.pager {
  display: flex;
  justify-content: flex-end;
  margin-top: 16px;
}
</style>