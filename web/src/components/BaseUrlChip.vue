<script setup lang="ts">
// 顶栏的客户端接入地址。做成常驻 chip 而不是仪表盘上的一张卡片：
// 这个地址是配客户端时才要看的东西，配的时候人可能在任何一页；
// 而仪表盘首屏该留给指标。点一下即复制，悬浮给出当前生效供应商的全貌。
//
// 必须是个子组件：useMessage() 依赖 NMessageProvider 的注入，
// 而 provider 由 App.vue 自己的模板渲染 —— 在 App.vue 的 setup 里取不到。
import { computed, onBeforeUnmount, ref } from 'vue'
import { useMessage, NTooltip } from 'naive-ui'
import { useAppStore } from '@/stores/app'

const store = useAppStore()
const message = useMessage()

const url = computed(() => store.listenBaseURL)
const active = computed(() => store.activeProvider)
const shortName = computed(() => active.value?.name ?? '<短名>')

const copied = ref(false)
let resetTimer: number | undefined

function copy() {
  navigator.clipboard.writeText(url.value).then(
    () => {
      message.success('已复制')
      copied.value = true
      window.clearTimeout(resetTimer)
      resetTimer = window.setTimeout(() => {
        copied.value = false
      }, 1400)
    },
    () => message.error('复制失败'),
  )
}

onBeforeUnmount(() => window.clearTimeout(resetTimer))
</script>

<template>
  <NTooltip trigger="hover" placement="bottom-start" :style="{ maxWidth: '420px' }">
    <template #trigger>
      <button class="chip" type="button" @click="copy">
        <code class="url mono">{{ url }}</code>
        <svg
          v-if="!copied"
          class="icon"
          viewBox="0 0 16 16"
          width="12"
          height="12"
          aria-hidden="true"
        >
          <rect
            x="5.5"
            y="5.5"
            width="8"
            height="8"
            rx="1.5"
            fill="none"
            stroke="currentColor"
            stroke-width="1.3"
          />
          <path
            d="M10.5 3.5V3a1.5 1.5 0 0 0-1.5-1.5H3.5A1.5 1.5 0 0 0 2 3v5.5A1.5 1.5 0 0 0 3.5 10H4"
            fill="none"
            stroke="currentColor"
            stroke-width="1.3"
            stroke-linecap="round"
          />
        </svg>
        <svg v-else class="icon ok" viewBox="0 0 16 16" width="12" height="12" aria-hidden="true">
          <path
            d="M3 8.5 6.5 12 13 4.5"
            fill="none"
            stroke="currentColor"
            stroke-width="1.6"
            stroke-linecap="round"
            stroke-linejoin="round"
          />
        </svg>
      </button>
    </template>

    <div class="tip">
      <div class="tip-row">
        <span class="tip-label">客户端 base_url</span>
        <code class="mono">{{ url }}</code>
      </div>
      <template v-if="active">
        <div class="tip-row">
          <span class="tip-label">当前生效</span>
          <span>{{ active.displayName }}（短名 <code class="mono">{{ active.name }}</code>，{{ active.apiFormat }}）</span>
        </div>
        <div class="tip-row">
          <span class="tip-label">上游地址</span>
          <code class="mono">{{ active.baseUrl }}</code>
        </div>
      </template>
      <div v-else class="tip-row">
        <span class="tip-label">当前生效</span>
        <span>未配置供应商</span>
      </div>
      <div class="tip-row">
        <span class="tip-label">指定供应商</span>
        <code class="mono">{{ url }}/p/{{ shortName }}/…</code>
      </div>
      <div class="tip-note">点击复制客户端 base_url</div>
    </div>
  </NTooltip>
</template>

<style scoped>
.chip {
  display: inline-flex;
  align-items: center;
  gap: 6px;
  max-width: 280px;
  height: 26px;
  padding: 0 8px;
  border: none;
  border-radius: 6px;
  background: rgba(128, 128, 128, 0.12);
  color: inherit;
  cursor: pointer;
  transition: background 0.15s ease;
}

.chip:hover {
  background: rgba(128, 128, 128, 0.22);
}

.url {
  font-size: 12px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
}

.icon {
  flex-shrink: 0;
  opacity: 0.5;
}

.icon.ok {
  color: #18a058;
  opacity: 1;
}

.tip {
  display: flex;
  flex-direction: column;
  gap: 4px;
  font-size: 12px;
}

.tip-row {
  display: flex;
  gap: 8px;
}

.tip-label {
  flex-shrink: 0;
  width: 76px;
  white-space: nowrap;
  opacity: 0.6;
}

.tip-note {
  opacity: 0.6;
  font-size: 11px;
}
</style>
