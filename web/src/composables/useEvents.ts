// 订阅后端的 SSE 事件流。
//
// 断线由浏览器自动重连；这里只负责把事件分发给回调，以及在页面不可见时
// 暂停刷新（避免后台标签页白白重渲染）。

import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import { getAccessKey } from '@/api'
import { useAppStore } from '@/stores/app'
import type { ProxyEvent } from '@/types'

export interface EventHandlers {
  onLog?: (log: Extract<ProxyEvent, { type: 'log' }>['data']) => void
  onLive?: (items: Extract<ProxyEvent, { type: 'live' }>['data']) => void
  onProvidersChanged?: () => void
  onSettingsChanged?: (s: Extract<ProxyEvent, { type: 'settings' }>['data']) => void
  /** 日志被清空（清空历史或按条件删除）。载荷是删掉的行数。 */
  onLogsCleared?: (deleted: number) => void
  onUpdate?: (s: Extract<ProxyEvent, { type: 'update' }>['data']) => void
  onDesktopCloseAsk?: () => void
}

export function useEvents(handlers: EventHandlers) {
  const store = useAppStore()
  const connected = ref(false)
  let source: EventSource | null = null

  function connect() {
    // 先关旧连接再建新的：这个方法会被 setup、密钥变更、手动重连调用多次，
    // 不先关就会出现两条 EventSource 同时推送 —— 日志页会把同一条日志插两次，
    // 旧的还被浏览器无限重连，白占连接数。
    disconnect()
    const key = getAccessKey()
    // EventSource 无法自定义请求头，所以访问密钥走查询参数。
    const url = key ? `/api/events?key=${encodeURIComponent(key)}` : '/api/events'
    const es = new EventSource(url)
    source = es

    es.onopen = () => {
      // 迟到的旧连接回调不能把新连接的状态改回去。
      if (source === es) connected.value = true
    }

    es.onerror = () => {
      // EventSource 会自动重连，这里只更新状态指示。
      if (source === es) connected.value = false
    }

    es.onmessage = (ev) => {
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
        case 'logs-cleared':
          handlers.onLogsCleared?.(payload.data?.deleted ?? 0)
          break
        case 'update':
          handlers.onUpdate?.(payload.data)
          break
        case 'desktop-close-ask':
          handlers.onDesktopCloseAsk?.()
          break
      }
    }
  }

  function disconnect() {
    source?.close()
    source = null
    connected.value = false
  }

  // 访问密钥变更后必须整体重建连接：EventSource 的 URL 里带着旧密钥，
  // 服务端会持续 401，而它遇到非 2xx 不会自动重连。置页与顶栏的实例都盯这个版本号，
  // 所以设置页只改一次密钥，所有订阅方都能一起恢复。
  watch(
    () => store.authVersion,
    () => connect(),
  )

  onMounted(connect)
  onBeforeUnmount(disconnect)

  return { connected, disconnect, reconnect: connect }
}
