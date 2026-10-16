(function () {
  var style = getComputedStyle(document.documentElement);
  var accent = style.getPropertyValue('--accent').trim();
  var accent2 = style.getPropertyValue('--accent2').trim();
  var ink = style.getPropertyValue('--ink').trim();
  var muted = style.getPropertyValue('--muted').trim();
  var rule = style.getPropertyValue('--rule').trim();

  function ready(id, fn) { if (document.getElementById(id)) fn(); }
  function rs(c) { window.addEventListener('resize', function () { c.resize(); }); }
  var baseGrid = { left: 8, right: 24, top: 48, bottom: 8, containLabel: true };

  /* ---------- 图：每轮花费 vs 缓存读token数（真实价：deepseek-v4-flash） ---------- */
  ready('chart-perround', function () {
    // Venice AI: IO 便宜、缓存读贵；DeepSeek: 缓存读便宜、输出贵（单位 美元/百万token，相对比较）
    var crA = 0.028, inA = 0.138, ouA = 0.275;
    var crB = 0.003, inB = 0.150, ouB = 0.600;
    var u = 1000, o = 1000;
    var base = (inA * u + ouA * o) / 1e6; // 归一化基准
    function cost(cr, pin, pou, C) { return (cr * C + pin * u + pou * o) / 1e6 / base; }
    var x = [], yA = [], yB = [];
    for (var C = 0; C <= 40000; C += 2000) {
      x.push(C); yA.push(+cost(crA, inA, ouA, C).toFixed(3)); yB.push(+cost(crB, inB, ouB, C).toFixed(3));
    }
    var c = echarts.init(document.getElementById('chart-perround'), null, { renderer: 'svg' });
    c.setOption({
      animation: false, color: [accent2, accent],
      tooltip: { trigger: 'axis', appendToBody: true, valueFormatter: function (v) { return v.toFixed(2) + '×'; } },
      legend: { data: ['Venice AI（输入/输出便宜）', 'DeepSeek（缓存读便宜）'], textStyle: { color: muted }, top: 0 },
      grid: baseGrid,
      xAxis: { type: 'category', name: '缓存读token数', nameLocation: 'middle', nameGap: 30, nameTextStyle: { color: muted },
        boundaryGap: false, data: x, axisLine: { lineStyle: { color: rule } }, axisTick: { show: false },
        axisLabel: { color: muted, formatter: function (v) { return v / 1000 + 'k'; } } },
      yAxis: { type: 'value', name: '每轮花费（基准=1）', nameTextStyle: { color: muted },
        axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } },
        axisLabel: { color: muted } },
      series: [
        { name: 'Venice AI（输入/输出便宜）', type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 3 }, data: yA,
          markLine: { silent: true, symbol: 'none', lineStyle: { color: muted, type: 'dashed' },
            label: { color: ink, fontWeight: 700, position: 'insideEndTop', formatter: '临界 ≈ 1.35 万 token' },
            data: [{ xAxis: 7 }] } },
        { name: 'DeepSeek（缓存读便宜）', type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 3 }, data: yB }
      ]
    });
    rs(c);
  });

  /* ---------- 图：各渠道价随时刻变（闲忙时） ---------- */
  ready('chart-intraday', function () {
    var hours = [], A = [], B = [];
    for (var h = 0; h < 24; h++) {
      hours.push((h < 10 ? '0' + h : '' + h) + ':00');
      A.push((h >= 22 || h < 8) ? 0.5 : 1.0);
      B.push((h < 6) ? 0.275 : 1.1);
    }
    var c = echarts.init(document.getElementById('chart-intraday'), null, { renderer: 'svg' });
    c.setOption({
      animation: false, color: [accent2, accent],
      tooltip: { trigger: 'axis', appendToBody: true, valueFormatter: function (v) { return v.toFixed(3); } },
      legend: { data: ['渠道 A', '渠道 B'], textStyle: { color: muted }, top: 0 },
      grid: baseGrid,
      xAxis: { type: 'category', name: '一天中的时刻', nameLocation: 'middle', nameGap: 30, nameTextStyle: { color: muted },
        boundaryGap: false, data: hours, axisLine: { lineStyle: { color: rule } }, axisTick: { show: false }, axisLabel: { color: muted, interval: 2 } },
      yAxis: { type: 'value', name: '有效单价（相对）', nameTextStyle: { color: muted },
        axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } }, axisLabel: { color: muted } },
      series: [
        { name: '渠道 A', type: 'line', step: 'end', symbol: 'none', lineStyle: { width: 3 }, data: A },
        { name: '渠道 B', type: 'line', step: 'end', symbol: 'none', lineStyle: { width: 3 }, data: B }
      ]
    });
    rs(c);
  });

  if (window.mermaid) {
    mermaid.initialize({ startOnLoad: true, theme: 'neutral', securityLevel: 'loose' });
  }
})();
