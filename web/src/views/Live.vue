<script setup lang="ts">
import { computed } from 'vue'
import { NCard, NGrid, NGridItem, NEmpty, NTag, NProgress, NSpace, NStatistic, NAlert } from 'naive-ui'
import { useAppStore } from '@/stores/app'
import type { LiveRequest } from '@/types'
import { formatDuration, formatNumber, formatTps } from '@/utils/format'

const store = useAppStore()

const requests = computed(() => store.liveRequests)

/** 用首个请求的耗时做进度条的参考刻度，让条长有相对意义。 */
const maxElapsed = computed(() =>
  Math.max(10_000, ...requests.value.map((r) => r.elapsedMs)),
)

function progress(r: LiveRequest): number {
  return Math.min(100, (r.elapsedMs / maxElapsed.value) * 100)
}
</script>

<template>
  <div class="page">
    <div class="page-head">
      <div>
        <h2>实时监控</h2>
        <p class="sub">
          正在转发中的请求。token 数是按已生成内容实时估算的，请求结束后以服务端记录的精确值替换。
        </p>
      </div>
      <NTag :type="requests.length ? 'info' : 'default'" :bordered="false" size="small">
        {{ requests.length }} 个进行中
      </NTag>
    </div>

    <NAlert v-if="!requests.length" type="default" :bordered="false">
      <NEmpty description="当前没有进行中的请求" style="padding: 60px 0" />
    </NAlert>

    <NGrid v-else :cols="24" :x-gap="12" :y-gap="12">
      <NGridItem v-for="r in requests" :key="r.id" :span="8">
        <NCard size="small" class="live-card" :bordered="false">
          <div class="live-head">
            <div class="live-title">
              <span class="dot" />
              <span class="mono model">{{ r.model || '未知模型' }}</span>
            </div>
            <NTag v-if="r.stream" size="tiny" type="info" :bordered="false">流式</NTag>
            <NTag v-else size="tiny" :bordered="false">非流式</NTag>
          </div>

          <div class="live-meta">
            <span>{{ r.providerName || '—' }}</span>
            <span class="mono path">{{ r.path }}</span>
          </div>

          <NProgress
            type="line"
            :percentage="progress(r)"
            :show-indicator="false"
            :height="4"
            :border-radius="2"
            status="info"
            style="margin: 10px 0"
          />

          <div class="live-stats">
            <NStatistic label="已生成" size="small">
              <span class="num big">{{ formatNumber(r.completionTokens) }}</span>
            </NStatistic>
            <NStatistic label="实时 TPS" size="small">
              <span class="num big">{{ formatTps(r.tps) }}</span>
            </NStatistic>
            <NStatistic label="首 token" size="small">
              <span class="num big">{{ r.ttftMs ? formatDuration(r.ttftMs) : '等待中' }}</span>
            </NStatistic>
            <NStatistic label="已耗时" size="small">
              <span class="num big">{{ formatDuration(r.elapsedMs) }}</span>
            </NStatistic>
          </div>

          <div class="live-foot">
            <span class="muted">{{ r.clientIp }}</span>
            <span class="muted mono">{{ r.id }}</span>
          </div>
        </NCard>
      </NGridItem>
    </NGrid>

    <NSpace v-if="requests.length" justify="center" style="margin-top: 16px">
      <span class="muted small">数据由服务端每 0.7 秒推送一次</span>
    </NSpace>
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

.live-card {
  border-radius: 12px;
  box-shadow: 0 1px 2px rgba(16, 24, 40, 0.04), 0 4px 16px rgba(16, 24, 40, 0.06);
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
  font-size: 13px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

/* 呼吸灯：一眼能看出这是「正在发生」的东西 */
.dot {
  width: 8px;
  height: 8px;
  border-radius: 50%;
  background: #2082ff;
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
  font-size: 12px;
  opacity: 0.65;
}

.path {
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.live-stats {
  display: grid;
  grid-template-columns: repeat(4, 1fr);
  gap: 8px;
}

.big {
  font-size: 15px;
  font-weight: 600;
}

.live-foot {
  display: flex;
  align-items: center;
  justify-content: space-between;
  margin-top: 10px;
  font-size: 11px;
  opacity: 0.5;
}

.muted {
  opacity: 0.6;
}

.small {
  font-size: 11px;
}
</style>
