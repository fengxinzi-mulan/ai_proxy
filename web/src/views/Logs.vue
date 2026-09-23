<script setup lang="ts">
import { computed, h, onMounted, onBeforeUnmount, ref } from 'vue'
import {
  NCard,
  NDataTable,
  NInput,
  NSelect,
  NButton,
  NSpace,
  NTag,
  NTooltip,
  NPopconfirm,
  NPagination,
  NSwitch,
  useMessage,
} from 'naive-ui'
import type { DataTableColumns, PaginationProps } from 'naive-ui'
import LogDetail from '@/components/LogDetail.vue'
import { api } from '@/api'
import { useAppStore } from '@/stores/app'
import { useEvents } from '@/composables/useEvents'
import type { RequestLog } from '@/types'
import {
  OUTCOME_LABELS,
  cacheHitRateOf,
  formatDuration,
  formatNumber,
  formatPercent,
  formatStamp,
  formatTps,
  logOutcome,
  modificationLabel,
} from '@/utils/format'

const store = useAppStore()
const message = useMessage()

const RANGES = [
  { label: '最近 1 小时', value: '1h' },
  { label: '最近 6 小时', value: '6h' },
  { label: '最近 24 小时', value: '24h' },
  { label: '最近 7 天', value: '7d' },
  { label: '最近 30 天', value: '30d' },
  { label: '全部', value: 'all' },
]

const STATUS = [
  { label: '全部', value: '' },
  { label: '仅成功', value: 'success' },
  { label: '仅失败', value: 'failed' },
  { label: '仅已取消', value: 'cancelled' },
]

// 0 表示不按供应商筛选（NSelect 的 value 用同一种类型更好处理）。
const ALL_PROVIDERS = 0

const rows = ref<RequestLog[]>([])
const total = ref(0)
const page = ref(1)
const pageSize = ref(50)
const loading = ref(false)
const live = ref(true)

const filter = ref({ range: '24h', providerId: ALL_PROVIDERS, model: '', status: '', q: '' })
const models = ref<string[]>([])

const detailShow = ref(false)
const detailId = ref<number | null>(null)

async function load() {
  loading.value = true
  try {
    const res = await api.listLogs({
      page: page.value,
      pageSize: pageSize.value,
      range: filter.value.range,
      providerId: filter.value.providerId || undefined,
      model: filter.value.model || undefined,
      status: filter.value.status || undefined,
      q: filter.value.q || undefined,
    })
    rows.value = res.items ?? []
    total.value = res.total
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    loading.value = false
  }
}

async function loadModels() {
  try {
    models.value = await api.distinctModels()
  } catch {
    /* 模型列表只是筛选项，失败不影响主流程 */
  }
}

onMounted(async () => {
  if (!store.providers.length) await store.reloadProviders()
  await Promise.all([load(), loadModels()])
})

function applyFilter() {
  page.value = 1
  void load()
}

function resetFilter() {
  filter.value = { range: '24h', providerId: ALL_PROVIDERS, model: '', status: '', q: '' }
  applyFilter()
}

const pagination = computed<PaginationProps>(() => ({
  page: page.value,
  pageSize: pageSize.value,
  itemCount: total.value,
  showSizePicker: true,
  pageSizes: [20, 50, 100, 200],
  prefix: ({ itemCount }) => `共 ${formatNumber(itemCount)} 条`,
  onChange: (p: number) => {
    page.value = p
    void load()
  },
  onUpdatePageSize: (s: number) => {
    pageSize.value = s
    page.value = 1
    void load()
  },
}))

// 实时模式下新日志直接插到最前面，省去手动刷新。
// 只在第一页且无额外筛选时才插入，避免打乱已筛选的结果。
const canStream = computed(
  () => live.value && page.value === 1 && !filter.value.q && !filter.value.model && !filter.value.status,
)

useEvents({
  onLog: (entry) => {
    if (!canStream.value) return
    if (filter.value.providerId && entry.providerId !== filter.value.providerId) return
    rows.value = [entry, ...rows.value].slice(0, pageSize.value)
    total.value += 1
  },
})

let refreshTimer: number | undefined
// 实时插入会让「当前页」与后端的偏移量错位，定期整体刷新一次纠正。
onMounted(() => {
  refreshTimer = window.setInterval(() => {
    if (canStream.value && rows.value.length) void load()
  }, 30_000)
})
onBeforeUnmount(() => {
  if (refreshTimer) window.clearInterval(refreshTimer)
})

const providerOptions = computed(() => [
  { label: '全部供应商', value: ALL_PROVIDERS },
  ...store.providers.map((p) => ({ label: p.displayName || p.name, value: p.id })),
])

const modelOptions = computed(() => [
  { label: '全部模型', value: '' },
  ...models.value.map((m) => ({ label: m, value: m })),
])

function openDetail(row: RequestLog) {
  detailId.value = row.id
  detailShow.value = true
}

async function clearAll() {
  try {
    const res = await api.clearLogs()
    message.success(`已清空 ${res.deleted} 条记录`)
    page.value = 1
    await load()
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  }
}

const columns = computed<DataTableColumns<RequestLog>>(() => [
  {
    title: '时间',
    key: 'tsStart',
    width: 158,
    fixed: 'left',
    render: (r) => h('span', { class: 'num' }, formatStamp(r.tsStart)),
  },
  {
    title: '供应商',
    key: 'providerName',
    width: 118,
    ellipsis: { tooltip: true },
    render: (r) => r.providerName || '—',
  },
  {
    title: '模型',
    key: 'model',
    width: 256,
    ellipsis: { tooltip: true },
    render: (r) =>
      h('span', { class: 'mono' }, r.model || r.modelResponse || '—'),
  },
  {
    title: '状态',
    key: 'status',
    width: 90,
    render: (r) => {
      const o = logOutcome(r)
      const type =
        o === 'success' ? 'success' : o === 'cancelled' ? 'warning' : 'error'
      const label = `${OUTCOME_LABELS[o]}${r.httpStatus ? ` ${r.httpStatus}` : ''}`
      return r.errorMsg
        ? h(NTooltip, null, {
            trigger: () => h(NTag, { size: 'small', type, bordered: false }, { default: () => label }),
            default: () => r.errorMsg,
          })
        : h(NTag, { size: 'small', type, bordered: false }, { default: () => label })
    },
  },
  {
    title: '首 token',
    key: 'ttftMs',
    width: 80,
    align: 'right',
    render: (r) => h('span', { class: 'num' }, r.ttftMs ? formatDuration(r.ttftMs) : '—'),
  },
  {
    title: '总耗时',
    key: 'totalMs',
    width: 80,
    align: 'right',
    render: (r) => h('span', { class: 'num' }, formatDuration(r.totalMs)),
  },
  {
    title: 'TPS',
    key: 'tps',
    width: 58,
    align: 'right',
    render: (r) => h('span', { class: 'num' }, formatTps(r.tps)),
  },
  {
    title: () =>
      h(NTooltip, null, {
        trigger: () => h('span', null, '输入'),
        default: () => '输入 token（含缓存命中与写入）',
      }),
    key: 'promptTokens',
    width: 84,
    align: 'right',
    render: (r) => h('span', { class: 'num' }, formatNumber(r.promptTokens)),
  },
  {
    title: () =>
      h(NTooltip, null, {
        trigger: () => h('span', null, '缓存'),
        default: () => '命中缓存的输入 token 数，括号内为占输入总量的比例',
      }),
    key: 'cachedTokens',
    width: 126,
    align: 'right',
    render: (r) => {
      if (!r.cachedTokens) return h('span', { style: 'opacity:.35' }, '—')
      const rate = cacheHitRateOf(r.cachedTokens, r.promptTokens)
      return h('span', { class: 'num', style: 'color:#18a058' }, [
        formatNumber(r.cachedTokens),
        h('span', { style: 'opacity:.55;font-size:11px' }, ` ${formatPercent(rate)}`),
      ])
    },
  },
  {
    title: '输出',
    key: 'completionTokens',
    width: 84,
    align: 'right',
    render: (r) => h('span', { class: 'num' }, formatNumber(r.completionTokens)),
  },
  {
    title: '推理',
    key: 'reasoningEffort',
    width: 80,
    ellipsis: { tooltip: true },
    render: (r) =>
      r.reasoningEffort
        ? h(NTag, { size: 'tiny', bordered: false }, { default: () => r.reasoningEffort })
        : h('span', { style: 'opacity:.35' }, '—'),
  },
  {
    title: '标记',
    key: 'flags',
    width: 130,
    render: (r) => {
      const tags: ReturnType<typeof h>[] = []
      if (r.stream) tags.push(h(NTag, { size: 'tiny', bordered: false, type: 'info' }, { default: () => '流式' }))
      if (r.tokensEstimated)
        tags.push(h(NTag, { size: 'tiny', bordered: false, type: 'warning' }, { default: () => '估算' }))
      if (r.requestModified)
        tags.push(
          h(
            NTooltip,
            null,
            {
              trigger: () =>
                h(NTag, { size: 'tiny', bordered: false, type: 'warning' }, { default: () => '改写' }),
              default: () => r.modifications.map(modificationLabel).join('、') || '请求体已改写',
            },
          ),
        )
      if (!tags.length) return h('span', { style: 'opacity:.35' }, '—')
      return h(NSpace, { size: 2 }, { default: () => tags })
    },
  },
  {
    title: '',
    key: 'actions',
    width: 56,
    fixed: 'right',
    render: (r) =>
      h(NButton, { size: 'tiny', quaternary: true, onClick: () => openDetail(r) }, { default: () => '详情' }),
  },
])
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2>请求日志</h2>
        <p class="sub">
          每次转发都会记一条。开启实时后会即时插入新记录，每 30 秒与后端校正一次。
        </p>
      </div>
      <NSpace align="center">
        <NSwitch v-model:value="live" size="small">
          <template #checked>实时</template>
          <template #unchecked>实时</template>
        </NSwitch>
        <NButton :loading="loading" @click="load">刷新</NButton>
        <NPopconfirm @positive-click="clearAll">
          <template #trigger>
            <NButton type="error" ghost>清空日志</NButton>
          </template>
          将删除所有历史记录，且无法恢复。确定继续吗？
        </NPopconfirm>
      </NSpace>
    </div>

    <NCard size="small" :bordered="false" class="filter-card">
      <NSpace align="center" :size="10" style="flex-wrap: wrap">
        <NSelect
          v-model:value="filter.range"
          :options="RANGES"
          style="width: 140px"
          @update:value="applyFilter"
        />
        <NSelect
          v-model:value="filter.providerId"
          :options="providerOptions"
          style="width: 170px"
          @update:value="applyFilter"
        />
        <NSelect
          v-model:value="filter.model"
          :options="modelOptions"
          filterable
          style="width: 210px"
          @update:value="applyFilter"
        />
        <NSelect
          v-model:value="filter.status"
          :options="STATUS"
          style="width: 130px"
          @update:value="applyFilter"
        />
        <NInput
          v-model:value="filter.q"
          placeholder="搜索模型 / 路径 / 错误信息"
          style="width: 260px"
          clearable
          @keyup.enter="applyFilter"
        />
        <NButton type="primary" @click="applyFilter">查询</NButton>
        <NButton @click="resetFilter">重置</NButton>
      </NSpace>
    </NCard>

    <NCard size="small" :bordered="false" class="table-card">
      <NDataTable
        :columns="columns"
        :data="rows"
        :loading="loading"
        :row-key="(r: RequestLog) => r.id"
        :scroll-x="1400"
        :bordered="false"
        size="small"
        flex-height
        style="height: calc(100vh - 300px); min-height: 320px"
        :row-props="(r: RequestLog) => ({ style: 'cursor:pointer', onClick: () => openDetail(r) })"
      />
      <div class="pager">
        <NPagination v-bind="pagination" />
      </div>
    </NCard>

    <LogDetail v-model:show="detailShow" :log-id="detailId" />
  </div>
</template>

<style scoped>
.page {
  max-width: 1720px;
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

.filter-card,
.table-card {
  border-radius: 12px;
  box-shadow: 0 1px 2px rgba(16, 24, 40, 0.04), 0 4px 16px rgba(16, 24, 40, 0.06);
}

.filter-card {
  margin-bottom: 12px;
}

.pager {
  display: flex;
  justify-content: flex-end;
  padding-top: 12px;
}
</style>
