<script setup lang="ts">
import { computed } from 'vue'
import { NEmpty, NTag, NProgress } from 'naive-ui'
import PageHeader from '@/components/PageHeader.vue'
import { useAppStore } from '@/stores/app'
import type { LiveRequest } from '@/types'
import { formatDuration, formatNumber, formatTps } from '@/utils/format'

const store = useAppStore()

const requests = computed(() => store.liveRequests)

/** 用首个请求的耗时做进度条的参考刻度，让条长有相对意义。 */
const maxElapsed = computed(() => Math.max(10_000, ...requests.value.map((r) => r.elapsedMs)))

function progress(r: LiveRequest): number {
  return Math.min(100, (r.elapsedMs / maxElapsed.value) * 100)
}
</script>

<template>
  <div class="page">
    <PageHeader
      title="实时监控"
      description="正在转发中的请求。token 数是按已生成内容实时估算的，请求结束后以服务端记录的精确值替换。"
    >
      <template #actions>
        <span :class="['count-pill', requests.length ? 'busy' : 'idle']">
          <span class="dot" />
          {{ requests.length }} 个进行中
        </span>
      </template>
    </PageHeader>

    <div v-if="!requests.length" class="empty-wrap">
      <NEmpty description="当前没有进行中的请求" />
    </div>

    <template v-else>
      <div class="live-grid">
        <article v-for="r in requests" :key="r.id" class="live-card">
          <header class="live-head">
            <div class="live-title">
              <span class="pulse-dot" aria-hidden="true" />
              <span class="mono model">{{ r.model || '未知模型' }}</span>
            </div>
            <NTag v-if="r.stream" size="tiny" type="info" :bordered="false">流式</NTag>
            <NTag v-else size="tiny" :bordered="false">非流式</NTag>
          </header>

          <div class="live-meta">
            <span class="provider">{{ r.providerName || '—' }}</span>
            <span class="mono path">{{ r.path }}</span>
          </div>

          <NProgress
            type="line"
            :percentage="progress(r)"
            :show-indicator="false"
            :height="4"
            :border-radius="2"
            status="info"
            class="live-progress"
          />

          <div class="live-stats">
            <div class="stat">
              <span class="stat-label">已生成</span>
              <span class="stat-value num">{{ formatNumber(r.completionTokens) }}</span>
            </div>
            <div class="stat">
              <span class="stat-label">实时 TPS</span>
              <span class="stat-value num">{{ formatTps(r.tps) }}</span>
            </div>
            <div class="stat">
              <span class="stat-label">首 token</span>
              <span class="stat-value num" :class="{ pending: !r.ttftMs }">
                {{ r.ttftMs ? formatDuration(r.ttftMs) : '等待中' }}
              </span>
            </div>
            <div class="stat">
              <span class="stat-label">已耗时</span>
              <span class="stat-value num">{{ formatDuration(r.elapsedMs) }}</span>
            </div>
          </div>

          <footer class="live-foot">
            <span>{{ r.clientIp }}</span>
            <span class="mono">{{ r.id }}</span>
          </footer>
        </article>
      </div>

      <p class="live-hint">数据由服务端每 0.7 秒推送一次</p>
    </template>
  </div>
</template>

<style scoped>
.page {
  width: 100%;
  max-width: 1600px;
  margin: 0 auto;
}

.count-pill {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  height: 26px;
  padding: 0 10px;
  border-radius: 6px;
  border: 1px solid var(--border);
  background: var(--surface-2);
  font-size: var(--text-2xs);
  color: var(--text-2);
  white-space: nowrap;
}

.count-pill.busy {
  color: var(--accent);
  border-color: var(--accent-border);
  background: var(--accent-soft);
}

.count-pill .dot {
  width: 6px;
  height: 6px;
  border-radius: 50%;
  background: currentColor;
  flex-shrink: 0;
}

.empty-wrap {
  display: flex;
  align-items: center;
  justify-content: center;
  min-height: 280px;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius);
}

.live-grid {
  display: grid;
  grid-template-columns: repeat(auto-fill, minmax(320px, 1fr));
  gap: var(--space-3);
}

.live-card {
  display: flex;
  flex-direction: column;
  background: var(--surface);
  border: 1px solid var(--border);
  border-radius: var(--radius);
  padding: var(--space-4);
  min-width: 0;
}

.live-head {
  display: flex;
  align-items: center;
  gap: 8px;
}

.live-title {
  display: flex;
  align-items: center;
  gap: 8px;
  flex: 1;
  min-width: 0;
}

.model {
  font-weight: 600;
  font-size: var(--text-sm);
  color: var(--text);
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* 呼吸灯：一眼能看出这是「正在发生」的东西 */
.pulse-dot {
  width: 7px;
  height: 7px;
  border-radius: 50%;
  background: var(--accent);
  flex-shrink: 0;
  animation: pulse 1.4s ease-in-out infinite;
}

@keyframes pulse {
  0%,
  100% {
    opacity: 1;
    transform: scale(1);
  }
  50% {
    opacity: 0.35;
    transform: scale(0.8);
  }
}

.live-meta {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  margin-top: 6px;
  font-size: var(--text-xs);
  color: var(--text-3);
}

.provider {
  flex-shrink: 0;
}

.path {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.live-progress {
  margin: 10px 0;
}

.live-stats {
  display: grid;
  grid-template-columns: repeat(4, minmax(0, 1fr));
  gap: 8px;
}

.stat {
  display: flex;
  flex-direction: column;
  gap: 2px;
  min-width: 0;
}

.stat-label {
  font-size: var(--text-2xs);
  color: var(--text-3);
  white-space: nowrap;
}

.stat-value {
  font-size: var(--text-base);
  font-weight: 600;
  color: var(--text);
  white-space: nowrap;
  overflow: hidden;
  text-overflow: ellipsis;
}

.stat-value.pending {
  color: var(--warning);
  font-weight: 500;
}

.live-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  gap: 8px;
  margin-top: var(--space-3);
  padding-top: var(--space-2);
  border-top: 1px solid var(--border);
  font-size: var(--text-2xs);
  color: var(--text-3);
  overflow: hidden;
}

.live-hint {
  margin: var(--space-4) 0 0;
  text-align: center;
  font-size: var(--text-2xs);
  color: var(--text-3);
}
</style>
