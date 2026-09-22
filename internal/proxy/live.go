package proxy

import (
	"sort"
	"sync"
	"time"

	"ai_proxy/internal/model"
)

// LiveRegistry 记录正在转发中的请求，供实时监控页展示。
//
// 这里的每个条目都对应一个还没结束的 HTTP 转发，生命周期与请求一致；
// 快照里的 ElapsedMs 与 TPS 在读取时实时计算，而不是靠定时器刷新。
type LiveRegistry struct {
	mu      sync.RWMutex
	entries map[string]*liveEntry
}

type liveEntry struct {
	info      model.LiveRequest
	startedAt time.Time
	ttftAt    time.Time
	// tokens 与 lastAt 用于计算即时 TPS，由流式解析器在每次增量后更新。
	tokens int
	lastAt time.Time
	tps    float64
}

func NewLiveRegistry() *LiveRegistry {
	return &LiveRegistry{entries: map[string]*liveEntry{}}
}

// Add 登记一个进行中的请求，返回其 ID。
func (r *LiveRegistry) Add(info model.LiveRequest) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	info.StartedAt = now
	r.entries[info.ID] = &liveEntry{info: info, startedAt: now}
	return info.ID
}

// MarkFirstToken 记录首 token 出现的时刻。
func (r *LiveRegistry) MarkFirstToken(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok || !e.ttftAt.IsZero() {
		return
	}
	e.ttftAt = time.Now()
}

// UpdateTokens 更新已生成 token 数，并据此重算即时 TPS。
func (r *LiveRegistry) UpdateTokens(id string, tokens int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.entries[id]
	if !ok || e.ttftAt.IsZero() {
		return
	}
	now := time.Now()
	elapsed := now.Sub(e.ttftAt).Seconds()
	if elapsed > 0 {
		// 用整体均值而不是瞬时差分：瞬时值抖动太大，界面上跳得没法看。
		e.tps = float64(tokens) / elapsed
	}
	e.tokens = tokens
	e.lastAt = now
}

// Remove 摘除已结束的请求。
func (r *LiveRegistry) Remove(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.entries, id)
}

// Snapshot 返回当前所有进行中请求的快照，按开始时间升序。
func (r *LiveRegistry) Snapshot() []model.LiveRequest {
	r.mu.RLock()
	defer r.mu.RUnlock()

	now := time.Now()
	out := make([]model.LiveRequest, 0, len(r.entries))
	for _, e := range r.entries {
		item := e.info
		item.StartedAt = e.startedAt
		item.ElapsedMs = now.Sub(e.startedAt).Milliseconds()
		if !e.ttftAt.IsZero() {
			item.TTFTMs = e.ttftAt.Sub(e.startedAt).Milliseconds()
		}
		item.CompletionTokens = e.tokens
		item.TPS = e.tps
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].StartedAt.Before(out[j].StartedAt) })
	return out
}

// Count 返回进行中的请求数。
func (r *LiveRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.entries)
}
