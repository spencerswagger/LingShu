// 会话成本路由 · 更新方案评审 —— 图表与图元
(function () {
  var style = getComputedStyle(document.documentElement);
  var accent = style.getPropertyValue('--accent').trim();
  var accent2 = style.getPropertyValue('--accent2').trim();
  var ink = style.getPropertyValue('--ink').trim();
  var muted = style.getPropertyValue('--muted').trim();
  var rule = style.getPropertyValue('--rule').trim();
  var bg2 = style.getPropertyValue('--bg2').trim();

  var axisLabel = { color: muted, fontSize: 12 };
  var splitLine = { lineStyle: { color: rule } };

  function initChart(id) {
    var el = document.getElementById(id);
    if (!el || typeof echarts === 'undefined') return null;
    return echarts.init(el, null, { renderer: 'svg' });
  }

  // ---------- 图 2：N_eff 退化 ----------
  (function () {
    var chart = initChart('chart-neff');
    if (!chart) return;
    var xs = [];
    var neff = [];
    var ideal = [];
    for (var x = 0; x <= 12; x++) {
      xs.push(x);
      neff.push(x < 1 ? 0 : 1);
      ideal.push(x);
    }
    chart.setOption({
      animation: false,
      tooltip: { trigger: 'axis', appendToBody: true },
      legend: { data: ['实际 N_eff = min(1, N̂)', '理想 N_eff = N̂'], bottom: 0, textStyle: { color: ink }, itemGap: 20 },
      grid: { left: 8, right: 18, top: 22, bottom: 46, containLabel: true },
      xAxis: {
        type: 'category',
        data: xs,
        name: '统计剩余轮数 N̂',
        nameLocation: 'middle',
        nameGap: 28,
        nameTextStyle: { color: muted },
        axisLabel: axisLabel,
        axisLine: { lineStyle: { color: rule } }
      },
      yAxis: {
        type: 'value',
        name: 'N_eff',
        nameTextStyle: { color: muted },
        axisLabel: axisLabel,
        splitLine: splitLine,
        max: 12
      },
      series: [
        {
          name: '理想 N_eff = N̂',
          type: 'line',
          step: false,
          symbol: 'none',
          lineStyle: { color: muted, width: 2, type: 'dashed' },
          itemStyle: { color: muted },
          data: ideal
        },
        {
          name: '实际 N_eff = min(1, N̂)',
          type: 'line',
          step: 'end',
          symbol: 'circle',
          symbolSize: 6,
          lineStyle: { color: accent, width: 3 },
          itemStyle: { color: accent },
          data: neff,
          markPoint: {
            symbol: 'pin',
            symbolSize: 46,
            itemStyle: { color: accent },
            label: { color: '#fff', fontSize: 9, formatter: '恒为 1' },
            data: [{ coord: [8, 1] }]
          }
        }
      ]
    });
    window.addEventListener('resize', function () { chart.resize(); });
  })();

  // ---------- 图 3：问题严重度（修订后） ----------
  (function () {
    var chart = initChart('chart-priority');
    if (!chart) return;
    var cats = [
      '门控退化 N_eff',
      '账本移动基准 + 双门控',
      '统计缺模型维度 / off-policy',
      '质量 / 一致性护栏缺失',
      '并发 / 状态 / 重放',
      'P50 / P90 层间混用',
      '创建时不可观测维度'
    ];
    var vals = [95, 88, 82, 74, 66, 58, 52];
    chart.setOption({
      animation: false,
      tooltip: { trigger: 'axis', appendToBody: true, axisPointer: { type: 'shadow' } },
      grid: { left: 8, right: 40, top: 16, bottom: 24, containLabel: true },
      xAxis: {
        type: 'value',
        max: 100,
        axisLabel: axisLabel,
        splitLine: splitLine
      },
      yAxis: {
        type: 'category',
        data: cats,
        axisLabel: { color: ink, fontSize: 12 },
        axisLine: { lineStyle: { color: rule } }
      },
      series: [
        {
          type: 'bar',
          data: vals.map(function (v, i) {
            return { value: v, itemStyle: { color: i < 3 ? accent : accent2 } };
          }),
          barWidth: '52%',
          label: { show: true, position: 'right', color: muted, fontSize: 11, formatter: '{c}' }
        }
      ]
    });
    window.addEventListener('resize', function () { chart.resize(); });
  })();

  // ---------- 图 4：修订后的复杂度雷达 ----------
  (function () {
    var chart = initChart('chart-radar');
    if (!chart) return;
    chart.setOption({
      animation: false,
      tooltip: { appendToBody: true },
      radar: {
        indicator: [
          { name: '门控正确性', max: 100 },
          { name: '账本口径', max: 100 },
          { name: '统计可信度', max: 100 },
          { name: '状态一致性', max: 100 },
          { name: '目标完整性（成本 vs 质量）', max: 100 },
          { name: '非凸处理', max: 100 }
        ],
        radius: '66%',
        center: ['50%', '54%'],
        axisName: { color: ink, fontSize: 12 },
        splitLine: { lineStyle: { color: rule } },
        splitArea: { areaStyle: { color: [bg2, 'transparent'] } },
        axisLine: { lineStyle: { color: rule } }
      },
      series: [
        {
          type: 'radar',
          symbol: 'circle',
          symbolSize: 6,
          lineStyle: { color: accent, width: 3 },
          itemStyle: { color: accent },
          areaStyle: { color: accent + '40' },
          data: [{ value: [90, 88, 82, 60, 75, 40], name: '剩余复杂度' }]
        }
      ]
    });
    window.addEventListener('resize', function () { chart.resize(); });
  })();

  // ---------- Mermaid ----------
  if (typeof mermaid !== 'undefined') {
    mermaid.initialize({ startOnLoad: false, theme: 'neutral', securityLevel: 'loose' });
    function runMermaid() {
      try { mermaid.run({ querySelector: '.mermaid' }); } catch (e) { /* noop */ }
    }
    if (document.readyState === 'complete' || document.readyState === 'interactive') {
      runMermaid();
    } else {
      document.addEventListener('DOMContentLoaded', runMermaid);
    }
  }
})();