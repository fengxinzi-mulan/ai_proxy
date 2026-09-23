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

/** 套餐余量查询配置。template 为空表示该供应商不启用用量查询。 */
export interface UsageQueryConfig {
  template: string
  /** 留空时由模板从 baseUrl 推导 */
  baseUrl: string
  /** 定时刷新间隔（秒）。0 = 关闭。只在管理页面打开时生效。 */
  autoRefreshSeconds: number
}

export interface UsageBalance {
  label: string
  amount: number
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

  usageInjectMode: 'inherit' | 'on' | 'off'
  stripUsageChunk: boolean

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

export interface Settings {
  host: string
  port: number
  accessKey: string
  proxy: ProxyConfig
  globalPrompt: PromptConfig
  usageInjectDefault: boolean
  defaultTimeoutSeconds: number
  defaultConnectTimeoutSeconds: number
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
  apiFormat: string
  stream: boolean
  reasoningEffort: string
  clientIp: string
  requestModified: boolean
  responseModified: boolean
  modifications: string[]
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
}

/** 后端通过 SSE 推送的事件。 */
export type ProxyEvent =
  | { type: 'log'; data: RequestLog }
  | { type: 'live'; data: LiveRequest[] }
  | { type: 'settings'; data: Settings }
  | { type: 'providers'; data: unknown }
  | { type: 'logs-cleared'; data: { deleted: number } }
