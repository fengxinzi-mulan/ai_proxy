// Package model 定义全程序共享的数据结构。
//
// 这些结构同时承担三个角色：SQLite 行的映射目标、管理 API 的 JSON 载体、
// 以及 proxy 转发流程的内部状态。字段标签统一使用 JSON 小驼峰，
// 便于前端直接消费。
package model

import (
	"strings"
	"time"
)

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

// Responses 会话续传的三种模式。
//
//   - off：完全不处理，保持原生透传。
//   - dry-run：只统计「本地历史是否构成上一轮的前缀」以及可节省的字节，
//     不改请求、不设置 store，用于在真实流量上验证客户端回放是否稳定。
//   - on：命中后把全量 input 截成增量，补 store:true + previous_response_id。
//
// 是否真的能用取决于上游模型：只有原生 Responses 且实现 previous_response_id
// 的模型才有效。flash 一类模型会拒绝 store 并自动回退；deepseek-v4-pro 这类
// 模型可能返回 200 但静默忽略 previous_response_id，需要用户自行确认。
const (
	ContinuationOff    = "off"
	ContinuationDryRun = "dry-run"
	ContinuationOn     = "on"
)

// prompt_cache_key 的注入模式。
//
//   - off：完全不注入，保持原样透传。
//   - auto：从会话头（X-Session-Id 等）或「稳定前缀指纹」派生一个不透明 key
//     注入到请求体顶层。Mistral 官方文档说明：同一会话使用同一个 prompt_cache_key
//     可以提高前缀缓存命中率（缓存输入按 10% 计费），但**不保证命中**。
//
// 默认 off：部分严格网关的请求体是 additionalProperties=false 且不认识这个字段，
// 注入会直接 422，所以必须由用户按供应商显式开启。
const (
	PromptCacheKeyOff  = "off"
	PromptCacheKeyAuto = "auto"
)

// 续传冷却默认值。0 表示不冷却；provider 里的负值表示「未配置」，
// ApplyDefaults 会补成这里的默认值。
const (
	DefaultContinuationCooldownSeconds        = 3600
	DefaultContinuationInvalidCooldownSeconds = 300
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

// ModelMapping 是一条模型名改写规则：客户端请求的模型名与 From 完全相等时，
// 实际发给上游的模型名改成 To。
//
// 只做精确匹配，不做前缀/正则匹配 —— 模型名是上游用来选模型和计费的关键字段，
// 模糊匹配一旦误命中，请求就会被送到一个完全不相干的模型上，而且很难从日志里看出来。
type ModelMapping struct {
	From string `json:"from"` // 客户端请求的模型名
	To   string `json:"to"`   // 实际发给上游的模型名
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

// UsageQueryConfig 描述如何向供应商查询套餐余量。
//
// 只有「模板 id」是必填的：具体请求哪些接口、怎么解析响应，都由模板决定。
// 模板 id 为空表示该供应商不启用用量查询。
type UsageQueryConfig struct {
	Template string `json:"template"`
	// BaseURL 留空时由模板从供应商的 BaseURL 推导（例如 commandcode 会去掉 /provider/v1），
	// 只在推导结果不对时才需要手填。
	BaseURL string `json:"baseUrl"`
	// Account / Password 是模板可用的账号凭据，目前只有 Mistral 模板在用。
	//
	// Mistral 的订阅额度只挂在网页后台（admin.mistral.ai），普通 API key 访问
	// Admin 端点会被直接拒绝（实测 401 Invalid API Key），所以只能拿账号密码去
	// 换网页会话。程序用 Ory 的登录 API 自动登录，把 session_token 缓存起来复用，
	// 过期或收到 401 时自动重登 —— 用户不需要手工粘贴会过期的 Cookie。
	//
	// 密码与 API 密钥同等对待：明文存库，不额外加密。
	//
	// 这两个字段虽然挂在 UsageQueryConfig 上，但**不进 usage_query_json**：
	// 它们单独存 providers 表的 usage_account / usage_password 列
	// （见 store.marshalProviderParts 里的剥离逻辑）。否则同一份密码会在库里
	// 存两遍，改一处忘另一处就会读到旧值。
	Account  string `json:"account"`
	Password string `json:"password"`
	// AutoRefreshSeconds 是用量快照的刷新间隔，0 表示不自动刷新。
	// 刷新由服务端按这个间隔做（见 proxy.UsageRefresher），结果落库后广播给管理界面，
	// 前端不自己定时查上游 —— 页面关着的时候额度也一直在更新。
	AutoRefreshSeconds int `json:"autoRefreshSeconds"`
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

	// 模型映射（可选）。只做精确匹配，未命中的请求原样透传。
	ModelMap []ModelMapping `json:"modelMap"`

	// Responses 会话续传。默认 off，只有用户显式开启才生效。
	// ContinuationModels 是可选的模型范围（留空表示全部模型），
	// 由用户决定在哪些模型上开启，不是程序内置的白名单。
	ContinuationMode   string   `json:"continuationMode"`
	ContinuationModels []string `json:"continuationModels"`
	// 冷却时间（秒）。不支持类错误与 response id 失效分开配置；0 表示不冷却。
	ContinuationCooldownSeconds        int `json:"continuationCooldownSeconds"`
	ContinuationInvalidCooldownSeconds int `json:"continuationInvalidCooldownSeconds"`

	// usage 注入
	UsageInjectMode string `json:"usageInjectMode"` // inherit | on | off
	StripUsageChunk bool   `json:"stripUsageChunk"`

	// StripUnsupportedParams 开启后，OpenAI / Responses 格式的请求体会剔除非标准
	// 字段：顶层的 thinking / enable_thinking / thinking_budget（Chat 还含 store），
	// 以及 assistant 消息里回放的思维链 reasoning_content / reasoning_details /
	// reasoning。用来对接只认 OpenAI 规范、其余字段一律 422 的严格上游
	// （例如 Mistral 的 extra="forbid" schema）。
	// 默认关闭：不少兼容网关确实认 thinking，不能替用户做主。
	StripUnsupportedParams bool `json:"stripUnsupportedParams"`

	// ReplayReasoningAsThinkChunk 开启后，在 OpenAI Chat 请求改写阶段会把
	// assistant 消息里回放的 reasoning_content 对称还原成 Mistral 原生 ThinkChunk
	// （{"type":"thinking","thinking":[{"type":"text","text":"..."}],"closed":true}）
	// 合并进 content 数组，而不是直接删掉 —— 避免多步 tool_calls 之间思维链断裂，
	// 同时保持历史前缀稳定以命中前缀缓存。
	ReplayReasoningAsThinkChunk bool `json:"replayReasoningAsThinkChunk"`

	// PromptCacheKeyMode 控制是否给请求注入 prompt_cache_key（off | auto）。
	// 只对 OpenAI 系格式生效；客户端自己带了 key 时不覆盖。
	PromptCacheKeyMode string `json:"promptCacheKeyMode"`

	// MaxImagesPerRequest 是单次请求允许携带的图片张数上限，0 表示不限制。
	//
	// 上游按「整个请求里的图片总数」硬性拒绝，Mistral 超过就直接整条 400：
	//
	//	{"message":"Total number of images exceeds the maximum allowed of 20."}
	//
	// 而客户端（DSH、Codex）每一轮都把历史消息连同其中的图片原样回放，图片
	// 数量随会话自然增长、完全不受代理控制 —— 攒够上限之后这个会话就再也发不出去，
	// 只能靠手工清历史自救。所以由代理按上游能力兜底：超出部分里更早的图片
	// 替换成一行占位文本（见 proxy.trimExcessImages）。
	//
	// 放在供应商级而不是全局：这是上游能力，不是流量体积优化 —— 不同厂商、
	// 同一厂商的不同模型，上限都不一样。
	MaxImagesPerRequest int `json:"maxImagesPerRequest"`

	// ImageTrimBatch 是「一批丢几张」：超出上限时按这个步长成批替换，而不是
	// 每多一张图就往前挪一格。0 或 1 表示逐张（最简单但缓存代价最高）。
	//
	// 为什么需要它：客户端每轮重发全量历史，如果丢弃边界随图片数逐张前移，
	// 那么「第一张保留下来的图」的位置每轮都在变 —— 前缀缓存从那里断开，
	// 该轮就得把后面十几张图全价重算一遍。成批替换把边界钉在一个窗口里：
	// 图片数从 max+1 长到 max+batch 的过程中，丢的是同一批、位置也相同，
	// 前缀逐字节一致，缓存照常命中。每长满一批才失效一次。
	ImageTrimBatch int `json:"imageTrimBatch"`

	// 自定义用量映射（apiFormat=custom 时使用）
	CustomUsage *CustomUsageMapping `json:"customUsage"`

	// 套餐余量查询（可选，需要一个能读账号额度的接口）
	UsageQuery UsageQueryConfig `json:"usageQuery"`

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

// MapModel 按模型映射把请求模型名改写成目标模型名，返回改写结果与是否命中。
//
// 逐条按顺序比较，命中第一条即返回；两侧都去掉首尾空白后比较，
// 这样用户在表单里粘贴带空格的名字不会造成「看起来一样却不匹配」。
// 未命中时返回原值与 false，调用方据此保持请求原样透传。
func (p Provider) MapModel(name string) (string, bool) {
	if len(p.ModelMap) == 0 {
		return name, false
	}
	trimmed := strings.TrimSpace(name)
	if trimmed == "" {
		return name, false
	}
	for _, m := range p.ModelMap {
		to := strings.TrimSpace(m.To)
		if to == "" {
			continue
		}
		if strings.TrimSpace(m.From) == trimmed {
			return to, true
		}
	}
	return name, false
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
	if p.ModelMap == nil {
		p.ModelMap = []ModelMapping{}
	}
	if p.ContinuationMode == "" {
		p.ContinuationMode = ContinuationOff
	}
	switch p.ContinuationMode {
	case ContinuationOff, ContinuationDryRun, ContinuationOn:
	default:
		p.ContinuationMode = ContinuationOff
	}
	if p.ContinuationModels == nil {
		p.ContinuationModels = []string{}
	}
	if p.ContinuationCooldownSeconds < 0 {
		p.ContinuationCooldownSeconds = DefaultContinuationCooldownSeconds
	}
	if p.ContinuationInvalidCooldownSeconds < 0 {
		p.ContinuationInvalidCooldownSeconds = DefaultContinuationInvalidCooldownSeconds
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
	switch p.PromptCacheKeyMode {
	case PromptCacheKeyOff, PromptCacheKeyAuto:
	default:
		p.PromptCacheKeyMode = PromptCacheKeyOff
	}
	// 图片上限：0 是「不限制」这个有效取值，只有负数才视为非法。
	if p.MaxImagesPerRequest < 0 {
		p.MaxImagesPerRequest = 0
	}
	// 批大小：负数归一成 0（= 逐张），上限 1000 防手滑填成天文数字把整段历史丢光。
	if p.ImageTrimBatch < 0 {
		p.ImageTrimBatch = 0
	}
	if p.ImageTrimBatch > 1000 {
		p.ImageTrimBatch = 1000
	}
	if p.Prompt.Strategy == "" {
		p.Prompt.Strategy = StrategyAppend
	}
	if p.UsageQuery.AutoRefreshSeconds < 0 {
		p.UsageQuery.AutoRefreshSeconds = 0
	}
}

// ImageCompressSettings 控制「请求体内联图片压缩」。
//
// 为什么放在全局设置而不是供应商级：这是纯粹的流量体积优化，与上游是谁无关；
// 放到供应商级会让用户在几十个供应商里各配一遍，而收益完全一样。
//
// 触发条件是「或」的关系：字节数超阈值，或者长边超上限。只按字节判断会漏掉
// 「体积不大但分辨率极高」的截图（PNG 压缩率高，字节很小但边长 4000px），
// 这类图对上游的视觉 token 消耗最大，恰恰是最该缩的。
type ImageCompressSettings struct {
	Enabled bool `json:"enabled"`
	// ThresholdBytes 是触发压缩的字节阈值（原始图片字节，不是 base64 后长度）。
	ThresholdBytes int `json:"thresholdBytes"`
	// MaxEdge 是长边上限（像素）。0 表示只按字节阈值判断，不做尺寸缩放。
	MaxEdge int `json:"maxEdge"`
	// Quality 是 JPEG 输出质量（1-100）。只在输出为 JPEG 时生效。
	Quality int `json:"quality"`
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

	// 请求体改写：内联图片压缩。默认开启，见 ImageCompressSettings。
	ImageCompress ImageCompressSettings `json:"imageCompress"`

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
		ImageCompress: ImageCompressSettings{
			Enabled:        true,
			ThresholdBytes: 512 * 1024,
			MaxEdge:        2048,
			Quality:        85,
		},
		StoreBodies:   true,
		MaxBodyBytes:  64 * 1024,
		RetentionDays: 30,
		MaxLogs:       200000,
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
	// 图片压缩只修非法值，**不动 Enabled**：Enabled=false 是用户的显式选择，
	// 用默认值顶回来等于「关掉之后下次启动又自己开了」。
	if s.ImageCompress.ThresholdBytes <= 0 {
		s.ImageCompress.ThresholdBytes = d.ImageCompress.ThresholdBytes
	}
	if s.ImageCompress.MaxEdge < 0 {
		s.ImageCompress.MaxEdge = d.ImageCompress.MaxEdge
	}
	if s.ImageCompress.Quality <= 0 || s.ImageCompress.Quality > 100 {
		s.ImageCompress.Quality = d.ImageCompress.Quality
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
	// ModelMapped 是「模型映射」命中后实际发给上游的模型名；未命中为空。
	// 有它才能把「客户端要的模型」和「上游看到的模型」对上号。
	ModelMapped string `json:"modelMapped"`
	APIFormat   string `json:"apiFormat"`
	Stream      bool   `json:"stream"`
	// ReasoningEffort 是请求中的推理强度设置原文（如 "high"、"4096"）。
	ReasoningEffort string `json:"reasoningEffort"`
	ClientIP        string `json:"clientIp"`

	// 改写留痕
	RequestModified  bool     `json:"requestModified"`
	ResponseModified bool     `json:"responseModified"`
	Modifications    []string `json:"modifications"`

	// 续传留痕。off/未命中时为空或 0；dry-run 下 savedBytes 是估算值。
	ContinuationMode   string `json:"continuationMode"`
	ContinuationReason string `json:"continuationReason"`
	SavedBytes         int64  `json:"savedBytes"`

	// 图片压缩留痕：本次请求改写了多少张内联图片、省下多少字节。
	// 单独成列而不是塞进 modifications —— 这两个是可累加的量，
	// 用户想知道的是「开了这个功能到底省了多少流量」。
	ImageCompressed int   `json:"imageCompressed"`
	ImageSavedBytes int64 `json:"imageSavedBytes"`

	// ImagesDropped 是本次请求里被占位文本替换掉的图片张数（供应商配了
	// MaxImagesPerRequest 且请求超限时才会非 0）。与 ImageCompressed 分开记：
	// 一个是「为了过上游而丢掉的内容」，一个是「为了省流量而缩小的字节」，
	// 排查 400 时只关心前者。
	ImagesDropped int `json:"imagesDropped"`

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
