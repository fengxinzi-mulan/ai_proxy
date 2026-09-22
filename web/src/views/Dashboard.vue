<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { useRouter } from 'vue-router'
import {
  NCard,
  NGrid,
  NGridItem,
  NSelect,
  NSpace,
  NTag,
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
  CACHE_RATE,
  SUCCESS_RATE,
  formatBucketLabel,
  formatCompact,
  formatDuration,
  formatNumber,
  formatPercent,
  formatTps,
  enumerateBuckets,
  rateAccent,
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

/** 成功率按档位着色：健康区间是绿色，轻微失败转琥珀，明显异常才标红。 */
const successAccent = computed(() => {
  if (!stats.value || stats.value.totalRequests <= 0) return undefined
  return rateAccent(stats.value.successRate, SUCCESS_RATE.good, SUCCESS_RATE.warn)
})

/** 命中率为 0 不标红：很多上游压根不支持提示词缓存，那不是故障。 */
const cacheAccent = computed(() => {
  if (!stats.value) return undefined
  return rateAccent(stats.value.cacheHitRate, CACHE_RATE.good, CACHE_RATE.warn, true)
})

/** 请求量与 token 的双轴趋势图。 */
const trendOption = computed<EChartsOption>(() => {
  const labels = points.value.map((p) => formatBucketLabel(p.bucket))
  return {
    tooltip: { trigger: 'axis', axisPointer: { type: 'cross' } },
    legend: {
      data: ['请求数', '失败数', '输入 token', '缓存 token', '输出 token', '缓存命中率'],
      top: 0,
      itemGap: 12,
      textStyle: { fontSize: 11 },
    },
    // 右侧要放两个轴：token 量级与命中率百分比。轴的刻度与轴名都画在绘图区之外，
    // 留窄了第二个轴就会被卡片边缘裁掉，所以这里给足 150px。
    // 顶部留 56px：窄窗口下 6 项图例会折成两行，留窄了会压到绘图区上沿。
    grid: { left: 48, right: 150, top: 56, bottom: 40 },
    xAxis: {
      type: 'category',
      data: labels,
      axisLabel: { hideOverlap: true },
    },
    // 三个轴都不写 name：图例已经逐条标注了系列名，再写轴名在窄窗口下会互相重叠。
    // 单位直接体现在刻度上（命中率轴补 %）。
    yAxis: [
      { type: 'value', minInterval: 1 },
      { type: 'value' },
      {
        type: 'value',
        min: 0,
        max: 100,
        position: 'right',
        offset: 56,
        splitLine: { show: false },
        axisLabel: { formatter: '{value}%' },
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

/** 性能趋势：首 token 耗时与 TPS。 */
const perfOption = computed<EChartsOption>(() => {
  const labels = points.value.map((p) => formatBucketLabel(p.bucket))
  return {
    tooltip: { trigger: 'axis' },
    legend: { data: ['平均首 token', '平均 TPS'], top: 0 },
    grid: { left: 60, right: 60, top: 44, bottom: 40 },
    xAxis: {
      type: 'category',
      data: labels,
      axisLabel: { hideOverlap: true },
    },
    yAxis: [
      { type: 'value', name: 'ms' },
      { type: 'value', name: 'token/s' },
    ],
    series: [
      {
        name: '平均首 token',
        type: 'line',
        smooth: true,
        data: points.value.map((p) => Math.round(p.avgTtftMs)),
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

const activeProvider = computed(() => store.activeProvider)
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

    <!-- 当前生效供应商：这是最常确认的一件事，放在最显眼的位置 -->
    <NCard size="small" class="active-card" :bordered="false">
      <div class="active-row">
        <div>
          <div class="active-label">当前生效供应商</div>
          <div class="active-name">
            <template v-if="activeProvider">
              {{ activeProvider.displayName }}
              <NTag size="tiny" :bordered="false" type="info">{{ activeProvider.name }}</NTag>
              <NTag size="tiny" :bordered="false">{{ activeProvider.apiFormat }}</NTag>
            </template>
            <span v-else class="muted">未配置</span>
          </div>
          <div v-if="activeProvider" class="mono muted">{{ activeProvider.baseUrl }}</div>
        </div>
        <div class="active-right">
          <div class="muted small">客户端 base_url</div>
          <code class="mono">{{ store.listenBaseURL }}</code>
          <div class="muted small" style="margin-top: 6px">
            也可用 <code class="mono">/p/{{ activeProvider?.name ?? '&lt;短名&gt;' }}/…</code> 显式指定
          </div>
        </div>
      </div>
    </NCard>

    <!-- 指标卡用 CSS grid 的 auto-fit：列数随宽度自动增减，
         不用为每个断点单独写栅格跨度。 -->
    <div class="stat-row">
      <StatCard label="请求总数" :value="formatNumber(stats?.totalRequests ?? 0)" :loading="loading" />
      <StatCard
        label="成功率"
        :value="formatPercent(stats?.successRate)"
        :hint="`成功 ${formatNumber(stats?.successRequests ?? 0)} / 失败 ${formatNumber(stats?.failedRequests ?? 0)}。≥${SUCCESS_RATE.good}% 绿色，≥${SUCCESS_RATE.warn}% 琥珀色，更低为红色`"
        :accent="successAccent"
        :loading="loading"
      />
      <StatCard
        label="缓存命中率"
        :value="formatPercent(stats?.cacheHitRate)"
        :hint="`输入中命中缓存 ${formatCompact(stats?.cachedTokens ?? 0)} / 共 ${formatCompact(stats?.promptTokens ?? 0)}。≥${CACHE_RATE.good}% 绿色，≥${CACHE_RATE.warn}% 琥珀色。命中率越高，重复内容的请求越便宜、首 token 越快`"
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

.active-card,
.chart-card {
  border-radius: 12px;
  box-shadow: 0 1px 2px rgba(16, 24, 40, 0.04), 0 4px 16px rgba(16, 24, 40, 0.06);
}

/* 让卡片撑满栅格行高：否则内容少的卡片（比如只有几行的模型排行）
   底部会明显短于同排的图表卡，一排卡片的底边参差不齐。 */
.chart-card {
  height: 100%;
}

.active-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 24px;
  flex-wrap: wrap;
}

.active-label {
  font-size: 12px;
  opacity: 0.6;
}

.active-name {
  font-size: 17px;
  font-weight: 600;
  margin: 4px 0 2px;
  display: flex;
  align-items: center;
  gap: 8px;
}

.active-right {
  text-align: right;
}

.small {
  font-size: 11px;
}

.muted {
  opacity: 0.6;
}
</style>
