package proxy

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"ai_proxy/internal/model"
)

// rewriteRequestBody 按供应商与全局配置改写请求体，返回改写后的字节与改动清单。
//
// 这是整个程序里唯一会改动请求内容的地方，每一项改动都会记进日志
// （req_body_original 保存改写前的原文），保证「透明转发」可核查。
// 提示词与用量注入各自有独立开关；responses 的 input 合规性修正（见
// normalizeResponsesInput）只在客户端确实写漏字段时才动手，因此不设开关。
//
// 任何一步出错（JSON 解析失败、结构不符合该格式）都直接放弃改写并原样转发 —
// 宁可少注入一次提示词，也不能把用户的请求改坏。
//
// modelName 由调用方解析后传入，用于匹配「按模型正则」的提示词规则：Gemini 的模型名
// 不在请求体里，只有调用方（能看到 URL 路径）解析得出来。
//
// 模型映射直接由 modelName 现算：命中时请求体里的 model 会被替换成目标模型名 ——
// 这是唯一会改变「这次请求打的是哪个上游模型」的改动项。
// （Gemini 的模型名在 URL 路径上而不是请求体里，路径那一段的替换由调用方负责。）
func rewriteRequestBody(prov model.Provider, st model.Settings, rawBody []byte, modelName string) ([]byte, []string) {
	r := rewriteRequest(prov, st, rawBody, modelName, rewriteOptions{})
	return r.Body, r.Modifications
}

// rewriteResult 是改写结果。
//
// 比「字节 + 改动清单」多带两个量：图片压缩的张数与省下的字节。
// 单独成结构体而不是往返回值里再加两个参数，是因为绝大多数调用方
// （尤其是测试）只关心 body 与 mods，加参数会让几十处调用全部变噪音。
type rewriteResult struct {
	Body          []byte
	Modifications []string
	// ImageCompressed 是本次被压缩的内联图片张数。
	ImageCompressed int
	// ImageSavedBytes 是 base64 串长度的净减少量。
	ImageSavedBytes int64
}

// rewriteOptions 承载「不在请求体里、但会决定改写结果」的上下文。
//
// 单独抽出来是因为改写函数是纯函数（不碰 *http.Request），
// 而 prompt_cache_key 要从请求头推导会话身份，只能由调用方算好传进来。
type rewriteOptions struct {
	// cacheKeySeed 是会话级标识（如客户端 X-Session-Id）；空串时改写函数
	// 会退化为按「首条消息 + tools」的指纹派生，保证多轮之间仍然稳定。
	cacheKeySeed string
}

// rewriteRequestBodyOpts 是带上下文版本的改写入口。
func rewriteRequestBodyOpts(prov model.Provider, st model.Settings, rawBody []byte, modelName string, opts rewriteOptions) ([]byte, []string) {
	r := rewriteRequest(prov, st, rawBody, modelName, opts)
	return r.Body, r.Modifications
}

// rewriteRequest 是真正的改写实现，返回带图片压缩统计的完整结果。
func rewriteRequest(prov model.Provider, st model.Settings, rawBody []byte, modelName string, opts rewriteOptions) rewriteResult {
	if len(rawBody) == 0 {
		return rewriteResult{Body: rawBody}
	}

	mappedModel, mapped := prov.MapModel(modelName)

	// stream_options 是 OpenAI chat/completions 特有的字段，
	// 其余格式（anthropic / gemini / responses）要么本来就返回用量，
	// 要么不认这个字段，一律不注入。
	wantUsageInject := prov.APIFormat == model.FormatOpenAI && shouldInjectUsageOption(prov, st)
	// responses 的 input 项缺 type 时上游会整条请求判 400，而 OpenAI 规范允许省略
	// 这个字段，所以客户端省略不算写错。这是合规性修正，不设开关：客户端本来就
	// 带 type 时它是空操作，不会留下改写标记。
	wantResponsesFix := prov.APIFormat == model.FormatResponses
	// developer 角色归一化：OpenAI 新客户端（ZCode、DSH）会把系统消息写成
	// role:"developer"，而 Mistral 这类严格上游只认 system/user/assistant/tool，
	// 整条请求会 422。改名无损，直接对所有 OpenAI 系格式生效。
	wantRoleFix := isOpenAIStyle(prov.APIFormat) && bytes.Contains(rawBody, []byte(`"developer"`))
	// thinking / reasoning_content 裁剪则相反：不少兼容网关确实认这些字段，
	// 替用户做主会悄悄关掉推理或丢掉思维链回放，所以只在供应商显式开启严格兼容时才动。
	wantStrip := prov.StripUnsupportedParams && isOpenAIStyle(prov.APIFormat) && hasUnsupportedParam(rawBody)
	// 图片压缩是全局开关，与上游是谁无关：它是纯粹的流量体积优化。
	wantImageCompress := st.ImageCompress.Enabled && hasInlineImage(rawBody)
	// prompt_cache_key 同理：不认这个字段的严格网关会 422，必须按供应商显式开启。
	wantCacheKey := prov.PromptCacheKeyMode == model.PromptCacheKeyAuto && isOpenAIStyle(prov.APIFormat)
	promptText, promptStrategy, hasPrompt := resolvePromptForModel(prov, st, modelName, mappedModel, mapped)
	if !wantUsageInject && !hasPrompt && !wantResponsesFix && !mapped && !wantRoleFix && !wantStrip && !wantCacheKey && !wantImageCompress {
		return rewriteResult{Body: rawBody}
	}

	obj, err := decodeJSONObject(rawBody)
	if err != nil {
		return rewriteResult{Body: rawBody}
	}

	var mods []string
	var imgStats compressStats
	if wantRoleFix && normalizeDeveloperRole(obj) {
		mods = append(mods, "role_developer_to_system")
	}
	if wantUsageInject {
		if injectIncludeUsage(obj) {
			mods = append(mods, "usage_inject")
		}
	}
	// 模型映射排在提示词之前：先确定这次请求到底打的是哪个模型，再往里注入内容。
	if mapped {
		if applyModelMapping(obj, mappedModel) {
			mods = append(mods, "model_map")
		}
	}
	if hasPrompt {
		if applyPrompt(obj, prov.APIFormat, promptText, promptStrategy) {
			mods = append(mods, "prompt_"+promptStrategy)
		}
	}
	// 放在提示词注入之后：注入出来的项同样会被补齐，最终发出去的 body 一律合规。
	if wantResponsesFix && normalizeResponsesInput(obj) {
		mods = append(mods, "responses_input_type")
	}
	if wantStrip {
		// 续传真的要用 store 时才保留它；其余情况下 Responses 的 store 也要裁
		// （Mistral 的 Responses API 是无状态的，store:true 直接 400）。
		keepResponsesStore := prov.ContinuationMode == model.ContinuationOn
		for _, key := range stripUnsupportedParams(obj, keepResponsesStore) {
			mods = append(mods, "strip_"+key)
		}
		for _, key := range stripChatMessageExtraFields(obj) {
			mods = append(mods, "strip_"+key)
		}
		// 工具定义与 Responses 输入项的类型校验：严格上游只认「客户端自己执行的
		// 函数工具」，内置工具（web_search 等）整条 400。见 stripStrictTools。
		for _, name := range stripStrictTools(obj) {
			mods = append(mods, "strip_tool_"+name)
		}
		// 必须排在工具裁剪之后：required 要求至少有一个工具、点名的函数必须在
		// tools 里，裁完之后这些组合才可能变成非法。只修真正非法的组合，
		// 合法的 tool_choice 一个都不动。
		if fixToolChoice(obj) {
			mods = append(mods, "strip_tool_choice")
		}
		for _, name := range stripStrictResponsesInput(obj) {
			mods = append(mods, "strip_input_"+name)
		}
		if stripReasoningEncryptedContent(obj) {
			mods = append(mods, "strip_reasoning_encrypted")
		}
	}
	// 图片压缩排在裁剪之后：被裁掉的项（比如上游不认的 input 项）里可能带着图，
	// 先裁再压能省掉白压一遍的开销。
	if wantImageCompress {
		imgStats = compressImagesInBody(obj, st.ImageCompress)
		if imgStats.Count > 0 {
			mods = append(mods, "image_compress")
		}
	}
	if wantCacheKey {
		if key := derivePromptCacheKey(prov, obj, opts.cacheKeySeed); key != "" && applyPromptCacheKey(obj, key) {
			mods = append(mods, "prompt_cache_key")
		}
	}
	if len(mods) == 0 {
		return rewriteResult{Body: rawBody}
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return rewriteResult{Body: rawBody}
	}
	return rewriteResult{
		Body:            out,
		Modifications:   mods,
		ImageCompressed: imgStats.Count,
		ImageSavedBytes: imgStats.SavedBytes,
	}
}

// isOpenAIStyle 判断该格式是否建立在 OpenAI 的 messages / input 结构上。
func isOpenAIStyle(f model.APIFormat) bool {
	return f == model.FormatOpenAI || f == model.FormatResponses
}

// unsupportedTopLevelParams 是严格上游不认的顶层字段名。
//
// 只收「OpenAI 规范之外、又被常见客户端塞进 body」的几个：Anthropic 风格的
// thinking 开关，以及部分国产网关的 enable_thinking / thinking_budget。
var unsupportedTopLevelParams = []string{"thinking", "enable_thinking", "thinking_budget"}

// chatOnlyUnsupportedParams 只在 chat 形态的请求体上裁剪。
//
// Responses 里 store 是合法字段（续传就靠它），不能动；但 OpenAI chat 规范之外
// 又校得很紧的上游（Mistral 就是）会把 store 当非法输入直接 422 —— 客户端哪怕
// 传的是 store:false 也照拒。
var chatOnlyUnsupportedParams = []string{"store"}

var chatUnsupportedTopLevelParams = append(
	append([]string(nil), unsupportedTopLevelParams...), chatOnlyUnsupportedParams...)

// chatMessageExtraFields 是 chat 消息体里常见、但不属于 OpenAI 规范的字段。
//
// reasoning_content 是客户端把上一轮 assistant 的思维链原样带进历史时捎上的
// （DSH、ZCode 都这么干）。它不在 messages schema 里，而 Mistral 这类
// extra="forbid" 的上游会连消息一起校验，整条请求直接 422：
//
//	{"detail":[{"type":"extra_forbidden","loc":["body","messages",5,"assistant",
//	"reasoning_content"],"msg":"Extra inputs are not permitted"}]}
//
// 删掉它不影响上游理解对话：真正决定上下文的是 content 与 tool_calls，
// 思维链只是回放给模型看的旁注。
//
// 同一类字段按官方 schema 一并补齐（Mistral 的 AssistantMessage 只有
// role/content/tool_calls/prefix）：refusal、annotations、audio 是 OpenAI 的
// 消息级扩展，function_call 是 tool_calls 之前的旧写法，都不在 schema 里。
var chatMessageExtraFields = []string{
	"reasoning_content", "reasoning_details", "reasoning",
	"refusal", "annotations", "audio", "function_call",
}

// prefilterParams 是 hasUnsupportedParam 的字节预筛清单
// （顶层 ∪ 消息级 ∪ name ∪ 严格上游不认的工具/输入项类型）。
var prefilterParams = func() []string {
	out := make([]string, 0, len(chatUnsupportedTopLevelParams)+len(chatMessageExtraFields)+16)
	out = append(out, chatUnsupportedTopLevelParams...)
	out = append(out, chatMessageExtraFields...)
	out = append(out, "name", "previous_response_id")
	// 内置工具与 Responses 输入项的类型名：命中才值得把 body 解析成 JSON。
	out = append(out, "web_search", "namespace", "local_shell", "file_search",
		"code_interpreter", "image_generation", "item_reference", "compaction",
		"mcp_call", "encrypted_content")
	return out
}()

// isResponsesShaped 按 body 结构判断这是不是一次 Responses 请求。
//
// 不能只看供应商的 apiFormat：客户端可能把 /v1/responses 打到配置为 openai 的
// 供应商上。chat 用 messages，Responses 用 input（或 instructions /
// previous_response_id）——有 messages 就按 chat 处理。
func isResponsesShaped(obj map[string]any) bool {
	if _, ok := obj["messages"]; ok {
		return false
	}
	for _, key := range []string{"input", "instructions", "previous_response_id"} {
		if _, ok := obj[key]; ok {
			return true
		}
	}
	return false
}

// unsupportedParamsForBody 按请求体结构选裁剪清单。
func unsupportedParamsForBody(obj map[string]any) []string {
	if isResponsesShaped(obj) {
		return unsupportedTopLevelParams
	}
	return chatUnsupportedTopLevelParams
}

// hasUnsupportedParam 是廉价的字节预筛：没命中就不必把整个 body 解析成 JSON。
//
// 这里按并集扫（含只在 chat 上裁的 store、只在消息上裁的 reasoning_content），
// 因为还没解析、不知道 body 结构；多扫几个词的成本可以忽略，误命中时后面解析完
// 发现无可裁剪字段也不会改写。name 这种常见词几乎必中，代价只是多一次解析。
func hasUnsupportedParam(body []byte) bool {
	for _, key := range prefilterParams {
		if bytes.Contains(body, []byte(`"`+key+`"`)) {
			return true
		}
	}
	return false
}

// normalizeDeveloperRole 把 OpenAI 新引入的 developer 角色改回 system。
//
// Chat 格式看 messages[]，Responses 格式看 input[]（元素可能带 type:"message"）。
// developer 只是 system 的新名字，改名无损；而 Mistral 这类严格上游的 messages
// schema 只有 system/user/assistant/tool 四种角色，不改就整条 422。
func normalizeDeveloperRole(obj map[string]any) bool {
	changed := false
	for _, key := range []string{"messages", "input"} {
		items, ok := obj[key].([]any)
		if !ok {
			continue
		}
		for _, raw := range items {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if role, ok := item["role"].(string); ok && role == "developer" {
				item["role"] = "system"
				changed = true
			}
		}
	}
	return changed
}

// stripUnsupportedParams 删除顶层非标准字段，返回被删掉的键名（用于日志留痕）。
//
// 只动顶层：消息内容里的 {"type":"thinking"} 之类是上游正常返回的结构，不能连带删掉。
// store 默认只对 chat 形态的 body 删；keepResponsesStore 为 false 时连 Responses
// 形态的 store 也删 —— Mistral 的 Responses API 是无状态的，store 只能为 false，
// 客户端传 true 会被直接 400。续传开着的时候必须保留，否则会把续传功能裁掉。
func stripUnsupportedParams(obj map[string]any, keepResponsesStore bool) []string {
	keys := unsupportedParamsForBody(obj)
	if !keepResponsesStore && isResponsesShaped(obj) {
		keys = append(append([]string(nil), keys...), "store", "previous_response_id")
	}
	var removed []string
	for _, key := range keys {
		if _, ok := obj[key]; ok {
			delete(obj, key)
			removed = append(removed, key)
		}
	}
	return removed
}

// stripChatMessageExtraFields 删除 chat 消息里的非标准字段，返回被删掉的键名
// （去重，用于日志留痕）。
//
// 只处理 messages[] 每个元素顶层的键：消息 content 里的 {"type":"thinking"} 是
// 上游正常结构，不能连带删掉；Responses 的 input 也不动 —— 那边的 reasoning 项
// 是合法输入。
//
// 两类字段分开处理：
//   - chatMessageExtraFields：在任何角色上都不是合法字段，一律删；
//   - name：schema 只给 tool 消息留了它，system/user/assistant 带上是 422。
func stripChatMessageExtraFields(obj map[string]any) []string {
	if isResponsesShaped(obj) {
		return nil
	}
	messages, ok := obj["messages"].([]any)
	if !ok {
		return nil
	}
	var removed []string
	seen := map[string]bool{}
	mark := func(key string) {
		// 历史里往往有好几条消息都带同一个字段，日志上留一个标记就够了，
		// 否则改动清单会被同一项刷屏。
		if !seen[key] {
			seen[key] = true
			removed = append(removed, key)
		}
	}
	for _, raw := range messages {
		msg, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range chatMessageExtraFields {
			if _, has := msg[key]; has {
				delete(msg, key)
				mark(key)
			}
		}
		if role, _ := msg["role"].(string); role != "tool" {
			if _, has := msg["name"]; has {
				delete(msg, "name")
				mark("name")
			}
		}
	}
	return removed
}

// ---------- 严格上游的工具与输入项裁剪 ----------
//
// 严格上游（Mistral 的 Responses API 就是）只接受「客户端自己执行的函数工具」，
// 其余一律整条 400：
//
//	{"error":{"code":"3051","message":"tools: Value error, Tool types ['web_search']
//	 are not supported; this endpoint currently supports only client-executed
//	 function tools.","param":"tools","type":"invalid_request_error"}}
//
// 而 Codex 这类客户端默认就带 web_search（服务端内置工具），于是整轮对话直接
// 打穿。这些内置工具本来也轮不到我们执行 —— 代理只做转发，不会替上游跑搜索 ——
// 所以按供应商开关裁掉是无损的：函数工具一个不动，客户端侧的工具调用照常往返。

// strictUnsupportedToolTypes 是严格上游不认的工具类型（顶层与 namespace 内层都算）。
//
// 只收 OpenAI 定义的内置工具：function 是唯一被接受的类型，namespace 是 Codex
// 的分组包装（内层仍然是 function），其余都需要上游自己执行。
var strictUnsupportedToolTypes = map[string]bool{
	"web_search":                    true,
	"web_search_preview":            true,
	"web_search_preview_2025_03_11": true,
	"local_shell":                   true,
	"file_search":                   true,
	"code_interpreter":              true,
	"image_generation":              true,
	"computer_use_preview":          true,
	"mcp":                           true,
	"custom":                        true,
}

// stripStrictTools 删除 tools[] 里严格上游不认的工具，返回被删掉的类型名（去重）。
//
// namespace 分组要单独处理：内层全是 function 时整组保留（Mistral 认这个包装，
// 实测能正常返回带 namespace 的 function_call），内层混了内置工具就把那一项删掉；
// 删空的分组整个移除 —— 空 namespace 会被判成 tools.N.FunctionTool.type 错误。
func stripStrictTools(obj map[string]any) []string {
	tools, ok := obj["tools"].([]any)
	if !ok || len(tools) == 0 {
		return nil
	}

	var removed []string
	seen := map[string]bool{}
	mark := func(name string) {
		if !seen[name] {
			seen[name] = true
			removed = append(removed, name)
		}
	}

	kept := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		typ, _ := tool["type"].(string)
		if typ == "namespace" {
			if pruned, dropped := pruneNamespaceTools(tool); len(dropped) > 0 {
				for _, name := range dropped {
					mark(name)
				}
				if pruned {
					continue // 内层被删空，整组丢弃
				}
			}
			kept = append(kept, tool)
			continue
		}
		if strictUnsupportedToolTypes[typ] {
			mark(typ)
			continue
		}
		kept = append(kept, tool)
	}

	if len(removed) == 0 {
		return nil
	}
	if len(kept) == 0 {
		// 一个工具都不剩时把 tools 整个删掉：空数组本身能被接受，但留着会让
		// tool_choice:"required" 变成非法组合，不如彻底不带。
		delete(obj, "tools")
		return removed
	}
	obj["tools"] = kept
	return removed
}

// pruneNamespaceTools 原地裁掉 namespace 内层的非 function 工具。
// 返回「分组是否已被删空」与「被删掉的类型名」。
func pruneNamespaceTools(tool map[string]any) (bool, []string) {
	inner, ok := tool["tools"].([]any)
	if !ok {
		return false, nil
	}
	var dropped []string
	kept := make([]any, 0, len(inner))
	for _, raw := range inner {
		item, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		if typ, _ := item["type"].(string); strictUnsupportedToolTypes[typ] {
			dropped = append(dropped, typ)
			continue
		}
		kept = append(kept, item)
	}
	if len(dropped) == 0 {
		return false, nil
	}
	tool["tools"] = kept
	return len(kept) == 0, dropped
}

// fixToolChoice 在工具被裁掉之后修正 tool_choice，返回是否真的改动了。
//
// 严格上游对 tool_choice 的校验很死：required 要求至少有一个工具、点名的函数
// 必须在 tools 里、custom 类型直接拒。裁掉内置工具后这几种组合都会变成 400，
// 所以统一回落到 auto —— 语义上就是「本来想强制用某个工具，那个工具没了」。
func fixToolChoice(obj map[string]any) bool {
	choice, ok := obj["tool_choice"]
	if !ok {
		return false
	}
	tools, _ := obj["tools"].([]any)

	switch c := choice.(type) {
	case string:
		if c == "required" && len(tools) == 0 {
			obj["tool_choice"] = "auto"
			return true
		}
		return false
	case map[string]any:
		typ, _ := c["type"].(string)
		if typ == "custom" {
			obj["tool_choice"] = "auto"
			return true
		}
		if typ == "function" {
			// Responses 把函数名放在顶层，chat/completions 包在 function 里，
			// 两种形状都要认。
			name, _ := c["name"].(string)
			if name == "" {
				if fn, ok := c["function"].(map[string]any); ok {
					name, _ = fn["name"].(string)
				}
			}
			if name != "" && !toolNameExists(tools, name) {
				obj["tool_choice"] = "auto"
				return true
			}
		}
	}
	return false
}

// toolNameExists 报告 tools[] 里（含 namespace 内层）是否有这个名字的函数。
//
// 两种工具形状都认：Responses 用顶层 name，chat/completions 包一层 function。
func toolNameExists(tools []any, name string) bool {
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if n, _ := tool["name"].(string); n == name {
			return true
		}
		if fn, ok := tool["function"].(map[string]any); ok {
			if n, _ := fn["name"].(string); n == name {
				return true
			}
		}
		if inner, ok := tool["tools"].([]any); ok && toolNameExists(inner, name) {
			return true
		}
	}
	return false
}

// strictUnsupportedInputTypes 是严格上游不认的 Responses input 项类型。
//
// function_call / function_call_output / message / reasoning 是它认的四种，
// 其余（web_search_call、item_reference、compaction、mcp_call 等）都会整条 400：
//
//	{"error":{"code":"3051","message":"input: Value error, Input items of type
//	 'web_search_call' are not supported.","param":"input"}}
//
// 这些项本身也没有可回放的内容 —— 它们记录的是上游内置工具的执行轨迹。
var strictUnsupportedInputTypes = map[string]bool{
	"web_search_call":         true,
	"file_search_call":        true,
	"code_interpreter_call":   true,
	"image_generation_call":   true,
	"computer_call":           true,
	"computer_call_output":    true,
	"local_shell_call":        true,
	"mcp_call":                true,
	"mcp_list_tools":          true,
	"mcp_approval_request":    true,
	"mcp_approval_response":   true,
	"custom_tool_call":        true,
	"custom_tool_call_output": true,
	"item_reference":          true,
	"compaction":              true,
}

// stripStrictResponsesInput 删除 Responses input[] 里严格上游不认的项类型，
// 返回被删掉的类型名（去重）。只对 Responses 形态的 body 生效。
func stripStrictResponsesInput(obj map[string]any) []string {
	if !isResponsesShaped(obj) {
		return nil
	}
	input, ok := obj["input"].([]any)
	if !ok {
		return nil
	}

	var removed []string
	seen := map[string]bool{}
	kept := make([]any, 0, len(input))
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			kept = append(kept, raw)
			continue
		}
		if typ, _ := item["type"].(string); strictUnsupportedInputTypes[typ] {
			if !seen[typ] {
				seen[typ] = true
				removed = append(removed, typ)
			}
			continue
		}
		kept = append(kept, item)
	}
	if len(removed) == 0 {
		return nil
	}
	obj["input"] = kept
	return removed
}

// stripReasoningEncryptedContent 删除 reasoning 项里的 encrypted_content，返回是否改动。
//
// 客户端（Codex）会带 include:["reasoning.encrypted_content"]，把上一轮拿到的
// 密文原样回放。严格上游明确拒绝：
//
//	{"error":{"code":"3051","message":"input: Value error, Reasoning items with
//	 encrypted_content are not supported; replay reasoning as plaintext content."}}
//
// 注意只删「有值」的：null 能被接受，空串和非空串都会 400。删掉密文后
// summary/content 里的明文思维链仍然保留，多轮推理的连贯性不受影响。
func stripReasoningEncryptedContent(obj map[string]any) bool {
	input, ok := obj["input"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if typ, _ := item["type"].(string); typ != "reasoning" {
			continue
		}
		if v, has := item["encrypted_content"]; has && v != nil {
			delete(item, "encrypted_content")
			changed = true
		}
	}
	return changed
}

// applyPromptCacheKey 把一个稳定的不透明 key 写进请求体顶层。
//
// Mistral 官方文档（prompt-caching）：同一会话使用同一个 prompt_cache_key 能提高
// 前缀缓存命中率，缓存输入按标准输入价的 10% 计费；但它**不保证命中**，只是亲和提示。
// chat 与 responses 的请求体顶层都有这个字段。
//
// 客户端自己带了非空 key 时不覆盖 —— 那是它自己的会话标识，比我们推导的更贴近真实会话。
func applyPromptCacheKey(obj map[string]any, key string) bool {
	if key == "" {
		return false
	}
	if cur, ok := obj["prompt_cache_key"].(string); ok && strings.TrimSpace(cur) != "" {
		return false
	}
	obj["prompt_cache_key"] = key
	return true
}

// shouldInjectUsageOption 解析供应商级的三态开关，inherit 时回落到全局默认。
func shouldInjectUsageOption(prov model.Provider, st model.Settings) bool {
	switch prov.UsageInjectMode {
	case "on":
		return true
	case "off":
		return false
	default:
		return st.UsageInjectDefault
	}
}

// injectIncludeUsage 给流式 OpenAI 请求补上 stream_options.include_usage。
//
// 只有真正改动了内容才返回 true，否则视为未修改，避免日志上出现无意义的改写标记。
func injectIncludeUsage(obj map[string]any) bool {
	streaming, ok := obj["stream"].(bool)
	if !ok || !streaming {
		// 非流式响应本来就带 usage，不需要这个参数。
		return false
	}
	so, ok := obj["stream_options"].(map[string]any)
	if !ok || so == nil {
		obj["stream_options"] = map[string]any{"include_usage": true}
		return true
	}
	if cur, ok := so["include_usage"].(bool); ok && cur {
		return false // 客户端自己已经开了
	}
	// 只补这一个键，保留客户端可能设置的其他 stream_options。
	so["include_usage"] = true
	return true
}

// applyModelMapping 把请求体顶层的 model 字段改写成目标模型名，返回是否真的改动了。
//
// 只在顶层确实有一个非空的字符串 model 键时才动：拉模型列表这类没有 model 的请求
// 不该被无中生有地塞一个字段进去。
func applyModelMapping(obj map[string]any, mappedModel string) bool {
	cur, ok := obj["model"].(string)
	if !ok || cur == "" || cur == mappedModel {
		return false
	}
	obj["model"] = mappedModel
	return true
}

// modelNameFromBody 从请求体里读取模型名，用于匹配提示词规则。
func modelNameFromBody(body []byte) string {
	var probe struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return ""
	}
	return probe.Model
}

// ---------- 提示词解析 ----------

var (
	regexCacheMu sync.Mutex
	regexCache   = map[string]*regexp.Regexp{}
)

// compilePattern 带缓存地编译正则，非法正则返回 nil。
func compilePattern(pattern string) *regexp.Regexp {
	regexCacheMu.Lock()
	defer regexCacheMu.Unlock()
	if re, ok := regexCache[pattern]; ok {
		return re
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		re = nil
	}
	// 缓存里也存 nil，避免每次都重复尝试编译一个坏正则。
	regexCache[pattern] = re
	return re
}

// resolvePromptForModel 决定本次请求使用哪段提示词。
//
// 提示词规则里的模型名默认拿「客户端请求的模型名」去匹配 —— 用户写规则时看到的就是
// 客户端用的名字。但当这次请求被模型映射改写成了另一个上游模型时，按上游真名写的规则
// 也该能生效，所以在客户端名没命中规则时，再用目标模型名补匹配一次。
func resolvePromptForModel(prov model.Provider, st model.Settings, modelName, mappedModel string, mapped bool) (string, string, bool) {
	if text, strategy, ok := resolvePrompt(prov, st, modelName); ok {
		return text, strategy, true
	}
	if mapped && mappedModel != modelName {
		return resolvePrompt(prov, st, mappedModel)
	}
	return "", "", false
}

// resolvePrompt 按优先级决定本次请求使用的提示词：
// 供应商的模型正则规则 > 供应商配置 > 全局配置。
func resolvePrompt(prov model.Provider, st model.Settings, modelName string) (string, string, bool) {
	for _, rule := range prov.PromptRules {
		if !rule.Enabled || strings.TrimSpace(rule.Text) == "" {
			continue
		}
		if rule.ModelPattern == "" {
			return rule.Text, strategyOrDefault(rule.Strategy), true
		}
		if re := compilePattern(rule.ModelPattern); re != nil && re.MatchString(modelName) {
			return rule.Text, strategyOrDefault(rule.Strategy), true
		}
	}

	switch prov.PromptMode {
	case model.ModeCustom:
		if strings.TrimSpace(prov.Prompt.Text) == "" {
			return "", "", false
		}
		return prov.Prompt.Text, strategyOrDefault(prov.Prompt.Strategy), true
	case model.ModeDisable:
		return "", "", false
	}

	if st.GlobalPrompt.Enabled && strings.TrimSpace(st.GlobalPrompt.Text) != "" {
		return st.GlobalPrompt.Text, strategyOrDefault(st.GlobalPrompt.Strategy), true
	}
	return "", "", false
}

func strategyOrDefault(s string) string {
	switch s {
	case model.StrategyAppend, model.StrategyReplace, model.StrategyPrependUser:
		return s
	default:
		return model.StrategyAppend
	}
}

// ---------- 提示词注入 ----------

// applyPrompt 按请求格式把提示词写进请求体，返回是否真的改动了内容。
func applyPrompt(obj map[string]any, format model.APIFormat, text, strategy string) bool {
	switch format {
	case model.FormatAnthropic:
		return applyPromptAnthropic(obj, text, strategy)
	case model.FormatGemini:
		return applyPromptGemini(obj, text, strategy)
	case model.FormatResponses:
		return applyPromptResponses(obj, text, strategy)
	default:
		// openai 与 custom 都按 OpenAI 的消息数组结构处理。
		return applyPromptOpenAI(obj, text, strategy)
	}
}

// applyPromptOpenAI 处理 messages 数组里的 system/developer 消息。
func applyPromptOpenAI(obj map[string]any, text, strategy string) bool {
	messages, ok := obj["messages"].([]any)
	if !ok {
		return false
	}

	if strategy == model.StrategyPrependUser {
		msg := map[string]any{"role": "user", "content": text}
		obj["messages"] = append([]any{msg}, messages...)
		return true
	}

	idx := -1
	for i, raw := range messages {
		m, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if role, _ := m["role"].(string); role == "system" || role == "developer" {
			idx = i
			break
		}
	}
	if idx < 0 {
		// 没有 system 消息就补一条在最前面。
		msg := map[string]any{"role": "system", "content": text}
		obj["messages"] = append([]any{msg}, messages...)
		return true
	}

	m, ok := messages[idx].(map[string]any)
	if !ok {
		return false
	}
	if strategy == model.StrategyReplace {
		m["content"] = text
		return true
	}

	// append：content 可能是字符串，也可能是分块数组，两种都要支持。
	switch c := m["content"].(type) {
	case nil:
		m["content"] = text
	case string:
		m["content"] = c + "\n\n" + text
	case []any:
		m["content"] = append(c, map[string]any{"type": "text", "text": text})
	default:
		return false
	}
	return true
}

// applyPromptAnthropic 处理顶层 system 字段（字符串或 blocks 数组）。
func applyPromptAnthropic(obj map[string]any, text, strategy string) bool {
	if strategy == model.StrategyPrependUser {
		messages, ok := obj["messages"].([]any)
		if !ok {
			return false
		}
		msg := map[string]any{
			"role":    "user",
			"content": []any{map[string]any{"type": "text", "text": text}},
		}
		obj["messages"] = append([]any{msg}, messages...)
		return true
	}

	switch cur := obj["system"].(type) {
	case nil:
		obj["system"] = text
	case string:
		if strategy == model.StrategyReplace {
			obj["system"] = text
		} else {
			obj["system"] = cur + "\n\n" + text
		}
	case []any:
		if strategy == model.StrategyReplace {
			obj["system"] = []any{map[string]any{"type": "text", "text": text}}
		} else {
			obj["system"] = append(cur, map[string]any{"type": "text", "text": text})
		}
	default:
		return false
	}
	return true
}

// applyPromptGemini 处理 systemInstruction.parts 结构。
func applyPromptGemini(obj map[string]any, text, strategy string) bool {
	if strategy == model.StrategyPrependUser {
		contents, ok := obj["contents"].([]any)
		if !ok {
			return false
		}
		msg := map[string]any{
			"role":  "user",
			"parts": []any{map[string]any{"text": text}},
		}
		obj["contents"] = append([]any{msg}, contents...)
		return true
	}

	si, ok := obj["systemInstruction"].(map[string]any)
	if !ok || si == nil {
		obj["systemInstruction"] = map[string]any{
			"parts": []any{map[string]any{"text": text}},
		}
		return true
	}
	parts, _ := si["parts"].([]any)
	if strategy == model.StrategyReplace || parts == nil {
		si["parts"] = []any{map[string]any{"text": text}}
		return true
	}
	si["parts"] = append(parts, map[string]any{"text": text})
	return true
}

// applyPromptResponses 处理 responses 接口的 instructions 字段。
func applyPromptResponses(obj map[string]any, text, strategy string) bool {
	if strategy == model.StrategyPrependUser {
		input, ok := obj["input"].([]any)
		if !ok {
			return false
		}
		msg := map[string]any{"role": "user", "content": text}
		obj["input"] = append([]any{msg}, input...)
		return true
	}

	cur, _ := obj["instructions"].(string)
	if strategy == model.StrategyReplace || cur == "" {
		obj["instructions"] = text
		return true
	}
	obj["instructions"] = cur + "\n\n" + text
	return true
}

// ---------- responses 请求体规范化 ----------

// normalizeResponsesInput 给 responses 请求体 input 数组里缺 type 的消息项补上 type。
//
// 上游按判别字段逐项校验 input，缺 type 的消息项会被判成无法识别的输入，
// 整条请求回 400（实测 commandcode 的报错是 MissingParameter `input.type`）。
// 而 OpenAI 对 EasyInputMessage 的 type 是可选的，所以客户端省略 type 并不算写错 ——
// 这正是实际踩到的坑：ZCode 把 assistant 历史写成
// {"role":"assistant","content":[{"type":"output_text",...}]}，第一轮只有 system+user
// 时上游照收，第二轮带上 assistant 历史就整条被拒。
//
// 只补 type，内容片段与其他字段一律不动（实测补完即被上游接受，且 system/user 项
// 一并补 type 也照收）。input 直接是字符串、或项本身没有 role 时无从判断，保持原样。
func normalizeResponsesInput(obj map[string]any) bool {
	input, ok := obj["input"].([]any)
	if !ok {
		return false
	}

	changed := false
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			continue // input 允许整体是字符串
		}
		if _, has := item["type"]; has {
			continue // 客户端本来就写对了
		}
		if _, has := item["role"]; !has {
			continue // 既没有 type 也没有 role，猜不出它是什么
		}
		item["type"] = "message"
		changed = true
	}
	return changed
}
