<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import {
  NDrawer,
  NDrawerContent,
  NTabs,
  NTabPane,
  NDescriptions,
  NDescriptionsItem,
  NTag,
  NSpin,
  NEmpty,
  NAlert,
  NSpace,
  NButton,
  useMessage,
} from 'naive-ui'
import { api } from '@/api'
import type { RequestLog } from '@/types'
import {
  OUTCOME_LABELS,
  cacheHitRateOf,
  formatBytes,
  formatDateTime,
  formatDuration,
  formatNumber,
  formatPercent,
  formatTps,
  logOutcome,
  modificationLabel,
  prettyJSON,
} from '@/utils/format'

const props = defineProps<{
  show: boolean
  logId: number | null
}>()

const emit = defineEmits<{ 'update:show': [boolean] }>()

const message = useMessage()
const log = ref<RequestLog | null>(null)
const loading = ref(false)
const tab = ref('overview')

// 列表接口不带报文正文，展开详情时再单独拉一次。
watch(
  () => [props.show, props.logId] as const,
  async ([show, id]) => {
    if (!show || !id) return
    loading.value = true
    log.value = null
    tab.value = 'overview'
    try {
      log.value = await api.getLog(id)
    } catch (e) {
      message.error(e instanceof Error ? e.message : String(e))
    } finally {
      loading.value = false
    }
  },
)

const outcome = computed(() => (log.value ? logOutcome(log.value) : 'error'))

/** 该次请求的缓存命中率。输入量为 0 时无意义，调用处会另行处理。 */
const cacheHitRate = computed(() => cacheHitRateOf(log.value?.cachedTokens ?? 0, log.value?.promptTokens ?? 0))

function copy(text: string | undefined) {
  if (!text) return
  navigator.clipboard.writeText(text).then(
    () => message.success('已复制'),
    () => message.error('复制失败'),
  )
}

const prettyReq = computed(() => prettyJSON(log.value?.reqBody))
const prettyReqOriginal = computed(() => prettyJSON(log.value?.reqBodyOriginal))
const prettyResp = computed(() => prettyJSON(log.value?.respBody))
</script>

<template>
  <NDrawer :show="show" :width="820" placement="right" @update:show="emit('update:show', $event)">
    <NDrawerContent :title="log ? `请求详情 #${log.id}` : '请求详情'" closable>
      <NSpin :show="loading">
        <NEmpty v-if="!loading && !log" description="未找到该记录" style="padding: 60px 0" />

        <template v-else-if="log">
          <NAlert
            v-if="log.errorMsg"
            :type="log.cancelled ? 'warning' : 'error'"
            :bordered="false"
            style="margin-bottom: 12px"
          >
            {{ log.errorMsg }}
          </NAlert>

          <NSpace :size="6" style="margin-bottom: 12px">
            <NTag :type="outcome === 'success' ? 'success' : outcome === 'cancelled' ? 'warning' : 'error'" :bordered="false">
              {{ OUTCOME_LABELS[outcome] }}
            </NTag>
            <NTag :bordered="false">HTTP {{ log.httpStatus }}</NTag>
            <NTag v-if="log.stream" :bordered="false" type="info">流式</NTag>
            <NTag v-if="log.reasoningEffort" :bordered="false">推理 {{ log.reasoningEffort }}</NTag>
            <NTag v-if="log.tokensEstimated" :bordered="false" type="warning">token 为估算值</NTag>
            <NTag v-if="log.requestModified" :bordered="false" type="warning">请求已改写</NTag>
            <NTag v-if="log.responseModified" :bordered="false" type="warning">响应已改写</NTag>
          </NSpace>

          <NTabs v-model:value="tab" type="line" animated>
            <NTabPane name="overview" tab="概览">
              <NDescriptions :column="2" label-placement="left" bordered size="small">
                <NDescriptionsItem label="时间">{{ formatDateTime(log.tsStart) }}</NDescriptionsItem>
                <NDescriptionsItem label="客户端">{{ log.clientIp || '—' }}</NDescriptionsItem>
                <NDescriptionsItem label="供应商">
                  {{ log.providerName || '—' }}
                </NDescriptionsItem>
                <NDescriptionsItem label="API 格式">{{ log.apiFormat || '—' }}</NDescriptionsItem>
                <NDescriptionsItem label="请求模型">
                  <span class="mono">{{ log.model || '—' }}</span>
                </NDescriptionsItem>
                <NDescriptionsItem label="响应模型">
                  <span class="mono">{{ log.modelResponse || '—' }}</span>
                </NDescriptionsItem>
                <NDescriptionsItem label="方法 / 路径" :span="2">
                  <span class="mono">{{ log.method }} {{ log.path }}</span>
                </NDescriptionsItem>
                <NDescriptionsItem label="上游地址" :span="2">
                  <span class="mono">{{ log.upstreamUrl || '—' }}</span>
                </NDescriptionsItem>
                <NDescriptionsItem v-if="log.proxyUsed" label="使用代理" :span="2">
                  <span class="mono">{{ log.proxyUsed }}</span>
                </NDescriptionsItem>
              </NDescriptions>

              <h4 class="section">用量</h4>
              <NDescriptions :column="3" label-placement="left" bordered size="small">
                <NDescriptionsItem label="输入 token">
                  {{ formatNumber(log.promptTokens) }}
                </NDescriptionsItem>
                <NDescriptionsItem label="缓存命中">
                  {{ formatNumber(log.cachedTokens) }}
                </NDescriptionsItem>
                <NDescriptionsItem label="缓存写入">
                  {{ formatNumber(log.cacheWriteTokens) }}
                </NDescriptionsItem>
                <NDescriptionsItem label="缓存命中率">
                  <!-- 输入量为 0（例如请求没到上游）时没有可算的比值，显示不适用 -->
                  <span v-if="log.promptTokens">{{ formatPercent(cacheHitRate) }}</span>
                  <span v-else style="opacity: 0.5">不适用</span>
                </NDescriptionsItem>
                <NDescriptionsItem label="输出 token">
                  {{ formatNumber(log.completionTokens) }}
                </NDescriptionsItem>
                <NDescriptionsItem label="推理 token">
                  {{ formatNumber(log.reasoningTokens) }}
                </NDescriptionsItem>
                <NDescriptionsItem label="总计">
                  {{ formatNumber(log.totalTokens) }}
                </NDescriptionsItem>
              </NDescriptions>

              <h4 class="section">性能</h4>
              <NDescriptions :column="3" label-placement="left" bordered size="small">
                <NDescriptionsItem label="首 token">
                  {{ log.ttftMs ? formatDuration(log.ttftMs) : '不适用' }}
                </NDescriptionsItem>
                <NDescriptionsItem label="总耗时">
                  {{ formatDuration(log.totalMs) }}
                </NDescriptionsItem>
                <NDescriptionsItem label="TPS">{{ formatTps(log.tps) }}</NDescriptionsItem>
              </NDescriptions>

              <template v-if="log.modifications?.length">
                <h4 class="section">本次改写</h4>
                <NSpace :size="6">
                  <NTag v-for="m in log.modifications" :key="m" size="small" type="warning" :bordered="false">
                    {{ modificationLabel(m) }}
                  </NTag>
                </NSpace>
              </template>
            </NTabPane>

            <NTabPane name="request" tab="请求">
              <div v-if="log.reqHeaders" class="head-block">
                <div class="block-title">
                  <span>请求头（凭据已脱敏）</span>
                  <NButton size="tiny" quaternary @click="copy(log.reqHeaders)">复制</NButton>
                </div>
                <pre class="code-block">{{ log.reqHeaders }}</pre>
              </div>

              <div v-if="log.requestModified && log.reqBodyOriginal" class="compare">
                <div class="compare-col">
                  <div class="block-title">
                    <span>改写前</span>
                    <NButton size="tiny" quaternary @click="copy(log.reqBodyOriginal)">复制</NButton>
                  </div>
                  <pre class="code-block before">{{ prettyReqOriginal }}</pre>
                </div>
                <div class="compare-col">
                  <div class="block-title">
                    <span>改写后（实际发出）</span>
                    <NButton size="tiny" quaternary @click="copy(log.reqBody)">复制</NButton>
                  </div>
                  <pre class="code-block after">{{ prettyReq }}</pre>
                </div>
              </div>

              <div v-else-if="log.reqBody">
                <div class="block-title">
                  <span>请求体（实际发出）{{ formatBytes(log.reqBody.length) }}</span>
                  <NButton size="tiny" quaternary @click="copy(log.reqBody)">复制</NButton>
                </div>
                <pre class="code-block">{{ prettyReq }}</pre>
              </div>

              <NEmpty v-else description="未记录请求体（可在设置里开启报文存储）" style="padding: 40px 0" />
            </NTabPane>

            <NTabPane name="response" tab="响应">
              <template v-if="log.respBody">
                <div class="block-title">
                  <span>响应体 {{ formatBytes(log.respBody.length) }}</span>
                  <NButton size="tiny" quaternary @click="copy(log.respBody)">复制</NButton>
                </div>
                <pre class="code-block">{{ prettyResp }}</pre>
              </template>
              <NEmpty v-else description="未记录响应体（可在设置里开启报文存储）" style="padding: 40px 0" />
            </NTabPane>
          </NTabs>
        </template>
      </NSpin>
    </NDrawerContent>
  </NDrawer>
</template>

<style scoped>
.section {
  margin: 18px 0 8px;
  font-size: 13px;
  font-weight: 600;
  opacity: 0.8;
}

.block-title {
  display: flex;
  align-items: center;
  justify-content: space-between;
  font-size: 12px;
  opacity: 0.72;
  margin-bottom: 6px;
}

.head-block {
  margin-bottom: 16px;
}

.code-block {
  background: rgba(128, 128, 128, 0.09);
  border-radius: 8px;
  padding: 10px 12px;
  max-height: 460px;
  overflow: auto;
}

.compare {
  display: grid;
  grid-template-columns: 1fr 1fr;
  gap: 12px;
}

.compare-col {
  min-width: 0;
}

.code-block.before {
  border-left: 3px solid #f0a020;
}

.code-block.after {
  border-left: 3px solid #18a058;
}
</style>
