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
	get     func(ctx context.Context, path string, query url.Values) (int, []byte, error)
}

// usageTemplate 描述一种「向上游查询套餐余量」的协议实现。
//
// 新增一个上游平台 = 在 usageTemplateRegistry 里加一条，并写一个 fetch 函数：
// 调接口、把响应映射进 model.UsageSnapshot。前端下拉框会自动出现这个模板。
type usageTemplate struct {
	info  model.UsageTemplateInfo
	fetch func(ctx context.Context, cli *usageClient) (*model.UsageSnapshot, error)
}

var usageTemplateRegistry = []usageTemplate{commandCodeTemplate()}

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

	if _, ok := firstProviderKey(prov); !ok {
		// 客户端持钥的用法下代理手上没有密钥，而额度接口只认账号密钥。
		snap.Error = "该供应商未配置密钥，用量查询需要账号密钥，无法透传客户端密钥"
		return finish()
	}

	base, err := deriveUsageBaseURL(prov)
	if err != nil {
		snap.Error = err.Error()
		return finish()
	}

	netcfg := resolveNet(prov, settings)
	client, err := s.clients.client(netcfg)
	if err != nil {
		snap.Error = "初始化连接失败: " + err.Error()
		return finish()
	}

	// 模板会并发发请求，所以原始响应与地址清单要加锁。
	var (
		rawMu    sync.Mutex
		rawStore = map[string]any{}
	)
	cli := &usageClient{
		baseURL: base,
		get: func(ctx context.Context, path string, query url.Values) (int, []byte, error) {
			target := strings.TrimRight(base, "/") + path
			if len(query) > 0 {
				target += "?" + query.Encode()
			}
			// 每次请求单独构造头：同一个 http.Header 并发复用会被上游写坏
			// （Transport 会往里塞 Host 之类的字段）。
			req, err := buildRequest(ctx, http.MethodGet, target, nil, probeHeaders(prov))
			if err != nil {
				return 0, nil, err
			}
			resp, err := client.Do(req)
			if err != nil {
				return 0, nil, err
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

			var decoded any
			if json.Unmarshal(body, &decoded) != nil {
				decoded = string(body)
			}
			rawMu.Lock()
			snap.UsedURLs = append(snap.UsedURLs, target)
			rawStore[path] = decoded
			rawMu.Unlock()

			return resp.StatusCode, body, nil
		},
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
