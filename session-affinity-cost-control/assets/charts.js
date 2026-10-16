(function () {
  var style = getComputedStyle(document.documentElement);
  var accent = style.getPropertyValue('--accent').trim();
  var accent2 = style.getPropertyValue('--accent2').trim();
  var ink = style.getPropertyValue('--ink').trim();
  var muted = style.getPropertyValue('--muted').trim();
  var rule = style.getPropertyValue('--rule').trim();
  var bg2 = style.getPropertyValue('--bg2').trim();

  function whenReady(el, fn) {
    if (document.getElementById(el)) { fn(); }
  }

  /* ---------- Chart 1: 售价结构对比 ---------- */
  whenReady('chart-price-structure', function () {
    var chart = echarts.init(document.getElementById('chart-price-structure'), null, { renderer: 'svg' });
    chart.setOption({
      animation: false,
      color: [accent2, accent],
      tooltip: {
        trigger: 'axis',
        appendToBody: true,
        axisPointer: { type: 'shadow' },
        valueFormatter: function (v) { return v + ' 倍输入价'; }
      },
      legend: {
        data: ['缓存便宜型（如 DeepSeek 官方）', '均衡型（如火山引擎）'],
        textStyle: { color: muted },
        top: 0
      },
      grid: { left: 8, right: 16, top: 48, bottom: 8, containLabel: true },
      xAxis: {
        type: 'category',
        data: ['输入', '缓存读', '输出'],
        axisLine: { lineStyle: { color: rule } },
        axisTick: { show: false },
        axisLabel: { color: ink, fontSize: 13 }
      },
      yAxis: {
        type: 'value',
        name: '相对输入价',
        nameTextStyle: { color: muted },
        axisLine: { show: false },
        axisTick: { show: false },
        splitLine: { lineStyle: { color: rule } },
        axisLabel: { color: muted }
      },
      series: [
        {
          name: '缓存便宜型（如 DeepSeek 官方）',
          type: 'bar',
          data: [1, 0.04, 4],
          barMaxWidth: 46,
          itemStyle: { borderRadius: [4, 4, 0, 0] },
          label: { show: true, position: 'top', color: ink, fontWeight: 700,
            formatter: function (p) { return p.value; } }
        },
        {
          name: '均衡型（如火山引擎）',
          type: 'bar',
          data: [1, 1, 1],
          barMaxWidth: 46,
          itemStyle: { borderRadius: [4, 4, 0, 0] },
          label: { show: true, position: 'top', color: ink, fontWeight: 700,
            formatter: function (p) { return p.value; } }
        }
      ]
    });
    window.addEventListener('resize', function () { chart.resize(); });
  });

  /* ---------- Chart 2: 累计成本 vs 会话轮数（临界点） ---------- */
  whenReady('chart-crossover', function () {
    var delta = 1000;   // 每轮新增输入 token
    var rho = 1;        // 输出 = 新增输入的 rho 倍
    var inA = 2.0, crA = 0.2, outA = 8.0;  // 缓存便宜型（元/百万 token）
    var inB = 0.8, crB = 0.8, outB = 0.8;  // 均衡型

    function total(inp, cr, outp, T) {
      var sumCtx = delta * T * (T - 1) / 2;
      var sumIn = delta * T;
      var sumOut = rho * delta * T;
      return (cr * sumCtx + inp * sumIn + outp * sumOut) / 1e6;
    }

    var turns = [], seriesA = [], seriesB = [], crossT = 0, best = Infinity;
    for (var T = 1; T <= 60; T++) {
      turns.push(T);
      var a = total(inA, crA, outA, T);
      var b = total(inB, crB, outB, T);
      seriesA.push(+a.toFixed(5));
      seriesB.push(+b.toFixed(5));
      var d = Math.abs(a - b);
      if (d < best) { best = d; crossT = T; }
    }

    var chart = echarts.init(document.getElementById('chart-crossover'), null, { renderer: 'svg' });
    chart.setOption({
      animation: false,
      color: [accent, accent2],
      tooltip: {
        trigger: 'axis',
        appendToBody: true,
        valueFormatter: function (v) { return v.toFixed(4) + ' 元'; }
      },
      legend: {
        data: ['A 缓存便宜型', 'B 均衡型'],
        textStyle: { color: muted },
        top: 0
      },
      grid: { left: 8, right: 20, top: 48, bottom: 8, containLabel: true },
      xAxis: {
        type: 'category',
        name: '会话轮数 T',
        nameLocation: 'middle',
        nameGap: 30,
        nameTextStyle: { color: muted },
        boundaryGap: false,
        data: turns,
        axisLine: { lineStyle: { color: rule } },
        axisTick: { show: false },
        axisLabel: { color: muted }
      },
      yAxis: {
        type: 'value',
        name: '累计成本（元）',
        nameTextStyle: { color: muted },
        axisLine: { show: false },
        axisTick: { show: false },
        splitLine: { lineStyle: { color: rule } },
        axisLabel: { color: muted }
      },
      series: [
        {
          name: 'A 缓存便宜型',
          type: 'line',
          smooth: true,
          symbol: 'none',
          lineStyle: { width: 2.5 },
          areaStyle: { opacity: 0.05 },
          data: seriesA,
          markLine: {
            silent: true,
            symbol: 'none',
            lineStyle: { color: muted, type: 'dashed' },
            label: { color: muted, formatter: '临界轮数 T*≈29' },
            data: [{ xAxis: 28 }]
          }
        },
        {
          name: 'B 均衡型',
          type: 'line',
          smooth: true,
          symbol: 'none',
          lineStyle: { width: 2.5 },
          data: seriesB
        }
      ]
    });
    window.addEventListener('resize', function () { chart.resize(); });
  });

  /* ---------- Chart 3: 迁移盈亏平衡 ---------- */
  whenReady('chart-breakeven', function () {
    var stayPerTurn = 0.020;   // 不迁移：每轮成本（元）
    var movePenalty = 0.240;   // 迁移一次性代价（元）
    var movePerTurn = 0.012;   // 迁移后：每轮成本（元）

    var R = [], stay = [], move = [];
    for (var i = 0; i <= 60; i++) {
      R.push(i);
      stay.push(+(stayPerTurn * i).toFixed(4));
      move.push(+(movePenalty + movePerTurn * i).toFixed(4));
    }

    var chart = echarts.init(document.getElementById('chart-breakeven'), null, { renderer: 'svg' });
    chart.setOption({
      animation: false,
      color: [accent2, accent],
      tooltip: {
        trigger: 'axis',
        appendToBody: true,
        valueFormatter: function (v) { return v.toFixed(3) + ' 元'; }
      },
      legend: {
        data: ['不迁移（每轮 0.020 元）', '迁移（一次 0.240 元 + 每轮 0.012 元）'],
        textStyle: { color: muted },
        top: 0
      },
      grid: { left: 8, right: 20, top: 48, bottom: 8, containLabel: true },
      xAxis: {
        type: 'category',
        name: '期望剩余轮数 R',
        nameLocation: 'middle',
        nameGap: 30,
        nameTextStyle: { color: muted },
        boundaryGap: false,
        data: R,
        axisLine: { lineStyle: { color: rule } },
        axisTick: { show: false },
        axisLabel: { color: muted }
      },
      yAxis: {
        type: 'value',
        name: '累计成本（元）',
        nameTextStyle: { color: muted },
        axisLine: { show: false },
        axisTick: { show: false },
        splitLine: { lineStyle: { color: rule } },
        axisLabel: { color: muted }
      },
      series: [
        {
          name: '不迁移（每轮 0.020 元）',
          type: 'line',
          smooth: true,
          symbol: 'none',
          lineStyle: { width: 2.5 },
          data: stay
        },
        {
          name: '迁移（一次 0.240 元 + 每轮 0.012 元）',
          type: 'line',
          smooth: true,
          symbol: 'none',
          lineStyle: { width: 2.5 },
          areaStyle: { opacity: 0.05 },
          data: move,
          markLine: {
            silent: true,
            symbol: 'none',
            lineStyle: { color: muted, type: 'dashed' },
            label: { color: muted, formatter: '盈亏平衡 R*≈30' },
            data: [{ xAxis: 30 }]
          }
        }
      ]
    });
    window.addEventListener('resize', function () { chart.resize(); });
  });

  /* ---------- Mermaid ---------- */
  if (window.mermaid) {
    mermaid.initialize({ startOnLoad: true, theme: 'neutral', securityLevel: 'loose' });
  }
})();
