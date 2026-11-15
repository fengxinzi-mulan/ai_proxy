package proxy

// Mistral 用量模板的解析用例。
//
// 订阅页是 Next.js 的 RSC flight 输出（self.__next_f.push([1,"..."])），
// 数据被转义过一层。这里的样本按实测结构裁剪，只保留解析关心的部分，
// 不含任何真实账号信息。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai_proxy/internal/model"
)

// mistralSubscriptionHTML 是一份仿真的订阅页，结构照抄实测响应。
//
// 刻意保留真实页面里 "subscriptions" 出现三次的形态（两次是 i18n 文案、
// 最后一次才是数据数组）—— 这正是解析最容易踩的坑。
func mistralSubscriptionHTML() string {
	// 内层 JSON 先按真实形态转义成 flight 里的样子。
	budget := `{"usage_percentage":0,"initial_budget":30,"currency":"USD","reset_at":"2026-12-01T00:00:00Z",` +
		`"api_budget":{"usage_percentage":0,"initial_budget":30,"currency":"USD","reset_at":"2026-12-01T00:00:00Z"},` +
		`"vibe_budget":{"usage_percentage":0.977104408,"initial_budget":300,"currency":"USD","reset_at":"2026-12-01T00:00:00Z"}}`

	subs := `[{"type":"API","subscription":null},{"type":"CHAT","subscription":{"plan":"INDIVIDUAL","periodicity":"MONTHLY","status":"A",` +
		`"seats":null,"user":"00000000-0000-0000-0000-000000000000"}},{"type":"MISTRAL_CODE","subscription":null}]`

	// 真实页面里这两处 "subscriptions" 是文案，不是数据。
	i18n := `"subscriptions":"Subscriptions","billing":"Billing",` +
		`"subscriptions":{"title":"Subscriptions","api":"API","chat":"Chat"},`

	payload := i18n +
		`"billingInfo":{"legal_name":"EXAMPLE","currency":"USD"},` +
		`"budget":` + budget + `,` +
		`"subscriptions":` + subs

	escaped := strings.NewReplacer(`"`, `\"`).Replace(payload)
	return `<html><body><script>self.__next_f.push([1,"` + escaped + `"])</script></body></html>`
}

// mistralLimitsJSON 是 /api/billing/limits 的实测结构。
const mistralLimitsJSON = `{"limits":{"completion":{"no_monthly_limit":true,` +
	`"monthly_limit_reached":false,"usage":1.25,"vibe_usage":2.5,"total_usage":3.75,` +
	`"usage_limit":null},"last_payment_failure":false,"currency":"USD"}}`

// mistralUsageJSON 是 /api/billing/v2/usage 的实测结构（只留 token 部分）。
const mistralUsageJSON = `{"completion":{"models":{}},"chat":{"models":{}},` +
	`"vibe_code":{"completion":{"models":{"some-model::some-model":{` +
	`"input":[{"timestamp":"2026-11-14","value":1000},{"timestamp":"2026-11-15","value":500}],` +
	`"output":[{"timestamp":"2026-11-14","value":200}],` +
	`"cached":[{"timestamp":"2026-11-14","value":3000}]}}}},` +
	`"currency":"USD"}`

// ---------- 括号平衡扫描 ----------

func TestBalancedJSONObject(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"平铺对象", `{"a":1}xxx`, `{"a":1}`},
		{"嵌套对象", `{"a":{"b":2}}tail`, `{"a":{"b":2}}`},
		{"字符串里的花括号", `{"a":"{not structure}"}tail`, `{"a":"{not structure}"}`},
		{"字符串里的转义引号", `{"a":"say \"hi\""}tail`, `{"a":"say \"hi\""}`},
		{"字符串里的反斜杠", `{"a":"C:\\path\\"}tail`, `{"a":"C:\\path\\"}`},
		{"未闭合", `{"a":{"b":1}`, ``},
		{"不以花括号开头", `[1,2]`, ``},
		{"空串", ``, ``},
	}
	for _, c := range cases {
		if got := balancedJSONObject(c.in); got != c.want {
			t.Errorf("%s: 期望 %q，实际 %q", c.name, c.want, got)
		}
	}
}

func TestExtractJSONObject(t *testing.T) {
	s := `prefix"budget":{"a":1,"b":{"c":2}}suffix`
	if got := extractJSONObject(s, `"budget":`); got != `{"a":1,"b":{"c":2}}` {
		t.Errorf("截取错误: %q", got)
	}
	if got := extractJSONObject(s, `"missing":`); got != "" {
		t.Errorf("找不到 key 时应返回空串，实际 %q", got)
	}
	// key 与对象之间有空格也要能处理。
	if got := extractJSONObject(`"budget": {"a":1}`, `"budget":`); got != `{"a":1}` {
		t.Errorf("带空格时截取错误: %q", got)
	}
}

// 同名 key 在页面里重复出现时要能全部找出来。
//
// 实测的订阅页里 "subscriptions" 出现三次：两次是 i18n 文案
// （字符串与对象），最后一次才是数据数组。只取第一个匹配会解析到文案上。
func TestExtractAllJSONArrays(t *testing.T) {
	s := `"subscriptions":"Subscriptions",` +
		`"subscriptions":{"title":"Subscriptions"},` +
		`"subscriptions":[{"type":"CHAT"}]`
	got := extractAllJSONArrays(s, `"subscriptions":`)
	if len(got) != 1 {
		t.Fatalf("应只截出 1 个数组（前两处不是数组），实际 %d: %v", len(got), got)
	}
	if got[0] != `[{"type":"CHAT"}]` {
		t.Errorf("截取错误: %q", got[0])
	}
}

func TestBalancedJSONArray(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"平铺数组", `[1,2]xxx`, `[1,2]`},
		{"嵌套数组", `[[1],[2]]tail`, `[[1],[2]]`},
		{"字符串里的方括号", `["[not structure]"]tail`, `["[not structure]"]`},
		{"未闭合", `[1,2`, ``},
		{"不以方括号开头", `{"a":1}`, ``},
	}
	for _, c := range cases {
		if got := balancedJSONArray(c.in); got != c.want {
			t.Errorf("%s: 期望 %q，实际 %q", c.name, c.want, got)
		}
	}
}

// ---------- 订阅页解析 ----------

func TestParseMistralSubscription(t *testing.T) {
	got, err := parseMistralSubscription([]byte(mistralSubscriptionHTML()))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	if len(got.windows) != 2 {
		t.Fatalf("应有 2 条额度窗口，实际 %d: %+v", len(got.windows), got.windows)
	}

	api := got.windows[0]
	if api.Label != "API 额度" {
		t.Errorf("第一条应为 API 额度，实际 %q", api.Label)
	}
	if api.Percent != 0 {
		t.Errorf("API 已用百分比应为 0，实际 %v", api.Percent)
	}
	if api.Cap != 30 {
		t.Errorf("API 总额度应为 30，实际 %v", api.Cap)
	}
	if api.Used != 0 {
		t.Errorf("API 已用量应为 0，实际 %v", api.Used)
	}
	if api.ResetAt == nil || api.ResetAt.Format("2006-01-02") != "2026-12-01" {
		t.Errorf("API 重置时间不对: %v", api.ResetAt)
	}

	vibe := got.windows[1]
	if vibe.Label != "Vibe 额度" {
		t.Errorf("第二条应为 Vibe 额度，实际 %q", vibe.Label)
	}
	// 0.977104408 是百分数，不是 0.9771% 的分数形式。
	if vibe.Percent < 0.977 || vibe.Percent > 0.978 {
		t.Errorf("Vibe 已用百分比应约为 0.977，实际 %v", vibe.Percent)
	}
	if vibe.Cap != 300 {
		t.Errorf("Vibe 总额度应为 300，实际 %v", vibe.Cap)
	}
	// 已用量按 总额 × 百分比 / 100 算出，约 2.93。
	if vibe.Used < 2.9 || vibe.Used > 2.95 {
		t.Errorf("Vibe 已用量应约为 2.93，实际 %v", vibe.Used)
	}

	if got.planName != "INDIVIDUAL / MONTHLY" {
		t.Errorf("计划名应为 INDIVIDUAL / MONTHLY，实际 %q", got.planName)
	}
	if got.status != "活跃" {
		t.Errorf("状态应翻译成活跃，实际 %q", got.status)
	}
}

// 未登录时后台返回的是登录流程对象，不能当成数据解析成功。
func TestParseMistralSubscriptionLoginPage(t *testing.T) {
	page := `{"id":"x","type":"browser","request_url":"https://auth.mistral.ai/self-service/login/browser"}`
	_, err := parseMistralSubscription([]byte(page))
	if err == nil {
		t.Fatal("登录页应被识别为失败")
	}
	if !strings.Contains(err.Error(), "登录页") {
		t.Errorf("错误信息应说明返回的是登录页，实际 %q", err.Error())
	}
}

// 页面结构变了（没有 budget）时要报错，不能返回空额度冒充成功。
func TestParseMistralSubscriptionMissingBudget(t *testing.T) {
	_, err := parseMistralSubscription([]byte(`<html>nothing here</html>`))
	if err == nil {
		t.Fatal("没有 budget 时应报错")
	}
}

// ---------- 限额与 token 解析 ----------

func TestParseMistralLimits(t *testing.T) {
	stats, warnings, err := parseMistralLimits([]byte(mistralLimitsJSON))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	want := map[string]float64{
		"本月 API 用量":  1.25,
		"本月 Vibe 用量": 2.5,
		"本月合计用量":    3.75,
	}
	if len(stats) != len(want) {
		t.Fatalf("应有 %d 条统计，实际 %d: %+v", len(want), len(stats), stats)
	}
	for _, s := range stats {
		if s.Unit != model.UnitUSD {
			t.Errorf("%s 的单位应为 usd，实际 %q", s.Label, s.Unit)
		}
		if v, ok := want[s.Label]; ok && v != s.Value {
			t.Errorf("%s 应为 %v，实际 %v", s.Label, v, s.Value)
		}
	}

	// no_monthly_limit=true 要提示出来，否则用户会以为有限额却看不到数值。
	if !hasWarning(warnings, "未设置月度限额") {
		t.Errorf("缺少免限额说明: %v", warnings)
	}
}

func TestParseMistralLimitsReached(t *testing.T) {
	body := `{"limits":{"completion":{"no_monthly_limit":false,"monthly_limit_reached":true,` +
		`"usage":30.0,"vibe_usage":0.0,"total_usage":30.0,"usage_limit":30.0},` +
		`"last_payment_failure":true,"currency":"USD"}}`
	stats, warnings, err := parseMistralLimits([]byte(body))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if !hasWarning(warnings, "已达上限") {
		t.Errorf("缺少达上限告警: %v", warnings)
	}
	if !hasWarning(warnings, "支付失败") {
		t.Errorf("缺少支付失败告警: %v", warnings)
	}
	// usage_limit 有值时要单独出一条。
	found := false
	for _, s := range stats {
		if s.Label == "月度限额" && s.Value == 30 {
			found = true
		}
	}
	if !found {
		t.Errorf("应有月度限额统计: %+v", stats)
	}
}

func TestParseMistralUsageTokens(t *testing.T) {
	stats, err := parseMistralUsageTokens([]byte(mistralUsageJSON))
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}

	want := map[string]float64{
		"本月输入 token": 1500, // 1000 + 500
		"本月输出 token": 200,
		"本月缓存 token": 3000,
	}
	if len(stats) != len(want) {
		t.Fatalf("应有 %d 条统计，实际 %d: %+v", len(want), len(stats), stats)
	}
	for _, s := range stats {
		if s.Unit != model.UnitTokens {
			t.Errorf("%s 的单位应为 tokens，实际 %q", s.Label, s.Unit)
		}
		if v, ok := want[s.Label]; ok && v != s.Value {
			t.Errorf("%s 应为 %v，实际 %v", s.Label, v, s.Value)
		}
	}
}

// 完全没有 token 用量时要报错，而不是返回三条 0。
func TestParseMistralUsageTokensEmpty(t *testing.T) {
	body := `{"completion":{"models":{}},"chat":{"models":{}},"vibe_code":{"completion":{"models":{}}}}`
	if _, err := parseMistralUsageTokens([]byte(body)); err == nil {
		t.Fatal("没有用量时应报错")
	}
}

// ---------- 模板注册与端到端 ----------

func TestMistralTemplateRegistered(t *testing.T) {
	found := false
	for _, tpl := range UsageTemplates() {
		if tpl.ID == mistralTemplateID {
			found = true
			if tpl.Name != "Mistral" {
				t.Errorf("模板名应为 Mistral，实际 %q", tpl.Name)
			}
		}
	}
	if !found {
		t.Fatal("mistral 模板未注册")
	}
}

// 缺账号密码时不该发任何请求。
func TestQueryUsageMistralRequiresCredentials(t *testing.T) {
	srv, st := newTestServer(t)
	prov, err := st.CreateProvider(model.Provider{
		Name:       "mistral",
		BaseURL:    "https://api.mistral.ai/v1",
		APIFormat:  model.FormatOpenAI,
		UsageQuery: model.UsageQueryConfig{Template: mistralTemplateID},
	})
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}

	snap := runQueryUsage(t, srv, st, prov)
	if snap.OK {
		t.Fatal("缺账号密码时不该成功")
	}
	if !strings.Contains(snap.Error, "账号密码") {
		t.Errorf("错误信息应说明需要账号密码，实际 %q", snap.Error)
	}
	if len(snap.UsedURLs) != 0 {
		t.Errorf("缺凭据时不该发出请求，实际请求了 %v", snap.UsedURLs)
	}
}

// 端到端：登录 + 三个接口都成功时，快照里要有额度、统计与计划。
func TestQueryUsageMistralEndToEnd(t *testing.T) {
	var adminSrv *httptest.Server
	adminSrv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ory_st_test123" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case mistralPathSubscription:
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte(mistralSubscriptionHTML()))
		case mistralPathLimits:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(mistralLimitsJSON))
		case mistralPathUsage:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(mistralUsageJSON))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer adminSrv.Close()

	// 登录服务
	authMock := &mistralAuthMock{
		account:   "user@example.com",
		password:  "secret",
		expiresAt: "2026-11-16T00:00:00Z",
	}
	authSrv := authMock.start(t)

	// 把后台地址也指向 mock：模板里写死的是 admin.mistral.ai。
	prevAdmin := mistralAdminOrigin
	mistralAdminOrigin = adminSrv.URL
	t.Cleanup(func() { mistralAdminOrigin = prevAdmin })
	_ = authSrv

	srv, st := newTestServer(t)
	prov, err := st.CreateProvider(model.Provider{
		Name:      "mistral",
		BaseURL:   "https://api.mistral.ai/v1",
		APIFormat: model.FormatOpenAI,
		UsageQuery: model.UsageQueryConfig{
			Template: mistralTemplateID,
			Account:  authMock.account,
			Password: authMock.password,
		},
	})
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}

	snap := runQueryUsage(t, srv, st, prov)
	if !snap.OK {
		t.Fatalf("查询应成功，错误: %s，警告: %v", snap.Error, snap.Warnings)
	}
	if len(snap.Windows) != 2 {
		t.Errorf("应有 2 条额度窗口，实际 %d: %+v", len(snap.Windows), snap.Windows)
	}
	// 3 条限额统计 + 3 条 token 统计
	if len(snap.Period) != 6 {
		t.Errorf("应有 6 条周期统计，实际 %d: %+v", len(snap.Period), snap.Period)
	}
	if snap.PlanName != "INDIVIDUAL / MONTHLY" {
		t.Errorf("计划名不对: %q", snap.PlanName)
	}
	if snap.Status != "活跃" {
		t.Errorf("状态不对: %q", snap.Status)
	}
}

// 某个接口挂了仍要展示其它两个，并写清降级原因。
func TestQueryUsageMistralPartialFailure(t *testing.T) {
	adminSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == mistralPathLimits {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		switch r.URL.Path {
		case mistralPathSubscription:
			_, _ = w.Write([]byte(mistralSubscriptionHTML()))
		case mistralPathUsage:
			_, _ = w.Write([]byte(mistralUsageJSON))
		}
	}))
	defer adminSrv.Close()

	authMock := &mistralAuthMock{account: "user@example.com", password: "secret", expiresAt: "2026-11-16T00:00:00Z"}
	authMock.start(t)

	prevAdmin := mistralAdminOrigin
	mistralAdminOrigin = adminSrv.URL
	t.Cleanup(func() { mistralAdminOrigin = prevAdmin })

	srv, st := newTestServer(t)
	prov, err := st.CreateProvider(model.Provider{
		Name:      "mistral",
		BaseURL:   "https://api.mistral.ai/v1",
		APIFormat: model.FormatOpenAI,
		UsageQuery: model.UsageQueryConfig{
			Template: mistralTemplateID,
			Account:  authMock.account,
			Password: authMock.password,
		},
	})
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}

	snap := runQueryUsage(t, srv, st, prov)
	if !snap.OK {
		t.Fatalf("部分失败时仍应成功: %s", snap.Error)
	}
	if len(snap.Windows) != 2 {
		t.Errorf("额度窗口应仍然存在，实际 %d", len(snap.Windows))
	}
	if !hasWarning(snap.Warnings, "限额接口不可用") {
		t.Errorf("缺少限额接口的降级说明: %v", snap.Warnings)
	}
}

// 三个接口全挂时要报整体失败，不能返回一个空快照冒充成功。
func TestQueryUsageMistralAllFailed(t *testing.T) {
	adminSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer adminSrv.Close()

	authMock := &mistralAuthMock{account: "user@example.com", password: "secret", expiresAt: "2026-11-16T00:00:00Z"}
	authMock.start(t)

	prevAdmin := mistralAdminOrigin
	mistralAdminOrigin = adminSrv.URL
	t.Cleanup(func() { mistralAdminOrigin = prevAdmin })

	srv, st := newTestServer(t)
	prov, err := st.CreateProvider(model.Provider{
		Name:      "mistral",
		BaseURL:   "https://api.mistral.ai/v1",
		APIFormat: model.FormatOpenAI,
		UsageQuery: model.UsageQueryConfig{
			Template: mistralTemplateID,
			Account:  authMock.account,
			Password: authMock.password,
		},
	})
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}

	snap := runQueryUsage(t, srv, st, prov)
	if snap.OK {
		t.Fatal("三个接口全挂时不该成功")
	}
	if snap.Error == "" {
		t.Error("应给出整体失败原因")
	}
}

// 认证失败要给出「会话无效」这类可操作的提示。
func TestQueryUsageMistralAuthFailure(t *testing.T) {
	adminSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer adminSrv.Close()

	authMock := &mistralAuthMock{account: "user@example.com", password: "secret", expiresAt: "2026-11-16T00:00:00Z"}
	authMock.start(t)

	prevAdmin := mistralAdminOrigin
	mistralAdminOrigin = adminSrv.URL
	t.Cleanup(func() { mistralAdminOrigin = prevAdmin })

	srv, st := newTestServer(t)
	prov, err := st.CreateProvider(model.Provider{
		Name:      "mistral",
		BaseURL:   "https://api.mistral.ai/v1",
		APIFormat: model.FormatOpenAI,
		UsageQuery: model.UsageQueryConfig{
			Template: mistralTemplateID,
			Account:  authMock.account,
			Password: authMock.password,
		},
	})
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}

	snap := runQueryUsage(t, srv, st, prov)
	if snap.OK {
		t.Fatal("认证失败时不该成功")
	}
	if !strings.Contains(snap.Error, "会话无效") {
		t.Errorf("错误信息应说明会话无效，实际 %q", snap.Error)
	}
}
