// Package model 定义全程序共享的数据结构。
//
// 这些结构同时承担三个角色：SQLite 行的映射目标、管理 API 的 JSON 载体、
// 以及 proxy 转发流程的内部状态。字段标签统一使用 JSON 小驼峰，
// 便于前端直接消费。
package model

import "time"

// APIFormat 标识上游供应商使用的协议格式。
//
// 注意：该字段仅用于「解析用量、定位提示词注入位置」，
// 程序不会在不同格式之间做任何请求/响应转换。
type APIFormat string

const (
	FormatOpenAI    APIFormat = "openai"           // POST /v1/chat/completions
	FormatResponses APIFormat = "openai-responses" // POST /v1/responses
	FormatAnthropic APIFormat = "anthropic"        // POST /v1/messages
	FormatGemini    APIFormat = "gemini"           // POST /v1beta/models/*:generateContent
	FormatCustom    APIFormat = "custom"           // 自定义网关，用量路径由用户指定
)

// AllAPIFormats 供前端下拉框与校验使用。
var AllAPIFormats = []APIFormat{
	FormatOpenAI, FormatResponses, FormatAnthropic, FormatGemini, FormatCustom,
}

// Valid 报告 f 是否为已知格式。
func (f APIFormat) Valid() bool {
	for _, k := range AllAPIFormats {
		if k == f {
			return true
		}
	}
	return false
}

// 三态覆盖模式，用于「全局默认 + 供应商覆盖」的配置项。
const (
	ModeInherit = "inherit" // 继承全局
	ModeCustom  = "custom"  // 使用本供应商自己的配置
	ModeDisable = "disable" // 明确关闭
)

// 提示词注入策略。
const (
	StrategyAppend      = "append"       // 追加到已有 system 之后
	StrategyReplace     = "replace"      // 替换已有 system
	StrategyPrependUser = "prepend_user" // 作为首条 user 消息插入
)

// 代理类型。
const (
	ProxyHTTP   = "http"
	ProxySOCKS5 = "socks5"
)

// APIKey 是供应商的一个密钥。一个供应商可配置多个，运行时轮询使用。
type APIKey struct {
	ID      string `json:"id"`
	Key     string `json:"key"`
	Remark  string `json:"remark"`
	Enabled bool   `json:"enabled"`
}

// ProxyConfig 描述一个代理端点。
type ProxyConfig struct {
	Enabled  bool     `json:"enabled"`
	Type     string   `json:"type"` // http | socks5
	URL      string   `json:"url"`  // host:port 或 scheme://host:port
	Username string   `json:"username"`
	Password string   `json:"password"`
	NoProxy  []string `json:"noProxy"` // 命中则直连的域名后缀列表
}

// Normalized 返回补全 scheme 后的代理地址。
func (p ProxyConfig) Normalized() string {
	if p.URL == "" {
		return ""
	}
	// 允许用户只填 host:port
	for _, scheme := range []string{"http://", "https://", "socks5://"} {
		if len(p.URL) >= len(scheme) && p.URL[:len(scheme)] == scheme {
			return p.URL
		}
	}
	if p.Type == ProxySOCKS5 {
		return "socks5://" + p.URL
	}
	return "http://" + p.URL
}

// PromptRule 是一条按模型名正则匹配的提示词规则，优先级高于供应商/全局配置。
type PromptRule struct {
	ID           string `json:"id"`
	Enabled      bool   `json:"enabled"`
	ModelPattern string `json:"modelPattern"` // 正则，空则匹配全部
	Text         string `json:"text"`
	Strategy     string `json:"strategy"`
}

// PromptConfig 是一份可直接使用的提示词配置（全局或供应商级别）。
type PromptConfig struct {
	Enabled  bool   `json:"enabled"`
	Text     string `json:"text"`
	Strategy string `json:"strategy"`
}

// CustomUsageMapping 用于 apiFormat=custom 时告诉程序去哪儿取用量。
// 值是简化版 JSON 路径，支持 a.b.c 形式；* 表示数组通配。
type CustomUsageMapping struct {
	PromptTokens     string `json:"promptTokens"`
	CachedTokens     string `json:"cachedTokens"`
	CacheWriteTokens string `json:"cacheWriteTokens"`
	CompletionTokens string `json:"completionTokens"`
	ReasoningTokens  string `json:"reasoningTokens"`
	TotalTokens      string `json:"totalTokens"`
	Model            string `json:"model"`
}

// Provider 是被代理的上游供应商配置。
type Provider struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"` // 唯一短名，用于 /p/<name>/ 路由
	DisplayName string `json:"displayName"`
	Remark      string `json:"remark"`
	Enabled     bool   `json:"enabled"`
	Active      bool   `json:"active"` // 是否为「当前生效」供应商
	SortOrder   int    `json:"sortOrder"`

	// 上游地址与协议
	BaseURL    string    `json:"baseUrl"`
	APIFormat  APIFormat `json:"apiFormat"`
	CustomPath string    `json:"customPath"` // 非空时完全接管上游路径

	// 认证
	Keys         []APIKey          `json:"keys"`
	AuthHeader   string            `json:"authHeader"` // 默认 Authorization
	AuthPrefix   string            `json:"authPrefix"` // 默认 "Bearer "
	ExtraHeaders map[string]string `json:"extraHeaders"`

	// 网络
	TimeoutSeconds        int         `json:"timeoutSeconds"`
	ConnectTimeoutSeconds int         `json:"connectTimeoutSeconds"`
	InsecureSkipTLS       bool        `json:"insecureSkipTls"`
	ProxyMode             string      `json:"proxyMode"` // inherit | custom | direct
	Proxy                 ProxyConfig `json:"proxy"`

	// 提示词
	PromptMode  string       `json:"promptMode"` // inherit | custom | disable
	Prompt      PromptConfig `json:"prompt"`
	PromptRules []PromptRule `json:"promptRules"`

	// usage 注入
	UsageInjectMode string `json:"usageInjectMode"` // inherit | on | off
	StripUsageChunk bool   `json:"stripUsageChunk"`

	// 自定义用量映射（apiFormat=custom 时使用）
	CustomUsage *CustomUsageMapping `json:"customUsage"`

	Tags      []string  `json:"tags"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// DefaultAuthForFormat 返回某种 API 格式惯用的认证头与取值前缀。
//
//   - OpenAI 系用 Authorization: Bearer <key>
//   - Anthropic 用 x-api-key: <key>
//   - Gemini 用 x-goog-api-key: <key>
//
// 供应商没有显式配置时按此回落；前端在切换格式时也会用同一套默认值预填，
// 所以两边永远一致。
func DefaultAuthForFormat(f APIFormat) (header, prefix string) {
	switch f {
	case FormatAnthropic:
		return "x-api-key", ""
	case FormatGemini:
		return "x-goog-api-key", ""
	default:
		return "Authorization", "Bearer "
	}
}

// AuthHeaderFor 返回该供应商实际使用的认证头名与前缀。
func (p Provider) AuthHeaderFor() (string, string) {
	if p.AuthHeader != "" {
		return p.AuthHeader, p.AuthPrefix
	}
	return DefaultAuthForFormat(p.APIFormat)
}

// ApplyDefaults 补齐空字段，保证从数据库或 API 读入的对象始终可用。
func (p *Provider) ApplyDefaults() {
	if p.APIFormat == "" {
		p.APIFormat = FormatOpenAI
	}
	// AuthHeader / AuthPrefix 故意不在这里补默认值：
	// 默认值取决于 API 格式，靠 AuthHeaderFor 在读取时决定，
	// 这样用户把格式从 openai 改成 anthropic 时不会残留错误的认证头。
	if p.ExtraHeaders == nil {
		p.ExtraHeaders = map[string]string{}
	}
	if p.Tags == nil {
		p.Tags = []string{}
	}
	if p.Proxy.Type == "" {
		p.Proxy.Type = ProxyHTTP
	}
	if p.Proxy.NoProxy == nil {
		p.Proxy.NoProxy = []string{}
	}
	if p.Keys == nil {
		p.Keys = []APIKey{}
	}
	if p.PromptRules == nil {
		p.PromptRules = []PromptRule{}
	}
	// TimeoutSeconds / ConnectTimeoutSeconds 为 0 表示「继承全局默认」，
	// 只有负数才视为非法值。
	if p.TimeoutSeconds < 0 {
		p.TimeoutSeconds = 0
	}
	if p.ConnectTimeoutSeconds < 0 {
		p.ConnectTimeoutSeconds = 0
	}
	switch p.ProxyMode {
	case ModeInherit, ModeCustom, "direct":
	default:
		p.ProxyMode = ModeInherit
	}
	switch p.PromptMode {
	case ModeInherit, ModeCustom, ModeDisable:
	default:
		p.PromptMode = ModeInherit
	}
	switch p.UsageInjectMode {
	case ModeInherit, "on", "off":
	default:
		p.UsageInjectMode = ModeInherit
	}
	if p.Prompt.Strategy == "" {
		p.Prompt.Strategy = StrategyAppend
	}
}

// Settings 是全局设置，整体以单条 JSON 记录存放。
type Settings struct {
	// 服务
	Host      string `json:"host"`
	Port      int    `json:"port"`
	AccessKey string `json:"accessKey"` // 非空时管理接口与代理入口都要求携带

	// 全局代理（VPN）
	Proxy ProxyConfig `json:"proxy"`

	// 全局提示词
	GlobalPrompt PromptConfig `json:"globalPrompt"`

	// usage 注入全局默认
	UsageInjectDefault bool `json:"usageInjectDefault"`

	// 上游默认超时（秒）。供应商未单独设置时使用；0 表示不限制总时长。
	// AI 请求的生成耗时跨度极大，所以默认给足 10 分钟而不是常见的几十秒。
	DefaultTimeoutSeconds        int `json:"defaultTimeoutSeconds"`
	DefaultConnectTimeoutSeconds int `json:"defaultConnectTimeoutSeconds"`

	// 日志
	StoreBodies   bool `json:"storeBodies"`   // 是否落库请求/响应报文
	MaxBodyBytes  int  `json:"maxBodyBytes"`  // 单条报文落库截断长度
	RetentionDays int  `json:"retentionDays"` // 保留天数，0 表示不按时间清理
	MaxLogs       int  `json:"maxLogs"`       // 最大条数，0 表示不按条数清理
}

// DefaultSettings 返回首次启动时使用的默认设置。
func DefaultSettings() Settings {
	return Settings{
		Host: "127.0.0.1",
		Port: 8080,
		Proxy: ProxyConfig{
			Type:    ProxyHTTP,
			NoProxy: []string{},
		},
		GlobalPrompt: PromptConfig{
			Strategy: StrategyAppend,
		},
		UsageInjectDefault:           true,
		DefaultTimeoutSeconds:        600,
		DefaultConnectTimeoutSeconds: 15,
		StoreBodies:                  true,
		MaxBodyBytes:                 64 * 1024,
		RetentionDays:                30,
		MaxLogs:                      200000,
	}
}

// ApplyDefaults 补齐零值字段。
func (s *Settings) ApplyDefaults() {
	d := DefaultSettings()
	if s.Host == "" {
		s.Host = d.Host
	}
	if s.Port <= 0 || s.Port > 65535 {
		s.Port = d.Port
	}
	if s.Proxy.Type == "" {
		s.Proxy.Type = d.Proxy.Type
	}
	if s.Proxy.NoProxy == nil {
		s.Proxy.NoProxy = []string{}
	}
	if s.GlobalPrompt.Strategy == "" {
		s.GlobalPrompt.Strategy = d.GlobalPrompt.Strategy
	}
	// DefaultTimeoutSeconds 允许为 0（不限制），只在负数时回落到默认值。
	if s.DefaultTimeoutSeconds < 0 {
		s.DefaultTimeoutSeconds = d.DefaultTimeoutSeconds
	}
	if s.DefaultConnectTimeoutSeconds <= 0 {
		s.DefaultConnectTimeoutSeconds = d.DefaultConnectTimeoutSeconds
	}
	if s.MaxBodyBytes <= 0 {
		s.MaxBodyBytes = d.MaxBodyBytes
	}
}

// RequestLog 是一次转发请求的完整记录。
type RequestLog struct {
	ID           int64     `json:"id"`
	TSStart      time.Time `json:"tsStart"`
	TSEnd        time.Time `json:"tsEnd"`
	ProviderID   int64     `json:"providerId"`
	ProviderName string    `json:"providerName"`
	UpstreamURL  string    `json:"upstreamUrl"`
	ProxyUsed    string    `json:"proxyUsed"`

	Method string `json:"method"`
	Path   string `json:"path"`
	// Model 是客户端请求的模型名，ModelResponse 是上游响应里回显的模型名。
	Model         string `json:"model"`
	ModelResponse string `json:"modelResponse"`
	APIFormat     string `json:"apiFormat"`
	Stream        bool   `json:"stream"`
	// ReasoningEffort 是请求中的推理强度设置原文（如 "high"、"4096"）。
	ReasoningEffort string `json:"reasoningEffort"`
	ClientIP        string `json:"clientIp"`

	// 改写留痕
	RequestModified  bool     `json:"requestModified"`
	ResponseModified bool     `json:"responseModified"`
	Modifications    []string `json:"modifications"`

	HTTPStatus int    `json:"httpStatus"`
	Success    bool   `json:"success"`
	ErrorMsg   string `json:"errorMsg"`
	Cancelled  bool   `json:"cancelled"`

	// 用量
	PromptTokens     int  `json:"promptTokens"`
	CachedTokens     int  `json:"cachedTokens"`
	CacheWriteTokens int  `json:"cacheWriteTokens"`
	CompletionTokens int  `json:"completionTokens"`
	ReasoningTokens  int  `json:"reasoningTokens"`
	TotalTokens      int  `json:"totalTokens"`
	TokensEstimated  bool `json:"tokensEstimated"`

	// 性能
	TTFTMs  int64   `json:"ttftMs"`
	TotalMs int64   `json:"totalMs"`
	TPS     float64 `json:"tps"`

	// 报文（按设置截断）
	ReqHeaders      string `json:"reqHeaders,omitempty"`
	ReqBody         string `json:"reqBody,omitempty"`
	ReqBodyOriginal string `json:"reqBodyOriginal,omitempty"`
	RespBody        string `json:"respBody,omitempty"`
}

// OverviewStats 是仪表盘顶部指标卡的聚合结果。
type OverviewStats struct {
	TotalRequests    int64   `json:"totalRequests"`
	SuccessRequests  int64   `json:"successRequests"`
	FailedRequests   int64   `json:"failedRequests"`
	SuccessRate      float64 `json:"successRate"`
	PromptTokens     int64   `json:"promptTokens"`
	CachedTokens     int64   `json:"cachedTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	TotalTokens      int64   `json:"totalTokens"`
	// CacheHitRate 是缓存命中率（百分比）：命中缓存的输入量 ÷ 输入总量。
	// 输入总量本身已含缓存部分，所以比值天然落在 0–100 之间。
	CacheHitRate  float64 `json:"cacheHitRate"`
	AvgTTFTMs     float64 `json:"avgTtftMs"`
	AvgTPS        float64 `json:"avgTps"`
	AvgDurationMs float64 `json:"avgDurationMs"`
}

// TimeSeriesPoint 是趋势图上的一个数据点。
type TimeSeriesPoint struct {
	Bucket           string  `json:"bucket"`
	Requests         int64   `json:"requests"`
	Failed           int64   `json:"failed"`
	PromptTokens     int64   `json:"promptTokens"`
	CachedTokens     int64   `json:"cachedTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	CacheHitRate     float64 `json:"cacheHitRate"`
	AvgTTFTMs        float64 `json:"avgTtftMs"`
	AvgTPS           float64 `json:"avgTps"`
}

// ModelStatsRow 是「按模型」维度的聚合行。
//
// 不带供应商名：同一模型名可能由多个供应商提供，聚合后进行归属会失真，
// 供应商维度由 ProviderStatsRow 单独给出。
type ModelStatsRow struct {
	Model            string  `json:"model"`
	Requests         int64   `json:"requests"`
	Failed           int64   `json:"failed"`
	PromptTokens     int64   `json:"promptTokens"`
	CachedTokens     int64   `json:"cachedTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	CacheHitRate     float64 `json:"cacheHitRate"`
	AvgTTFTMs        float64 `json:"avgTtftMs"`
	AvgTPS           float64 `json:"avgTps"`
}

// ProviderStatsRow 是「按供应商」维度的聚合行。
type ProviderStatsRow struct {
	ProviderID       int64   `json:"providerId"`
	ProviderName     string  `json:"providerName"`
	Requests         int64   `json:"requests"`
	Failed           int64   `json:"failed"`
	PromptTokens     int64   `json:"promptTokens"`
	CachedTokens     int64   `json:"cachedTokens"`
	CompletionTokens int64   `json:"completionTokens"`
	CacheHitRate     float64 `json:"cacheHitRate"`
	AvgTTFTMs        float64 `json:"avgTtftMs"`
	AvgTPS           float64 `json:"avgTps"`
}

// CacheHitRate 计算缓存命中率（百分比）。输入量为 0 时返回 0，避免除零。
func CacheHitRate(cachedTokens, promptTokens int64) float64 {
	if promptTokens <= 0 {
		return 0
	}
	return float64(cachedTokens) / float64(promptTokens) * 100
}

// LiveRequest 是正在转发中的请求快照，用于实时监控页。
type LiveRequest struct {
	ID               string    `json:"id"`
	ProviderName     string    `json:"providerName"`
	Model            string    `json:"model"`
	Path             string    `json:"path"`
	Stream           bool      `json:"stream"`
	ClientIP         string    `json:"clientIp"`
	StartedAt        time.Time `json:"startedAt"`
	ElapsedMs        int64     `json:"elapsedMs"`
	TTFTMs           int64     `json:"ttftMs"`
	CompletionTokens int       `json:"completionTokens"`
	TPS              float64   `json:"tps"`
}

// LogQuery 是日志列表的查询条件。
type LogQuery struct {
	Page       int
	PageSize   int
	ProviderID int64
	Model      string
	Status     string // success | failed | cancelled
	Keyword    string
	From       time.Time
	To         time.Time
}
