<script setup lang="ts">
import { onMounted, reactive, ref } from 'vue'
import {
  NCard,
  NForm,
  NFormItem,
  NInput,
  NInputNumber,
  NSwitch,
  NButton,
  NSpace,
  NDivider,
  NAlert,
  NSelect,
  NRadioGroup,
  NRadioButton,
  NGrid,
  NGridItem,
  NTabs,
  NTabPane,
  NTag,
  useMessage,
} from 'naive-ui'
import { api } from '@/api'
import { useAppStore } from '@/stores/app'
import type { Settings } from '@/types'
import { PROMPT_STRATEGY_LABELS } from '@/types'

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
    if (!form.proxy) form.proxy = { enabled: false, type: 'http', url: '', username: '', password: '', noProxy: [] }
    noProxyText.value = (form.proxy.noProxy ?? []).join('\n')
    keyCleared.value = false
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    loading.value = false
  }
}

onMounted(load)

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
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2>设置</h2>
        <p class="sub">全局配置。改动监听地址与端口需要重启程序才会生效。</p>
      </div>
      <NSpace>
        <NButton :loading="loading" @click="load">重新载入</NButton>
        <NButton type="primary" :loading="saving" @click="save">保存</NButton>
      </NSpace>
    </div>

    <NTabs type="line" animated>
      <NTabPane name="service" tab="服务">
        <NGrid :cols="24" :x-gap="12" :y-gap="12">
          <NGridItem :span="14">
            <NCard size="small" title="监听与访问" :bordered="false" class="card">
              <NForm label-placement="left" label-width="150" size="small">
                <NFormItem label="监听地址">
                  <NInput v-model:value="form.host" placeholder="127.0.0.1" style="width: 240px" />
                  <span class="muted">改成 0.0.0.0 可以给同网段其它设备用</span>
                </NFormItem>
                <NFormItem label="监听端口">
                  <NInputNumber v-model:value="form.port" :min="1" :max="65535" style="width: 240px" />
                </NFormItem>
                <NFormItem label="访问密钥">
                  <NSpace align="center">
                    <NInput
                      v-model:value="form.accessKey"
                      type="password"
                      show-password-on="click"
                      placeholder="留空表示不校验"
                      style="width: 240px"
                    />
                    <NButton v-if="store.settings?.accessKey" size="small" @click="clearAccessKey">
                      清除密钥
                    </NButton>
                  </NSpace>
                </NFormItem>
              </NForm>
              <NAlert
                v-if="form.host !== '127.0.0.1' && form.host !== 'localhost' && !form.accessKey"
                type="warning"
                :bordered="false"
                style="margin-top: 8px; font-size: 12px"
              >
                监听非本机地址但没有设置访问密钥：同网段的任何人都能使用这个代理、查看全部请求日志。
                建议设置一个密钥。
              </NAlert>
            </NCard>

            <NCard size="small" title="客户端接入" :bordered="false" class="card" style="margin-top: 12px">
              <div class="hint">
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
              <NDivider style="margin: 12px 0" />
              <div class="hint">
                <div class="hint-row">
                  <span class="hint-label">示例</span>
                  <code class="mono">OPENAI_BASE_URL={{ currentBase() }}/v1</code>
                </div>
                <div class="hint-row">
                  <span class="hint-label"></span>
                  <code class="mono">ANTHROPIC_BASE_URL={{ currentBase() }}</code>
                </div>
              </div>
            </NCard>
          </NGridItem>

          <NGridItem :span="10">
            <NCard size="small" title="默认超时" :bordered="false" class="card">
              <NForm label-placement="left" label-width="150" size="small">
                <NFormItem label="上游总超时（秒）">
                  <NInputNumber v-model:value="form.defaultTimeoutSeconds" :min="0" :max="86400" style="width: 200px" />
                </NFormItem>
                <NFormItem label="连接超时（秒）">
                  <NInputNumber v-model:value="form.defaultConnectTimeoutSeconds" :min="1" :max="600" style="width: 200px" />
                </NFormItem>
              </NForm>
              <NAlert type="info" :bordered="false" style="font-size: 12px">
                AI 请求的生成耗时跨度很大，总超时默认给了 10 分钟。填 0 表示不限制。
                单个供应商可以在自己的配置里覆盖这两项。
              </NAlert>
            </NCard>

            <NCard size="small" title="日志保留" :bordered="false" class="card" style="margin-top: 12px">
              <NForm label-placement="left" label-width="150" size="small">
                <NFormItem label="存储请求响应体">
                  <NSwitch v-model:value="form.storeBodies" />
                </NFormItem>
                <NFormItem label="单条报文上限">
                  <NInputNumber
                    v-model:value="form.maxBodyBytes"
                    :min="1024"
                    :max="10485760"
                    :step="1024"
                    :disabled="!form.storeBodies"
                    style="width: 200px"
                  />
                  <span class="muted">字节</span>
                </NFormItem>
                <NFormItem label="保留时长">
                  <NSelect v-model:value="form.retentionDays" :options="retentionOptions" style="width: 200px" />
                </NFormItem>
                <NFormItem label="最多保留条数">
                  <NInputNumber v-model:value="form.maxLogs" :min="0" :max="10000000" :step="10000" style="width: 200px" />
                  <span class="muted">0 = 不限制</span>
                </NFormItem>
              </NForm>
              <NAlert type="info" :bordered="false" style="font-size: 12px">
                后台每小时清理一次。关掉报文存储后仍会记录全部统计指标，只是详情页看不到请求与响应内容。
              </NAlert>
            </NCard>
          </NGridItem>
        </NGrid>
      </NTabPane>

      <NTabPane name="proxy" tab="全局代理（VPN）">
        <NCard size="small" :bordered="false" class="card">
          <NForm label-placement="left" label-width="150" size="small">
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
              <NInput
                v-model:value="form.proxy.url"
                :placeholder="form.proxy.type === 'socks5' ? '127.0.0.1:1080' : '127.0.0.1:7890'"
                style="width: 320px"
              />
              <span class="muted">host:port，带不带 http:// 都可以</span>
            </NFormItem>
            <NFormItem label="用户名">
              <NInput v-model:value="form.proxy.username" placeholder="可留空" style="width: 320px" />
            </NFormItem>
            <NFormItem label="密码">
              <NInput
                v-model:value="form.proxy.password"
                type="password"
                show-password-on="click"
                placeholder="可留空"
                style="width: 320px"
              />
            </NFormItem>
            <NFormItem label="不走代理的域名">
              <NInput
                v-model:value="noProxyText"
                type="textarea"
                :rows="4"
                placeholder="每行一个，例如&#10;internal.corp&#10;localhost"
                style="width: 420px"
              />
            </NFormItem>
          </NForm>
          <NAlert type="info" :bordered="false" style="font-size: 12px">
            全局代理对所有「代理模式 = 继承全局」的供应商生效。
            需要单独设置或完全直连的供应商，可在其编辑页的「网络代理」里覆盖。
            保存后连接缓存会立即失效并重建。
          </NAlert>
        </NCard>
      </NTabPane>

      <NTabPane name="prompt" tab="全局提示词">
        <NCard size="small" :bordered="false" class="card">
          <NForm label-placement="left" label-width="150" size="small">
            <NFormItem label="启用">
              <NSwitch v-model:value="form.globalPrompt.enabled" />
            </NFormItem>
            <NFormItem label="注入方式">
              <NSelect
                v-model:value="form.globalPrompt.strategy"
                :options="strategyOptions"
                style="width: 300px"
                :disabled="!form.globalPrompt.enabled"
              />
            </NFormItem>
            <NFormItem label="提示词内容">
              <NInput
                v-model:value="form.globalPrompt.text"
                type="textarea"
                :rows="8"
                style="width: 620px"
                :disabled="!form.globalPrompt.enabled"
                placeholder="例如：始终使用简体中文回答，代码示例保留英文注释。"
              />
            </NFormItem>
          </NForm>

          <NDivider style="margin: 8px 0 12px" />
          <div class="hint">
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
        </NCard>

        <NCard size="small" title="统计口径" :bordered="false" class="card" style="margin-top: 12px">
          <NForm label-placement="left" label-width="150" size="small">
            <NFormItem label="注入 include_usage">
              <NSwitch v-model:value="form.usageInjectDefault" />
            </NFormItem>
          </NForm>
          <NAlert type="info" :bordered="false" style="font-size: 12px">
            对 OpenAI Chat Completions 格式的流式请求注入
            <code class="mono">stream_options: {"include_usage": true}</code>，
            让上游在流末尾返回精确的 token 用量。
            关闭后，流式请求的 token 数只能用本地规则估算（日志里会标注「估算」）。
            单个供应商可以覆盖这一项。
          </NAlert>
        </NCard>
      </NTabPane>
    </NTabs>
  </div>
</template>

<style scoped>
.page {
  max-width: 1400px;
  margin: 0 auto;
}

.page-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 14px;
}

h2 {
  margin: 0 0 4px;
  font-size: 20px;
  font-weight: 600;
}

.sub {
  margin: 0;
  font-size: 12px;
  opacity: 0.6;
}

.card {
  border-radius: 12px;
  box-shadow: 0 1px 2px rgba(16, 24, 40, 0.04), 0 4px 16px rgba(16, 24, 40, 0.06);
}

.muted {
  font-size: 12px;
  opacity: 0.55;
  margin-left: 8px;
}

.hint {
  font-size: 12px;
  line-height: 1.9;
}

.hint-row {
  display: flex;
  align-items: center;
  gap: 10px;
  flex-wrap: wrap;
}

.hint-label {
  width: 84px;
  opacity: 0.55;
  flex-shrink: 0;
}
</style>
