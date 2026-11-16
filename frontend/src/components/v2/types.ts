// V2 运营台共用类型（script setup 里不能导出类型，类型集中放这里）。
//
// V2PageState：页面四态 + 演示态。刻意区分 pending（接口未上线）与 error（真出错），
// 因为前者不该弹红条、也不该显示 0，后者要带 requestId 让人能查日志。
export type V2PageState = 'loading' | 'live' | 'pending' | 'error' | 'demo'

// V2Dep：页面依赖的接口 + 就绪度，渲染在页头，方便和后端/测试对齐「这页现在能不能用」。
export interface V2Dep {
  path: string
  state: 'ready' | 'pending'
}

// StatItem：指标卡一项。tone 只表达「要不要引起注意」，不表达数值好坏判断（那是页面的事）。
export interface StatItem {
  label: string
  value: string
  // 次级说明：公式、口径、或「> 0 要警惕」的原因
  hint?: string
  tone?: 'default' | 'ok' | 'warn' | 'danger'
}
