<script setup lang="ts">
import { onMounted, reactive, ref, computed } from 'vue'
import {
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NSwitch,
  NButton,
  NDivider,
  NAlert,
  NSelect,
  NRadioGroup,
  NRadioButton,
  NRadio,
  NTabs,
  NTabPane,
  NTag,
  NIcon,
  NSpin,
  useMessage,
} from 'naive-ui'
import { FolderOpen, RefreshCw, Save, FileText, Download, RotateCw } from 'lucide-vue-next'
import PageHeader from '@/components/PageHeader.vue'
import AppCard from '@/components/AppCard.vue'
import { api } from '@/api'
import { useAppStore } from '@/stores/app'
import type { AutostartState, Settings, UpdateState } from '@/types'
import { PROMPT_STRATEGY_LABELS } from '@/types'
import { desktopApi, type DesktopConfig } from '@/api/index'

const store = useAppStore()
const message = useMessage()

const form = reactive<Settings>({
  host: '127.0.0.1',
  port: 8080,
  accessKey: '',
  proxy: { enabled: false, type: 'http', url: '', username: '', password: '', noProxy: [] },
  globalPrompt: { enabled: false, text: '', strategy: 'append' },
  usageInjectDefault: true,
  defaultTimeoutSeconds: 600,
  defaultConnectTimeoutSeconds: 15,
  imageCompress: { enabled: true, thresholdBytes: 524288, maxEdge: 2048, quality: 85 },
  storeBodies: true,
  maxBodyBytes: 65536,
  retentionDays: 30,
  maxLogs: 200000,
})

const saving = ref(false)
const loading = ref(true)
const noProxyText = ref('')
const keyCleared = ref(false)

const strategyOptions = Object.entries(PROMPT_STRATEGY_LABELS).map(([value, label]) => ({
  label,
  value,
}))

const retentionOptions = [
  { label: '保留 7 天', value: 7 },
  { label: '保留 30 天', value: 30 },
  { label: '保留 90 天', value: 90 },
  { label: '保留 365 天', value: 365 },
  { label: '不按时间清理', value: 0 },
]

async function load() {
  loading.value = true
  try {
    const s = await api.getSettings()
    Object.assign(form, s)
    if (!form.proxy) {
      form.proxy = { enabled: false, type: 'http', url: '', username: '', password: '', noProxy: [] }
    }
    // 老版本后端没有这个字段：补一份默认值，避免表单里出现 undefined。
    if (!form.imageCompress) {
      form.imageCompress = { enabled: true, thresholdBytes: 524288, maxEdge: 2048, quality: 85 }
    }
    noProxyText.value = (form.proxy.noProxy ?? []).join('\n')
    keyCleared.value = false
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    loading.value = false
  }
}

onMounted(async () => {
  await load()
  await loadAutostart()
  if (store.isDesktop) await loadDesktop()
})

async function save() {
  saving.value = true
  try {
    const payload: Settings = {
      ...form,
      // 留空表示「保持原值」；要清除密钥得走下面的显式按钮。
      accessKey: keyCleared.value ? '' : form.accessKey,
      proxy: {
        ...form.proxy,
        noProxy: noProxyText.value
          .split(/[\n,]/)
          .map((t) => t.trim())
          .filter(Boolean),
      },
    }
    const saved = await api.saveSettings(payload)
    store.settings = saved
    Object.assign(form, saved)
    keyCleared.value = false
    message.success('已保存')
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    saving.value = false
  }
}

function clearAccessKey() {
  form.accessKey = ''
  keyCleared.value = true
  message.info('保存后访问密钥将被清除')
}

const currentBase = () => store.listenBaseURL

/* ---------- 桌面应用（仅桌面模式） ---------- */

const desktopConfig = ref<DesktopConfig | null>(null)
const desktopInfo = ref<{ dataDir: string; logDir: string; dbPath: string } | null>(null)
const desktopLoading = ref(false)

const closeOptions = [
  { label: '每次询问（默认）', value: 'ask' as const },
  { label: '直接退出', value: 'quit' as const },
  { label: '托盘运行，代理继续工作', value: 'tray' as const },
]

async function loadDesktop() {
  try {
    const [cfg, info] = await Promise.all([desktopApi.getSettings(), desktopApi.info()])
    desktopConfig.value = cfg
    desktopInfo.value = info
  } catch {
    /* 非桌面环境或接口不可用：整块入口本来就不显示 */
  }
}

async function saveCloseAction(v: DesktopConfig['closeAction']) {
  if (!desktopConfig.value) return
  desktopLoading.value = true
  try {
    desktopConfig.value = await desktopApi.saveSettings({ closeAction: v })
    message.success('已保存关闭行为')
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    desktopLoading.value = false
  }
}

async function openDir(target: 'data' | 'logs') {
  try {
    await desktopApi.open(target)
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  }
}

/* ---------- 开机自启 ---------- */

const autostart = ref<AutostartState | null>(null)
const autostartBusy = ref(false)

async function loadAutostart() {
  try {
    autostart.value = await api.autostart.get()
  } catch {
    // 老版本后端没有这个接口：整块入口保持禁用即可。
    autostart.value = null
  }
}

async function toggleAutostart(enabled: boolean) {
  autostartBusy.value = true
  // 先本地翻转：写注册表通常很快，但网络慢时开关停在原位会让人以为没点上。
  // 失败时用 loadAutostart() 拉回真实状态。
  const prev = autostart.value
  if (autostart.value) autostart.value = { ...autostart.value, enabled }
  try {
    autostart.value = await api.autostart.set(enabled)
    message.success(enabled ? '已开启开机自启' : '已关闭开机自启')
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
    // 失败时把开关拨回真实状态，别让界面骗人。
    if (prev) autostart.value = prev
    await loadAutostart()
  } finally {
    autostartBusy.value = false
  }
}

/* ---------- 版本更新 ---------- */

// 更新状态放在 store 里：弹框组件与这里共用同一份，否则两处各自拉一次必然漂移。
const updateState = computed<UpdateState | null>(() => store.updateState)
const updateBusy = ref(false)

async function checkUpdate() {
  updateBusy.value = true
  try {
    const s = await api.update.check()
    store.setUpdateState(s)
    // 检查失败时后端返回 200 + phase=failed，要如实报错，不能报成「已是最新」。
    if (s.phase === 'failed' || s.error) {
      message.error(s.error || '检查更新失败')
    } else if (s.hasUpdate) {
      message.success(`发现新版本 ${s.latestVersion}`)
    } else {
      message.success('已是最新版本')
    }
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    updateBusy.value = false
  }
}

async function clearSkip() {
  try {
    store.setUpdateState(await api.update.clearSkip())
    message.success('已取消跳过，下次检查会重新提示')
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  }
}

function openReleasePage() {
  const url = updateState.value?.htmlUrl
  if (url) window.open(url, '_blank', 'noopener')
}

/**
 * 从设置页发起安装。
 *
 * 进度由 App.vue 里的更新弹框统一展示（它是 SSE 的消费方），
 * 这里只负责把请求打出去并给出即时反馈。
 */
async function startUpdateFromSettings() {
  if (updateBusy.value) return
  updateBusy.value = true
  try {
    await api.update.install()
    message.info('已开始更新，进度见弹出的更新窗口；完成后程序会自动重启')
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    updateBusy.value = false
  }
}
</script>

<template>
  <div class="page">
    <PageHeader title="设置" description="全局配置。改动监听地址与端口需要重启程序才会生效。">
      <template #actions>
        <NButton :loading="loading" @click="load">
          <template #icon>
            <NIcon :size="15"><RefreshCw /></NIcon>
          </template>
          重新载入
        </NButton>
        <NButton type="primary" :loading="saving" @click="save">
          <template #icon>
            <NIcon :size="15"><Save /></NIcon>
          </template>
          保存
        </NButton>
      </template>
    </PageHeader>

    <NTabs type="line" animated>
      <NTabPane name="service" tab="服务">
        <div class="settings-grid">
          <div class="col-main">
            <AppCard title="监听与访问">
              <NForm label-placement="left" :label-width="132" size="small" class="settings-form">
                <NFormItem label="监听地址">
                  <div class="field">
                    <NInput v-model:value="form.host" placeholder="127.0.0.1" />
                    <span class="field-hint">改成 0.0.0.0 可以给同网段其它设备用</span>
                  </div>
                </NFormItem>
                <NFormItem label="监听端口">
                  <NInputNumber v-model:value="form.port" :min="1" :max="65535" class="field-narrow" />
                </NFormItem>
                <NFormItem label="访问密钥">
                  <div class="field-row">
                    <NInput
                      v-model:value="form.accessKey"
                      type="password"
                      show-password-on="click"
                      placeholder="留空表示不校验"
                      class="field-narrow"
                    />
                    <NButton v-if="store.settings?.accessKey" size="small" @click="clearAccessKey">
                      清除密钥
                    </NButton>
                  </div>
                </NFormItem>
              </NForm>
              <NAlert
                v-if="form.host !== '127.0.0.1' && form.host !== 'localhost' && !form.accessKey"
                type="warning"
                :bordered="false"
                class="inline-alert"
              >
                监听非本机地址但没有设置访问密钥：同网段的任何人都能使用这个代理、查看全部请求日志。
                建议设置一个密钥。
              </NAlert>
            </AppCard>

            <AppCard title="客户端接入" class="card-gap">
              <div class="hint-list">
                <div class="hint-row">
                  <span class="hint-label">固定入口</span>
                  <code class="mono">{{ currentBase() }}</code>
                  <NTag size="tiny" :bordered="false" type="success">所有请求走当前生效供应商</NTag>
                </div>
                <div class="hint-row">
                  <span class="hint-label">指定供应商</span>
                  <code class="mono">{{ currentBase() }}/p/&lt;短名&gt;/v1/…</code>
                  <NTag size="tiny" :bordered="false">同机多客户端可各走各的</NTag>
                </div>
              </div>
              <NDivider class="hint-divider" />
              <div class="hint-list">
                <div class="hint-row">
                  <span class="hint-label">示例</span>
                  <code class="mono">OPENAI_BASE_URL={{ currentBase() }}/v1</code>
                </div>
                <div class="hint-row">
                  <span class="hint-label"></span>
                  <code class="mono">ANTHROPIC_BASE_URL={{ currentBase() }}</code>
                </div>
              </div>
            </AppCard>
          </div>

          <div class="col-side">
            <AppCard title="开机自启">
              <NForm label-placement="left" :label-width="132" size="small">
                <NFormItem label="登录时自动启动">
                  <NSwitch
                    :value="autostart?.enabled ?? false"
                    :disabled="autostartBusy || !autostart?.supported"
                    :loading="autostartBusy"
                    @update:value="toggleAutostart"
                  />
                </NFormItem>
              </NForm>
              <div v-if="autostart?.command" class="path-list">
                <div class="path-row">
                  <span class="path-label">启动命令</span>
                  <code class="mono path-value">{{ autostart.command }}</code>
                </div>
              </div>
              <NAlert v-if="autostart && !autostart.supported" type="warning" :bordered="false" class="inline-alert">
                {{ autostart.reason }}
              </NAlert>
              <NAlert v-else-if="!autostart" type="warning" :bordered="false" class="inline-alert">
                当前后端不提供开机自启接口（可能是旧版本），升级后可用。
              </NAlert>
              <NAlert v-else-if="autostart.stale" type="warning" :bordered="false" class="inline-alert">
                启动项指向的是旧路径（{{ autostart.registered }}）。程序被移动过位置，
                关掉再打开一次开关即可更新。
              </NAlert>
              <NAlert v-else type="info" :bordered="false" class="inline-alert">
                <template v-if="store.isDesktop">
                  登录后自动启动并隐藏到系统托盘，代理立刻可用；双击托盘图标打开窗口。
                </template>
                <template v-else>
                  登录后以控制台方式自动启动，数据目录用绝对路径写入启动项，
                  所以从任何工作目录启动都能找到同一个数据库。
                </template>
                这一项直接写系统启动项，不走「保存」按钮。
              </NAlert>
            </AppCard>

            <AppCard title="版本与更新" class="card-gap">
              <div class="about-list">
                <div class="about-row">
                  <span class="about-label">当前版本</span>
                  <span class="mono">{{ store.version || 'dev' }}</span>
                </div>
                <div v-if="updateState?.latestVersion" class="about-row">
                  <span class="about-label">最新版本</span>
                  <span class="mono">{{ updateState.latestVersion }}</span>
                </div>
                <div v-if="updateState?.checkedAt" class="about-row">
                  <span class="about-label">上次检查</span>
                  <span class="mono">{{ new Date(updateState.checkedAt).toLocaleString() }}</span>
                </div>
              </div>

              <div v-if="updateState?.skippedVersion" class="path-list skip-row">
                <div class="path-row">
                  <span class="path-label">已跳过</span>
                  <code class="mono path-value">{{ updateState.skippedVersion }}</code>
                  <NButton size="tiny" @click="clearSkip">不再跳过</NButton>
                </div>
              </div>

              <div class="quick-actions update-actions">
                <NButton block :loading="updateBusy" @click="checkUpdate">
                  <template #icon>
                    <NIcon :size="14"><RotateCw /></NIcon>
                  </template>
                  检查更新
                </NButton>
                <NButton
                  v-if="updateState?.hasUpdate && updateState.canAutoInstall"
                  block
                  type="primary"
                  :loading="updateBusy"
                  @click="startUpdateFromSettings"
                >
                  <template #icon>
                    <NIcon :size="14"><Download /></NIcon>
                  </template>
                  立即更新到 {{ updateState.latestVersion }}
                </NButton>
                <NButton
                  v-else-if="updateState?.hasUpdate"
                  block
                  type="primary"
                  :disabled="!updateState.htmlUrl"
                  @click="openReleasePage"
                >
                  <template #icon>
                    <NIcon :size="14"><Download /></NIcon>
                  </template>
                  打开下载页
                </NButton>
              </div>

              <NAlert v-if="updateState?.error" type="warning" :bordered="false" class="inline-alert">
                {{ updateState.error }}
              </NAlert>
              <NAlert v-else-if="updateState?.hasUpdate" type="info" :bordered="false" class="inline-alert">
                有新版本 {{ updateState.latestVersion }} 可用。启动时也会自动检查一次，
                发现新版本会弹出确认框，可以直接一键更新并重启。
              </NAlert>
              <NAlert v-else type="info" :bordered="false" class="inline-alert">
                更新包来自 GitHub Release，安装前会校验 SHA-256。
                自动检查每 6 小时最多一次，手动检查不受限制。
              </NAlert>
            </AppCard>

            <AppCard title="默认超时" class="card-gap">
              <NForm label-placement="left" :label-width="132" size="small">
                <NFormItem label="上游总超时（秒）">
                  <NInputNumber
                    v-model:value="form.defaultTimeoutSeconds"
                    :min="0"
                    :max="86400"
                    class="field-narrow"
                  />
                </NFormItem>
                <NFormItem label="连接超时（秒）">
                  <NInputNumber
                    v-model:value="form.defaultConnectTimeoutSeconds"
                    :min="1"
                    :max="600"
                    class="field-narrow"
                  />
                </NFormItem>
              </NForm>
              <NAlert type="info" :bordered="false" class="inline-alert">
                AI 请求的生成耗时跨度很大，总超时默认给了 10 分钟。填 0 表示不限制。
                单个供应商可以在自己的配置里覆盖这两项。
              </NAlert>
            </AppCard>

            <AppCard title="日志保留" class="card-gap">
              <NForm label-placement="left" :label-width="132" size="small">
                <NFormItem label="存储请求响应体">
                  <NSwitch v-model:value="form.storeBodies" />
                </NFormItem>
                <NFormItem label="单条报文上限">
                  <div class="field-row">
                    <NInputNumber
                      v-model:value="form.maxBodyBytes"
                      :min="1024"
                      :max="10485760"
                      :step="1024"
                      :disabled="!form.storeBodies"
                      class="field-narrow"
                    />
                    <span class="field-hint">字节</span>
                  </div>
                </NFormItem>
                <NFormItem label="保留时长">
                  <NSelect v-model:value="form.retentionDays" :options="retentionOptions" class="field-narrow" />
                </NFormItem>
                <NFormItem label="最多保留条数">
                  <div class="field-row">
                    <NInputNumber
                      v-model:value="form.maxLogs"
                      :min="0"
                      :max="10000000"
                      :step="10000"
                      class="field-narrow"
                    />
                    <span class="field-hint">0 = 不限制</span>
                  </div>
                </NFormItem>
              </NForm>
              <NAlert type="info" :bordered="false" class="inline-alert">
                后台每小时清理一次。关掉报文存储后仍会记录全部统计指标，只是详情页看不到请求与响应内容。
              </NAlert>
            </AppCard>
          </div>
        </div>
      </NTabPane>

      <NTabPane name="proxy" tab="全局代理（VPN）">
        <AppCard>
          <NForm label-placement="left" :label-width="132" size="small" class="settings-form">
            <NFormItem label="启用全局代理">
              <NSwitch v-model:value="form.proxy.enabled" />
            </NFormItem>
            <NFormItem label="代理类型">
              <NRadioGroup v-model:value="form.proxy.type">
                <NRadioButton value="http">HTTP / HTTPS</NRadioButton>
                <NRadioButton value="socks5">SOCKS5</NRadioButton>
              </NRadioGroup>
            </NFormItem>
            <NFormItem label="代理地址">
              <div class="field">
                <NInput
                  v-model:value="form.proxy.url"
                  :placeholder="form.proxy.type === 'socks5' ? '127.0.0.1:1080' : '127.0.0.1:7890'"
                  class="field-wide"
                />
                <span class="field-hint">host:port，带不带 http:// 都可以</span>
              </div>
            </NFormItem>
            <NFormItem label="用户名">
              <NInput v-model:value="form.proxy.username" placeholder="可留空" class="field-wide" />
            </NFormItem>
            <NFormItem label="密码">
              <NInput
                v-model:value="form.proxy.password"
                type="password"
                show-password-on="click"
                placeholder="可留空"
                class="field-wide"
              />
            </NFormItem>
            <NFormItem label="不走代理的域名">
              <NInput
                v-model:value="noProxyText"
                type="textarea"
                :rows="4"
                placeholder="每行一个，例如&#10;internal.corp&#10;localhost"
                class="field-wide"
              />
            </NFormItem>
          </NForm>
          <NAlert type="info" :bordered="false" class="inline-alert">
            全局代理对所有「代理模式 = 继承全局」的供应商生效。
            需要单独设置或完全直连的供应商，可在其编辑页的「网络代理」里覆盖。
            保存后连接缓存会立即失效并重建。
          </NAlert>
        </AppCard>
      </NTabPane>

      <NTabPane name="prompt" tab="全局提示词">
        <AppCard>
          <NForm label-placement="left" :label-width="132" size="small" class="settings-form">
            <NFormItem label="启用">
              <NSwitch v-model:value="form.globalPrompt.enabled" />
            </NFormItem>
            <NFormItem label="注入方式">
              <NSelect
                v-model:value="form.globalPrompt.strategy"
                :options="strategyOptions"
                class="field-wide"
                :disabled="!form.globalPrompt.enabled"
              />
            </NFormItem>
            <NFormItem label="提示词内容">
              <NInput
                v-model:value="form.globalPrompt.text"
                type="textarea"
                :rows="8"
                class="field-full"
                :disabled="!form.globalPrompt.enabled"
                placeholder="例如：始终使用简体中文回答，代码示例保留英文注释。"
              />
            </NFormItem>
          </NForm>

          <NDivider class="hint-divider" />
          <div class="hint-list">
            <div class="hint-row">
              <span class="hint-label">优先级</span>
              <span>供应商的模型正则规则 &gt; 供应商自己的提示词 &gt; 这里的全局提示词</span>
            </div>
            <div class="hint-row">
              <span class="hint-label">生效范围</span>
              <span>
                只对「提示词模式 = 继承全局」的供应商生效。三种注入方式分别适配
                OpenAI 的 messages、Anthropic 的顶层 system、Gemini 的 systemInstruction。
              </span>
            </div>
            <div class="hint-row">
              <span class="hint-label">留痕</span>
              <span>凡是注入过提示词的请求，日志里都会标记「已改写」，并保存改写前后的请求体供对比。</span>
            </div>
          </div>
        </AppCard>

        <AppCard title="统计口径" class="card-gap">
          <NForm label-placement="left" :label-width="132" size="small">
            <NFormItem label="注入 include_usage">
              <NSwitch v-model:value="form.usageInjectDefault" />
            </NFormItem>
          </NForm>
          <NAlert type="info" :bordered="false" class="inline-alert">
            对 OpenAI Chat Completions 格式的流式请求注入
            <code class="mono">stream_options: {"include_usage": true}</code>，
            让上游在流末尾返回精确的 token 用量。
            关闭后，流式请求的 token 数只能用本地规则估算（日志里会标注「估算」）。
            单个供应商可以覆盖这一项。
          </NAlert>
        </AppCard>
      </NTabPane>

      <NTabPane name="rewrite" tab="请求改写">
        <div class="settings-grid">
          <div class="col-main">
            <AppCard title="图片压缩">
              <NForm label-placement="left" :label-width="132" size="small" class="settings-form">
                <NFormItem label="启用">
                  <NSwitch v-model:value="form.imageCompress.enabled" />
                </NFormItem>
                <NFormItem label="触发阈值">
                  <div class="field-row">
                    <NInputNumber
                      v-model:value="form.imageCompress.thresholdBytes"
                      :min="10240"
                      :max="52428800"
                      :step="102400"
                      :disabled="!form.imageCompress.enabled"
                      class="field-narrow"
                    />
                    <span class="field-hint">字节，超过就压缩</span>
                  </div>
                </NFormItem>
                <NFormItem label="长边上限">
                  <div class="field-row">
                    <NInputNumber
                      v-model:value="form.imageCompress.maxEdge"
                      :min="0"
                      :max="8192"
                      :step="256"
                      :disabled="!form.imageCompress.enabled"
                      class="field-narrow"
                    />
                    <span class="field-hint">像素，0 = 只按字节判断</span>
                  </div>
                </NFormItem>
                <NFormItem label="JPEG 质量">
                  <div class="field-row">
                    <NInputNumber
                      v-model:value="form.imageCompress.quality"
                      :min="1"
                      :max="100"
                      :step="5"
                      :disabled="!form.imageCompress.enabled"
                      class="field-narrow"
                    />
                    <span class="field-hint">1-100，只影响不透明图片</span>
                  </div>
                </NFormItem>
              </NForm>
              <NAlert type="info" :bordered="false" class="inline-alert">
                只处理请求体里<strong>内联</strong>的图片（data URL / base64），不会去下载远程图片。
                触发条件是「或」：字节数超阈值，或者长边超上限 —— 后者能救下那种体积不大、
                但分辨率极高的截图（PNG 压得很小，视觉 token 却很贵）。
                <br />
                输出格式：带透明像素的图保持 PNG，其余转 JPEG。GIF 与无法解码的图跳过；
                压完反而更大时保留原图。压缩是确定性的（同一张图每次结果逐字节相同），
                所以不会破坏 Responses 续传的前缀匹配。
              </NAlert>
            </AppCard>
          </div>

          <div class="col-side">
            <AppCard title="为什么值得开">
              <div class="hint-list">
                <div class="hint-row">
                  <span class="hint-label">体积</span>
                  <span>手机截图常见 2–4 MB，长边压到 2048 后通常只剩 10%–20%。</span>
                </div>
                <div class="hint-row">
                  <span class="hint-label">延迟</span>
                  <span>请求体变小直接降低上行时间，多图会话的首 token 延迟改善最明显。</span>
                </div>
                <div class="hint-row">
                  <span class="hint-label">留痕</span>
                  <span>
                    每次压缩都会在日志里记下张数与节省字节，详情页能对照改写前后的请求体。
                  </span>
                </div>
              </div>
            </AppCard>

            <AppCard title="注意" class="card-gap">
              <div class="hint-list">
                <div class="hint-row">
                  <span class="hint-label">画质</span>
                  <span>这是有损压缩。需要逐像素比对（例如 OCR 校验）时，把阈值调大或直接关掉。</span>
                </div>
                <div class="hint-row">
                  <span class="hint-label">CPU</span>
                  <span>压缩在改写阶段同步执行，单请求最多处理 16 张、单图 50MP 上限。</span>
                </div>
              </div>
            </AppCard>
          </div>
        </div>
      </NTabPane>

      <NTabPane v-if="store.isDesktop" name="desktop" tab="桌面应用">
        <div class="settings-grid">
          <div class="col-main">
            <AppCard title="关闭窗口时">
              <p class="desk-desc">
                关闭窗口时默认会询问一次。选择「托盘运行」后窗口隐藏，代理继续在后台工作；
                双击任务栏通知区的托盘图标可以重新打开，右键托盘图标可以退出。
                关闭确认框里勾选「记住我的选择」后不再询问，也可以随时在本页修改。
              </p>
              <NRadioGroup
                :value="desktopConfig?.closeAction ?? 'ask'"
                :disabled="desktopLoading || !desktopConfig"
                @update:value="saveCloseAction"
              >
                <div class="close-options">
                  <NRadio v-for="o in closeOptions" :key="o.value" :value="o.value">
                    {{ o.label }}
                  </NRadio>
                </div>
              </NRadioGroup>
            </AppCard>

            <AppCard title="数据位置" class="card-gap">
              <NSpin :show="!desktopInfo">
                <div class="path-list">
                  <div class="path-row">
                    <span class="path-label">数据目录</span>
                    <code class="mono path-value">{{ desktopInfo?.dataDir || '—' }}</code>
                    <NButton size="tiny" @click="openDir('data')">
                      <template #icon>
                        <NIcon :size="13"><FolderOpen /></NIcon>
                      </template>
                      打开
                    </NButton>
                  </div>
                  <div class="path-row">
                    <span class="path-label">日志目录</span>
                    <code class="mono path-value">{{ desktopInfo?.logDir || '—' }}</code>
                    <NButton size="tiny" @click="openDir('logs')">
                      <template #icon>
                        <NIcon :size="13"><FolderOpen /></NIcon>
                      </template>
                      打开
                    </NButton>
                  </div>
                  <div class="path-row">
                    <span class="path-label">数据库</span>
                    <code class="mono path-value">{{ desktopInfo?.dbPath || '—' }}</code>
                    <span class="path-fill" />
                  </div>
                </div>
              </NSpin>
              <NAlert type="info" :bordered="false" class="inline-alert">
                桌面版优先沿用程序同目录下的 data；不存在时使用用户配置目录。
                日志按 5 MB 轮转，保留 5 份，可以在这里直接打开排查问题。
              </NAlert>
            </AppCard>
          </div>

          <div class="col-side">
            <AppCard title="版本信息">
              <div class="about-list">
                <div class="about-row">
                  <span class="about-label">版本</span>
                  <span class="mono">{{ store.version || 'dev' }}</span>
                </div>
                <div class="about-row">
                  <span class="about-label">运行形态</span>
                  <NTag size="tiny" type="info" :bordered="false">桌面窗口</NTag>
                </div>
                <div class="about-row">
                  <span class="about-label">实际监听</span>
                  <span class="mono">{{ store.listenBaseURL }}</span>
                </div>
              </div>
            </AppCard>

            <AppCard title="快速操作" class="card-gap">
              <div class="quick-actions">
                <NButton block @click="openDir('logs')">
                  <template #icon>
                    <NIcon :size="14"><FileText /></NIcon>
                  </template>
                  查看运行日志
                </NButton>
                <NButton block @click="openDir('data')">
                  <template #icon>
                    <NIcon :size="14"><FolderOpen /></NIcon>
                  </template>
                  打开数据目录
                </NButton>
              </div>
            </AppCard>
          </div>
        </div>
      </NTabPane>
    </NTabs>
  </div>
</template>

<style scoped>
.page {
  width: 100%;
  max-width: 1440px;
  margin: 0 auto;
}

/* 主列更宽、侧列更窄；窄窗口塌成单列。 */
.settings-grid {
  display: grid;
  grid-template-columns: minmax(0, 1.45fr) minmax(0, 1fr);
  gap: var(--space-3);
  align-items: start;
}

@media (max-width: 1080px) {
  .settings-grid {
    grid-template-columns: minmax(0, 1fr);
  }
}

.col-main,
.col-side {
  min-width: 0;
}

.card-gap {
  margin-top: var(--space-3);
}

.settings-form :deep(.n-form-item) {
  margin-bottom: var(--space-3);
}

/* 输入框宽度自适应容器，不再写死 px；上限避免超宽屏上拉得过长。 */
.field {
  display: flex;
  flex-direction: column;
  gap: 4px;
  width: 100%;
  max-width: 380px;
}

.field-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  flex-wrap: wrap;
  width: 100%;
  max-width: 440px;
}

.field-narrow {
  width: 100%;
  max-width: 240px;
}

.field-wide {
  width: 100%;
  max-width: 380px;
}

.field-full {
  width: 100%;
  max-width: 720px;
}

.field-hint {
  font-size: var(--text-2xs);
  color: var(--text-3);
  line-height: 1.5;
}

.inline-alert {
  margin-top: var(--space-3);
  font-size: var(--text-xs);
  line-height: 1.6;
}

.hint-list {
  display: flex;
  flex-direction: column;
  gap: 6px;
  font-size: var(--text-xs);
  color: var(--text-2);
  line-height: 1.7;
}

.hint-row {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}

.hint-label {
  width: 76px;
  flex-shrink: 0;
  color: var(--text-3);
}

.hint-divider {
  margin: var(--space-3) 0;
}

/* ---------- 桌面应用页 ---------- */

.desk-desc {
  margin: 0 0 var(--space-3);
  font-size: var(--text-xs);
  color: var(--text-2);
  line-height: 1.65;
}

.close-options {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

.path-list {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

.path-row {
  display: flex;
  align-items: center;
  gap: var(--space-2);
  min-width: 0;
}

.path-label {
  width: 68px;
  flex-shrink: 0;
  font-size: var(--text-xs);
  color: var(--text-3);
}

.path-value {
  flex: 1 1 auto;
  min-width: 0;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  color: var(--text-2);
}

.path-fill {
  width: 61px;
  flex-shrink: 0;
}

.about-list {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

.about-row {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: var(--space-2);
  font-size: var(--text-xs);
}

.about-label {
  color: var(--text-3);
}

.quick-actions {
  display: flex;
  flex-direction: column;
  gap: var(--space-2);
}

.update-actions {
  margin-top: var(--space-3);
}

.skip-row {
  margin-top: var(--space-2);
}
</style>
