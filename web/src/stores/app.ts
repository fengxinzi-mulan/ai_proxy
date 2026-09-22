import { defineStore } from 'pinia'
import { ref, computed } from 'vue'
import { api } from '@/api'
import type { LiveRequest, MetaInfo, Provider, Settings } from '@/types'

const THEME_STORAGE = 'ai-proxy-theme'

/** 全局状态：设置、供应商列表、进行中的请求、主题。 */
export const useAppStore = defineStore('app', () => {
  const settings = ref<Settings | null>(null)
  const providers = ref<Provider[]>([])
  const liveRequests = ref<LiveRequest[]>([])
  const meta = ref<MetaInfo | null>(null)
  const loading = ref(false)
  const error = ref('')

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

  return {
    settings,
    providers,
    liveRequests,
    meta,
    loading,
    error,
    dark,
    activeProvider,
    enabledProviders,
    listenBaseURL,
    applyTheme,
    toggleTheme,
    bootstrap,
    reloadProviders,
    reloadSettings,
    activate,
  }
})
