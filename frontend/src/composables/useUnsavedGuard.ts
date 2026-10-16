import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import { onBeforeRouteLeave } from 'vue-router'
import { ElMessageBox } from 'element-plus'

// 未保存修改守卫：统一「脏检查 + 离开二次确认 + 浏览器关闭拦截」。
// getSnapshot 返回「提交口径」的可序列化快照字符串；数据加载/保存成功后调用 markSaved() 重建基线。
// 基线未建立（例如编辑页数据加载失败）时不判定为脏，避免误报「有未保存的修改」。
export function useUnsavedGuard(getSnapshot: () => string) {
  const savedSnapshot = ref<string | null>(null)
  const dirty = computed(() => savedSnapshot.value !== null && getSnapshot() !== savedSnapshot.value)

  function markSaved() {
    savedSnapshot.value = getSnapshot()
  }

  function onBeforeUnload(e: BeforeUnloadEvent) {
    if (!dirty.value) return
    e.preventDefault()
    e.returnValue = ''
  }

  onMounted(() => window.addEventListener('beforeunload', onBeforeUnload))
  onBeforeUnmount(() => window.removeEventListener('beforeunload', onBeforeUnload))

  onBeforeRouteLeave(async () => {
    if (!dirty.value) return true
    try {
      await ElMessageBox.confirm('当前有未保存的修改，离开将丢失，确定离开吗？', '未保存的修改', {
        type: 'warning',
        confirmButtonText: '放弃修改并离开',
        cancelButtonText: '留在本页',
      })
      return true
    } catch {
      return false
    }
  })

  return { dirty, markSaved }
}
