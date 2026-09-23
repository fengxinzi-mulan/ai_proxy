// 后端管理接口的薄封装。
//
// 只做三件事：拼 URL、发请求、把后端的 {"error": "..."} 转成异常。
// 不缓存、不重试 —— 界面上的数据都以服务端为准。

import type {
  APIFormat,
  LogPage,
  MetaInfo,
  ModelStatsRow,
  OverviewStats,
  ProbeResult,
  Provider,
  ProviderStatsRow,
  RequestLog,
  Settings,
  TimeSeriesPoint,
  UsageSnapshot,
} from '@/types'

/** 访问密钥，配置了之后所有请求都要带上。 */
const KEY_STORAGE = 'ai-proxy-access-key'

export function getAccessKey(): string {
  return localStorage.getItem(KEY_STORAGE) ?? ''
}

export function setAccessKey(key: string) {
  if (key) localStorage.setItem(KEY_STORAGE, key)
  else localStorage.removeItem(KEY_STORAGE)
}

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message)
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const headers = new Headers(init?.headers)
  if (init?.body && !headers.has('Content-Type')) {
    headers.set('Content-Type', 'application/json')
  }
  const key = getAccessKey()
  if (key) headers.set('X-AI-Proxy-Key', key)

  const resp = await fetch(path, { ...init, headers })
  const text = await resp.text()

  if (!resp.ok) {
    // 后端的错误体统一是 {"error": "..."}，解析失败时退回状态文本。
    let msg = text || resp.statusText
    try {
      const parsed = JSON.parse(text)
      if (typeof parsed?.error === 'string') msg = parsed.error
    } catch {
      /* 保持原始文本 */
    }
    throw new ApiError(msg, resp.status)
  }

  if (!text) return undefined as T
  return JSON.parse(text) as T
}

function query(params: Record<string, string | number | undefined | null>): string {
  const qs = new URLSearchParams()
  for (const [k, v] of Object.entries(params)) {
    if (v !== undefined && v !== null && v !== '') qs.set(k, String(v))
  }
  const s = qs.toString()
  return s ? `?${s}` : ''
}

export const api = {
  meta: () => request<MetaInfo>('/api/meta'),

  getSettings: () => request<Settings>('/api/settings'),
  saveSettings: (s: Settings) =>
    request<Settings>('/api/settings', { method: 'PUT', body: JSON.stringify(s) }),

  listProviders: () => request<Provider[]>('/api/providers'),
  getProvider: (id: number) => request<Provider>(`/api/providers/${id}`),
  createProvider: (p: Partial<Provider>) =>
    request<Provider>('/api/providers', { method: 'POST', body: JSON.stringify(p) }),
  updateProvider: (p: Partial<Provider> & { id: number }) =>
    request<Provider>(`/api/providers/${p.id}`, { method: 'PUT', body: JSON.stringify(p) }),
  deleteProvider: (id: number) =>
    request<{ ok: boolean }>(`/api/providers/${id}`, { method: 'DELETE' }),
  activateProvider: (id: number) =>
    request<Provider[]>(`/api/providers/${id}/activate`, { method: 'POST' }),
  reorderProviders: (ids: number[]) =>
    request<{ ok: boolean }>('/api/providers/reorder', {
      method: 'POST',
      body: JSON.stringify({ ids }),
    }),
  /** 测试连通性。可带上尚未保存的配置，省去「先存再测」的来回。 */
  testProvider: (id: number, provider: Partial<Provider>, probeModel?: string) =>
    request<ProbeResult>(`/api/providers/${id}/test`, {
      method: 'POST',
      body: JSON.stringify({ ...provider, probeModel: probeModel ?? '' }),
    }),
  fetchProviderModels: (id: number) =>
    request<{ models: string[] }>(`/api/providers/${id}/models`, { method: 'POST' }),

  /**
   * 查询套餐余量。与 testProvider 一样可带上尚未保存的配置，
   * 这样在编辑抽屉里改完模板就能直接查，不用先保存。
   */
  queryProviderUsage: (id: number, provider?: Partial<Provider>) =>
    request<UsageSnapshot>(`/api/providers/${id}/usage`, {
      method: 'POST',
      body: JSON.stringify(provider ?? {}),
    }),

  listLogs: (params: {
    page?: number
    pageSize?: number
    providerId?: number
    model?: string
    status?: string
    q?: string
    range?: string
    from?: string
    to?: string
  }) => request<LogPage>(`/api/logs${query(params)}`),
  getLog: (id: number) => request<RequestLog>(`/api/logs/${id}`),
  deleteLogs: (params: Record<string, string | number | undefined>) =>
    request<{ deleted: number }>(`/api/logs${query(params)}`, { method: 'DELETE' }),
  clearLogs: () =>
    request<{ deleted: number }>('/api/logs/clear', {
      method: 'POST',
      body: JSON.stringify({ confirm: true }),
    }),
  distinctModels: () => request<string[]>('/api/models'),

  overview: (range: string) =>
    request<{ stats: OverviewStats; liveCount: number; activeProvider?: Provider }>(
      `/api/stats/overview${query({ range })}`,
    ),
  timeSeries: (range: string, bucket?: string) =>
    request<{ bucket: string; from: string; to: string; points: TimeSeriesPoint[] }>(
      `/api/stats/timeseries${query({ range, bucket })}`,
    ),
  byModel: (range: string, limit = 10) =>
    request<ModelStatsRow[]>(`/api/stats/by-model${query({ range, limit })}`),
  byProvider: (range: string, limit = 10) =>
    request<ProviderStatsRow[]>(`/api/stats/by-provider${query({ range, limit })}`),

  live: () => request<import('@/types').LiveRequest[]>('/api/live'),
}

export { API_FORMAT_LABELS as formatLabels } from '@/types'
export type { APIFormat }
