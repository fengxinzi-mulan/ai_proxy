package model

import "time"

// 套餐余量查询的归一化结果。
//
// 不同的上游（commandcode、以后可能的其它平台）返回的 JSON 结构完全不同，
// 但界面要展示的东西是同一套：计划名、剩余额度、滚动窗口用量、本周期统计。
// 所以每个模板负责把自己的响应映射到下面这些结构里，界面只认这一份形状。
//
// 设计约束：模板遇到「字段不存在」时必须让对应段落留空并把原因写进 Warnings，
// 绝不能把读不出来的值渲染成 0 —— 未公开接口随时可能改结构，
// 把「读不到」显示成「额度为 0」会直接误导用户。

// UsageSnapshot 是一次用量查询的完整结果。
//
// 与 ProbeResult 同理，它带 OK/Error 而不是让调用方处理 error：
// 部分接口失败时仍然要能展示已经拿到的部分，并把失败原因说清楚。
type UsageSnapshot struct {
	OK bool `json:"ok"`
	// ProviderID 标明这份快照属于哪个供应商。存库后靠它索引，
	// 前端也可以据此确认拿到的快照没有串号。
	ProviderID   int64     `json:"providerId"`
	Template     string    `json:"template"`
	TemplateName string    `json:"templateName"`
	FetchedAt    time.Time `json:"fetchedAt"`
	LatencyMs    int64     `json:"latencyMs"`

	// 账号与计划
	Account  string `json:"account"`  // 账号名（登录名或组织名）
	PlanID   string `json:"planId"`   // 上游原始计划 id，例如 individual-goat
	PlanName string `json:"planName"` // 计划友好名，例如 GOAT；模板认不出时回落为 PlanID
	Status   string `json:"status"`   // 订阅状态，例如 active
	// 计费周期
	PeriodStart *time.Time `json:"periodStart"`
	PeriodEnd   *time.Time `json:"periodEnd"`

	// 余额（月度额度、充值额度、赠送额度等）
	Balances []UsageBalance `json:"balances"`
	// 滚动窗口限额（5 小时、每周等）
	Windows []UsageWindow `json:"windows"`
	// 本周期聚合统计（花费、请求数、成功率、token 数等）
	Period []UsageStat `json:"period"`

	// 部分接口失败时的降级说明
	Warnings []string `json:"warnings"`
	// 整体失败原因（模板未配置、鉴权被拒、推导地址失败等）
	Error string `json:"error"`

	// 各接口的原始响应，界面上折叠展示，便于上游改结构时排查
	Raw map[string]any `json:"raw,omitempty"`
	// 实际请求过的地址
	UsedURLs []string `json:"usedUrls"`
}

// UsageBalance 是一项额度余额。金额单位统一按上游给的记账单位（这些平台都是美元）。
type UsageBalance struct {
	Label  string  `json:"label"` // 月度额度 / 充值额度 / 赠送额度
	Amount float64 `json:"amount"`
	// Hint 用于补充说明，例如「包含订阅额度，每周期重置」
	Hint string `json:"hint,omitempty"`
}

// UsageWindow 是一个滚动窗口的用量与上限。
type UsageWindow struct {
	Label   string     `json:"label"` // 5 小时 / 本周 / 本月
	Used    float64    `json:"used"`
	Cap     float64    `json:"cap"`
	Percent float64    `json:"percent"` // 0-100，Cap 为 0 时留 0
	ResetAt *time.Time `json:"resetAt"`
	// Exceeded 表示上游明确标记该窗口已超限
	Exceeded bool `json:"exceeded"`
	// Hint 说明这个数字是怎么来的。推导出来的窗口（上游没直接给）必须写清楚，
	// 界面上以悬浮提示展示 —— 否则用户没法判断该不该信这个百分比。
	Hint string `json:"hint,omitempty"`
}

// 统计值的单位，决定前端怎么格式化。
const (
	UnitUSD     = "usd"
	UnitCount   = "count"
	UnitPercent = "percent"
	UnitTokens  = "tokens"
)

// UsageStat 是周期内的一个聚合指标。
type UsageStat struct {
	Label string  `json:"label"`
	Value float64 `json:"value"`
	Unit  string  `json:"unit"`
	// Hint 用于补充说明，例如「按上游记账口径」
	Hint string `json:"hint,omitempty"`
}

// UsageTemplateInfo 是模板的对外元数据，供前端下拉框展示。
// 真正的实现在 internal/proxy 里，那边是模板的唯一来源。
type UsageTemplateInfo struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}
