<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  NCard,
  NGrid,
  NGridItem,
  NSelect,
  NSpace,
  NButton,
  NSpin,
  NEmpty,
  NDataTable,
  NAlert,
  NTooltip,
  useMessage,
} from 'naive-ui'
import type { DataTableColumns } from 'naive-ui'
import type { EChartsOption } from 'echarts'
import StatCard from '@/components/StatCard.vue'
import EChart from '@/components/EChart.vue'
import { api } from '@/api'
import { useAppStore } from '@/stores/app'
import {
  cacheRateColor,
  successRateColor,
  formatBucketLabel,
  formatCompact,
  formatDuration,
  formatNumber,
  formatPercent,
  formatTps,
  enumerateBuckets,
  gradeLegend,
} from '@/utils/format'
import type { ModelStatsRow, OverviewStats, ProviderStatsRow, TimeSeriesPoint } from '@/types'

const store = useAppStore()
const router = useRouter()
const message = useMessage()

const RANGES = [
  { label: '最近 1 小时', value: '1h' },
  { label: '最近 6 小时', value: '6h' },
  { label: '最近 24 小时', value: '24h' },
  { label: '最近 7 天', value: '7d' },
  { label: '最近 30 天', value: '30d' },
  { label: '全部', value: 'all' },
]

const range = ref('24h')
const loading = ref(true)
const stats = ref<OverviewStats | null>(null)
const rawPoints = ref<TimeSeriesPoint[]>([])
const rangeFrom = ref('')
const rangeTo = ref('')
const byModel = ref<ModelStatsRow[]>([])
const byProvider = ref<ProviderStatsRow[]>([])
const bucket = ref('hour')

async function load() {
  loading.value = true
  try {
    const [ov, ts, bm, bp] = await Promise.all([
      api.overview(range.value),
      api.timeSeries(range.value),
      api.byModel(range.value, 8),
      api.byProvider(range.value, 8),
    ])
    stats.value = ov.stats
    if (ov.activeProvider) {
      const activeId = ov.activeProvider.id
      store.providers = store.providers.map((p) => ({ ...p, active: p.id === activeId }))
    }
    rawPoints.value = ts.points
    rangeFrom.value = ts.from
    rangeTo.value = ts.to
    bucket.value = ts.bucket
    byModel.value = bm
    byProvider.value = bp
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    loading.value = false
  }
}

// 后端只返回有请求的时间桶，这里补齐空桶，让横轴覆盖完整的所选区间。
const points = computed<TimeSeriesPoint[]>(() => {
  const keys = enumerateBuckets(rangeFrom.value, rangeTo.value, bucket.value)
  if (!keys.length) return rawPoints.value
  const byKey = new Map(rawPoints.value.map((p) => [p.bucket, p]))
  return keys.map(
    (k) =>
      byKey.get(k) ?? {
        bucket: k,
        requests: 0,
        failed: 0,
        promptTokens: 0,
        cachedTokens: 0,
        completionTokens: 0,
        cacheHitRate: 0,
        avgTtftMs: 0,
        avgTps: 0,
      },
  )
})

onMounted(load)

/** 成功率配色。与日志列表共用同一套四档，同一个数值在两处必须是同一个颜色。 */
const successAccent = computed(() => {
  if (!stats.value || stats.value.totalRequests <= 0) return undefined
  return successRateColor(stats.value.successRate)
})

/** 命中率为 0 不标红：很多上游压根不支持提示词缓存，那不是故障。 */
const cacheAccent = computed(() => {
  if (!stats.value) return undefined
  return cacheRateColor(stats.value.cacheHitRate)
})

/**
 * 轴触发的悬浮提示：按系列名分派格式，表头取该桶的时间标签。
 *
 * 两张趋势图共用同一套写法 —— 差别只在 valueFmt，而 ECharts 默认的提示会
 * 把 token 量级和毫秒数原样吐出来，读不下去。
 */
function axisTooltip(labels: string[], valueFmt: Record<string, (v: number) => string>) {
  return (params: unknown) => {
    const list = (Array.isArray(params) ? params : [params]) as Array<{
      seriesName?: string
      value?: unknown
      marker?: string
      dataIndex?: number
    }>
    const first = list[0]
    const head = first && first.dataIndex != null ? labels[first.dataIndex] ?? '' : ''
    const rows = list.map((p) => {
      const fmt = valueFmt[String(p.seriesName)] ?? formatNumber
      const value = typeof p.value === 'number' ? p.value : null
      return `${p.marker ?? ''}${p.seriesName}：${value == null ? '—' : fmt(value)}`
    })
    return [head, ...rows].join('<br/>')
  }
}

/** 请求量与 token 的双轴趋势图。 */
const trendOption = computed<EChartsOption>(() => {
  const labels = points.value.map((p) => formatBucketLabel(p.bucket))
  // 悬浮提示按系列分派格式：token 量级动辄几千万，原样显示会把提示框撑得读不下去。
  const valueFmt: Record<string, (v: number) => string> = {
    请求数: formatNumber,
    失败数: formatNumber,
    '输入 token': formatCompact,
    '缓存 token': formatCompact,
    '输出 token': formatCompact,
    缓存命中率: (v) => formatPercent(v, 1),
  }
  return {
    tooltip: {
      trigger: 'axis',
      axisPointer: { type: 'cross' },
      formatter: axisTooltip(labels, valueFmt),
    },
    legend: {
      data: ['请求数', '失败数', '输入 token', '缓存 token', '输出 token', '缓存命中率'],
      top: 0,
      itemGap: 12,
      textStyle: { fontSize: 11 },
    },
    // 左右各留 56px 就够：左边只画 token 刻度（短格式，最宽「80M」），
    // 右边只有一个百分比轴。顶部留 56px 是因为窄窗口下 6 项图例会折成两行，
    // 留窄了会压到绘图区上沿。
    grid: { left: 56, right: 56, top: 56, bottom: 40 },
    xAxis: {
      type: 'category',
      data: labels,
      axisLabel: { hideOverlap: true },
    },
    // 只有两个轴写 name：图例已经逐条标注了系列名，再写轴名在窄窗口下会互相重叠。
    // 单位直接体现在刻度上（命中率轴补 %，token 轴走短格式）。
    yAxis: [
      // 柱子（请求数/失败数）的刻度轴。不画出来是有意的：左侧已经被 token 轴占用，
      // 两条轴叠在同一边必然互相压字。柱子仍然按这条轴缩放，具体数值看悬浮提示。
      // 十字准星也要一起关掉，否则它会飘出一个「57.35」这种带小数的请求数。
      { type: 'value', minInterval: 1, show: false, axisPointer: { show: false } },
      {
        type: 'value',
        position: 'left',
        axisLabel: { formatter: (v: number) => formatCompact(v) },
        axisPointer: { label: { formatter: (p: { value: unknown }) => formatCompact(Number(p.value)) } },
      },
      {
        type: 'value',
        min: 0,
        max: 100,
        position: 'right',
        splitLine: { show: false },
        axisLabel: { formatter: '{value}%' },
        axisPointer: {
          label: { formatter: (p: { value: unknown }) => formatPercent(Number(p.value), 0) },
        },
      },
    ],
    series: [
      {
        name: '请求数',
        type: 'bar',
        stack: 'req',
        // 单个数据桶时限制柱宽，否则一根柱子会横跨整个绘图区，看着很怪。
        barMaxWidth: 42,
        data: points.value.map((p) => p.requests - p.failed),
        itemStyle: { color: '#2082ff' },
      },
      {
        name: '失败数',
        type: 'bar',
        stack: 'req',
        barMaxWidth: 42,
        data: points.value.map((p) => p.failed),
        itemStyle: { color: '#d03050' },
      },
      {
        name: '输入 token',
        type: 'line',
        smooth: true,
        yAxisIndex: 1,
        data: points.value.map((p) => p.promptTokens),
        itemStyle: { color: '#18a058' },
      },
      {
        name: '缓存 token',
        type: 'line',
        smooth: true,
        yAxisIndex: 1,
        data: points.value.map((p) => p.cachedTokens),
        itemStyle: { color: '#f0a020' },
      },
      {
        name: '输出 token',
        type: 'line',
        smooth: true,
        yAxisIndex: 1,
        data: points.value.map((p) => p.completionTokens),
        itemStyle: { color: '#7c4dff' },
      },
      {
        // 命中率是百分比，需要独立的 0–100 轴，否则会被 token 量级压成一条平线。
        // 用青色虚线，避免和同为绿色的「输入 token」混淆。
        name: '缓存命中率',
        type: 'line',
        smooth: true,
        yAxisIndex: 2,
        data: points.value.map((p) => (p.promptTokens > 0 ? Number(p.cacheHitRate.toFixed(1)) : null)),
        itemStyle: { color: '#13c2c2' },
        lineStyle: { type: 'dashed', width: 2 },
      },
    ],
  }
})

/** 性能趋势：首 token 耗时（秒）与 TPS。 */
const perfOption = computed<EChartsOption>(() => {
  const labels = points.value.map((p) => formatBucketLabel(p.bucket))
  const valueFmt: Record<string, (v: number) => string> = {
    '平均首 token': (v) => `${v.toFixed(2)} s`,
    '平均 TPS': (v) => `${v.toFixed(1)} token/s`,
  }
  return {
    tooltip: { trigger: 'axis', formatter: axisTooltip(labels, valueFmt) },
    legend: { data: ['平均首 token', '平均 TPS'], top: 0 },
    // 左轴用秒而不是毫秒：几秒级的读数比四位数毫秒好认，也和指标卡、
    // 模型排行里的「4.02 s」是同一个口径。
    grid: { left: 60, right: 60, top: 44, bottom: 40 },
    xAxis: {
      type: 'category',
      data: labels,
      axisLabel: { hideOverlap: true },
    },
    yAxis: [
      { type: 'value', name: 's' },
      { type: 'value', name: 'token/s' },
    ],
    series: [
      {
        name: '平均首 token',
        type: 'line',
        smooth: true,
        data: points.value.map((p) => Number((p.avgTtftMs / 1000).toFixed(2))),
        itemStyle: { color: '#f0a020' },
      },
      {
        name: '平均 TPS',
        type: 'line',
        smooth: true,
        yAxisIndex: 1,
        data: points.value.map((p) => Number(p.avgTps.toFixed(1))),
        itemStyle: { color: '#18a058' },
      },
    ],
  }
})

const providerShareOption = computed<EChartsOption>(() => ({
  tooltip: { trigger: 'item', formatter: '{b}: {c} 次 ({d}%)' },
  legend: { bottom: 0, type: 'scroll' },
  series: [
    {
      type: 'pie',
      radius: ['46%', '70%'],
      avoidLabelOverlap: true,
      itemStyle: { borderRadius: 6, borderWidth: 2, borderColor: 'transparent' },
      label: { show: false },
      data: byProvider.value.map((p) => ({
        // providerId 为 0 表示请求没能匹配到任何供应商（例如短名写错），
        // 直接显示 #0 会让人摸不着头脑。
        name: p.providerName || (p.providerId ? `#${p.providerId}` : '未匹配供应商'),
        value: p.requests,
      })),
    },
  ],
}))

const modelColumns: DataTableColumns<ModelStatsRow> = [
  { title: '模型', key: 'model', ellipsis: { tooltip: true } },
  { title: '请求', key: 'requests', align: 'right', width: 80, render: (r) => formatNumber(r.requests) },
  {
    title: '失败',
    key: 'failed',
    align: 'right',
    width: 70,
    render: (r) =>
      r.failed
        ? h('span', { style: 'color:#d03050' }, String(r.failed))
        : h('span', { style: 'opacity:.4' }, '0'),
  },
  {
    title: '输出 token',
    key: 'completionTokens',
    align: 'right',
    width: 100,
    render: (r) => formatCompact(r.completionTokens),
  },
  {
    title: () =>
      h(NTooltip, null, {
        trigger: () => h('span', null, '缓存率'),
        default: () => '命中缓存的输入量占输入总量的比例',
      }),
    key: 'cacheHitRate',
    align: 'right',
    width: 88,
    render: (r) =>
      r.promptTokens
        ? h(
            'span',
            { class: 'num', style: r.cacheHitRate >= 50 ? 'color:#18a058' : undefined },
            formatPercent(r.cacheHitRate, 1),
          )
        : h('span', { style: 'opacity:.35' }, '—'),
  },
  {
    title: '首 token',
    key: 'avgTtftMs',
    align: 'right',
    width: 94,
    render: (r) => formatDuration(r.avgTtftMs),
  },
  { title: 'TPS', key: 'avgTps', align: 'right', width: 72, render: (r) => formatTps(r.avgTps) },
]
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2>仪表盘</h2>
        <p class="sub">
          统计的是经过本代理的请求。成功率要求 HTTP 2xx 且流式响应正常收尾。
        </p>
      </div>
      <NSpace align="center">
        <NSelect v-model:value="range" :options="RANGES" style="width: 150px" @update:value="load" />
        <NButton :loading="loading" @click="load">刷新</NButton>
      </NSpace>
    </div>

    <NAlert v-if="!store.providers.length" type="warning" :bordered="false" style="margin-bottom: 16px">
      还没有配置任何供应商。
      <NButton text type="primary" @click="router.push('/providers')">去添加</NButton>
    </NAlert>

    <!-- 指标卡用 CSS grid 的 auto-fit：列数随宽度自动增减，
         不用为每个断点单独写栅格跨度。 -->
    <div class="stat-row">
      <StatCard label="请求总数" :value="formatNumber(stats?.totalRequests ?? 0)" :loading="loading" />
      <StatCard
        label="成功率"
        :value="formatPercent(stats?.successRate)"
        :hint="`成功 ${formatNumber(stats?.successRequests ?? 0)} / 失败 ${formatNumber(stats?.failedRequests ?? 0)}。${gradeLegend('successRate', (v) => `${v}%`)}`"
        :accent="successAccent"
        :loading="loading"
      />
      <StatCard
        label="缓存命中率"
        :value="formatPercent(stats?.cacheHitRate)"
        :hint="`输入中命中缓存 ${formatCompact(stats?.cachedTokens ?? 0)} / 共 ${formatCompact(stats?.promptTokens ?? 0)}。${gradeLegend('cacheRate', (v) => `${v}%`)}。命中率越高，重复内容的请求越便宜、首 token 越快`"
        :accent="cacheAccent"
        :loading="loading"
      />
      <StatCard
        label="输入 token"
        :value="formatCompact(stats?.promptTokens ?? 0)"
        :hint="`其中命中缓存 ${formatCompact(stats?.cachedTokens ?? 0)}`"
        :loading="loading"
      />
      <StatCard label="输出 token" :value="formatCompact(stats?.completionTokens ?? 0)" :loading="loading" />
      <StatCard
        label="平均首 token"
        :value="formatDuration(stats?.avgTtftMs)"
        hint="从发出请求到收到第一个生成 token 的平均耗时"
        :loading="loading"
      />
      <StatCard
        label="平均 TPS"
        :value="formatTps(stats?.avgTps)"
        hint="每秒生成 token 数，已排除首 token 等待时间；仅流式响应计入"
        :loading="loading"
      />
    </div>

    <NGrid :cols="24" :x-gap="12" :y-gap="12" style="margin-top: 12px">
      <NGridItem :span="16">
        <NCard size="small" title="请求与 token 趋势" :bordered="false" class="chart-card">
          <NSpin :show="loading">
            <NEmpty v-if="!points.length" description="该时间段内没有请求" style="padding: 60px 0" />
            <EChart v-else :option="trendOption" height="300px" />
          </NSpin>
        </NCard>
      </NGridItem>
      <NGridItem :span="8">
        <NCard size="small" title="供应商请求占比" :bordered="false" class="chart-card">
          <NSpin :show="loading">
            <NEmpty v-if="!byProvider.length" description="暂无数据" style="padding: 60px 0" />
            <EChart v-else :option="providerShareOption" height="300px" />
          </NSpin>
        </NCard>
      </NGridItem>
    </NGrid>

    <NGrid :cols="24" :x-gap="12" :y-gap="12" style="margin-top: 12px">
      <NGridItem :span="12">
        <NCard size="small" title="性能趋势" :bordered="false" class="chart-card">
          <NSpin :show="loading">
            <NEmpty v-if="!points.length" description="暂无数据" style="padding: 60px 0" />
            <EChart v-else :option="perfOption" height="260px" />
          </NSpin>
        </NCard>
      </NGridItem>
      <NGridItem :span="12">
        <NCard size="small" title="模型排行" :bordered="false" class="chart-card">
          <NDataTable
            :columns="modelColumns"
            :data="byModel"
            :loading="loading"
            :bordered="false"
            size="small"
            :max-height="260"
            :row-key="(r: ModelStatsRow) => r.model"
          />
        </NCard>
      </NGridItem>
    </NGrid>
  </div>
</template>

<style scoped>
.page {
  max-width: 1560px;
  margin: 0 auto;
}

.page-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 16px;
}

h2 {
  margin: 0 0 4px;
  font-size: 20px;
  font-weight: 600;
}

.sub {
  margin: 0;
  font-size: 12px;
  opacity: 0.6;
}

.stat-row {
  display: grid;
  grid-template-columns: repeat(auto-fit, minmax(178px, 1fr));
  gap: 12px;
  margin-top: 12px;
}

.chart-card {
  border-radius: 12px;
  box-shadow: 0 1px 2px rgba(16, 24, 40, 0.04), 0 4px 16px rgba(16, 24, 40, 0.06);
}

/* 让卡片撑满栅格行高：否则内容少的卡片（比如只有几行的模型排行）
   底部会明显短于同排的图表卡，一排卡片的底边参差不齐。 */
.chart-card {
  height: 100%;
}
</style>
