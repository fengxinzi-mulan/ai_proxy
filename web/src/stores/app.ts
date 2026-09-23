import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { api } from '@/api'
import type { LiveRequest, MetaInfo, Provider, Settings, UsageSnapshot } from '@/types'

const THEME_STORAGE = 'ai-proxy-theme'

/** 全局状态：设置、供应商列表、进行中的请求、主题。 */
export const useAppStore = defineStore('app', () => {
  const settings = ref<Settings | null>(null)
  const providers = ref<Provider[]>([])
  const liveRequests = ref<LiveRequest[]>([])
  const meta = ref<MetaInfo | null>(null)
  const loading = ref(false)
  const error = ref('')

  // 用量快照本身不入 store：它是服务端定时刷出来、随供应商列表一起下发的数据，
  // 放在 providers 的每一项上即可（见 Provider.usage）。这里只维护「谁正在手动刷新」。
  const usageRefreshing = ref<Record<number, boolean>>({})

  const dark = ref(localStorage.getItem(THEME_STORAGE) === 'dark')

  const activeProvider = computed(
    () => providers.value.find((p) => p.active && p.enabled) ?? providers.value.find((p) => p.enabled),
  )
  const enabledProviders = computed(() => providers.value.filter((p) => p.enabled))

  /** 客户端应当填写的接入地址。以进程实际监听地址为准，而不是设置里的值。 */
  const listenBaseURL = computed(() => {
    const host = meta.value?.listen?.host || settings.value?.host || '127.0.0.1'
    const port = meta.value?.listen?.port || settings.value?.port || 8080
    const shown = host === '0.0.0.0' || host === '' ? '127.0.0.1' : host
    return `http://${shown}:${port}`
  })

  function applyTheme() {
    document.documentElement.classList.toggle('dark', dark.value)
    localStorage.setItem(THEME_STORAGE, dark.value ? 'dark' : 'light')
  }

  function toggleTheme() {
    dark.value = !dark.value
    applyTheme()
  }

  async function bootstrap() {
    loading.value = true
    error.value = ''
    try {
      const [s, p, m] = await Promise.all([api.getSettings(), api.listProviders(), api.meta()])
      settings.value = s
      providers.value = p
      meta.value = m
    } catch (e) {
      error.value = e instanceof Error ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }

  async function reloadProviders() {
    providers.value = await api.listProviders()
  }

  async function reloadSettings() {
    settings.value = await api.getSettings()
  }

  /** 切换「当前生效」供应商。 */
  async function activate(id: number) {
    providers.value = await api.activateProvider(id)
  }

  /**
   * 让服务端立刻查一次用量并落库，然后把新快照写回列表里的那一项。
   *
   * 定时刷新是服务端的事（见 proxy.UsageRefresher），这里只负责「用户现在就想看最新的」
   * 这种情况：额度是查一次就能拿到的东西，不该逼用户等下一个刷新周期。
   *
   * provider 可以传编辑抽屉里尚未保存的配置，让「改完模板直接查」成为可能；
   * 服务端同样会把这次结果存下来。
   */
  async function refreshUsage(id: number, provider?: Partial<Provider>): Promise<UsageSnapshot | null> {
    usageRefreshing.value = { ...usageRefreshing.value, [id]: true }
    try {
      const snap = await api.queryProviderUsage(id, provider)
      applyUsage(id, snap)
      return snap
    } catch (e) {
      // 请求根本没发出去（网络、密钥被拒）时也要让界面看到原因，
      // 而不是留着上一次的旧数字让人以为还是最新的。
      const failed: UsageSnapshot = {
        ok: false,
        providerId: id,
        template: provider?.usageQuery?.template ?? '',
        templateName: '',
        fetchedAt: new Date().toISOString(),
        latencyMs: 0,
        account: '',
        planId: '',
        planName: '',
        status: '',
        periodStart: null,
        periodEnd: null,
        balances: [],
        windows: [],
        period: [],
        warnings: [],
        error: e instanceof Error ? e.message : String(e),
        usedUrls: [],
      }
      applyUsage(id, failed)
      return failed
    } finally {
      usageRefreshing.value = { ...usageRefreshing.value, [id]: false }
    }
  }

  function applyUsage(id: number, snap: UsageSnapshot) {
    providers.value = providers.value.map((p) => (p.id === id ? { ...p, usage: snap } : p))
  }

  return {
    settings,
    providers,
    liveRequests,
    meta,
    loading,
    error,
    dark,
    usageRefreshing,
    activeProvider,
    enabledProviders,
    listenBaseURL,
    applyTheme,
    toggleTheme,
    bootstrap,
    reloadProviders,
    reloadSettings,
    activate,
    refreshUsage,
  }
})
