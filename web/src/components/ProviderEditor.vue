<script setup lang="ts">
import { computed, h, onBeforeUnmount, reactive, ref, watch } from 'vue'
import {
  NDrawer,
  NDrawerContent,
  NTabs,
  NTabPane,
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NSwitch,
  NSelect,
  NButton,
  NSpace,
  NDivider,
  NAlert,
  NTag,
  NRadioGroup,
  NRadioButton,
  NList,
  NListItem,
  NThing,
  NPopconfirm,
  useMessage,
  useDialog,
} from 'naive-ui'
import { api } from '@/api'
import { useAppStore } from '@/stores/app'
import ProviderUsagePanel from '@/components/ProviderUsagePanel.vue'
import HelpTip from '@/components/HelpTip.vue'
import type { APIFormat, CustomUsageMapping, ProbeResult, Provider, ProviderPreset } from '@/types'
import { API_FORMAT_LABELS, PROMPT_STRATEGY_LABELS } from '@/types'
import { formatDuration } from '@/utils/format'
import { hostOf, pendingPresets } from '@/utils/preset'

const props = defineProps<{
  show: boolean
  provider: Provider | null
}>()

const emit = defineEmits<{
  'update:show': [boolean]
  saved: []
}>()

/** 抽屉宽度跟随窗口：固定 720px 在窄窗口下会顶满甚至溢出。 */
const drawerWidth = computed(() => 'min(780px, 94vw)')

const store = useAppStore()
const message = useMessage()
const dialog = useDialog()

/** 空的自定义用量映射。后端用指针表示「未配置」，这里用全空字符串表示。 */
function emptyCustomUsage(): CustomUsageMapping {
  return {
    promptTokens: '',
    cachedTokens: '',
    cacheWriteTokens: '',
    completionTokens: '',
    reasoningTokens: '',
    totalTokens: '',
    model: '',
  }
}

/** 新建时的空白模板。 */
function blank(): Provider {
  return {
    id: 0,
    name: '',
    displayName: '',
    remark: '',
    enabled: true,
    active: false,
    sortOrder: 0,
    baseUrl: '',
    apiFormat: 'openai',
    customPath: '',
    keys: [],
    authHeader: 'Authorization',
    authPrefix: 'Bearer ',
    extraHeaders: {},
    timeoutSeconds: 0,
    connectTimeoutSeconds: 0,
    insecureSkipTls: false,
    proxyMode: 'inherit',
    proxy: { enabled: false, type: 'http', url: '', username: '', password: '', noProxy: [] },
    promptMode: 'inherit',
    prompt: { enabled: true, text: '', strategy: 'append' },
    promptRules: [],
    modelMap: [],
    continuationMode: 'off',
    continuationModels: [],
    continuationCooldownSeconds: 3600,
    continuationInvalidCooldownSeconds: 300,
    usageInjectMode: 'inherit',
    stripUsageChunk: false,
    stripUnsupportedParams: false,
    replayReasoningAsThinkChunk: false,
    promptCacheKeyMode: 'off',
    maxImagesPerRequest: 0,
    imageTrimBatch: 0,
    customUsage: null,
    usageQuery: { template: '', baseUrl: '', autoRefreshSeconds: 0, account: '', password: '' },
    tags: [],
    createdAt: '',
    updatedAt: '',
  }
}

const form = reactive<Provider>(blank())
const saving = ref(false)
const testing = ref(false)
const probeModel = ref('')
const probeResult = ref<ProbeResult | null>(null)
const fetchedModels = ref<string[]>([])
const mapModels = ref<string[]>([])
const loadingMapModels = ref(false)
const baseURLManuallyEdited = ref(false)
const activeTab = ref('basic')

const isEdit = computed(() => form.id > 0)

// extraHeaders 在后端是 map，编辑时需要可增删的数组形态。
const headerRows = ref<{ name: string; value: string }[]>([])
const noProxyText = ref('')
const tagText = ref('')
const customUsage = ref<CustomUsageMapping>(emptyCustomUsage())

const formatOptions = (Object.keys(API_FORMAT_LABELS) as APIFormat[]).map((f) => ({
  label: API_FORMAT_LABELS[f],
  value: f,
}))

const strategyOptions = Object.entries(PROMPT_STRATEGY_LABELS).map(([value, label]) => ({
  label,
  value,
}))

const proxyTypeOptions = [
  { label: 'HTTP / HTTPS 代理', value: 'http' },
  { label: 'SOCKS5 代理', value: 'socks5' },
]

/** BaseURL 的提示随格式变化，减少填错。 */
const baseURLPlaceholder = computed(() => {
  switch (form.apiFormat) {
    case 'anthropic':
      return 'https://api.anthropic.com'
    case 'gemini':
      return 'https://generativelanguage.googleapis.com'
    case 'openai-responses':
    case 'openai':
      return 'https://api.openai.com/v1'
    default:
      return 'https://your-gateway.example.com/v1'
  }
})

watch(
  () => props.show,
  (show) => {
    if (!show) {
      // 关抽屉时把还没触发的防抖检测撤掉（见 clearPresetTimer 的说明）。
      clearPresetTimer()
      return
    }
    const source = props.provider ? JSON.parse(JSON.stringify(props.provider)) : blank()
    // keyHealth / usage 是服务端的视图数据，不属于可编辑配置：
    // 留在表单里既会随保存被回写，又可能在抽屉打开期间变旧（冷却结束、用量刷新）。
    // 界面上的密钥冷却状态读的是 props.provider，不受这里影响。
    delete source.keyHealth
    delete source.usage
    Object.assign(form, blank(), source)
    if (!form.proxy) form.proxy = blank().proxy
    headerRows.value = Object.entries(form.extraHeaders ?? {}).map(([name, value]) => ({ name, value }))
    noProxyText.value = (form.proxy?.noProxy ?? []).join('\n')
    tagText.value = (form.tags ?? []).join(', ')
    customUsage.value = { ...emptyCustomUsage(), ...(form.customUsage ?? {}) }
    probeResult.value = null
    fetchedModels.value = []
    mapModels.value = []
    baseURLManuallyEdited.value = Boolean(form.baseUrl)
    activeTab.value = 'basic'
    // 打开抽屉时先看看这个地址是不是某个内置预设的厂商；是的话弹窗询问。
    void loadPresets().then(maybeAskPreset)
  },
)

/** 切换 API 格式时，如果认证头还是上一个格式的默认值，就跟着换成新格式的默认值。 */
function onFormatChange(next: APIFormat) {
  const defaults = store.meta?.authDefaults ?? []
  const isOldDefault = defaults.some(
    (d) => d.header === form.authHeader && d.prefix === form.authPrefix,
  )
  const target = defaults.find((d) => d.format === next)
  if (target && (isOldDefault || !form.authHeader)) {
    form.authHeader = target.header
    form.authPrefix = target.prefix
  }
}

function addKey() {
  form.keys.push({ id: `k${Date.now()}${Math.random().toString(36).slice(2, 6)}`, key: '', remark: '', enabled: true })
}

function removeKey(idx: number) {
  form.keys.splice(idx, 1)
}

/* ---------- 厂商优化预设 ---------- */

// 预设表来自后端（GET /api/provider-presets），前端只负责「识别 + 询问 + 套用」，
// 避免同一份推荐配置在前后端各写一遍、慢慢漂移。
const presets = ref<ProviderPreset[]>([])
let presetsLoaded = false

/** 当前 BaseURL 命中的、且表单还没完全符合的预设 —— 弹窗与提示条都看这个。 */
const pending = computed(() => pendingPresets(presets.value, form))

/** 把预设写进表单；只动预设声明过的字段。 */
function applyPreset(preset: ProviderPreset) {
  if (preset.stripUnsupportedParams !== undefined) form.stripUnsupportedParams = preset.stripUnsupportedParams
  if (preset.replayReasoningAsThinkChunk !== undefined) {
    form.replayReasoningAsThinkChunk = preset.replayReasoningAsThinkChunk
  }
  if (preset.promptCacheKeyMode !== undefined) form.promptCacheKeyMode = preset.promptCacheKeyMode
  if (preset.usageInjectMode !== undefined) form.usageInjectMode = preset.usageInjectMode
  if (preset.continuationMode !== undefined) form.continuationMode = preset.continuationMode
  if (preset.timeoutSeconds !== undefined) form.timeoutSeconds = preset.timeoutSeconds
  if (preset.maxImagesPerRequest !== undefined) form.maxImagesPerRequest = preset.maxImagesPerRequest
  if (preset.imageTrimBatch !== undefined) form.imageTrimBatch = preset.imageTrimBatch
  dismissPreset(preset)
  message.success('已应用优化配置，保存后生效')
}

// 「暂不」按 预设 + 主机名 记住，避免同一个厂商反复弹窗。
const dismissKey = (preset: ProviderPreset, host: string) => `provider-preset-dismissed:${preset.id}:${host}`

function dismissPreset(preset: ProviderPreset) {
  const host = hostOf(form.baseUrl)
  if (host) localStorage.setItem(dismissKey(preset, host), '1')
}

function isDismissed(preset: ProviderPreset): boolean {
  const host = hostOf(form.baseUrl)
  return Boolean(host) && localStorage.getItem(dismissKey(preset, host)) === '1'
}

/** 确保预设表已加载（只拉一次）。 */
async function loadPresets() {
  if (presetsLoaded) return
  try {
    presets.value = await api.listProviderPresets()
    presetsLoaded = true
  } catch {
    // 拉不到就不提示，编辑功能本身不受影响。
  }
}

/**
 * 手动询问：用户点了「应用推荐配置」/提示条里的按钮。
 *
 * 与自动检测的区别：不看「暂不」记录（用户主动点的，再拦一次等于点了没反应），
 * 也不因为「已经全部符合」而跳过 —— 调用方已经明确指定了要应用哪个预设。
 */
function askPreset(preset: ProviderPreset | undefined) {
  // 模板绑定的是 pending[0]；点击瞬间 pending 可能刚好变空，别让它抛异常。
  if (!preset) return
  const dialogRef = dialog.info({
    title: `应用 ${preset.title} 的推荐配置`,
    content: () =>
      h('div', { style: 'font-size: 13px; line-height: 1.7' }, [
        h('p', { style: 'margin: 0 0 8px' }, preset.summary),
        h(
          'ul',
          { style: 'margin: 0; padding-left: 18px' },
          preset.changes.map((c) =>
            h('li', {}, [
              h('strong', {}, `${c.field}：${c.value}`),
              h('span', { style: 'color: #888' }, ` —— ${c.reason}`),
            ]),
          ),
        ),
      ]),
    positiveText: '应用优化配置',
    negativeText: '取消',
    onPositiveClick: () => {
      applyPreset(preset)
      dialogRef.destroy()
    },
    onNegativeClick: () => {
      dialogRef.destroy()
    },
  })
}

/**
 * 自动询问：打开抽屉、BaseURL 改动（防抖）时触发。
 *
 * 已经全部符合、或用户点过「暂不」的不再打扰 —— 自动弹窗一旦烦人，
 * 用户就会条件反射地关掉，真正需要处理时反而不看了。
 */
function maybeAskPreset() {
  // 预设表是异步拉的：请求回来时抽屉可能已经关了、或已经换到另一个供应商。
  // 没有这个短路，弹窗会在抽屉外冒出来，还可能对着新表单弹旧厂商的建议。
  if (!props.show) return
  const host = hostOf(form.baseUrl)
  if (!host) return
  const preset = pending.value.find((p) => !isDismissed(p))
  if (!preset) return

  const dialogRef = dialog.info({
    title: `检测到 ${preset.title}`,
    content: () =>
      h('div', { style: 'font-size: 13px; line-height: 1.7' }, [
        h('p', { style: 'margin: 0 0 8px' }, preset.summary),
        h(
          'ul',
          { style: 'margin: 0; padding-left: 18px' },
          preset.changes.map((c) =>
            h('li', {}, [
              h('strong', {}, `${c.field}：${c.value}`),
              h('span', { style: 'color: #888' }, ` —— ${c.reason}`),
            ]),
          ),
        ),
      ]),
    positiveText: '应用优化配置',
    negativeText: '暂不',
    onPositiveClick: () => {
      applyPreset(preset)
      dialogRef.destroy()
    },
    onNegativeClick: () => {
      dismissPreset(preset)
      dialogRef.destroy()
    },
  })
}

/** BaseURL 改动后的防抖检测：别在用户每敲一个字时弹窗。 */
let presetTimer: number | undefined

function clearPresetTimer() {
  if (presetTimer !== undefined) {
    window.clearTimeout(presetTimer)
    presetTimer = undefined
  }
}

function schedulePresetCheck() {
  clearPresetTimer()
  presetTimer = window.setTimeout(() => {
    presetTimer = undefined
    void loadPresets().then(maybeAskPreset)
  }, 600)
}

// 抽屉关闭、组件卸载时都要摘掉定时器：否则关掉抽屉 600ms 后
// 还会弹出一个「检测到某厂商」的对话框，此时用户已经不在这个表单里了。
onBeforeUnmount(clearPresetTimer)


/** 提交给后端的供应商结构：时间戳由服务端管理，不在其中。 */
type ProviderPayload = Omit<Provider, 'createdAt' | 'updatedAt'>

/** 把界面上的临时结构写回 Provider 对象。 */
function collect(): ProviderPayload {
  const extraHeaders: Record<string, string> = {}
  for (const row of headerRows.value) {
    if (row.name.trim()) extraHeaders[row.name.trim()] = row.value
  }
  // createdAt / updatedAt 是服务端管理的只读字段，一律不提交：
  // 新建时表单里是空串，后端解析 time.Time 会直接 400。
  // 后端现在也做了容错，但客户端本来就该只发自己能决定的东西。
  //
  // keyHealth / usage 同理：它们是服务端算出来的视图数据（密钥冷却状态、用量快照），
  // 只供界面展示。跟着表单一起提交，等于把可能已经过期的快照写回后端。
  const {
    createdAt: _createdAt,
    updatedAt: _updatedAt,
    keyHealth: _keyHealth,
    usage: _usage,
    ...rest
  } = form
  void _createdAt
  void _updatedAt
  void _keyHealth
  void _usage
  return {
    ...rest,
    // 预设接口会把这一项返回成 true，payload 里缺字段后端就按零值写回 false，
    // 等于每次编辑都悄悄关掉预设。显式带上（布尔字段不必区分「没传」）。
    replayReasoningAsThinkChunk: form.replayReasoningAsThinkChunk ?? false,
    continuationCooldownSeconds: form.continuationCooldownSeconds ?? 3600,
    continuationInvalidCooldownSeconds: form.continuationInvalidCooldownSeconds ?? 300,
    // NInputNumber 清空后值是 null，直接提交会让后端收到 null。Go 侧会当零值处理
    // （0 = 不限制 / 逐张，语义上正好），但前端类型声明是 number，null 会让
    // presetApplied 把「已应用」误判成「未应用」。这里统一收敛成 0。
    maxImagesPerRequest: form.maxImagesPerRequest ?? 0,
    imageTrimBatch: form.imageTrimBatch ?? 0,
    extraHeaders,
    // 只提交填写完整的映射行：空行（用户点了「添加」还没填）不该落库。
    // 下拉框 cleanable 清空后 from/to 会是 null，直接 .trim() 会抛异常把整个保存卡死。
    modelMap: form.modelMap
      .map((m) => ({ from: (m.from ?? '').trim(), to: (m.to ?? '').trim() }))
      .filter((m) => m.from && m.to),
    tags: tagText.value
      .split(',')
      .map((t) => t.trim())
      .filter(Boolean),
    // 只有 custom 格式才需要这份映射，其它格式存空值，避免留下无用配置。
    customUsage: form.apiFormat === 'custom' ? { ...customUsage.value } : null,
    proxy: {
      ...form.proxy,
      noProxy: noProxyText.value
        .split(/[\n,]/)
        .map((t) => t.trim())
        .filter(Boolean),
    },
  }
}

async function save() {
  if (!form.name.trim()) {
    message.warning('请填写供应商短名')
    return
  }
  if (!form.baseUrl.trim()) {
    message.warning('请填写 BaseURL')
    return
  }
  saving.value = true
  try {
    const payload = collect()
    if (isEdit.value) {
      await api.updateProvider(payload)
      message.success('已保存')
    } else {
      await api.createProvider(payload)
      message.success('已创建')
    }
    await store.reloadProviders()
    emit('saved')
    emit('update:show', false)
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    saving.value = false
  }
}

/** 测试连通性。带上传当前未保存的配置，省去「先存再测」的来回。 */
async function test() {
  if (!isEdit.value) {
    message.info('请先保存供应商后再测试')
    return
  }
  testing.value = true
  probeResult.value = null
  try {
    probeResult.value = await api.testProvider(form.id, collect(), probeModel.value)
    if (probeResult.value.models?.length) fetchedModels.value = probeResult.value.models
  } catch (e) {
    probeResult.value = {
      ok: false,
      status: 0,
      latencyMs: 0,
      message: e instanceof Error ? e.message : String(e),
      usedUrl: '',
      models: null,
      authOk: false,
    }
  } finally {
    testing.value = false
  }
}

async function fetchModels() {
  // 新建态还没 id，接口无从寻址；先把「填好 BaseURL 并保存」这件事说清楚，
  // 而不是静默返回让按钮看起来没反应。
  if (!isEdit.value) {
    message.warning('请先填好 BaseURL 并保存供应商，再拉取模型列表')
    return
  }
  // 后端只在 BaseURL 非空时才用表单配置，空着会静默回退到已保存的地址 ——
  // 用户会拿到一份旧地址的模型列表，还以为新地址通了。
  if (!form.baseUrl.trim()) {
    message.warning('请先填写 BaseURL，再拉取模型列表')
    return
  }
  testing.value = true
  try {
    // 带上当前未保存的表单配置：改完 BaseURL/密钥可以直接拉，
    // 不用为了看一眼模型列表先保存一轮。
    const res = await api.fetchProviderModels(form.id, collect())
    fetchedModels.value = res.models ?? []
    message.success(`拉取到 ${fetchedModels.value.length} 个模型`)
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    testing.value = false
  }
}

// ---------- 模型映射 ----------

/** 模型映射的下拉候选项：上游拉到的模型 + 该供应商历史日志里出现过的模型。 */
const mapModelOptions = computed(() => {
  const set = new Set<string>([...mapModels.value, ...fetchedModels.value])
  for (const m of form.modelMap) {
    if (m.from) set.add(m.from)
    if (m.to) set.add(m.to)
  }
  return [...set].sort().map((m) => ({ label: m, value: m }))
})

function addMapping() {
  form.modelMap.push({ from: '', to: '' })
}

function removeMapping(idx: number) {
  form.modelMap.splice(idx, 1)
}

/**
 * 拉取候选项：上游模型列表 + 该供应商用过的模型名。
 *
 * 两个来源都要：上游列表是「可以映射到什么」，历史模型是「这个供应商实际在用哪些名字」。
 * 带上尚未保存的配置，改完 BaseURL 不用先存就能拉。
 */
async function loadMapModels() {
  if (!isEdit.value) {
    message.info('请先保存供应商后再拉取模型列表')
    return
  }
  loadingMapModels.value = true
  try {
    const [upstream, used] = await Promise.all([
      api.fetchProviderModels(form.id, collect()).catch(() => ({ models: [] as string[] })),
      api.distinctModels(form.id).catch(() => [] as string[]),
    ])
    mapModels.value = [...new Set([...(upstream.models ?? []), ...used])].sort()
    if (mapModels.value.length) {
      message.success(`已加载 ${mapModels.value.length} 个候选模型`)
    } else {
      message.warning('没有取到候选模型，可直接手动输入')
    }
  } finally {
    loadingMapModels.value = false
  }
}

/** 用量查询模板下拉的选项。模板由后端注册表提供，加模板不用改前端。 */
const templateOptions = computed(() => [
  { label: '不使用', value: '' },
  ...(store.meta?.usageTemplates ?? []).map((t) => ({
    label: t.name,
    value: t.id,
    description: t.description,
  })),
])

/** 当前选中模板的说明。放进问号气泡，避免在窄抽屉里挤占下拉框。 */
const usageTemplateDesc = computed(
  () => (store.meta?.usageTemplates ?? []).find((t) => t.id === form.usageQuery.template)?.description ?? '',
)

/**
 * 定时刷新的间隔。默认关闭 —— 一次查询要打四个上游额度接口，
 * 让人显式打开比默认轮询更合适。
 */
const autoRefreshOptions = [
  { label: '关闭', value: 0 },
  { label: '每 1 分钟', value: 60 },
  { label: '每 5 分钟', value: 300 },
  { label: '每 15 分钟', value: 900 },
  { label: '每 30 分钟', value: 1800 },
]

const usageSnapshot = computed(() =>
  store.providers.find((p) => p.id === form.id)?.usage ?? null,
)
const usageLoading = computed(() => Boolean(store.usageRefreshing[form.id]))
const usageConfigured = computed(() => Boolean(form.usageQuery?.template))

/** 让服务端立刻查一次。带上当前未保存的配置，改完模板不用先存再查。 */
async function queryUsage() {
  if (!isEdit.value) {
    message.info('请先保存供应商后再查询用量')
    return
  }
  if (!usageConfigured.value) {
    message.info('请先选择用量查询模板')
    return
  }
  await store.refreshUsage(form.id, collect())
}

/** 切到「用量」页时，如果服务端还没查到过，就让后端补一次。 */
watch(activeTab, (tab) => {
  if (tab !== 'usage') return
  if (!isEdit.value || !usageConfigured.value) return
  if (usageLoading.value || usageSnapshot.value) return
  void queryUsage()
})

/**
 * 删除供应商。
 *
 * 模板上已经有 NPopconfirm 的「删除后无法恢复，确定要删除…」确认，
 * 这里再弹一次 dialog.warning 等于让用户连着点两次确认 —— 而且两次文案不同，
 * 反倒让人怀疑点错了什么。
 */
async function remove() {
  try {
    await api.deleteProvider(form.id)
    message.success('已删除')
    await store.reloadProviders()
    emit('saved')
    emit('update:show', false)
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  }
}
</script>

<template>
  <NDrawer
    :show="show"
    :width="drawerWidth"
    placement="right"
    @update:show="emit('update:show', $event)"
  >
    <NDrawerContent :title="isEdit ? `编辑供应商 · ${form.displayName || form.name}` : '新增供应商'" closable>
      <NTabs v-model:value="activeTab" type="line" animated>
        <NTabPane name="basic" tab="基础">
          <NForm label-placement="left" label-width="120" size="small">
            <NFormItem label="短名" required>
              <NInput v-model:value="form.name" placeholder="用于 /p/<短名>/ 路由，建议只用字母数字和连字符" />
            </NFormItem>
            <NFormItem label="展示名">
              <NInput v-model:value="form.displayName" placeholder="留空则与短名相同" />
            </NFormItem>
            <NFormItem label="备注">
              <NInput v-model:value="form.remark" type="textarea" :rows="2" placeholder="例如：主力账号、限速说明" />
            </NFormItem>
            <NFormItem label="启用">
              <NSwitch v-model:value="form.enabled" />
            </NFormItem>
            <NFormItem label="API 格式">
              <NSelect
                v-model:value="form.apiFormat"
                :options="formatOptions"
                @update:value="onFormatChange"
              />
            </NFormItem>
            <NFormItem label="BaseURL" required>
              <NInput
                v-model:value="form.baseUrl"
                :placeholder="baseURLPlaceholder"
                @update:value="baseURLManuallyEdited = true; schedulePresetCheck()"
              />
              <HelpTip :width="420">
                <div>
                  请求路径怎么拼？客户端请求
                  <code class="mono">{{
                    form.apiFormat === 'anthropic'
                      ? '/v1/messages'
                      : form.apiFormat === 'gemini'
                        ? '/v1beta/models/X:generateContent'
                        : '/v1/chat/completions'
                  }}</code>
                  。
                </div>
                <div v-if="form.apiFormat === 'gemini'">
                  Gemini 的认证头默认是 <code class="mono">x-goog-api-key</code>，密钥直接填 key
                  值即可（不需要 Bearer 前缀）。
                </div>
                <div v-else-if="form.apiFormat === 'anthropic'">
                  Anthropic 的认证头默认是 <code class="mono">x-api-key</code>，前缀留空。
                </div>
                <div v-else>BaseURL 填不填 <code class="mono">/v1</code> 都可以，重复的路径段会自动去重。</div>
              </HelpTip>
            </NFormItem>
            <NFormItem v-if="pending.length" label="优化配置">
              <NButton type="primary" secondary size="small" @click="askPreset(pending[0])">
                应用「{{ pending[0].title }}」推荐配置
              </NButton>
            </NFormItem>
            <NFormItem label="标签">
              <NInput v-model:value="tagText" placeholder="逗号分隔，例如：主力, 便宜" />
            </NFormItem>
          </NForm>
          <NAlert
            v-if="pending.length"
            type="info"
            :bordered="false"
            style="margin-bottom: 12px; font-size: 12px"
          >
            检测到 <strong>{{ pending[0].title }}</strong>：{{ pending[0].summary }}
            点上面的「优化配置」可以一次套用推荐的 {{ pending[0].changes.length }} 项配置（保存后生效）。
          </NAlert>
        </NTabPane>

        <NTabPane name="mapping" tab="模型映射">
          <NSpace align="center" style="margin-bottom: 10px">
            <NButton size="small" :loading="loadingMapModels" @click="loadMapModels">
              拉取候选模型
            </NButton>
            <HelpTip
              text="客户端请求的模型名与左侧完全相等时，实际发给上游的模型名换成右侧。只做精确匹配，不做前缀/通配；没有命中的请求原样透传。留空即不使用。候选 = 上游模型列表 + 该供应商历史请求用过的模型名。"
            />
          </NSpace>

          <div v-for="(m, idx) in form.modelMap" :key="idx" class="map-row">
            <NSelect
              v-model:value="m.from"
              class="map-select"
              :options="mapModelOptions"
              :consistent-menu-width="false"
              :menu-props="{ style: { minWidth: '340px' } }"
              filterable
              tag
              clearable
              placeholder="请求模型（可下拉选或直接输入）"
            />
            <span class="map-arrow">→</span>
            <NSelect
              v-model:value="m.to"
              class="map-select"
              :options="mapModelOptions"
              :consistent-menu-width="false"
              :menu-props="{ style: { minWidth: '340px' } }"
              filterable
              tag
              clearable
              placeholder="目标模型"
            />
            <NButton size="small" quaternary type="error" @click="removeMapping(idx)">删除</NButton>
          </div>
          <NButton size="small" dashed block style="margin-top: 6px" @click="addMapping">
            + 添加映射
          </NButton>
        </NTabPane>

        <NTabPane name="auth" tab="认证">
          <NForm label-placement="left" label-width="120" size="small">
            <NFormItem label="认证头名">
              <NInput v-model:value="form.authHeader" placeholder="Authorization / x-api-key / x-goog-api-key" />
            </NFormItem>
            <NFormItem label="取值前缀">
              <NInput v-model:value="form.authPrefix" placeholder="例如 Bearer （含末尾空格）；不需要前缀就留空" />
            </NFormItem>
          </NForm>

          <NDivider title-placement="left" style="font-size: 12px">密钥（可配置多个，按顺序轮询）</NDivider>
          <NAlert v-if="!form.keys.length" type="warning" :bordered="false" style="margin-bottom: 8px; font-size: 12px">
            没有配置密钥时，客户端自带的认证信息会原样转发给上游 —— 适合「客户端持钥、代理只记账」的用法。
          </NAlert>

          <div v-for="(k, idx) in form.keys" :key="k.id" class="key-row">
            <NSpace align="center" style="width: 100%">
              <NSwitch v-model:value="k.enabled" size="small" />
              <NInput
                v-model:value="k.key"
                type="password"
                show-password-on="click"
                placeholder="密钥"
                style="flex: 1"
              />
              <NInput v-model:value="k.remark" placeholder="备注" style="width: 130px" />
              <NButton size="small" quaternary type="error" @click="removeKey(idx)">删除</NButton>
            </NSpace>
            <div v-if="provider?.keyHealth?.[k.id]?.cooling" class="key-health">
              <NTag size="tiny" type="warning" :bordered="false">
                冷却中 {{ Math.ceil((provider.keyHealth[k.id].cooldownMs || 0) / 1000) }}s
              </NTag>
              <span class="muted">{{ provider.keyHealth[k.id].lastError }}</span>
            </div>
          </div>
          <NButton size="small" dashed block style="margin-top: 4px" @click="addKey">+ 添加密钥</NButton>

          <NDivider title-placement="left" style="font-size: 12px">额外请求头</NDivider>
          <div v-for="(row, idx) in headerRows" :key="idx" style="margin-bottom: 8px">
            <NSpace align="center">
              <NInput v-model:value="row.name" placeholder="头名" style="width: 220px" />
              <NInput v-model:value="row.value" placeholder="值" style="width: 300px" />
              <NButton size="small" quaternary type="error" @click="headerRows.splice(idx, 1)">删除</NButton>
            </NSpace>
          </div>
          <NButton size="small" dashed block @click="headerRows.push({ name: '', value: '' })">
            + 添加请求头
          </NButton>
        </NTabPane>

        <NTabPane name="network" tab="网络代理">
          <NForm label-placement="left" label-width="140" size="small">
            <NFormItem label="总超时（秒）">
              <NInputNumber v-model:value="form.timeoutSeconds" :min="0" :max="86400" style="width: 180px" />
              <HelpTip text="0 = 继承全局；全局为 0 表示不限制" />
            </NFormItem>
            <NFormItem label="连接超时（秒）">
              <NInputNumber v-model:value="form.connectTimeoutSeconds" :min="0" :max="600" style="width: 180px" />
              <HelpTip text="0 = 继承全局" />
            </NFormItem>
            <NFormItem label="跳过 TLS 校验">
              <NSwitch v-model:value="form.insecureSkipTls" />
              <HelpTip text="仅用于自建网关的自签名证书" />
            </NFormItem>
            <NFormItem label="代理模式">
              <NRadioGroup v-model:value="form.proxyMode">
                <NRadioButton value="inherit">继承全局</NRadioButton>
                <NRadioButton value="custom">自定义</NRadioButton>
                <NRadioButton value="direct">直连</NRadioButton>
              </NRadioGroup>
            </NFormItem>
          </NForm>

          <template v-if="form.proxyMode === 'custom'">
            <NDivider title-placement="left" style="font-size: 12px">本供应商专用代理</NDivider>
            <NForm label-placement="left" label-width="140" size="small">
              <NFormItem label="启用">
                <NSwitch v-model:value="form.proxy.enabled" />
              </NFormItem>
              <NFormItem label="类型">
                <NSelect v-model:value="form.proxy.type" :options="proxyTypeOptions" style="width: 220px" />
              </NFormItem>
              <NFormItem label="地址">
                <NInput v-model:value="form.proxy.url" placeholder="127.0.0.1:7890" />
              </NFormItem>
              <NFormItem label="用户名">
                <NInput v-model:value="form.proxy.username" placeholder="可留空" />
              </NFormItem>
              <NFormItem label="密码">
                <NInput v-model:value="form.proxy.password" type="password" show-password-on="click" placeholder="可留空" />
              </NFormItem>
              <NFormItem label="不走代理">
                <NInput
                  v-model:value="noProxyText"
                  type="textarea"
                  :rows="2"
                  placeholder="每行一个域名，例如 internal.corp（含子域）"
                />
              </NFormItem>
            </NForm>
          </template>

          <NDivider title-placement="left" style="font-size: 12px">连通性测试</NDivider>
          <NSpace align="center" style="margin-bottom: 10px">
            <NInput
              v-model:value="probeModel"
              placeholder="模型名（可选，用于上游没有 /models 接口时兜底）"
              style="width: 320px"
            />
            <NButton size="small" :loading="testing" type="primary" @click="test">测试连接</NButton>
            <NButton size="small" :loading="testing" @click="fetchModels">拉取模型列表</NButton>
          </NSpace>

          <NAlert
            v-if="probeResult"
            :type="probeResult.ok ? 'success' : probeResult.authOk === false ? 'error' : 'warning'"
            :bordered="false"
          >
            <div style="font-size: 12px; line-height: 1.7">
              <div>
                <b>{{ probeResult.ok ? '连接正常' : '连接失败' }}</b>
                <span v-if="probeResult.status"> · HTTP {{ probeResult.status }}</span>
                <span v-if="probeResult.latencyMs"> · 耗时 {{ formatDuration(probeResult.latencyMs) }}</span>
              </div>
              <div>{{ probeResult.message }}</div>
              <div v-if="probeResult.usedUrl" class="mono" style="opacity: 0.7">{{ probeResult.usedUrl }}</div>
            </div>
          </NAlert>

          <NList v-if="fetchedModels.length" size="small" :bordered="false" style="margin-top: 10px">
            <NListItem v-for="m in fetchedModels.slice(0, 50)" :key="m">
              <NThing :title="m" />
            </NListItem>
          </NList>
        </NTabPane>

        <NTabPane name="prompt" tab="提示词">
          <NForm label-placement="left" label-width="140" size="small">
            <NFormItem label="提示词模式">
              <NRadioGroup v-model:value="form.promptMode">
                <NRadioButton value="inherit">继承全局</NRadioButton>
                <NRadioButton value="custom">自定义</NRadioButton>
                <NRadioButton value="disable">禁用</NRadioButton>
              </NRadioGroup>
            </NFormItem>
          </NForm>

          <template v-if="form.promptMode === 'custom'">
            <NForm label-placement="left" label-width="140" size="small">
              <NFormItem label="启用">
                <NSwitch v-model:value="form.prompt.enabled" />
              </NFormItem>
              <NFormItem label="注入方式">
                <NSelect v-model:value="form.prompt.strategy" :options="strategyOptions" style="width: 260px" />
              </NFormItem>
              <NFormItem label="提示词内容">
                <NInput
                  v-model:value="form.prompt.text"
                  type="textarea"
                  :rows="6"
                  placeholder="例如：始终使用简体中文回答。"
                />
              </NFormItem>
            </NForm>
          </template>

          <NDivider title-placement="left" style="font-size: 12px">
            按模型规则（优先级高于上面的配置）
            <HelpTip :width="360">
              命中第一条匹配的规则。模型名用正则匹配，例如 <code class="mono">^gpt-4</code> 或
              <code class="mono">claude</code>；留空表示匹配全部模型。
            </HelpTip>
          </NDivider>

          <div v-for="(rule, idx) in form.promptRules" :key="rule.id" class="rule-row">
            <NSpace align="center" style="margin-bottom: 6px">
              <NSwitch v-model:value="rule.enabled" size="small" />
              <NInput v-model:value="rule.modelPattern" placeholder="模型正则，留空匹配全部" style="width: 240px" />
              <NSelect v-model:value="rule.strategy" :options="strategyOptions" style="width: 220px" />
              <NButton size="small" quaternary type="error" @click="form.promptRules.splice(idx, 1)">
                删除
              </NButton>
            </NSpace>
            <NInput v-model:value="rule.text" type="textarea" :rows="3" placeholder="该模型使用的提示词" />
          </div>
          <NButton
            size="small"
            dashed
            block
            style="margin-top: 6px"
            @click="
              form.promptRules.push({
                id: `r${Date.now()}`,
                enabled: true,
                modelPattern: '',
                text: '',
                strategy: 'append',
              })
            "
          >
            + 添加模型规则
          </NButton>
        </NTabPane>

        <NTabPane name="advanced" tab="高级">
          <NForm label-placement="left" label-width="150" size="small">
            <NFormItem label="注入 include_usage">
              <NRadioGroup v-model:value="form.usageInjectMode">
                <NRadioButton value="inherit">继承全局</NRadioButton>
                <NRadioButton value="on">强制开启</NRadioButton>
                <NRadioButton value="off">关闭</NRadioButton>
              </NRadioGroup>
              <HelpTip :width="420">
                仅对 OpenAI Chat Completions 格式的<strong>流式</strong>请求生效。
                上游默认不会在流式响应里返回 token 用量，注入
                <code class="mono">stream_options.include_usage</code> 后就能拿到精确值；
                不注入则统计会退化为本地估算。这属于对请求体的改写，会在日志里留痕。
              </HelpTip>
            </NFormItem>
          </NForm>

          <NForm label-placement="left" label-width="150" size="small">
            <NFormItem label="剥离 usage 块">
              <NSwitch v-model:value="form.stripUsageChunk" />
            </NFormItem>
          </NForm>
          <NAlert v-if="form.stripUsageChunk" type="warning" :bordered="false" style="margin-bottom: 12px; font-size: 12px">
            开启后，上游返回的「只带 usage、没有内容」的收尾块不会转发给客户端。
            极少数严格校验 <code class="mono">choices</code> 为空的客户端需要它。
            统计不受影响。
          </NAlert>

          <NDivider title-placement="left" style="font-size: 12px">严格上游兼容</NDivider>
          <NForm label-placement="left" label-width="150" size="small">
            <NFormItem label="剔除非标准字段">
              <NSwitch v-model:value="form.stripUnsupportedParams" />
              <HelpTip :width="480">
                开启后，OpenAI / Responses 请求体顶层的
                <code class="mono">thinking</code>、<code class="mono">enable_thinking</code>、
                <code class="mono">thinking_budget</code> 会在转发前移除；
                OpenAI Chat 格式还会额外移除 <code class="mono">store</code>，
                Responses 格式在<strong>续传关闭时</strong>也会移除
                <code class="mono">store</code> / <code class="mono">previous_response_id</code>
                （无状态上游引用不到上一轮响应，带着只会 400）。
                消息级还会移除 <code class="mono">reasoning_content</code>、
                <code class="mono">reasoning_details</code>、<code class="mono">reasoning</code>、
                <code class="mono">refusal</code>、<code class="mono">annotations</code>、
                <code class="mono">audio</code>、旧 <code class="mono">function_call</code>，
                以及非 <code class="mono">tool</code> 消息上的 <code class="mono">name</code>
                —— 这些都是 Mistral 这类「多一个字段就 422」schema 不认的。
                <br />
                Responses 格式还会裁掉<strong>上游不执行的工具</strong>：
                <code class="mono">web_search</code>、<code class="mono">file_search</code>、
                <code class="mono">code_interpreter</code>、<code class="mono">image_generation</code>、
                <code class="mono">mcp</code>、<code class="mono">custom</code> 等内置工具整项移除
                （<code class="mono">namespace</code> 分组只删内层的这类工具，删空则整组去掉），
                函数工具一个不动；工具被裁到没有时 <code class="mono">tool_choice:"required"</code>
                回落 <code class="mono">auto</code>。同时移除
                <code class="mono">web_search_call</code>、<code class="mono">item_reference</code>
                这类回放项，以及 reasoning 项里有值的
                <code class="mono">encrypted_content</code>（明文思维链保留）。
                <br />
                还有三个「字段合法、取值被钉死」的 Responses 顶层字段也会按值修正：
                <code class="mono">truncation</code> 只留 <code class="mono">"disabled"</code>、
                <code class="mono">background</code> 只留 <code class="mono">false</code>、
                <code class="mono">metadata</code> 只留字符串值（删空则整项移除）。
                Codex 默认就带前两个，不开这个开关会每一轮都 400。
                如果上游本来就认这些字段（部分国产网关），别开，否则推理会被悄悄关掉。
                另外 <code class="mono">role:"developer"</code> 会无条件归一化成
                <code class="mono">system</code>，不受此开关影响。
              </HelpTip>
            </NFormItem>
            <NFormItem label="思维链还原（ThinkChunk）">
              <NSwitch v-model:value="form.replayReasoningAsThinkChunk" />
              <HelpTip>
                把历史 assistant.reasoning_content 还原成 Mistral 原生 thinking 内容块，
                保持多步工具调用的思维链连续。只对支持该内容块的上游开启。
              </HelpTip>
            </NFormItem>
            <NFormItem label="注入 prompt_cache_key">
              <NRadioGroup v-model:value="form.promptCacheKeyMode">
                <NRadioButton value="off">关闭</NRadioButton>
                <NRadioButton value="auto">自动派生</NRadioButton>
              </NRadioGroup>
              <HelpTip :width="480">
                <code class="mono">prompt_cache_key</code> 是上游的<strong>缓存亲和提示</strong>
                （Mistral 官方文档）：同一会话用稳定 key 能提高前缀缓存命中率，缓存输入按标准价的 10% 计费，
                但不保证命中。选「自动派生」后，代理优先用客户端会话头
                （<code class="mono">X-Session-Id</code> 等），没有就用「首条消息 + tools」的指纹，
                哈希成 <code class="mono">aipx-…</code> 再注入；客户端自己带了 key 时不覆盖。
                不认这个字段的严格网关会 422，所以默认关闭。
                <br />
                只对<strong>生成类端点</strong>注入（<code class="mono">/chat/completions</code>、
                <code class="mono">/fim/completions</code>、<code class="mono">/agents/completions</code>、
                <code class="mono">/responses</code>）。embeddings、moderations、ocr、classifications
                的 schema 里没有这个字段，带上就是 422，所以代理按路径判定，认不出路径时不注入。
              </HelpTip>
            </NFormItem>
            <NFormItem label="单请求图片上限">
              <NInputNumber
                v-model:value="form.maxImagesPerRequest"
                :min="0"
                :max="10000"
                placeholder="0 = 不限制"
                style="width: 180px"
              />
              <span class="field-hint">张</span>
              <HelpTip :width="440">
                <strong>单请求图片上限</strong>：上游按「整个请求里的图片总数」硬性拒绝，
                Mistral 超过就整条 400（<code class="mono">Total number of images exceeds
                the maximum allowed of 20.</code>）。而客户端每一轮都把历史消息连同其中的图片
                原样回放，图片数量随会话自然增长、代理管不着 —— 攒够上限之后这个会话就再也
                发不出去。
                <br />
                填上这里的上限后，超限时<strong>更早的图片</strong>会被就地替换成一行占位文本
                （数组长度与位置不变），会话继续可用；模型看不到那几张图，但知道自己漏了东西。
                日志里记 <code class="mono">image_limit_trim</code>，详情页能看到裁了几张。
                填 0 表示不限制。
              </HelpTip>
            </NFormItem>
            <NFormItem label="成批替换">
              <NInputNumber
                v-model:value="form.imageTrimBatch"
                :min="0"
                :max="1000"
                placeholder="0 = 逐张"
                :disabled="!form.maxImagesPerRequest"
                style="width: 180px"
              />
              <span class="field-hint">张一批</span>
              <HelpTip text="0/1 表示逐张。逐张推进时「第一张保留下来的图」的位置每轮前移，前缀缓存会从那里断开；按 5 张一批推进能让多图会话在 21~25 张这段窗口里发出同一份 body。" />
            </NFormItem>
          </NForm>

          <NDivider title-placement="left" style="font-size: 12px">Responses 会话续传</NDivider>
          <NForm label-placement="left" label-width="150" size="small">
            <NFormItem label="续传模式">
              <NRadioGroup v-model:value="form.continuationMode">
                <NRadioButton value="off">关闭</NRadioButton>
                <NRadioButton value="dry-run">仅诊断</NRadioButton>
                <NRadioButton value="on">开启改写</NRadioButton>
              </NRadioGroup>
            </NFormItem>
            <NFormItem v-if="form.continuationMode !== 'off'" label="模型范围">
              <NSelect
                v-model:value="form.continuationModels"
                multiple
                tag
                filterable
                clearable
                :options="[]"
                placeholder="留空=全部模型；输入模型名后回车可添加"
              />
            </NFormItem>
            <NFormItem v-if="form.continuationMode !== 'off'" label="不支持类冷却">
              <NInputNumber v-model:value="form.continuationCooldownSeconds" :min="0" :step="60" style="width: 160px">
                <template #suffix>秒</template>
              </NInputNumber>
            </NFormItem>
            <NFormItem v-if="form.continuationMode !== 'off'" label="id 失效冷却">
              <NInputNumber
                v-model:value="form.continuationInvalidCooldownSeconds"
                :min="0"
                :step="30"
                style="width: 160px"
              >
                <template #suffix>秒</template>
              </NInputNumber>
            </NFormItem>
          </NForm>

          <NAlert
            v-if="form.continuationMode === 'dry-run'"
            type="info"
            :bordered="false"
            style="margin-bottom: 12px; font-size: 12px"
          >
            只统计「本地历史能否接上上一轮的链」和预计可节省的请求字节，<strong>不修改请求、不开启 store</strong>。
            先让真实会话跑一段时间，再看日志里的「续传」列：<code class="mono">dry-hit</code> 比例高才值得开启改写。
            如果一直是 <code class="mono">dry-miss</code>，通常是客户端每轮重新编码了图片，或历史被改写、分支换了线。
          </NAlert>

          <NAlert
            v-else-if="form.continuationMode === 'on'"
            type="warning"
            :bordered="false"
            style="margin-bottom: 12px; font-size: 12px"
          >
            仅对 <code class="mono">openai-responses</code> 格式、路径以 <code class="mono">/responses</code>
            结尾的请求生效。命中时把全量 <code class="mono">input</code> 截成增量，并补
            <code class="mono">store:true</code> + <code class="mono">previous_response_id</code>。
            <br />
            <strong>后果：</strong>上游会保存整段会话（含图片）；只有原生 Responses 且真正实现
            <code class="mono">previous_response_id</code> 的模型有效。<code class="mono">flash</code> 一类模型会返回 503
            并自动回退；<code class="mono">deepseek-v4-pro</code> 这类模型可能返回 200
            但<strong>静默丢失上下文</strong>。请先用「仅诊断」在真实会话上验证。
            <br />
            两个冷却时间填 0 表示<strong>不冷却</strong>：回退后下一轮立刻重新建链，适合测试；
            但上游持续失败时每轮都会多一次请求。
          </NAlert>

          <NForm label-placement="left" label-width="150" size="small">
            <NFormItem label="自定义上游路径">
              <NInput v-model:value="form.customPath" placeholder="例如 /v1/chat/completions" />
              <HelpTip :width="460">
                留空时按「客户端请求路径」自动拼接（重复的路径段会去重，BaseURL 填不填
                <code class="mono">/v1</code> 都行）。填写后改用这里的内容，但<strong>仍会保留 BaseURL 自带的前缀</strong>
                —— 例如 BaseURL 为 <code class="mono">https://gw.example.com/api/v1</code>、此处填
                <code class="mono">/chat</code>，最终请求 <code class="mono">/api/v1/chat</code>。
                需要完全覆盖含前缀的整条路径时，把它写进 BaseURL。
              </HelpTip>
            </NFormItem>
          </NForm>

          <template v-if="form.apiFormat === 'custom'">
            <NDivider title-placement="left" style="font-size: 12px">
              自定义用量字段路径
              <HelpTip :width="360">
                用点号表示嵌套，例如 <code class="mono">meta.usage.in</code>；数组可以用
                <code class="mono">*</code> 求和。这些路径只在「读取用量」时使用，不改变转发的数据。
              </HelpTip>
            </NDivider>
            <NForm label-placement="left" label-width="150" size="small">
              <NFormItem label="输入 tokens">
                <NInput v-model:value="customUsage.promptTokens" placeholder="usage.prompt_tokens" />
              </NFormItem>
              <NFormItem label="缓存命中 tokens">
                <NInput v-model:value="customUsage.cachedTokens" placeholder="usage.cached_tokens" />
              </NFormItem>
              <NFormItem label="缓存写入 tokens">
                <NInput v-model:value="customUsage.cacheWriteTokens" placeholder="usage.cache_write_tokens" />
              </NFormItem>
              <NFormItem label="输出 tokens">
                <NInput v-model:value="customUsage.completionTokens" placeholder="usage.completion_tokens" />
              </NFormItem>
              <NFormItem label="推理 tokens">
                <NInput v-model:value="customUsage.reasoningTokens" placeholder="usage.reasoning_tokens" />
              </NFormItem>
              <NFormItem label="总计 tokens">
                <NInput v-model:value="customUsage.totalTokens" placeholder="usage.total_tokens" />
              </NFormItem>
              <NFormItem label="模型名">
                <NInput v-model:value="customUsage.model" placeholder="model" />
              </NFormItem>
            </NForm>
          </template>
        </NTabPane>
        <NTabPane name="usage" tab="用量">
          <NForm label-placement="left" label-width="120" size="small">
            <NFormItem label="查询模板">
              <NSelect
                v-model:value="form.usageQuery.template"
                :options="templateOptions"
                style="width: 260px"
              />
              <HelpTip v-if="usageTemplateDesc" :text="usageTemplateDesc" />
            </NFormItem>
            <NFormItem label="查询地址">
              <NInput
                v-model:value="form.usageQuery.baseUrl"
                placeholder="留空则自动从 BaseURL 推导"
              />
            </NFormItem>
            <template v-if="form.usageQuery.template === 'mistral'">
              <NFormItem label="账号（可选）">
                <NInput
                  v-model:value="form.usageQuery.account"
                  placeholder="admin.mistral.ai 的登录邮箱"
                />
              </NFormItem>
              <NFormItem label="密码（可选）">
                <NInput
                  v-model:value="form.usageQuery.password"
                  type="password"
                  show-password-on="click"
                  placeholder="该账号的登录密码"
                />
                <HelpTip :width="420">
                  预算与订阅计划走官方 API，用供应商密钥就能读，<strong>这两项不填也能用</strong>。
                  填了才能额外读到本月 token 明细与限额状态 —— 那部分还挂在网页后台，
                  程序会用这里的账号密码自动登录 admin.mistral.ai 并缓存会话，
                  过期后自动重登，无需手工维护 Cookie。密码与 API 密钥同等对待，明文存在本地数据库里。
                </HelpTip>
              </NFormItem>
            </template>
            <NFormItem label="定时刷新">
              <NSelect
                v-model:value="form.usageQuery.autoRefreshSeconds"
                :options="autoRefreshOptions"
                style="width: 160px"
              />
              <HelpTip text="服务端按此间隔查询并存入数据库，界面打开时直接读结果" />
            </NFormItem>
          </NForm>

          <NDivider title-placement="left" style="font-size: 12px">当前结果</NDivider>
          <NAlert
            v-if="!isEdit"
            type="info"
            :bordered="false"
            style="font-size: 12px; margin-bottom: 8px"
          >
            供应商还没保存，保存后即可查询。
          </NAlert>
          <ProviderUsagePanel
            :snapshot="usageSnapshot"
            :loading="usageLoading"
            :configured="usageConfigured"
            @query="queryUsage"
          />
        </NTabPane>
      </NTabs>

      <template #footer>
        <NSpace justify="space-between" style="width: 100%" align="center">
          <NPopconfirm v-if="isEdit" @positive-click="remove">
            <template #trigger>
              <NButton size="small" type="error" quaternary>删除供应商</NButton>
            </template>
            删除后无法恢复，确定要删除「{{ form.displayName || form.name }}」吗？
          </NPopconfirm>
          <span v-else />
          <NSpace>
            <NButton @click="emit('update:show', false)">取消</NButton>
            <NButton type="primary" :loading="saving" @click="save">保存</NButton>
          </NSpace>
        </NSpace>
      </template>
    </NDrawerContent>
  </NDrawer>
</template>

<style scoped>
.key-row {
  margin-bottom: 8px;
}

.key-health {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-top: 4px;
  font-size: 11px;
}

.rule-row {
  border: 1px solid rgba(128, 128, 128, 0.2);
  border-radius: 8px;
  padding: 10px;
  margin-bottom: 8px;
}

/* 用普通 flex 行而不是 NSpace：NSpace 会给每个子项包一层不带 flex 的 div，
   里面 NSelect 的 flex:1 撑不开，下拉框会缩到只够显示 placeholder 的宽度。 */
.map-row {
  display: flex;
  align-items: center;
  gap: 8px;
  margin-bottom: 8px;
}

/* 两个下拉框等分整行宽度，长模型名才显示得下。 */
.map-select {
  flex: 1;
  min-width: 0;
}

.map-arrow {
  opacity: 0.5;
}

.muted {
  opacity: 0.6;
  font-size: 12px;
}
</style>
