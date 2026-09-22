package proxy

import (
	"fmt"
	"strings"

	"ai_proxy/internal/model"
)

// statusClientClosedRequest 是 nginx 约定俗成的「客户端主动断开」状态码。
// 它不会真的发给客户端（客户端已经走了），只用于日志里区分「谁的锅」——
// 客户端取消不算上游故障，不该拉低成功率的分子分母。
const statusClientClosedRequest = 499

// extractReasoningEffort 从请求体里读出推理强度设置，用于日志展示。
//
// 各家的字段命名完全不同，这里统一渲染成一段可读文本：
//   - OpenAI / responses：原样返回 reasoning_effort 或 reasoning.effort；
//   - Anthropic：返回 "budget:<token 数>"；
//   - Gemini：返回 "budget:<token 数>"；
//
// 读不到就返回空串，前端显示为「—」。
func extractReasoningEffort(format model.APIFormat, body []byte) string {
	if len(body) == 0 {
		return ""
	}
	obj, err := decodeJSONObject(body)
	if err != nil {
		return ""
	}

	switch format {
	case model.FormatAnthropic:
		if thinking, ok := getMap(obj, "thinking"); ok {
			if t, ok := getString(thinking, "type"); ok && t != "" && t != "enabled" {
				return t
			}
			if n, ok := getPath(thinking, "budget_tokens"); ok {
				return fmt.Sprintf("budget:%d", n)
			}
			return "enabled"
		}
	case model.FormatGemini:
		if tc, ok := getMap(obj, "generationConfig.thinkingConfig"); ok {
			if n, ok := getPath(tc, "thinkingBudget"); ok {
				return fmt.Sprintf("budget:%d", n)
			}
			if lvl, ok := getString(tc, "thinkingLevel"); ok && lvl != "" {
				return lvl
			}
		}
	case model.FormatResponses:
		if s, ok := getString(obj, "reasoning.effort"); ok && s != "" {
			return s
		}
		if s, ok := getString(obj, "reasoning_effort"); ok && s != "" {
			return s
		}
	default:
		if s, ok := getString(obj, "reasoning_effort"); ok && s != "" {
			return s
		}
		if s, ok := getString(obj, "reasoning.effort"); ok && s != "" {
			return s
		}
		// 部分兼容网关把推理开关放在 enable_thinking / thinking 里。
		if b, ok := obj["enable_thinking"].(bool); ok && b {
			return "enabled"
		}
		if thinking, ok := getMap(obj, "thinking"); ok {
			if s, ok := getString(thinking, "type"); ok && s != "" && s != "disabled" {
				return s
			}
		}
	}

	// 一些网关用字符串形式的布尔值表达「开启思考」。
	if s, ok := getString(obj, "reasoning_effort"); ok {
		return strings.TrimSpace(s)
	}
	return ""
}
