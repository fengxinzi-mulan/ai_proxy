// 订阅后端的 SSE 事件流。
//
// 断线由浏览器自动重连；这里只负责把事件分发给回调，以及在页面不可见时
// 暂停刷新（避免后台标签页白白重渲染）。

import { onBeforeUnmount, onMounted, ref } from 'vue'
import { getAccessKey } from '@/api'
import type { ProxyEvent } from '@/types'

export interface EventHandlers {
  onLog?: (log: Extract<ProxyEvent, { type: 'log' }>['data']) => void
  onLive?: (items: Extract<ProxyEvent, { type: 'live' }>['data']) => void
  onProvidersChanged?: () => void
  onSettingsChanged?: (s: Extract<ProxyEvent, { type: 'settings' }>['data']) => void
}

export function useEvents(handlers: EventHandlers) {
  const connected = ref(false)
  let source: EventSource | null = null

  function connect() {
    const key = getAccessKey()
    // EventSource 无法自定义请求头，所以访问密钥走查询参数。
    const url = key ? `/api/events?key=${encodeURIComponent(key)}` : '/api/events'
    source = new EventSource(url)

    source.onopen = () => {
      connected.value = true
    }

    source.onerror = () => {
      // EventSource 会自动重连，这里只更新状态指示。
      connected.value = false
    }

    source.onmessage = (ev) => {
      let payload: ProxyEvent
      try {
        payload = JSON.parse(ev.data)
      } catch {
        return
      }
      switch (payload.type) {
        case 'log':
          handlers.onLog?.(payload.data)
          break
        case 'live':
          handlers.onLive?.(payload.data)
          break
        case 'providers':
          handlers.onProvidersChanged?.()
          break
        case 'settings':
          handlers.onSettingsChanged?.(payload.data)
          break
      }
    }
  }

  function disconnect() {
    source?.close()
    source = null
    connected.value = false
  }

  onMounted(connect)
  onBeforeUnmount(disconnect)

  return { connected, disconnect, reconnect: connect }
}
