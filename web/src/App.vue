<script setup lang="ts">
import { computed, h, onMounted, ref } from 'vue'
import { RouterView, useRoute, useRouter } from 'vue-router'
import {
  NConfigProvider,
  NDialogProvider,
  NMessageProvider,
  NNotificationProvider,
  NGlobalStyle,
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
  NCheckbox,
  NSpin,
  NResult,
  NModal,
  NInput,
  NIcon,
  darkTheme,
  zhCN,
  dateZhCN,
} from 'naive-ui'
import type { MenuOption } from 'naive-ui'
import {
  LayoutDashboard,
  Server,
  ScrollText,
  Radio,
  Settings as SettingsIcon,
  KeyRound,
  Sun,
  Moon,
  Monitor,
  Power,
  PanelBottom,
  X,
} from 'lucide-vue-next'
import { useAppStore } from '@/stores/app'
import { useEvents } from '@/composables/useEvents'
import BaseUrlChip from '@/components/BaseUrlChip.vue'
import { desktopApi, getAccessKey, setAccessKey } from '@/api'
import { themeOverrides } from '@/theme/naive'

const store = useAppStore()
const route = useRoute()
const router = useRouter()

const overrides = computed(() => themeOverrides(store.dark))

/** 菜单图标：统一 16px 线性图标，不再用彩色圆点。 */
function menuIcon(component: unknown) {
  return () => h(NIcon, { size: 16 }, { default: () => h(component as never) })
}

const menuOptions = computed<MenuOption[]>(() => [
  { label: '仪表盘', key: 'dashboard', icon: menuIcon(LayoutDashboard) },
  { label: '供应商', key: 'providers', icon: menuIcon(Server) },
  { label: '请求日志', key: 'logs', icon: menuIcon(ScrollText) },
  {
    label: () =>
      h('span', { class: 'menu-live' }, [
        '实时监控',
        store.liveRequests.length
          ? h(NBadge, {
              value: store.liveRequests.length,
              type: 'info',
              processing: false,
              style: 'margin-left:auto',
            })
          : null,
      ]),
    key: 'live',
    icon: menuIcon(Radio),
  },
  { label: '设置', key: 'settings', icon: menuIcon(SettingsIcon) },
])

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

/** 侧栏折叠：窄窗口自动收起，用户手动切换后以下次窗口宽度为准。 */
const collapsed = ref(false)
const manualCollapsed = ref(false)

function updateAutoCollapse() {
  if (manualCollapsed.value) return
  collapsed.value = window.innerWidth < 1180
}

function onCollapseToggle(v: boolean) {
  manualCollapsed.value = true
  collapsed.value = v
}

const { connected, reconnect } = useEvents({
  onLog: () => {
    // 日志页与实时页各自订阅，这里只维护顶栏的进行中数量。
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
  onDesktopCloseAsk: showCloseAsk,
})

const themeIcon = computed(() => {
  if (store.themeMode === 'system') return Monitor
  return store.dark ? Moon : Sun
})

const themeLabel = computed(() => {
  if (store.themeMode === 'system') return '跟随系统'
  return store.dark ? '暗色' : '亮色'
})

onMounted(async () => {
  store.applyTheme()
  store.watchSystemTheme()
  updateAutoCollapse()
  window.addEventListener('resize', updateAutoCollapse)
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

/* ---------- 桌面关闭确认 ---------- */

const closeAskShow = ref(false)
const closeAskRemember = ref(false)
const closeAskResolving = ref(false)
const closeAskAction = ref<'quit' | 'tray' | 'cancel' | ''>('')
const closeAskError = ref('')

function showCloseAsk() {
  closeAskError.value = ''
  closeAskRemember.value = false
  closeAskShow.value = true
}

async function resolveClose(action: 'quit' | 'tray' | 'cancel') {
  if (closeAskResolving.value) return
  closeAskResolving.value = true
  closeAskAction.value = action
  closeAskError.value = ''
  try {
    await desktopApi.close({ action, remember: closeAskRemember.value })
    closeAskShow.value = false
    if (action !== 'cancel') closeAskRemember.value = false
  } catch (e) {
    closeAskError.value = e instanceof Error ? e.message : String(e)
  } finally {
    closeAskResolving.value = false
    closeAskAction.value = ''
  }
}

/** Esc / 点遮罩也视为取消，但要把选择告诉桌面壳，释放关闭拦截状态。 */
function onCloseAskVisibility(v: boolean) {
  if (!v && closeAskShow.value && !closeAskResolving.value) {
    void resolveClose('cancel')
  }
}

const routeTitle = computed(() => String(route.meta.title ?? 'AI Proxy'))
</script>

<template>
  <NConfigProvider
    :theme="store.dark ? darkTheme : null"
    :theme-overrides="overrides"
    :locale="zhCN"
    :date-locale="dateZhCN"
  >
    <NGlobalStyle />
    <NDialogProvider>
      <NMessageProvider>
        <NNotificationProvider>
          <NLayout has-sider position="absolute">
            <NLayoutSider
              bordered
              :width="212"
              :collapsed-width="56"
              :collapsed="collapsed"
              collapse-mode="width"
              :show-trigger="true"
              @update:collapsed="onCollapseToggle"
            >
              <div class="brand" :class="{ collapsed }">
                <div class="brand-mark" aria-hidden="true">
                  <svg viewBox="0 0 24 24" fill="none">
                    <path
                      d="M4 15.5 9 9l4 4.2L19.5 6"
                      stroke="currentColor"
                      stroke-width="2.2"
                      stroke-linecap="round"
                      stroke-linejoin="round"
                    />
                    <circle cx="19.5" cy="6" r="2.1" fill="currentColor" />
                  </svg>
                </div>
                <div v-if="!collapsed" class="brand-text">
                  <div class="brand-title">AI Proxy</div>
                  <div class="brand-sub">{{ routeTitle }}</div>
                </div>
              </div>

              <NMenu
                :value="activeKey"
                :options="menuOptions"
                :collapsed="collapsed"
                :collapsed-width="56"
                :collapsed-icon-size="18"
                :root-indent="18"
                :indent="16"
                @update:value="onMenuSelect"
              />

              <div v-if="!collapsed" class="sider-foot">
                <div class="sider-status">
                  <span :class="['dot', connected ? 'dot-on' : 'dot-off']" />
                  <span class="sider-status-text">{{ connected ? '实时已连接' : '实时断开' }}</span>
                </div>
                <div v-if="store.version" class="sider-version">v{{ store.version }}</div>
              </div>
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
                    class="provider-select"
                    @update:value="onProviderChange"
                  />
                  <NTag v-else type="warning" size="small" :bordered="false">
                    尚未配置供应商
                  </NTag>
                  <BaseUrlChip />
                </div>

                <div class="topbar-right">
                  <NTooltip>
                    <template #trigger>
                      <span :class="['status-pill', connected ? 'ok' : 'bad']">
                        <span :class="['dot', connected ? 'dot-on' : 'dot-off']" />
                        {{ connected ? '实时' : '离线' }}
                      </span>
                    </template>
                    {{ connected ? '正在接收服务端推送' : '与服务端的实时通道已断开，正在重连' }}
                  </NTooltip>

                  <RouterLink to="/live" class="live-link">
                    <NBadge :value="liveCount" :show="liveCount > 0" type="info">
                      <span class="status-pill neutral">
                        <NIcon :size="14"><Radio /></NIcon>
                        进行中 {{ liveCount }}
                      </span>
                    </NBadge>
                  </RouterLink>

                  <NTooltip>
                    <template #trigger>
                      <NButton size="small" quaternary aria-label="访问密钥" @click="promptForKey">
                        <template #icon>
                          <NIcon :size="15"><KeyRound /></NIcon>
                        </template>
                      </NButton>
                    </template>
                    访问密钥
                  </NTooltip>

                  <NTooltip>
                    <template #trigger>
                      <NButton
                        size="small"
                        quaternary
                        :aria-label="`主题：${themeLabel}`"
                        @click="store.cycleTheme()"
                      >
                        <template #icon>
                          <NIcon :size="15"><component :is="themeIcon" /></NIcon>
                        </template>
                      </NButton>
                    </template>
                    主题：{{ themeLabel }}（点击切换）
                  </NTooltip>
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
                <RouterView v-else v-slot="{ Component }">
                  <transition name="fade" mode="out-in">
                    <component :is="Component" />
                  </transition>
                </RouterView>
              </NLayoutContent>
            </NLayout>
          </NLayout>

          <NModal
            v-model:show="keyDialog"
            preset="card"
            title="管理界面访问密钥"
            class="key-modal"
          >
            <p class="key-hint">
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
              <div class="key-actions">
                <NButton @click="keyDialog = false">取消</NButton>
                <NButton type="primary" @click="submitKey">保存并重试</NButton>
              </div>
            </template>
          </NModal>

          <NModal
            :show="closeAskShow"
            :mask-closable="!closeAskResolving"
            :close-on-esc="!closeAskResolving"
            @update:show="onCloseAskVisibility"
          >
            <div
              class="close-dialog"
              role="dialog"
              aria-modal="true"
              aria-labelledby="close-dialog-title"
            >
              <button
                class="close-dialog-x"
                type="button"
                aria-label="取消关闭"
                :disabled="closeAskResolving"
                @click="resolveClose('cancel')"
              >
                <NIcon :size="16"><X /></NIcon>
              </button>

              <div class="close-dialog-head">
                <div class="close-dialog-mark">
                  <NIcon :size="22"><Power /></NIcon>
                </div>
                <div>
                  <h2 id="close-dialog-title">关闭 AI Proxy？</h2>
                  <p>直接退出会停止代理，客户端会断开；托盘运行会隐藏窗口，代理继续在后台工作。</p>
                </div>
              </div>

              <div class="close-remember">
                <NCheckbox v-model:checked="closeAskRemember" :disabled="closeAskResolving">
                  <span class="close-remember-label">
                    记住我的选择
                    <span class="close-remember-hint">可在「设置 → 桌面应用」中修改</span>
                  </span>
                </NCheckbox>
              </div>

              <p v-if="closeAskError" class="close-dialog-error">{{ closeAskError }}</p>

              <div class="close-dialog-foot">
                <NButton quaternary :disabled="closeAskResolving" @click="resolveClose('cancel')">
                  取消
                </NButton>
                <NButton
                  type="error"
                  tertiary
                  :loading="closeAskResolving && closeAskAction === 'quit'"
                  :disabled="closeAskResolving"
                  @click="resolveClose('quit')"
                >
                  <template #icon>
                    <NIcon :size="15"><Power /></NIcon>
                  </template>
                  退出程序
                </NButton>
                <NButton
                  type="primary"
                  :loading="closeAskResolving && closeAskAction === 'tray'"
                  :disabled="closeAskResolving"
                  @click="resolveClose('tray')"
                >
                  <template #icon>
                    <NIcon :size="15"><PanelBottom /></NIcon>
                  </template>
                  托盘运行
                </NButton>
              </div>
            </div>
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
  padding: 16px 16px 12px;
  min-height: 60px;
}

.brand.collapsed {
  justify-content: center;
  padding: 16px 0 12px;
}

.brand-mark {
  width: 30px;
  height: 30px;
  border-radius: 8px;
  background: linear-gradient(140deg, var(--accent), #7c3aed);
  color: #fff;
  display: flex;
  align-items: center;
  justify-content: center;
  flex-shrink: 0;
}

.brand-mark svg {
  width: 18px;
  height: 18px;
}

.brand-text {
  min-width: 0;
}

.brand-title {
  font-weight: 600;
  font-size: var(--text-sm);
  line-height: 1.3;
  white-space: nowrap;
}

.brand-sub {
  font-size: var(--text-2xs);
  color: var(--text-3);
  line-height: 1.4;
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.menu-live {
  display: flex;
  align-items: center;
  gap: 8px;
  width: 100%;
}

.sider-foot {
  position: absolute;
  bottom: 0;
  left: 0;
  right: 0;
  padding: 12px 16px;
  border-top: 1px solid var(--border);
}

.sider-status {
  display: flex;
  align-items: center;
  gap: 6px;
  font-size: var(--text-2xs);
  color: var(--text-3);
}

.sider-status-text {
  white-space: nowrap;
}

.sider-version {
  margin-top: 4px;
  font-family: var(--font-mono);
  font-size: var(--text-2xs);
  color: var(--text-3);
  white-space: nowrap;
}

.topbar {
  height: 52px;
  display: flex;
  align-items: center;
  justify-content: space-between;
  padding: 0 20px;
  gap: 16px;
}

.topbar-left,
.topbar-right {
  display: flex;
  align-items: center;
  gap: 10px;
  min-width: 0;
}

.topbar-label {
  font-size: var(--text-xs);
  color: var(--text-3);
  white-space: nowrap;
}

.provider-select {
  width: 210px;
}

.status-pill {
  display: inline-flex;
  align-items: center;
  gap: 5px;
  height: 24px;
  padding: 0 8px;
  border-radius: 6px;
  font-size: var(--text-2xs);
  border: 1px solid var(--border);
  color: var(--text-2);
  white-space: nowrap;
}

.status-pill.ok {
  color: var(--success);
  background: var(--success-soft);
  border-color: var(--success-border);
}

.status-pill.bad {
  color: var(--danger);
  background: var(--danger-soft);
  border-color: var(--danger-border);
}

.status-pill.neutral {
  background: var(--surface-2);
}

.dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  flex-shrink: 0;
}

.dot-on {
  background: currentColor;
}

.dot-off {
  background: currentColor;
  opacity: 0.45;
}

.live-link {
  text-decoration: none;
  color: inherit;
}

.content {
  padding: 20px 24px;
}

.center {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 60vh;
}

.key-modal {
  max-width: 460px;
}

.key-hint {
  margin: 0 0 12px;
  font-size: var(--text-xs);
  color: var(--text-2);
  line-height: 1.6;
}

.key-actions {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
}

.fade-enter-active,
.fade-leave-active {
  transition: opacity 120ms var(--ease);
}

.fade-enter-from,
.fade-leave-to {
  opacity: 0;
}

/* ---------- 桌面关闭确认 ---------- */

.close-dialog {
  position: relative;
  width: min(460px, calc(100vw - 32px));
  padding: 24px;
  overflow: hidden;
  border: 1px solid var(--border);
  border-radius: 16px;
  background: var(--surface);
  color: var(--text);
  box-shadow: var(--shadow-overlay);
}

.close-dialog::before {
  content: '';
  position: absolute;
  top: -110px;
  right: -70px;
  width: 240px;
  height: 240px;
  border-radius: 50%;
  background: radial-gradient(circle, var(--accent-soft), transparent 70%);
  pointer-events: none;
}

.close-dialog-x {
  position: absolute;
  top: 14px;
  right: 14px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 30px;
  height: 30px;
  padding: 0;
  border: 1px solid transparent;
  border-radius: 8px;
  background: transparent;
  color: var(--text-3);
  cursor: pointer;
  transition:
    background var(--duration) var(--ease),
    color var(--duration) var(--ease);
}

.close-dialog-x:hover:not(:disabled) {
  background: var(--surface-hover);
  color: var(--text);
}

.close-dialog-x:disabled {
  opacity: 0.5;
  cursor: default;
}

.close-dialog-head {
  display: flex;
  align-items: flex-start;
  gap: 14px;
  padding-right: 24px;
}

.close-dialog-mark {
  display: flex;
  align-items: center;
  justify-content: center;
  width: 44px;
  height: 44px;
  flex-shrink: 0;
  border: 1px solid var(--danger-border);
  border-radius: 12px;
  background: var(--danger-soft);
  color: var(--danger);
}

.close-dialog h2 {
  margin: 1px 0 0;
  font-size: 17px;
  font-weight: 600;
  letter-spacing: -0.01em;
}

.close-dialog-head p {
  margin: 6px 0 0;
  color: var(--text-2);
  font-size: var(--text-xs);
  line-height: 1.65;
}

.close-remember {
  margin-top: 20px;
  padding: 11px 12px;
  border: 1px solid var(--border);
  border-radius: 10px;
  background: var(--surface-2);
  transition:
    border-color var(--duration) var(--ease),
    box-shadow var(--duration) var(--ease);
}

.close-remember:focus-within {
  border-color: var(--accent-border);
  box-shadow: 0 0 0 3px var(--accent-soft);
}

.close-remember :deep(.n-checkbox:focus-within) {
  outline: none;
}

.close-remember-label {
  display: inline-flex;
  align-items: baseline;
  gap: 8px;
  flex-wrap: wrap;
  font-size: var(--text-xs);
}

.close-remember-hint {
  color: var(--text-3);
  font-size: var(--text-2xs);
}

.close-dialog-error {
  margin: 12px 0 0;
  padding: 8px 10px;
  border-radius: 8px;
  background: var(--danger-soft);
  color: var(--danger);
  font-size: var(--text-xs);
  line-height: 1.5;
}

.close-dialog-foot {
  display: flex;
  justify-content: flex-end;
  gap: 8px;
  margin-top: 20px;
  padding-top: 16px;
  border-top: 1px solid var(--border);
}
</style>
