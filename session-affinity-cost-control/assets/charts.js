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
  var kfmt = function (v) { return (v / 1000) + 'k'; };

  /* ---------- 图：每轮花费 vs 缓存读token数（deepseek-v4-flash） ---------- */
  ready('chart-perround', function () {
    var crA = 0.028, inA = 0.138, ouA = 0.275;  // Venice AI
    var crB = 0.003, inB = 0.150, ouB = 0.600;  // DeepSeek
    var u = 1000, o = 1000;
    var base = (inA * u + ouA * o) / 1e6;
    var x = [], yA = [], yB = [];
    for (var C = 0; C <= 40000; C += 1000) {
      x.push(C);
      yA.push(+(((crA * C + inA * u + ouA * o) / 1e6) / base).toFixed(3));
      yB.push(+(((crB * C + inB * u + ouB * o) / 1e6) / base).toFixed(3));
    }
    var c = echarts.init(document.getElementById('chart-perround'), null, { renderer: 'svg' });
    c.setOption({
      animation: false, color: [accent2, accent],
      tooltip: { trigger: 'axis', appendToBody: true, valueFormatter: function (v) { return v.toFixed(2) + '×'; } },
      legend: { data: ['Venice AI（输入/输出便宜）', 'DeepSeek（缓存读便宜）'], textStyle: { color: muted }, top: 0 },
      grid: baseGrid,
      xAxis: { type: 'category', name: '缓存读token数', nameLocation: 'middle', nameGap: 30, nameTextStyle: { color: muted },
        boundaryGap: false, data: x, axisLine: { lineStyle: { color: rule } }, axisTick: { show: false },
        axisLabel: { color: muted, interval: 4, formatter: kfmt } },
      yAxis: { type: 'value', name: '每轮花费（基准=1）', nameTextStyle: { color: muted },
        axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } }, axisLabel: { color: muted } },
      series: [
        { name: 'Venice AI（输入/输出便宜）', type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 3 }, data: yA,
          markLine: { silent: true, symbol: 'none', lineStyle: { color: muted, type: 'dashed' },
            label: { color: ink, fontWeight: 700, position: 'insideEndTop', formatter: '临界 ≈ 1.35 万 token' }, data: [{ xAxis: 13 }] } },
        { name: 'DeepSeek（缓存读便宜）', type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 3 }, data: yB }
      ]
    });
    rs(c);
  });

  /* ---------- 图：回本轮数分布直方图（6850 组） ---------- */
  ready('chart-hist', function () {
    var labels = ['0–1', '1–2', '2–3', '3–5', '5–8', '8–12', '12–20', '20–30', '30–50', '>50'];
    var counts = [255, 364, 393, 762, 607, 713, 798, 641, 930, 1387];
    var c = echarts.init(document.getElementById('chart-hist'), null, { renderer: 'svg' });
    c.setOption({
      animation: false, color: [accent],
      tooltip: { trigger: 'axis', appendToBody: true, axisPointer: { type: 'shadow' },
        valueFormatter: function (v) { return v + ' 组'; } },
      grid: baseGrid,
      xAxis: { type: 'category', name: '回本轮数（长会话极限）', nameLocation: 'middle', nameGap: 30, nameTextStyle: { color: muted },
        data: labels, axisLine: { lineStyle: { color: rule } }, axisTick: { show: false }, axisLabel: { color: muted, fontSize: 11 } },
      yAxis: { type: 'value', name: '可切换对数', nameTextStyle: { color: muted },
        axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } }, axisLabel: { color: muted } },
      series: [{ type: 'bar', data: counts, barMaxWidth: 42, itemStyle: { borderRadius: [4, 4, 0, 0] },
        label: { show: true, position: 'top', color: ink, fontSize: 10 },
        markLine: { silent: true, symbol: 'none', lineStyle: { color: accent2, type: 'dashed' },
          label: { color: accent2, fontWeight: 700, formatter: '中位（≈15 轮）落在 12–20 档' },
          data: [{ xAxis: '12–20' }] } }]
    });
    rs(c);
  });

  /* ---------- 图：回本轮数随缓存读变化（5 种 新增输入/输出 规模，15 个数据点） ---------- */
  ready('chart-hcurve', function () {
    var aO = 0.138, bO = 0.028, oO = 0.275;  // 旧
    var aN = 0.150, bN = 0.003, oN = 0.600;  // 新
    var R = [];
    for (var r = 20000; r <= 300000; r += 20000) R.push(r);
    var combos = [[500, 500], [1000, 1000], [2000, 2000], [4000, 4000], [8000, 8000]];
    var colors = [accent, '#1E7F7F', muted, '#D97706', accent2];
    var series = combos.map(function (cm, idx) {
      var N = cm[0], O = cm[1];
      var data = R.map(function (RR) {
        var save = (bO - bN) * RR + (aO - aN) * N + (oO - oN) * O;
        if (save <= 0) return null;
        var h = (aN - bN) * RR / save;
        return +h.toFixed(1);
      });
      return { name: '新增输入/输出 = ' + N + '/' + O, type: 'line', symbol: 'circle', symbolSize: 6,
        lineStyle: { width: 2.5, color: colors[idx] }, itemStyle: { color: colors[idx] }, data: data, connectNulls: false };
    });
    var c = echarts.init(document.getElementById('chart-hcurve'), null, { renderer: 'svg' });
    c.setOption({
      animation: false,
      tooltip: { trigger: 'axis', appendToBody: true, valueFormatter: function (v) { return v === null ? '不回本' : v + ' 轮'; } },
      legend: { data: series.map(function (s) { return s.name; }), textStyle: { color: muted, fontSize: 11 }, top: 0 },
      grid: baseGrid,
      xAxis: { type: 'category', name: '缓存读token数', nameLocation: 'middle', nameGap: 30, nameTextStyle: { color: muted },
        boundaryGap: false, data: R, axisLine: { lineStyle: { color: rule } }, axisTick: { show: false },
        axisLabel: { color: muted, rotation: 30, formatter: kfmt } },
      yAxis: { type: 'value', name: '回本轮数', nameTextStyle: { color: muted }, max: 70,
        axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } }, axisLabel: { color: muted } },
      series: series
    });
    rs(c);
  });

  /* ---------- 图：三个价差维度的方向分布（30144 有序对） ---------- */
  ready('chart-dimbar', function () {
    var cats = ['输入价差', '缓存读价差', '输出价差'];
    var cheaper = [24.4, 26.4, 24.2];  // 新渠道更便宜
    var same = [51.1, 47.1, 51.5];     // 同价
    var pricier = [24.4, 26.4, 24.2];  // 新渠道更贵
    var c = echarts.init(document.getElementById('chart-dimbar'), null, { renderer: 'svg' });
    c.setOption({
      animation: false,
      tooltip: { trigger: 'axis', appendToBody: true, axisPointer: { type: 'shadow' },
        valueFormatter: function (v) { return v.toFixed(1) + '%'; } },
      legend: { data: ['新渠道更便宜', '同价', '新渠道更贵'], textStyle: { color: muted }, top: 0 },
      grid: baseGrid,
      xAxis: { type: 'category', data: cats, axisLine: { lineStyle: { color: rule } }, axisTick: { show: false }, axisLabel: { color: ink, fontSize: 13 } },
      yAxis: { type: 'value', name: '占全部有序对', nameTextStyle: { color: muted }, max: 60,
        axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } },
        axisLabel: { color: muted, formatter: '{value}%' } },
      series: [
        { name: '新渠道更便宜', type: 'bar', data: cheaper, barMaxWidth: 46, itemStyle: { color: accent, borderRadius: [4, 4, 0, 0] },
          label: { show: true, position: 'top', color: ink, fontSize: 10, formatter: '{c}%' } },
        { name: '同价', type: 'bar', data: same, barMaxWidth: 46, itemStyle: { color: '#B9B6AC', borderRadius: [4, 4, 0, 0] },
          label: { show: true, position: 'top', color: ink, fontSize: 10, formatter: '{c}%' } },
        { name: '新渠道更贵', type: 'bar', data: pricier, barMaxWidth: 46, itemStyle: { color: accent2, borderRadius: [4, 4, 0, 0] },
          label: { show: true, position: 'top', color: ink, fontSize: 10, formatter: '{c}%' } }
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
