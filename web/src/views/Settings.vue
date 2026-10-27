<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
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
import { FolderOpen, RefreshCw, Save, FileText } from 'lucide-vue-next'
import PageHeader from '@/components/PageHeader.vue'
import AppCard from '@/components/AppCard.vue'
import { api } from '@/api'
import { useAppStore } from '@/stores/app'
import type { Settings } from '@/types'
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
            <AppCard title="默认超时">
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
</style>
