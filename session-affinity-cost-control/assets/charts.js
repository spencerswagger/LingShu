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

  /* ---------- 图1：每轮成本 vs 当前上下文大小（缓存命中） ---------- */
  ready('chart-perround', function () {
    var cA = 0.2, iA = 2, oA = 8;   // A 缓存便宜型
    var cB = 0.8, iB = 0.8, oB = 0.8; // B 均衡型
    var u = 1000, o = 1000;         // 本轮新增输入 / 输出
    function perTurn(pcr, pin, pout, C) { return (C * pcr + u * pin + o * pout) / 1e6; }
    var x = [], yA = [], yB = [];
    for (var C = 0; C <= 40000; C += 2000) {
      x.push(C); yA.push(+perTurn(cA, iA, oA, C).toFixed(5)); yB.push(+perTurn(cB, iB, oB, C).toFixed(5));
    }
    var cross = 14000; // 0.2C+10000 = 0.8C+1600
    var c = echarts.init(document.getElementById('chart-perround'), null, { renderer: 'svg' });
    c.setOption({
      animation: false, color: [accent2, accent],
      tooltip: { trigger: 'axis', appendToBody: true, valueFormatter: function (v) { return v.toFixed(4) + ' 元'; } },
      legend: { data: ['A 缓存便宜型', 'B 均衡型'], textStyle: { color: muted }, top: 0 },
      grid: baseGrid,
      xAxis: { type: 'category', name: '缓存读 token 数（≈ 当前上下文）', nameLocation: 'middle', nameGap: 30, nameTextStyle: { color: muted },
        boundaryGap: false, data: x, axisLine: { lineStyle: { color: rule } }, axisTick: { show: false },
        axisLabel: { color: muted, formatter: function (v) { return v / 1000 + 'k'; } } },
      yAxis: { type: 'value', name: '每轮成本（元）', nameTextStyle: { color: muted },
        axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } },
        axisLabel: { color: muted, formatter: function (v) { return v.toFixed(2); } } },
      series: [
        { name: 'A 缓存便宜型', type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 3 }, data: yA,
          markLine: { silent: true, symbol: 'none', lineStyle: { color: muted, type: 'dashed' },
            label: { color: ink, fontWeight: 700, position: 'insideEndTop', formatter: '交叉 ≈ 14k token' },
            data: [{ xAxis: 7 }] } },
        { name: 'B 均衡型', type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 3 }, data: yB }
      ]
    });
    rs(c);
  });

  /* ---------- 图2：闲忙时——各渠道价随时刻变 ---------- */
  ready('chart-intraday', function () {
    var hours = [], A = [], B = [];
    for (var h = 0; h < 24; h++) {
      hours.push((h < 10 ? '0' + h : '' + h) + ':00');
      A.push((h >= 22 || h < 8) ? 0.5 : 1.0);  // A 闲时 22–08
      B.push((h < 6) ? 0.275 : 1.1);           // B 闲时 00–06
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

  /* ---------- Mermaid ---------- */
  if (window.mermaid) {
    mermaid.initialize({ startOnLoad: true, theme: 'neutral', securityLevel: 'loose' });
  }
})();
