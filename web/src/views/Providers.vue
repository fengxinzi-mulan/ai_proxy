<script setup lang="ts">
import { computed, onMounted, ref } from 'vue'
import {
  NCard,
  NGrid,
  NGridItem,
  NButton,
  NSpace,
  NTag,
  NSwitch,
  NEmpty,
  NSpin,
  NTooltip,
  NBadge,
  useMessage,
  NPopconfirm,
} from 'naive-ui'
import ProviderEditor from '@/components/ProviderEditor.vue'
import ProviderUsagePanel from '@/components/ProviderUsagePanel.vue'
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

onMounted(async () => {
  if (!store.providers.length) await store.reloadProviders()
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
    <div class="page-head">
      <div>
        <h2>供应商</h2>
        <p class="sub">
          共 {{ store.providers.length }} 个，其中 {{ store.enabledProviders.length }} 个已启用，
          {{ totalKeys }} 个可用密钥。顶部下拉或卡片上的「设为生效」决定默认走哪个。
        </p>
      </div>
      <NSpace>
        <NButton :loading="store.loading" @click="store.reloadProviders()">刷新</NButton>
        <NButton type="primary" @click="openNew">+ 新增供应商</NButton>
      </NSpace>
    </div>

    <NSpin :show="store.loading">
      <NEmpty
        v-if="!store.providers.length"
        description="还没有供应商，先添加一个被代理的上游"
        style="padding: 80px 0"
      >
        <template #extra>
          <NButton type="primary" @click="openNew">新增供应商</NButton>
        </template>
      </NEmpty>

      <NGrid v-else :cols="24" :x-gap="12" :y-gap="12">
        <NGridItem v-for="p in store.providers" :key="p.id" :span="8">
          <NCard size="small" class="provider-card" :class="{ active: p.active }" :bordered="false">
            <div class="card-head">
              <div class="card-title">
                <span class="name">{{ p.displayName || p.name }}</span>
                <NTag v-if="p.active" size="tiny" type="success" :bordered="false">生效中</NTag>
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
                      <span class="key-state warn">密钥异常</span>
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
              <span class="muted small">更新于 {{ formatRelative(p.updatedAt) }}</span>
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
          </NCard>
        </NGridItem>
      </NGrid>
    </NSpin>

    <ProviderEditor v-model:show="editorShow" :provider="editing" @saved="store.reloadProviders()" />
  </div>
</template>

<style scoped>
.page {
  max-width: 1560px;
  margin: 0 auto;
}

.page-head {
  display: flex;
  align-items: flex-start;
  justify-content: space-between;
  gap: 16px;
  margin-bottom: 16px;
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

.provider-card {
  border-radius: 12px;
  box-shadow: 0 1px 2px rgba(16, 24, 40, 0.04), 0 4px 16px rgba(16, 24, 40, 0.06);
  height: 100%;
  transition: box-shadow 0.15s ease, transform 0.15s ease;
}

/* NCard 在根元素和内容之间还包了一层 .n-card-content，
   flex 必须下沉到那一层，否则 .card-foot 的 margin-top: auto 推不动 ——
   内容短的卡片会在页脚下方留出一块空白，底部圆角看起来像被裁掉了。 */
.provider-card :deep(.n-card-content) {
  display: flex;
  flex-direction: column;
  height: 100%;
  box-sizing: border-box;
}

.provider-card:hover {
  transform: translateY(-1px);
  box-shadow: 0 2px 4px rgba(16, 24, 40, 0.06), 0 8px 24px rgba(16, 24, 40, 0.1);
}

.provider-card.active {
  /* 选中描边用内投影而不是外投影：卡片底边正好贴在滚动容器下沿，
     画在盒子外面的 2px 会被 overflow:scroll 的容器裁掉，导致轮廓缺一条边。
     内投影画在卡片内部，任何容器都不会裁到它。 */
  box-shadow: inset 0 0 0 2px rgba(24, 160, 88, 0.6),
    0 4px 16px rgba(16, 24, 40, 0.08);
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
  font-size: 15px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.key-state {
  font-size: 11px;
  color: #f0a020;
}

.key-state.warn {
  cursor: default;
}

.meta {
  opacity: 0.62;
  margin: 6px 0 8px;
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
  font-size: 12px;
  opacity: 0.68;
  margin-top: 8px;
  line-height: 1.5;
}

.card-usage {
  margin-top: 10px;
  padding-top: 10px;
  border-top: 1px solid rgba(128, 128, 128, 0.16);
}

.card-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  margin-top: auto;
  padding-top: 12px;
}

.small {
  font-size: 11px;
}

.muted {
  opacity: 0.55;
}
</style>
