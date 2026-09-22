package proxy

import (
	"encoding/json"
	"strconv"
	"strings"

	"ai_proxy/internal/model"
)

// Usage 是一次请求的用量统计。
//
// 各家的原始语义并不一致，这里已统一为同一口径：
//   - PromptTokens 是「输入总量」，含命中缓存的与写入缓存的部分
//     （Anthropic 的 input_tokens 不含缓存，读取时会补齐；
//     OpenAI 的 prompt_tokens 本身已包含，直接沿用）；
//   - CachedTokens 是输入中命中缓存的量；
//   - CacheWriteTokens 是输入中写入缓存的量（目前只有 Anthropic 报这个值）；
//   - CompletionTokens 是输出量；ReasoningTokens 是其中属于思维链的部分；
//   - TotalTokens 恒等于 PromptTokens + CompletionTokens。
//
// 这样前端跨供应商对比时口径一致。
type Usage struct {
	PromptTokens     int  `json:"promptTokens"`
	CachedTokens     int  `json:"cachedTokens"`
	CacheWriteTokens int  `json:"cacheWriteTokens"`
	CompletionTokens int  `json:"completionTokens"`
	ReasoningTokens  int  `json:"reasoningTokens"`
	TotalTokens      int  `json:"totalTokens"`
	Found            bool `json:"found"`     // 上游是否真的返回了用量
	Estimated        bool `json:"estimated"` // 数值是否来自本地估算
}

// normalize 负责补齐 TotalTokens 并做一致性收敛。
func (u *Usage) normalize() {
	if u.TotalTokens <= 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = u.PromptTokens + u.CompletionTokens
	}
	if u.ReasoningTokens > u.CompletionTokens {
		// 个别网关会把 reasoning 单独累加，导致大于输出量，收敛到输出量以内。
		u.ReasoningTokens = u.CompletionTokens
	}
}

// jsonNumber 是 JSON 里各种数字写法（int / float / string）的统一读取入口。
func jsonNumber(v any) (int, bool) {
	switch t := v.(type) {
	case float64:
		return int(t), true
	case int:
		return t, true
	case int64:
		return int(t), true
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return int(n), true
		}
		if f, err := t.Float64(); err == nil {
			return int(f), true
		}
	case string:
		// 少数网关把数字序列化成字符串。
		if n, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
			return n, true
		}
	}
	return 0, false
}

// getPath 按 "a.b.c" 路径读取嵌套 JSON 的数值，缺失返回 false。
// 路径段可以用 * 表示遍历数组求和，也可以用下标选择数组元素。
func getPath(root any, path string) (int, bool) {
	if path == "" {
		return jsonNumber(root)
	}
	seg, rest, _ := strings.Cut(path, ".")
	switch node := root.(type) {
	case map[string]any:
		next, ok := node[seg]
		if !ok {
			return 0, false
		}
		return getPath(next, rest)
	case []any:
		if seg == "*" {
			sum, found := 0, false
			for _, item := range node {
				if n, ok := getPath(item, rest); ok {
					sum += n
					found = true
				}
			}
			return sum, found
		}
		idx, err := strconv.Atoi(seg)
		if err != nil || idx < 0 || idx >= len(node) {
			return 0, false
		}
		return getPath(node[idx], rest)
	default:
		return 0, false
	}
}

// getString 按路径读取字符串字段。
func getString(root any, path string) (string, bool) {
	if path == "" {
		return "", false
	}
	cur := root
	for _, seg := range strings.Split(path, ".") {
		node, ok := cur.(map[string]any)
		if !ok {
			return "", false
		}
		next, ok := node[seg]
		if !ok {
			return "", false
		}
		cur = next
	}
	s, ok := cur.(string)
	return s, ok
}

// getMap 按路径读取子对象。
func getMap(root any, path string) (map[string]any, bool) {
	cur := root
	for _, seg := range strings.Split(path, ".") {
		node, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		next, ok := node[seg]
		if !ok {
			return nil, false
		}
		cur = next
	}
	m, ok := cur.(map[string]any)
	return m, ok
}

// decodeJSONObject 用 UseNumber 解析，避免大整数被转成 float64 丢精度。
func decodeJSONObject(body []byte) (map[string]any, error) {
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.UseNumber()
	var m map[string]any
	if err := dec.Decode(&m); err != nil {
		return nil, err
	}
	return m, nil
}

// extractUsageFromObject 从已解析的 JSON 对象中按格式提取用量。
//
// obj 对非流式响应是响应体本身；对流式响应是单个 SSE 事件的 data。
func extractUsageFromObject(format model.APIFormat, cu *model.CustomUsageMapping, obj map[string]any) Usage {
	var u Usage

	switch format {
	case model.FormatOpenAI:
		usage, ok := getMap(obj, "usage")
		if !ok {
			return u
		}
		u.Found = true
		u.PromptTokens, _ = getPath(usage, "prompt_tokens")
		u.CompletionTokens, _ = getPath(usage, "completion_tokens")
		u.TotalTokens, _ = getPath(usage, "total_tokens")
		u.CachedTokens, _ = getPath(usage, "prompt_tokens_details.cached_tokens")
		u.ReasoningTokens, _ = getPath(usage, "completion_tokens_details.reasoning_tokens")

	case model.FormatResponses:
		// responses 接口把 usage 放在顶层，或包在 response 对象里。
		usage, ok := getMap(obj, "usage")
		if !ok {
			usage, ok = getMap(obj, "response.usage")
		}
		if !ok {
			return u
		}
		u.Found = true
		u.PromptTokens, _ = getPath(usage, "input_tokens")
		u.CompletionTokens, _ = getPath(usage, "output_tokens")
		u.TotalTokens, _ = getPath(usage, "total_tokens")
		u.CachedTokens, _ = getPath(usage, "input_tokens_details.cached_tokens")
		u.ReasoningTokens, _ = getPath(usage, "output_tokens_details.reasoning_tokens")

	case model.FormatAnthropic:
		usage, ok := getMap(obj, "usage")
		if !ok {
			// message_start 事件把 usage 埋在 message 里。
			usage, ok = getMap(obj, "message.usage")
		}
		if !ok {
			return u
		}
		u.Found = true
		rawInput, _ := getPath(usage, "input_tokens")
		u.CachedTokens, _ = getPath(usage, "cache_read_input_tokens")
		u.CacheWriteTokens, _ = getPath(usage, "cache_creation_input_tokens")
		u.CompletionTokens, _ = getPath(usage, "output_tokens")
		// Anthropic 的 input_tokens 不含缓存部分，补齐成统一口径的输入总量。
		u.PromptTokens = rawInput + u.CachedTokens + u.CacheWriteTokens

	case model.FormatGemini:
		usage, ok := getMap(obj, "usageMetadata")
		if !ok {
			return u
		}
		u.Found = true
		u.PromptTokens, _ = getPath(usage, "promptTokenCount")
		u.CompletionTokens, _ = getPath(usage, "candidatesTokenCount")
		u.TotalTokens, _ = getPath(usage, "totalTokenCount")
		u.CachedTokens, _ = getPath(usage, "cachedContentTokenCount")
		u.ReasoningTokens, _ = getPath(usage, "thoughtsTokenCount")
		// Gemini 的 promptTokenCount 已含 cachedContentTokenCount，无需补齐。

	case model.FormatCustom:
		if cu == nil {
			return u
		}
		fields := []struct {
			path string
			dst  *int
		}{
			{cu.PromptTokens, &u.PromptTokens},
			{cu.CachedTokens, &u.CachedTokens},
			{cu.CacheWriteTokens, &u.CacheWriteTokens},
			{cu.CompletionTokens, &u.CompletionTokens},
			{cu.ReasoningTokens, &u.ReasoningTokens},
			{cu.TotalTokens, &u.TotalTokens},
		}
		for _, f := range fields {
			if f.path == "" {
				continue
			}
			if n, ok := getPath(obj, f.path); ok {
				*f.dst = n
				u.Found = true
			}
		}
	}

	u.normalize()
	return u
}

// extractModel 从响应对象里取回显的模型名。
func extractModel(format model.APIFormat, cu *model.CustomUsageMapping, obj map[string]any) string {
	if format == model.FormatCustom && cu != nil && cu.Model != "" {
		if s, ok := getString(obj, cu.Model); ok {
			return s
		}
		return ""
	}
	for _, path := range []string{"model", "modelVersion", "response.model", "message.model"} {
		if s, ok := getString(obj, path); ok && s != "" {
			return s
		}
	}
	return ""
}

// extractError 从响应对象里取错误描述，便于把失败原因落到日志里。
func extractError(obj map[string]any) string {
	if m, ok := getMap(obj, "error"); ok {
		if s, ok := getString(m, "message"); ok && s != "" {
			return s
		}
		if s, ok := getString(m, "type"); ok && s != "" {
			return s
		}
	}
	for _, path := range []string{"message", "detail", "error_description", "response.error.message"} {
		if s, ok := getString(obj, path); ok && s != "" {
			return s
		}
	}
	return ""
}
