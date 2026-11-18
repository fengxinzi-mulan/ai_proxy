package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"ai_proxy/internal/model"
)

// 套餐余量查询。
//
// 这里只做「读账号额度」这一件事，与转发链路完全无关：转发用的是上游的生成接口，
// 而额度通常挂在另一组地址上（例如 commandcode 的生成接口在 /provider/v1，
// 额度接口在 /alpha/*）。所以模板要自己负责把 BaseURL 推导成额度接口的根地址。
//
// 这些接口基本都是上游未公开的内部接口，随时可能改结构甚至消失。
// 因此全流程按「能读到就展示，读不到就说清楚」处理：
// 任何一段解析不出预期字段时记进 Warnings，绝不回落成 0。

// ---------- 模板注册表 ----------

// usageClient 是模板能用的全部能力：一次「带上游认证头的 GET」，以及推导好的查询根地址。
// 模板不需要知道代理怎么走、密钥怎么轮询、超时是多少。
type usageClient struct {
	baseURL string
	// origin 是 scheme://host 形式的站点根，供「额度接口固定在站点根某个绝对路径下」的模板使用
	// （例如 OpenCode 的额度接口固定挂在 /zen/go/v1/usage，与 BaseURL 推导出来的路径基准无关）。
	origin string
	// get 用供应商密钥发 GET（生成接口那套身份）。
	get func(ctx context.Context, path string, query url.Values) (int, []byte, error)
	// sessionGet 用网页会话 token 发 GET，未配置（或登录失败）时为 nil。
	//
	// 单独一个函数而不是让 get 自己猜认证方式：同一个模板里可能要同时用两种身份
	// —— Mistral 的预算接口认 API key，token 明细认网页会话。混在一个函数里
	// 就只能按 host 猜，那种隐式规则以后没人看得懂。
	sessionGet func(ctx context.Context, path string, query url.Values) (int, []byte, error)
	// sessionAttempted 记录「填了账号密码、尝试过登录」。
	//
	// 它与 sessionGet == nil 不是一回事：后者既可能是没填凭据，也可能是填了但登录失败。
	// 模板要给用户看的降级说明必须分清这两种，否则会同时冒出「登录失败」和
	// 「未填写账号密码」两条互相矛盾的提示。
	sessionAttempted bool
}

// usageTemplate 描述一种「向上游查询套餐余量」的协议实现。
//
// 新增一个上游平台 = 在 usageTemplateRegistry 里加一条，并写一个 fetch 函数：
// 调接口、把响应映射进 model.UsageSnapshot。前端下拉框会自动出现这个模板。
type usageTemplate struct {
	info  model.UsageTemplateInfo
	fetch func(ctx context.Context, cli *usageClient) (*model.UsageSnapshot, error)
}

var usageTemplateRegistry = []usageTemplate{
	commandCodeTemplate(),
	deepSeekTemplate(),
	openCodeTemplate(),
	mistralTemplate(),
}

// UsageTemplates 返回内置用量查询模板的元数据，供管理界面下拉框使用。
func UsageTemplates() []model.UsageTemplateInfo {
	out := make([]model.UsageTemplateInfo, 0, len(usageTemplateRegistry))
	for _, t := range usageTemplateRegistry {
		out = append(out, t.info)
	}
	return out
}

func findUsageTemplate(id string) (usageTemplate, bool) {
	for _, t := range usageTemplateRegistry {
		if strings.EqualFold(t.info.ID, id) {
			return t, true
		}
	}
	return usageTemplate{}, false
}

func usageTemplateIDs() []string {
	out := make([]string, 0, len(usageTemplateRegistry))
	for _, t := range usageTemplateRegistry {
		out = append(out, t.info.ID)
	}
	return out
}

// ---------- 对外入口 ----------

// QueryUsage 向供应商查询套餐余量。
//
// 与 ProbeProvider 一样返回结构体而不是 error：部分接口失败时仍要能展示已经拿到的部分，
// 并把失败原因和原始响应的位置讲清楚。
func (s *Server) QueryUsage(ctx context.Context, prov model.Provider, settings model.Settings) model.UsageSnapshot {
	start := time.Now()
	snap := model.UsageSnapshot{
		Template:  strings.TrimSpace(prov.UsageQuery.Template),
		FetchedAt: start,
		Balances:  []model.UsageBalance{},
		Windows:   []model.UsageWindow{},
		Period:    []model.UsageStat{},
		Warnings:  []string{},
		UsedURLs:  []string{},
	}
	finish := func() model.UsageSnapshot {
		snap.LatencyMs = time.Since(start).Milliseconds()
		return snap
	}

	if snap.Template == "" {
		snap.Error = "该供应商未配置用量查询模板"
		return finish()
	}
	tmpl, ok := findUsageTemplate(snap.Template)
	if !ok {
		snap.Error = fmt.Sprintf("未知的用量查询模板 %q（可用：%s）",
			snap.Template, strings.Join(usageTemplateIDs(), "、"))
		return finish()
	}
	snap.TemplateName = tmpl.info.Name

	// Mistral 的账号密码是**可选**的：预算与计划走官方 API（认 API key），
	// 只有 token 明细还挂在网页后台（要会话 token）。所以这里不再强制要求账号密码，
	// 也不再把 BaseURL 推导当作非致命错误 —— 预算接口就在 BaseURL 那个站点上。
	// 其它模板仍然只认账号密钥（客户端持钥时代理手上没有密钥，查不了额度）。
	isMistral := tmpl.info.ID == mistralTemplateID
	var mistralAccount, mistralPassword string
	if isMistral {
		mistralAccount, mistralPassword = mistralCredentials(prov)
		// 一个密钥都没有时，预算接口也读不了；只有账号密码也还能查 token 明细。
		if _, hasKey := firstProviderKey(prov); !hasKey && (mistralAccount == "" || mistralPassword == "") {
			snap.Error = "Mistral 用量查询需要供应商密钥（读预算与计划）；" +
				"想额外看本月 token 明细，再在「用量」页填 admin.mistral.ai 的登录账号与密码"
			return finish()
		}
	} else if _, ok := firstProviderKey(prov); !ok {
		snap.Error = "该供应商未配置密钥，用量查询需要账号密钥，无法透传客户端密钥"
		return finish()
	}

	base, err := deriveUsageBaseURL(prov)
	if err != nil {
		// 预算接口就在 BaseURL 的站点根上，推导不出来就查不了 ——
		// 这对所有模板（含 Mistral）都一样了，不再有「Mistral 可以回落写死地址」的分支。
		snap.Error = err.Error()
		return finish()
	}

	netcfg := resolveNet(prov, settings)
	client, err := s.clients.client(netcfg)
	if err != nil {
		snap.Error = "初始化连接失败: " + err.Error()
		return finish()
	}

	// 网页会话是可选的：只有填了账号密码才登录，登录失败也不该拖垮预算那部分。
	var sessionGet func(context.Context, string, url.Values) (int, []byte, error)
	sessionAttempted := false
	if isMistral && mistralAccount != "" && mistralPassword != "" {
		sessionAttempted = true
		token, loginErr := s.mistral.Token(ctx, client, mistralAccount, mistralPassword, false)
		if loginErr != nil {
			// 不 return：预算走 API key，仍然能查。把原因留成警告。
			snap.Warnings = append(snap.Warnings,
				"账号密码登录失败，已跳过本月 token 明细："+loginErr.Error())
		} else {
			// 用变量而不是常量：测试要把站点根指向 httptest，生产路径上两者相同。
			sessionGet = newUsageSessionGetter(client, mistralAdminOrigin, token)
		}
	}

	// 模板会并发发请求，所以原始响应与地址清单要加锁。
	var (
		rawMu    sync.Mutex
		rawStore = map[string]any{}
	)
	// record 把一次响应登记进快照，供界面折叠查看。raw 里只放可读的结构，
	// 非 JSON（HTML 登录页之类）只留长度 —— 几十万字符的页面既看不了也不该落库。
	record := func(path, target string, body []byte) {
		var decoded any
		if json.Unmarshal(body, &decoded) != nil {
			decoded = fmt.Sprintf("<非 JSON 响应，%d 字节>", len(body))
		}
		rawMu.Lock()
		snap.UsedURLs = append(snap.UsedURLs, target)
		rawStore[path] = decoded
		rawMu.Unlock()
	}

	rawSessionGet := sessionGet // 保留原函数，包装后仍要能调用它

	// 包装后的会话 getter：nil 时必须保持 nil。
	// 模板靠「cli.sessionGet == nil」判断要不要发 admin 请求，包一层非 nil 的
	// 闭包会让它以为有会话可用，进而打出「未配置网页会话」这种误导性警告。
	var recordedSessionGet func(context.Context, string, url.Values) (int, []byte, error)
	if rawSessionGet != nil {
		recordedSessionGet = func(ctx context.Context, path string, query url.Values) (int, []byte, error) {
			st, body, err := rawSessionGet(ctx, path, query)
			// 会话请求同样要登记到 UsedURLs / Raw：界面上「查了哪些地址」必须完整，
			// 否则排查时会以为 admin 那两个接口从来没被调用过。
			if err == nil {
				record(path, mistralAdminOrigin+path, body)
			}
			return st, body, err
		}
	}

	cli := &usageClient{
		baseURL: base,
		origin:  originOf(base),
		get: func(ctx context.Context, path string, query url.Values) (int, []byte, error) {
			// path 以 http:// 或 https:// 开头时视为完整地址（模板用 cli.origin 自行拼绝对路径），
			// 否则拼到推导出来的 base 上。
			var target string
			if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
				target = path
			} else {
				target = strings.TrimRight(base, "/") + path
			}
			if len(query) > 0 {
				target += "?" + query.Encode()
			}
			// 每次请求单独构造头：同一个 http.Header 并发复用会被上游写坏
			// （Transport 会往里塞 Host 之类的字段）。
			headers := probeHeaders(prov)
			req, err := buildRequest(ctx, http.MethodGet, target, nil, headers)
			if err != nil {
				return 0, nil, err
			}
			resp, err := client.Do(req)
			if err != nil {
				return 0, nil, err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			record(path, target, body)
			return resp.StatusCode, body, nil
		},
		sessionGet:       recordedSessionGet,
		sessionAttempted: sessionAttempted,
	}

	result, err := tmpl.fetch(ctx, cli)
	if err != nil {
		snap.Error = err.Error()
		rawMu.Lock()
		snap.Raw = rawStore
		rawMu.Unlock()
		return finish()
	}
	if result != nil {
		// 模板只负责填业务字段，元信息由这里统一补齐。
		snap.OK = result.OK
		snap.Account = result.Account
		snap.PlanID = result.PlanID
		snap.PlanName = result.PlanName
		snap.Status = result.Status
		snap.PeriodStart = result.PeriodStart
		snap.PeriodEnd = result.PeriodEnd
		if len(result.Balances) > 0 {
			snap.Balances = result.Balances
		}
		if len(result.Windows) > 0 {
			snap.Windows = result.Windows
		}
		if len(result.Period) > 0 {
			snap.Period = result.Period
		}
		if len(result.Warnings) > 0 {
			snap.Warnings = append(snap.Warnings, result.Warnings...)
		}
	}
	rawMu.Lock()
	snap.Raw = rawStore
	rawMu.Unlock()
	return finish()
}

// deriveUsageBaseURL 从供应商的 BaseURL 推导额度接口的根地址。
//
// 生成接口通常挂在某个协议前缀下（commandcode 是 /provider/v1、多数网关是 /v1），
// 而额度接口挂在站点根上。所以按「最具体的后缀优先」剥掉一层：
// 先试 /provider/v1，再试 /provider，最后 /v1 —— 顺序不能反，
// 否则 /provider/v1 会被 /v1 剥成 /provider。
//
// 用户在「用量」页显式填了地址时直接用它（只去掉末尾斜杠），不再猜测。
func deriveUsageBaseURL(prov model.Provider) (string, error) {
	explicit := strings.TrimSpace(prov.UsageQuery.BaseURL)
	raw := explicit
	if raw == "" {
		raw = strings.TrimSpace(prov.BaseURL)
	}
	if raw == "" {
		return "", errors.New("供应商未填写 BaseURL，无法推导用量查询地址，请在「用量」页手填")
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("无法从 %q 推导用量查询地址，请在「用量」页手填", raw)
	}
	origin := u.Scheme + "://" + u.Host

	path := strings.TrimRight(u.Path, "/")
	if explicit != "" {
		return origin + path, nil
	}
	for _, suffix := range []string{"/provider/v1", "/provider", "/v1"} {
		if strings.HasSuffix(path, suffix) {
			path = strings.TrimSuffix(path, suffix)
			break
		}
	}
	return origin + strings.TrimRight(path, "/"), nil
}

// originOf 取出 URL 的 scheme://host（站点根），解析失败返回空串。
func originOf(raw string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// newUsageSessionGetter 构造「用网页会话 token 发 GET」的取数函数。
//
// 只给 Mistral 的 token 明细用：那一份数据还挂在网页后台（admin.mistral.ai），
// 认的是账号密码登录换来的会话 token，与生成接口的 API key 完全是两套身份。
// 站点根写死 —— 这些接口与生成接口不同域，没法从 BaseURL 推导。
func newUsageSessionGetter(client *http.Client, origin, token string) func(context.Context, string, url.Values) (int, []byte, error) {
	return func(ctx context.Context, path string, query url.Values) (int, []byte, error) {
		target := strings.TrimRight(origin, "/") + path
		if len(query) > 0 {
			target += "?" + query.Encode()
		}
		headers := http.Header{}
		headers.Set("Authorization", "Bearer "+token)
		headers.Set("Accept", "application/json")
		req, err := buildRequest(ctx, http.MethodGet, target, nil, headers)
		if err != nil {
			return 0, nil, err
		}
		resp, err := client.Do(req)
		if err != nil {
			return 0, nil, err
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return resp.StatusCode, body, nil
	}
}

// ---------- commandcode 模板 ----------

// Command Code 的账号额度接口。生成接口在 /provider/v1，这几个在 /alpha 下，
// 是它自己的 CLI /usage 面板在读的同一组未公开接口。
const (
	ccPathWhoami        = "/alpha/whoami"
	ccPathCredits       = "/alpha/billing/credits"
	ccPathSubscriptions = "/alpha/billing/subscriptions"
	ccPathSummary       = "/alpha/usage/summary"
)

func commandCodeTemplate() usageTemplate {
	return usageTemplate{
		info: model.UsageTemplateInfo{
			ID:          "commandcode",
			Name:        "Command Code",
			Description: "读取剩余额度、5 小时/每周限额与计费周期统计",
		},
		fetch: fetchCommandCodeUsage,
	}
}

func fetchCommandCodeUsage(ctx context.Context, cli *usageClient) (*model.UsageSnapshot, error) {
	snap := &model.UsageSnapshot{}

	// whoami 单独先发：它最便宜，而且只有它能判断密钥是否有用。
	// 它的 org.id 用来给后三个接口加作用域；个人账号下 org 是 null，
	// 此时必须完全不传 orgId 参数，而不是传空串。
	orgID := ""
	status, body, err := cli.get(ctx, ccPathWhoami, url.Values{"limits": {"1"}})
	if err != nil {
		return nil, fmt.Errorf("无法连接 Command Code: %w", err)
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, fmt.Errorf("密钥被拒绝（HTTP %d），请检查该供应商的密钥是否有效", status)
	case status < 200 || status >= 300:
		snap.Warnings = append(snap.Warnings,
			fmt.Sprintf("账号信息不可用（whoami 返回 HTTP %d）", status))
	default:
		who, parseErr := parseCCWhoami(body)
		if parseErr != nil {
			snap.Warnings = append(snap.Warnings, "账号信息无法识别："+parseErr.Error())
		} else {
			snap.Account = who.login
			orgID = who.orgID
		}
	}

	scope := func(extra url.Values) url.Values {
		q := url.Values{}
		if orgID != "" {
			q.Set("orgId", orgID)
		}
		for k, v := range extra {
			q[k] = v
		}
		return q
	}

	// credits 与 subscriptions 互不依赖，并发发；summary 依赖订阅周期的起点，最后发。
	var (
		wg                       sync.WaitGroup
		creditsBody, subBody     []byte
		creditsStatus, subStatus int
		creditsErr, subErr       error
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		creditsStatus, creditsBody, creditsErr = cli.get(ctx, ccPathCredits, scope(nil))
	}()
	go func() {
		defer wg.Done()
		subStatus, subBody, subErr = cli.get(ctx, ccPathSubscriptions, scope(nil))
	}()
	wg.Wait()

	// 额度是这次查询的核心答案。它读不出来，这次查询就算失败 ——
	// 剩余额度是最不能靠猜的数字。
	var credits *ccCredits
	if creditsErr != nil || creditsStatus < 200 || creditsStatus >= 300 {
		snap.Warnings = append(snap.Warnings, fmt.Sprintf("额度不可用（billing/credits 返回 HTTP %d）", creditsStatus))
	} else if parsed, parseErr := parseCCCredits(creditsBody); parseErr != nil {
		snap.Warnings = append(snap.Warnings, "额度结构无法识别："+parseErr.Error())
	} else {
		snap.OK = true
		snap.Balances = parsed.balances
		snap.Windows = parsed.windows
		credits = &parsed
	}

	periodStart := ""
	if subStatus >= 200 && subStatus < 300 && subErr == nil {
		if sub, parseErr := parseCCSubscription(subBody); parseErr != nil {
			snap.Warnings = append(snap.Warnings, "订阅信息无法识别："+parseErr.Error())
		} else {
			snap.PlanID = sub.planID
			snap.PlanName = ccPlanName(sub.planID)
			snap.Status = sub.status
			snap.PeriodStart = sub.periodStart
			snap.PeriodEnd = sub.periodEnd
			if sub.periodStart != nil {
				periodStart = sub.periodStart.UTC().Format(time.RFC3339)
			}
		}
	} else {
		snap.Warnings = append(snap.Warnings,
			fmt.Sprintf("订阅信息不可用（billing/subscriptions 返回 HTTP %d）", subStatus))
	}

	extra := url.Values{}
	if periodStart != "" {
		// 按参考实现原样透传：这是产生该周期的服务自己给出的时间点，不该由这一侧重新格式化。
		extra.Set("since", periodStart)
	}
	sumStatus, sumBody, sumErr := cli.get(ctx, ccPathSummary, scope(extra))
	if sumErr == nil && sumStatus >= 200 && sumStatus < 300 {
		if sum, parseErr := parseCCSummary(sumBody); parseErr != nil {
			snap.Warnings = append(snap.Warnings, "周期统计无法识别："+parseErr.Error())
		} else {
			snap.Period = sum.stats

			// 月度窗口。上游只把 5 小时和每周做成"窗口"，月度只以「剩余额度」的形式存在，
			// 所以要自己算：能花的上限 = 本周期已花 + 月度剩余额度。
			// 两个数都读到才算得出来，缺一个就干脆不显示这个窗口 —— 宁可少一格，
			// 也不能拿一个半截的比值去展示「月度用了多少」。
			if credits != nil && credits.monthly != nil && sum.totalCost != nil {
				cap := *credits.monthly + *sum.totalCost
				w := model.UsageWindow{
					Label:   "本月",
					Used:    *sum.totalCost,
					Cap:     cap,
					ResetAt: snap.PeriodEnd,
					Hint:    "上限 = 本周期已花 + 月度剩余额度（上游没有单独的月度窗口，这个比值是推导出来的）",
				}
				if cap > 0 {
					w.Percent = *sum.totalCost / cap * 100
				}
				snap.Windows = append(snap.Windows, w)
			}
		}
	} else {
		snap.Warnings = append(snap.Warnings,
			fmt.Sprintf("周期统计不可用（usage/summary 返回 HTTP %d）", sumStatus))
	}

	return snap, nil
}

type ccWhoami struct {
	login string
	orgID string
}

func parseCCWhoami(body []byte) (ccWhoami, error) {
	var root struct {
		Org *struct {
			ID    string `json:"id"`
			Login string `json:"login"`
		} `json:"org"`
		User *struct {
			UserName    string `json:"userName"`
			Name        string `json:"name"`
			DisplayName string `json:"displayName"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return ccWhoami{}, errors.New("响应不是合法的 JSON")
	}

	var out ccWhoami
	if root.Org != nil {
		out.orgID = root.Org.ID
		out.login = root.Org.Login
	}
	if out.login == "" && root.User != nil {
		out.login = root.User.UserName
		if out.login == "" {
			out.login = root.User.Name
		}
		if out.login == "" {
			out.login = root.User.DisplayName
		}
	}
	if out.login == "" && out.orgID == "" {
		return out, errors.New("既没有 user 也没有 org 字段")
	}
	return out, nil
}

type ccCredits struct {
	balances []model.UsageBalance
	windows  []model.UsageWindow
	// monthly 是月度剩余额度，单独留一份指针：credits 里只有 5 小时和每周两个窗口，
	// 月度上限要靠它和周期花费推出来。
	monthly *float64
}

// ccWindow 的一个字段全部用指针：区分「上游明确给 0」和「上游没有这个字段」——
// 后者要报结构不可识别，而不是当成 0 额度展示。
type ccWindow struct {
	Used     *float64        `json:"used"`
	Cap      *float64        `json:"cap"`
	Exceeded *bool           `json:"exceeded"`
	ResetAt  json.RawMessage `json:"resetAt"`
}

func parseCCCredits(body []byte) (ccCredits, error) {
	var root struct {
		Credits *struct {
			Monthly   *float64 `json:"monthlyCredits"`
			Purchased *float64 `json:"purchasedCredits"`
			Free      *float64 `json:"freeCredits"`
		} `json:"credits"`
		WindowLimits *struct {
			FiveHour *ccWindow `json:"fiveHour"`
			Weekly   *ccWindow `json:"weekly"`
		} `json:"windowLimits"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return ccCredits{}, errors.New("响应不是合法的 JSON")
	}
	if root.Credits == nil ||
		(root.Credits.Monthly == nil && root.Credits.Purchased == nil && root.Credits.Free == nil) {
		return ccCredits{}, errors.New("没有 credits.monthlyCredits / purchasedCredits / freeCredits 字段")
	}

	var out ccCredits
	out.monthly = root.Credits.Monthly
	balances := []struct {
		label string
		value *float64
	}{
		{"月度额度", root.Credits.Monthly},
		{"充值额度", root.Credits.Purchased},
		{"赠送额度", root.Credits.Free},
	}
	for _, b := range balances {
		if b.value == nil {
			continue
		}
		out.balances = append(out.balances, model.UsageBalance{Label: b.label, Amount: *b.value})
	}

	if root.WindowLimits != nil {
		for _, w := range []struct {
			label string
			node  *ccWindow
		}{
			{"5 小时", root.WindowLimits.FiveHour},
			{"每周", root.WindowLimits.Weekly},
		} {
			if w.node == nil || w.node.Used == nil || w.node.Cap == nil {
				continue
			}
			// 上游给未启用窗口时会回 {used:0, cap:0}，这种不算限额，不展示。
			if *w.node.Used == 0 && *w.node.Cap == 0 {
				continue
			}
			item := model.UsageWindow{
				Label:   w.label,
				Used:    *w.node.Used,
				Cap:     *w.node.Cap,
				ResetAt: ccEpochMillis(w.node.ResetAt),
			}
			if w.node.Cap != nil && *w.node.Cap > 0 {
				item.Percent = *w.node.Used / *w.node.Cap * 100
			}
			if w.node.Exceeded != nil {
				item.Exceeded = *w.node.Exceeded
			}
			out.windows = append(out.windows, item)
		}
	}
	return out, nil
}

type ccSubscription struct {
	planID      string
	status      string
	periodStart *time.Time
	periodEnd   *time.Time
}

func parseCCSubscription(body []byte) (ccSubscription, error) {
	var root struct {
		Data *struct {
			PlanID             string `json:"planId"`
			Status             string `json:"status"`
			CurrentPeriodStart string `json:"currentPeriodStart"`
			CurrentPeriodEnd   string `json:"currentPeriodEnd"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return ccSubscription{}, errors.New("响应不是合法的 JSON")
	}
	// 四个接口里只有 subscriptions 把内容包在 data 下，其余都在顶层。
	if root.Data == nil {
		return ccSubscription{}, errors.New("没有 data 字段")
	}
	return ccSubscription{
		planID:      root.Data.PlanID,
		status:      root.Data.Status,
		periodStart: ccTime(root.Data.CurrentPeriodStart),
		periodEnd:   ccTime(root.Data.CurrentPeriodEnd),
	}, nil
}

// ccSummary 是周期统计的解析结果。
type ccSummary struct {
	stats []model.UsageStat
	// totalCost 单独留一份：推导月度窗口要用它
	totalCost *float64
}

func parseCCSummary(body []byte) (ccSummary, error) {
	var root struct {
		TotalCost      *float64 `json:"totalCost"`
		TotalCount     *float64 `json:"totalCount"`
		AverageCost    *float64 `json:"averageCost"`
		SuccessRate    *float64 `json:"successRate"`
		TotalTokensIn  *float64 `json:"totalTokensIn"`
		TotalTokensOut *float64 `json:"totalTokensOut"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return ccSummary{}, errors.New("响应不是合法的 JSON")
	}
	if root.TotalCost == nil && root.TotalCount == nil {
		return ccSummary{}, errors.New("没有 totalCost / totalCount 字段")
	}

	stats := make([]model.UsageStat, 0, 6)
	add := func(label string, value *float64, unit, hint string) {
		if value == nil {
			return
		}
		stats = append(stats, model.UsageStat{Label: label, Value: *value, Unit: unit, Hint: hint})
	}
	add("周期内花费", root.TotalCost, model.UnitUSD, "")
	add("周期内请求数", root.TotalCount, model.UnitCount, "")
	add("成功率", root.SuccessRate, model.UnitPercent, "")
	add("平均每次请求", root.AverageCost, model.UnitUSD, "按上游记账口径，含全部模型")
	add("输入 tokens", root.TotalTokensIn, model.UnitTokens, "含缓存命中部分")
	add("输出 tokens", root.TotalTokensOut, model.UnitTokens, "")
	return ccSummary{stats: stats, totalCost: root.TotalCost}, nil
}

// ccPlanName 把上游的计划 id 译成界面上的名字。认不出来就原样返回，
// 这样上游新增套餐时至少能看到 id，而不是显示成空白。
func ccPlanName(id string) string {
	if id == "" {
		return ""
	}
	names := map[string]string{
		"individual-go":       "Go",
		"individual-goat":     "GOAT",
		"individual-pro":      "Pro",
		"individual-max-10x":  "Max 10×",
		"individual-max-20x":  "Max 20×",
		"individual-ultra":    "Ultra",
		"individual-provider": "Provider",
		"team-pro":            "Team Pro",
	}
	if n, ok := names[strings.ToLower(id)]; ok {
		return n
	}
	return id
}

// ccEpochMillis 解析毫秒时间戳。上游给的是 JSON 数字，但也容忍字符串写法；
// 解析不出来就返回 nil（少一个重置时间，不影响其余数字的正确性）。
func ccEpochMillis(raw json.RawMessage) *time.Time {
	if len(raw) == 0 {
		return nil
	}
	s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
	if s == "" || s == "null" {
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return nil
	}
	t := time.UnixMilli(int64(f)).UTC()
	return &t
}

// ccTime 解析 ISO 8601 时间串，失败返回 nil。
func ccTime(s string) *time.Time {
	if s == "" {
		return nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil
	}
	return &t
}

// ---------- DeepSeek 模板 ----------

// DeepSeek 的查询余额接口。这是官方公开文档里的接口，与其它模板的「未公开接口」性质不同：
// https://api-docs.deepseek.com/zh-cn/api/get-user-balance
//
// 生成接口挂在 /v1 下，而这个接口挂在站点根（/user/balance），
// 所以 deriveUsageBaseURL 剥掉 /v1 后拼上它即可。
const dsPathBalance = "/user/balance"

func deepSeekTemplate() usageTemplate {
	return usageTemplate{
		info: model.UsageTemplateInfo{
			ID:          "deepseek",
			Name:        "DeepSeek",
			Description: "官方 /user/balance，读取总余额、充值余额与赠金余额（含币种）",
		},
		fetch: fetchDeepSeekUsage,
	}
}

// dsBalanceInfo 是 DeepSeek 返回的一条币种余额。金额字段是字符串（如 "110.00"），
// 所以用字符串接、再解析成 float，避免直接把 JSON 数字塞进 float 时因格式差异失败。
type dsBalanceInfo struct {
	Currency        string `json:"currency"`
	TotalBalance    string `json:"total_balance"`
	GrantedBalance  string `json:"granted_balance"`
	ToppedUpBalance string `json:"topped_up_balance"`
}

func fetchDeepSeekUsage(ctx context.Context, cli *usageClient) (*model.UsageSnapshot, error) {
	snap := &model.UsageSnapshot{}

	status, body, err := cli.get(ctx, dsPathBalance, nil)
	if err != nil {
		return nil, fmt.Errorf("无法连接 DeepSeek: %w", err)
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, fmt.Errorf("密钥被拒绝（HTTP %d），请检查该供应商的密钥是否有效", status)
	case status < 200 || status >= 300:
		snap.Warnings = append(snap.Warnings,
			fmt.Sprintf("余额接口不可用（/user/balance 返回 HTTP %d）", status))
		return snap, nil
	}

	parsed, parseErr := parseDeepSeekBalance(body)
	if parseErr != nil {
		snap.Warnings = append(snap.Warnings, "余额结构无法识别："+parseErr.Error())
		return snap, nil
	}
	snap.OK = true
	snap.Balances = parsed.balances
	snap.Status = parsed.status
	return snap, nil
}

type dsBalance struct {
	balances []model.UsageBalance
	status   string
}

// parseDeepSeekBalance 解析官方响应：
//
//	{"is_available":true,"balance_infos":[
//	  {"currency":"CNY","total_balance":"110.00","granted_balance":"10.00","topped_up_balance":"100.00"}]}
//
// balance_infos 是一个数组（可能同时有 CNY 与 USD），每一项拆成「总余额 / 充值余额 / 赠金余额」
// 三条，币种跟着每条走 —— 否则人民币余额会被按美元格式化显示。
// 金额是字符串，解析失败的那一条直接跳过（绝不回落成 0）。
func parseDeepSeekBalance(body []byte) (dsBalance, error) {
	var root struct {
		IsAvailable  *bool           `json:"is_available"`
		BalanceInfos []dsBalanceInfo `json:"balance_infos"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return dsBalance{}, errors.New("响应不是合法的 JSON")
	}
	if len(root.BalanceInfos) == 0 {
		return dsBalance{}, errors.New("没有 balance_infos 字段")
	}

	var out dsBalance
	if root.IsAvailable != nil {
		if *root.IsAvailable {
			out.status = "可用"
		} else {
			out.status = "余额不足"
		}
	}
	for _, info := range root.BalanceInfos {
		currency := strings.ToUpper(strings.TrimSpace(info.Currency))
		for _, item := range []struct {
			label string
			raw   string
			total bool
		}{
			// total_balance 是「总的可用余额，包括赠金和充值余额」，也就是真正的剩余额度；
			// 下面两条是它的组成。标记 Total 让界面拿它当剩余额度，而不是把三项相加。
			{"总余额", info.TotalBalance, true},
			{"充值余额", info.ToppedUpBalance, false},
			{"赠金余额", info.GrantedBalance, false},
		} {
			v, ok := parseMoneyString(item.raw)
			if !ok {
				continue
			}
			out.balances = append(out.balances, model.UsageBalance{
				Label:    item.label,
				Amount:   v,
				Currency: currency,
				Total:    item.total,
			})
		}
	}
	if len(out.balances) == 0 {
		return out, errors.New("balance_infos 里没有可解析的金额")
	}
	return out, nil
}

// ---------- OpenCode 模板 ----------

// OpenCode Go 的用量接口。它是订阅制（Go 计划）的配额窗口，返回的是一个百分比，
// 不是金额 —— 所以这个模板只填三根窗口条，不像 commandcode 那样有余额与周期花费。
//
// 接口固定在站点根的 /zen/go/v1/usage 下；用户填的 BaseURL 可能是 https://opencode.ai/zen/v1
// 也可能是 https://opencode.ai/zen/go/v1，剥 /v1 后推导出来的路径基准并不一致，所以这里
// 用 cli.origin 拼绝对路径，两种配置都能命中。
const ocUsagePath = "/zen/go/v1/usage"

func openCodeTemplate() usageTemplate {
	return usageTemplate{
		info: model.UsageTemplateInfo{
			ID:          "opencode",
			Name:        "OpenCode Go",
			Description: "读取 Go 订阅的 5 小时/每周/每月配额百分比与重置时间（需已订阅 Go）",
		},
		fetch: fetchOpenCodeUsage,
	}
}

// ocWindow 是一个配额窗口。上游只保证有 percent 与 resetsAt，两个都用指针/原始值接以容忍缺失。
type ocWindow struct {
	Percent  *float64        `json:"percent"`
	ResetAt  json.RawMessage `json:"resetsAt"`
	ResetSec *float64        `json:"resetInSec"`
}

func fetchOpenCodeUsage(ctx context.Context, cli *usageClient) (*model.UsageSnapshot, error) {
	snap := &model.UsageSnapshot{}

	if cli.origin == "" {
		return nil, errors.New("无法从 BaseURL 推导 OpenCode 站点根地址，请在「用量」页手填查询地址")
	}
	status, body, err := cli.get(ctx, cli.origin+ocUsagePath, nil)
	if err != nil {
		return nil, fmt.Errorf("无法连接 OpenCode: %w", err)
	}
	switch {
	case status == http.StatusUnauthorized || status == http.StatusForbidden:
		return nil, fmt.Errorf("密钥被拒绝或该账号未订阅 OpenCode Go（HTTP %d）", status)
	case status < 200 || status >= 300:
		snap.Warnings = append(snap.Warnings,
			fmt.Sprintf("用量接口不可用（/zen/go/v1/usage 返回 HTTP %d）；该接口只对已订阅 Go 的账号开放", status))
		return snap, nil
	}

	parsed, parseErr := parseOpenCodeUsage(body)
	if parseErr != nil {
		snap.Warnings = append(snap.Warnings, "用量结构无法识别："+parseErr.Error())
		return snap, nil
	}
	if len(parsed.windows) == 0 {
		snap.Warnings = append(snap.Warnings,
			"上游没有返回任何配额窗口；该账号可能未订阅 OpenCode Go，或接口结构已变更")
		return snap, nil
	}
	snap.OK = true
	snap.Windows = parsed.windows
	snap.PlanName = parsed.plan
	return snap, nil
}

type ocUsage struct {
	windows []model.UsageWindow
	plan    string
}

// parseOpenCodeUsage 解析 {"usage":{"rolling":{...},"weekly":{...},"monthly":{...}},"plan":"..."}。
//
// 上游只给 percent（0-100），不给金额，所以 Used/Cap 一律留 0，只把 percent 转进去，
// 并写明这个百分比没有金额口径 —— 编一个美元数出来比留空更糟。
func parseOpenCodeUsage(body []byte) (ocUsage, error) {
	var root struct {
		Plan  string `json:"plan"`
		Usage *struct {
			Rolling *ocWindow `json:"rolling"`
			FiveH   *ocWindow `json:"fiveHour"`
			Weekly  *ocWindow `json:"weekly"`
			Monthly *ocWindow `json:"monthly"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return ocUsage{}, errors.New("响应不是合法的 JSON")
	}
	if root.Usage == nil {
		return ocUsage{}, errors.New("没有 usage 字段")
	}

	var out ocUsage
	out.plan = strings.TrimSpace(root.Plan)
	if out.plan != "" && !strings.EqualFold(out.plan, "go") {
		out.plan = "OpenCode " + out.plan
	}
	rolling := root.Usage.Rolling
	if rolling == nil {
		rolling = root.Usage.FiveH // 少数实现把它叫 fiveHour
	}
	for _, w := range []struct {
		label string
		node  *ocWindow
	}{
		{"5 小时", rolling},
		{"每周", root.Usage.Weekly},
		{"本月", root.Usage.Monthly},
	} {
		if w.node == nil || w.node.Percent == nil {
			continue
		}
		item := model.UsageWindow{
			Label:   w.label,
			Percent: *w.node.Percent,
			ResetAt: ocResetTime(w.node.ResetAt, w.node.ResetSec),
			Hint:    "上游只返回用量百分比，没有金额口径，因此不显示金额",
		}
		out.windows = append(out.windows, item)
	}
	return out, nil
}

// ocResetTime 解析重置时间。上游可能给 ISO8601 串、epoch 秒或 epoch 毫秒，
// 也可能只给「还有多少秒」。都试一遍，解析不出来就留空（不影响百分比）。
func ocResetTime(raw json.RawMessage, resetSec *float64) *time.Time {
	if len(raw) > 0 {
		s := strings.Trim(strings.TrimSpace(string(raw)), `"`)
		if s != "" && s != "null" {
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				return &t
			}
			if f, err := strconv.ParseFloat(s, 64); err == nil && f > 0 {
				return epochToTime(f)
			}
		}
	}
	if resetSec != nil && *resetSec > 0 {
		t := time.Now().Add(time.Duration(*resetSec) * time.Second).UTC()
		return &t
	}
	return nil
}

// epochToTime 把 epoch 时间戳转成时间。大于 1e11 视为毫秒，否则视为秒 ——
// 秒级时间戳要到公元 5138 年才会超过这个量级，是一个安全的判别点。
func epochToTime(v float64) *time.Time {
	var t time.Time
	if v > 1e11 {
		t = time.UnixMilli(int64(v)).UTC()
	} else {
		t = time.Unix(int64(v), 0).UTC()
	}
	return &t
}

// parseMoneyString 解析 DeepSeek 那种字符串金额，空串或非数字返回 false。
func parseMoneyString(s string) (float64, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}

// ---------- Mistral 模板 ----------
//
// 数据分两处，认证也分两套：
//
//   - 预算（API / Vibe 额度与重置时间）与订阅计划在官方 API 上：GET /v1/billing/budget
//     与 GET /v1/billing/subscription，认普通 API key。2026-11-18 实测均返回 200，
//     结构稳定且带 OpenAPI 定义（beta.billing 标签）。
//   - 本月 token 明细与限额状态还挂在网页后台（admin.mistral.ai），只认账号密码
//     登录换来的会话 token（见 mistralauth.go）。这一部分是可选的。
//
// 早先整份数据都靠抓 admin 的 SSR 页面（解析 RSC flight 里内嵌的 budget JSON），
// 那套做法的脆弱点很明显：页面改版就失效、还得让用户把网页密码明文交出来。
// 现在预算与计划改走官方接口，账号密码只在「想额外看 token 明细」时才需要填。

// Mistral 官方接口路径。
const (
	mistralTemplateID = "mistral"

	// 官方 API：预算与计划。挂在生成接口同一个站点根上（api.mistral.ai）。
	mistralPathBudget       = "/v1/billing/budget"
	mistralPathSubscription = "/v1/billing/subscription"

	// 网页后台：token 明细与限额状态。挂在 admin.mistral.ai 上，需要会话 token。
	mistralAdminPathLimits = "/api/billing/limits"
	mistralAdminPathUsage  = "/api/billing/v2/usage"
)

// mistralAdminOrigin 是网页后台的站点根。写成变量是为了让测试能指向 httptest；
// 生产路径上它恒为 admin.mistral.ai（见 mistralauth.go 里的常量）。
var mistralAdminOrigin = mistralAdminOriginConst

func mistralTemplate() usageTemplate {
	return usageTemplate{
		info: model.UsageTemplateInfo{
			ID:   mistralTemplateID,
			Name: "Mistral",
			Description: "用 API key 读取官方 /v1/billing 的预算窗口（API / Vibe）与订阅计划；" +
				"再填 admin.mistral.ai 的账号密码可额外读到本月 token 明细与限额状态",
		},
		fetch: fetchMistralUsage,
	}
}

// mistralBudget 是一条预算额度。usage_percentage 是 0-100 的百分数
// （实测 vibe_budget 返回 20.81，表示已用 20.81%），与 UsageWindow.Percent 同口径。
type mistralBudget struct {
	UsagePercentage float64 `json:"usage_percentage"`
	InitialBudget   float64 `json:"initial_budget"`
	Currency        string  `json:"currency"`
	ResetAt         string  `json:"reset_at"`
}

// mistralBudgetRoot 是 /v1/billing/budget 的响应结构。
//
// 顶层的 usage_percentage / initial_budget / reset_at 与 api_budget 是同一份数据
// （实测两者完全一致），保留内嵌是为了 api_budget 缺失时能回退。
type mistralBudgetRoot struct {
	mistralBudget
	APIBudget  *mistralBudget `json:"api_budget"`
	VibeBudget *mistralBudget `json:"vibe_budget"`
}

// mistralSubscriptionInfo 是 /v1/billing/subscription 的响应结构。
type mistralSubscriptionInfo struct {
	PlanName        string `json:"plan_name"`         // FREE / INDIVIDUAL / EDU / TEAM
	PaygEnabled     bool   `json:"payg_enabled"`      // 是否开通按量付费（API 订阅）
	VibePaygEnabled bool   `json:"vibe_payg_enabled"` // Vibe Code 的按量付费
	Enterprise      bool   `json:"enterprise"`        // 是否企业版
}

// mistralResult 是一次接口调用的结果。
type mistralResult struct {
	status int
	body   []byte
	err    error
}

func fetchMistralUsage(ctx context.Context, cli *usageClient) (*model.UsageSnapshot, error) {
	snap := &model.UsageSnapshot{}

	// 认 API key 的那两个（官方接口），拼在 cli.baseURL 上。
	apiGet := func(path string) mistralResult {
		st, body, err := cli.get(ctx, path, nil)
		return mistralResult{status: st, body: body, err: err}
	}
	// 认网页会话的那两个（admin 后台）。没配账号密码时 cli.sessionGet 为 nil。
	adminGet := func(path string, query url.Values) (mistralResult, bool) {
		if cli.sessionGet == nil {
			return mistralResult{}, false
		}
		st, body, err := cli.sessionGet(ctx, path, query)
		return mistralResult{status: st, body: body, err: err}, true
	}
	budget := apiGet(mistralPathBudget)
	sub := apiGet(mistralPathSubscription)

	// 两个官方接口都 401/403 时，用户最需要知道的是「这把 key 不对」，
	// 而不是两条「HTTP 401」的泛化警告。
	if isHTTPAuthFailure(budget) && isHTTPAuthFailure(sub) {
		return nil, errors.New("Mistral 拒绝了这把 API 密钥（HTTP 401/403），请检查供应商密钥是否正确")
	}

	okCount := 0

	// ---------- 额度窗口 ----------
	if budget.err != nil {
		snap.Warnings = append(snap.Warnings,
			fmt.Sprintf("预算接口不可用（无法连接）：%v", budget.err))
	} else if budget.status < 200 || budget.status >= 300 {
		snap.Warnings = append(snap.Warnings,
			fmt.Sprintf("预算接口不可用（HTTP %d）", budget.status))
	} else if windows, err := parseMistralBudget(budget.body); err != nil {
		snap.Warnings = append(snap.Warnings, "预算结构无法识别："+err.Error())
	} else {
		okCount++
		snap.Windows = append(snap.Windows, windows...)
	}

	// ---------- 订阅计划 ----------
	if sub.err != nil {
		snap.Warnings = append(snap.Warnings,
			fmt.Sprintf("订阅接口不可用（无法连接）：%v", sub.err))
	} else if sub.status < 200 || sub.status >= 300 {
		snap.Warnings = append(snap.Warnings,
			fmt.Sprintf("订阅接口不可用（HTTP %d）", sub.status))
	} else if info, err := parseMistralSubscription(sub.body); err != nil {
		snap.Warnings = append(snap.Warnings, "订阅结构无法识别："+err.Error())
	} else {
		okCount++
		snap.PlanName, snap.Status = info.planName, info.status
		snap.Warnings = append(snap.Warnings, info.warnings...)
	}

	// ---------- 可选：本月限额状态 ----------
	limits, hasSession := adminGet(mistralAdminPathLimits, nil)
	switch {
	case !hasSession:
		// 只有「本来就没填凭据」才这么说。填了但登录失败的情况在 QueryUsage 里
		// 已经记过一条明确的警告，这里不能再冒一句「未填写」自相矛盾。
		if !cli.sessionAttempted {
			snap.Warnings = append(snap.Warnings,
				"未填写账号密码，已跳过本月限额状态与 token 明细（预算与计划不受影响）")
		}
	case limits.err != nil:
		snap.Warnings = append(snap.Warnings,
			fmt.Sprintf("限额接口不可用（无法连接）：%v", limits.err))
	case limits.status < 200 || limits.status >= 300:
		snap.Warnings = append(snap.Warnings,
			fmt.Sprintf("限额接口不可用（HTTP %d）", limits.status))
	default:
		if stats, warnings, err := parseMistralLimits(limits.body); err != nil {
			snap.Warnings = append(snap.Warnings, "限额结构无法识别："+err.Error())
		} else {
			okCount++
			snap.Period = append(snap.Period, stats...)
			snap.Warnings = append(snap.Warnings, warnings...)
		}
	}

	// ---------- 可选：本月 token 明细 ----------
	if hasSession {
		now := time.Now().UTC()
		query := url.Values{}
		query.Set("month", strconv.Itoa(int(now.Month())))
		query.Set("year", strconv.Itoa(now.Year()))

		usage, _ := adminGet(mistralAdminPathUsage, query)
		switch {
		case usage.err != nil:
			snap.Warnings = append(snap.Warnings,
				fmt.Sprintf("用量接口不可用（无法连接）：%v", usage.err))
		case usage.status < 200 || usage.status >= 300:
			snap.Warnings = append(snap.Warnings,
				fmt.Sprintf("用量接口不可用（HTTP %d）", usage.status))
		default:
			if stats, err := parseMistralUsageTokens(usage.body); err != nil {
				snap.Warnings = append(snap.Warnings, "用量结构无法识别："+err.Error())
			} else {
				okCount++
				snap.Period = append(snap.Period, stats...)
			}
		}
	}

	if okCount == 0 {
		return nil, errors.New("Mistral 的所有接口都没能读到数据，请检查密钥、账号密码与网络代理")
	}
	snap.OK = true
	return snap, nil
}

// isHTTPAuthFailure 判断一次调用是不是被认证拒绝（网络错误不算）。
func isHTTPAuthFailure(r mistralResult) bool {
	return r.err == nil && (r.status == http.StatusUnauthorized || r.status == http.StatusForbidden)
}

// parseMistralBudget 把官方 /v1/billing/budget 的响应映射成额度窗口。//
// 实测响应（2026-11-18）：
//
//	{"vibe_budget":{"usage_percentage":20.81,"initial_budget":300.0,"currency":"USD",
//	  "reset_at":"2026-12-01T00:00:00Z","payg_enabled":false},
//	 "api_budget":{"usage_percentage":0.0,"initial_budget":30.0,"currency":"USD",
//	  "reset_at":"2026-12-01T00:00:00Z","payg_enabled":false},
//	 "usage_percentage":0.0,"initial_budget":30.0,"currency":"USD","reset_at":"..."}
func parseMistralBudget(body []byte) ([]model.UsageWindow, error) {
	var root mistralBudgetRoot
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, errors.New("响应不是合法的 JSON")
	}

	// 顶层 budget 与 api_budget 实测是同一份数据，优先用 api_budget，
	// 缺了再退回顶层，免得同一条额度重复展示。
	//
	// 回退要有条件：顶层那三个字段全为空值时说明响应里根本没有预算数据，
	// 这时不能造一条「额度 0 / 已用 0」出来 —— 把「读不到」显示成「额度为 0」
	// 会直接误导用户（见 model.UsageSnapshot 的设计约束）。
	api := root.APIBudget
	if api == nil {
		if root.InitialBudget == 0 && root.ResetAt == "" && root.Currency == "" {
			return nil, errors.New("没有 api_budget / vibe_budget")
		}
		fallback := root.mistralBudget
		api = &fallback
	}
	if api == nil && root.VibeBudget == nil {
		return nil, errors.New("没有 api_budget / vibe_budget")
	}

	var windows []model.UsageWindow
	for _, item := range []struct {
		label string
		node  *mistralBudget
		hint  string
	}{
		{"API 额度", api, "PRO 计划包含的 API 预算，按周期重置"},
		{"Vibe 额度", root.VibeBudget, "PRO 计划包含的 Vibe 预算，按周期重置"},
	} {
		if item.node == nil {
			continue
		}
		w := model.UsageWindow{
			Label:   item.label,
			Percent: item.node.UsagePercentage,
			Cap:     item.node.InitialBudget,
			// 上游只给百分比与总额，已用量按两者相乘算出来，并写进 Hint 说明来源。
			Used:    item.node.InitialBudget * item.node.UsagePercentage / 100,
			ResetAt: mistralResetTime(item.node.ResetAt),
			Hint:    item.hint,
		}
		if item.node.Currency != "" {
			w.Hint += "（" + strings.ToUpper(item.node.Currency) + "）"
		}
		windows = append(windows, w)
	}
	if len(windows) == 0 {
		return nil, errors.New("没有 api_budget / vibe_budget")
	}
	return windows, nil
}

// parseMistralSubscription 解析官方 /v1/billing/subscription。
//
// 实测响应：{"plan_name":"INDIVIDUAL","payg_enabled":false,
// "vibe_payg_enabled":false,"enterprise":false}
func parseMistralSubscription(body []byte) (mistralSubscription, error) {
	var out mistralSubscription
	var info mistralSubscriptionInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return out, errors.New("响应不是合法的 JSON")
	}
	if info.PlanName == "" {
		return out, errors.New("没有 plan_name 字段")
	}

	out.planName = mistralPlanText(info.PlanName, info)
	out.status = "活跃"
	if info.PlanName == "FREE" {
		out.status = "免费版"
	}
	if info.PaygEnabled {
		out.warnings = append(out.warnings, "已开通按量付费（API 用量超出预算后继续计费）")
	}
	return out, nil
}

// mistralPlanText 把上游的计划名渲染成一句人话，顺带带上企业与按量付费标记。
func mistralPlanText(planName string, info mistralSubscriptionInfo) string {
	parts := []string{planName}
	if info.Enterprise {
		parts = append(parts, "企业版")
	}
	if info.VibePaygEnabled {
		parts = append(parts, "Vibe 按量付费")
	}
	return strings.Join(parts, " / ")
}

// mistralSubscription 是订阅计划解析出来的内容。
type mistralSubscription struct {
	planName string
	status   string
	warnings []string
}

// mistralResetTime 解析重置时间（实测是 RFC3339，如 2026-12-01T00:00:00Z）。
func mistralResetTime(raw string) *time.Time {
	if t, ok := parseMistralTime(raw); ok {
		return &t
	}
	return nil
}

// parseMistralLimits 解析本月用量与限额状态。
//
// 实测响应：
//
//	{"limits":{"completion":{"no_monthly_limit":true,"monthly_limit_reached":false,
//	  "usage":0.0,"vibe_usage":0.0,"total_usage":0.0,"usage_limit":null},
//	  "last_payment_failure":false,"currency":"USD"}}
func parseMistralLimits(body []byte) ([]model.UsageStat, []string, error) {
	var root struct {
		Limits *struct {
			Completion *struct {
				NoMonthlyLimit      *bool    `json:"no_monthly_limit"`
				MonthlyLimitReached *bool    `json:"monthly_limit_reached"`
				Usage               *float64 `json:"usage"`
				VibeUsage           *float64 `json:"vibe_usage"`
				TotalUsage          *float64 `json:"total_usage"`
				UsageLimit          *float64 `json:"usage_limit"`
			} `json:"completion"`
			LastPaymentFailure *bool  `json:"last_payment_failure"`
			Currency           string `json:"currency"`
		} `json:"limits"`
	}
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, nil, errors.New("响应不是合法的 JSON")
	}
	if root.Limits == nil || root.Limits.Completion == nil {
		return nil, nil, errors.New("没有 limits.completion 字段")
	}
	c := root.Limits.Completion

	var stats []model.UsageStat
	add := func(label string, v *float64) {
		if v == nil {
			return
		}
		stats = append(stats, model.UsageStat{Label: label, Value: *v, Unit: model.UnitUSD})
	}
	add("本月 API 用量", c.Usage)
	add("本月 Vibe 用量", c.VibeUsage)
	add("本月合计用量", c.TotalUsage)
	if c.UsageLimit != nil {
		stats = append(stats, model.UsageStat{
			Label: "月度限额", Value: *c.UsageLimit, Unit: model.UnitUSD,
		})
	}

	var warnings []string
	if c.MonthlyLimitReached != nil && *c.MonthlyLimitReached {
		warnings = append(warnings, "本月用量已达上限，后续请求会被拒绝")
	}
	if c.NoMonthlyLimit != nil && *c.NoMonthlyLimit {
		warnings = append(warnings, "该账号未设置月度限额（按量计费不受上限约束）")
	}
	if root.Limits.LastPaymentFailure != nil && *root.Limits.LastPaymentFailure {
		warnings = append(warnings, "最近一笔支付失败，请检查支付方式")
	}
	return stats, warnings, nil
}

// parseMistralUsageTokens 汇总本月的输入/输出/缓存 token 总量。
//
// 响应按「分类 → 模型 → input/output/cached 数组」组织，每个数组元素是一条
// 按日期的用量记录。这里把全部模型的 value 加总，只出三条总量。
func parseMistralUsageTokens(body []byte) ([]model.UsageStat, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(body, &root); err != nil {
		return nil, errors.New("响应不是合法的 JSON")
	}

	// 只统计对话类与 Vibe 的 token；OCR / 音频 / 微调按页、秒、小时计费，
	// 混进 token 总量会得出一个没有意义的数。
	type modelUsage struct {
		Input []struct {
			Value float64 `json:"value"`
		} `json:"input"`
		Output []struct {
			Value float64 `json:"value"`
		} `json:"output"`
		Cached []struct {
			Value float64 `json:"value"`
		} `json:"cached"`
	}
	type category struct {
		Models map[string]modelUsage `json:"models"`
	}

	var input, output, cached float64
	total := func(entries []struct {
		Value float64 `json:"value"`
	}) float64 {
		var sum float64
		for _, e := range entries {
			sum += e.Value
		}
		return sum
	}
	collect := func(raw json.RawMessage) {
		if len(raw) == 0 {
			return
		}
		var cat category
		if err := json.Unmarshal(raw, &cat); err != nil {
			return
		}
		for _, m := range cat.Models {
			input += total(m.Input)
			output += total(m.Output)
			cached += total(m.Cached)
		}
	}

	for _, key := range []string{"completion", "chat"} {
		collect(root[key])
	}
	// vibe_code 多包一层 completion。
	if raw, ok := root["vibe_code"]; ok && len(raw) > 0 {
		var vc struct {
			Completion json.RawMessage `json:"completion"`
		}
		if err := json.Unmarshal(raw, &vc); err == nil {
			collect(vc.Completion)
		}
	}

	if input == 0 && output == 0 && cached == 0 {
		return nil, errors.New("没有可统计的 token 用量")
	}
	return []model.UsageStat{
		{Label: "本月输入 token", Value: input, Unit: model.UnitTokens},
		{Label: "本月输出 token", Value: output, Unit: model.UnitTokens},
		{Label: "本月缓存 token", Value: cached, Unit: model.UnitTokens,
			Hint: "命中前缀缓存的输入部分，单独列出以便估算缓存收益"},
	}, nil
}
