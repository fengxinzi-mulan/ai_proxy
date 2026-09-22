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

  tags: string[]
  createdAt: string
  updatedAt: string
  keyHealth?: Record<string, KeyHealth>
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
