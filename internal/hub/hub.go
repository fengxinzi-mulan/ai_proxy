// Package hub 提供进程内的事件广播，供管理界面的 SSE 通道使用。
//
// 采用「慢订阅者丢消息」策略：管理界面只是展示用，订阅端一旦跟不上，
// 丢掉几条中间事件比拖慢转发主链路要划算得多。请求日志本身已经落库，
// 前端刷新页面就能拿到完整数据。
package hub

import (
	"encoding/json"
	"sync"
)

// subscriberBuffer 是每个订阅者的事件缓冲深度。
const subscriberBuffer = 64

// Event 是一条广播消息。
type Event struct {
	Type string `json:"type"`
	Data any    `json:"data"`
}

// Hub 是事件广播中心。
type Hub struct {
	mu   sync.RWMutex
	subs map[chan []byte]struct{}
}

func New() *Hub {
	return &Hub{subs: map[chan []byte]struct{}{}}
}

// Subscribe 注册一个订阅者。调用方必须在结束时调用 Unsubscribe。
func (h *Hub) Subscribe() chan []byte {
	ch := make(chan []byte, subscriberBuffer)
	h.mu.Lock()
	h.subs[ch] = struct{}{}
	h.mu.Unlock()
	return ch
}

// Unsubscribe 注销订阅者并关闭通道。
func (h *Hub) Unsubscribe(ch chan []byte) {
	h.mu.Lock()
	if _, ok := h.subs[ch]; ok {
		delete(h.subs, ch)
		close(ch)
	}
	h.mu.Unlock()
}

// Broadcast 向所有订阅者推送一条事件。
func (h *Hub) Broadcast(eventType string, data any) {
	payload, err := json.Marshal(Event{Type: eventType, Data: data})
	if err != nil {
		return
	}

	h.mu.RLock()
	defer h.mu.RUnlock()
	for ch := range h.subs {
		select {
		case ch <- payload:
		default:
			// 订阅者跟不上就跳过这条，不阻塞广播方。
		}
	}
}

// SubscriberCount 返回当前订阅者数量。
func (h *Hub) SubscriberCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs)
}
