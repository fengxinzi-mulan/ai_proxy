import { defineStore } from 'pinia'
import { computed, ref } from 'vue'
import { api } from '@/api'
import type { LiveRequest, MetaInfo, Provider, Settings, UpdateState, UsageSnapshot } from '@/types'

const THEME_STORAGE = 'ai-proxy-theme'

/** 主题模式：跟随系统 / 强制亮 / 强制暗。 */
export type ThemeMode = 'system' | 'light' | 'dark'

function readStoredMode(): ThemeMode {
  const v = localStorage.getItem(THEME_STORAGE)
  if (v === 'light' || v === 'dark' || v === 'system') return v
  // 旧版本只存了 dark/light；没有记录时默认跟随系统。
  return 'system'
}

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

  const themeMode = ref<ThemeMode>(readStoredMode())
  const systemDark = ref(
    typeof window !== 'undefined' && window.matchMedia
      ? window.matchMedia('(prefers-color-scheme: dark)').matches
      : false,
  )

  // 更新状态放在 store 而不是 App.vue 的局部 ref：弹框（App.vue 的子组件）
  // 与设置页都要读同一份，两处各自拉一次必然漂移。
  const updateState = ref<UpdateState | null>(null)

  /** 当前实际是否暗色：mode 为 system 时看系统偏好。 */
  const dark = computed(() =>
    themeMode.value === 'system' ? systemDark.value : themeMode.value === 'dark',
  )

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

  const isDesktop = computed(() => !!meta.value?.desktop)
  const version = computed(() => meta.value?.version || '')

  /** 有新版本且用户没跳过：侧边栏显示小圆点。 */
  const updateAvailable = computed(
    () => !!updateState.value?.hasUpdate && !updateState.value.skipped,
  )

  /** 安装流程正在进行中（下载/校验/替换/重启）。 */
  const updateInstalling = computed(() => {
    const p = updateState.value?.phase
    return p === 'downloading' || p === 'verifying' || p === 'applying' || p === 'restarting'
  })

  /**
   * 拉一次当前更新状态。
   *
   * 这个接口不发起网络请求（纯读服务端内存里的状态），所以页面加载时调它很便宜。
   * 不调的话，服务端启动 8 秒后那次自动检查广播时页面通常还没连上 SSE，
   * 之后 6 小时内不会再有事件 —— 用户就永远看不到「有新版本」。
   */
  async function loadUpdate() {
    try {
      updateState.value = await api.update.get()
    } catch {
      // 老后端没有这个接口：保持 null，界面按「不支持」处理。
      updateState.value = null
    }
  }

  function setUpdateState(s: UpdateState) {
    updateState.value = s
  }

  /** 跟随系统主题变化。只在 mode=system 时影响实际配色。 */
  function watchSystemTheme() {
    if (typeof window === 'undefined' || !window.matchMedia) return
    const mq = window.matchMedia('(prefers-color-scheme: dark)')
    const onChange = (e: MediaQueryListEvent) => {
      systemDark.value = e.matches
      applyTheme()
    }
    mq.addEventListener('change', onChange)
  }

  function applyTheme() {
    document.documentElement.classList.toggle('dark', dark.value)
    localStorage.setItem(THEME_STORAGE, themeMode.value)
  }

  /** 在 system → light → dark 之间循环。 */
  function cycleTheme() {
    const order: ThemeMode[] = ['system', 'light', 'dark']
    const i = order.indexOf(themeMode.value)
    themeMode.value = order[(i + 1) % order.length]
    applyTheme()
  }

  function setThemeMode(mode: ThemeMode) {
    themeMode.value = mode
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
    // 单独拉更新状态：它失败不该让整个界面进入错误态。
    void loadUpdate()
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
    themeMode,
    dark,
    isDesktop,
    version,
    usageRefreshing,
    updateState,
    updateAvailable,
    updateInstalling,
    activeProvider,
    enabledProviders,
    listenBaseURL,
    watchSystemTheme,
    applyTheme,
    cycleTheme,
    setThemeMode,
    bootstrap,
    reloadProviders,
    reloadSettings,
    activate,
    refreshUsage,
    loadUpdate,
    setUpdateState,
  }
})
