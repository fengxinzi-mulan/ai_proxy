/* 图表配色：与 tokens.css 保持同一套语义。
 *
 * ECharts 不认识 CSS 变量，所以这里必须有 JS 侧的副本；改颜色时
 * 两边一起改。图表序列色按顺序取用，保证同一指标在任何图里颜色一致。
 */

export interface ChartPalette {
  /** 按顺序分配给多序列图的颜色 */
  series: string[]
  axisLine: string
  axisLabel: string
  splitLine: string
  legendText: string
  tooltipBg: string
  tooltipBorder: string
  tooltipText: string
}

const light: ChartPalette = {
  series: ['#2563eb', '#16a34a', '#d97706', '#dc2626', '#7c3aed', '#0e9384'],
  axisLine: '#d0d5dd',
  axisLabel: '#8a94a6',
  splitLine: '#eef0f3',
  legendText: '#475467',
  tooltipBg: '#ffffff',
  tooltipBorder: '#e4e7ec',
  tooltipText: '#101828',
}

const dark: ChartPalette = {
  series: ['#4f8cff', '#34d399', '#fbbf24', '#f87171', '#a78bfa', '#2dd4bf'],
  axisLine: '#343b45',
  axisLabel: '#6f7887',
  splitLine: '#22262e',
  legendText: '#a4acba',
  tooltipBg: '#1a1e25',
  tooltipBorder: '#2b313a',
  tooltipText: '#e6e8ec',
}

export function chartPalette(dark_: boolean): ChartPalette {
  return dark_ ? dark : light
}

/** 指标分档色：数值好坏共用一个口径，亮暗各一套。 */
export interface GradePalette {
  good: string
  ok: string
  warn: string
  bad: string
  muted: string
}

const gradeLight: GradePalette = {
  good: '#16a34a',
  ok: '#2563eb',
  warn: '#d97706',
  bad: '#dc2626',
  muted: '#8a94a6',
}

const gradeDark: GradePalette = {
  good: '#34d399',
  ok: '#4f8cff',
  warn: '#fbbf24',
  bad: '#f87171',
  muted: '#6f7887',
}

export function gradePalette(dark_: boolean): GradePalette {
  return dark_ ? gradeDark : gradeLight
}

/** 语义色，用于状态标签与额度条。 */
export function semanticColor(
  kind: 'success' | 'warning' | 'danger' | 'info' | 'accent',
  dark_: boolean,
): string {
  const map = dark_
    ? {
        success: '#34d399',
        warning: '#fbbf24',
        danger: '#f87171',
        info: '#2dd4bf',
        accent: '#4f8cff',
      }
    : {
        success: '#16a34a',
        warning: '#d97706',
        danger: '#dc2626',
        info: '#0e9384',
        accent: '#2563eb',
      }
  return map[kind]
}
