package model

// 供应商优化预设。
//
// 同一个上游厂商对代理的要求往往是一整套的：严格 schema、要不要注入用量选项、
// 支持哪些缓存/续传能力。与其让用户逐项对照文档勾开关，不如按域名识别出厂商，
// 把「推荐配置」一次性摆到用户面前 —— 但前提是用户确认，且只改预设显式列出的
// 那几个字段，密钥、代理、提示词这些一概不碰。
//
// 预设以「指针字段」表示「只管这一项」：nil 的字段不会被写进供应商配置。
// 这样以后加新预设时，不需要在应用逻辑里写一堆 if。

import (
	"net/url"
	"strings"
)

// PresetChange 是给界面展示的一条变更说明：改什么、改成什么、为什么。
type PresetChange struct {
	Field  string `json:"field"`
	Value  string `json:"value"`
	Reason string `json:"reason"`
}

// ProviderPreset 是一套按上游厂商预置的优化配置。
type ProviderPreset struct {
	ID      string `json:"id"`
	Title   string `json:"title"`
	Summary string `json:"summary"`
	// Hosts 是主机后缀匹配规则：mistral.ai 同时命中 mistral.ai 与 api.mistral.ai。
	Hosts []string `json:"hosts"`
	// Changes 与下面的字段一一对应，供界面逐条展示。
	Changes []PresetChange `json:"changes"`

	StripUnsupportedParams *bool   `json:"stripUnsupportedParams,omitempty"`
	PromptCacheKeyMode     *string `json:"promptCacheKeyMode,omitempty"`
	UsageInjectMode        *string `json:"usageInjectMode,omitempty"`
	ContinuationMode       *string `json:"continuationMode,omitempty"`
	TimeoutSeconds         *int    `json:"timeoutSeconds,omitempty"`
	// MaxImagesPerRequest 是单请求图片数量上限；0 表示不限制（预设里不会出现 0，
	// 想让上限「不限制」就别列这一项）。
	MaxImagesPerRequest *int `json:"maxImagesPerRequest,omitempty"`
	// ImageTrimBatch 是一批丢几张；0/1 表示逐张。
	ImageTrimBatch *int `json:"imageTrimBatch,omitempty"`
}

// providerPresets 是内置预设表。新增厂商时只加条目，不动应用逻辑。
var providerPresets = []ProviderPreset{
	{
		ID:      "mistral",
		Title:   "Mistral 官方 API",
		Summary: "Mistral 的请求体是 additionalProperties=false 的严格 schema，同时支持 prompt_cache_key 缓存亲和，属于「该开的开、该关的关」的典型。",
		Hosts:   []string{"mistral.ai"},
		Changes: []PresetChange{
			{
				Field:  "严格上游兼容",
				Value:  "开启",
				Reason: "messages schema 不允许多余字段：reasoning_content、store、name 这类回放字段会让整条请求 422",
			},
			{
				Field:  "prompt_cache_key",
				Value:  "自动注入",
				Reason: "官方文档：同一会话用稳定 key 提高前缀缓存命中率，缓存输入按标准输入价的 10% 计费（只对 chat/fim/agents/responses 四个端点生效）",
			},
			{
				Field:  "usage 注入",
				Value:  "关闭",
				Reason: "Mistral 的流式响应每个 chunk 自带 usage，不需要 stream_options.include_usage；这里关掉的只是多余参数，统计照样精确",
			},
			{
				Field:  "Responses 续传",
				Value:  "关闭",
				Reason: "Mistral 的 Responses API 是无状态的（store 只接受 false），引用不到上一轮响应",
			},
			{
				Field:  "总超时",
				Value:  "900 秒",
				Reason: "它的模型支持 1M 上下文与 xhigh/max 推理档，长上下文的预填充本身就要好几分钟，300 秒的默认值容易被我们自己截断",
			},
			{
				Field:  "单请求图片上限",
				Value:  "20 张",
				Reason: "上游按请求内的图片总数硬性拒绝，超过 20 张整条 400（错误原文 \"Total number of images exceeds the maximum allowed of 20.\"）；客户端每轮回放全部历史图片，数量不可控，超出的更早图片会被替换成占位文本",
			},
			{
				Field:  "成批替换",
				Value:  "每 5 张一批",
				Reason: "逐张推进时丢弃边界每轮前移一格，「第一张保留下来的图」的位置跟着变，前缀缓存从那里断开、整段保留区全价重算；按 5 张一批推进后，图片数在 21~25 这段窗口里发出的是同一份 body，缓存照常命中，只有每长满 5 张才失效一次",
			},
		},
		StripUnsupportedParams: boolPtr(true),
		PromptCacheKeyMode:     strPtr(PromptCacheKeyAuto),
		UsageInjectMode:        strPtr("off"),
		ContinuationMode:       strPtr(ContinuationOff),
		TimeoutSeconds:         intPtr(900),
		MaxImagesPerRequest:    intPtr(20),
		ImageTrimBatch:         intPtr(5),
	},
}

func boolPtr(v bool) *bool    { return &v }
func strPtr(v string) *string { return &v }
func intPtr(v int) *int       { return &v }

// ProviderPresets 返回全部内置预设的副本，避免调用方改到全局表。
func ProviderPresets() []ProviderPreset {
	out := make([]ProviderPreset, len(providerPresets))
	copy(out, providerPresets)
	return out
}

// LookupProviderPreset 按 id 查预设。
func LookupProviderPreset(id string) (ProviderPreset, bool) {
	for _, p := range providerPresets {
		if p.ID == id {
			return p, true
		}
	}
	return ProviderPreset{}, false
}

// MatchProviderPresets 返回 baseURL 命中的预设。
//
// 只看主机名（忽略端口、路径、大小写），并且按后缀匹配，所以
// https://api.mistral.ai/v1 与 https://mistral.ai 都能命中 mistral.ai。
func MatchProviderPresets(baseURL string) []ProviderPreset {
	var matched []ProviderPreset
	for _, p := range providerPresets {
		if p.AppliesTo(baseURL) {
			matched = append(matched, p)
		}
	}
	return matched
}

// AppliesTo 报告该预设是否匹配这个 baseURL。
func (p ProviderPreset) AppliesTo(baseURL string) bool {
	host := hostOf(baseURL)
	if host == "" {
		return false
	}
	for _, h := range p.Hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if host == h || strings.HasSuffix(host, "."+h) {
			return true
		}
	}
	return false
}

// Apply 把预设写入供应商，返回实际改动的字段名（JSON 字段名）。
//
// 只改预设显式列出的字段；值与目标一致时不写入，也不进改动清单。
func (p ProviderPreset) Apply(prov *Provider) []string {
	var changed []string
	if p.StripUnsupportedParams != nil && prov.StripUnsupportedParams != *p.StripUnsupportedParams {
		prov.StripUnsupportedParams = *p.StripUnsupportedParams
		changed = append(changed, "stripUnsupportedParams")
	}
	if p.PromptCacheKeyMode != nil && prov.PromptCacheKeyMode != *p.PromptCacheKeyMode {
		prov.PromptCacheKeyMode = *p.PromptCacheKeyMode
		changed = append(changed, "promptCacheKeyMode")
	}
	if p.UsageInjectMode != nil && prov.UsageInjectMode != *p.UsageInjectMode {
		prov.UsageInjectMode = *p.UsageInjectMode
		changed = append(changed, "usageInjectMode")
	}
	if p.ContinuationMode != nil && prov.ContinuationMode != *p.ContinuationMode {
		prov.ContinuationMode = *p.ContinuationMode
		changed = append(changed, "continuationMode")
	}
	if p.TimeoutSeconds != nil && prov.TimeoutSeconds != *p.TimeoutSeconds {
		prov.TimeoutSeconds = *p.TimeoutSeconds
		changed = append(changed, "timeoutSeconds")
	}
	if p.MaxImagesPerRequest != nil && prov.MaxImagesPerRequest != *p.MaxImagesPerRequest {
		prov.MaxImagesPerRequest = *p.MaxImagesPerRequest
		changed = append(changed, "maxImagesPerRequest")
	}
	if p.ImageTrimBatch != nil && prov.ImageTrimBatch != *p.ImageTrimBatch {
		prov.ImageTrimBatch = *p.ImageTrimBatch
		changed = append(changed, "imageTrimBatch")
	}
	return changed
}

// Applied 报告供应商当前是否已经符合该预设（界面用它决定要不要提示）。
func (p ProviderPreset) Applied(prov Provider) bool {
	if p.StripUnsupportedParams != nil && prov.StripUnsupportedParams != *p.StripUnsupportedParams {
		return false
	}
	if p.PromptCacheKeyMode != nil && prov.PromptCacheKeyMode != *p.PromptCacheKeyMode {
		return false
	}
	if p.UsageInjectMode != nil && prov.UsageInjectMode != *p.UsageInjectMode {
		return false
	}
	if p.ContinuationMode != nil && prov.ContinuationMode != *p.ContinuationMode {
		return false
	}
	if p.TimeoutSeconds != nil && prov.TimeoutSeconds != *p.TimeoutSeconds {
		return false
	}
	if p.MaxImagesPerRequest != nil && prov.MaxImagesPerRequest != *p.MaxImagesPerRequest {
		return false
	}
	if p.ImageTrimBatch != nil && prov.ImageTrimBatch != *p.ImageTrimBatch {
		return false
	}
	return true
}

// hostOf 从 baseURL 里取小写主机名；解析失败时返回空串。
//
// 用户手填的地址常常缺 scheme（api.mistral.ai/v1），这里补一个 https://
// 再解析，免得「填得对但识别不出来」。
func hostOf(baseURL string) string {
	raw := strings.TrimSpace(baseURL)
	if raw == "" {
		return ""
	}
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}
