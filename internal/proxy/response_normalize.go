package proxy

// chat/completions 响应归一化。
//
// 有些推理模型（Mistral 上的 deepseek-v4-1-flash-1017 就是一例）会把
// delta.content 写成「分块数组」：
//
//	"content":[{"type":"thinking","thinking":[{"type":"text","text":"The"}],"closed":true}]
//
// 这是 Responses 风格的写法，但 OpenAI Chat Completions 规范里 content 只能是
// 字符串（或 null）。按 OpenAI SDK 生成的客户端会直接类型校验失败：
//
//	ZCode: invalid_union, choices.0.delta.content expected string, received array
//	DSH  : text-delta text must be a string
//
// 所以这里做一层最小归一化：正文块拼回字符串，思考块挪到 reasoning_content
//（DeepSeek / OpenRouter 的通行约定，不认识的客户端会忽略），其余类型丢弃。
//
// 只处理 chat/completions：Responses 的 output 本来就是带类型的数组，不能动。

import (
	"encoding/json"
	"io"
	"strings"
)

// maxNormalizeBodyBytes 是非流式响应体的归一化缓冲上限。超过就放弃改写、
// 原样透传：兼容性不能拿内存和稳定性去换。
const maxNormalizeBodyBytes = 32 << 20

// prefixedReadCloser 把「已缓冲的前缀 + 剩余上游流」接回一个 ReadCloser。
type prefixedReadCloser struct {
	io.Reader
	io.Closer
}

// normalizeChatSSEEvent 归一化一个 SSE 事件。
//
// 只在事件没有 event 名时动手——OpenAI chat 的流就是一串纯 data 行，
// 带 event 名的多半是别家协议，不能按 chat 的解释去改。返回新的完整事件字节
// 与新的 data 文本，第三个返回值表示是否真的改了。
func normalizeChatSSEEvent(event, data string) ([]byte, string, bool) {
	if event != "" {
		return nil, "", false
	}
	newData, changed := normalizeChatChunkData(data)
	if !changed {
		return nil, "", false
	}
	return []byte("data: " + newData + "\n\n"), newData, true
}

// normalizeChatChunkData 归一化单个 data 负载（不含 "data: " 前缀）。
func normalizeChatChunkData(data string) (string, bool) {
	if data == "" || data == "[DONE]" {
		return data, false
	}
	obj, err := decodeJSONObject([]byte(data))
	if err != nil {
		return data, false
	}
	if !normalizeChatChoices(obj) {
		return data, false
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return data, false
	}
	return string(out), true
}

// normalizeChatResponseBody 归一化非流式响应体。
//
// 解析失败（比如上游返回了错误页或超限体）时原样返回，绝不把响应改坏。
func normalizeChatResponseBody(body []byte) ([]byte, bool) {
	obj, err := decodeJSONObject(body)
	if err != nil {
		return body, false
	}
	if !normalizeChatChoices(obj) {
		return body, false
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return body, false
	}
	return out, true
}

// normalizeChatChoices 遍历 choices，处理 delta 与 message 两种容器。
func normalizeChatChoices(obj map[string]any) bool {
	choices, ok := obj["choices"].([]any)
	if !ok {
		return false
	}
	changed := false
	for _, raw := range choices {
		choice, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		for _, key := range []string{"delta", "message"} {
			container, ok := choice[key].(map[string]any)
			if !ok {
				continue
			}
			if normalizeChatContainer(container) {
				changed = true
			}
		}
	}
	return changed
}

// normalizeChatContainer 把 content 数组拍平成字符串。
//
// 正文（text / output_text）回填 content；思考（thinking / reasoning）回填
// reasoning_content。返回是否改动过。
func normalizeChatContainer(container map[string]any) bool {
	raw, ok := container["content"]
	if !ok {
		return false
	}
	arr, ok := raw.([]any)
	if !ok {
		// 已经是字符串（或 null），没什么可做的。
		return false
	}

	var text, reasoning strings.Builder
	for _, item := range arr {
		part, ok := item.(map[string]any)
		if !ok {
			continue
		}
		kind, _ := part["type"].(string)
		switch kind {
		case "text", "output_text":
			if s, ok := part["text"].(string); ok {
				text.WriteString(s)
			}
		case "thinking", "reasoning":
			if inner, ok := part["thinking"]; ok {
				appendThinkingText(inner, &reasoning)
			}
			if inner, ok := part["reasoning"]; ok {
				appendThinkingText(inner, &reasoning)
			}
			if s, ok := part["text"].(string); ok {
				reasoning.WriteString(s)
			}
		}
	}

	container["content"] = text.String()
	if reasoning.Len() > 0 {
		// 上游自己已经带了 reasoning_content 时往后追加，不覆盖。
		if prev, ok := container["reasoning_content"].(string); ok && prev != "" {
			container["reasoning_content"] = prev + reasoning.String()
		} else {
			container["reasoning_content"] = reasoning.String()
		}
	}
	return true
}

// appendThinkingText 从思考块里抽文本，兼容三种写法：
// 字符串、{"type":"text","text":...} 数组、以及再嵌套一层的结构。
func appendThinkingText(v any, sb *strings.Builder) {
	switch t := v.(type) {
	case string:
		sb.WriteString(t)
	case []any:
		for _, item := range t {
			appendThinkingText(item, sb)
		}
	case map[string]any:
		if s, ok := t["text"].(string); ok {
			sb.WriteString(s)
		}
		if inner, ok := t["content"]; ok {
			appendThinkingText(inner, sb)
		}
	}
}
