package proxy

import (
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
	if len(rawBody) == 0 {
		return rawBody, nil
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
	promptText, promptStrategy, hasPrompt := resolvePromptForModel(prov, st, modelName, mappedModel, mapped)
	if !wantUsageInject && !hasPrompt && !wantResponsesFix && !mapped {
		return rawBody, nil
	}

	obj, err := decodeJSONObject(rawBody)
	if err != nil {
		return rawBody, nil
	}

	var mods []string
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
	if len(mods) == 0 {
		return rawBody, nil
	}

	out, err := json.Marshal(obj)
	if err != nil {
		return rawBody, nil
	}
	return out, mods
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
