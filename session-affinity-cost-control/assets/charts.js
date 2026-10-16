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

  /* ---------- 图A：两种定价 ---------- */
  ready('chart-price', function () {
    var c = echarts.init(document.getElementById('chart-price'), null, { renderer: 'svg' });
    c.setOption({
      animation: false, color: [accent2, accent],
      tooltip: { trigger: 'axis', appendToBody: true, axisPointer: { type: 'shadow' },
        valueFormatter: function (v) { return v + ' 倍输入价'; } },
      legend: { data: ['A 缓存便宜型', 'B 均衡型'], textStyle: { color: muted }, top: 0 },
      grid: { left: 8, right: 16, top: 46, bottom: 8, containLabel: true },
      xAxis: { type: 'category', data: ['输入', '缓存读', '输出'],
        axisLine: { lineStyle: { color: rule } }, axisTick: { show: false }, axisLabel: { color: ink, fontSize: 13 } },
      yAxis: { type: 'value', name: '相对输入价', nameTextStyle: { color: muted },
        axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } }, axisLabel: { color: muted } },
      series: [
        { name: 'A 缓存便宜型', type: 'bar', data: [1, 0.1, 4], barMaxWidth: 48,
          itemStyle: { borderRadius: [4, 4, 0, 0] },
          label: { show: true, position: 'top', color: ink, fontWeight: 700, formatter: function (p) { return p.value; } } },
        { name: 'B 均衡型', type: 'bar', data: [1, 1, 1], barMaxWidth: 48,
          itemStyle: { borderRadius: [4, 4, 0, 0] },
          label: { show: true, position: 'top', color: ink, fontWeight: 700, formatter: function (p) { return p.value; } } }
      ]
    });
    rs(c);
  });

  /* ---------- 图B：累计成本 vs 轮数，交叉即临界 ---------- */
  ready('chart-crossover', function () {
    var delta = 1000, rho = 1, inA = 2.0, crA = 0.2, outA = 8.0, inB = 0.8, crB = 0.8, outB = 0.8;
    function total(inp, cr, outp, T) {
      return (cr * delta * T * (T - 1) / 2 + inp * delta * T + outp * rho * delta * T) / 1e6;
    }
    var x = [], sa = [], sb = [];
    for (var T = 1; T <= 60; T++) { x.push(T); sa.push(+total(inA, crA, outA, T).toFixed(4)); sb.push(+total(inB, crB, outB, T).toFixed(4)); }
    var c = echarts.init(document.getElementById('chart-crossover'), null, { renderer: 'svg' });
    c.setOption({
      animation: false, color: [accent2, accent],
      tooltip: { trigger: 'axis', appendToBody: true, valueFormatter: function (v) { return v.toFixed(3) + ' 元'; } },
      legend: { data: ['A 缓存便宜型', 'B 均衡型'], textStyle: { color: muted }, top: 0 },
      grid: baseGrid,
      xAxis: { type: 'category', name: '会话轮数', nameLocation: 'middle', nameGap: 28, nameTextStyle: { color: muted },
        boundaryGap: false, data: x, axisLine: { lineStyle: { color: rule } }, axisTick: { show: false }, axisLabel: { color: muted } },
      yAxis: { type: 'value', name: '累计成本（元）', nameTextStyle: { color: muted }, axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } }, axisLabel: { color: muted } },
      series: [
        { name: 'A 缓存便宜型', type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 3 }, data: sa,
          markLine: { silent: true, symbol: 'none', lineStyle: { color: muted, type: 'dashed' },
            label: { color: ink, fontWeight: 700, position: 'insideEndTop', formatter: '临界：第 29 轮' }, data: [{ xAxis: 28 }] },
          markPoint: { symbol: 'circle', symbolSize: 10, itemStyle: { color: accent2 }, label: { show: false }, data: [{ coord: [28, sa[28]] }] } },
        { name: 'B 均衡型', type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 3 }, data: sb,
          markPoint: { symbol: 'circle', symbolSize: 10, itemStyle: { color: accent }, label: { show: false }, data: [{ coord: [28, sb[28]] }] } }
      ]
    });
    rs(c);
  });

  /* ---------- 图C：闲忙时——日内有效单价翻转 ---------- */
  ready('chart-intraday', function () {
    var hours = [];
    for (var h = 0; h < 24; h++) hours.push((h < 10 ? '0' + h : '' + h) + ':00');
    var A = [], B = [];
    for (var i = 0; i < 24; i++) {
      A.push((i >= 22 || i < 8) ? 0.5 : 1.0);   // A 闲时 22:00–08:00（系数 0.5）
      B.push((i < 6) ? 0.275 : 1.1);            // B 闲时 00:00–06:00（系数 0.25）
    }
    var c = echarts.init(document.getElementById('chart-intraday'), null, { renderer: 'svg' });
    c.setOption({
      animation: false, color: [accent2, accent],
      tooltip: { trigger: 'axis', appendToBody: true, valueFormatter: function (v) { return v.toFixed(3); } },
      legend: { data: ['渠道 A（闲时 22–08）', '渠道 B（闲时 00–06）'], textStyle: { color: muted }, top: 0 },
      grid: baseGrid,
      xAxis: { type: 'category', name: '一天中的时刻', nameLocation: 'middle', nameGap: 30, nameTextStyle: { color: muted },
        boundaryGap: false, data: hours, axisLine: { lineStyle: { color: rule } }, axisTick: { show: false },
        axisLabel: { color: muted, interval: 2 } },
      yAxis: { type: 'value', name: '有效单价（相对）', nameTextStyle: { color: muted }, axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } }, axisLabel: { color: muted } },
      series: [
        { name: '渠道 A（闲时 22–08）', type: 'line', step: 'end', symbol: 'none', lineStyle: { width: 3 }, data: A },
        { name: '渠道 B（闲时 00–06）', type: 'line', step: 'end', symbol: 'none', lineStyle: { width: 3 }, data: B,
          markArea: { silent: true, itemStyle: { color: 'rgba(14,110,110,0.06)' },
            label: { color: muted, fontSize: 11, position: 'insideTop' },
            data: [[{ xAxis: '00:00', name: 'B 更便宜' }, { xAxis: '06:00' }]] } }
      ]
    });
    rs(c);
  });

  /* ---------- 图D：迁移盈亏平衡 ---------- */
  ready('chart-breakeven', function () {
    var stay = 0.020, penalty = 0.240, move = 0.012, x = [], s1 = [], s2 = [];
    for (var i = 0; i <= 60; i++) { x.push(i); s1.push(+(stay * i).toFixed(4)); s2.push(+(penalty + move * i).toFixed(4)); }
    var c = echarts.init(document.getElementById('chart-breakeven'), null, { renderer: 'svg' });
    c.setOption({
      animation: false, color: [accent2, accent],
      tooltip: { trigger: 'axis', appendToBody: true, valueFormatter: function (v) { return v.toFixed(3) + ' 元'; } },
      legend: { data: ['不换（每轮 0.020）', '换（一次 0.240 + 每轮 0.012）'], textStyle: { color: muted }, top: 0 },
      grid: baseGrid,
      xAxis: { type: 'category', name: '预计还要发几轮', nameLocation: 'middle', nameGap: 28, nameTextStyle: { color: muted },
        boundaryGap: false, data: x, axisLine: { lineStyle: { color: rule } }, axisTick: { show: false }, axisLabel: { color: muted } },
      yAxis: { type: 'value', name: '累计成本（元）', nameTextStyle: { color: muted }, axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } }, axisLabel: { color: muted } },
      series: [
        { name: '不换（每轮 0.020）', type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 3 }, data: s1 },
        { name: '换（一次 0.240 + 每轮 0.012）', type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 3 }, data: s2,
          markLine: { silent: true, symbol: 'none', lineStyle: { color: muted, type: 'dashed' },
            label: { color: ink, fontWeight: 700, position: 'insideEndTop', formatter: '≥30 轮才划算' }, data: [{ xAxis: 30 }] } }
      ]
    });
    rs(c);
  });
})();
