import { ref, type Ref } from 'vue'
import type { V2Load } from '@/api/v2'
import type { V2PageState } from '@/components/v2/types'

// V2 页面统一取数：三态（live / pending / error）+ 演示数据开关。
//
// 为什么单独抽出来：这十几页的「未就绪」处理必须完全一致——默认空态、不弹红条、
// 允许显式预览演示数据、退出预览立刻回到真实请求。写在每页里必然走样。
//
// loader 由页面传入（内部通常包一次 `@/api/v2` 的请求）。`load()` 可重复调用，
// 页面切换筛选条件时传新的 loader 即可（内部记住最后一次，刷新按钮复用它）。
export function useV2Load<T>(loader: () => Promise<V2Load<T>>, demo: () => T) {
  const state = ref<V2PageState>('loading')
  // 用 Ref 显式标注（而非 any 化）：模板里会自动解包，脚本里 data.value 拿到的是 T | null
  const data: Ref<T | null> = ref<T | null>(null) as Ref<T | null>
  const error = ref({ message: '', requestId: '' })

  let current: () => Promise<V2Load<T>> = loader

  async function load(next?: () => Promise<V2Load<T>>) {
    if (next) current = next
    state.value = 'loading'
    error.value = { message: '', requestId: '' }
    const res = await current()
    if (res.state === 'live') {
      data.value = res.data
      state.value = 'live'
      return
    }
    data.value = null
    if (res.state === 'pending') {
      state.value = 'pending'
      return
    }
    error.value = { message: res.error?.message || '请求失败', requestId: res.error?.requestId || '' }
    state.value = 'error'
  }

  // 显式进入演示数据（只有用户点击才进；财务页默认绝不显示假数字）
  function showDemo() {
    data.value = demo()
    state.value = 'demo'
  }

  function exitDemo() {
    void load()
  }

  return { state, data, error, load, showDemo, exitDemo }
}
