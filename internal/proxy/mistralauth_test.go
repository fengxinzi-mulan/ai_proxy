package proxy

// Mistral 自动登录的用例。
//
// 登录是 Ory Kratos 的两阶段流程，其中「提交邮箱返回 HTTP 400」是协议本身的
// 用法（400 里携带下一阶段要填的字段），不是失败。这条最容易被后来的改动
// 当成 bug「修掉」，所以单独盯住。

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// mistralAuthMock 模拟 Ory 登录服务。
type mistralAuthMock struct {
	// 期望的账号密码
	account, password string
	// 第一阶段返回的错误消息（模拟账号不存在）
	identifierError string
	// 密码阶段返回的错误消息（模拟密码错误）
	passwordError string
	// session 过期时间
	expiresAt string
	// csrfToken 为空时模拟 api 流程（不签发 CSRF），非空时模拟浏览器流程
	csrfToken string

	flowCalls     atomic.Int32
	passwordCalls atomic.Int32
}

func (m *mistralAuthMock) start(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == mistralLoginAPIPath:
			m.flowCalls.Add(1)
			writeJSON(w, map[string]any{"id": "flow-1", "type": "api"})

		case r.Method == http.MethodPost && r.URL.Path == mistralLoginPath:
			var body map[string]any
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &body)

			if body["method"] == "identifier_first" {
				nodes := []any{
					nodeWith("identifier", m.account),
				}
				if m.identifierError != "" {
					nodes = append(nodes, nodeWithMessage("identifier", m.identifierError))
				} else {
					// 关键：进入下一阶段用的是 400，不是 200。
					// csrfToken 为空时照抄 api 流程的真实形态（value 是空串）。
					nodes = append(nodes,
						nodeWith("csrf_token", m.csrfToken),
						nodeWith("password", ""))
				}
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]any{
					"state": "choose_method",
					"ui":    map[string]any{"nodes": nodes},
				})
				return
			}

			// 密码阶段
			m.passwordCalls.Add(1)
			if m.passwordError != "" || body["password"] != m.password {
				msg := m.passwordError
				if msg == "" {
					msg = "The provided credentials are invalid."
				}
				w.WriteHeader(http.StatusBadRequest)
				writeJSON(w, map[string]any{
					"state": "choose_method",
					"ui": map[string]any{"nodes": []any{
						nodeWithMessage("password", msg),
					}},
				})
				return
			}
			writeJSON(w, map[string]any{
				"session_token": "ory_st_test123",
				"session":       map[string]any{"expires_at": m.expiresAt},
			})

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)

	// 登录地址写死在生产代码里（auth.mistral.ai），测试里换成 mock 的地址。
	prev := mistralAuthBase
	mistralAuthBase = srv.URL
	t.Cleanup(func() { mistralAuthBase = prev })
	return srv
}

func nodeWith(name, value string) map[string]any {
	return map[string]any{
		"attributes": map[string]any{"name": name, "value": value},
		"messages":   []any{},
	}
}

func nodeWithMessage(name, text string) map[string]any {
	return map[string]any{
		"attributes": map[string]any{"name": name, "value": ""},
		"messages":   []any{map[string]any{"type": "error", "text": text}},
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// 两阶段登录：提交邮箱返回 400 时必须继续走密码阶段，而不是当失败返回。
func TestMistralLoginTwoStage(t *testing.T) {
	mock := &mistralAuthMock{
		account:   "user@example.com",
		password:  "secret",
		expiresAt: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}
	srv := mock.start(t)

	tok, err := mistralLogin(context.Background(), srv.Client(), mock.account, mock.password)
	if err != nil {
		t.Fatalf("登录应成功，实际报错: %v", err)
	}
	if tok.token != "ory_st_test123" {
		t.Errorf("session_token 应为 ory_st_test123，实际 %q", tok.token)
	}
	if !tok.expires.After(time.Now()) {
		t.Errorf("过期时间应在将来，实际 %v", tok.expires)
	}
	// 过期时间要留出提前量，不能等于上游给的时刻。
	if got := time.Until(tok.expires); got > 24*time.Hour {
		t.Errorf("过期时间应减去提前量，实际还剩 %v", got)
	}
	if mock.passwordCalls.Load() != 1 {
		t.Errorf("密码阶段应被调用 1 次，实际 %d", mock.passwordCalls.Load())
	}
}

// 密码错误要给出能看懂的提示，而不是把上游原文直接抛出去。
func TestMistralLoginWrongPassword(t *testing.T) {
	mock := &mistralAuthMock{account: "user@example.com", password: "right"}
	srv := mock.start(t)

	_, err := mistralLogin(context.Background(), srv.Client(), mock.account, "wrong")
	if err == nil {
		t.Fatal("密码错误时应返回错误")
	}
	if !strings.Contains(err.Error(), "账号或密码错误") {
		t.Errorf("错误信息应说明账号或密码错误，实际 %q", err.Error())
	}
}

// 账号不存在时 Ory 在第一阶段就挂错误消息，不能继续往下走。
func TestMistralLoginUnknownAccount(t *testing.T) {
	mock := &mistralAuthMock{
		account:         "nobody@example.com",
		password:        "x",
		identifierError: "This account does not exist or has no login method configured.",
	}
	srv := mock.start(t)

	_, err := mistralLogin(context.Background(), srv.Client(), mock.account, mock.password)
	if err == nil {
		t.Fatal("账号不存在时应返回错误")
	}
	if !strings.Contains(err.Error(), "账号不存在") {
		t.Errorf("错误信息应说明账号不存在，实际 %q", err.Error())
	}
	if mock.passwordCalls.Load() != 0 {
		t.Errorf("账号不存在时不该进入密码阶段，实际调用 %d 次", mock.passwordCalls.Load())
	}
}

// token 要缓存：定时刷新场景下每次都重登会被上游限流。
func TestMistralSessionCachesToken(t *testing.T) {
	mock := &mistralAuthMock{
		account:   "user@example.com",
		password:  "secret",
		expiresAt: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}
	srv := mock.start(t)

	sess := newMistralSession()
	for i := 0; i < 3; i++ {
		tok, err := sess.Token(context.Background(), srv.Client(), mock.account, mock.password, false)
		if err != nil {
			t.Fatalf("第 %d 次取 token 失败: %v", i+1, err)
		}
		if tok != "ory_st_test123" {
			t.Fatalf("token 不对: %q", tok)
		}
	}
	if got := mock.flowCalls.Load(); got != 1 {
		t.Errorf("三次取 token 只应登录一次，实际登录 %d 次", got)
	}
}

// forceRefresh 必须绕过缓存重新登录（调用方收到 401 时用）。
func TestMistralSessionForceRefresh(t *testing.T) {
	mock := &mistralAuthMock{
		account:   "user@example.com",
		password:  "secret",
		expiresAt: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}
	srv := mock.start(t)

	sess := newMistralSession()
	if _, err := sess.Token(context.Background(), srv.Client(), mock.account, mock.password, false); err != nil {
		t.Fatalf("首次登录失败: %v", err)
	}
	if _, err := sess.Token(context.Background(), srv.Client(), mock.account, mock.password, true); err != nil {
		t.Fatalf("强制刷新失败: %v", err)
	}
	if got := mock.flowCalls.Load(); got != 2 {
		t.Errorf("强制刷新应重新登录，期望 2 次实际 %d 次", got)
	}
}

// 过期时间已过的 token 不能再用。
func TestMistralSessionExpiredTokenRelogin(t *testing.T) {
	mock := &mistralAuthMock{
		account:  "user@example.com",
		password: "secret",
		// 已经过期
		expiresAt: time.Now().Add(-time.Hour).UTC().Format(time.RFC3339),
	}
	srv := mock.start(t)

	sess := newMistralSession()
	if _, err := sess.Token(context.Background(), srv.Client(), mock.account, mock.password, false); err != nil {
		t.Fatalf("首次登录失败: %v", err)
	}
	if _, err := sess.Token(context.Background(), srv.Client(), mock.account, mock.password, false); err != nil {
		t.Fatalf("第二次取 token 失败: %v", err)
	}
	// 第一次登录拿到的 token 已过期，第二次必须重登。
	if got := mock.flowCalls.Load(); got != 2 {
		t.Errorf("token 过期后应重新登录，期望 2 次实际 %d 次", got)
	}
}

// 缺账号或密码时不该发请求。
func TestMistralSessionRequiresCredentials(t *testing.T) {
	sess := newMistralSession()
	if _, err := sess.Token(context.Background(), http.DefaultClient, "", "pass", false); err == nil {
		t.Error("缺账号时应报错")
	}
	if _, err := sess.Token(context.Background(), http.DefaultClient, "user@example.com", "", false); err == nil {
		t.Error("缺密码时应报错")
	}
}

// 上游没给 expires_at 时要有兜底有效期，不能当成「永不过期」。
func TestMistralLoginFallbackExpiry(t *testing.T) {
	mock := &mistralAuthMock{
		account:  "user@example.com",
		password: "secret",
		// 不给 expiresAt
	}
	srv := mock.start(t)

	tok, err := mistralLogin(context.Background(), srv.Client(), mock.account, mock.password)
	if err != nil {
		t.Fatalf("登录应成功: %v", err)
	}
	if tok.expires.IsZero() {
		t.Error("没有 expires_at 时也应给出兜底过期时间")
	}
	if !tok.expires.After(time.Now()) {
		t.Errorf("兜底过期时间应在将来，实际 %v", tok.expires)
	}
}

// Ory 的 api 流程不签发 CSRF token（节点 value 是空串），浏览器流程才有真值。
//
// 2026-11-15 实测踩到：把空串当 csrf_token 提交上去会被拒。这里盯住
// 「csrf 为空时也能登录」，以及「有 csrf 时确实带上」两种形态。
func TestMistralLoginWithoutCSRF(t *testing.T) {
	mock := &mistralAuthMock{
		account:   "user@example.com",
		password:  "secret",
		expiresAt: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		// csrfToken 留空 = 模拟 api 流程
	}
	srv := mock.start(t)

	tok, err := mistralLogin(context.Background(), srv.Client(), mock.account, mock.password)
	if err != nil {
		t.Fatalf("api 流程（无 CSRF）应能登录，实际报错: %v", err)
	}
	if tok.token != "ory_st_test123" {
		t.Errorf("session_token 不对: %q", tok.token)
	}
}

// 浏览器流程会签发 CSRF，有值时必须带上。
func TestMistralLoginWithCSRF(t *testing.T) {
	mock := &mistralAuthMock{
		account:   "user@example.com",
		password:  "secret",
		expiresAt: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
		csrfToken: "csrf-abc",
	}
	srv := mock.start(t)

	if _, err := mistralLogin(context.Background(), srv.Client(), mock.account, mock.password); err != nil {
		t.Fatalf("浏览器流程（有 CSRF）应能登录，实际报错: %v", err)
	}
}

// 缓存键必须带上密码：同一账号改了密码之后，按账号缓存的那份旧 session
// 会被上游判失效，而缓存还没过期，于是每次查询都撞 401。
func TestMistralSessionCacheKeyIncludesPassword(t *testing.T) {
	mock := &mistralAuthMock{
		account:   "user@example.com",
		password:  "secret",
		expiresAt: time.Now().Add(24 * time.Hour).UTC().Format(time.RFC3339),
	}
	srv := mock.start(t)

	sess := newMistralSession()
	for i := 0; i < 2; i++ {
		if _, err := sess.Token(context.Background(), srv.Client(), mock.account, mock.password, false); err != nil {
			t.Fatalf("第 %d 次登录失败: %v", i+1, err)
		}
	}
	if got := mock.flowCalls.Load(); got != 1 {
		t.Fatalf("相同凭据应命中缓存，实际登录 %d 次", got)
	}

	// 换密码 → 换缓存项 → 必须重新登录（这里故意用错密码，登录会失败）。
	if _, err := sess.Token(context.Background(), srv.Client(), mock.account, "another-password", false); err == nil {
		t.Fatal("密码不对时应报错")
	}
	if got := mock.flowCalls.Load(); got != 2 {
		t.Errorf("换密码后应重新登录，期望 2 次实际 %d 次", got)
	}

	// 失败的那次不该清掉原密码的缓存。
	if _, err := sess.Token(context.Background(), srv.Client(), mock.account, mock.password, false); err != nil {
		t.Fatalf("原密码的缓存不该被清掉: %v", err)
	}
	if got := mock.flowCalls.Load(); got != 2 {
		t.Errorf("原密码应仍在缓存里，实际登录 %d 次", got)
	}
}

// Ory 的节点值不全是字符串（复选框是 bool，数字输入是 number）。
// 给成 string 会让整份响应用 Unmarshal 直接失败。
func TestParseMistralFlowNonStringValues(t *testing.T) {
	body := []byte(`{"state":"choose_method","ui":{
		"nodes":[
			{"attributes":{"name":"csrf_token","value":null},"messages":[]},
			{"attributes":{"name":"remember_me","value":true},"messages":[]},
			{"attributes":{"name":"captcha_code","value":1234},"messages":[]},
			{"attributes":{"name":"identifier","value":"user@example.com"},"messages":[]}
		],
		"messages":[{"type":"error","text":"Too many login attempts, please retry later."}]
	}}`)

	flow, err := parseMistralFlow(body)
	if err != nil {
		t.Fatalf("节点值不是字符串时也应能解析: %v", err)
	}
	if got := flow.nodeValue("identifier"); got != "user@example.com" {
		t.Errorf("字符串值应原样返回，实际 %q", got)
	}
	if got := flow.nodeValue("remember_me"); got != "true" {
		t.Errorf("bool 值应转成可读文本，实际 %q", got)
	}
	if got := flow.nodeValue("captcha_code"); got != "1234" {
		t.Errorf("数字值应转成可读文本，实际 %q", got)
	}
	if got := flow.nodeValue("csrf_token"); got != "" {
		t.Errorf("null 值应返回空串，实际 %q", got)
	}

	// ui.messages 是流程级错误（限流、会话失效），只看 ui.nodes 会漏掉它。
	if got := flow.firstError(); !strings.Contains(got, "Too many login attempts") {
		t.Errorf("应扫出顶层 ui.messages 里的错误，实际 %q", got)
	}
}

// 节点上的错误优先于流程级消息：字段级提示对用户更具体。
func TestParseMistralFlowNodeErrorWins(t *testing.T) {
	body := []byte(`{"ui":{
		"nodes":[{"attributes":{"name":"password","value":""},
			"messages":[{"type":"error","text":"The provided credentials are invalid."}]}],
		"messages":[{"type":"error","text":"flow level error"}]
	}}`)
	flow, err := parseMistralFlow(body)
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got := flow.firstError(); !strings.Contains(got, "credentials") {
		t.Errorf("应优先返回节点上的错误，实际 %q", got)
	}
}
