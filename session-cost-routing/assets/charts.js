/* 会话成本路由 · 管理界面函数图（图 A / B / C / E）+ Mermaid 初始化
 * 颜色全部取自 HTML 中的 CSS 变量；图表用 SVG 渲染，静态（animation:false），含 resize。
 * 所有图由页面的“参数面板”驱动：拨动控件 → 实时重算。
 */
(function () {
  'use strict';

  var style = getComputedStyle(document.documentElement);
  function cv(name, fallback) { var v = style.getPropertyValue(name).trim(); return v || fallback; }

  var accent = cv('--accent', '#5a5ce8');
  var accent2 = cv('--accent2', '#8b5cf6');
  var accent3 = cv('--accent3', '#0e9f8a');
  var accent4 = cv('--accent4', '#e0537a');
  var ink = cv('--ink', '#131a2e');
  var muted = cv('--muted', '#61697f');
  var rule = cv('--rule', '#e5e8f2');
  var bg2 = cv('--bg2', '#f5f6fb');
  var warn = cv('--warn', '#f79009');
  var PALETTE = [accent, accent2, accent3, accent4];
  var FONT = "'Instrument Sans', 'PingFang SC', 'Microsoft YaHei', system-ui, sans-serif";

  /* 每张图相互隔离：单图异常不会阻断其他图或流程图。 */
  function run(name, fn) {
    try { fn(); } catch (e) {
      if (window.console && console.warn) console.warn('[chart:' + name + '] ' + (e && e.message ? e.message : e));
    }
  }

  function el(id) { return document.getElementById(id); }
  function q(sel, root) { return (root || document).querySelector(sel); }
  function qa(sel, root) { return Array.prototype.slice.call((root || document).querySelectorAll(sel)); }

  /* ================= 参数读取 ================= */
  function readChannels() {
    var rows = qa('#router-panel .ctrl-row[data-ch]');
    if (!rows.length) {
      return [
        { name: 'Venice（当前）', in: 0.138, cr: 0.028, out: 0.275 },
        { name: 'DeepSeek（候选）', in: 0.150, cr: 0.003, out: 0.600 },
        { name: '渠道 C（示例）', in: 0.120, cr: 0.012, out: 0.420 }
      ];
    }
    return rows.map(function (r, i) {
      var nm = q('.ch-name', r);
      function pv(k) { var e = q('input[data-p="' + k + '"]', r); return e ? parseFloat(e.value) : 0; }
      return { name: (nm && nm.value) || ('渠道' + (i + 1)), in: pv('in'), cr: pv('cr'), out: pv('out') };
    });
  }

  function gv(id, def) { var e = el(id); if (!e) return def; var v = parseFloat(e.value); return isFinite(v) ? v : def; }
  function readGlobals() {
    var time = gv('g-time', 1), bucket = gv('g-bucket', 1);
    return {
      newIn: gv('g-newin', 1000), out: gv('g-out', 1000), turns: gv('g-turns', 6), rho: gv('g-rho', 0.5),
      time: time, bucket: bucket, mult: time * bucket, horizon: gv('g-horizon', 10),
      sessions: gv('g-sessions', 1000), spread: gv('g-spread', 0.3)
    };
  }

  /* ================= 决策数学（单位：毫积分 = 0.001 积分） ================= */
  function perTurnMilli(ch, c, g) { return (c * ch.cr + g.newIn * ch.in + g.out * ch.out) * g.mult / 1000; }
  function crossX(ci, cj, g) {
    var d = ci.cr - cj.cr;
    if (Math.abs(d) < 1e-12) return null;
    return (g.newIn * (cj.in - ci.in) + g.out * (cj.out - ci.out)) / d;
  }
  function nOf(c, A, B, g) {
    var num = (B.in - B.cr) * c;
    var den = (A.cr - B.cr) * c + (A.in - B.in) * g.newIn + (A.out - B.out) * g.out;
    return den > 0 ? num / den : NaN;
  }
  function paybackLimit(A, B) { var d = A.cr - B.cr; return d > 1e-12 ? (B.in - B.cr) / d : NaN; }

  function seeder(seed) {
    return function () {
      seed |= 0; seed = (seed + 0x6D2B79F5) | 0;
      var t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
      t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
      return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
    };
  }

  /* ================= ECharts 实例管理 ================= */
  var instances = {};
  function ensure(id) {
    var e = el(id);
    if (!e || !window.echarts) return null;
    if (!instances[id]) {
      instances[id] = echarts.init(e, null, { renderer: 'svg' });
      (function (inst) { window.addEventListener('resize', function () { inst.resize(); }); })(instances[id]);
    }
    return instances[id];
  }
  function axisCommon(extra) {
    return Object.assign({
      axisLine: { lineStyle: { color: rule } },
      axisTick: { lineStyle: { color: rule } },
      axisLabel: { color: muted, fontFamily: FONT, fontSize: 12 },
      splitLine: { lineStyle: { color: rule, type: 'dashed' } }
    }, extra || {});
  }
  function grid() { return { left: 66, right: 132, top: 48, bottom: 56, containLabel: true }; }
  function wan(v) { return v === 0 ? '0' : (v / 10000) + '万'; }
  function setNote(id, text) { var e = el(id); if (e) e.textContent = text; }

  /* ================= 图 A：多渠道路由临界点 ================= */
  function renderA(chs, g) {
    var chart = ensure('chart-turn-cost');
    if (!chart) return;
    var xmax = 60000;

    var series = chs.map(function (c, i) {
      var data = [];
      for (var x = 0; x <= xmax; x += 500) data.push([x, perTurnMilli(c, x, g)]);
      return {
        name: c.name, type: 'line', smooth: false, symbol: 'none', data: data,
        lineStyle: { color: PALETTE[i % 4], width: 2.2 }, itemStyle: { color: PALETTE[i % 4] }
      };
    });

    var crit = [];
    for (var i = 0; i < chs.length; i++) {
      for (var j = i + 1; j < chs.length; j++) {
        var x = crossX(chs[i], chs[j], g);
        if (x !== null && x > 0 && x < xmax) {
          var vi = perTurnMilli(chs[i], x, g), onEnv = true;
          for (var k = 0; k < chs.length; k++) {
            if (k !== i && k !== j && perTurnMilli(chs[k], x, g) < vi - 1e-9) { onEnv = false; break; }
          }
          if (onEnv) crit.push(x);
        }
      }
    }
    crit.sort(function (a, b) { return a - b; });

    if (series.length) {
      series[0].markLine = {
        silent: true, symbol: 'none',
        data: crit.map(function (x) {
          return {
            xAxis: x, lineStyle: { color: warn, type: 'dashed', width: 1.4 },
            label: { color: warn, fontFamily: FONT, fontSize: 11, position: 'insideEndTop', formatter: '临界 ' + (x / 10000).toFixed(2) + '万' }
          };
        })
      };
    }

    chart.setOption({
      animation: false, textStyle: { fontFamily: FONT },
      grid: grid(),
      legend: { top: 8, right: 10, icon: 'roundRect', textStyle: { color: ink, fontFamily: FONT, fontSize: 12 } },
      tooltip: {
        trigger: 'axis', appendToBody: true,
        axisPointer: { type: 'line', lineStyle: { color: accent2, opacity: 0.5 } },
        formatter: function (ps) {
          var s = '缓存读 ' + wan(ps[0].axisValue) + ' token<br/>';
          ps.forEach(function (p) { s += p.marker + p.seriesName + '：' + Number(p.data[1]).toFixed(3) + ' 毫积分<br/>'; });
          return s;
        }
      },
      xAxis: axisCommon({
        type: 'value', min: 0, max: xmax, name: '缓存读 token 数', nameLocation: 'middle', nameGap: 34,
        nameTextStyle: { color: muted, fontFamily: FONT },
        axisLabel: { color: muted, fontFamily: FONT, fontSize: 12, formatter: wan }
      }),
      yAxis: axisCommon({
        type: 'value', name: '每轮花费（毫积分）', nameTextStyle: { color: muted, fontFamily: FONT },
        axisLabel: { color: muted, fontFamily: FONT, fontSize: 12, formatter: function (v) { return v.toFixed(1); } }
      }),
      series: series
    }, { notMerge: true });

    setNote('turn-cost-note',
      '读法：曲线越低越划算；' +
      (crit.length ? ('当前参数下有 ' + crit.length + ' 个临界点：' + crit.map(function (x) { return '≈' + (x / 10000).toFixed(2) + '万'; }).join('、') + '。') : '当前参数下各渠道曲线不交叉（无临界点）。') +
      '（纵轴单位为毫积分，1 毫积分 = 0.001 积分。）');
  }

  /* ================= 图 B：回本轮数 vs 缓存读 token ================= */
  function renderB(chs, g) {
    var chart = ensure('chart-payback');
    if (!chart) return;
    var A = chs[0], B = chs[1];
    if (!A || !B) { chart.clear(); return; }

    var slope = A.cr - B.cr;
    if (slope <= 1e-12) {
      chart.clear();
      setNote('payback-note', '当前「' + A.name + ' → ' + B.name + '」组合下，候选渠道缓存读并不更便宜（回本分母 ≤ 0）——永不回本。可调低候选渠道的缓存读价再看。');
      return;
    }

    var xmax = 300000;
    var d0 = -((A.in - B.in) * g.newIn + (A.out - B.out) * g.out) / slope;
    var xmin = Math.max(18000, isFinite(d0) && d0 > 0 ? Math.ceil(d0 * 1.08 / 1000) * 1000 : 0);

    var data = [];
    var maxN = 0;
    for (var c = xmin; c <= xmax; c += 1000) {
      var n = nOf(c, A, B, g);
      if (isFinite(n)) { data.push([c, n]); if (n > maxN) maxN = n; }
    }
    var yMax = Math.min(60, Math.max(20, Math.ceil(maxN / 5) * 5));

    var marks = [20000, 30000, 60000, 120000, 240000]
      .filter(function (x) { return x >= xmin && x <= xmax; })
      .map(function (x) { return { coord: [x, nOf(x, A, B, g)], value: nOf(x, A, B, g) }; })
      .filter(function (m) { return isFinite(m.value); });

    var limit = paybackLimit(A, B);
    var markLines = [
      { yAxis: g.turns, lineStyle: { color: warn, type: 'dashed', width: 1.6 }, label: { color: warn, fontFamily: FONT, fontSize: 11.5, formatter: '门槛：预计剩余轮数 = ' + Math.round(g.turns), position: 'end' } }
    ];
    if (isFinite(limit)) {
      markLines.push({ yAxis: limit, lineStyle: { color: muted, type: 'dashed', width: 1.4 }, label: { color: muted, fontFamily: FONT, fontSize: 11.5, formatter: '下限 ≈ ' + limit.toFixed(1), position: 'start' } });
    }

    chart.setOption({
      animation: false, textStyle: { fontFamily: FONT },
      grid: grid(),
      tooltip: {
        trigger: 'axis', appendToBody: true,
        axisPointer: { type: 'line', lineStyle: { color: accent2, opacity: 0.5 } },
        formatter: function (ps) {
          var p = ps[0];
          return '缓存读 ' + wan(p.axisValue) + ' token<br/>' + p.marker + '回本轮数：' + Number(p.data[1]).toFixed(2) + ' 轮';
        }
      },
      xAxis: axisCommon({
        type: 'value', min: xmin, max: xmax, name: '缓存读 token 数', nameLocation: 'middle', nameGap: 34,
        nameTextStyle: { color: muted, fontFamily: FONT },
        axisLabel: { color: muted, fontFamily: FONT, fontSize: 12, formatter: wan }
      }),
      yAxis: axisCommon({
        type: 'value', min: 0, max: yMax, name: '回本轮数（轮）', nameTextStyle: { color: muted, fontFamily: FONT },
        axisLabel: { color: muted, fontFamily: FONT, fontSize: 12 }
      }),
      series: [{
        name: A.name + ' → ' + B.name, type: 'line', smooth: true, symbol: 'none', data: data,
        lineStyle: { color: accent, width: 2.6 }, areaStyle: { color: accent, opacity: 0.06 },
        markLine: { silent: true, symbol: 'none', data: markLines },
        markPoint: {
          symbol: 'circle', symbolSize: 8,
          itemStyle: { color: accent2, borderColor: '#ffffff', borderWidth: 1.5 },
          label: { show: true, color: ink, fontFamily: FONT, fontSize: 11.5, position: 'top', formatter: function (p) { return Number(p.value).toFixed(1); } },
          data: marks
        }
      }]
    }, { notMerge: true });

    setNote('payback-note',
      '「' + A.name + ' → ' + B.name + '」：回本随缓存读增大而下降' +
      (isFinite(limit) ? ('，并收敛到下限 ≈ ' + limit.toFixed(1) + ' 轮') : '') +
      '。门槛线 = 预计剩余轮数（当前 ' + Math.round(g.turns) + '），门槛线上方需“剩余轮数 > 回本轮数”才切。');
  }

  /* ================= 图 C：n* vs 折扣 ρ ================= */
  function renderC(g) {
    var chart = ensure('chart-discount');
    if (!chart) return;
    var data = [];
    for (var r = 0.05; r <= 0.8501; r += 0.01) {
      var rr = Math.round(r * 100) / 100;
      data.push([rr, (12.5 * rr) / (1 - rr)]);
    }
    var h = g.horizon;
    var crossRho = h / (12.5 + h);

    chart.setOption({
      animation: false, textStyle: { fontFamily: FONT },
      grid: grid(),
      tooltip: {
        trigger: 'axis', appendToBody: true,
        axisPointer: { type: 'line', lineStyle: { color: accent2, opacity: 0.5 } },
        formatter: function (ps) {
          var p = ps[0];
          return '折扣 ρ = ' + Number(p.axisValue).toFixed(2) + '<br/>' + p.marker + '回本所需剩余请求数：' + Number(p.data[1]).toFixed(1) + ' 轮';
        }
      },
      xAxis: axisCommon({
        type: 'value', min: 0, max: 0.85, name: '折扣 ρ（目标价 ÷ 当前价，越小越便宜）', nameLocation: 'middle', nameGap: 34,
        nameTextStyle: { color: muted, fontFamily: FONT },
        axisLabel: { color: muted, fontFamily: FONT, fontSize: 12, formatter: function (v) { return v.toFixed(2); } }
      }),
      yAxis: axisCommon({
        type: 'value', min: 0, max: 80, name: '回本所需剩余请求数 n*（轮）', nameTextStyle: { color: muted, fontFamily: FONT },
        axisLabel: { color: muted, fontFamily: FONT, fontSize: 12 }
      }),
      series: [
        {
          name: 'n*', type: 'line', smooth: true, symbol: 'none', data: data,
          lineStyle: { color: accent, width: 2.6 }, areaStyle: { color: accent, opacity: 0.06 }
        },
        {
          name: 'horizon', type: 'line', symbol: 'none', silent: true, tooltip: { show: false },
          data: [[0, h], [0.85, h]],
          lineStyle: { color: warn, type: 'dashed', width: 1.6 },
          endLabel: { show: true, color: warn, fontFamily: FONT, fontSize: 11.5, formatter: '短会话 horizon = ' + Math.round(h) + ' 发' }
        },
        {
          name: 'cross', type: 'scatter', symbolSize: 9, tooltip: { show: false },
          data: [[crossRho, h]],
          itemStyle: { color: accent2, borderColor: '#ffffff', borderWidth: 1.5 },
          label: { show: true, position: 'top', color: ink, fontFamily: FONT, fontSize: 11.5, formatter: 'ρ ≈ ' + crossRho.toFixed(2) + ' → ' + Math.round(h) + ' 轮' }
        }
      ]
    }, { notMerge: true });

    setNote('discount-note', 'ρ = 目标价 ÷ 当前价，越小越便宜。折扣打深到 ρ ≈ ' + crossRho.toFixed(2) + '（约 ' + Math.round((1 - crossRho) * 100) + '% off）时，回本 = 短会话 horizon（当前 ' + Math.round(h) + ' 发）；再深，短会话也值得切。');
  }

  /* ================= 图 E：临界点分布（模拟） ================= */
  function renderE(chs, g) {
    var chart = ensure('chart-distribution');
    if (!chart) return;
    var A = chs[0], B = chs[1];
    var slope = (A && B) ? (A.cr - B.cr) : 0;
    if (!A || !B || slope <= 1e-12) {
      chart.clear();
      setNote('distribution-note', '当前渠道组合无临界点（候选缓存读不更便宜），无法形成分布。说明：本图为参数化模拟，非真实统计数据。');
      return;
    }

    var K = Math.max(1, Math.round(g.sessions));
    var spread = g.spread;
    var rnd = seeder(987654321);
    var vals = [];
    for (var i = 0; i < K; i++) {
      var ni = Math.max(0, g.newIn * (1 + spread * (2 * rnd() - 1)));
      var ou = Math.max(0, g.out * (1 + spread * (2 * rnd() - 1)));
      var c = (ni * (B.in - A.in) + ou * (B.out - A.out)) / slope;
      if (isFinite(c) && c > 0) vals.push(c);
    }
    if (!vals.length) {
      chart.clear();
      setNote('distribution-note', '无有效临界点样本（请检查价格与 token 入参）。说明：本图为参数化模拟，非真实统计数据。');
      return;
    }
    vals.sort(function (a, b) { return a - b; });
    function pct(p) { var idx = Math.min(vals.length - 1, Math.max(0, Math.round(p * (vals.length - 1)))); return vals[idx]; }
    var p10 = pct(0.10), p50 = pct(0.50), p90 = pct(0.90);

    var lo = vals[0], hi = vals[vals.length - 1];
    var nb = 16;
    var w = (hi - lo) / nb || 1;
    var bins = new Array(nb).fill(0);
    vals.forEach(function (v) { bins[Math.min(nb - 1, Math.floor((v - lo) / w))]++; });
    var cats = [], counts = [];
    for (var b = 0; b < nb; b++) { cats.push(((lo + w * (b + 0.5)) / 10000).toFixed(2) + '万'); counts.push(bins[b]); }
    var medBin = Math.min(nb - 1, Math.floor((p50 - lo) / w));

    chart.setOption({
      animation: false, textStyle: { fontFamily: FONT },
      grid: grid(),
      tooltip: {
        trigger: 'axis', appendToBody: true,
        axisPointer: { type: 'shadow' },
        formatter: function (ps) {
          var p = ps[0];
          return '临界缓存读 ≈ ' + p.axisValue + ' token<br/>' + p.marker + '会话数：' + p.data;
        }
      },
      xAxis: axisCommon({
        type: 'category', data: cats, name: '临界缓存读（token）', nameLocation: 'middle', nameGap: 40,
        nameTextStyle: { color: muted, fontFamily: FONT },
        axisLabel: { color: muted, fontFamily: FONT, fontSize: 10.5, rotate: 45, interval: 0 },
        splitLine: { show: false }
      }),
      yAxis: axisCommon({
        type: 'value', name: '会话数', nameTextStyle: { color: muted, fontFamily: FONT },
        axisLabel: { color: muted, fontFamily: FONT, fontSize: 12 }
      }),
      series: [{
        name: '会话数', type: 'bar', data: counts,
        itemStyle: { color: accent3 },
        markLine: {
          silent: true, symbol: 'none',
          data: [{ xAxis: cats[medBin], lineStyle: { color: warn, type: 'dashed', width: 1.6 }, label: { color: warn, fontFamily: FONT, fontSize: 11.5, formatter: '中位', position: 'insideEndTop' } }]
        }
      }]
    }, { notMerge: true });

    setNote('distribution-note',
      '模拟 ' + vals.length + ' 条会话：P10 ≈ ' + (p10 / 10000).toFixed(2) + '万、中位 ≈ ' + (p50 / 10000).toFixed(2) + '万、P90 ≈ ' + (p90 / 10000).toFixed(2) + '万 token。' +
      '（参数化模拟，非真实统计；接入真实调用日志后以实测分布替代。）');
  }

  /* ================= 输出值刷新 + 渲染调度 ================= */
  function refreshOutputs() {
    qa('#router-panel .ctrl-row[data-ch]').forEach(function (r) {
      qa('input.ch', r).forEach(function (inp) {
        var span = q('span[data-out="' + inp.getAttribute('data-p') + '"]', r);
        if (span) span.textContent = parseFloat(inp.value).toFixed(3);
      });
    });
    var map = {
      'g-newin': ['v-newin', function (v) { return Math.round(v); }],
      'g-out': ['v-out', function (v) { return Math.round(v); }],
      'g-turns': ['v-turns', function (v) { return Math.round(v); }],
      'g-rho': ['v-rho', function (v) { return v.toFixed(2); }],
      'g-time': ['v-time', function (v) { return v.toFixed(2); }],
      'g-bucket': ['v-bucket', function (v) { return v.toFixed(2); }],
      'g-horizon': ['v-horizon', function (v) { return Math.round(v); }],
      'g-sessions': ['v-sessions', function (v) { return Math.round(v); }],
      'g-spread': ['v-spread', function (v) { return Math.round(v * 100) + '%'; }]
    };
    Object.keys(map).forEach(function (id) {
      var e = el(id), s = el(map[id][0]);
      if (e && s) s.textContent = map[id][1](parseFloat(e.value));
    });
  }

  function renderAll() {
    var chs = readChannels(), g = readGlobals();
    run('A', function () { renderA(chs, g); });
    run('B', function () { renderB(chs, g); });
    run('C', function () { renderC(g); });
    run('E', function () { renderE(chs, g); });
  }

  /* ================= Mermaid：决策流程图（图 D） =================
   * 在解析阶段尽早初始化，确保 startOnLoad 生效。 */
  run('mermaid', function () {
    if (!window.mermaid) return;
    window.mermaid.initialize({
      startOnLoad: true, theme: 'neutral', securityLevel: 'loose',
      themeVariables: {
        fontFamily: 'Instrument Sans, PingFang SC, Microsoft YaHei, sans-serif', fontSize: '14px',
        primaryColor: '#eef0f8', primaryTextColor: '#131a2e', primaryBorderColor: '#5a5ce8',
        lineColor: '#8b5cf6', tertiaryColor: '#f5f6fb'
      }
    });
  });

  function boot() {
    refreshOutputs();
    renderAll();
    var panel = el('router-panel');
    if (panel) panel.addEventListener('input', function () { refreshOutputs(); renderAll(); });
  }
  if (document.readyState === 'loading') document.addEventListener('DOMContentLoaded', boot);
  else boot();
})();
