package proxy

import (
	"strings"
	"unicode"

	"ai_proxy/internal/model"
)

// 上游没有返回用量时（典型场景：OpenAI 流式未开启 include_usage，
// 或请求在收到 usage 之前就断了），用这里的估算值兜底。
//
// 估算规则刻意做得简单且可解释：
//   - CJK / 假名 / 谚文等表意文字按「1 字 ≈ 1 token」；
//   - 其余字符按「4 字符 ≈ 1 token」；
//
// 精确度对英文大致在 ±15%，中文较好。日志里会用 tokens_estimated 标记，
// 前端也会明确提示这是估算值，不会和上游真实用量混为一谈。

// estimateTokens 估算一段文本的 token 数。
func estimateTokens(s string) int {
	wide, narrow := tokenClassCounts(s)
	return wide + (narrow+3)/4
}

// tokenClassCounts 把文本拆成「按字计数」与「按 4 字符计数」两类字符数。
//
// 拆成原始计数而不是每段各自估算再加总：流式响应是一个片段一个片段喂进来的，
// 每段都自己 (narrow+3)/4 会把每段的零头都向上取整，一段一个 token 地累积
// 膨胀，短片段多时估出来的输出量能高出一大截。
func tokenClassCounts(s string) (wide, narrow int) {
	for _, r := range s {
		if isWideRune(r) {
			wide++
		} else if !unicode.IsSpace(r) {
			narrow++
		}
	}
	return wide, narrow
}

// isWideRune 判断是否属于按字计数的表意文字区段。
func isWideRune(r rune) bool {
	switch {
	case r >= 0x4E00 && r <= 0x9FFF, // CJK 统一表意文字
		r >= 0x3400 && r <= 0x4DBF, // 扩展 A
		r >= 0xF900 && r <= 0xFAFF, // 兼容表意文字
		r >= 0x3000 && r <= 0x303F, // CJK 标点
		r >= 0x3040 && r <= 0x30FF, // 平假名 / 片假名
		r >= 0xAC00 && r <= 0xD7AF, // 谚文音节
		r >= 0xFF00 && r <= 0xFFEF, // 全角字符
		r >= 0x2E80 && r <= 0x2EFF: // CJK 部首补充
		return true
	}
	return false
}

// estimatePromptTokens 从请求体估算输入 token 数。
func estimatePromptTokens(format model.APIFormat, body []byte) int {
	obj, err := decodeJSONObject(body)
	if err != nil {
		return 0
	}
	var sb strings.Builder
	messages := 0

	switch format {
	case model.FormatAnthropic:
		appendTextValue(obj["system"], &sb)
		messages = appendMessages(obj["messages"], &sb)
	case model.FormatGemini:
		appendTextValue(obj["systemInstruction"], &sb)
		messages = appendGeminiContents(obj["contents"], &sb)
	case model.FormatResponses:
		appendTextValue(obj["instructions"], &sb)
		// Responses 的 input 既可以是消息数组，也可以是一整段字符串
		// （{"input":"你好"} 是最简写法，probe 里也用它）。字符串形式
		// 不处理就会把全部输入算成 0。
		if text, ok := obj["input"].(string); ok {
			sb.WriteString(text)
			messages = 1
		} else {
			messages = appendMessages(obj["input"], &sb)
		}
		if messages == 0 {
			messages = appendMessages(obj["messages"], &sb)
		}
	default:
		messages = appendMessages(obj["messages"], &sb)
	}

	// 每条消息的结构开销（role、分隔符等），按 OpenAI 的经验值取 4。
	return estimateTokens(sb.String()) + messages*4
}

// appendTextValue 从一个 content 字段里抽取文本，兼容三种常见写法：
// 纯字符串、字符串数组、以及 {"type":"text","text":"..."} 结构数组。
// 返回抽取到的文本片段数量。
func appendTextValue(v any, sb *strings.Builder) int {
	switch t := v.(type) {
	case string:
		sb.WriteString(t)
		return 1
	case []any:
		n := 0
		for _, item := range t {
			n += appendTextValue(item, sb)
		}
		return n
	case map[string]any:
		n := 0
		// text 是标准的文本块字段；content 用于嵌套结构。
		if s, ok := t["text"].(string); ok {
			sb.WriteString(s)
			n++
		}
		if inner, ok := t["content"]; ok {
			n += appendTextValue(inner, sb)
		}
		if parts, ok := t["parts"]; ok {
			n += appendTextValue(parts, sb)
		}
		return n
	}
	return 0
}

// appendMessages 遍历消息数组，返回消息条数。
func appendMessages(v any, sb *strings.Builder) int {
	arr, ok := v.([]any)
	if !ok {
		return 0
	}
	for _, item := range arr {
		switch m := item.(type) {
		case string:
			sb.WriteString(m)
		case map[string]any:
			if c, ok := m["content"]; ok {
				appendTextValue(c, sb)
			} else if txt, ok := m["text"]; ok {
				appendTextValue(txt, sb)
			}
		}
	}
	return len(arr)
}

// appendGeminiContents 遍历 Gemini 的 contents 数组，返回消息条数。
func appendGeminiContents(v any, sb *strings.Builder) int {
	arr, ok := v.([]any)
	if !ok {
		return 0
	}
	for _, item := range arr {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		appendTextValue(m["parts"], sb)
	}
	return len(arr)
}
