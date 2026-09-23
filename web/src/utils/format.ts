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

/** 四档分档的档位。全站统一：优秀(绿) → 良好(蓝) → 一般(琥珀) → 差(红)。 */
export type Grade = 'excellent' | 'good' | 'fair' | 'poor'

/**
 * 全站唯一的配色表。
 *
 * 颜色只在这里定义一次：同一个档位在任何指标、任何页面上都必须是同一个颜色。
 * 「绿」如果在列表里表示优秀、在总览里表示另一件事，颜色就不再是信息，
 * 只是装饰。中间两档用蓝色而不是灰色 —— 灰色读起来像「没有数据」，
 * 把「正常」和「没测到」混在一起同样是失去意义。
 */
export const GRADE_STYLE: Record<Grade, { label: string; color: string }> = {
  excellent: { label: '优秀', color: '#18a058' },
  good: { label: '良好', color: '#2080f0' },
  fair: { label: '一般', color: '#f0a020' },
  poor: { label: '差', color: '#d03050' },
}

/** 一个指标的档位边界。better 说明数值往哪个方向算好。 */
interface GradeBounds {
  better: 'lower' | 'higher'
  excellent: number
  good: number
  fair: number
}

/**
 * 各指标的档位边界，全站唯一出处。
 *
 * 边界整体放得比较宽，目标是让绝大多数正常请求落在优秀/良好，只有真正异常
 * （首 token 十几秒、总耗时几分钟、缓存基本没命中）才掉进一般和差。
 * 阈值定太严会让列表长期一片红与琥珀，反而看不出哪一次是真的有问题。
 *
 * 首 token 10 秒、总耗时 60 秒算优秀，是照着 agent 客户端这种动辄几十万 token
 * 提示词的场景定的：这类请求本来就要等，拿普通聊天接口的标准衡量会满屏告警。
 */
export const GRADES = {
  /** 首 token 耗时（毫秒） */
  ttftMs: { better: 'lower', excellent: 10_000, good: 20_000, fair: 30_000 },
  /** 总耗时（毫秒） */
  totalMs: { better: 'lower', excellent: 60_000, good: 120_000, fair: 300_000 },
  /** 生成速度（token/秒） */
  tps: { better: 'higher', excellent: 200, good: 120, fair: 60 },
  /** 缓存命中率（百分比）。越高越省，所以门槛也最高档。 */
  cacheRate: { better: 'higher', excellent: 90, good: 60, fair: 30 },
  /** 成功率（百分比） */
  successRate: { better: 'higher', excellent: 98, good: 95, fair: 85 },
  /** 思维链 token 数 */
  reasoningTokens: { better: 'lower', excellent: 500, good: 1500, fair: 4000 },
} as const satisfies Record<string, GradeBounds>

/** 可分档的指标名。 */
export type GradeMetric = keyof typeof GRADES

/**
 * 把数值判成一个档位。
 *
 * 0 或缺失返回 undefined 而不是「差」：这些指标里的 0 通常只意味着「没测到」
 * （非流式请求没有 TPS、上游没回缓存字段），标红属于误报。
 */
export function gradeOf(value: number, metric: GradeMetric): Grade | undefined {
  if (!value || value <= 0) return undefined
  const b: GradeBounds = GRADES[metric]
  if (b.better === 'higher') {
    if (value >= b.excellent) return 'excellent'
    if (value >= b.good) return 'good'
    if (value >= b.fair) return 'fair'
    return 'poor'
  }
  if (value <= b.excellent) return 'excellent'
  if (value <= b.good) return 'good'
  if (value <= b.fair) return 'fair'
  return 'poor'
}

/** 数值对应的颜色。缺失时返回 undefined，由调用方用默认文字色。 */
export function metricColor(value: number, metric: GradeMetric): string | undefined {
  const grade = gradeOf(value, metric)
  return grade ? GRADE_STYLE[grade].color : undefined
}

/**
 * 把某个指标的档位边界写成一句人话，用于列头提示。
 *
 * 由 GRADES 生成而不是手写：边界值和说明一旦分开维护，改阈值时必然有一处被忘掉，
 * 界面上就会显示错的边界 —— 那比不显示更糟。
 */
export function gradeLegend(metric: GradeMetric, fmt: (v: number) => string): string {
  const b: GradeBounds = GRADES[metric]
  const sign = b.better === 'higher' ? '≥' : '≤'
  return [
    `${GRADE_STYLE.excellent.label} ${sign} ${fmt(b.excellent)}`,
    `${GRADE_STYLE.good.label} ${sign} ${fmt(b.good)}`,
    `${GRADE_STYLE.fair.label} ${sign} ${fmt(b.fair)}`,
    `${GRADE_STYLE.poor.label} 其余`,
  ].join(' · ')
}

// 各指标的取色入口。保留语义化的名字，调用方不必记指标键名。
export const ttftColor = (ms: number) => metricColor(ms, 'ttftMs')
export const totalMsColor = (ms: number) => metricColor(ms, 'totalMs')
export const tpsColor = (v: number) => metricColor(v, 'tps')
export const cacheRateColor = (v: number) => metricColor(v, 'cacheRate')
export const successRateColor = (v: number) => metricColor(v, 'successRate')
export const reasoningTokenColor = (v: number) => metricColor(v, 'reasoningTokens')


/**
 * 额度用量的配色。
 *
 * 故意不并入上面那套四档：方向是反的 —— 其它指标都是「高 = 好」，
 * 而套餐额度是「用得越多越接近断供」，所以高百分比才是告警，
 * 「优秀/良好」这套话术套上去会自相矛盾。
 * 0 值给中性灰：窗口刚重置时用量为 0，标绿会让人以为「很健康」，
 * 其实那一刻没有任何信息。
 */
export function quotaColor(percent: number): string {
  if (percent <= 0) return 'rgba(128, 128, 128, 0.9)'
  if (percent >= 85) return '#d03050'
  if (percent >= 60) return '#f0a020'
  return '#18a058'
}

/**
 * 金额显示。额度多是几美元到几十美元，但单次请求成本可能只有千分之几，
 * 所以小额保留更多位数，避免全部显示成 $0.00。
 */
export function formatUSD(v: number | null | undefined): string {
  if (v === null || v === undefined) return '—'
  const abs = Math.abs(v)
  if (abs === 0) return '$0'
  if (abs < 0.01) return `$${v.toFixed(4)}`
  if (abs < 1000) return `$${v.toFixed(2)}`
  return `$${v.toLocaleString('en-US', { maximumFractionDigits: 2 })}`
}

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

/**
 * 未来时间点的相对描述（「4 小时后」）。
 *
 * 额度窗口的重置时间、计费周期的结束时间都在未来，用 formatRelative 会算出负的差值，
 * 落进它的「小于 1 秒」分支显示成「刚刚」——把「还有 4 小时重置」说成「刚刚重置」。
 */
export function formatUntil(iso: string | null | undefined): string {
  if (!iso) return '—'
  const d = new Date(iso).getTime()
  if (Number.isNaN(d)) return iso
  const diff = d - Date.now()
  if (diff <= 0) return '即将重置'
  if (diff < 60_000) return '不到 1 分钟后'
  if (diff < 3_600_000) return `${Math.floor(diff / 60_000)} 分钟后`
  if (diff < 86_400_000) return `${Math.floor(diff / 3_600_000)} 小时后`
  return `${Math.floor(diff / 86_400_000)} 天后`
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
