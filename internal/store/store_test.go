package store

import (
	"math"
	"testing"
	"time"

	"ai_proxy/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestSettingsRoundTrip(t *testing.T) {
	st := newTestStore(t)

	got, err := st.GetSettings()
	if err != nil {
		t.Fatalf("首次读取设置失败: %v", err)
	}
	// 首次读取应返回可用的默认值，而不是全零。
	if got.Host == "" || got.Port == 0 {
		t.Errorf("首次读取应得到默认监听配置，实际 %+v", got)
	}
	if !got.UsageInjectDefault {
		t.Error("usage 注入默认应为开启")
	}

	got.Proxy = model.ProxyConfig{Enabled: true, Type: model.ProxySOCKS5, URL: "127.0.0.1:1080"}
	got.GlobalPrompt = model.PromptConfig{Enabled: true, Text: "你好", Strategy: model.StrategyAppend}
	if err := st.SaveSettings(got); err != nil {
		t.Fatalf("保存设置失败: %v", err)
	}

	again, err := st.GetSettings()
	if err != nil {
		t.Fatalf("重新读取设置失败: %v", err)
	}
	if !again.Proxy.Enabled || again.Proxy.Type != model.ProxySOCKS5 || again.Proxy.URL != "127.0.0.1:1080" {
		t.Errorf("代理设置未正确往返: %+v", again.Proxy)
	}
	if again.GlobalPrompt.Text != "你好" {
		t.Errorf("提示词设置未正确往返: %+v", again.GlobalPrompt)
	}
}

// TestSettingsMissingFieldsFallBack 模拟「老版本数据库缺少新字段」的升级场景。
func TestSettingsMissingFieldsFallBack(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.db.Exec(
		`INSERT INTO settings(key, value) VALUES('global', ?)`, `{"host":"0.0.0.0","port":9999}`,
	); err != nil {
		t.Fatalf("写入旧版设置失败: %v", err)
	}

	got, err := st.GetSettings()
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	if got.Host != "0.0.0.0" || got.Port != 9999 {
		t.Errorf("已有字段应被保留: %+v", got)
	}
	// 缺失字段应回落到默认值，而不是 0。
	if got.DefaultTimeoutSeconds == 0 || got.MaxBodyBytes == 0 {
		t.Errorf("缺失字段应回落默认值: %+v", got)
	}
}

func newProvider(name string) model.Provider {
	return model.Provider{
		Name:         name,
		DisplayName:  name,
		Enabled:      true,
		BaseURL:      "https://api.example.com/v1",
		APIFormat:    model.FormatOpenAI,
		Keys:         []model.APIKey{{ID: "k1", Key: "sk-1", Enabled: true}},
		ExtraHeaders: map[string]string{"X-A": "1"},
		PromptRules:  []model.PromptRule{{ID: "r1", Enabled: true, ModelPattern: "^gpt", Text: "hi"}},
		Proxy:        model.ProxyConfig{Enabled: true, Type: model.ProxyHTTP, URL: "127.0.0.1:7890"},
		Prompt:       model.PromptConfig{Enabled: true, Text: "系统提示", Strategy: model.StrategyReplace},
		CustomUsage:  &model.CustomUsageMapping{PromptTokens: "a.b"},
		Tags:         []string{"主力"},
	}
}

func TestProviderCRUD(t *testing.T) {
	st := newTestStore(t)

	created, err := st.CreateProvider(newProvider("alpha"))
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}
	if created.ID == 0 {
		t.Fatal("应返回自增 ID")
	}
	if !created.Active {
		t.Error("第一个创建的供应商应自动成为当前生效")
	}
	if created.DisplayName != "alpha" {
		t.Errorf("展示名缺省时应回落到短名，实际 %q", created.DisplayName)
	}

	// 往返后嵌套结构必须完整保留。
	got, err := st.GetProvider(created.ID)
	if err != nil {
		t.Fatalf("读取供应商失败: %v", err)
	}
	if len(got.Keys) != 1 || got.Keys[0].Key != "sk-1" {
		t.Errorf("密钥未正确往返: %+v", got.Keys)
	}
	if got.ExtraHeaders["X-A"] != "1" {
		t.Errorf("额外请求头未正确往返: %+v", got.ExtraHeaders)
	}
	if len(got.PromptRules) != 1 || got.PromptRules[0].ModelPattern != "^gpt" {
		t.Errorf("提示词规则未正确往返: %+v", got.PromptRules)
	}
	if !got.Proxy.Enabled || got.Proxy.URL != "127.0.0.1:7890" {
		t.Errorf("代理配置未正确往返: %+v", got.Proxy)
	}
	if got.CustomUsage == nil || got.CustomUsage.PromptTokens != "a.b" {
		t.Errorf("自定义用量映射未正确往返: %+v", got.CustomUsage)
	}
	if len(got.Tags) != 1 || got.Tags[0] != "主力" {
		t.Errorf("标签未正确往返: %+v", got.Tags)
	}

	// 更新
	got.DisplayName = "阿尔法"
	got.BaseURL = "https://new.example.com"
	got.Enabled = false
	updated, err := st.UpdateProvider(got)
	if err != nil {
		t.Fatalf("更新供应商失败: %v", err)
	}
	if updated.DisplayName != "阿尔法" || updated.BaseURL != "https://new.example.com" || updated.Enabled {
		t.Errorf("更新未生效: %+v", updated)
	}
	// active 由专门接口维护，普通编辑不应把它改掉。
	if !updated.Active {
		t.Error("普通更新不应影响当前生效标记")
	}

	if _, err := st.GetProviderByName("alpha"); err != nil {
		t.Errorf("应按短名查到供应商: %v", err)
	}

	if err := st.DeleteProvider(created.ID); err != nil {
		t.Fatalf("删除供应商失败: %v", err)
	}
	if _, err := st.GetProvider(created.ID); err != ErrNotFound {
		t.Errorf("删除后应按 NotFound 报错，实际 %v", err)
	}
}

// 用量查询配置也走 JSON 列，需要确认迁移之后能完整往返，且未配置时不留下垃圾值。
func TestProviderUsageQueryRoundTrip(t *testing.T) {
	st := newTestStore(t)

	// 未配置：应保持零值，而不是被写成一个空对象。
	plain, err := st.CreateProvider(newProvider("plain"))
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}
	if got, err := st.GetProvider(plain.ID); err != nil {
		t.Fatalf("读取供应商失败: %v", err)
	} else if got.UsageQuery.Template != "" || got.UsageQuery.BaseURL != "" {
		t.Errorf("未配置用量查询时应保持为空，实际 %+v", got.UsageQuery)
	}

	p := newProvider("cc")
	p.UsageQuery = model.UsageQueryConfig{
		Template:           "commandcode",
		BaseURL:            "https://usage.example.com",
		AutoRefreshSeconds: 300,
	}
	created, err := st.CreateProvider(p)
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}
	got, err := st.GetProvider(created.ID)
	if err != nil {
		t.Fatalf("读取供应商失败: %v", err)
	}
	if got.UsageQuery.Template != "commandcode" || got.UsageQuery.BaseURL != "https://usage.example.com" {
		t.Fatalf("用量查询配置未正确往返: %+v", got.UsageQuery)
	}
	// 定时刷新间隔也要能存住，否则重启后自动刷新会静默失效
	if got.UsageQuery.AutoRefreshSeconds != 300 {
		t.Fatalf("定时刷新间隔未正确往返: %+v", got.UsageQuery)
	}

	// 改模板
	got.UsageQuery.Template = "other"
	got.UsageQuery.BaseURL = ""
	got.UsageQuery.AutoRefreshSeconds = 0
	updated, err := st.UpdateProvider(got)
	if err != nil {
		t.Fatalf("更新供应商失败: %v", err)
	}
	if updated.UsageQuery.Template != "other" || updated.UsageQuery.BaseURL != "" {
		t.Fatalf("更新未生效: %+v", updated.UsageQuery)
	}
	if updated.UsageQuery.AutoRefreshSeconds != 0 {
		t.Fatalf("关闭定时刷新未生效: %+v", updated.UsageQuery)
	}

	// 关掉用量查询：应回到零值
	updated.UsageQuery = model.UsageQueryConfig{}
	cleared, err := st.UpdateProvider(updated)
	if err != nil {
		t.Fatalf("更新供应商失败: %v", err)
	}
	if cleared.UsageQuery.Template != "" {
		t.Fatalf("关闭后应回到零值: %+v", cleared.UsageQuery)
	}
}

// 用量快照是服务端定时刷出来的运行态数据，要能覆盖写、能按供应商索引、
// 还要随供应商删除一起清掉（否则新建同名供应商会读到上一个的旧快照）。
func TestProviderUsageSnapshot(t *testing.T) {
	st := newTestStore(t)

	p, err := st.CreateProvider(newProvider("cc"))
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}

	// 没查过时是 nil，不是错误
	got, err := st.GetProviderUsage(p.ID)
	if err != nil {
		t.Fatalf("读取不存在的快照不该报错: %v", err)
	}
	if got != nil {
		t.Fatalf("没查过时应返回 nil，实际 %+v", got)
	}

	first := model.UsageSnapshot{
		OK:         true,
		ProviderID: p.ID,
		Template:   "commandcode",
		PlanName:   "GOAT",
		Account:    "tester",
		FetchedAt:  time.Now().Add(-time.Minute).UTC().Truncate(time.Millisecond),
		Windows: []model.UsageWindow{
			{Label: "5 小时", Used: 1, Cap: 14, Percent: 7.1},
			{Label: "本月", Used: 26, Cap: 70, Percent: 37, Hint: "推导"},
		},
		Raw: map[string]any{"/alpha/whoami": map[string]any{"ok": true}},
	}
	if err := st.SaveProviderUsage(first); err != nil {
		t.Fatalf("保存快照失败: %v", err)
	}

	got, err = st.GetProviderUsage(p.ID)
	if err != nil {
		t.Fatalf("读取快照失败: %v", err)
	}
	if got == nil || got.PlanName != "GOAT" || got.ProviderID != p.ID {
		t.Fatalf("快照未正确往返: %+v", got)
	}
	if len(got.Windows) != 2 || got.Windows[1].Hint != "推导" {
		t.Fatalf("窗口未正确往返: %+v", got.Windows)
	}
	if !got.FetchedAt.Equal(first.FetchedAt) {
		t.Fatalf("抓取时间未正确往返: %v vs %v", got.FetchedAt, first.FetchedAt)
	}
	if _, ok := got.Raw["/alpha/whoami"]; !ok {
		t.Fatalf("原始响应未正确往返: %+v", got.Raw)
	}

	// 覆盖写：同一个供应商只保留最近一份
	second := first
	second.PlanName = "Pro"
	second.FetchedAt = time.Now().UTC().Truncate(time.Millisecond)
	if err := st.SaveProviderUsage(second); err != nil {
		t.Fatalf("覆盖保存快照失败: %v", err)
	}
	all, err := st.ListProviderUsage()
	if err != nil {
		t.Fatalf("列出快照失败: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("同一个供应商应只保留一份快照，实际 %d 份", len(all))
	}
	if all[p.ID].PlanName != "Pro" {
		t.Fatalf("覆盖写未生效: %+v", all[p.ID])
	}

	// 删除供应商时快照跟着走
	if err := st.DeleteProvider(p.ID); err != nil {
		t.Fatalf("删除供应商失败: %v", err)
	}
	if all, err = st.ListProviderUsage(); err != nil || len(all) != 0 {
		t.Fatalf("删除供应商后不该残留快照: %+v err=%v", all, err)
	}
}

func TestProviderNameUniqueness(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.CreateProvider(newProvider("dup")); err != nil {
		t.Fatalf("首次创建失败: %v", err)
	}
	_, err := st.CreateProvider(newProvider("dup"))
	if err == nil {
		t.Fatal("重名短名应被拒绝")
	}
	if !contains(err.Error(), "已存在") {
		t.Errorf("错误信息应说明重名，实际 %v", err)
	}
}

func TestActiveProviderFallback(t *testing.T) {
	st := newTestStore(t)
	a, _ := st.CreateProvider(newProvider("a"))
	b, _ := st.CreateProvider(newProvider("b"))

	if err := st.ActivateProvider(b.ID); err != nil {
		t.Fatalf("切换生效供应商失败: %v", err)
	}
	active, err := st.GetActiveProvider()
	if err != nil {
		t.Fatalf("读取生效供应商失败: %v", err)
	}
	if active.ID != b.ID {
		t.Errorf("生效供应商应为 b，实际 %s", active.Name)
	}

	// 删掉生效的那个之后，应自动落到剩余的启用供应商上，而不是报错。
	if err := st.DeleteProvider(b.ID); err != nil {
		t.Fatalf("删除供应商失败: %v", err)
	}
	active, err = st.GetActiveProvider()
	if err != nil {
		t.Fatalf("删除后应能回落到其它供应商: %v", err)
	}
	if active.ID != a.ID {
		t.Errorf("应回落到 a，实际 %s", active.Name)
	}
}

func TestProviderRequiresName(t *testing.T) {
	st := newTestStore(t)
	p := newProvider("")
	if _, err := st.CreateProvider(p); err == nil {
		t.Error("缺少短名时应创建失败")
	}
}

func TestGetActiveProviderWhenEmpty(t *testing.T) {
	st := newTestStore(t)
	if _, err := st.GetActiveProvider(); err != ErrNotFound {
		t.Errorf("没有供应商时应返回 NotFound，实际 %v", err)
	}
}

// ---------- 日志与统计 ----------

func insertLog(t *testing.T, st *Store, mutate func(*model.RequestLog)) int64 {
	t.Helper()
	entry := model.RequestLog{
		TSStart:          time.Now(),
		TSEnd:            time.Now(),
		ProviderID:       1,
		ProviderName:     "上游",
		Model:            "gpt-4o",
		APIFormat:        "openai",
		HTTPStatus:       200,
		Success:          true,
		PromptTokens:     100,
		CachedTokens:     40,
		CompletionTokens: 50,
		TotalTokens:      150,
		TTFTMs:           200,
		TotalMs:          2000,
		TPS:              27.8,
		Modifications:    []string{},
	}
	if mutate != nil {
		mutate(&entry)
	}
	id, err := st.InsertLog(&entry)
	if err != nil {
		t.Fatalf("写入日志失败: %v", err)
	}
	return id
}

func TestLogInsertAndGet(t *testing.T) {
	st := newTestStore(t)
	id := insertLog(t, st, func(l *model.RequestLog) {
		l.Modifications = []string{"usage_inject", "prompt_append"}
		l.RequestModified = true
		l.ReqBody = `{"a":1}`
		l.RespBody = `{"b":2}`
	})

	got, err := st.GetLog(id)
	if err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	if !got.Success || got.PromptTokens != 100 || got.CachedTokens != 40 {
		t.Errorf("字段未正确往返: %+v", got)
	}
	if len(got.Modifications) != 2 || got.Modifications[1] != "prompt_append" {
		t.Errorf("改写清单未正确往返: %+v", got.Modifications)
	}
	if !got.RequestModified {
		t.Error("改写标记未正确往返")
	}
	if got.RespBody != `{"b":2}` {
		t.Errorf("响应报文未正确往返: %q", got.RespBody)
	}
	// 时间列必须能被解析（SQLite 只认 3 位毫秒，纳秒会被拒）。
	if got.TSStart.IsZero() || !got.TSStart.Equal(got.TSStart.UTC()) {
		t.Errorf("时间列往返异常: %v", got.TSStart)
	}
}

func TestLogListDoesNotCarryBodies(t *testing.T) {
	st := newTestStore(t)
	insertLog(t, st, func(l *model.RequestLog) { l.RespBody = "很大的响应体" })

	logs, total, err := st.ListLogs(model.LogQuery{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("查询日志失败: %v", err)
	}
	if total != 1 || len(logs) != 1 {
		t.Fatalf("应查到 1 条日志，实际 total=%d len=%d", total, len(logs))
	}
	// 列表接口刻意不带报文，避免一次拉回几十兆。
	if logs[0].RespBody != "" {
		t.Error("列表接口不应返回响应报文正文")
	}
}

func TestLogFiltering(t *testing.T) {
	st := newTestStore(t)
	insertLog(t, st, func(l *model.RequestLog) { l.Model = "gpt-4o" })
	insertLog(t, st, func(l *model.RequestLog) {
		l.Model = "claude-3"
		l.ProviderID = 2
		l.Success = false
		l.HTTPStatus = 429
	})
	insertLog(t, st, func(l *model.RequestLog) {
		l.Model = "claude-3"
		l.ProviderID = 2
		l.Success = false
		l.Cancelled = true
	})
	old := time.Now().AddDate(0, 0, -10)
	insertLog(t, st, func(l *model.RequestLog) { l.Model = "old"; l.TSStart = old; l.TSEnd = old })

	cases := []struct {
		name  string
		query model.LogQuery
		want  int64
	}{
		{"按模型", model.LogQuery{Model: "claude-3"}, 2},
		{"按供应商", model.LogQuery{ProviderID: 2}, 2},
		{"按失败状态", model.LogQuery{Status: "failed"}, 1},
		{"按取消状态", model.LogQuery{Status: "cancelled"}, 1},
		{"按成功状态", model.LogQuery{Status: "success"}, 2},
		{"按时间范围", model.LogQuery{From: time.Now().AddDate(0, 0, -1)}, 3},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, total, err := st.ListLogs(c.query)
			if err != nil {
				t.Fatalf("查询失败: %v", err)
			}
			if total != c.want {
				t.Errorf("期望 %d 条，实际 %d", c.want, total)
			}
		})
	}
}

func TestLogPagination(t *testing.T) {
	st := newTestStore(t)
	for i := 0; i < 25; i++ {
		insertLog(t, st, nil)
	}

	page1, total, err := st.ListLogs(model.LogQuery{Page: 1, PageSize: 10})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if total != 25 || len(page1) != 10 {
		t.Fatalf("分页结果错误: total=%d len=%d", total, len(page1))
	}
	page3, _, err := st.ListLogs(model.LogQuery{Page: 3, PageSize: 10})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(page3) != 5 {
		t.Errorf("第 3 页应为 5 条，实际 %d", len(page3))
	}
	// 倒序：最新的在前。
	if page1[0].ID <= page1[9].ID {
		t.Error("日志应按时间倒序返回")
	}
}

func TestDeleteLogsRequiresFilter(t *testing.T) {
	st := newTestStore(t)
	insertLog(t, st, nil)

	// 无条件删除必须被拒绝，防止前端误调用清空全部数据。
	if _, err := st.DeleteLogs(model.LogQuery{}); err == nil {
		t.Error("无条件删除应被拒绝")
	}
	n, err := st.DeleteLogs(model.LogQuery{Model: "gpt-4o"})
	if err != nil {
		t.Fatalf("条件删除失败: %v", err)
	}
	if n != 1 {
		t.Errorf("应删除 1 条，实际 %d", n)
	}
}

func TestCleanupLogs(t *testing.T) {
	st := newTestStore(t)
	insertLog(t, st, func(l *model.RequestLog) {
		l.TSStart = time.Now().AddDate(0, 0, -40)
		l.TSEnd = l.TSStart
	})
	insertLog(t, st, nil)

	deleted, err := st.CleanupLogs(30, 0)
	if err != nil {
		t.Fatalf("按时间清理失败: %v", err)
	}
	if deleted != 1 {
		t.Errorf("应清理 1 条过期日志，实际 %d", deleted)
	}

	// 条数限制：保留最新的 2 条。
	for i := 0; i < 5; i++ {
		insertLog(t, st, nil)
	}
	if _, err := st.CleanupLogs(0, 2); err != nil {
		t.Fatalf("按条数清理失败: %v", err)
	}
	_, total, err := st.ListLogs(model.LogQuery{Page: 1, PageSize: 100})
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if total != 2 {
		t.Errorf("应保留 2 条，实际 %d", total)
	}
}

func TestStatsAggregation(t *testing.T) {
	st := newTestStore(t)
	base := time.Now().Add(-2 * time.Hour)
	for i := 0; i < 3; i++ {
		insertLog(t, st, func(l *model.RequestLog) {
			l.TSStart = base.Add(time.Duration(i) * time.Minute)
			l.TSEnd = l.TSStart
			l.Model = "gpt-4o"
		})
	}
	insertLog(t, st, func(l *model.RequestLog) {
		l.TSStart = base.Add(90 * time.Minute)
		l.TSEnd = l.TSStart
		l.Model = "claude-3"
		l.ProviderID = 2
		l.ProviderName = "上游B"
		l.Success = false
		l.HTTPStatus = 500
		l.CompletionTokens = 10
		l.PromptTokens = 20
		l.CachedTokens = 0 // 这一条没有命中缓存，用来对照命中率的计算
		l.TotalTokens = 30
	})

	ov, err := st.Overview(base.Add(-time.Minute), time.Now())
	if err != nil {
		t.Fatalf("聚合总览失败: %v", err)
	}
	if ov.TotalRequests != 4 || ov.SuccessRequests != 3 || ov.FailedRequests != 1 {
		t.Errorf("请求数聚合错误: %+v", ov)
	}
	if ov.SuccessRate < 74.9 || ov.SuccessRate > 75.1 {
		t.Errorf("成功率应为 75%%，实际 %v", ov.SuccessRate)
	}
	if ov.PromptTokens != 3*100+20 || ov.CompletionTokens != 3*50+10 {
		t.Errorf("token 汇总错误: %+v", ov)
	}
	// 缓存命中率 = 命中缓存 / 输入总量：三条各 40/100，一条 0/20。
	if want := float64(3*40) / float64(3*100+20) * 100; math.Abs(ov.CacheHitRate-want) > 0.01 {
		t.Errorf("整体缓存命中率应为 %.2f%%，实际 %.2f%%", want, ov.CacheHitRate)
	}
	if ov.AvgTTFTMs <= 0 || ov.AvgTPS <= 0 {
		t.Errorf("性能均值错误: %+v", ov)
	}

	// 时间分桶
	points, err := st.TimeSeries(base.Add(-time.Minute), time.Now(), BucketHour)
	if err != nil {
		t.Fatalf("聚合趋势失败: %v", err)
	}
	if len(points) != 2 {
		t.Fatalf("应分成 2 个时间桶，实际 %d: %+v", len(points), points)
	}
	if points[0].Requests != 3 || points[1].Requests != 1 {
		t.Errorf("分桶计数错误: %+v", points)
	}
	if points[1].Failed != 1 {
		t.Errorf("分桶失败数错误: %+v", points[1])
	}
	// 第一个桶全部命中 40/100，第二个桶没有命中。
	if math.Abs(points[0].CacheHitRate-40) > 0.01 {
		t.Errorf("第一个桶的缓存命中率应为 40%%，实际 %.2f%%", points[0].CacheHitRate)
	}
	if points[1].CacheHitRate != 0 {
		t.Errorf("第二个桶的缓存命中率应为 0，实际 %.2f%%", points[1].CacheHitRate)
	}

	byModel, err := st.ByModel(base.Add(-time.Minute), time.Now(), 10)
	if err != nil {
		t.Fatalf("按模型聚合失败: %v", err)
	}
	if len(byModel) != 2 || byModel[0].Model != "gpt-4o" || byModel[0].Requests != 3 {
		t.Errorf("模型排行应按请求数降序: %+v", byModel)
	}
	if math.Abs(byModel[0].CacheHitRate-40) > 0.01 {
		t.Errorf("模型维度缓存命中率应为 40%%，实际 %.2f%%", byModel[0].CacheHitRate)
	}

	byProvider, err := st.ByProvider(base.Add(-time.Minute), time.Now(), 10)
	if err != nil {
		t.Fatalf("按供应商聚合失败: %v", err)
	}
	if len(byProvider) != 2 {
		t.Errorf("应有 2 个供应商分组，实际 %+v", byProvider)
	}
	for _, row := range byProvider {
		switch row.ProviderID {
		case 1:
			if math.Abs(row.CacheHitRate-40) > 0.01 {
				t.Errorf("供应商 1 的缓存命中率应为 40%%，实际 %.2f%%", row.CacheHitRate)
			}
		case 2:
			if row.CacheHitRate != 0 {
				t.Errorf("供应商 2 的缓存命中率应为 0，实际 %.2f%%", row.CacheHitRate)
			}
		}
	}

	models, err := st.DistinctModels(10)
	if err != nil {
		t.Fatalf("查询模型列表失败: %v", err)
	}
	if len(models) != 2 {
		t.Errorf("应有 2 个不同模型，实际 %+v", models)
	}
}

func TestOverviewOnEmptyData(t *testing.T) {
	st := newTestStore(t)
	ov, err := st.Overview(time.Time{}, time.Now())
	if err != nil {
		t.Fatalf("空数据聚合应正常返回: %v", err)
	}
	if ov.TotalRequests != 0 || ov.SuccessRate != 0 {
		t.Errorf("空数据应返回零值: %+v", ov)
	}
}

func TestNormalizeProviderName(t *testing.T) {
	if got := NormalizeProviderName("  MyProvider "); got != "myprovider" {
		t.Errorf("短名应规整为小写无空格，实际 %q", got)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
