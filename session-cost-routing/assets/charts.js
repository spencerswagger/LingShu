/* 会话成本路由 · 管理界面函数图（图 A / B / C）+ Mermaid 初始化
 * 颜色全部取自 HTML 中的 CSS 变量；图表用 SVG 渲染，静态（animation:false），含 resize。
 */
(function () {
  'use strict';

  var style = getComputedStyle(document.documentElement);
  function cssVar(name, fallback) {
    var v = style.getPropertyValue(name).trim();
    return v || fallback;
  }

  var accent = cssVar('--accent', '#5a5ce8');
  var accent2 = cssVar('--accent2', '#8b5cf6');
  var ink = cssVar('--ink', '#131a2e');
  var muted = cssVar('--muted', '#61697f');
  var rule = cssVar('--rule', '#e5e8f2');
  var bg2 = cssVar('--bg2', '#f5f6fb');
  var warn = cssVar('--warn', '#f79009');

  var FONT = "'Instrument Sans', 'PingFang SC', 'Microsoft YaHei', system-ui, sans-serif";

  function baseGrid() {
    return {
      left: 66, right: 128, top: 46, bottom: 56, containLabel: true
    };
  }

  function axisCommon(extra) {
    return Object.assign({
      axisLine: { lineStyle: { color: rule } },
      axisTick: { lineStyle: { color: rule } },
      axisLabel: { color: muted, fontFamily: FONT, fontSize: 12 },
      splitLine: { lineStyle: { color: rule, type: 'dashed' } }
    }, extra || {});
  }

  function wan(v) {
    if (v === 0) return '0';
    return (v / 10000) + '万';
  }

  /* 每张图相互隔离：单图异常不会阻断其他图或流程图。 */
  function run(name, fn) {
    try { fn(); } catch (e) {
      if (window.console && console.warn) console.warn('[chart:' + name + '] ' + (e && e.message ? e.message : e));
    }
  }

  /* ================= 图 A：每轮花费 vs 缓存读 token 数 =================
   * 价格三段顺序为「输入 / 缓存读 / 输出」：
   *   Venice(旧)   0.138 / 0.028 / 0.275
   *   DeepSeek(新) 0.150 / 0.003 / 0.600
   * 设新增输入、输出各 1000；倍率取 1。纵轴单位：毫积分（1 毫积分 = 0.001 积分）。
   *   本轮花费 = (缓存读 × 缓存读价 + 新增输入 × 输入价 + 输出 × 输出价) / 1e6
   * 交叉点 = 337 / 0.025 = 13,480 token ≈ 1.35 万。
   */
  run('turnCost', function chartTurnCost() {
    var el = document.getElementById('chart-turn-cost');
    if (!el || !window.echarts) return;

    var xs = [], venice = [], deepseek = [];
    for (var c = 0; c <= 60000; c += 500) {
      xs.push(c);
      venice.push((0.028 * c + 413) / 1000);   // 毫积分
      deepseek.push((0.003 * c + 750) / 1000);  // 毫积分
    }
    var CROSS = 13480;

    var chart = echarts.init(el, null, { renderer: 'svg' });
    chart.setOption({
      animation: false,
      textStyle: { fontFamily: FONT },
      grid: baseGrid(),
      legend: {
        data: ['Venice（旧 / 当前）', 'DeepSeek（新 / 候选）'],
        top: 8, right: 10, icon: 'roundRect',
        textStyle: { color: ink, fontFamily: FONT, fontSize: 12 }
      },
      tooltip: {
        trigger: 'axis',
        appendToBody: true,
        axisPointer: { type: 'line', lineStyle: { color: accent2, opacity: 0.5 } },
        formatter: function (ps) {
          var s = '缓存读 ' + wan(ps[0].axisValue) + ' token<br/>';
          ps.forEach(function (p) {
            s += p.marker + p.seriesName + '：' + Number(p.data).toFixed(3) + ' 毫积分<br/>';
          });
          return s;
        }
      },
      xAxis: axisCommon({
        type: 'value', min: 0, max: 60000, name: '缓存读 token 数',
        nameLocation: 'middle', nameGap: 34,
        nameTextStyle: { color: muted, fontFamily: FONT },
        axisLabel: { color: muted, fontFamily: FONT, fontSize: 12, formatter: wan }
      }),
      yAxis: axisCommon({
        type: 'value', name: '每轮花费（毫积分）',
        nameTextStyle: { color: muted, fontFamily: FONT },
        axisLabel: { color: muted, fontFamily: FONT, fontSize: 12, formatter: function (v) { return v.toFixed(1); } }
      }),
      series: [
        {
          name: 'Venice（旧 / 当前）',
          type: 'line', smooth: false, symbol: 'none',
          data: xs.map(function (x, i) { return [x, venice[i]]; }),
          lineStyle: { color: accent2, width: 2.4 },
          itemStyle: { color: accent2 },
          markLine: {
            silent: true, symbol: 'none',
            lineStyle: { color: warn, type: 'dashed', width: 1.6 },
            label: {
              color: warn, fontFamily: FONT, fontSize: 11.5, formatter: '临界缓存读 ≈ 1.35万', position: 'insideEndTop'
            },
            data: [{ xAxis: CROSS }]
          }
        },
        {
          name: 'DeepSeek（新 / 候选）',
          type: 'line', smooth: false, symbol: 'none',
          data: xs.map(function (x, i) { return [x, deepseek[i]]; }),
          lineStyle: { color: accent, width: 2.4 },
          itemStyle: { color: accent }
        }
      ]
    });
    window.addEventListener('resize', function () { chart.resize(); });
  });

  /* ================= 图 B：回本轮数 vs 缓存读 token 数 =================
   * 回本轮数 n(c) = 分子 / 分母
   *   分子 = (0.150 − 0.003) × c = 0.147c
   *   分母 = (0.028 − 0.003) × c + (0.138 − 0.150)×1000 + (0.275 − 0.600)×1000
   *        = 0.025c − 337
   * 分母 ≤ 0（c ≤ 13,480）时永不回本；c→∞ 时 n → 0.147/0.025 = 5.88 ≈ 5.9。
   */
  run('payback', function chartPayback() {
    var el = document.getElementById('chart-payback');
    if (!el || !window.echarts) return;

    function nOf(c) { return (0.147 * c) / (0.025 * c - 337); }

    var data = [];
    for (var c = 18000; c <= 300000; c += 1000) { data.push([c, nOf(c)]); }

    var marks = [
      [20000, 18.0], [30000, 10.7], [60000, 7.6], [120000, 6.6], [240000, 6.2]
    ].map(function (p) { return { coord: [p[0], p[1]], value: p[1] }; });

    var chart = echarts.init(el, null, { renderer: 'svg' });
    chart.setOption({
      animation: false,
      textStyle: { fontFamily: FONT },
      grid: baseGrid(),
      tooltip: {
        trigger: 'axis',
        appendToBody: true,
        axisPointer: { type: 'line', lineStyle: { color: accent2, opacity: 0.5 } },
        formatter: function (ps) {
          var p = ps[0];
          return '缓存读 ' + wan(p.axisValue) + ' token<br/>' + p.marker + '回本轮数：' + Number(p.data[1]).toFixed(2) + ' 轮';
        }
      },
      xAxis: axisCommon({
        type: 'value', min: 0, max: 300000, name: '缓存读 token 数',
        nameLocation: 'middle', nameGap: 34,
        nameTextStyle: { color: muted, fontFamily: FONT },
        axisLabel: { color: muted, fontFamily: FONT, fontSize: 12, formatter: wan }
      }),
      yAxis: axisCommon({
        type: 'value', min: 0, max: 30, name: '回本轮数（轮）',
        nameTextStyle: { color: muted, fontFamily: FONT },
        axisLabel: { color: muted, fontFamily: FONT, fontSize: 12, formatter: function (v) { return v; } }
      }),
      series: [{
        name: '回本轮数',
        type: 'line', smooth: true, symbol: 'none',
        data: data,
        lineStyle: { color: accent, width: 2.6 },
        areaStyle: { color: accent, opacity: 0.06 },
        markLine: {
          silent: true, symbol: 'none',
          data: [
            {
              yAxis: 6,
              lineStyle: { color: warn, type: 'dashed', width: 1.6 },
              label: { color: warn, fontFamily: FONT, fontSize: 11.5, formatter: '门槛：预计剩余轮数 = 6', position: 'end' }
            },
            {
              yAxis: 5.9,
              lineStyle: { color: muted, type: 'dashed', width: 1.4 },
              label: { color: muted, fontFamily: FONT, fontSize: 11.5, formatter: '下限 ≈ 5.9', position: 'start' }
            }
          ]
        },
        markPoint: {
          symbol: 'circle', symbolSize: 8,
          itemStyle: { color: accent2, borderColor: '#ffffff', borderWidth: 1.5 },
          label: {
            show: true, color: ink, fontFamily: FONT, fontSize: 11.5,
            formatter: function (p) { return Number(p.value).toFixed(1); }, position: 'top'
          },
          data: marks
        }
      }]
    });
    window.addEventListener('resize', function () { chart.resize(); });
  });

  /* ================= 图 C：回本所需剩余请求数 n* vs 折扣 ρ =================
   * n*(ρ) = 12.5 · ρ / (1 − ρ)   （ρ = 目标价 ÷ 当前价）
   * 与 horizon = 10 相交于 ρ = 10 / 22.5 = 0.444。
   */
  run('discount', function chartDiscount() {
    var el = document.getElementById('chart-discount');
    if (!el || !window.echarts) return;

    function nStar(rho) { return (12.5 * rho) / (1 - rho); }

    var data = [];
    for (var r = 0.05; r <= 0.8201; r += 0.01) {
      var rr = Math.round(r * 100) / 100;
      data.push([rr, nStar(rr)]);
    }

    var chart = echarts.init(el, null, { renderer: 'svg' });
    chart.setOption({
      animation: false,
      textStyle: { fontFamily: FONT },
      grid: baseGrid(),
      tooltip: {
        trigger: 'axis',
        appendToBody: true,
        axisPointer: { type: 'line', lineStyle: { color: accent2, opacity: 0.5 } },
        formatter: function (ps) {
          var p = ps[0];
          return '折扣 ρ = ' + Number(p.axisValue).toFixed(2) + '<br/>' + p.marker + '回本所需剩余请求数：' + Number(p.data[1]).toFixed(1) + ' 轮';
        }
      },
      xAxis: axisCommon({
        type: 'value', min: 0, max: 0.85, name: '折扣 ρ（目标价 ÷ 当前价，越小越便宜）',
        nameLocation: 'middle', nameGap: 34,
        nameTextStyle: { color: muted, fontFamily: FONT },
        axisLabel: { color: muted, fontFamily: FONT, fontSize: 12, formatter: function (v) { return v.toFixed(2); } }
      }),
      yAxis: axisCommon({
        type: 'value', min: 0, max: 60, name: '回本所需剩余请求数 n*（轮）',
        nameTextStyle: { color: muted, fontFamily: FONT },
        axisLabel: { color: muted, fontFamily: FONT, fontSize: 12, formatter: function (v) { return v; } }
      }),
      series: [{
        name: 'n*',
        type: 'line', smooth: true, symbol: 'none',
        data: data,
        lineStyle: { color: accent, width: 2.6 },
        areaStyle: { color: accent, opacity: 0.06 },
        markLine: {
          silent: true, symbol: 'none',
          data: [{
            yAxis: 10,
            lineStyle: { color: warn, type: 'dashed', width: 1.6 },
            label: { color: warn, fontFamily: FONT, fontSize: 11.5, formatter: '短会话 horizon ≈ 10 发', position: 'end' }
          }]
        },
        markPoint: {
          symbol: 'circle', symbolSize: 9,
          itemStyle: { color: accent2, borderColor: '#ffffff', borderWidth: 1.5 },
          label: {
            show: true, color: ink, fontFamily: FONT, fontSize: 11.5,
            formatter: 'ρ ≈ 0.44 → 10 轮', position: 'top'
          },
          data: [{ coord: [0.444, 10] }]
        }
      }]
    });
    window.addEventListener('resize', function () { chart.resize(); });
  });

  /* ================= Mermaid：决策流程图（图 D） ================= */
  run('mermaid', function () {
    if (!window.mermaid) return;
    window.mermaid.initialize({
      startOnLoad: true,
      theme: 'neutral',
      securityLevel: 'loose',
      themeVariables: {
        fontFamily: 'Instrument Sans, PingFang SC, Microsoft YaHei, sans-serif',
        fontSize: '14px',
        primaryColor: '#eef0f8',
        primaryTextColor: '#131a2e',
        primaryBorderColor: '#5a5ce8',
        lineColor: '#8b5cf6',
        tertiaryColor: '#f5f6fb'
      }
    });
  });
})();