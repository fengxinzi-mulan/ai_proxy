// 界面上的格式化工具。集中放在这里，保证各页面的数字口径一致。

/** 把毫秒格式化成便于扫读的形式。 */
export function formatDuration(ms: number | null | undefined): string {
  if (ms === null || ms === undefined || ms <= 0) return '—'
  if (ms < 1000) return `${Math.round(ms)} ms`
  if (ms < 60_000) return `${(ms / 1000).toFixed(2)} s`
  const m = Math.floor(ms / 60_000)
  const s = Math.round((ms % 60_000) / 1000)
  return `${m}m ${s}s`
}

/** 大数字加千分位。 */
export function formatNumber(v: number | null | undefined): string {
  if (v === null || v === undefined) return '—'
  return v.toLocaleString('en-US')
}

/** 紧凑数字，用于指标卡（1.2K / 3.4M）。 */
export function formatCompact(v: number | null | undefined): string {
  if (v === null || v === undefined) return '—'
  const abs = Math.abs(v)
  if (abs < 1000) return String(v)
  if (abs < 1_000_000) return `${(v / 1000).toFixed(abs < 10_000 ? 1 : 0)}K`
  if (abs < 1_000_000_000) return `${(v / 1_000_000).toFixed(2)}M`
  return `${(v / 1_000_000_000).toFixed(2)}B`
}

export function formatPercent(v: number | null | undefined, digits = 1): string {
  if (v === null || v === undefined) return '—'
  return `${v.toFixed(digits)}%`
}

export function formatTps(v: number | null | undefined): string {
  if (!v || v <= 0) return '—'
  return v.toFixed(1)
}

/**
 * 按百分比数值给出配色：达到 good 为绿色，达到 warn 为琥珀色，其余为红色。
 *
 * neutralAtZero 为 true 时把 0 当作「未启用」而不判为差，返回 undefined 用默认文字色 ——
 * 缓存命中率为 0 可能只是上游根本不支持提示词缓存，标红属于误报。
 */
export function rateAccent(
  rate: number,
  good: number,
  warn: number,
  neutralAtZero = false,
): string | undefined {
  if (neutralAtZero && rate <= 0) return undefined
  if (rate >= good) return '#18a058'
  if (rate >= warn) return '#f0a020'
  return '#d03050'
}

/** 成功率配色阈值。代理会记录客户端取消、限流等各类失败，95% 以上算健康。 */
export const SUCCESS_RATE = { good: 95, warn: 85 } as const

/** 缓存命中率配色阈值。命中率取决于客户端是否复用前缀，60% 以上算工作良好。 */
export const CACHE_RATE = { good: 60, warn: 20 } as const

/**
 * 缓存命中率（百分比）：命中缓存的输入量 ÷ 输入总量。
 *
 * 输入总量本身已包含缓存部分（各家的原始口径已在服务端统一），
 * 所以比值天然落在 0–100 之间，不需要再做归一。输入量为 0 时返回 0。
 */
export function cacheHitRateOf(cachedTokens: number, promptTokens: number): number {
  if (!promptTokens || promptTokens <= 0) return 0
  return (cachedTokens / promptTokens) * 100
}

/** 后端时间统一是 UTC，这里转成本地时间显示。 */
export function formatTime(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleTimeString('zh-CN', { hour12: false })
}

export function formatDateTime(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  return d.toLocaleString('zh-CN', { hour12: false })
}

/**
 * 固定格式的本地日期时间（YYYY-MM-DD HH:mm:ss）。
 *
 * 与 formatDateTime 的区别是不走 toLocaleString —— 后者在 zh-CN 下输出
 * 「2026/9/22 17:48:20」，月日不补零、宽度还会随月份位数跳动，在表格列里
 * 对不齐。这里统一补零，配合 tabular-nums 得到整齐的一列。
 */
export function formatStamp(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso)
  if (Number.isNaN(d.getTime())) return iso
  const p = (n: number) => String(n).padStart(2, '0')
  return `${d.getFullYear()}-${p(d.getMonth() + 1)}-${p(d.getDate())} ${p(d.getHours())}:${p(d.getMinutes())}:${p(d.getSeconds())}`
}

/** 相对时间，用于「多久以前」。 */
export function formatRelative(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso).getTime()
  if (Number.isNaN(d)) return iso
  const diff = Date.now() - d
  if (diff < 1000) return '刚刚'
  if (diff < 60_000) return `${Math.floor(diff / 1000)} 秒前`
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟前`
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)} 小时前`
  return `${Math.floor(diff / 86_400_000)} 天前`
}

/** 字节数，用于报文长度。 */
export function formatBytes(n: number): string {
  if (n < 1024) return `${n} B`
  if (n < 1024 * 1024) return `${(n / 1024).toFixed(1)} KB`
  return `${(n / 1024 / 1024).toFixed(2)} MB`
}

/** 请求结果的语义分类，决定标签颜色。 */
export type LogOutcome = 'success' | 'cancelled' | 'client-error' | 'server-error' | 'error'

export function logOutcome(log: {
  success: boolean
  cancelled: boolean
  httpStatus: number
}): LogOutcome {
  if (log.cancelled) return 'cancelled'
  if (log.success) return 'success'
  if (log.httpStatus >= 400 && log.httpStatus < 500) return 'client-error'
  if (log.httpStatus >= 500) return 'server-error'
  return 'error'
}

export const OUTCOME_LABELS: Record<LogOutcome, string> = {
  success: '成功',
  cancelled: '已取消',
  'client-error': '客户端错误',
  'server-error': '上游错误',
  error: '失败',
}

/** 改写标记的可读名称。 */
export const MODIFICATION_LABELS: Record<string, string> = {
  usage_inject: '注入 include_usage',
  prompt_append: '追加提示词',
  prompt_replace: '替换提示词',
  prompt_prepend_user: '前置提示词',
}

export function modificationLabel(key: string): string {
  return MODIFICATION_LABELS[key] ?? key
}

/**
 * 把后端的时间桶标签渲染成横轴刻度。
 *
 * 桶标签是 SQLite 按 UTC 生成的（例如 2026-09-22T08:00Z），直接显示会让
 * 用户对不上本地时间 —— 日志列明明显示 16:13，图上却落在 08:00。
 * 这里把小时/分钟粒度的标签换算成本地时间，天粒度只取月日（跨时区换算日期没有意义）。
 */
export function formatBucketLabel(bucketKey: string): string {
  const pad = (n: number) => String(n).padStart(2, '0')

  if (!bucketKey.includes('T')) {
    // 天粒度：2026-09-22
    return bucketKey.slice(5)
  }
  const d = new Date(bucketKey)
  if (Number.isNaN(d.getTime())) return bucketKey
  if (bucketKey.length <= 17) {
    // 小时粒度：2026-09-22T08:00Z
    return `${pad(d.getHours())}:00`
  }
  return `${pad(d.getHours())}:${pad(d.getMinutes())}`
}

/** 桶标签是否带具体时刻（决定横轴是否需要更细的刻度）。 */
export function bucketHasTime(bucketKey: string): boolean {
  return bucketKey.includes('T')
}

/**
 * 按分桶粒度枚举 [from, to] 区间内的所有时间桶。
 *
 * 后端只返回「有请求的那些桶」，直接画图会得到一个横跨整个宽度的大柱子，
 * 看不出时间分布。这里补齐空桶，让横轴始终覆盖所选的整个范围。
 *
 * 后端的桶标签来自 SQLite 对 UTC 时间戳做的 strftime，因此这里也必须按 UTC 生成。
 */
export function enumerateBuckets(fromIso: string, toIso: string, bucket: string): string[] {
  const from = new Date(fromIso)
  const to = new Date(toIso)
  if (Number.isNaN(from.getTime()) || Number.isNaN(to.getTime()) || from > to) return []

  const stepMs = bucket === 'minute' ? 60_000 : bucket === 'day' ? 86_400_000 : 3_600_000
  const pad = (n: number) => String(n).padStart(2, '0')

  // 对齐到桶边界，与 SQLite 的截断行为保持一致。
  const aligned = new Date(
    Date.UTC(
      from.getUTCFullYear(),
      from.getUTCMonth(),
      from.getUTCDate(),
      bucket === 'day' ? 0 : from.getUTCHours(),
      bucket === 'minute' ? from.getUTCMinutes() : 0,
    ),
  )

  const out: string[] = []
  // 上限兜底，避免区间异常时生成海量刻度把页面卡死。
  const maxBuckets = 800
  for (let t = aligned.getTime(); t <= to.getTime() && out.length < maxBuckets; t += stepMs) {
    const d = new Date(t)
    if (bucket === 'day') {
      out.push(`${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}`)
    } else if (bucket === 'minute') {
      out.push(
        `${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}T${pad(d.getUTCHours())}:${pad(d.getUTCMinutes())}:00Z`,
      )
    } else {
      out.push(`${d.getUTCFullYear()}-${pad(d.getUTCMonth() + 1)}-${pad(d.getUTCDate())}T${pad(d.getUTCHours())}:00Z`)
    }
  }
  return out
}

/** 尽量把 JSON 字符串排版好；不是 JSON 就原样返回。 */
export function prettyJSON(raw: string | undefined | null): string {
  if (!raw) return ''
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
}
