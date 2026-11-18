// mock 总开关（Spec `2026-11-16-stage3-frontend-13-pages.md` §5 规则 2/3）：
//   `VITE_MOCK=1` 时用 `src/mock/<page>.ts` 的数据渲染，并在页面顶部固定黄条「演示数据 · 接口待接入」；
//   接口就绪后**同一 PR 内删掉对应 mock 文件与本开关分支**，不留死代码。
//
// 与「显式点按钮才看演示数据」的关系：两者并存——
//   VITE_MOCK=1（演示环境）→ 默认就是演示数据；
//   生产/联调（未设该变量）→ 接口没上线时出空态，只有用户主动点「预览演示数据」才看 mock（财务页不许默认显示假数字）。
export const MOCK_ENABLED = import.meta.env.VITE_MOCK === '1'
