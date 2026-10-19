// 会话成本路由 · 极简版方案 —— 图表与图元
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

  // ---------- 图 1：有无分档/时段的成本形状 ----------
  (function () {
    var chart = initChart('chart-cost-shape');
    if (!chart) return;
    var turns = [];
    for (var i = 1; i <= 20; i++) turns.push('第 ' + i + ' 轮');
    var full = [100, 105, 110, 115, 120, 125, 130, 195, 205, 215, 225, 235, 120, 125, 130, 135, 140, 145, 150, 155];
    var lite = [100, 105, 110, 115, 120, 125, 130, 135, 140, 145, 150, 155, 160, 165, 170, 175, 180, 185, 190, 195];
    chart.setOption({
      animation: false,
      tooltip: { trigger: 'axis', appendToBody: true },
      legend: { data: ['完整版（有分档 / 时段）', '极简版（无分档 / 时段）'], bottom: 0, textStyle: { color: ink }, itemGap: 20 },
      grid: { left: 8, right: 20, top: 24, bottom: 48, containLabel: true },
      xAxis: {
        type: 'category',
        data: turns,
        axisLabel: { color: muted, fontSize: 11, interval: 1 },
        axisLine: { lineStyle: { color: rule } }
      },
      yAxis: {
        type: 'value',
        name: '这一轮花多少',
        nameTextStyle: { color: muted },
        axisLabel: axisLabel,
        splitLine: splitLine
      },
      series: [
        {
          name: '完整版（有分档 / 时段）',
          type: 'line',
          step: 'end',
          symbol: 'none',
          lineStyle: { color: accent, width: 3 },
          itemStyle: { color: accent },
          data: full
        },
        {
          name: '极简版（无分档 / 时段）',
          type: 'line',
          smooth: false,
          symbol: 'none',
          lineStyle: { color: accent2, width: 3 },
          itemStyle: { color: accent2 },
          data: lite
        }
      ]
    });
    window.addEventListener('resize', function () { chart.resize(); });
  })();

  // ---------- 图 3：体量对比 ----------
  (function () {
    var chart = initChart('chart-scope');
    if (!chart) return;
    chart.setOption({
      animation: false,
      tooltip: { trigger: 'axis', appendToBody: true, axisPointer: { type: 'shadow' } },
      legend: { data: ['完整版', '极简版'], bottom: 0, textStyle: { color: ink } },
      grid: { left: 8, right: 16, top: 22, bottom: 46, containLabel: true },
      xAxis: {
        type: 'category',
        data: ['决策门', '需预测的未来量', '需处理的跳变', '需算的临界点'],
        axisLabel: { color: ink, fontSize: 12 },
        axisLine: { lineStyle: { color: rule } }
      },
      yAxis: {
        type: 'value',
        name: '数量',
        nameTextStyle: { color: muted },
        axisLabel: axisLabel,
        splitLine: splitLine,
        minInterval: 1
      },
      series: [
        {
          name: '完整版',
          type: 'bar',
          data: [3, 2, 2, 2],
          itemStyle: { color: accent },
          barWidth: '32%',
          label: { show: true, position: 'top', color: muted, fontSize: 11 }
        },
        {
          name: '极简版',
          type: 'bar',
          data: [2, 2, 0, 0],
          itemStyle: { color: accent2 },
          barWidth: '32%',
          label: { show: true, position: 'top', color: muted, fontSize: 11 }
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