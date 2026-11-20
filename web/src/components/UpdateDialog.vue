<script setup lang="ts">
// 版本更新确认框。
//
// 必须是个子组件（和 BaseUrlChip 同样的理由）：useMessage() 依赖
// NMessageProvider 的注入，而 provider 由 App.vue 自己的模板渲染 ——
// 在 App.vue 的 setup 里调 useMessage 会直接抛
// "No outer <n-message-provider /> founded."，整个前端白屏。
//
// 状态放在 store 里，这个组件只负责「怎么显示」与「用户点了什么」。
import { computed, onBeforeUnmount, ref, watch } from 'vue'
import {
  NAlert,
  NButton,
  NIcon,
  NModal,
  NProgress,
  useMessage,
} from 'naive-ui'
import { Download, X } from 'lucide-vue-next'
import { api } from '@/api'
import { useAppStore } from '@/stores/app'
import { formatBytes } from '@/utils/format'
import type { UpdateState } from '@/types'

const store = useAppStore()
const message = useMessage()

const show = ref(false)
const busy = ref(false)
// dismissed 记住「本次会话里用户已经关掉过弹框」，避免每次 SSE 推送都重新弹。
// 但换了新版本号要复位 —— 否则「稍后」过一次，以后所有版本都被永久静音。
const dismissed = ref(false)
// 安装进行中用户主动关掉弹框：后续进度事件不能再把弹框顶回来。
// 与 dismissed 分开：dismissed 是「这个版本不再提醒」，这里只表示「这次安装
// 我不想盯着看」，安装本身照常推进。
const userHidden = ref(false)

const state = computed<UpdateState | null>(() => store.updateState)
const installing = computed(() => store.updateInstalling)

// 标题跟着状态走：安装中 / 失败 / 发现新版本三种语气，别让失败时还写着「发现新版本」。
const dialogTitle = computed(() => {
  if (installing.value) return '正在更新'
  if (state.value?.phase === 'failed') return '更新失败'
  return '发现新版本'
})

const phaseText = computed(() => {
  switch (state.value?.phase) {
    case 'downloading':
      return `正在下载 ${state.value.progress}%`
    case 'verifying':
      return '正在校验 SHA-256…'
    case 'applying':
      return '正在替换程序文件…'
    case 'restarting':
      return '正在重启…'
    case 'failed':
      return state.value.error || '更新失败'
    default:
      return ''
  }
})

/**
 * 处理服务端推来的更新状态。
 *
 * 三条规则：
 *  1. 检查失败（离线/限流）不该强弹模态框打断用户 —— 只在用户主动发起过安装时弹。
 *  2. 安装进度只更新状态，不重复开弹框。
 *  3. 从「没有更新」变成「有更新」才自动弹，且换版本号时复位 dismissed。
 */
function onState(s: UpdateState) {
  const prev = store.updateState
  store.setUpdateState(s)

  if (prev?.latestVersion !== s.latestVersion) {
    dismissed.value = false
  }

  if (s.phase === 'failed') {
    // 先记下「这次失败是不是用户自己发起的」，再清 busy ——
    // 顺序反了的话判断永远为 false，用户点了立即更新却看不到失败原因。
    const userInitiated = busy.value || prev?.phase === 'downloading' || prev?.phase === 'verifying'
    busy.value = false
    // 后台自动检查失败（离线很常见）不该弹模态框打断用户。
    if (userInitiated) {
      show.value = true
    }
    return
  }

  // 安装进行中：无论进度是哪儿发起的（设置页、自动更新、另一个标签页），
  // 都要把弹框拉起来并启动兜底轮询 —— 只靠 SSE 的话，连接一断
  // 进度条会永久冻在 downloading，用户既看不到结果也没法重试。
  if (s.phase === 'downloading' || s.phase === 'verifying' || s.phase === 'applying' || s.phase === 'restarting') {
    busy.value = true
    // 用户主动关掉的那次不该被下一条进度事件顶回来；安装本身照常推进。
    if (!userHidden.value) show.value = true
    if (pollTimer === null) startPollFallback()
    return
  }

  // 重启后服务端回到 idle：说明新版本已经起来，页面里跑的还是旧前端的代码，
  // 必须刷新才能加载新资源。这里延迟一点，等后端把窗口重新挂起来。
  if (s.phase === 'idle' && (prev?.phase === 'restarting' || prev?.phase === 'applying')) {
    busy.value = false
    userHidden.value = false
    stopPollFallback()
    show.value = false
    message.success('更新完成，正在重新加载页面…')
    window.setTimeout(() => window.location.reload(), 1200)
    return
  }

  if (s.phase === 'idle' && s.hasUpdate && !s.skipped) {
    const isNew = !prev?.hasUpdate || prev.latestVersion !== s.latestVersion
    if (isNew && !dismissed.value) {
      show.value = true
    }
  }
}

defineExpose({
  /** 供 App.vue 的 SSE 回调转发。 */
  onState,
  /** 首次拉到的快照里就是「安装中」时补一次弹框（App.vue bootstrap 之后调用）。 */
  syncInstalling,
  /** 点侧边栏版本号：手动打开（会先强制检查一次）。 */
  open,
  /** 侧边栏小圆点用。 */
  available: computed(() => store.updateAvailable),
})

async function open() {
  dismissed.value = false
  userHidden.value = false
  try {
    store.setUpdateState(await api.update.check())
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
    return
  }
  const s = store.updateState
  // 检查失败（后端返回 200 + phase=failed）要如实报错，不能报成「已是最新」。
  if (s?.phase === 'failed' || s?.error) {
    message.error(s.error || '检查更新失败')
    return
  }
  if (!s?.hasUpdate) {
    message.success(`已是最新版本（${s?.latestVersion || store.version || 'dev'}）`)
    return
  }
  show.value = true
}

async function startUpdate() {
  if (busy.value) return
  busy.value = true
  userHidden.value = false
  show.value = true
  try {
    // 202 只表示「已受理」。真实进度全部走 SSE —— 不要用响应覆盖 store，
    // 否则会把已经收到的 downloading 打回 idle。
    await api.update.install()
    startPollFallback()
  } catch (e) {
    busy.value = false
    message.error(e instanceof Error ? e.message : String(e))
  }
}

/**
 * SSE 断线时的兜底轮询。
 *
 * 安装进度只靠 SSE 推送，一旦连接断了，phase 会永远停在 downloading：
 * 进度条冻住、按钮一直转圈，用户只能刷新页面。这里每 3 秒拉一次真实状态，
 * 拿到终态（idle/failed）就停。
 */
let pollTimer: ReturnType<typeof setInterval> | null = null

function stopPollFallback() {
  if (pollTimer !== null) {
    clearInterval(pollTimer)
    pollTimer = null
  }
}

function startPollFallback() {
  stopPollFallback()
  pollTimer = setInterval(async () => {
    try {
      const s = await api.update.get()
      onState(s)
      if (s.phase === 'idle' || s.phase === 'failed') stopPollFallback()
    } catch {
      // 服务端正在重启（连接被拒）属于正常，继续轮询等它回来。
    }
  }, 3000)
}

onBeforeUnmount(stopPollFallback)

/**
 * 页面在安装过程中打开/刷新时，那条 SSE 事件已经错过了。
 * 这里只补「正在安装」这几种状态：idle + 有新版本不自动弹，
 * 否则每次刷新页面都会被更新弹框拦住。
 */
function syncInstalling() {
  const s = store.updateState
  if (!s) return
  if (s.phase === 'downloading' || s.phase === 'verifying' || s.phase === 'applying' || s.phase === 'restarting') {
    onState({ ...s })
  }
}

async function skipThisVersion() {
  const v = state.value?.latestVersion ?? ''
  try {
    store.setUpdateState(await api.update.skip(v))
    show.value = false
    message.info(`已跳过 ${v}，可在「设置 → 服务」里取消`)
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  }
}

/**
 * 关闭弹框。
 *
 * 安装中允许关：SSE 一断，phase 会永远停在 downloading，若这里硬拦着，
 * 用户既关不掉弹框、也看不到结果 —— 只能刷新页面。关掉不影响后台安装，
 * 装完重启时页面自然会重载。
 */
function close() {
  show.value = false
  if (installing.value) {
    // 安装中关窗只表示「不想盯着看」，别把后续进度事件又弹回来。
    userHidden.value = true
    return
  }
  dismissed.value = true
}

function openReleasePage() {
  const url = state.value?.htmlUrl
  if (url) window.open(url, '_blank', 'noopener')
}

// 安装结束时停掉兜底轮询。
watch(installing, (v, old) => {
  if (!v && old) {
    busy.value = false
    stopPollFallback()
  }
})
</script>

<template>
  <NModal
    :show="show"
    :mask-closable="false"
    :close-on-esc="true"
    @update:show="(v: boolean) => { if (!v) close() }"
  >
    <div class="update-dialog" role="dialog" aria-modal="true" aria-labelledby="update-dialog-title">
      <button class="update-dialog-x" type="button" aria-label="稍后再说" @click="close">
        <NIcon :size="16"><X /></NIcon>
      </button>

      <div class="update-dialog-head">
        <div class="update-dialog-mark">
          <NIcon :size="22"><Download /></NIcon>
        </div>
        <div>
          <h2 id="update-dialog-title">{{ dialogTitle }}</h2>
          <p>
            {{ store.version || 'dev' }}
            <span class="update-arrow">→</span>
            <strong>{{ state?.latestVersion }}</strong>
            <span v-if="state?.assetSize" class="update-size">· {{ formatBytes(state.assetSize) }}</span>
          </p>
        </div>
      </div>

      <template v-if="installing">
        <NProgress
          type="line"
          :percentage="state?.progress ?? 0"
          :processing="state?.phase !== 'restarting'"
        />
        <p class="update-phase" role="status" aria-live="polite">{{ phaseText }}</p>
      </template>

      <template v-else-if="state?.phase === 'failed'">
        <NAlert type="error" :bordered="false" class="update-error">{{ state.error }}</NAlert>
      </template>

      <template v-else>
        <div v-if="state?.notes" class="update-notes">
          <div class="update-notes-title">更新内容</div>
          <pre class="update-notes-body">{{ state.notes }}</pre>
        </div>
        <NAlert v-if="!state?.canAutoInstall" type="info" :bordered="false" class="update-error">
          当前形态不支持自动安装（开发调试形态或非 Windows），请手动下载替换。
        </NAlert>
      </template>

      <div class="update-dialog-foot">
        <template v-if="installing">
          <span class="update-hint">更新完成后程序会自动重启；现在关掉这个窗口不影响安装</span>
        </template>
        <template v-else>
          <NButton quaternary @click="close">稍后</NButton>
          <NButton v-if="state?.hasUpdate" quaternary @click="skipThisVersion">跳过此版本</NButton>
          <!-- 没有新版本时不能出现「立即更新」：重启后的状态里 latestVersion 仍带着
               上一次的版本号，只看 canAutoInstall 会给出一个点了没反应的按钮。 -->
          <NButton
            v-if="state?.hasUpdate && state?.canAutoInstall"
            type="primary"
            :loading="busy"
            @click="startUpdate"
          >
            <template #icon>
              <NIcon :size="15"><Download /></NIcon>
            </template>
            立即更新
          </NButton>
          <NButton
            v-else-if="state?.hasUpdate"
            type="primary"
            :disabled="!state?.htmlUrl"
            @click="openReleasePage"
          >
            打开下载页
          </NButton>
        </template>
      </div>
    </div>
  </NModal>
</template>

<style scoped>
.update-dialog {
  position: relative;
  width: min(560px, calc(100vw - 40px));
  padding: 24px;
  border-radius: 14px;
  background: var(--surface, #fff);
  border: 1px solid var(--border, rgba(0, 0, 0, 0.08));
  box-shadow: 0 18px 48px rgba(0, 0, 0, 0.22);
}

.update-dialog-x {
  position: absolute;
  top: 12px;
  right: 12px;
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 28px;
  height: 28px;
  padding: 0;
  border: 0;
  border-radius: 8px;
  background: transparent;
  color: var(--text-3);
  cursor: pointer;
}

.update-dialog-x:hover {
  background: rgba(127, 127, 127, 0.12);
  color: var(--text);
}

.update-dialog-head {
  display: flex;
  align-items: flex-start;
  gap: 12px;
  margin-bottom: 16px;
}

.update-dialog-mark {
  display: inline-flex;
  align-items: center;
  justify-content: center;
  width: 40px;
  height: 40px;
  flex-shrink: 0;
  border-radius: 10px;
  border: 1px solid var(--accent-border);
  background: var(--accent-soft);
  color: var(--accent);
}

.update-dialog-head h2 {
  margin: 0 0 4px;
  font-size: var(--text-lg);
  color: var(--text);
}

.update-dialog-head p {
  margin: 0;
  font-size: var(--text-xs);
  color: var(--text-2);
}

.update-arrow {
  margin: 0 6px;
  color: var(--text-3);
}

.update-size {
  margin-left: 4px;
  color: var(--text-3);
}

.update-notes {
  margin-bottom: 14px;
}

.update-notes-title {
  font-size: var(--text-2xs);
  color: var(--text-3);
  margin-bottom: 6px;
}

.update-notes-body {
  margin: 0;
  max-height: 220px;
  overflow: auto;
  padding: 10px 12px;
  border-radius: 8px;
  background: rgba(127, 127, 127, 0.08);
  font-size: var(--text-xs);
  line-height: 1.6;
  white-space: pre-wrap;
  word-break: break-word;
  color: var(--text-2);
}

.update-phase {
  margin: 10px 0 0;
  font-size: var(--text-xs);
  color: var(--text-2);
}

.update-error {
  margin-bottom: 14px;
  font-size: var(--text-xs);
}

.update-dialog-foot {
  display: flex;
  align-items: center;
  justify-content: flex-end;
  gap: 8px;
  margin-top: 18px;
}

.update-hint {
  font-size: var(--text-2xs);
  color: var(--text-3);
}
</style>
