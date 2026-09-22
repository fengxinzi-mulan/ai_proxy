<script setup lang="ts">
// ECharts 的轻量封装：只注册用到的图表与组件，避免把整包图表库打进产物。
import { onBeforeUnmount, onMounted, ref, shallowRef, watch } from 'vue'
import * as echarts from 'echarts/core'
import { BarChart, LineChart, PieChart } from 'echarts/charts'
import {
  GridComponent,
  LegendComponent,
  TooltipComponent,
  DataZoomComponent,
} from 'echarts/components'
import { CanvasRenderer } from 'echarts/renderers'
import type { EChartsOption } from 'echarts'
import { useAppStore } from '@/stores/app'

echarts.use([
  LineChart,
  BarChart,
  PieChart,
  GridComponent,
  LegendComponent,
  TooltipComponent,
  DataZoomComponent,
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

function render() {
  if (!chart.value) return
  // 合并更新而不是 notMerge：这里的系列集合是固定的，合并能让数据变化平滑过渡，
  // 而不是每次刷新都整图重建、把入场动画重播一遍。
  // 动画时长压到 500ms（默认 1 秒），仪表盘切来切去时不至于总在「正在画」。
  chart.value.setOption({
    animationDuration: 500,
    animationDurationUpdate: 300,
    ...props.option,
  })
}

onMounted(() => {
  if (!el.value) return
  chart.value = echarts.init(el.value, store.dark ? 'dark' : undefined, { renderer: 'canvas' })
  render()
  observer = new ResizeObserver(() => chart.value?.resize())
  observer.observe(el.value)
})

onBeforeUnmount(() => {
  observer?.disconnect()
  chart.value?.dispose()
  chart.value = null
})

watch(() => props.option, render, { deep: true })

// 主题切换需要重建实例，ECharts 不支持运行中换主题。
watch(
  () => store.dark,
  () => {
    if (!el.value) return
    chart.value?.dispose()
    chart.value = echarts.init(el.value, store.dark ? 'dark' : undefined, { renderer: 'canvas' })
    render()
  },
)
</script>

<template>
  <div ref="el" :style="{ width: '100%', height: height ?? '280px' }" />
</template>
