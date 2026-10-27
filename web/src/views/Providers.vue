<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, ref } from 'vue'
import {
  NButton,
  NSpace,
  NTag,
  NSwitch,
  NEmpty,
  NSpin,
  NTooltip,
  NBadge,
  NIcon,
  useMessage,
  NPopconfirm,
} from 'naive-ui'
import { Plus, RefreshCw, TriangleAlert } from 'lucide-vue-next'
import ProviderEditor from '@/components/ProviderEditor.vue'
import ProviderUsagePanel from '@/components/ProviderUsagePanel.vue'
import PageHeader from '@/components/PageHeader.vue'
import { api } from '@/api'
import { useAppStore } from '@/stores/app'
import type { Provider } from '@/types'
import { API_FORMAT_LABELS } from '@/types'
import { formatRelative } from '@/utils/format'

const store = useAppStore()
const message = useMessage()

const editorShow = ref(false)
const editing = ref<Provider | null>(null)
const busy = ref<number | null>(null)

// 自动重拉间隔。用量快照是服务端按各供应商自己的间隔刷的（最短 1 分钟），
// 页面比它密一档地重拉列表，新快照落库后最多 30 秒就会出现在卡片上。
const AUTO_REFRESH_MS = 30_000
let refreshTimer: number | undefined

/**
 * 重拉供应商列表（列表接口自带用量快照）。
 *
 * 页面在后台时不查，切回前台会立刻补一次（visibilitychange 也走这里）——
 * 用户切回来看到的必须是刚取到的数，而不是上次离开时那份。
 */
async function autoRefresh() {
  if (document.visibilityState !== 'visible') return
  try {
    await store.reloadProviders()
  } catch {
    // 后台刷新失败就留着上一次的数据，不弹提示打扰：下个周期会再试。
  }
}

onMounted(async () => {
  if (!store.providers.length) await store.reloadProviders()
  refreshTimer = window.setInterval(() => void autoRefresh(), AUTO_REFRESH_MS)
  document.addEventListener('visibilitychange', autoRefresh)
})

onBeforeUnmount(() => {
  if (refreshTimer) window.clearInterval(refreshTimer)
  document.removeEventListener('visibilitychange', autoRefresh)
})

function openNew() {
  editing.value = null
  editorShow.value = true
}

function openEdit(p: Provider) {
  editing.value = p
  editorShow.value = true
}

async function activate(p: Provider) {
  busy.value = p.id
  try {
    await store.activate(p.id)
    message.success(`已切换到「${p.displayName}」`)
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    busy.value = null
  }
}

async function toggleEnabled(p: Provider, value: boolean) {
  busy.value = p.id
  try {
    await api.updateProvider({ ...p, enabled: value })
    await store.reloadProviders()
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  } finally {
    busy.value = null
  }
}

async function remove(p: Provider) {
  try {
    await api.deleteProvider(p.id)
    message.success('已删除')
    await store.reloadProviders()
  } catch (e) {
    message.error(e instanceof Error ? e.message : String(e))
  }
}

/** 每个供应商的密钥健康概览：有几个在冷却。 */
function coolingCount(p: Provider): number {
  return Object.values(p.keyHealth ?? {}).filter((h) => h.cooling).length
}

/** 是否配了用量查询模板 —— 没配就不在卡片上占位置。 */
function hasUsageQuery(p: Provider): boolean {
  return Boolean(p.usageQuery?.template)
}

/** 卡片的用量区读服务端定时刷出来的快照；按钮只是让后端立刻再查一次。 */
function queryUsage(p: Provider) {
  void store.refreshUsage(p.id, p)
}

const totalKeys = computed(() =>
  store.providers.reduce((sum, p) => sum + (p.keys?.filter((k) => k.enabled && k.key).length ?? 0), 0),
)
</script>

<template>
  <div class="page">
    <PageHeader
      title="供应商"
      :description="`共 ${store.providers.length} 个，其中 ${store.enabledProviders.length} 个已启用，${totalKeys} 个可用密钥。顶部下拉或卡片上的「设为生效」决定默认走哪个。`"
    >
      <template #actions>
        <NTooltip>
          <template #trigger>
            <NButton :loading="store.loading" @click="store.reloadProviders()">
              <template #icon>
                <NIcon :size="15"><RefreshCw /></NIcon>
              </template>
              刷新
            </NButton>
          </template>
          用量快照由服务端按各供应商的间隔定时刷新，本页每 30 秒自动重拉一次；
          这个按钮是立刻重拉一遍。
        </NTooltip>
        <NButton type="primary" @click="openNew">
          <template #icon>
            <NIcon :size="15"><Plus /></NIcon>
          </template>
          新增供应商
        </NButton>
      </template>
    </PageHeader>

    <NSpin :show="store.loading">
      <NEmpty
        v-if="!store.providers.length"
        description="还没有供应商，先添加一个被代理的上游"
        class="empty-block"
      >
        <template #extra>
          <NButton type="primary" @click="openNew">新增供应商</NButton>
        </template>
      </NEmpty>

      <div v-else class="provider-grid">
        <div
          v-for="p in store.providers"
          :key="p.id"
          class="provider-card"
          :class="{ active: p.active, disabled: !p.enabled }"
        >
          <div class="card-head">
            <div class="card-title">
              <span class="name">{{ p.displayName || p.name }}</span>
              <NTag v-if="p.active" size="tiny" type="info" :bordered="false">当前生效</NTag>
              <NTag v-if="!p.enabled" size="tiny" :bordered="false">已停用</NTag>
            </div>
            <NSpace align="center" :size="4">
              <NBadge
                v-if="coolingCount(p)"
                :value="coolingCount(p)"
                type="warning"
                :offset="[-2, 2]"
              >
                <NTooltip>
                  <template #trigger>
                    <span class="key-state warn">
                      <NIcon :size="12"><TriangleAlert /></NIcon>
                      密钥异常
                    </span>
                  </template>
                  有 {{ coolingCount(p) }} 个密钥处于冷却期（鉴权失败或限流）
                </NTooltip>
              </NBadge>
              <NSwitch
                size="small"
                :value="p.enabled"
                :loading="busy === p.id"
                @update:value="(v: boolean) => toggleEnabled(p, v)"
              />
            </NSpace>
          </div>

          <div class="meta mono">{{ p.baseUrl }}</div>

          <div class="tags">
            <NTag size="tiny" :bordered="false" type="info">
              {{ API_FORMAT_LABELS[p.apiFormat] ?? p.apiFormat }}
            </NTag>
            <NTag v-if="p.keys?.length" size="tiny" :bordered="false">
              {{ p.keys.filter((k) => k.enabled && k.key).length }} 个密钥
            </NTag>
            <NTag v-else size="tiny" :bordered="false" type="warning">透传客户端密钥</NTag>
            <NTag v-if="p.proxyMode === 'custom' && p.proxy?.enabled" size="tiny" :bordered="false">
              {{ p.proxy.type }} 代理
            </NTag>
            <NTag v-else-if="p.proxyMode === 'direct'" size="tiny" :bordered="false">直连</NTag>
            <NTag
              v-if="p.promptMode === 'custom' || p.promptRules?.length"
              size="tiny"
              :bordered="false"
              type="success"
            >
              提示词
            </NTag>
            <NTag v-for="t in p.tags ?? []" :key="t" size="tiny" :bordered="false">{{ t }}</NTag>
          </div>

          <div v-if="p.remark" class="remark">{{ p.remark }}</div>

          <div v-if="hasUsageQuery(p)" class="card-usage">
            <ProviderUsagePanel
              compact
              :snapshot="p.usage ?? null"
              :loading="Boolean(store.usageRefreshing[p.id])"
              :configured="true"
              @query="queryUsage(p)"
            />
          </div>

          <div class="card-foot">
            <span class="foot-meta">更新于 {{ formatRelative(p.updatedAt) }}</span>
            <NSpace :size="6">
              <NButton
                v-if="!p.active"
                size="tiny"
                :disabled="!p.enabled"
                :loading="busy === p.id"
                @click="activate(p)"
              >
                设为生效
              </NButton>
              <NButton size="tiny" @click="openEdit(p)">编辑</NButton>
              <NPopconfirm @positive-click="remove(p)">
                <template #trigger>
                  <NButton size="tiny" quaternary type="error">删除</NButton>
                </template>
                确定删除「{{ p.displayName || p.name }}」？
              </NPopconfirm>
            </NSpace>
          </div>
        </div>
      </div>
    </NSpin>

    <ProviderEditor v-model:show="editorShow" :provider="editing" @saved="store.reloadProviders()" />
  </div>
</template>

<style scoped>
.page {
  width: 100%;
  max-width: 1600px;
  margin: 0 auto;
}

.empty-block {
  padding: 80px 0;
}

/* 卡片按最小宽度自动排布，不再固定三列：窄窗口会自然降为两列/一列。 */
.provider-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(340px, 1fr));
  gap: var(--space-3);
  /* 同一行的卡片等高，底部操作区才能对齐。 */
  align-items: stretch;
  grid-auto-rows: 1fr;
}

.provider-card {
  display: flex;
  flex-direction: column;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: var(--space-4);
  min-width: 0;
  transition:
    border-color var(--duration) var(--ease),
    background var(--duration) var(--ease);
}

.provider-card:hover {
  border-color: var(--border-strong);
}

/* 当前生效：细描边 + 左侧强调条，不用粗绿框。 */
.provider-card.active {
  border-color: var(--accent-border);
  box-shadow: inset 3px 0 0 var(--accent);
}

.provider-card.disabled {
  opacity: 0.62;
}

.card-head {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
}

.card-title {
  display: flex;
  align-items: center;
  gap: 6px;
  min-width: 0;
}

.name {
  font-weight: 600;
  font-size: var(--text-base);
  color: var(--text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.key-state {
  display: inline-flex;
  align-items: center;
  gap: 3px;
  font-size: var(--text-2xs);
  color: var(--warning);
}

.key-state.warn {
  cursor: default;
}

.meta {
  margin: 6px 0 10px;
  color: var(--text-3);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.tags {
  display: flex;
  flex-wrap: wrap;
  gap: 4px;
}

.remark {
  margin-top: 10px;
  font-size: var(--text-xs);
  color: var(--text-2);
  line-height: 1.55;
}

.card-usage {
  margin-top: var(--space-3);
  padding-top: var(--space-3);
  border-top: 1px solid var(--border);
}

.card-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  margin-top: auto;
  padding-top: var(--space-3);
}

.foot-meta {
  font-size: var(--text-2xs);
  color: var(--text-3);
  white-space: nowrap;
}
</style>
