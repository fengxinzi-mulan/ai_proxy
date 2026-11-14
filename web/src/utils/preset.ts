// 供应商优化预设的前端辅助。
//
// 预设内容本身由后端提供（GET /api/provider-presets），这里只负责三件事：
// 从 BaseURL 取主机名、判断是否命中某个预设、判断当前配置是否已经符合预设。
// 编辑抽屉和供应商列表共用这一份逻辑，免得两处判定标准慢慢分叉。

import type { Provider, ProviderPreset } from '@/types'

/** 从 BaseURL 取小写主机名；缺 scheme 时补 https:// 再解析，与后端 hostOf 一致。 */
export function hostOf(raw: string | undefined | null): string {
  const text = (raw ?? '').trim()
  if (!text) return ''
  try {
    const url = new URL(text.includes('://') ? text : 'https://' + text)
    return url.hostname.toLowerCase()
  } catch {
    return ''
  }
}

/** 该预设是否匹配这个 BaseURL（按主机后缀匹配，api.mistral.ai 命中 mistral.ai）。 */
export function presetMatchesHost(preset: ProviderPreset, baseUrl: string | undefined | null): boolean {
  const host = hostOf(baseUrl)
  if (!host) return false
  return preset.hosts.some((h) => {
    const want = h.trim().toLowerCase()
    return want !== '' && (host === want || host.endsWith('.' + want))
  })
}

/** 命中该 BaseURL 的全部预设。 */
export function matchPresets(
  presets: ProviderPreset[],
  baseUrl: string | undefined | null,
): ProviderPreset[] {
  return presets.filter((p) => presetMatchesHost(p, baseUrl))
}

/** 逐项比较预设与当前配置：预设声明过的字段全部一致才算「已应用」。 */
export function presetApplied(preset: ProviderPreset, p: Partial<Provider>): boolean {
  if (preset.stripUnsupportedParams !== undefined && p.stripUnsupportedParams !== preset.stripUnsupportedParams) {
    return false
  }
  if (preset.promptCacheKeyMode !== undefined && p.promptCacheKeyMode !== preset.promptCacheKeyMode) {
    return false
  }
  if (preset.usageInjectMode !== undefined && p.usageInjectMode !== preset.usageInjectMode) {
    return false
  }
  if (preset.continuationMode !== undefined && p.continuationMode !== preset.continuationMode) {
    return false
  }
  if (preset.timeoutSeconds !== undefined && p.timeoutSeconds !== preset.timeoutSeconds) {
    return false
  }
  return true
}

/** 命中但还没完全应用的预设 —— 界面据此提示「可优化」。 */
export function pendingPresets(
  presets: ProviderPreset[],
  p: Partial<Provider>,
): ProviderPreset[] {
  return matchPresets(presets, p.baseUrl).filter((preset) => !presetApplied(preset, p))
}
