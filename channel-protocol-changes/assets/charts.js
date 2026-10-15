/* 渠道协议体系 · 变更影响报告 —— 图表初始化
 * 图 1：一期各层受影响文件数（改造现有 / 新增），数据与第四节逐文件清单一致。
 */
(function () {
  function cssVar(name, fallback) {
    var v = getComputedStyle(document.documentElement).getPropertyValue(name);
    v = (v || '').trim();
    return v || fallback;
  }

  function initChangeDist() {
    var el = document.getElementById('chart-change-dist');
    if (!el || typeof echarts === 'undefined') return;

    var ink = cssVar('--ink', '#16181d');
    var muted = cssVar('--muted', '#6b7280');
    var rule = cssVar('--rule', '#e2e5ea');
    var accent = cssVar('--accent', '#2563eb');
    var accent2 = cssVar('--accent2', '#c2410c');

    // echarts 类目轴自下而上排列，故数组按「从上到下」的展示顺序反转。
    var layers = ['装配与测试', '控制面 UI', '预设包导入', '渠道协议实体与数据层', '网关协议适配层'];
    var rework = [5, 5, 0, 7, 4];
    var added = [0, 3, 2, 3, 3];

    var chart = echarts.init(el, null, { renderer: 'canvas' });
    chart.setOption({
      tooltip: {
        trigger: 'axis',
        axisPointer: { type: 'shadow' },
        textStyle: { fontFamily: 'Instrument Sans' }
      },
      legend: {
        top: 0,
        right: 0,
        itemGap: 18,
        data: ['改造现有', '新增文件'],
        textStyle: { fontFamily: 'Instrument Sans', color: ink }
      },
      grid: { left: 8, right: 24, top: 44, bottom: 8, containLabel: true },
      xAxis: {
        type: 'value',
        minInterval: 1,
        axisLabel: { color: muted, fontFamily: 'JetBrains Mono' },
        axisLine: { lineStyle: { color: rule } },
        splitLine: { lineStyle: { color: rule } }
      },
      yAxis: {
        type: 'category',
        data: layers,
        axisTick: { show: false },
        axisLine: { lineStyle: { color: rule } },
        axisLabel: { color: ink, fontFamily: 'Instrument Sans' }
      },
      series: [
        {
          name: '改造现有',
          type: 'bar',
          stack: 'total',
          barMaxWidth: 26,
          itemStyle: { color: accent },
          label: {
            show: true,
            position: 'inside',
            color: '#ffffff',
            fontFamily: 'JetBrains Mono',
            fontWeight: 'bold'
          },
          data: rework
        },
        {
          name: '新增文件',
          type: 'bar',
          stack: 'total',
          barMaxWidth: 26,
          itemStyle: { color: accent2 },
          label: {
            show: true,
            position: 'inside',
            color: '#ffffff',
            fontFamily: 'JetBrains Mono',
            fontWeight: 'bold'
          },
          data: added
        }
      ]
    });

    window.addEventListener('resize', function () { chart.resize(); });
  }

  function boot() {
    initChangeDist();
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', boot);
  } else {
    boot();
  }
})();
