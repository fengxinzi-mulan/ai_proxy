<script setup lang="ts">
import { computed, reactive, ref, watch } from 'vue'
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
import type { APIFormat, CustomUsageMapping, ProbeResult, Provider } from '@/types'
import { API_FORMAT_LABELS, PROMPT_STRATEGY_LABELS } from '@/types'
import { formatDuration } from '@/utils/format'

const props = defineProps<{
  show: boolean
  provider: Provider | null
}>()

const emit = defineEmits<{
  'update:show': [boolean]
  saved: []
}>()

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
    usageInjectMode: 'inherit',
    stripUsageChunk: false,
    customUsage: null,
    usageQuery: { template: '', baseUrl: '', autoRefreshSeconds: 0 },
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
    if (!show) return
    const source = props.provider ? JSON.parse(JSON.stringify(props.provider)) : blank()
    Object.assign(form, blank(), source)
    if (!form.proxy) form.proxy = blank().proxy
    headerRows.value = Object.entries(form.extraHeaders ?? {}).map(([name, value]) => ({ name, value }))
    noProxyText.value = (form.proxy?.noProxy ?? []).join('\n')
    tagText.value = (form.tags ?? []).join(', ')
    customUsage.value = { ...emptyCustomUsage(), ...(form.customUsage ?? {}) }
    probeResult.value = null
    fetchedModels.value = []
    baseURLManuallyEdited.value = Boolean(form.baseUrl)
    activeTab.value = 'basic'
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
  const { createdAt: _createdAt, updatedAt: _updatedAt, ...rest } = form
  void _createdAt
  void _updatedAt
  return {
    ...rest,
    extraHeaders,
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
  if (!isEdit.value) return
  testing.value = true
  try {
    const res = await api.fetchProviderModels(form.id)
    fetchedModels.value = res.models ?? []
    message.success(`拉取到 ${fetchedModels.value.length} 个模型`)
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    testing.value = false
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

/** 删除是不可逆操作，走一次确认对话框。 */
function remove() {
  dialog.warning({
    title: '删除供应商',
    content: `确定删除「${form.displayName || form.name}」吗？该操作无法撤销。`,
    positiveText: '删除',
    negativeText: '取消',
    onPositiveClick: async () => {
      try {
        await api.deleteProvider(form.id)
        message.success('已删除')
        await store.reloadProviders()
        emit('saved')
        emit('update:show', false)
      } catch (e) {
        message.error(e instanceof Error ? e.message : String(e))
      }
    },
  })
}
</script>

<template>
  <NDrawer
    :show="show"
    :width="720"
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
                @update:value="baseURLManuallyEdited = true"
              />
            </NFormItem>
            <NFormItem label="标签">
              <NInput v-model:value="tagText" placeholder="逗号分隔，例如：主力, 便宜" />
            </NFormItem>
          </NForm>

          <NAlert type="info" :bordered="false" style="margin-top: 8px">
            <div style="font-size: 12px; line-height: 1.7">
              <div><b>请求路径怎么拼？</b>客户端请求 <code class="mono">{{ form.apiFormat === 'anthropic' ? '/v1/messages' : form.apiFormat === 'gemini' ? '/v1beta/models/X:generateContent' : '/v1/chat/completions' }}</code>。</div>
              <div v-if="form.apiFormat === 'gemini'">
                Gemini 的认证头默认是 <code class="mono">x-goog-api-key</code>，密钥直接填 key 值即可（不需要 Bearer 前缀）。
              </div>
              <div v-else-if="form.apiFormat === 'anthropic'">
                Anthropic 的认证头默认是 <code class="mono">x-api-key</code>，前缀留空。
              </div>
              <div v-else>
                BaseURL 填不填 <code class="mono">/v1</code> 都可以，重复的路径段会自动去重。
              </div>
            </div>
          </NAlert>
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
              <span class="muted" style="margin-left: 8px">0 = 继承全局；全局为 0 表示不限制</span>
            </NFormItem>
            <NFormItem label="连接超时（秒）">
              <NInputNumber v-model:value="form.connectTimeoutSeconds" :min="0" :max="600" style="width: 180px" />
              <span class="muted" style="margin-left: 8px">0 = 继承全局</span>
            </NFormItem>
            <NFormItem label="跳过 TLS 校验">
              <NSwitch v-model:value="form.insecureSkipTls" />
              <span class="muted" style="margin-left: 8px">仅用于自建网关的自签名证书</span>
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
          </NDivider>
          <NAlert type="info" :bordered="false" style="margin-bottom: 10px; font-size: 12px">
            命中第一条匹配的规则。模型名用正则匹配，例如 <code class="mono">^gpt-4</code> 或
            <code class="mono">claude</code>；留空表示匹配全部模型。
          </NAlert>

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
            </NFormItem>
          </NForm>

          <NAlert type="info" :bordered="false" style="margin-bottom: 12px; font-size: 12px">
            仅对 OpenAI Chat Completions 格式的<strong>流式</strong>请求生效。
            上游默认不会在流式响应里返回 token 用量，注入
            <code class="mono">stream_options.include_usage</code> 后就能拿到精确值；
            不注入则统计会退化为本地估算。这属于对请求体的改写，会在日志里留痕。
          </NAlert>

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

          <NForm label-placement="left" label-width="150" size="small">
            <NFormItem label="自定义上游路径">
              <NInput v-model:value="form.customPath" placeholder="例如 /v1/chat/completions" />
            </NFormItem>
          </NForm>
          <NAlert type="info" :bordered="false" style="margin-bottom: 12px; font-size: 12px">
            留空时按「客户端请求路径」自动拼接（重复的路径段会去重，BaseURL 填不填
            <code class="mono">/v1</code> 都行）。填写后改用这里的内容，但<strong>仍会保留 BaseURL 自带的前缀</strong>
            —— 例如 BaseURL 为 <code class="mono">https://gw.example.com/api/v1</code>、此处填
            <code class="mono">/chat</code>，最终请求 <code class="mono">/api/v1/chat</code>。
            需要完全覆盖含前缀的整条路径时，把它写进 BaseURL。
          </NAlert>

          <template v-if="form.apiFormat === 'custom'">
            <NDivider title-placement="left" style="font-size: 12px">自定义用量字段路径</NDivider>
            <NAlert type="info" :bordered="false" style="margin-bottom: 10px; font-size: 12px">
              用点号表示嵌套，例如 <code class="mono">meta.usage.in</code>；数组可以用
              <code class="mono">*</code> 求和。这些路径只在「读取用量」时使用，不改变转发的数据。
            </NAlert>
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
              <span class="muted" style="margin-left: 8px">
                {{ (store.meta?.usageTemplates ?? []).find((t) => t.id === form.usageQuery.template)?.description ?? '' }}
              </span>
            </NFormItem>
            <NFormItem label="查询地址">
              <NInput
                v-model:value="form.usageQuery.baseUrl"
                placeholder="留空则自动从 BaseURL 推导"
              />
            </NFormItem>
            <NFormItem label="定时刷新">
              <NSelect
                v-model:value="form.usageQuery.autoRefreshSeconds"
                :options="autoRefreshOptions"
                style="width: 160px"
              />
              <span class="muted" style="margin-left: 8px">
                服务端按此间隔查询并存入数据库，界面打开时直接读结果
              </span>
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

.muted {
  opacity: 0.6;
  font-size: 12px;
}
</style>
