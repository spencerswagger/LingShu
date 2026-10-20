// 会话成本路由 · 最终设计方案 —— 图表与图元
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

  // ---------- 图 1：每轮花费随轮次跳变 ----------
  (function () {
    var chart = initChart('chart-cost-overturn');
    if (!chart) return;
    var turns = [];
    for (var i = 1; i <= 20; i++) turns.push('第 ' + i + ' 轮');
    var cost = [100, 105, 110, 115, 120, 125, 130, 195, 205, 215, 225, 235, 120, 125, 130, 135, 140, 145, 150, 155];
    chart.setOption({
      animation: false,
      tooltip: { trigger: 'axis', appendToBody: true },
      grid: { left: 8, right: 20, top: 26, bottom: 40, containLabel: true },
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
          type: 'line',
          step: 'end',
          symbol: 'none',
          lineStyle: { color: accent, width: 3 },
          itemStyle: { color: accent },
          areaStyle: { color: accent + '1f' },
          data: cost,
          markLine: {
            silent: true,
            symbol: 'none',
            lineStyle: { color: accent2, type: 'dashed', width: 2 },
            label: { color: accent2, fontSize: 11 },
            data: [
              { xAxis: '第 8 轮', name: '跨入更高档位，整轮变贵' },
              { xAxis: '第 13 轮', name: '进入优惠时段，整轮变便宜' }
            ]
          }
        }
      ]
    });
    window.addEventListener('resize', function () { chart.resize(); });
  })();

  // ---------- 图 2：账本随时间变化 ----------
  (function () {
    var chart = initChart('chart-budget');
    if (!chart) return;
    var turns = [];
    for (var i = 0; i <= 12; i++) turns.push('第 ' + i + ' 轮');
    var saved = [0, 3, 6, 9, 12, 15, 18, 21, 24, 27, 30, 33, 36];
    var fee = [0, 0, 0, 8, 8, 8, 8, 14, 14, 14, 14, 14, 14];
    var balance = saved.map(function (v, i) { return v - fee[i]; });
    chart.setOption({
      animation: false,
      tooltip: { trigger: 'axis', appendToBody: true },
      legend: { data: ['累计省下的钱', '累计切换费', '账本余额'], bottom: 0, textStyle: { color: ink }, itemGap: 18 },
      grid: { left: 8, right: 20, top: 24, bottom: 48, containLabel: true },
      xAxis: {
        type: 'category',
        data: turns,
        axisLabel: axisLabel,
        axisLine: { lineStyle: { color: rule } }
      },
      yAxis: {
        type: 'value',
        name: '累计（元）',
        nameTextStyle: { color: muted },
        axisLabel: axisLabel,
        splitLine: splitLine
      },
      series: [
        {
          name: '累计省下的钱',
          type: 'line',
          symbol: 'none',
          lineStyle: { color: accent2, width: 2 },
          itemStyle: { color: accent2 },
          data: saved
        },
        {
          name: '累计切换费',
          type: 'line',
          step: 'end',
          symbol: 'none',
          lineStyle: { color: accent, width: 2, type: 'dashed' },
          itemStyle: { color: accent },
          data: fee
        },
        {
          name: '账本余额',
          type: 'line',
          smooth: false,
          symbol: 'circle',
          symbolSize: 6,
          lineStyle: { color: ink, width: 3 },
          itemStyle: { color: ink },
          areaStyle: { color: bg2 },
          data: balance
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