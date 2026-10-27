<script setup lang="ts">
// ECharts 的轻量封装：只注册用到的图表与组件，避免把整包图表库打进产物。
import { onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import * as echarts from 'echarts/core'
import { BarChart, LineChart, PieChart } from 'echarts/charts'
import { GridComponent, LegendComponent, TooltipComponent } from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
import type { EChartsOption } from 'echarts'
import { useAppStore } from '@/stores/app'
import { chartPalette } from '@/theme/palette'

echarts.use([
  LineChart,
  BarChart,
  PieChart,
  GridComponent,
  LegendComponent,
  TooltipComponent,
  CanvasRenderer,
])

const props = defineProps<{
  option: EChartsOption
  height?: string
}>()

const store = useAppStore()
const el = ref<HTMLDivElement | null>(null)
const chart = shallowRef<echarts.ECharts | null>(null)
let observer: ResizeObserver | null = null

/**
 * 生成与站点主题一致的图表基础样式。
 *
 * 不用 ECharts 内置的 'dark' 主题：它会把背景刷成另一种黑，
 * 和自绘卡片对不上；这里只改轴线/文字/tooltip，让图表融进页面。
 */
function baseTheme(dark: boolean): EChartsOption {
  const p = chartPalette(dark)
  return {
    color: p.series,
    backgroundColor: 'transparent',
    textStyle: { color: p.legendText, fontSize: 11 },
    legend: {
      textStyle: { color: p.legendText, fontSize: 11 },
      itemWidth: 10,
      itemHeight: 10,
      icon: 'roundRect',
    },
    tooltip: {
      backgroundColor: p.tooltipBg,
      borderColor: p.tooltipBorder,
      borderWidth: 1,
      textStyle: { color: p.tooltipText, fontSize: 12 },
      extraCssText: 'box-shadow: 0 8px 24px rgba(16,24,40,.12); border-radius: 8px;',
      axisPointer: {
        lineStyle: { color: p.axisLine },
        crossStyle: { color: p.axisLine },
      },
    },
  }
}

/**
 * 把主题默认值补进调用方的 option。
 *
 * 必须逐个子对象合并：setOption 对 legend/tooltip 是「整体替换」，
 * 直接 spread 会让调用方写的 { legend: { data } } 把主题里的文字色一起顶掉，
 * 结果暗色下图例变成默认深色、几乎看不见。
 */
function mergeTheme(theme: EChartsOption, opt: EChartsOption): EChartsOption {
  const tLegend = (theme.legend ?? {}) as Record<string, unknown>
  const oLegend = (opt.legend ?? {}) as Record<string, unknown>
  const tTooltip = (theme.tooltip ?? {}) as Record<string, unknown>
  const oTooltip = (opt.tooltip ?? {}) as Record<string, unknown>

  return {
    ...theme,
    ...opt,
    legend: {
      ...tLegend,
      ...oLegend,
      textStyle: {
        ...((tLegend.textStyle ?? {}) as Record<string, unknown>),
        ...((oLegend.textStyle ?? {}) as Record<string, unknown>),
      },
    },
    tooltip: {
      ...tTooltip,
      ...oTooltip,
      textStyle: {
        ...((tTooltip.textStyle ?? {}) as Record<string, unknown>),
        ...((oTooltip.textStyle ?? {}) as Record<string, unknown>),
      },
    },
    textStyle: {
      ...((theme.textStyle ?? {}) as Record<string, unknown>),
      ...((opt.textStyle ?? {}) as Record<string, unknown>),
    },
  }
}

function render() {
  if (!chart.value) return
  // 合并更新而不是 notMerge：这里的系列集合是固定的，合并能让数据变化平滑过渡，
  // 而不是每次刷新都整图重建、把入场动画重播一遍。
  // 动画时长压到 500ms（默认 1 秒），仪表盘切来切去时不至于总在「正在画」。
  chart.value.setOption(
    {
      ...mergeTheme(baseTheme(store.dark), props.option),
      animationDuration: 500,
      animationDurationUpdate: 300,
    },
    false,
  )
}

function mount() {
  if (!el.value) return
  chart.value = echarts.init(el.value, undefined, { renderer: 'canvas' })
  render()
}

onMounted(() => {
  mount()
  if (!el.value) return
  observer = new ResizeObserver(() => chart.value?.resize())
  observer.observe(el.value)
})

onBeforeUnmount(() => {
  observer?.disconnect()
  chart.value?.dispose()
  chart.value = null
})

watch(() => props.option, render, { deep: true })

// 主题切换不需要重建实例：这里不依赖 ECharts 主题，只重新 setOption 即可。
watch(
  () => store.dark,
  () => {
    if (!chart.value) {
      mount()
      return
    }
    render()
  },
)
</script>

<template>
  <div ref="el" :style="{ width: '100%', height: height ?? '280px' }" />
</template>
