package proxy

// Mistral 用量模板的解析与端到端用例。
//
// 数据分两处、认证分两套（见 usagequery.go 里的说明）：
//   - 预算与订阅计划走官方 API（/v1/billing/budget、/v1/billing/subscription），认 API key；
//   - 本月限额与 token 明细还挂在 admin.mistral.ai，认账号密码登录换来的会话 token。
//
// 这里的样本按 2026-11-18 的实测响应裁剪，只保留解析关心的部分，
// 不含任何真实账号信息。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ai_proxy/internal/model"
	"ai_proxy/internal/store"
)

// mistralBudgetJSON 是官方 /v1/billing/budget 的实测结构。
const mistralBudgetJSON = `{"vibe_budget":{"usage_percentage":20.81,"initial_budget":300.0,` +
	`"currency":"USD","reset_at":"2026-12-01T00:00:00Z","payg_enabled":false},` +
	`"api_budget":{"usage_percentage":10.0,"initial_budget":30.0,"currency":"USD",` +
	`"reset_at":"2026-12-01T00:00:00Z","payg_enabled":false},` +
	`"usage_percentage":10.0,"initial_budget":30.0,"currency":"USD","reset_at":"2026-12-01T00:00:00Z"}`

// mistralSubscriptionJSON 是官方 /v1/billing/subscription 的实测结构。
const mistralSubscriptionJSON = `{"plan_name":"INDIVIDUAL","payg_enabled":false,` +
	`"vibe_payg_enabled":false,"enterprise":false}`

// mistralLimitsJSON 是 admin /api/billing/limits 的实测结构。
const mistralLimitsJSON = `{"limits":{"completion":{"no_monthly_limit":true,` +
	`"monthly_limit_reached":false,"usage":1.25,"vibe_usage":2.5,"total_usage":3.75,` +
	`"usage_limit":null},"last_payment_failure":false,"currency":"USD"}}`

// mistralUsageJSON 是 admin /api/billing/v2/usage 的实测结构（只留 token 部分）。
const mistralUsageJSON = `{"completion":{"models":{}},"chat":{"models":{}},` +
	`"vibe_code":{"completion":{"models":{"some-model::some-model":{` +
	`"input":[{"timestamp":"2026-11-14","value":1000},{"timestamp":"2026-11-15","value":500}],` +
	`"output":[{"timestamp":"2026-11-14","value":200}],` +
	`"cached":[{"timestamp":"2026-11-14","value":3000}]}}}},` +
	`"currency":"USD"}`

// ---------- 预算解析 ----------

func TestParseMistralBudget(t *testing.T) {
	windows, err := parseMistralBudget([]byte(mistralBudgetJSON))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(windows) != 2 {
		t.Fatalf("应有 2 条额度窗口，实际 %d: %+v", len(windows), windows)
	}

	api, vibe := windows[0], windows[1]
	if api.Label != "API 额度" || api.Cap != 30 {
		t.Errorf("API 额度解析不对: %+v", api)
	}
	// 已用量由「总额 × 百分比」算出来：30 × 10% = 3
	if api.Used != 3 {
		t.Errorf("API 已用量应为 3，实际 %v", api.Used)
	}
	if api.ResetAt == nil || api.ResetAt.Year() != 2026 {
		t.Errorf("重置时间没解析出来: %+v", api.ResetAt)
	}
	if !strings.Contains(api.Hint, "USD") {
		t.Errorf("提示里应带币种: %q", api.Hint)
	}

	if vibe.Label != "Vibe 额度" || vibe.Cap != 300 {
		t.Errorf("Vibe 额度解析不对: %+v", vibe)
	}
	if vibe.Percent != 20.81 {
		t.Errorf("Vibe 百分比应为 20.81，实际 %v", vibe.Percent)
	}
}

// api_budget 缺失时要退回顶层那份，不能整条额度消失。
func TestParseMistralBudgetFallsBackToTopLevel(t *testing.T) {
	body := `{"usage_percentage":50.0,"initial_budget":40.0,"currency":"EUR",` +
		`"reset_at":"2026-12-01T00:00:00Z",` +
		`"vibe_budget":{"usage_percentage":0,"initial_budget":300.0,"currency":"EUR",` +
		`"reset_at":"2026-12-01T00:00:00Z"}}`
	windows, err := parseMistralBudget([]byte(body))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(windows) != 2 {
		t.Fatalf("应有 2 条，实际 %d", len(windows))
	}
	if windows[0].Cap != 40 || windows[0].Used != 20 {
		t.Errorf("顶层回退值不对: %+v", windows[0])
	}
	if !strings.Contains(windows[0].Hint, "EUR") {
		t.Errorf("币种应来自顶层: %q", windows[0].Hint)
	}
}

// 只返回 vibe_budget 的响应是合法的（账号只有 Vibe 预算时实测如此）。
// 顶层为空只说明 api 那一侧没数据，不能因此整条额度都报错。
func TestParseMistralBudgetVibeOnly(t *testing.T) {
	body := `{"vibe_budget":{"usage_percentage":20.81,"initial_budget":300.0,` +
		`"currency":"USD","reset_at":"2026-12-01T00:00:00Z","payg_enabled":false}}`

	windows, err := parseMistralBudget([]byte(body))
	if err != nil {
		t.Fatalf("只有 vibe_budget 时应解析成功，实际报错: %v", err)
	}
	if len(windows) != 1 {
		t.Fatalf("应产出 1 条额度窗口，实际 %d: %+v", len(windows), windows)
	}
	w := windows[0]
	if w.Label != "Vibe 额度" || w.Cap != 300 || w.Percent != 20.81 {
		t.Errorf("Vibe 额度解析不对: %+v", w)
	}
	if w.ResetAt == nil || w.ResetAt.Year() != 2026 {
		t.Errorf("重置时间没解析出来: %+v", w.ResetAt)
	}
	if !strings.Contains(w.Hint, "USD") {
		t.Errorf("提示里应带币种: %q", w.Hint)
	}
}

func TestParseMistralBudgetErrors(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"非 JSON", `not json`, "不是合法的 JSON"},
		// 顶层与两个子对象都没有 initial_budget，说明响应里根本没有预算数据。
		// 注意顶层字段本身就是 api_budget 的回退值，所以「只有顶层」不该算错。
		{"三个 budget 都缺", `{"unexpected":true}`, "没有 api_budget"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := parseMistralBudget([]byte(c.body))
			if err == nil {
				t.Fatal("应报错")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("错误信息应包含 %q，实际 %q", c.want, err.Error())
			}
		})
	}
}

// ---------- 订阅解析 ----------

func TestParseMistralSubscription(t *testing.T) {
	sub, err := parseMistralSubscription([]byte(mistralSubscriptionJSON))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if sub.planName != "INDIVIDUAL" {
		t.Errorf("计划名应为 INDIVIDUAL，实际 %q", sub.planName)
	}
	if sub.status != "活跃" {
		t.Errorf("付费计划状态应为活跃，实际 %q", sub.status)
	}
	// payg_enabled=false，不该冒出一条按量付费的警告。
	for _, w := range sub.warnings {
		if strings.Contains(w, "按量付费") {
			t.Errorf("未开通按量付费却给了警告: %v", sub.warnings)
		}
	}
}

func TestParseMistralSubscriptionPaygWarning(t *testing.T) {
	body := `{"plan_name":"TEAM","payg_enabled":true,"vibe_payg_enabled":true,"enterprise":true}`
	sub, err := parseMistralSubscription([]byte(body))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !strings.Contains(sub.planName, "企业版") || !strings.Contains(sub.planName, "Vibe 按量付费") {
		t.Errorf("计划名应带上企业与 Vibe 标记，实际 %q", sub.planName)
	}
	if !hasWarning(sub.warnings, "按量付费") {
		t.Errorf("开通按量付费时应给警告，实际 %v", sub.warnings)
	}
}

func TestParseMistralSubscriptionFree(t *testing.T) {
	sub, err := parseMistralSubscription([]byte(`{"plan_name":"FREE","payg_enabled":false}`))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if sub.status != "免费版" {
		t.Errorf("FREE 计划状态应为免费版，实际 %q", sub.status)
	}
}

func TestParseMistralSubscriptionErrors(t *testing.T) {
	if _, err := parseMistralSubscription([]byte(`not json`)); err == nil {
		t.Error("非 JSON 应报错")
	}
	// 以前这里是靠正则从 HTML 里抠的，现在只认 plan_name 这个字段。
	if _, err := parseMistralSubscription([]byte(`{"payg_enabled":false}`)); err == nil {
		t.Error("缺 plan_name 应报错")
	}
}

// ---------- 限额与 token 明细（沿用原有解析） ----------

func TestParseMistralLimits(t *testing.T) {
	stats, warnings, err := parseMistralLimits([]byte(mistralLimitsJSON))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(stats) != 3 {
		t.Errorf("应产出 3 条金额统计（usage_limit 为 null 不展示），实际 %d: %+v", len(stats), stats)
	}
	if !hasWarning(warnings, "未设置月度限额") {
		t.Errorf("no_monthly_limit=true 时应给说明，实际 %v", warnings)
	}
}

func TestParseMistralUsageTokens(t *testing.T) {
	stats, err := parseMistralUsageTokens([]byte(mistralUsageJSON))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(stats) != 3 {
		t.Fatalf("应产出 3 条 token 统计，实际 %d: %+v", len(stats), stats)
	}
	// input 合计 1000+500=1500，output 200，cached 3000
	if stats[0].Value != 1500 || stats[1].Value != 200 || stats[2].Value != 3000 {
		t.Errorf("汇总值不对: %+v", stats)
	}
}

// ---------- 端到端 ----------

// mistralAPIHandler 是官方 API 的正常响应。
func mistralAPIHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case mistralPathBudget:
		_, _ = w.Write([]byte(mistralBudgetJSON))
	case mistralPathSubscription:
		_, _ = w.Write([]byte(mistralSubscriptionJSON))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// mistralAdminHandler 是 admin 后台的正常响应（会话 token 由 mistralAuthMock 发放）。
func mistralAdminHandler(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer ory_st_test123" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch r.URL.Path {
	case mistralAdminPathLimits:
		_, _ = w.Write([]byte(mistralLimitsJSON))
	case mistralAdminPathUsage:
		_, _ = w.Write([]byte(mistralUsageJSON))
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

// mistralFixture 是一套「官方 API + admin 后台 + 登录服务」的 mock。
type mistralFixture struct {
	apiURL  string
	apiKey  string
	account string
	pass    string
}

// newMistralFixture 起三套 mock 并把站点根指过去。
//
// apiHandler / adminHandler 传 nil 表示那个服务不可用（返回 500 / 401）。
func newMistralFixture(t *testing.T, apiHandler, adminHandler http.HandlerFunc) *mistralFixture {
	t.Helper()

	const apiKey = "sk-mistral-test"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+apiKey {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if apiHandler == nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		apiHandler(w, r)
	}))
	t.Cleanup(api.Close)

	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if adminHandler == nil {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		adminHandler(w, r)
	}))
	t.Cleanup(admin.Close)

	prevAdmin := mistralAdminOrigin
	mistralAdminOrigin = admin.URL
	t.Cleanup(func() { mistralAdminOrigin = prevAdmin })

	return &mistralFixture{
		apiURL:  api.URL + "/v1",
		apiKey:  apiKey,
		account: "user@example.com",
		pass:    "secret",
	}
}

// startMistralLogin 起 Ory 登录 mock，返回 fixture 里的账号。
//
// 有效期必须给未来时间：mistralSession 会按 expires_at 减去 5 分钟提前量
// 判断是否可用，写一个过去的固定时间会让 token 一拿到手就过期，
// 后续的 admin 请求就会带着空 token 发出去（表现为整轮静默跳过）。
func (f *mistralFixture) startMistralLogin(t *testing.T) {
	t.Helper()
	mock := &mistralAuthMock{
		account:   f.account,
		password:  f.pass,
		expiresAt: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}
	mock.start(t)
}

// createProvider 建一个指向 mock 官方 API 的供应商。
func (f *mistralFixture) createProvider(t *testing.T, st *store.Store, usage model.UsageQueryConfig) model.Provider {
	t.Helper()
	usage.Template = mistralTemplateID
	p := model.Provider{
		Name:       "mistral",
		BaseURL:    f.apiURL,
		APIFormat:  model.FormatOpenAI,
		Keys:       []model.APIKey{{ID: "k1", Key: f.apiKey, Enabled: true}},
		UsageQuery: usage,
	}
	created, err := st.CreateProvider(p)
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}
	return created
}

// 只配 API key（不填账号密码）：预算与计划要正常，并说明 token 明细被跳过。
func TestQueryUsageMistralAPIKeyOnly(t *testing.T) {
	f := newMistralFixture(t, mistralAPIHandler, nil)

	srv, st := newTestServer(t)
	prov := f.createProvider(t, st, model.UsageQueryConfig{})

	snap := runQueryUsage(t, srv, st, prov)
	if !snap.OK {
		t.Fatalf("只配密钥时应成功，错误: %s", snap.Error)
	}
	if len(snap.Windows) != 2 {
		t.Errorf("应有 2 条额度窗口，实际 %d: %+v", len(snap.Windows), snap.Windows)
	}
	if snap.PlanName != "INDIVIDUAL" {
		t.Errorf("计划名应为 INDIVIDUAL，实际 %q", snap.PlanName)
	}
	if !hasWarning(snap.Warnings, "未填写账号密码") {
		t.Errorf("应说明 token 明细被跳过，实际 %v", snap.Warnings)
	}
	// 没有会话就不该去碰 admin 后台。
	for _, u := range snap.UsedURLs {
		if strings.Contains(u, "admin") {
			t.Errorf("没填账号密码时不该请求 admin：%s", u)
		}
	}
}

// 配了账号密码：三部分数据都要在。
func TestQueryUsageMistralFull(t *testing.T) {
	f := newMistralFixture(t, mistralAPIHandler, mistralAdminHandler)
	f.startMistralLogin(t)

	srv, st := newTestServer(t)
	prov := f.createProvider(t, st, model.UsageQueryConfig{
		Account:  f.account,
		Password: f.pass,
	})

	snap := runQueryUsage(t, srv, st, prov)
	if !snap.OK {
		t.Fatalf("查询应成功，错误: %s，警告: %v", snap.Error, snap.Warnings)
	}
	if len(snap.Windows) != 2 {
		t.Errorf("应有 2 条额度窗口，实际 %d", len(snap.Windows))
	}
	// 3 条限额金额 + 3 条 token 汇总
	if len(snap.Period) != 6 {
		t.Errorf("应有 6 条周期统计，实际 %d: %+v; warnings=%v; urls=%v", len(snap.Period), snap.Period, snap.Warnings, snap.UsedURLs)
	}
	if snap.PlanName != "INDIVIDUAL" {
		t.Errorf("计划名不对: %q", snap.PlanName)
	}
}

// 官方接口挂了、admin 还活着：仍要给出能读到的部分，并把失败原因写清楚。
func TestQueryUsageMistralAPIFailure(t *testing.T) {
	f := newMistralFixture(t, nil, mistralAdminHandler)
	f.startMistralLogin(t)

	srv, st := newTestServer(t)
	prov := f.createProvider(t, st, model.UsageQueryConfig{
		Account:  f.account,
		Password: f.pass,
	})

	snap := runQueryUsage(t, srv, st, prov)
	if !snap.OK {
		t.Fatalf("admin 还能读时不该整体失败: %s", snap.Error)
	}
	if len(snap.Windows) != 0 {
		t.Errorf("预算接口挂了不该有窗口数据，实际 %+v", snap.Windows)
	}
	if !hasWarning(snap.Warnings, "预算接口不可用") {
		t.Errorf("应说明预算接口不可用: %v", snap.Warnings)
	}
	// token 明细来自 admin，应当还在。
	if len(snap.Period) == 0 {
		t.Errorf("admin 可用时 token 明细应保留，实际为空")
	}
}

// 账号密码错：不该拖垮预算那部分，只把 token 明细降级成警告。
//
// 这里额外盯住一个坑：登录失败时不能同时冒「未填写账号密码」——
// 那是两件不同的事，同屏出现会让用户以为自己的凭据根本没存进去。
func TestQueryUsageMistralBadPasswordKeepsBudget(t *testing.T) {
	f := newMistralFixture(t, mistralAPIHandler, mistralAdminHandler)
	f.startMistralLogin(t)

	srv, st := newTestServer(t)
	prov := f.createProvider(t, st, model.UsageQueryConfig{
		Account:  f.account,
		Password: "wrong-password",
	})

	snap := runQueryUsage(t, srv, st, prov)
	if !snap.OK {
		t.Fatalf("登录失败时预算仍应可读，错误: %s", snap.Error)
	}
	if len(snap.Windows) != 2 {
		t.Errorf("预算窗口应仍然存在，实际 %d", len(snap.Windows))
	}
	if !hasWarning(snap.Warnings, "登录失败") {
		t.Errorf("应说明登录失败并跳过明细: %v", snap.Warnings)
	}
	// 关键断言：填了凭据就不该再说「未填写」。
	if hasWarning(snap.Warnings, "未填写账号密码") {
		t.Errorf("登录失败时不该同时出现「未填写账号密码」: %v", snap.Warnings)
	}
}

// 全部不可用时要报整体失败，不能返回空快照冒充成功。
func TestQueryUsageMistralAllFailed(t *testing.T) {
	f := newMistralFixture(t, nil, nil)

	srv, st := newTestServer(t)
	prov := f.createProvider(t, st, model.UsageQueryConfig{})

	snap := runQueryUsage(t, srv, st, prov)
	if snap.OK {
		t.Fatal("所有接口都挂时不该成功")
	}
	if snap.Error == "" {
		t.Error("应给出整体失败原因")
	}
}

// 密钥无效：两个官方接口都 401，要给一句「密钥不对」而不是两条泛化警告。
func TestQueryUsageMistralBadKey(t *testing.T) {
	f := newMistralFixture(t, mistralAPIHandler, nil)

	srv, st := newTestServer(t)
	usage := model.UsageQueryConfig{Template: mistralTemplateID}
	p := model.Provider{
		Name:       "mistral",
		BaseURL:    f.apiURL,
		APIFormat:  model.FormatOpenAI,
		Keys:       []model.APIKey{{ID: "k1", Key: "wrong-key", Enabled: true}},
		UsageQuery: usage,
	}
	prov, err := st.CreateProvider(p)
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}

	snap := runQueryUsage(t, srv, st, prov)
	if snap.OK {
		t.Fatal("密钥无效时不该成功")
	}
	if !strings.Contains(snap.Error, "密钥") {
		t.Errorf("错误信息应指出密钥问题，实际 %q", snap.Error)
	}
}

// 一个密钥都没有、也没账号密码：直接说清楚需要什么，不发请求。
func TestQueryUsageMistralNoCredentials(t *testing.T) {
	f := newMistralFixture(t, mistralAPIHandler, mistralAdminHandler)

	srv, st := newTestServer(t)
	usage := model.UsageQueryConfig{Template: mistralTemplateID}
	p := model.Provider{
		Name:       "mistral",
		BaseURL:    f.apiURL,
		APIFormat:  model.FormatOpenAI,
		UsageQuery: usage,
	}
	prov, err := st.CreateProvider(p)
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}

	snap := runQueryUsage(t, srv, st, prov)
	if snap.OK {
		t.Fatal("没有任何凭据时不该成功")
	}
	if !strings.Contains(snap.Error, "密钥") {
		t.Errorf("错误信息应提示需要密钥，实际 %q", snap.Error)
	}
	if len(snap.UsedURLs) != 0 {
		t.Errorf("缺凭据时不该发出请求，实际请求了 %v", snap.UsedURLs)
	}
}

// 只填账号密码、没有 API key：认 key 的官方接口根本发不出去，
// 必须跳过它们，把 admin 能给的额度与 token 明细正常读回来。
func TestQueryUsageMistralAccountOnly(t *testing.T) {
	f := newMistralFixture(t, mistralAPIHandler, mistralAdminHandler)
	f.startMistralLogin(t)

	srv, st := newTestServer(t)
	p := model.Provider{
		Name:      "mistral",
		BaseURL:   f.apiURL,
		APIFormat: model.FormatOpenAI,
		UsageQuery: model.UsageQueryConfig{
			Template: mistralTemplateID,
			Account:  f.account,
			Password: f.pass,
		},
	}
	prov, err := st.CreateProvider(p)
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}

	snap := runQueryUsage(t, srv, st, prov)
	if !snap.OK {
		t.Fatalf("只有账号密码时应能读到 admin 数据，错误: %s，警告: %v", snap.Error, snap.Warnings)
	}
	// 没有密钥就没有预算窗口，但限额 + token 明细共 6 条统计要在。
	if len(snap.Windows) != 0 {
		t.Errorf("没有密钥时不该有预算窗口，实际 %+v", snap.Windows)
	}
	if len(snap.Period) != 6 {
		t.Errorf("应读到 6 条周期统计，实际 %d: %+v", len(snap.Period), snap.Period)
	}
	for _, u := range snap.UsedURLs {
		if strings.HasPrefix(u, f.apiURL) {
			t.Errorf("没有密钥时不该请求官方接口: %s", u)
		}
	}
	for _, w := range snap.Warnings {
		if strings.Contains(w, "预算接口不可用") || strings.Contains(w, "订阅接口不可用") ||
			strings.Contains(w, "密钥被拒") {
			t.Errorf("跳过官方接口时不该报不可用/密钥被拒: %v", snap.Warnings)
		}
	}
}

// key 被拒但网页会话可用：不能因官方接口 401 提前收工，
// admin 的额度与明细仍要读回来。
func TestQueryUsageMistralKeyRejectedStillReadsAdmin(t *testing.T) {
	rejecting := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	f := newMistralFixture(t, rejecting, mistralAdminHandler)
	f.startMistralLogin(t)

	srv, st := newTestServer(t)
	prov := f.createProvider(t, st, model.UsageQueryConfig{
		Account:  f.account,
		Password: f.pass,
	})

	snap := runQueryUsage(t, srv, st, prov)
	if !snap.OK {
		t.Fatalf("admin 还能读时不该整体失败: %s", snap.Error)
	}
	if len(snap.Windows) != 0 {
		t.Errorf("官方接口 401 时不该有预算窗口，实际 %+v", snap.Windows)
	}
	if len(snap.Period) == 0 {
		t.Errorf("admin 可读时限额与明细应保留，实际为空；警告: %v", snap.Warnings)
	}
	if !hasWarning(snap.Warnings, "401") {
		t.Errorf("应说明官方接口被拒，实际 %v", snap.Warnings)
	}
}

// 会话 getter 遇到 401 要强制重登并重试一次 —— 只一次，多试只会撞上 Ory 的
// 登录限流。
func TestUsageSessionGetterRetriesOnceOn401(t *testing.T) {
	var mu sync.Mutex
	var seen []string
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("Authorization"))
		mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer fresh" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(admin.Close)

	var forced int
	tokenFn := func(_ context.Context, force bool) (string, error) {
		if force {
			forced++
			return "fresh", nil
		}
		return "stale", nil
	}

	get := newUsageSessionGetter(admin.Client(), admin.URL, tokenFn)
	status, body, err := get(context.Background(), "/api/x", nil)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if status != http.StatusOK || !strings.Contains(string(body), "ok") {
		t.Errorf("401 后重登重试应成功，实际 status=%d body=%s", status, body)
	}
	if forced != 1 {
		t.Errorf("应强制重登 1 次，实际 %d 次", forced)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("应发 2 次请求（原始 + 重试），实际 %d: %v", len(seen), seen)
	}
	if seen[0] != "Bearer stale" || seen[1] != "Bearer fresh" {
		t.Errorf("两次请求的 token 不对: %v", seen)
	}
}

// 重登拿回同一个 token 时不能再重试，否则会在 401 上打转。
func TestUsageSessionGetterNoRetryLoopWhenTokenUnchanged(t *testing.T) {
	var calls atomic.Int32
	admin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	t.Cleanup(admin.Close)

	var forced int
	tokenFn := func(_ context.Context, force bool) (string, error) {
		if force {
			forced++
		}
		return "same", nil
	}

	get := newUsageSessionGetter(admin.Client(), admin.URL, tokenFn)
	status, _, err := get(context.Background(), "/api/x", nil)
	if err != nil {
		t.Fatalf("请求失败: %v", err)
	}
	if status != http.StatusUnauthorized {
		t.Errorf("应把原来的 401 交给调用方，实际 %d", status)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("token 没变时不该重发请求，实际发了 %d 次", got)
	}
	if forced != 1 {
		t.Errorf("应强制重登尝试 1 次，实际 %d 次", forced)
	}
}
