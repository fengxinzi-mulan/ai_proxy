package proxy

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"ai_proxy/internal/model"
	"ai_proxy/internal/store"
)

// ---------- 假上游 ----------
//
// 下列响应体照抄 Command Code 真实接口（2026-09 实测），
// 字段名与嵌套层级保持原样、只把账号标识脱敏。
// 故意不用「自己编一个合理结构」的写法：这几个接口没有公开契约，
// 测试的价值就在于钉住真实字段名，编出来的结构只会一起编错。

const (
	ccWhoamiFixture = `{"success":true,"user":{"id":"u-1","name":"Test User",
		"email":"t***@example.com","userName":"tester"},"org":null}`

	ccCreditsFixture = `{"credits":{"belowThreshold":false,"creditThreshold":0,
		"monthlyCredits":44.7338348663,"purchasedCredits":0,"freeCredits":0},
		"windowLimits":{"limited":true,"exceeded":null,
			"fiveHour":{"used":0.202380829,"cap":14,"exceeded":false,"resetAt":1790142088195},
			"weekly":{"used":25.2661651337,"cap":35,"exceeded":false,"resetAt":1790150388772}}}`

	ccSubscriptionsFixture = `{"success":true,"data":{"id":"sub_1","status":"active",
		"userId":"u-1","orgId":null,"createdAt":"2026-09-15T08:09:48.000Z",
		"priceId":"price_x","quantity":1,"cancelAtPeriodEnd":false,
		"currentPeriodStart":"2026-09-16T07:50:53.000Z",
		"currentPeriodEnd":"2026-10-16T07:50:53.000Z","planId":"individual-goat"}}`

	ccSummaryFixture = `{"totalCount":10274,"totalCost":26.013961449700005,
		"averageCost":0.002532018829053923,"successRate":100,"completedCount":10274,
		"failedCount":0,"totalTokensIn":2081502006,"totalTokensOut":9090863,
		"totalTokens":2090592869,"periodBasis":"billing-period"}`
)

// ccCall 是一次收到的请求，用于断言鉴权头与作用域参数。
type ccCall struct {
	path  string
	auth  string
	query url.Values
}

// ccMock 是额度接口的假上游。字段留空表示用上面的真实样本。
type ccMock struct {
	whoami, credits, subscriptions, summary string
	// 非零则用该状态码回答对应接口
	whoamiStatus, creditsStatus, subStatus, summaryStatus int

	mu    sync.Mutex
	calls []ccCall
}

func (m *ccMock) body(field string, fallback string) string {
	if field != "" {
		return field
	}
	return fallback
}

func (m *ccMock) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.calls = append(m.calls, ccCall{
			path:  r.URL.Path,
			auth:  r.Header.Get("Authorization"),
			query: r.URL.Query(),
		})
		m.mu.Unlock()

		var status int
		var body string
		switch r.URL.Path {
		case ccPathWhoami:
			status, body = m.whoamiStatus, m.body(m.whoami, ccWhoamiFixture)
		case ccPathCredits:
			status, body = m.creditsStatus, m.body(m.credits, ccCreditsFixture)
		case ccPathSubscriptions:
			status, body = m.subStatus, m.body(m.subscriptions, ccSubscriptionsFixture)
		case ccPathSummary:
			status, body = m.summaryStatus, m.body(m.summary, ccSummaryFixture)
		default:
			// 路径拼错（例如 /provider/v1 没被剥掉）会走到这里，
			// 测试里表现为「额度不可用（HTTP 404）」。
			http.NotFound(w, r)
			return
		}
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			io.WriteString(w, body)
			return
		}
		io.WriteString(w, `{"error":"boom"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// callsFor 返回指定路径收到的所有请求；path 为空表示全部。
func (m *ccMock) callsFor(path string) []ccCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []ccCall{}
	for _, c := range m.calls {
		if path == "" || c.path == path {
			out = append(out, c)
		}
	}
	return out
}

func (m *ccMock) authHeader() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		return ""
	}
	return m.calls[0].auth
}

func (m *ccMock) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

// ---------- 测试脚手架 ----------

// addUsageProvider 建一个配了用量查询模板的供应商。
// baseURL 故意带上 /provider/v1，用来验证推导规则真的把它剥掉了。
func addUsageProvider(t *testing.T, st *store.Store, baseURL string, withKey bool) model.Provider {
	t.Helper()
	p := model.Provider{
		Name:       "cc",
		Enabled:    true,
		BaseURL:    baseURL,
		APIFormat:  model.FormatOpenAI,
		UsageQuery: model.UsageQueryConfig{Template: "commandcode"},
	}
	if withKey {
		p.Keys = []model.APIKey{{ID: "k1", Key: "sk-test", Enabled: true}}
	}
	created, err := st.CreateProvider(p)
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}
	return created
}

func runQueryUsage(t *testing.T, srv *Server, st *store.Store, prov model.Provider) model.UsageSnapshot {
	t.Helper()
	settings, err := st.GetSettings()
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.QueryUsage(ctx, prov, settings)
}

// ---------- 正常路径 ----------

func TestQueryUsageCommandCode(t *testing.T) {
	mock := &ccMock{}
	upstream := mock.start(t)

	srv, st := newTestServer(t)
	prov := addUsageProvider(t, st, upstream.URL+"/provider/v1", true)

	snap := runQueryUsage(t, srv, st, prov)

	if !snap.OK {
		t.Fatalf("期望 ok，得到 error=%q warnings=%v", snap.Error, snap.Warnings)
	}
	if snap.Error != "" {
		t.Fatalf("不该有错误: %s", snap.Error)
	}
	if len(snap.Warnings) != 0 {
		t.Fatalf("不该有降级警告: %v", snap.Warnings)
	}
	if snap.Template != "commandcode" || snap.TemplateName != "Command Code" {
		t.Fatalf("模板元信息不对: %q / %q", snap.Template, snap.TemplateName)
	}

	// 鉴权头走的是供应商自己的认证配置。
	if got := mock.authHeader(); got != "Bearer sk-test" {
		t.Fatalf("鉴权头不对: %q", got)
	}
	// 四个接口都该被调到 —— 如果 /provider/v1 没被剥掉，路径会 404，
	// 这里就会因为额度不可用而失败。
	if n := mock.callCount(); n != 4 {
		t.Fatalf("期望 4 次调用，实际 %d 次", n)
	}

	if snap.Account != "tester" {
		t.Fatalf("账号名不对: %q", snap.Account)
	}
	if snap.PlanID != "individual-goat" || snap.PlanName != "GOAT" {
		t.Fatalf("计划不对: %q / %q", snap.PlanID, snap.PlanName)
	}
	if snap.Status != "active" {
		t.Fatalf("订阅状态不对: %q", snap.Status)
	}
	if snap.PeriodStart == nil || snap.PeriodStart.UTC().Format(time.RFC3339) != "2026-09-16T07:50:53Z" {
		t.Fatalf("周期起点不对: %v", snap.PeriodStart)
	}
	if snap.PeriodEnd == nil || snap.PeriodEnd.UTC().Format(time.RFC3339) != "2026-10-16T07:50:53Z" {
		t.Fatalf("周期终点不对: %v", snap.PeriodEnd)
	}

	// 余额：上游只给了 monthlyCredits 有值，其余为 0，三项都该出现。
	if len(snap.Balances) != 3 {
		t.Fatalf("期望 3 项余额，实际 %d: %+v", len(snap.Balances), snap.Balances)
	}
	if snap.Balances[0].Label != "月度额度" || snap.Balances[0].Amount < 44.73 || snap.Balances[0].Amount > 44.74 {
		t.Fatalf("月度额度不对: %+v", snap.Balances[0])
	}

	// 窗口：5 小时、每周来自上游，月度是推导出来的（上游只给剩余额度）。
	if len(snap.Windows) != 3 {
		t.Fatalf("期望 3 个窗口，实际 %d: %+v", len(snap.Windows), snap.Windows)
	}
	five := snap.Windows[0]
	if five.Label != "5 小时" || five.Cap != 14 {
		t.Fatalf("5 小时窗口不对: %+v", five)
	}
	if five.Percent < 1.4 || five.Percent > 1.5 {
		t.Fatalf("5 小时百分比不对: %v", five.Percent)
	}
	if five.ResetAt == nil {
		t.Fatalf("5 小时窗口缺少重置时间")
	}
	// resetAt 是毫秒时间戳，不是秒。按秒解会跑到 1970 年。
	if five.ResetAt.Year() < 2020 {
		t.Fatalf("重置时间解错了（毫秒当秒？）: %v", five.ResetAt)
	}
	weekly := snap.Windows[1]
	if weekly.Label != "每周" || weekly.Cap != 35 {
		t.Fatalf("每周窗口不对: %+v", weekly)
	}
	if weekly.Percent < 72.1 || weekly.Percent > 72.2 {
		t.Fatalf("每周百分比不对: %v", weekly.Percent)
	}

	// 月度：上限 = 月度剩余 44.7338348663 + 周期已花 26.013961449700005
	monthly := snap.Windows[2]
	if monthly.Label != "本月" {
		t.Fatalf("第三个窗口应是本月: %+v", monthly)
	}
	wantCap := 44.7338348663 + 26.013961449700005
	if math.Abs(monthly.Cap-wantCap) > 1e-6 {
		t.Fatalf("月度上限推导错误: 期望 %v，实际 %v", wantCap, monthly.Cap)
	}
	if math.Abs(monthly.Used-26.013961449700005) > 1e-6 {
		t.Fatalf("月度已用应取周期花费: %v", monthly.Used)
	}
	if monthly.Percent < 36.7 || monthly.Percent > 36.8 {
		t.Fatalf("月度百分比不对: %v", monthly.Percent)
	}
	if monthly.ResetAt == nil || monthly.ResetAt.UTC().Format(time.RFC3339) != "2026-10-16T07:50:53Z" {
		t.Fatalf("月度窗口的重置时间应取计费周期末: %v", monthly.ResetAt)
	}
	// 推导出来的数字必须说明来源，否则用户没法判断该不该信
	if monthly.Hint == "" {
		t.Fatal("推导出来的月度窗口必须带 Hint 说明")
	}
	// 上游直接给的窗口不该带 Hint
	if five.Hint != "" || weekly.Hint != "" {
		t.Fatalf("上游直出的窗口不该有 Hint: %q / %q", five.Hint, weekly.Hint)
	}

	// 周期统计
	labels := map[string]model.UsageStat{}
	for _, s := range snap.Period {
		labels[s.Label] = s
	}
	if got := labels["周期内花费"]; got.Unit != model.UnitUSD || got.Value < 26.01 || got.Value > 26.02 {
		t.Fatalf("周期内花费不对: %+v", got)
	}
	if got := labels["周期内请求数"]; got.Value != 10274 {
		t.Fatalf("周期内请求数不对: %+v", got)
	}
	if got := labels["输入 tokens"]; got.Value != 2081502006 {
		t.Fatalf("输入 tokens 不对: %+v", got)
	}

	// 原始响应要留下来供排查
	if len(snap.Raw) != 4 {
		t.Fatalf("期望保留 4 份原始响应，实际 %d", len(snap.Raw))
	}
	if len(snap.UsedURLs) != 4 {
		t.Fatalf("期望记录 4 个地址，实际 %d", len(snap.UsedURLs))
	}
	for _, u := range snap.UsedURLs {
		if strings.Contains(u, "/provider/v1") {
			t.Fatalf("地址里不该残留 /provider/v1: %s", u)
		}
	}
}

// ---------- 降级 ----------

func TestQueryUsageCreditsUnavailable(t *testing.T) {
	mock := &ccMock{creditsStatus: http.StatusInternalServerError}
	upstream := mock.start(t)

	srv, st := newTestServer(t)
	prov := addUsageProvider(t, st, upstream.URL+"/provider/v1", true)

	snap := runQueryUsage(t, srv, st, prov)

	// 额度是这次查询的答案，读不到就算失败 —— 但其余部分照常展示。
	if snap.OK {
		t.Fatalf("额度不可用时不该报 ok")
	}
	if len(snap.Balances) != 0 {
		t.Fatalf("额度不可用时不该有余额项: %+v", snap.Balances)
	}
	if snap.PlanName != "GOAT" {
		t.Fatalf("计划信息应不受影响: %q", snap.PlanName)
	}
	if len(snap.Period) == 0 {
		t.Fatalf("周期统计应不受影响")
	}
	if !hasWarning(snap.Warnings, "额度不可用") {
		t.Fatalf("缺少额度降级说明: %v", snap.Warnings)
	}
}

// 最关键的行为：上游改了结构时，绝不能把「读不到」渲染成「额度为 0」。
func TestQueryUsageSchemaDriftIsNotZero(t *testing.T) {
	mock := &ccMock{credits: `{"creditsV2":{"remaining":12.5},"unexpected":true}`}
	upstream := mock.start(t)

	srv, st := newTestServer(t)
	prov := addUsageProvider(t, st, upstream.URL+"/provider/v1", true)

	snap := runQueryUsage(t, srv, st, prov)

	if snap.OK {
		t.Fatalf("结构无法识别时不该报 ok")
	}
	if len(snap.Balances) != 0 {
		t.Fatalf("结构无法识别时不该编出余额: %+v", snap.Balances)
	}
	if len(snap.Windows) != 0 {
		t.Fatalf("结构无法识别时不该编出窗口: %+v", snap.Windows)
	}
	if !hasWarning(snap.Warnings, "无法识别") {
		t.Fatalf("缺少结构降级说明: %v", snap.Warnings)
	}
	// 原始响应必须留下来，否则用户没法排查上游到底回了什么。
	if _, ok := snap.Raw[ccPathCredits]; !ok {
		t.Fatalf("原始响应里应保留 credits 的返回: %+v", snap.Raw)
	}
}

func TestQueryUsageAuthRejected(t *testing.T) {
	mock := &ccMock{whoamiStatus: http.StatusUnauthorized}
	upstream := mock.start(t)

	srv, st := newTestServer(t)
	prov := addUsageProvider(t, st, upstream.URL+"/provider/v1", true)

	snap := runQueryUsage(t, srv, st, prov)

	if snap.OK || snap.Error == "" {
		t.Fatalf("密钥被拒时应整体失败: ok=%v error=%q", snap.OK, snap.Error)
	}
	if !strings.Contains(snap.Error, "密钥被拒绝") {
		t.Fatalf("错误信息应指明是密钥问题: %q", snap.Error)
	}
	// whoami 就被拒了，不该再用同一个坏密钥去发另外三个请求。
	if n := mock.callCount(); n != 1 {
		t.Fatalf("鉴权失败后不该继续发请求，实际发了 %d 次", n)
	}
}

// ---------- 作用域参数 ----------

func TestQueryUsageOrgScoped(t *testing.T) {
	mock := &ccMock{whoami: `{"success":true,"org":{"id":"org-42","login":"acme"},
		"user":{"id":"u-1","userName":"tester"}}`}
	upstream := mock.start(t)

	srv, st := newTestServer(t)
	prov := addUsageProvider(t, st, upstream.URL+"/provider/v1", true)

	snap := runQueryUsage(t, srv, st, prov)

	if snap.Account != "acme" {
		t.Fatalf("有 org 时应优先用组织名: %q", snap.Account)
	}
	// 除了 whoami 自己，另外三个接口都必须带上组织作用域。
	for _, path := range []string{ccPathCredits, ccPathSubscriptions, ccPathSummary} {
		calls := mock.callsFor(path)
		if len(calls) != 1 {
			t.Fatalf("%s 应被调用 1 次，实际 %d 次", path, len(calls))
		}
		if got := calls[0].query.Get("orgId"); got != "org-42" {
			t.Fatalf("%s 缺少 orgId 作用域，实际 %q（%v）", path, got, calls[0].query)
		}
	}
}

// 个人账号下 org 是 null —— 此时必须完全不传 orgId，而不是传空串。
func TestQueryUsageOmitsOrgIdWhenNull(t *testing.T) {
	mock := &ccMock{}
	upstream := mock.start(t)

	srv, st := newTestServer(t)
	prov := addUsageProvider(t, st, upstream.URL+"/provider/v1", true)

	runQueryUsage(t, srv, st, prov)

	for _, c := range mock.callsFor("") {
		if _, present := c.query["orgId"]; present {
			t.Fatalf("org 为 null 时 %s 不该出现 orgId 参数: %v", c.path, c.query)
		}
	}
}

// ---------- 配置与推导 ----------

func TestQueryUsageWithoutTemplate(t *testing.T) {
	srv, st := newTestServer(t)
	prov := addUsageProvider(t, st, "https://api.commandcode.ai/provider/v1", true)
	prov.UsageQuery.Template = ""

	snap := runQueryUsage(t, srv, st, prov)
	if snap.OK || !strings.Contains(snap.Error, "未配置") {
		t.Fatalf("未配置模板时应给出明确提示: ok=%v error=%q", snap.OK, snap.Error)
	}
}

func TestQueryUsageUnknownTemplate(t *testing.T) {
	srv, st := newTestServer(t)
	prov := addUsageProvider(t, st, "https://api.commandcode.ai/provider/v1", true)
	prov.UsageQuery.Template = "not-a-template"

	snap := runQueryUsage(t, srv, st, prov)
	if snap.OK || !strings.Contains(snap.Error, "commandcode") {
		t.Fatalf("未知模板应把可用模板列出来: %q", snap.Error)
	}
}

func TestQueryUsageWithoutKey(t *testing.T) {
	srv, st := newTestServer(t)
	prov := addUsageProvider(t, st, "https://api.commandcode.ai/provider/v1", false)

	snap := runQueryUsage(t, srv, st, prov)
	if snap.OK || !strings.Contains(snap.Error, "密钥") {
		t.Fatalf("无密钥时应说明用量查询需要账号密钥: ok=%v error=%q", snap.OK, snap.Error)
	}
}

func TestDeriveUsageBaseURL(t *testing.T) {
	cases := []struct {
		name     string
		baseURL  string
		override string
		want     string
		wantErr  bool
	}{
		{"剥掉 provider/v1", "https://api.commandcode.ai/provider/v1", "", "https://api.commandcode.ai", false},
		{"末尾斜杠", "https://api.commandcode.ai/provider/v1/", "", "https://api.commandcode.ai", false},
		{"剥掉 provider", "https://api.commandcode.ai/provider", "", "https://api.commandcode.ai", false},
		{"剥掉 v1", "https://api.openai.com/v1", "", "https://api.openai.com", false},
		{"裸域名原样", "https://api.commandcode.ai", "", "https://api.commandcode.ai", false},
		{"只保留路径前缀", "https://gw.corp/ai/provider/v1", "", "https://gw.corp/ai", false},
		{"带端口", "http://127.0.0.1:9911/provider/v1", "", "http://127.0.0.1:9911", false},
		{"无协议头补 https", "api.commandcode.ai/provider/v1", "", "https://api.commandcode.ai", false},
		{"显式覆盖优先且不再剥", "https://api.commandcode.ai/provider/v1", "https://custom.example.com/root", "https://custom.example.com/root", false},
		{"显式覆盖去尾斜杠", "https://api.commandcode.ai/provider/v1", "https://custom.example.com/root/", "https://custom.example.com/root", false},
		{"空地址报错", "", "", "", true},
		{"非法地址报错", "://nope", "", "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := model.Provider{BaseURL: c.baseURL}
			p.UsageQuery.BaseURL = c.override
			got, err := deriveUsageBaseURL(p)
			if c.wantErr {
				if err == nil {
					t.Fatalf("期望报错，得到 %q", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("不该报错: %v", err)
			}
			if got != c.want {
				t.Fatalf("期望 %q，得到 %q", c.want, got)
			}
		})
	}
}

// ---------- 模板元数据 ----------

func TestUsageTemplatesRegistered(t *testing.T) {
	list := UsageTemplates()
	if len(list) == 0 {
		t.Fatal("模板注册表为空")
	}
	seen := map[string]bool{}
	for _, tpl := range list {
		if tpl.ID == "" || tpl.Name == "" {
			t.Fatalf("模板元数据不完整: %+v", tpl)
		}
		if seen[tpl.ID] {
			t.Fatalf("模板 id 重复: %s", tpl.ID)
		}
		seen[tpl.ID] = true
	}
	for _, want := range []string{"commandcode", "deepseek", "opencode"} {
		if !seen[want] {
			t.Errorf("缺少 %s 模板", want)
		}
	}
}

// 月度窗口是推导出来的，依赖周期花费。周期统计读不到时宁可少一格，
// 也不能拿一个半截的比值去展示「月度用了多少」。
func TestQueryUsageNoMonthlyWindowWithoutSummary(t *testing.T) {
	mock := &ccMock{summaryStatus: http.StatusInternalServerError}
	upstream := mock.start(t)

	srv, st := newTestServer(t)
	prov := addUsageProvider(t, st, upstream.URL+"/provider/v1", true)

	snap := runQueryUsage(t, srv, st, prov)

	if !snap.OK {
		t.Fatalf("额度本身读到了，不该报整体失败: %v", snap.Warnings)
	}
	if len(snap.Windows) != 2 {
		t.Fatalf("周期统计不可用时应只剩上游直出的 2 个窗口，实际 %d: %+v",
			len(snap.Windows), snap.Windows)
	}
	for _, w := range snap.Windows {
		if w.Label == "本月" {
			t.Fatalf("周期统计不可用时不该出现推导出来的月度窗口: %+v", w)
		}
	}
	if !hasWarning(snap.Warnings, "周期统计不可用") {
		t.Fatalf("缺少周期统计的降级说明: %v", snap.Warnings)
	}
}

// 额度读不到时同样不该有月度窗口（连剩余额度都没有，何谈上限）。
func TestQueryUsageNoMonthlyWindowWithoutCredits(t *testing.T) {
	mock := &ccMock{credits: `{"creditsV2":{"remaining":12.5}}`}
	upstream := mock.start(t)

	srv, st := newTestServer(t)
	prov := addUsageProvider(t, st, upstream.URL+"/provider/v1", true)

	snap := runQueryUsage(t, srv, st, prov)

	if len(snap.Windows) != 0 {
		t.Fatalf("额度读不到时不该有任何窗口: %+v", snap.Windows)
	}
	// 周期统计本身是读到了的
	if len(snap.Period) == 0 {
		t.Fatal("周期统计应不受影响")
	}
}

func hasWarning(warnings []string, substr string) bool {
	for _, w := range warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// ---------- DeepSeek 模板 ----------
//
// 响应体照抄官方文档示例（api-docs.deepseek.com/zh-cn/api/get-user-balance）。
// 这是公开接口，但仍照抄原文而不自编结构：字段名（下划线风格）与字符串金额都是接口的一部分。

const dsBalanceFixture = `{"is_available":true,"balance_infos":[
	{"currency":"CNY","total_balance":"110.00","granted_balance":"10.00","topped_up_balance":"100.00"}]}`

// simpleUsageMock 是一个「只回一个固定响应」的假上游，用于 deepseek / opencode 这类单接口模板。
type simpleUsageMock struct {
	path   string
	status int
	body   string

	mu     sync.Mutex
	calls  []ccCall
	prefix string // 收到的路径前缀，用于断言推导出来的地址
}

func (m *simpleUsageMock) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.mu.Lock()
		m.calls = append(m.calls, ccCall{path: r.URL.Path, auth: r.Header.Get("Authorization"), query: r.URL.Query()})
		m.mu.Unlock()

		if r.URL.Path != m.path {
			http.NotFound(w, r)
			return
		}
		status := m.status
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		if status == http.StatusOK {
			io.WriteString(w, m.body)
			return
		}
		io.WriteString(w, `{"error":"boom"}`)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (m *simpleUsageMock) callCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.calls)
}

func (m *simpleUsageMock) authHeader() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.calls) == 0 {
		return ""
	}
	return m.calls[0].auth
}

// addTemplateProvider 建一个指定模板的供应商。baseURL 原样传入，由各模板自行推导。
func addTemplateProvider(t *testing.T, st *store.Store, baseURL, template string) model.Provider {
	t.Helper()
	p := model.Provider{
		Name:       "tpl-" + template,
		Enabled:    true,
		BaseURL:    baseURL,
		APIFormat:  model.FormatOpenAI,
		Keys:       []model.APIKey{{ID: "k1", Key: "sk-test", Enabled: true}},
		UsageQuery: model.UsageQueryConfig{Template: template},
	}
	created, err := st.CreateProvider(p)
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}
	return created
}

func TestQueryUsageDeepSeek(t *testing.T) {
	mock := &simpleUsageMock{path: dsPathBalance, body: dsBalanceFixture}
	upstream := mock.start(t)

	srv, st := newTestServer(t)
	// BaseURL 带 /v1，模板应剥掉它再拼 /user/balance。
	prov := addTemplateProvider(t, st, upstream.URL+"/v1", "deepseek")

	snap := runQueryUsage(t, srv, st, prov)
	if !snap.OK {
		t.Fatalf("查询应成功，error=%q warnings=%v", snap.Error, snap.Warnings)
	}
	if snap.TemplateName != "DeepSeek" {
		t.Errorf("模板名不正确: %q", snap.TemplateName)
	}
	if snap.Status != "可用" {
		t.Errorf("is_available=true 时应为「可用」，实际 %q", snap.Status)
	}
	if len(snap.Balances) != 3 {
		t.Fatalf("应有三条余额（总/充值/赠金），实际 %+v", snap.Balances)
	}
	// 金额与币种都要对：人民币余额不能被当成美元。
	byLabel := map[string]model.UsageBalance{}
	for _, b := range snap.Balances {
		byLabel[b.Label] = b
	}
	total := byLabel["总余额"]
	if total.Amount != 110 || total.Currency != "CNY" {
		t.Errorf("总余额应为 110 CNY，实际 %+v", total)
	}
	// 总余额是「汇总余额」，它已经包含充值与赠金。界面据此判断不该再加一遍分项，
	// 所以这个标记是必填的 —— 丢了它「剩余额度」就会变成两倍。
	if !total.Total {
		t.Error("总余额应标记为汇总余额（total=true）")
	}
	if b := byLabel["充值余额"]; b.Amount != 100 || b.Total {
		t.Errorf("充值余额应为 100 且非汇总，实际 %+v", b)
	}
	if b := byLabel["赠金余额"]; b.Amount != 10 || b.Total {
		t.Errorf("赠金余额应为 10 且非汇总，实际 %+v", b)
	}
	// 样本本身自洽：总余额 = 充值 + 赠金。
	if total.Amount != byLabel["充值余额"].Amount+byLabel["赠金余额"].Amount {
		t.Errorf("样本应满足 total = topped_up + granted")
	}
	if got := mock.authHeader(); got != "Bearer sk-test" {
		t.Errorf("鉴权头不正确: %q", got)
	}
}

func TestQueryUsageDeepSeekUnauthorized(t *testing.T) {
	mock := &simpleUsageMock{path: dsPathBalance, status: http.StatusUnauthorized, body: "{}"}
	upstream := mock.start(t)

	srv, st := newTestServer(t)
	prov := addTemplateProvider(t, st, upstream.URL+"/v1", "deepseek")

	snap := runQueryUsage(t, srv, st, prov)
	if snap.Error == "" {
		t.Fatal("401 时应给出整体失败原因")
	}
	if snap.OK {
		t.Error("鉴权失败时不该判为成功")
	}
}

// 金额读不出来时绝不能回落成 0。
func TestQueryUsageDeepSeekSchemaDriftIsNotZero(t *testing.T) {
	mock := &simpleUsageMock{
		path: dsPathBalance,
		body: `{"is_available":true,"balance_infos":[{"currency":"CNY","total_balance":"","granted_balance":"","topped_up_balance":""}]}`,
	}
	upstream := mock.start(t)

	srv, st := newTestServer(t)
	prov := addTemplateProvider(t, st, upstream.URL+"/v1", "deepseek")

	snap := runQueryUsage(t, srv, st, prov)
	if len(snap.Balances) != 0 {
		t.Errorf("空金额不该被解析成 0 额度，实际 %+v", snap.Balances)
	}
	if snap.OK {
		t.Error("读不到任何金额时不该判为成功")
	}
}

// ---------- OpenCode 模板 ----------
//
// 字段名照抄 openusage 文档对 GET https://opencode.ai/zen/go/v1/usage 的描述：
// usage.{rolling,weekly,monthly}，各含 percent 与 resetsAt。

const ocUsageFixture = `{"usage":{
	"rolling":{"percent":1,"resetsAt":"2026-06-06T11:36:00Z"},
	"weekly":{"percent":29.7,"resetsAt":"2026-06-11T10:15:00Z"},
	"monthly":{"percent":25,"resetsAt":"2026-07-02T10:15:00Z"}},"plan":"go"}`

func TestQueryUsageOpenCode(t *testing.T) {
	mock := &simpleUsageMock{path: "/zen/go/v1/usage", body: ocUsageFixture}
	upstream := mock.start(t)

	srv, st := newTestServer(t)
	// BaseURL 填到 /zen/v1（常见写法），模板应改用站点根 + 绝对路径。
	prov := addTemplateProvider(t, st, upstream.URL+"/zen/v1", "opencode")

	snap := runQueryUsage(t, srv, st, prov)
	if !snap.OK {
		t.Fatalf("查询应成功，error=%q warnings=%v", snap.Error, snap.Warnings)
	}
	if len(snap.Windows) != 3 {
		t.Fatalf("应有三个窗口（5 小时/每周/本月），实际 %+v", snap.Windows)
	}
	byLabel := map[string]model.UsageWindow{}
	for _, w := range snap.Windows {
		byLabel[w.Label] = w
	}
	if w := byLabel["5 小时"]; w.Percent != 1 {
		t.Errorf("5 小时窗口百分比应为 1，实际 %v", w.Percent)
	}
	if w := byLabel["每周"]; w.Percent != 29.7 {
		t.Errorf("每周窗口百分比应为 29.7，实际 %v", w.Percent)
	}
	if w := byLabel["本月"]; w.Percent != 25 {
		t.Errorf("本月窗口百分比应为 25，实际 %v", w.Percent)
	}
	// 上游只给百分比，没有金额，Cap 必须保持 0 而不是编一个数。
	for _, w := range snap.Windows {
		if w.Cap != 0 || w.Used != 0 {
			t.Errorf("%s 窗口不该有金额，实际 used=%v cap=%v", w.Label, w.Used, w.Cap)
		}
		if w.Hint == "" {
			t.Errorf("%s 窗口应说明百分比没有金额口径", w.Label)
		}
	}
	if w := byLabel["5 小时"]; w.ResetAt == nil {
		t.Error("应解析出重置时间")
	}
	if snap.PlanName == "" {
		t.Error("应给出计划名")
	}
	if got := mock.authHeader(); got != "Bearer sk-test" {
		t.Errorf("鉴权头不正确: %q", got)
	}
}

// BaseURL 换成 /zen/go/v1 也应能命中同一个绝对路径（推导基准不同但 origin 相同）。
func TestQueryUsageOpenCodeAlternateBaseURL(t *testing.T) {
	mock := &simpleUsageMock{path: "/zen/go/v1/usage", body: ocUsageFixture}
	upstream := mock.start(t)

	srv, st := newTestServer(t)
	prov := addTemplateProvider(t, st, upstream.URL+"/zen/go/v1", "opencode")

	snap := runQueryUsage(t, srv, st, prov)
	if !snap.OK || len(snap.Windows) != 3 {
		t.Fatalf("另一种 BaseURL 写法也应命中，ok=%v windows=%+v warnings=%v", snap.OK, snap.Windows, snap.Warnings)
	}
}

// resetsAt 用 epoch 秒也应能解析。
func TestQueryUsageOpenCodeEpochReset(t *testing.T) {
	body := `{"usage":{"rolling":{"percent":5,"resetsAt":1780000000}}}`
	mock := &simpleUsageMock{path: "/zen/go/v1/usage", body: body}
	upstream := mock.start(t)

	srv, st := newTestServer(t)
	prov := addTemplateProvider(t, st, upstream.URL+"/zen/v1", "opencode")

	snap := runQueryUsage(t, srv, st, prov)
	if len(snap.Windows) != 1 {
		t.Fatalf("应只有一个窗口，实际 %+v", snap.Windows)
	}
	if snap.Windows[0].ResetAt == nil {
		t.Error("epoch 秒形式的 resetsAt 应被解析")
	}
}

// 未订阅 Go（上游 403）时给出可操作的原因，而不是静默成功。
func TestQueryUsageOpenCodeNotSubscribed(t *testing.T) {
	mock := &simpleUsageMock{path: "/zen/go/v1/usage", status: http.StatusForbidden, body: "{}"}
	upstream := mock.start(t)

	srv, st := newTestServer(t)
	prov := addTemplateProvider(t, st, upstream.URL+"/zen/v1", "opencode")

	snap := runQueryUsage(t, srv, st, prov)
	if snap.Error == "" {
		t.Fatal("403 时应给出失败原因")
	}
	if !strings.Contains(snap.Error, "Go") {
		t.Errorf("失败原因应提到 Go 订阅，实际 %q", snap.Error)
	}
}

// 没有任何窗口时不要把「空」当成「成功」。
func TestQueryUsageOpenCodeEmptyUsage(t *testing.T) {
	mock := &simpleUsageMock{path: "/zen/go/v1/usage", body: `{"usage":{}}`}
	upstream := mock.start(t)

	srv, st := newTestServer(t)
	prov := addTemplateProvider(t, st, upstream.URL+"/zen/v1", "opencode")

	snap := runQueryUsage(t, srv, st, prov)
	if snap.OK {
		t.Error("没有任何窗口时不该判为成功")
	}
	if len(snap.Windows) != 0 {
		t.Errorf("不该有窗口，实际 %+v", snap.Windows)
	}
}

// deriveUsageBaseURL + originOf 的组合：把常见 BaseURL 收敛到同一个站点根。
func TestOriginOf(t *testing.T) {
	cases := []struct{ in, want string }{
		{"https://opencode.ai/zen/v1", "https://opencode.ai"},
		{"https://opencode.ai/zen/go/v1", "https://opencode.ai"},
		{"https://api.deepseek.com/v1", "https://api.deepseek.com"},
		{"://bad", ""},
	}
	for _, c := range cases {
		if got := originOf(c.in); got != c.want {
			t.Errorf("originOf(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}
}
