// 多图表共享一个 window resize 监听。
// 统计页的图表容器不止一个（趋势 / 构成 / Top 密钥…），每个实例各挂一个 window
// resize 监听既浪费也会在同一帧里重复重排；这里收敛成模块级单例：
// 第一个订阅者装上监听，最后一个退订时移除（rAF 合并同一帧内的抖动）。

type ResizeHandler = () => void

const handlers = new Set<ResizeHandler>()
let raf = 0

function onWindowResize() {
  if (raf) return
  raf = requestAnimationFrame(() => {
    raf = 0
    for (const h of handlers) h()
  })
}

// 打标记仅为自测/排查时能把这一个监听和第三方（element-plus popper 等）的 resize 监听区分开
;(onWindowResize as unknown as { __sharedWindowResize?: boolean }).__sharedWindowResize = true

// 返回退订函数；调用方在组件卸载时执行它（幂等，重复调用不会误删别人的订阅）。
export function subscribeWindowResize(handler: ResizeHandler): () => void {
  if (handlers.size === 0) window.addEventListener('resize', onWindowResize)
  handlers.add(handler)
  let done = false
  return () => {
    if (done) return
    done = true
    handlers.delete(handler)
    if (handlers.size === 0) {
      window.removeEventListener('resize', onWindowResize)
      if (raf) {
        cancelAnimationFrame(raf)
        raf = 0
      }
    }
  }
}
