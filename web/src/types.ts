// 与后端 model 包一一对应的类型定义。
// 字段名保持和 Go 的 JSON 标签一致，改动后端时要同步改这里。

export type APIFormat = 'openai' | 'openai-responses' | 'anthropic' | 'gemini' | 'custom'

export const API_FORMAT_LABELS: Record<APIFormat, string> = {
  openai: 'OpenAI Chat Completions',
  'openai-responses': 'OpenAI Responses',
  anthropic: 'Anthropic Messages',
  gemini: 'Google Gemini',
  custom: '自定义网关',
}

export type OverrideMode = 'inherit' | 'custom' | 'disable'
export type PromptStrategy = 'append' | 'replace' | 'prepend_user'

export const PROMPT_STRATEGY_LABELS: Record<PromptStrategy, string> = {
  append: '追加到已有 system',
  replace: '替换已有 system',
  prepend_user: '作为首条 user 消息插入',
}

export interface APIKey {
  id: string
  key: string
  remark: string
  enabled: boolean
}

export interface KeyHealth {
  cooling: boolean
  cooldownMs: number
  recoverAt: string
  lastError: string
}

export interface ProxyConfig {
  enabled: boolean
  type: 'http' | 'socks5'
  url: string
  username: string
  password: string
  noProxy: string[]
}

export interface PromptConfig {
  enabled: boolean
  text: string
  strategy: PromptStrategy
}

export interface PromptRule {
  id: string
  enabled: boolean
  modelPattern: string
  text: string
  strategy: PromptStrategy
}

export interface CustomUsageMapping {
  promptTokens: string
  cachedTokens: string
  cacheWriteTokens: string
  completionTokens: string
  reasoningTokens: string
  totalTokens: string
  model: string
}

/** 一条模型映射：客户端请求的模型名与 from 完全相等时，实际发给上游的模型改成 to。 */
export interface ModelMapping {
  from: string
  to: string
}

/** 套餐余量查询配置。template 为空表示该供应商不启用用量查询。 */
export interface UsageQueryConfig {
  template: string
  /** 留空时由模板从 baseUrl 推导 */
  baseUrl: string
  /** 用量快照的刷新间隔（秒）。0 = 不自动刷新。刷新由服务端按此间隔执行。 */
  autoRefreshSeconds: number
  /**
   * 账号密码，目前只有 Mistral 模板在用。
   *
   * Mistral 的订阅额度只挂在网页后台（admin.mistral.ai），普通 API key 访问
   * Admin 端点会被拒（401），所以只能拿账号密码去换网页会话。服务端会自动登录
   * 并缓存会话，过期后自动重登 —— 不需要手工维护 Cookie。
   */
  account?: string
  password?: string
}

export interface UsageBalance {
  label: string
  amount: number
  /** ISO 币种代码（USD / CNY）；上游没给时为空。 */
  currency?: string
  /** 是否是「汇总余额」，它本身已包含下面的分项；算剩余额度时不能与分项相加。 */
  total?: boolean
  hint?: string
}

export interface UsageWindow {
  label: string
  used: number
  cap: number
  /** 0-100，服务端算好，避免各处口径不一致 */
  percent: number
  resetAt: string | null
  exceeded: boolean
  /** 这个数字怎么来的。推导出来的窗口（上游没直接给）才有，界面上以悬浮提示展示。 */
  hint?: string
}

export type UsageStatUnit = 'usd' | 'count' | 'percent' | 'tokens'

export interface UsageStat {
  label: string
  value: number
  unit: UsageStatUnit
  hint?: string
}

export interface UsageTemplateInfo {
  id: string
  name: string
  description: string
}

/**
 * 一次用量查询的结果。
 *
 * ok=false 不等于请求失败：上游那组接口没有公开契约，可能只读到一部分，
 * 这时 warnings 里会写明哪一段不可用，raw 里保留原始响应供排查。
 * 读不到的段落一律留空，不会显示成 0。
 */
export interface UsageSnapshot {
  ok: boolean
  providerId: number
  template: string
  templateName: string
  fetchedAt: string
  latencyMs: number
  account: string
  planId: string
  planName: string
  status: string
  periodStart: string | null
  periodEnd: string | null
  balances: UsageBalance[]
  windows: UsageWindow[]
  period: UsageStat[]
  warnings: string[]
  error: string
  raw?: Record<string, unknown>
  usedUrls: string[]
}

export interface Provider {
  id: number
  name: string
  displayName: string
  remark: string
  enabled: boolean
  active: boolean
  sortOrder: number

  baseUrl: string
  apiFormat: APIFormat
  customPath: string

  keys: APIKey[]
  authHeader: string
  authPrefix: string
  extraHeaders: Record<string, string>

  timeoutSeconds: number
  connectTimeoutSeconds: number
  insecureSkipTls: boolean
  proxyMode: 'inherit' | 'custom' | 'direct'
  proxy: ProxyConfig

  promptMode: OverrideMode
  prompt: PromptConfig
  promptRules: PromptRule[]

  /** 模型映射（只做精确匹配，未命中的请求原样透传）。 */
  modelMap: ModelMapping[]

  /**
   * Responses 会话续传。默认 off。
   * dry-run 只统计前缀命中与可节省字节，不改请求；on 才会写 store/previous_response_id。
   */
  continuationMode: 'off' | 'dry-run' | 'on'
  /** 可选的模型范围，留空表示全部模型（不是内置白名单）。 */
  continuationModels: string[]
  /** 不支持类错误的冷却秒数；0 表示不冷却。 */
  continuationCooldownSeconds: number
  /** response id 失效的冷却秒数；0 表示回退后下一轮立刻重新建链。 */
  continuationInvalidCooldownSeconds: number

  usageInjectMode: 'inherit' | 'on' | 'off'
  stripUsageChunk: boolean

  /** 严格上游兼容：剔除非标准字段（thinking 等顶层字段、assistant 消息里的 reasoning_content）。默认关闭。 */
  stripUnsupportedParams: boolean

  /**
   * prompt_cache_key 注入模式。auto 时按「会话头 → 首条消息+tools 指纹」派生一个稳定值，
   * 提升上游前缀缓存命中率；不认这个字段的严格网关会被 422，所以默认 off。
   */
  promptCacheKeyMode: 'off' | 'auto'

  customUsage: CustomUsageMapping | null

  usageQuery: UsageQueryConfig

  tags: string[]
  createdAt: string
  updatedAt: string
  keyHealth?: Record<string, KeyHealth>
  /**
   * 服务端定时刷出来的最近一次用量快照，随供应商列表一起下发。
   * null 表示服务端还没查过（或该供应商没配模板）。
   */
  usage?: UsageSnapshot | null
}

/**
 * 请求体内联图片压缩配置。
 *
 * 触发条件是「或」：字节数超阈值，或者长边超上限。只按字节判断会漏掉
 * 「体积不大但分辨率极高」的截图 —— 那类图对上游的视觉 token 消耗最大。
 */
export interface ImageCompressSettings {
  enabled: boolean
  /** 触发压缩的原始图片字节阈值。 */
  thresholdBytes: number
  /** 长边上限（像素）。0 表示只按字节判断，不做缩放。 */
  maxEdge: number
  /** JPEG 输出质量（1-100），只对不透明图片生效。 */
  quality: number
}

export interface Settings {
  host: string
  port: number
  accessKey: string
  proxy: ProxyConfig
  globalPrompt: PromptConfig
  usageInjectDefault: boolean
  defaultTimeoutSeconds: number
  defaultConnectTimeoutSeconds: number
  imageCompress: ImageCompressSettings
  storeBodies: boolean
  maxBodyBytes: number
  retentionDays: number
  maxLogs: number
}

export interface RequestLog {
  id: number
  tsStart: string
  tsEnd: string
  providerId: number
  providerName: string
  upstreamUrl: string
  proxyUsed: string
  method: string
  path: string
  model: string
  modelResponse: string
  /** 「模型映射」命中后实际发给上游的模型名；未命中为空。 */
  modelMapped: string
  apiFormat: string
  stream: boolean
  reasoningEffort: string
  clientIp: string
  requestModified: boolean
  responseModified: boolean
  modifications: string[]
  /** 续传留痕：seed/continued/fallback/unsupported/dry-hit/dry-miss。 */
  continuationMode: string
  continuationReason: string
  savedBytes: number
  /** 本次请求压缩了多少张内联图片。 */
  imageCompressed: number
  /** 压缩省下的请求字节数（base64 串长度差）。 */
  imageSavedBytes: number
  httpStatus: number
  success: boolean
  errorMsg: string
  cancelled: boolean
  promptTokens: number
  cachedTokens: number
  cacheWriteTokens: number
  completionTokens: number
  reasoningTokens: number
  totalTokens: number
  tokensEstimated: boolean
  ttftMs: number
  totalMs: number
  tps: number
  reqHeaders?: string
  reqBody?: string
  reqBodyOriginal?: string
  respBody?: string
}

export interface OverviewStats {
  totalRequests: number
  successRequests: number
  failedRequests: number
  successRate: number
  promptTokens: number
  cachedTokens: number
  completionTokens: number
  totalTokens: number
  /** 缓存命中率（百分比）：命中缓存的输入量 ÷ 输入总量。 */
  cacheHitRate: number
  avgTtftMs: number
  avgTps: number
  avgDurationMs: number
}

export interface TimeSeriesPoint {
  bucket: string
  requests: number
  failed: number
  promptTokens: number
  cachedTokens: number
  completionTokens: number
  cacheHitRate: number
  avgTtftMs: number
  avgTps: number
}

export interface ModelStatsRow {
  model: string
  requests: number
  failed: number
  promptTokens: number
  cachedTokens: number
  completionTokens: number
  cacheHitRate: number
  avgTtftMs: number
  avgTps: number
}

export interface ProviderStatsRow {
  providerId: number
  providerName: string
  requests: number
  failed: number
  promptTokens: number
  cachedTokens: number
  completionTokens: number
  cacheHitRate: number
  avgTtftMs: number
  avgTps: number
}

export interface LiveRequest {
  id: string
  providerName: string
  model: string
  path: string
  stream: boolean
  clientIp: string
  startedAt: string
  elapsedMs: number
  ttftMs: number
  completionTokens: number
  tps: number
}

export interface ProbeResult {
  ok: boolean
  status: number
  latencyMs: number
  message: string
  usedUrl: string
  models: string[] | null
  authOk: boolean
}

export interface LogPage {
  items: RequestLog[]
  total: number
  page: number
  pageSize: number
}

/** 一条「这个预设会改什么」的说明。 */
export interface PresetChange {
  field: string
  value: string
  reason: string
}

/**
 * 供应商优化预设：按上游厂商给的一整套推荐配置。
 *
 * 字段是「可选」的：只有出现的项才会被写进供应商配置，没出现的保持原值。
 */
export interface ProviderPreset {
  id: string
  title: string
  summary: string
  /** 主机后缀匹配规则，例如 mistral.ai 同时命中 api.mistral.ai。 */
  hosts: string[]
  changes: PresetChange[]
  stripUnsupportedParams?: boolean
  promptCacheKeyMode?: 'off' | 'auto'
  usageInjectMode?: 'inherit' | 'on' | 'off'
  continuationMode?: 'off' | 'dry-run' | 'on'
  timeoutSeconds?: number
}

/** 数据库页级占用，用来展示「回收空间」能腾出多少。 */
export interface DBStats {
  pageCount: number
  freeCount: number
  pageSize: number
  fileBytes: number
  freeBytes: number
  freeRatio: number
}

export interface MetaInfo {
  formats: APIFormat[]
  strategies: PromptStrategy[]
  proxyTypes: string[]
  authDefaults: { format: APIFormat; header: string; prefix: string }[]
  /** 内置的套餐余量查询模板。 */
  usageTemplates: UsageTemplateInfo[]
  liveCount: number
  /** 进程实际监听的地址，可能与设置里的值不同（命令行参数优先）。 */
  listen: { host: string; port: number }
  /** 构建版本号；桌面版用于设置页展示。 */
  version?: string
  /** 是否运行在桌面窗口里。为 true 时才显示「桌面应用」设置页。 */
  desktop?: boolean
}

/** 后端通过 SSE 推送的事件。 */
export type ProxyEvent =
  | { type: 'log'; data: RequestLog }
  | { type: 'live'; data: LiveRequest[] }
  | { type: 'settings'; data: Settings }
  | { type: 'providers'; data: unknown }
  | { type: 'logs-cleared'; data: { deleted: number } }
  | { type: 'update'; data: UpdateState }
  | { type: 'desktop-close-ask'; data: null }

/** 开机自启的真实状态（读的是系统，不是数据库）。 */
export interface AutostartState {
  /** false 时 reason 说明原因，开关应禁用。 */
  supported: boolean
  reason: string
  enabled: boolean
  /** 本进程「应该」写入的启动项名字与命令行。 */
  name: string
  command: string
  /** 系统里当前实际注册的命令；未开启时为空。 */
  registered: string
  /** 已开启但注册的命令与当前应有的不一致（例如 exe 被挪了位置）。 */
  stale: boolean
}

/** 更新安装阶段。 */
export type UpdatePhase =
  | 'idle'
  | 'checking'
  | 'downloading'
  | 'verifying'
  | 'applying'
  | 'restarting'
  | 'failed'

/** 版本检测与安装的状态快照。 */
export interface UpdateState {
  currentVersion: string
  latestVersion: string
  hasUpdate: boolean
  /** 当前是未注入版本号的开发构建，不参与比较。 */
  dev: boolean
  /** 用户显式跳过过这个版本：自动弹框不弹，手动检查仍能看见。 */
  skipped: boolean
  skippedVersion: string
  notes: string
  htmlUrl: string
  publishedAt: string
  assetName: string
  assetSize: number
  /** false 时界面把主按钮换成「打开下载页」。 */
  canAutoInstall: boolean
  checkedAt: string

  phase: UpdatePhase
  /** 0-100。 */
  progress: number
  downloadedBytes: number
  error: string
}
