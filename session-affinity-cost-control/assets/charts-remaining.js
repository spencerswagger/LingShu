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

  var K = []; for (var k = 0; k <= 30; k++) K.push(k);

  // 几何模型（无记忆，续跑概率 0.8）
  var Sgeo = [1.0, 0.8, 0.64, 0.512, 0.41, 0.328, 0.262, 0.21, 0.168, 0.134, 0.107, 0.086, 0.069, 0.055, 0.044, 0.035, 0.028, 0.023, 0.018, 0.014, 0.012, 0.009, 0.007, 0.006, 0.005, 0.004, 0.003, 0.002, 0.002, 0.002, 0.001];
  // 长尾（真实会话更接近）
  var Slt = [1.0, 0.911, 0.807, 0.711, 0.629, 0.559, 0.5, 0.45, 0.408, 0.371, 0.34, 0.313, 0.289, 0.268, 0.249, 0.233, 0.218, 0.205, 0.193, 0.183, 0.173, 0.164, 0.156, 0.148, 0.142, 0.135, 0.129, 0.124, 0.119, 0.114, 0.11];
  var mgeo = []; for (var i = 0; i <= 30; i++) mgeo.push(4.0);
  var mlt = [15.7, 16.23, 17.34, 18.66, 20.11, 21.62, 23.17, 24.74, 26.32, 27.9, 29.48, 31.05, 32.6, 34.14, 35.67, 37.18, 38.68, 40.16, 41.62, 43.07, 44.49, 45.9, 47.3, 48.67, 50.03, 51.37, 52.69, 54.0, 55.29, 56.56, 57.82];

  /* ---------- 图：生存曲线 S(k)=P(会话超过k轮) ---------- */
  ready('chart-surv', function () {
    var c = echarts.init(document.getElementById('chart-surv'), null, { renderer: 'svg' });
    c.setOption({
      animation: false, color: [muted, accent],
      tooltip: { trigger: 'axis', appendToBody: true, valueFormatter: function (v) { return (v * 100).toFixed(0) + '%'; } },
      legend: { data: ['几何模型（无记忆）', '长尾（更接近真实）'], textStyle: { color: muted }, top: 0 },
      grid: baseGrid,
      xAxis: { type: 'category', name: '会话已进行轮数 k', nameLocation: 'middle', nameGap: 30, nameTextStyle: { color: muted },
        boundaryGap: false, data: K, axisLine: { lineStyle: { color: rule } }, axisTick: { show: false }, axisLabel: { color: muted } },
      yAxis: { type: 'value', name: 'S(k)', nameTextStyle: { color: muted }, min: 0, max: 1,
        axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } },
        axisLabel: { color: muted, formatter: function (v) { return (v * 100) + '%'; } } },
      series: [
        { name: '几何模型（无记忆）', type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 2.5 }, data: Sgeo },
        { name: '长尾（更接近真实）', type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 3 }, data: Slt }
      ]
    });
    rs(c);
  });

  /* ---------- 图：平均剩余寿命 m(k) = Σ_{j>k}S(j) / S(k) ---------- */
  ready('chart-mrl', function () {
    var c = echarts.init(document.getElementById('chart-mrl'), null, { renderer: 'svg' });
    c.setOption({
      animation: false, color: [muted, accent2],
      tooltip: { trigger: 'axis', appendToBody: true, valueFormatter: function (v) { return v + ' 轮'; } },
      legend: { data: ['几何模型：恒为 4 轮', '长尾：越久剩得越多'], textStyle: { color: muted }, top: 0 },
      grid: baseGrid,
      xAxis: { type: 'category', name: '会话已进行轮数 k', nameLocation: 'middle', nameGap: 30, nameTextStyle: { color: muted },
        boundaryGap: false, data: K, axisLine: { lineStyle: { color: rule } }, axisTick: { show: false }, axisLabel: { color: muted } },
      yAxis: { type: 'value', name: '预计剩余轮数', nameTextStyle: { color: muted }, max: 60,
        axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } }, axisLabel: { color: muted } },
      series: [
        { name: '几何模型：恒为 4 轮', type: 'line', symbol: 'none', lineStyle: { width: 2.5, type: 'dashed' }, data: mgeo },
        { name: '长尾：越久剩得越多', type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 3 }, data: mlt }
      ]
    });
    rs(c);
  });

  /* ---------- 图 3：按"当前轮消息角色"分段的 m(k)（示意） ---------- */
  // 四条曲线基数不同，体现"角色是主信号"；均随 k 缓慢上升（次要信号）
  function roleSeries(base) {
    var out = [];
    for (var i = 0; i <= 30; i++) {
      out.push(+(base * (1 + 0.04 * i)).toFixed(2));
    }
    return out;
  }
  var mUser = roleSeries(9);
  var mAsstTool = roleSeries(5);
  var mTool = roleSeries(2.5);
  var mAsstFinal = roleSeries(1.5);

  ready('chart-role', function () {
    var c = echarts.init(document.getElementById('chart-role'), null, { renderer: 'svg' });
    c.setOption({
      animation: false,
      color: [accent, accent2, '#7C3AED', muted],
      tooltip: { trigger: 'axis', appendToBody: true, valueFormatter: function (v) { return v + ' 轮'; } },
      legend: {
        data: ['最后一条是用户消息', '最后一条是含工具调用的助手消息', '最后一条是工具结果', '最后一条是最终助手消息'],
        textStyle: { color: muted }, top: 0
      },
      grid: baseGrid,
      xAxis: { type: 'category', name: '会话已进行轮数 k', nameLocation: 'middle', nameGap: 30, nameTextStyle: { color: muted },
        boundaryGap: false, data: K, axisLine: { lineStyle: { color: rule } }, axisTick: { show: false }, axisLabel: { color: muted } },
      yAxis: { type: 'value', name: '预计剩余轮数', nameTextStyle: { color: muted },
        axisLine: { show: false }, axisTick: { show: false }, splitLine: { lineStyle: { color: rule } }, axisLabel: { color: muted } },
      series: [
        { name: '最后一条是用户消息', type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 3 }, data: mUser },
        { name: '最后一条是含工具调用的助手消息', type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 2.5 }, data: mAsstTool },
        { name: '最后一条是工具结果', type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 2.5 }, data: mTool },
        { name: '最后一条是最终助手消息', type: 'line', smooth: true, symbol: 'none', lineStyle: { width: 2, type: 'dashed' }, data: mAsstFinal }
      ]
    });
    rs(c);
  });
})();
