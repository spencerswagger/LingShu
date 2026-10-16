(function () {
  var style = getComputedStyle(document.documentElement);
  var accent = style.getPropertyValue('--accent').trim();
  var accent2 = style.getPropertyValue('--accent2').trim();
  var ink = style.getPropertyValue('--ink').trim();
  var muted = style.getPropertyValue('--muted').trim();
  var rule = style.getPropertyValue('--rule').trim();

  function ready(id, fn) { if (document.getElementById(id)) fn(); }
  function rs(c) { window.addEventListener('resize', function () { c.resize(); }); }
  var baseGrid = { left: 8, right: 26, top: 50, bottom: 8, containLabel: true };
  var kfmt = function (v) { return (v / 1000) + 'k'; };
  var palette = [accent, accent2, accent + 'aa', accent2 + 'aa', muted, '#5B6B8C'];

  // 基准渠道对（示例）：旧 Venice、新 DeepSeek；新增输入/输出默认 1000/1000
  var aO = 0.138, bO = 0.028, oO = 0.275;
  var aN = 0.150, bN = 0.003, oN = 0.600;
  function H(R, N, O, aN_, bN_, oN_) {
    aN_ = aN_ === undefined ? aN : aN_; bN_ = bN_ === undefined ? bN : bN_; oN_ = oN_ === undefined ? oN : oN_;
    var save = (bO - bN_) * R + (aO - aN_) * N + (oO - oN_) * O;
    if (save <= 0) return null;
    return (aN_ - bN_) * R / save;
  }
  function axisR() { var r = []; for (var x = 20000; x <= 300000; x += 20000) r.push(x); return r; }

  /* ---------- 研究1：固定示例，回本轮数 vs 缓存读token数（标"下限"） ---------- */
  ready('study-q1', function () {
    var R = axisR();
    var data = R.map(function (x) { var v = H(x, 1000, 1000); return v === null ? null : +v.toFixed(1); });
    var c = echarts.init(document.getElementById('study-q1'), null, { renderer: 'svg' });
    c.setOption({
      animation: false, color: [accent],
      tooltip: { trigger: 'axis', appendToBody: true, valueFormatter: function (v) { return v === null ? '不回本' : v + ' 轮'; } },
      grid: baseGrid,
      xAxis: { type: 'category', name: '缓存读token数', nameLocation: 'middle', nameGap: 30, nameTextStyle: { color: muted },
        boundaryGap: false, data: R, axisLine: { lineStyle: { color: rule } }, axisTick: { show: false },
        axisLabel: { color: muted, rotation: 30, formatter: kfmt } },
      yAxis: { type: 'value', name: '回本轮数', nameTextStyle: { color: muted }, max: 40,
        axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } }, axisLabel: { color: muted } },
      series: [{ name: '回本轮数', type: 'line', smooth: true, symbol: 'circle', symbolSize: 6, lineStyle: { width: 3 }, data: data,
        markLine: { silent: true, symbol: 'none', lineStyle: { color: accent2, type: 'dashed' },
          label: { color: accent2, fontWeight: 700, position: 'insideEndTop', formatter: '下限 ≈ 6 轮（仅当期很大时成立）' },
          data: [{ yAxis: 6 }] } }]
    });
    rs(c);
  });

  /* ---------- 研究2：变动新渠道缓存读价，回本轮数 vs 缓存读token数 ---------- */
  ready('study-q2', function () {
    var R = axisR();
    var bNews = [0.001, 0.005, 0.010, 0.020, 0.025];
    var series = bNews.map(function (bn, idx) {
      var d = R.map(function (x) { var v = H(x, 1000, 1000, aN, bn, oN); return v === null ? null : +v.toFixed(1); });
      return { name: '新渠道缓存读价 ' + bn.toFixed(3), type: 'line', symbol: 'none', smooth: true,
        lineStyle: { width: 2.5, color: palette[idx] }, data: d, connectNulls: false };
    });
    var c = echarts.init(document.getElementById('study-q2'), null, { renderer: 'svg' });
    c.setOption({
      animation: false,
      tooltip: { trigger: 'axis', appendToBody: true, valueFormatter: function (v) { return v === null ? '不回本' : v + ' 轮'; } },
      legend: { data: series.map(function (s) { return s.name; }), textStyle: { color: muted, fontSize: 11 }, top: 0 },
      grid: baseGrid,
      xAxis: { type: 'category', name: '缓存读token数', nameLocation: 'middle', nameGap: 30, nameTextStyle: { color: muted },
        boundaryGap: false, data: R, axisLine: { lineStyle: { color: rule } }, axisTick: { show: false },
        axisLabel: { color: muted, rotation: 30, formatter: kfmt } },
      yAxis: { type: 'value', name: '回本轮数', nameTextStyle: { color: muted }, max: 60,
        axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } }, axisLabel: { color: muted } },
      series: series
    });
    rs(c);
  });

  /* ---------- 研究3：变动 输入:输出 比，回本轮数 vs 缓存读token数 ---------- */
  ready('study-q3a', function () {
    var R = axisR();
    var combos = [[4000, 1000, '输入:输出 = 4:1（输入重）'], [2000, 2000, '2:1'], [1000, 1000, '1:1'], [1000, 4000, '1:4（输出重）']];
    var series = combos.map(function (cm, idx) {
      var d = R.map(function (x) { var v = H(x, cm[0], cm[1]); return v === null ? null : +v.toFixed(1); });
      return { name: cm[2], type: 'line', symbol: 'none', smooth: true, lineStyle: { width: 2.5, color: palette[idx] }, data: d, connectNulls: false };
    });
    var c = echarts.init(document.getElementById('study-q3a'), null, { renderer: 'svg' });
    c.setOption({
      animation: false,
      tooltip: { trigger: 'axis', appendToBody: true, valueFormatter: function (v) { return v === null ? '不回本' : v + ' 轮'; } },
      legend: { data: series.map(function (s) { return s.name; }), textStyle: { color: muted, fontSize: 11 }, top: 0 },
      grid: baseGrid,
      xAxis: { type: 'category', name: '缓存读token数', nameLocation: 'middle', nameGap: 30, nameTextStyle: { color: muted },
        boundaryGap: false, data: R, axisLine: { lineStyle: { color: rule } }, axisTick: { show: false },
        axisLabel: { color: muted, rotation: 30, formatter: kfmt } },
      yAxis: { type: 'value', name: '回本轮数', nameTextStyle: { color: muted }, max: 60,
        axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } }, axisLabel: { color: muted } },
      series: series
    });
    rs(c);
  });

  /* ---------- 研究4：缓存率（= 缓存读 ÷ 总输入），回本轮数 vs 缓存率 ---------- */
  ready('study-q3b', function () {
    var T = [20000, 30000, 60000];   // 每轮总输入
    var O = 1000;
    var rhos = []; for (var p = 0.55; p <= 1.0001; p += 0.025) rhos.push(+p.toFixed(3));
    var series = T.map(function (tot, idx) {
      var d = rhos.map(function (rho) {
        var R = rho * tot, N = (1 - rho) * tot;
        var save = (bO - bN) * R + (aO - aN) * N + (oO - oN) * O;
        if (save <= 0) return null;
        return +(((aN - bN) * R / save)).toFixed(1);
      });
      return { name: '每轮总输入 ' + (tot / 1000) + 'k', type: 'line', symbol: 'circle', symbolSize: 5,
        lineStyle: { width: 2.5, color: palette[idx] }, data: d, connectNulls: false };
    });
    var c = echarts.init(document.getElementById('study-q3b'), null, { renderer: 'svg' });
    c.setOption({
      animation: false,
      tooltip: { trigger: 'axis', appendToBody: true, valueFormatter: function (v) { return v === null ? '不回本' : v + ' 轮'; } },
      legend: { data: series.map(function (s) { return s.name; }), textStyle: { color: muted }, top: 0 },
      grid: baseGrid,
      xAxis: { type: 'category', name: '缓存率（缓存读 ÷ 总输入）', nameLocation: 'middle', nameGap: 32, nameTextStyle: { color: muted },
        boundaryGap: false, data: rhos, axisLine: { lineStyle: { color: rule } }, axisTick: { show: false },
        axisLabel: { color: muted, formatter: function (v) { return Math.round(v * 100) + '%'; } } },
      yAxis: { type: 'value', name: '回本轮数', nameTextStyle: { color: muted }, max: 80,
        axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } }, axisLabel: { color: muted } },
      series: series
    });
    rs(c);
  });
})();
