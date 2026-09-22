package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"ai_proxy/internal/model"
)

// ProbeResult 是一次连通性测试的结果。
type ProbeResult struct {
	OK        bool     `json:"ok"`
	Status    int      `json:"status"`
	LatencyMs int64    `json:"latencyMs"`
	Message   string   `json:"message"`
	UsedURL   string   `json:"usedUrl"`
	Models    []string `json:"models"`
	// AuthOK 区分「网络不通」和「通了但密钥不对」——这是排障时最先要分清的两件事。
	AuthOK bool `json:"authOk"`
}

// ProbeProvider 验证供应商是否可用。
//
// 探测分两步，先轻后重：
//  1. 拉模型列表（GET），既能验证连通性与鉴权，又能顺带拿到模型清单；
//  2. 若上游不提供模型列表（不少兼容网关没有这个接口），
//     且在调用方给了模型名的情况下，退化为一次 max_tokens=1 的最小对话请求。
//
// 这样无论面对的是官方 API 还是精简过的自建网关，都能给出有用的结论。
func (s *Server) ProbeProvider(ctx context.Context, prov model.Provider, probeModel string, settings model.Settings) ProbeResult {
	start := time.Now()
	netcfg := resolveNet(prov, settings)
	client, err := s.clients.client(netcfg)
	if err != nil {
		return ProbeResult{Message: "初始化连接失败: " + err.Error()}
	}

	models, listStatus, listErr := s.fetchModels(ctx, client, prov)
	if listErr == nil && listStatus >= 200 && listStatus < 300 {
		return ProbeResult{
			OK:        true,
			Status:    listStatus,
			LatencyMs: time.Since(start).Milliseconds(),
			Message:   fmt.Sprintf("连接正常，上游返回 %d 个模型", len(models)),
			Models:    models,
			AuthOK:    true,
		}
	}

	// 模型列表不可用时，用最小的对话请求兜底。
	if probeModel != "" {
		status, body, chatErr := s.probeChat(ctx, client, prov, probeModel)
		latency := time.Since(start).Milliseconds()
		if chatErr == nil && status >= 200 && status < 300 {
			return ProbeResult{
				OK:        true,
				Status:    status,
				LatencyMs: latency,
				Message:   "连接正常（该上游未提供模型列表接口，已用最小请求验证）",
				AuthOK:    true,
			}
		}
		if chatErr != nil {
			return ProbeResult{Status: status, LatencyMs: latency, Message: "请求失败: " + chatErr.Error()}
		}
		return ProbeResult{
			Status:    status,
			LatencyMs: latency,
			Message:   describeProbeFailure(status, body),
			AuthOK:    status != http.StatusUnauthorized && status != http.StatusForbidden,
		}
	}

	latency := time.Since(start).Milliseconds()
	if listErr != nil {
		return ProbeResult{LatencyMs: latency, Message: "请求失败: " + listErr.Error()}
	}
	return ProbeResult{
		Status:    listStatus,
		LatencyMs: latency,
		Message:   "上游未提供模型列表接口（HTTP " + fmt.Sprint(listStatus) + "），请填写模型名后再测试",
		AuthOK:    listStatus != http.StatusUnauthorized && listStatus != http.StatusForbidden,
	}
}

// fetchModels 拉取上游模型列表，返回模型名与 HTTP 状态。
func (s *Server) fetchModels(ctx context.Context, client *http.Client, prov model.Provider) ([]string, int, error) {
	target, err := buildUpstreamURL(prov, modelsPath(prov.APIFormat), "")
	if err != nil {
		return nil, 0, err
	}

	req, err := buildRequest(ctx, http.MethodGet, target, nil, probeHeaders(prov))
	if err != nil {
		return nil, 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, resp.StatusCode, nil
	}
	return parseModelList(body), resp.StatusCode, nil
}

// ListUpstreamModels 供管理界面的「拉取模型列表」使用。
func (s *Server) ListUpstreamModels(ctx context.Context, prov model.Provider, settings model.Settings) ([]string, error) {
	netcfg := resolveNet(prov, settings)
	client, err := s.clients.client(netcfg)
	if err != nil {
		return nil, fmt.Errorf("初始化连接失败: %w", err)
	}
	models, status, err := s.fetchModels(ctx, client, prov)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("上游返回 HTTP %d", status)
	}
	return models, nil
}

// probeChat 发一个消耗最小的对话请求。
func (s *Server) probeChat(ctx context.Context, client *http.Client, prov model.Provider, probeModel string) (int, []byte, error) {
	path, body := chatProbe(prov.APIFormat, probeModel)
	target, err := buildUpstreamURL(prov, path, "")
	if err != nil {
		return 0, nil, err
	}

	headers := probeHeaders(prov)
	headers.Set("Content-Type", "application/json")

	req, err := buildRequest(ctx, http.MethodPost, target, body, headers)
	if err != nil {
		return 0, nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return resp.StatusCode, respBody, nil
}

// probeHeaders 构造探测请求的头，与真实转发走同一套认证逻辑。
func probeHeaders(prov model.Provider) http.Header {
	h := http.Header{}
	header, prefix := prov.AuthHeaderFor()
	if key, ok := firstProviderKey(prov); ok {
		h.Set(header, prefix+key)
	}
	for name, value := range prov.ExtraHeaders {
		if name != "" {
			h.Set(name, value)
		}
	}
	h.Set("Accept", "application/json")
	return h
}

func firstProviderKey(prov model.Provider) (string, bool) {
	for _, k := range prov.Keys {
		if k.Enabled && k.Key != "" {
			return k.Key, true
		}
	}
	return "", false
}

// modelsPath 返回该格式下模型列表的请求路径。
func modelsPath(format model.APIFormat) string {
	if format == model.FormatGemini {
		return "/v1beta/models"
	}
	return "/v1/models"
}

// chatProbe 返回该格式下最小对话请求的路径与请求体。
func chatProbe(format model.APIFormat, probeModel string) (string, []byte) {
	switch format {
	case model.FormatAnthropic:
		body, _ := json.Marshal(map[string]any{
			"model":      probeModel,
			"max_tokens": 1,
			"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		})
		return "/v1/messages", body

	case model.FormatGemini:
		body, _ := json.Marshal(map[string]any{
			"contents": []any{map[string]any{
				"role":  "user",
				"parts": []any{map[string]any{"text": "hi"}},
			}},
			"generationConfig": map[string]any{"maxOutputTokens": 1},
		})
		return "/v1beta/models/" + probeModel + ":generateContent", body

	case model.FormatResponses:
		body, _ := json.Marshal(map[string]any{
			"model":             probeModel,
			"input":             "hi",
			"max_output_tokens": 16,
		})
		return "/v1/responses", body

	default:
		body, _ := json.Marshal(map[string]any{
			"model":      probeModel,
			"max_tokens": 1,
			"messages":   []any{map[string]any{"role": "user", "content": "hi"}},
		})
		return "/v1/chat/completions", body
	}
}

// parseModelList 兼容 OpenAI 的 data[].id 与 Gemini 的 models[].name 两种结构，
// 也接受裸数组，尽量把能认出来的模型名都捞出来。
func parseModelList(body []byte) []string {
	var root any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}

	set := map[string]struct{}{}
	var walk func(node any)
	walk = func(node any) {
		switch t := node.(type) {
		case []any:
			for _, item := range t {
				walk(item)
			}
		case map[string]any:
			if s, ok := t["id"].(string); ok && s != "" {
				set[s] = struct{}{}
			}
			if s, ok := t["name"].(string); ok && s != "" {
				// Gemini 的 name 形如 models/gemini-pro
				set[strings.TrimPrefix(s, "models/")] = struct{}{}
			}
			if s, ok := t["model"].(string); ok && s != "" {
				set[s] = struct{}{}
			}
			for _, key := range []string{"data", "models", "items"} {
				if child, ok := t[key]; ok {
					walk(child)
				}
			}
		}
	}
	walk(root)

	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// describeProbeFailure 把上游的失败响应译成一句能指导操作的话。
func describeProbeFailure(status int, body []byte) string {
	snippet := strings.TrimSpace(string(body))
	if len(snippet) > 300 {
		snippet = snippet[:300] + "…"
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Sprintf("鉴权失败（HTTP %d），请检查密钥与认证头设置：%s", status, snippet)
	case http.StatusNotFound:
		return fmt.Sprintf("接口不存在（HTTP 404），请检查 BaseURL 与 API 格式：%s", snippet)
	case http.StatusTooManyRequests:
		return fmt.Sprintf("被限流（HTTP 429）：%s", snippet)
	default:
		return fmt.Sprintf("上游返回 HTTP %d：%s", status, snippet)
	}
}
