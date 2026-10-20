// 会话成本路由解读报告 —— 图表与图元
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
  var baseTooltip = { trigger: 'axis', appendToBody: true };

  function initChart(id) {
    var el = document.getElementById(id);
    if (!el || typeof echarts === 'undefined') return null;
    return echarts.init(el, null, { renderer: 'svg' });
  }

  // ---------- 图 1：渠道三档成本价对比 ----------
  (function () {
    var chart = initChart('chart-channel-cost');
    if (!chart) return;
    chart.setOption({
      animation: false,
      tooltip: baseTooltip,
      legend: { data: ['输入价', '缓存读价', '输出价'], bottom: 0, textStyle: { color: ink }, itemGap: 18 },
      grid: { left: 8, right: 16, top: 24, bottom: 44, containLabel: true },
      xAxis: {
        type: 'category',
        data: ['渠道 A', '渠道 B', '渠道 C'],
        axisLabel: axisLabel,
        axisLine: { lineStyle: { color: rule } }
      },
      yAxis: {
        type: 'value',
        name: '积分 / 1e6 token',
        nameTextStyle: { color: muted },
        axisLabel: axisLabel,
        splitLine: splitLine
      },
      series: [
        { name: '输入价', type: 'bar', data: [5, 12, 10], itemStyle: { color: accent }, barGap: '10%' },
        { name: '缓存读价', type: 'bar', data: [1.0, 1.2, 0.5], itemStyle: { color: accent + '99' } },
        { name: '输出价', type: 'bar', data: [90, 30, 50], itemStyle: { color: accent2 } }
      ]
    });
    window.addEventListener('resize', function () { chart.resize(); });
  })();

  // ---------- 图 2：上下文分档阶跃 ----------
  (function () {
    var chart = initChart('chart-ctx-tier');
    if (!chart) return;
    chart.setOption({
      animation: false,
      tooltip: baseTooltip,
      grid: { left: 8, right: 16, top: 24, bottom: 30, containLabel: true },
      xAxis: {
        type: 'category',
        data: ['0', '16k', '32k', '48k', '64k', '96k', '128k', '160k', '200k'],
        axisLabel: axisLabel,
        axisLine: { lineStyle: { color: rule } },
        name: '上下文 token',
        nameLocation: 'middle',
        nameGap: 30,
        nameTextStyle: { color: muted }
      },
      yAxis: {
        type: 'value',
        min: 0,
        max: 2.4,
        name: '分档系数',
        nameTextStyle: { color: muted },
        axisLabel: axisLabel,
        splitLine: splitLine
      },
      series: [
        {
          name: '分档系数',
          type: 'line',
          step: 'end',
          symbol: 'circle',
          symbolSize: 7,
          lineStyle: { color: accent, width: 3 },
          itemStyle: { color: accent },
          areaStyle: { color: accent + '1f' },
          data: [1.0, 1.0, 1.5, 1.5, 1.5, 1.5, 2.0, 2.0, 2.0],
          markLine: {
            silent: true,
            symbol: 'none',
            lineStyle: { color: muted, type: 'dashed' },
            label: { color: muted, formatter: '{b}' },
            data: [
              { xAxis: '32k', name: '跳变' },
              { xAxis: '128k', name: '跳变' }
            ]
          }
        }
      ]
    });
    window.addEventListener('resize', function () { chart.resize(); });
  })();

  // ---------- 图 3：时段 / 节假日系数 ----------
  (function () {
    var chart = initChart('chart-time-band');
    if (!chart) return;
    var hours = [];
    var coeff = [];
    var holiday = [];
    for (var h = 0; h < 24; h++) {
      hours.push(h + '时');
      var c;
      if (h < 8) c = 0.5;
      else if (h < 18) c = 1.0;
      else if (h < 22) c = 0.8;
      else c = 0.5;
      coeff.push(c);
      holiday.push(0.3); // 节假日覆盖：全天折扣
    }
    chart.setOption({
      animation: false,
      tooltip: baseTooltip,
      legend: { data: ['常规日', '节假日覆盖'], bottom: 0, textStyle: { color: ink } },
      grid: { left: 8, right: 16, top: 24, bottom: 44, containLabel: true },
      xAxis: {
        type: 'category',
        data: hours,
        axisLabel: { color: muted, fontSize: 11, interval: 2 },
        axisLine: { lineStyle: { color: rule } }
      },
      yAxis: {
        type: 'value',
        min: 0,
        max: 1.2,
        name: '时段系数',
        nameTextStyle: { color: muted },
        axisLabel: axisLabel,
        splitLine: splitLine
      },
      series: [
        {
          name: '常规日',
          type: 'line',
          step: 'end',
          symbol: 'none',
          lineStyle: { color: accent2, width: 3 },
          itemStyle: { color: accent2 },
          areaStyle: { color: accent2 + '1f' },
          data: coeff
        },
        {
          name: '节假日覆盖',
          type: 'line',
          step: 'end',
          symbol: 'none',
          lineStyle: { color: accent, width: 2, type: 'dashed' },
          itemStyle: { color: accent },
          data: holiday
        }
      ]
    });
    window.addEventListener('resize', function () { chart.resize(); });
  })();

  // ---------- 图 5：回本轮数曲线 ----------
  (function () {
    var chart = initChart('chart-payback');
    if (!chart) return;
    var xs = [0.2, 0.5, 1, 1.5, 2, 3, 4, 5, 6, 8, 10, 15, 20];
    var ys = xs.map(function (x) { return x / (x / 9 + 0.22); });
    chart.setOption({
      animation: false,
      tooltip: baseTooltip,
      grid: { left: 8, right: 20, top: 24, bottom: 34, containLabel: true },
      xAxis: {
        type: 'value',
        name: '缓存读量（万 token）',
        nameLocation: 'middle',
        nameGap: 30,
        nameTextStyle: { color: muted },
        axisLabel: axisLabel,
        splitLine: splitLine,
        min: 0,
        max: 21
      },
      yAxis: {
        type: 'value',
        name: '回本轮数',
        nameTextStyle: { color: muted },
        axisLabel: axisLabel,
        splitLine: splitLine,
        min: 0,
        max: 10
      },
      series: [
        {
          name: '回本轮数',
          type: 'line',
          smooth: true,
          symbol: 'circle',
          symbolSize: 6,
          lineStyle: { color: accent, width: 3 },
          itemStyle: { color: accent },
          data: xs.map(function (x, i) { return [x, ys[i]]; }),
          markLine: {
            silent: true,
            symbol: 'none',
            lineStyle: { color: accent2, width: 2, type: 'dashed' },
            label: { color: accent2, formatter: '预计剩余轮数 = 6' },
            data: [{ yAxis: 6 }]
          },
          markPoint: {
            symbol: 'pin',
            symbolSize: 46,
            itemStyle: { color: accent2 },
            label: { color: '#fff', fontSize: 10, formatter: '临界' },
            data: [{ coord: [3.9, 6] }]
          }
        }
      ]
    });
    window.addEventListener('resize', function () { chart.resize(); });
  })();

  // ---------- 图 6：账本口径对比 ----------
  (function () {
    var chart = initChart('chart-ledger');
    if (!chart) return;
    var turns = [];
    var book = [0, 4, 8, 12, 16, 20, 24, 28, 32, 36, 40, 44, 48];
    var real = [0, 3, 6, 8, 9, 9, 8, 7, 7, 8, 10, 13, 17];
    for (var i = 0; i < book.length; i++) turns.push('T' + i);
    chart.setOption({
      animation: false,
      tooltip: baseTooltip,
      legend: { data: ['账面节省（移动基准）', '真实净收益（固定基准）'], bottom: 0, textStyle: { color: ink }, itemGap: 20 },
      grid: { left: 8, right: 20, top: 24, bottom: 48, containLabel: true },
      xAxis: {
        type: 'category',
        data: turns,
        axisLabel: axisLabel,
        axisLine: { lineStyle: { color: rule } }
      },
      yAxis: {
        type: 'value',
        name: '累计（积分）',
        nameTextStyle: { color: muted },
        axisLabel: axisLabel,
        splitLine: splitLine
      },
      series: [
        {
          name: '账面节省（移动基准）',
          type: 'line',
          smooth: false,
          symbol: 'circle',
          symbolSize: 5,
          lineStyle: { color: accent, width: 3 },
          itemStyle: { color: accent },
          data: book
        },
        {
          name: '真实净收益（固定基准）',
          type: 'line',
          smooth: false,
          symbol: 'circle',
          symbolSize: 5,
          lineStyle: { color: accent2, width: 3 },
          itemStyle: { color: accent2 },
          data: real,
          markPoint: {
            symbol: 'pin',
            symbolSize: 44,
            itemStyle: { color: accent },
            label: { color: '#fff', fontSize: 9, formatter: '虚增' },
            data: [{ coord: [10, 40] }]
          }
        }
      ]
    });
    window.addEventListener('resize', function () { chart.resize(); });
  })();

  // ---------- 图 7：复杂度雷达 ----------
  (function () {
    var chart = initChart('chart-radar');
    if (!chart) return;
    chart.setOption({
      animation: false,
      tooltip: { appendToBody: true },
      radar: {
        indicator: [
          { name: '非凸 / 不连续', max: 100 },
          { name: '估计不确定性', max: 100 },
          { name: '路径依赖 / 账本', max: 100 },
          { name: '跨渠道语义不可比', max: 100 },
          { name: '并发 / 状态 / 重放', max: 100 },
          { name: '目标多元（成本 vs 质量）', max: 100 },
          { name: '定价维度组合', max: 100 }
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
          data: [{ value: [85, 90, 80, 75, 70, 82, 65], name: '复杂度烈度' }]
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