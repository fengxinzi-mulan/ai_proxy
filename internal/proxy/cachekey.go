package proxy

// prompt_cache_key 的派生。
//
// 上游（Mistral）只把它当「缓存亲和」的提示：同一个 key 的请求更容易落到同一个
// 缓存分片，从而命中此前算好的前缀 KV。因此它必须**跨轮稳定**，又不能把客户端
// 的会话标识原样送给上游：这里统一 SHA-256 后取前 16 字节（32 个 hex 字符），
// 再加 aipx- 前缀。
//
// 官方文档：https://docs.mistral.ai/inference/conversations/advanced/prompt-caching

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"ai_proxy/internal/model"
)

// promptCacheKeyPrefix 让日志/抓包时一眼看出这是我们注入的 key，而不是客户端自带。
const promptCacheKeyPrefix = "aipx-"

// sessionCacheSeed 取客户端会话头作为稳定标识。
//
// 复用续传那边的提取逻辑（X-Session-Id / X-Opencode-Session / Session_id）：
// 这几个头在同一个会话里不变，正好是官方文档建议的「会话 id」。
// 客户端没给会话头时返回空串，由前缀指纹兜底。
func sessionCacheSeed(r *http.Request) string {
	if r == nil {
		return ""
	}
	return continuationSessionKey(r, nil)
}

// derivePromptCacheKey 为一个请求推导 prompt_cache_key；空串表示本次不注入。
//
// seed 优先（客户端会话头）；没有 seed 时退回「稳定前缀指纹」：
// 首条消息 + tools 的摘要 —— 这两部分在多轮请求里不变，只有尾部在增长，
// 所以同一会话派生出的 key 稳定，不同会话（系统提示不同）自然分开。
func derivePromptCacheKey(prov model.Provider, obj map[string]any, seed string) string {
	identity := strings.TrimSpace(seed)
	if identity == "" {
		identity = stablePromptFingerprint(obj)
	}
	if identity == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s", prov.ID, prov.APIFormat, identity)))
	return promptCacheKeyPrefix + hex.EncodeToString(sum[:16])
}

// stablePromptFingerprint 取「首条消息 + tools」的摘要，作为会话身份的替身。
//
// chat 看 messages[0]，Responses 看 input（可能是数组也可能是字符串）；
// 都没有时返回空串，宁可不注入也不要拿整份 body 去算 —— 那样每轮都会变，
// 反而把缓存打散。
func stablePromptFingerprint(obj map[string]any) string {
	var sb strings.Builder

	switch {
	case hasNonEmptyArray(obj, "messages"):
		writeFirstArrayItem(&sb, obj, "messages")
	case hasNonEmptyArray(obj, "input"):
		writeFirstArrayItem(&sb, obj, "input")
	default:
		if s, ok := obj["input"].(string); ok && strings.TrimSpace(s) != "" {
			sb.WriteString(s)
		} else if s, ok := obj["instructions"].(string); ok {
			sb.WriteString(s)
		}
	}
	if sb.Len() == 0 {
		return ""
	}

	// tools 也参与指纹：换了工具集（比如换了 MCP 配置）就是另一条缓存链。
	if tools, ok := obj["tools"]; ok {
		if b, err := json.Marshal(tools); err == nil {
			sb.Write(b)
		}
	}

	sum := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(sum[:16])
}

func hasNonEmptyArray(obj map[string]any, key string) bool {
	arr, ok := obj[key].([]any)
	return ok && len(arr) > 0
}

func writeFirstArrayItem(sb *strings.Builder, obj map[string]any, key string) {
	arr, _ := obj[key].([]any)
	if len(arr) == 0 {
		return
	}
	if b, err := json.Marshal(arr[0]); err == nil {
		sb.Write(b)
	}
}
