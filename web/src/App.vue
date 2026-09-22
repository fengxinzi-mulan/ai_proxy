<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'
import {
  NConfigProvider,
  NDialogProvider,
  NMessageProvider,
  NNotificationProvider,
  NLayout,
  NLayoutSider,
  NLayoutHeader,
  NLayoutContent,
  NMenu,
  NSelect,
  NTag,
  NBadge,
  NTooltip,
  NButton,
  NSpin,
  NResult,
  NModal,
  NInput,
  darkTheme,
  zhCN,
  dateZhCN,
} from 'naive-ui'
import type { MenuOption, GlobalThemeOverrides } from 'naive-ui'
import { useAppStore } from '@/stores/app'
import { useEvents } from '@/composables/useEvents'
import { getAccessKey, setAccessKey } from '@/api'

const store = useAppStore()
const route = useRoute()
const router = useRouter()

const themeOverrides: GlobalThemeOverrides = {
  common: {
    primaryColor: '#2082ff',
    primaryColorHover: '#3d93ff',
    primaryColorPressed: '#0a6fe0',
    borderRadius: '8px',
  },
  Card: {
    borderRadius: '12px',
  },
}

const menuOptions = computed<MenuOption[]>(() => [
  { label: '仪表盘', key: 'dashboard', icon: () => h(IconDot, { color: '#2082ff' }) },
  { label: '供应商', key: 'providers', icon: () => h(IconDot, { color: '#18a058' }) },
  { label: '请求日志', key: 'logs', icon: () => h(IconDot, { color: '#f0a020' }) },
  {
    label: () =>
      h('span', { style: 'display:flex;align-items:center;gap:8px' }, [
        '实时监控',
        store.liveRequests.length
          ? h(NBadge, { value: store.liveRequests.length, type: 'info', processing: false })
          : null,
      ]),
    key: 'live',
    icon: () => h(IconDot, { color: '#d03050' }),
  },
  { label: '设置', key: 'settings', icon: () => h(IconDot, { color: '#8a8a8a' }) },
])

/* 侧边栏图标用一个小圆点即可，避免为一个装饰性图标引入整套图标库。 */
const IconDot = {
  props: { color: { type: String, default: '#2082ff' } },
  setup(props: { color: string }) {
    return () =>
      h('span', {
        style: `display:inline-block;width:8px;height:8px;border-radius:50%;background:${props.color}`,
      })
  },
}

const activeKey = computed(() => String(route.name ?? 'dashboard'))

function onMenuSelect(key: string) {
  router.push({ name: key })
}

/** 顶栏的供应商快捷切换。 */
const providerOptions = computed(() =>
  store.enabledProviders.map((p) => ({
    label: p.active ? `${p.displayName}（当前）` : p.displayName,
    value: p.id,
  })),
)
const selectedProvider = computed(() => store.activeProvider?.id ?? null)
const switching = ref(false)

async function onProviderChange(id: number) {
  switching.value = true
  try {
    await store.activate(id)
  } finally {
    switching.value = false
  }
}

const liveCount = computed(() => store.liveRequests.length)

const { connected, reconnect } = useEvents({
  onLog: (log) => {
    // 日志页与实时页各自订阅，这里只维护顶栏的进行中数量。
    void log
  },
  onLive: (items) => {
    store.liveRequests = items
  },
  onProvidersChanged: () => {
    void store.reloadProviders()
  },
  onSettingsChanged: (s) => {
    store.settings = s
  },
})

onMounted(async () => {
  store.applyTheme()
  await store.bootstrap()
  if (store.error === '访问密钥无效' || store.error.includes('密钥')) {
    promptForKey()
  }
})

/** 访问密钥失效时弹出输入框，避免用户卡在空白页。 */
const keyDialog = ref(false)
const keyInput = ref('')

function promptForKey() {
  keyDialog.value = true
  keyInput.value = getAccessKey()
}

function submitKey() {
  setAccessKey(keyInput.value.trim())
  keyDialog.value = false
  void store.bootstrap()
  // 页面加载时还没有密钥，实时通道当时是被拒的；EventSource 遇到非 2xx 不会自动重连，
  // 所以这里必须把它整个重建一次，否则顶栏会一直显示「实时断开」。
  reconnect()
}
</script>

<template>
  <NConfigProvider
    :theme="store.dark ? darkTheme : null"
    :theme-overrides="themeOverrides"
    :locale="zhCN"
    :date-locale="dateZhCN"
  >
    <NDialogProvider>
      <NMessageProvider>
        <NNotificationProvider>
          <NLayout has-sider position="absolute">
            <NLayoutSider
              bordered
              :width="200"
              :collapsed-width="64"
              collapse-mode="width"
              show-trigger
            >
              <div class="brand">
                <div class="brand-mark">AI</div>
                <div class="brand-text">
                  <div class="brand-title">AI Proxy</div>
                  <div class="brand-sub">模型供应商代理</div>
                </div>
              </div>
              <NMenu
                :value="activeKey"
                :options="menuOptions"
                :root-indent="18"
                @update:value="onMenuSelect"
              />
            </NLayoutSider>

            <NLayout>
              <NLayoutHeader bordered class="topbar">
                <div class="topbar-left">
                  <span class="topbar-label">当前生效</span>
                  <NSelect
                    v-if="providerOptions.length"
                    :value="selectedProvider"
                    :options="providerOptions"
                    :loading="switching"
                    size="small"
                    style="width: 220px"
                    @update:value="onProviderChange"
                  />
                  <NTag v-else type="warning" size="small" :bordered="false">
                    尚未配置供应商
                  </NTag>
                </div>

                <div class="topbar-right">
                  <NTooltip>
                    <template #trigger>
                      <NTag
                        :type="connected ? 'success' : 'error'"
                        size="small"
                        :bordered="false"
                      >
                        {{ connected ? '实时已连接' : '实时断开' }}
                      </NTag>
                    </template>
                    {{ connected ? '正在接收服务端推送' : '与服务端的实时通道已断开，正在重连' }}
                  </NTooltip>

                  <RouterLink to="/live" class="live-link">
                    <NBadge :value="liveCount" :show="liveCount > 0" type="info">
                      <NTag size="small" :bordered="false">进行中 {{ liveCount }}</NTag>
                    </NBadge>
                  </RouterLink>

                  <NButton size="small" quaternary @click="promptForKey">密钥</NButton>
                  <NButton size="small" quaternary @click="store.toggleTheme">
                    {{ store.dark ? '☀ 亮色' : '☾ 暗色' }}
                  </NButton>
                </div>
              </NLayoutHeader>

              <NLayoutContent class="content" :native-scrollbar="false">
                <div v-if="store.loading && !store.settings" class="center">
                  <NSpin size="large" />
                </div>
                <NResult
                  v-else-if="store.error && !store.settings"
                  status="error"
                  title="无法连接后端"
                  :description="store.error"
                  class="center"
                >
                  <template #footer>
                    <NButton @click="store.bootstrap()">重试</NButton>
                  </template>
                </NResult>
                <RouterView v-else />
              </NLayoutContent>
            </NLayout>
          </NLayout>

          <NModal
            v-model:show="keyDialog"
            preset="card"
            title="管理界面访问密钥"
            style="max-width: 460px"
          >
            <p style="margin-top: 0; opacity: 0.75">
              服务端在设置里配置了访问密钥时，管理界面与代理入口都需要携带它。
              留空表示不使用密钥。
            </p>
            <NInput
              v-model:value="keyInput"
              type="password"
              show-password-on="click"
              placeholder="请输入访问密钥"
              @keyup.enter="submitKey"
            />
            <template #footer>
              <div style="display: flex; justify-content: flex-end; gap: 8px">
                <NButton @click="keyDialog = false">取消</NButton>
                <NButton type="primary" @click="submitKey">保存并重试</NButton>
              </div>
            </template>
          </NModal>
        </NNotificationProvider>
      </NMessageProvider>
    </NDialogProvider>
  </NConfigProvider>
</template>

<style scoped>
.brand {
  display: flex;
  align-items: center;
  gap: 10px;
  padding: 18px 16px 14px;
}

.brand-mark {
  width: 32px;
  height: 32px;
  border-radius: 9px;
  background: linear-gradient(135deg, #2082ff, #18a058);
  color: #fff;
  font-weight: 700;
  font-size: 13px;
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
}

.brand-title {
  font-weight: 600;
  font-size: 14px;
  line-height: 1.2;
}

.brand-sub {
  font-size: 11px;
  opacity: 0.55;
  line-height: 1.4;
}

.topbar {
  height: 56px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 18px;
  gap: 16px;
}

.topbar-left,
.topbar-right {
  display: flex;
  align-items: center;
  gap: 10px;
}

.topbar-label {
  font-size: 12px;
  opacity: 0.6;
}

.live-link {
  text-decoration: none;
}

.content {
  padding: 18px;
}

.center {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 60vh;
}
</style>
