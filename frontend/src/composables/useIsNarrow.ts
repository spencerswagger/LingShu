import { onBeforeUnmount, onMounted, ref } from 'vue'

// 窄屏判定（移动端适配用）：断点与全站既有媒体查询保持一致（768px 为主、900px 为表单回落）。
// 用 matchMedia 而不是 resize 事件：手机旋转/浏览器地址栏收放只触发一次回调，开销更小。
// 用途：需要按宽度切换**结构**（表格 ↔ 卡片、descriptions 双列 ↔ 单列）的页面；
// 纯样式差异仍走 CSS 媒体查询，不要为此引入 JS 分支。
const QUERIES = {
  // 手机/窄平板：表格转卡片、descriptions 单列
  narrow: '(max-width: 768px)',
  // 中等宽度：表单双列回落单列（与 theme.css 的 .me-grid 断点同源）
  medium: '(max-width: 900px)',
} as const

export type NarrowKind = keyof typeof QUERIES

/**
 * 返回响应式的窄屏标志。
 * kind='narrow'（默认，≤768px）用于「表格转卡片」这类结构切换；
 * kind='medium'（≤900px）用于表单/描述列表的双列回落。
 */
export function useIsNarrow(kind: NarrowKind = 'narrow') {
  const isNarrow = ref(false)
  let mql: MediaQueryList | null = null

  const sync = () => {
    isNarrow.value = mql ? mql.matches : false
  }

  onMounted(() => {
    // SSR / 非浏览器环境（无 matchMedia）时保持 false，渲染桌面结构
    if (typeof window === 'undefined' || typeof window.matchMedia !== 'function') return
    mql = window.matchMedia(QUERIES[kind])
    sync()
    // Safari < 14 只有 addListener；新旧都挂上，重复触发无副作用（赋值幂等）
    if (typeof mql.addEventListener === 'function') mql.addEventListener('change', sync)
    else if (typeof mql.addListener === 'function') mql.addListener(sync)
  })

  onBeforeUnmount(() => {
    if (!mql) return
    if (typeof mql.removeEventListener === 'function') mql.removeEventListener('change', sync)
    else if (typeof mql.removeListener === 'function') mql.removeListener(sync)
    mql = null
  })

  return isNarrow
}
