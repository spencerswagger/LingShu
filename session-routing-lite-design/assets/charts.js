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

  // ---------- 图 2：火山 vs DeepSeek 官方（闲/忙时）交叉点 ----------
  (function () {
    var chart = initChart('chart-crossover');
    if (!chart) return;
    var xs = [];
    var vol = [];
    var idle = [];
    var busy = [];
    for (var x = 0; x <= 140; x += 10) {
      xs.push(x);
      vol.push(0.1 * x + 0.6);       // 火山：三项同价 0.1
      idle.push(0.01 * x + 4.5);     // 官方闲时：缓存 0.01、输入 0.5、输出 2
      busy.push(0.02 * x + 9.0);     // 官方忙时：缓存 0.02、输入 1、输出 4
    }
    chart.setOption({
      animation: false,
      tooltip: { trigger: 'axis', appendToBody: true },
      legend: { data: ['火山（三项同价）', '官方·闲时', '官方·忙时'], bottom: 0, textStyle: { color: ink }, itemGap: 18 },
      grid: { left: 8, right: 24, top: 24, bottom: 48, containLabel: true },
      xAxis: {
        type: 'value',
        min: 0,
        max: 140,
        name: '累计缓存读量（千 token）',
        nameLocation: 'middle',
        nameGap: 28,
        nameTextStyle: { color: muted },
        axisLabel: axisLabel,
        splitLine: splitLine
      },
      yAxis: {
        type: 'value',
        name: '每轮成本（示意单位）',
        nameTextStyle: { color: muted },
        axisLabel: axisLabel,
        splitLine: splitLine
      },
      series: [
        {
          name: '火山（三项同价）',
          type: 'line',
          symbol: 'none',
          lineStyle: { color: accent, width: 3 },
          itemStyle: { color: accent },
          data: xs.map(function (v, i) { return [v, vol[i]]; }),
          markLine: {
            silent: true,
            symbol: 'none',
            lineStyle: { color: muted, type: 'dotted' },
            label: { color: muted, fontSize: 10, formatter: '交叉点' },
            data: [{ xAxis: 43 }, { xAxis: 105 }]
          }
        },
        {
          name: '官方·闲时',
          type: 'line',
          symbol: 'none',
          lineStyle: { color: accent2, width: 3 },
          itemStyle: { color: accent2 },
          data: xs.map(function (v, i) { return [v, idle[i]]; }),
          markPoint: {
            symbol: 'pin',
            symbolSize: 46,
            itemStyle: { color: accent2 },
            label: { color: '#fff', fontSize: 9, formatter: '≈43' },
            data: [{ coord: [43, 0.01 * 43 + 4.5] }]
          }
        },
        {
          name: '官方·忙时',
          type: 'line',
          symbol: 'none',
          lineStyle: { color: accent2, width: 2, type: 'dashed' },
          itemStyle: { color: accent2 },
          data: xs.map(function (v, i) { return [v, busy[i]]; }),
          markPoint: {
            symbol: 'pin',
            symbolSize: 46,
            itemStyle: { color: accent2 },
            label: { color: '#fff', fontSize: 9, formatter: '≈105' },
            data: [{ coord: [105, 0.02 * 105 + 9.0] }]
          }
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