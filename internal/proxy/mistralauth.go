package proxy

// Mistral 网页后台的自动登录。
//
// 背景：Mistral 的订阅额度（PRO 计划的 API / Vibe 预算）只挂在网页后台
// admin.mistral.ai 上，普通 API key 访问 Admin 端点会被直接拒绝
// （2026-11-15 实测 /v1/admin/usage 返回 401 Invalid API Key，官方文档也写明
// 「A user's standard API key never grants admin access」）。
//
// 所以只能拿账号密码去换网页会话。后台用 Ory Kratos 做认证，登录是两阶段：
//
//	1. GET  /self-service/login/api             → 拿 flow.id
//	2. POST /self-service/login?flow=<id>       → 提交 identifier（邮箱）
//	3. POST /self-service/login?flow=<id>       → 提交 password + csrf_token
//
// 第 2 步返回 **HTTP 400** 是正常的：Ory 用 400 携带「下一阶段要填什么」，
// 里面才有 password 节点与 csrf_token。把 400 当失败就永远登不进去。
//
// 第 3 步成功返回 session_token（ory_st_xxx），拿它当 Bearer token 就能访问
// admin.mistral.ai 的所有接口 —— 不需要手工粘贴会过期的 Cookie。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"ai_proxy/internal/model"
)

const (
	// mistralAuthOrigin 是 Ory 认证服务的站点根。它与生成接口的 api.mistral.ai、
	// 后台的 admin.mistral.ai 都是不同域，必须写死。
	mistralAuthOrigin = "https://auth.mistral.ai"
	// mistralAdminOrigin 是网页后台的站点根，额度与用量接口都挂在这里。
	mistralAdminOriginConst = "https://admin.mistral.ai"

	mistralLoginAPIPath = "/self-service/login/api"
	mistralLoginPath    = "/self-service/login"

	// token 提前量：快过期时主动重登，免得请求打到一半失效。
	mistralTokenRefreshMargin = 5 * time.Minute
	// 上游没给 expires_at 时的兜底有效期。实测 Ory 的 session 是 24 小时。
	mistralTokenFallbackTTL = 12 * time.Hour
)

// mistralAuthBase 是登录请求的站点根，测试里可以替换成 httptest 的地址。
// 生产路径上它恒等于 mistralAuthOrigin。
var mistralAuthBase = mistralAuthOrigin

// mistralToken 是一份缓存下来的登录会话。
type mistralToken struct {
	token   string
	expires time.Time
}

// mistralSession 用账号密码登录 admin.mistral.ai，并按账号缓存 session_token。
//
// 缓存的意义不只是省一次登录：Ory 对登录有速率限制，定时刷新的场景下
// 每次都重登很快就会被拦。
type mistralSession struct {
	mu      sync.Mutex
	entries map[string]mistralToken
}

func newMistralSession() *mistralSession {
	return &mistralSession{entries: map[string]mistralToken{}}
}

// mistralSessionCacheKey 是会话缓存的键：账号 + 密码摘要。
//
// 为什么不能只用账号：用户改了密码之后，按账号缓存的那份旧 session 会被上游
// 判失效，而缓存里的过期时间还没到，于是每次查询都拿着旧 token 撞 401，
// 要等十几个小时缓存自然过期才恢复。带上密码摘要等于「换密码即换缓存项」。
// 摘要而不是明文：缓存键会出现在内存 dump 与日志排查里，没必要带明文口令。
func mistralSessionCacheKey(account, password string) string {
	sum := sha256.Sum256([]byte(password))
	return account + "\x00" + hex.EncodeToString(sum[:])
}

// Token 返回该账号可用的 session_token，必要时先登录。
//
// forceRefresh 为 true 时忽略缓存强制重登（调用方收到 401 时用）。
func (s *mistralSession) Token(
	ctx context.Context, client *http.Client, account, password string, forceRefresh bool,
) (string, error) {
	account = strings.TrimSpace(account)
	if account == "" {
		return "", errors.New("未填写 Mistral 账号")
	}
	if password == "" {
		return "", errors.New("未填写 Mistral 密码")
	}
	key := mistralSessionCacheKey(account, password)

	s.mu.Lock()
	defer s.mu.Unlock()

	// 顺手清掉过期条目：缓存键带密码摘要，用户反复改密码时旧键会一直留着。
	now := time.Now()
	for k, v := range s.entries {
		if v.token == "" || !now.Before(v.expires) {
			delete(s.entries, k)
		}
	}

	if !forceRefresh {
		if t, ok := s.entries[key]; ok && t.token != "" {
			return t.token, nil
		}
	}

	tok, err := mistralLogin(ctx, client, account, password)
	if err != nil {
		// 登录失败就把旧缓存丢掉：留着只会在下次静默返回一个已失效的 token。
		delete(s.entries, key)
		return "", err
	}
	s.entries[key] = tok
	return tok.token, nil
}

// mistralLogin 走完 Ory 的两阶段登录，返回 session_token 与过期时间。
func mistralLogin(ctx context.Context, client *http.Client, account, password string) (mistralToken, error) {
	flowID, err := mistralStartFlow(ctx, client)
	if err != nil {
		return mistralToken{}, err
	}

	csrf, err := mistralSubmitIdentifier(ctx, client, flowID, account)
	if err != nil {
		return mistralToken{}, err
	}

	return mistralSubmitPassword(ctx, client, flowID, account, password, csrf)
}

// mistralStartFlow 创建登录流程，返回 flow id。
func mistralStartFlow(ctx context.Context, client *http.Client) (string, error) {
	req, err := buildRequest(ctx, http.MethodGet, mistralAuthBase+mistralLoginAPIPath, nil,
		http.Header{"Accept": []string{"application/json"}})
	if err != nil {
		return "", err
	}
	status, body, err := doMistralRequest(client, req)
	if err != nil {
		return "", fmt.Errorf("无法连接 Mistral 登录服务: %w", err)
	}
	if status < 200 || status >= 300 {
		return "", fmt.Errorf("Mistral 登录服务不可用（HTTP %d）", status)
	}

	var flow struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &flow); err != nil || flow.ID == "" {
		return "", errors.New("Mistral 登录流程结构无法识别（没有 flow id）")
	}
	return flow.ID, nil
}

// mistralSubmitIdentifier 提交邮箱，返回下一阶段要用的 csrf_token。
//
// 注意：Ory 对「进入下一阶段」返回 HTTP 400，这是协议本身的用法，不是失败。
// 真正的失败要看节点上的 messages。
func mistralSubmitIdentifier(ctx context.Context, client *http.Client, flowID, account string) (string, error) {
	payload, err := json.Marshal(map[string]any{
		"identifier": account,
		"method":     "identifier_first",
	})
	if err != nil {
		return "", err
	}

	status, body, err := doMistralLogin(ctx, client, flowID, payload)
	if err != nil {
		return "", fmt.Errorf("无法连接 Mistral 登录服务: %w", err)
	}

	flow, parseErr := parseMistralFlow(body)
	if parseErr != nil {
		return "", parseErr
	}

	// 账号不存在时，Ory 在 identifier 节点上挂一条 error 消息，状态仍是 400。
	if msg := flow.firstError(); msg != "" {
		if strings.Contains(strings.ToLower(msg), "does not exist") {
			return "", errors.New("Mistral 账号不存在，或该账号未配置密码登录方式")
		}
		return "", fmt.Errorf("Mistral 登录被拒绝：%s", msg)
	}
	if status >= 200 && status < 300 {
		// 少数配置下第一阶段就会直接给出会话（例如已通过其它方式认证）。
		return "", errors.New("Mistral 登录流程异常：提交邮箱后未进入密码阶段")
	}

	if !flow.hasNode("password") {
		return "", errors.New("Mistral 登录流程异常：没有出现密码输入阶段")
	}
	// csrf_token 在 api 流程里是空串（浏览器流程才有真值），空着就空着，
	// 调用方不会把它提交上去 —— 见 mistralSubmitPassword。
	return flow.nodeValue("csrf_token"), nil
}

// mistralSubmitPassword 提交密码，成功后返回 session_token。
//
// csrf 可能为空：Ory 的 api 流程不签发 CSRF token（浏览器流程才有），
// 空值时不能把空串提交上去，否则会被当成「CSRF 校验失败」。
func mistralSubmitPassword(
	ctx context.Context, client *http.Client, flowID, account, password, csrf string,
) (mistralToken, error) {
	fields := map[string]any{
		"identifier": account,
		"password":   password,
		"method":     "password",
	}
	if csrf != "" {
		fields["csrf_token"] = csrf
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		return mistralToken{}, err
	}

	status, body, err := doMistralLogin(ctx, client, flowID, payload)
	if err != nil {
		return mistralToken{}, fmt.Errorf("无法连接 Mistral 登录服务: %w", err)
	}

	if status < 200 || status >= 300 {
		// 密码错误时 Ory 返回 400 并在节点上挂消息，解析出来给用户看。
		if flow, parseErr := parseMistralFlow(body); parseErr == nil {
			if msg := flow.firstError(); msg != "" {
				if isMistralCredentialError(msg) {
					return mistralToken{}, errors.New("Mistral 账号或密码错误")
				}
				return mistralToken{}, fmt.Errorf("Mistral 登录失败：%s", msg)
			}
		}
		return mistralToken{}, fmt.Errorf("Mistral 登录失败（HTTP %d）", status)
	}

	var resp struct {
		SessionToken string `json:"session_token"`
		Session      *struct {
			ExpiresAt string `json:"expires_at"`
		} `json:"session"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return mistralToken{}, errors.New("Mistral 登录响应结构无法识别")
	}
	if resp.SessionToken == "" {
		return mistralToken{}, errors.New("Mistral 登录成功但未返回 session_token")
	}

	expires := time.Now().Add(mistralTokenFallbackTTL)
	if resp.Session != nil {
		if t, ok := parseMistralTime(resp.Session.ExpiresAt); ok {
			expires = t.Add(-mistralTokenRefreshMargin)
		}
	}
	return mistralToken{token: resp.SessionToken, expires: expires}, nil
}

// doMistralLogin 向登录端点 POST 一份 JSON，返回状态码与响应体。
func doMistralLogin(ctx context.Context, client *http.Client, flowID string, payload []byte) (int, []byte, error) {
	headers := http.Header{
		"Accept":       []string{"application/json"},
		"Content-Type": []string{"application/json"},
	}
	target := mistralAuthBase + mistralLoginPath + "?flow=" + flowID
	req, err := buildRequest(ctx, http.MethodPost, target, payload, headers)
	if err != nil {
		return 0, nil, err
	}
	return doMistralRequest(client, req)
}

// doMistralRequest 发出请求并读回响应体。
func doMistralRequest(client *http.Client, req *http.Request) (int, []byte, error) {
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, body, nil
}

// mistralFlow 是 Ory 登录流程响应里我们关心的部分。
//
// Attributes.Value 用 any 而不是 string：Ory 的节点值不全是字符串（复选框是
// bool，数字输入是 number），给成 string 会让整份响应用 Unmarshal 直接失败，
// 于是「密码阶段拿不到 csrf」被误报成「登录响应不是合法 JSON」。
type mistralFlow struct {
	State string `json:"state"`
	UI    struct {
		Nodes    []mistralFlowNode    `json:"nodes"`
		Messages []mistralFlowMessage `json:"messages"`
	} `json:"ui"`
}

// mistralFlowNode 是一个输入节点。
type mistralFlowNode struct {
	Attributes struct {
		Name  string `json:"name"`
		Value any    `json:"value"`
	} `json:"attributes"`
	Messages []mistralFlowMessage `json:"messages"`
}

// mistralFlowMessage 是节点或流程级的一条提示（type=error 才是错误）。
type mistralFlowMessage struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func parseMistralFlow(body []byte) (mistralFlow, error) {
	var flow mistralFlow
	if err := json.Unmarshal(bytes.TrimSpace(body), &flow); err != nil {
		return flow, errors.New("Mistral 登录响应不是合法的 JSON")
	}
	return flow, nil
}

// hasNode 报告流程里是否有某个输入节点。
func (f mistralFlow) hasNode(name string) bool {
	for _, n := range f.UI.Nodes {
		if n.Attributes.Name == name {
			return true
		}
	}
	return false
}

// nodeValue 取某个节点的值（csrf_token 这类 hidden 字段用）。
func (f mistralFlow) nodeValue(name string) string {
	for _, n := range f.UI.Nodes {
		if n.Attributes.Name != name {
			continue
		}
		switch v := n.Attributes.Value.(type) {
		case string:
			return v
		case nil:
			return ""
		default:
			// 少数节点是 bool / number（如复选框），按可读文本处理即可：
			// 这里只取给上游回填的值，格式由 Ory 自己校验。
			return fmt.Sprint(v)
		}
	}
	return ""
}

// firstError 返回流程里的第一条错误消息。
//
// 两处都要扫：节点上的消息是「这个字段填错了」，ui.messages 是流程级错误
// （限流、会话失效等）。只看节点会把这类整体性失败当成「登录响应异常」，
// 用户拿到的提示就完全指错了方向。
func (f mistralFlow) firstError() string {
	for _, n := range f.UI.Nodes {
		if msg := firstErrorMessage(n.Messages); msg != "" {
			return msg
		}
	}
	return firstErrorMessage(f.UI.Messages)
}

// firstErrorMessage 从一组消息里取出第一条 type=error 的非空文本。
func firstErrorMessage(messages []mistralFlowMessage) string {
	for _, m := range messages {
		if strings.EqualFold(m.Type, "error") && strings.TrimSpace(m.Text) != "" {
			return strings.TrimSpace(m.Text)
		}
	}
	return ""
}

// isMistralCredentialError 判断错误消息是不是「密码不对」这一类。
func isMistralCredentialError(msg string) bool {
	lower := strings.ToLower(msg)
	for _, kw := range []string{
		"password", "credentials", "invalid", "incorrect", "wrong",
	} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// parseMistralTime 解析上游给的 RFC3339 时间，失败时返回 false。
func parseMistralTime(raw string) (time.Time, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, raw); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// mistralCredentials 从供应商配置里取账号密码。
func mistralCredentials(prov model.Provider) (account, password string) {
	return strings.TrimSpace(prov.UsageQuery.Account), prov.UsageQuery.Password
}
