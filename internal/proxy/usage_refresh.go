package proxy

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"ai_proxy/internal/model"
	"ai_proxy/internal/store"
)

// 用量快照的服务端定时刷新。
//
// 为什么放在服务端而不是前端定时器：额度是「过一会儿就想知道」的东西，
// 而管理界面大多数时候是关着的。放在服务端，快照一直在库里保持新鲜，
// 打开页面直接读到结果，不必等四个上游接口跑完；前端也就不需要为了
// 保持数据新鲜而常驻一个定时器。
//
// 节拍固定，是否到期由每个供应商自己的间隔决定 —— 一个循环管所有供应商，
// 不需要按间隔分组起多个定时器。

// RefreshUsage 立刻查一次用量并落库，返回这次的结果。
//
// 手动刷新和定时刷新走的是同一条路径 —— 两者唯一的区别只是谁触发，
// 没必要为「手动」再写一份「查询+保存」，那只会让两处慢慢分叉。
func (s *Server) RefreshUsage(ctx context.Context, prov model.Provider, settings model.Settings) model.UsageSnapshot {
	snap := s.QueryUsage(ctx, prov, settings)
	snap.ProviderID = prov.ID
	if err := s.store.SaveProviderUsage(snap); err != nil {
		s.logger.Warn("保存用量快照失败", "provider", prov.Name, "err", err)
	}
	return snap
}

// UsageRefresher 按供应商配置的间隔刷新用量快照。
type UsageRefresher struct {
	store  *store.Store
	proxy  *Server
	logger *slog.Logger

	// tick 是检查节拍，不是刷新间隔。取比最小可选间隔（1 分钟）小一档，
	// 保证到期后最多等一个节拍就开始查。
	tick time.Duration
	// timeout 是单个供应商一次查询的超时。额度接口是几个小请求，
	// 30 秒还没回来就说明上游有问题，不该继续占着这一轮。
	timeout time.Duration
}

// NewUsageRefresher 构造用量刷新器。
func NewUsageRefresher(st *store.Store, srv *Server, logger *slog.Logger) *UsageRefresher {
	return &UsageRefresher{
		store:   st,
		proxy:   srv,
		logger:  logger,
		tick:    15 * time.Second,
		timeout: 30 * time.Second,
	}
}

// Run 阻塞运行，直到 ctx 结束。启动时先立即检查一次，
// 这样进程重启后即使刚过刷新点也能马上补上，不用干等一个节拍。
func (r *UsageRefresher) Run(ctx context.Context) {
	r.refreshDue(ctx)

	ticker := time.NewTicker(r.tick)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.refreshDue(ctx)
		}
	}
}

// refreshDue 遍历所有开了定时刷新的供应商，把到期的查一遍。
func (r *UsageRefresher) refreshDue(ctx context.Context) {
	providers, err := r.store.ListProviders()
	if err != nil {
		r.logger.Warn("读取供应商失败，跳过本轮用量刷新", "err", err)
		return
	}
	stored, err := r.store.ListProviderUsage()
	if err != nil {
		r.logger.Warn("读取用量快照失败，跳过本轮用量刷新", "err", err)
		return
	}
	settings, err := r.store.GetSettings()
	if err != nil {
		r.logger.Warn("读取设置失败，跳过本轮用量刷新", "err", err)
		return
	}

	now := time.Now()
	var due []model.Provider
	for _, p := range providers {
		interval := time.Duration(p.UsageQuery.AutoRefreshSeconds) * time.Second
		if interval <= 0 || p.UsageQuery.Template == "" {
			continue
		}
		if last, ok := stored[p.ID]; ok && now.Sub(last.FetchedAt) < interval {
			continue
		}
		due = append(due, p)
	}
	if len(due) == 0 {
		return
	}

	// 并发查：某个上游卡住时不该拖住其它供应商的刷新，
	// 但本轮要等它们都结束再进入下一个节拍，避免请求越堆越多。
	var wg sync.WaitGroup
	for _, p := range due {
		wg.Add(1)
		go func(prov model.Provider) {
			defer wg.Done()
			qctx, cancel := context.WithTimeout(ctx, r.timeout)
			defer cancel()

			snap := r.proxy.RefreshUsage(qctx, prov, settings)
			if snap.OK {
				r.logger.Debug("已刷新用量快照", "provider", prov.Name, "plan", snap.PlanName)
			} else {
				// 读不到也照常落库：界面上要能看到「上一次查询为什么失败」，
				// 而不是一片空白让人以为功能坏了。
				r.logger.Warn("用量刷新未成功", "provider", prov.Name, "err", snap.Error)
			}
		}(p)
	}
	wg.Wait()
}
