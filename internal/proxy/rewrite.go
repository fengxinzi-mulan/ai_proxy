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
// 这是整个程序里唯一会改动请求内容的地方，改动项各自独立开关，并且每一项都会
// 记进日志（req_body_original 保存改写前的原文），保证「透明转发」可核查。
//
// 任何一步出错（JSON 解析失败、结构不符合该格式）都直接放弃改写并原样转发 —
// 宁可少注入一次提示词，也不能把用户的请求改坏。
func rewriteRequestBody(prov model.Provider, st model.Settings, rawBody []byte) ([]byte, []string) {
	if len(rawBody) == 0 {
		return rawBody, nil
	}

	// stream_options 是 OpenAI chat/completions 特有的字段，
	// 其余格式（anthropic / gemini / responses）要么本来就返回用量，
	// 要么不认这个字段，一律不注入。
	wantUsageInject := prov.APIFormat == model.FormatOpenAI && shouldInjectUsageOption(prov, st)
	promptText, promptStrategy, hasPrompt := resolvePrompt(prov, st, modelNameFromBody(rawBody))
	if !wantUsageInject && !hasPrompt {
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
	if hasPrompt {
		if applyPrompt(obj, prov.APIFormat, promptText, promptStrategy) {
			mods = append(mods, "prompt_"+promptStrategy)
		}
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
