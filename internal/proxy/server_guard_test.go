package proxy

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"ai_proxy/internal/model"
)

// 访问密钥校验要覆盖各家原生认证头：Anthropic 客户端只发 x-api-key，
// Gemini 客户端只发 x-goog-api-key，两者都不会带 Authorization。
func TestAuthorizedAcceptsNativeAuthHeaders(t *testing.T) {
	srv, _ := newTestServer(t)
	st := model.DefaultSettings()
	st.AccessKey = "proxy-secret"

	cases := []struct {
		name    string
		headers map[string]string
		want    bool
	}{
		{"OpenAI Bearer", map[string]string{"Authorization": "Bearer proxy-secret"}, true},
		{"Anthropic x-api-key", map[string]string{"x-api-key": "proxy-secret"}, true},
		{"Gemini x-goog-api-key", map[string]string{"x-goog-api-key": "proxy-secret"}, true},
		{"代理自定义头", map[string]string{"X-AI-Proxy-Key": "proxy-secret"}, true},
		{"错误密钥", map[string]string{"Authorization": "Bearer nope"}, false},
		{"Anthropic 头错误", map[string]string{"x-api-key": "nope"}, false},
		{"完全没有认证头", map[string]string{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := newTestRequest(t, "/v1/chat/completions", tc.headers)
			if got := srv.authorized(r, st); got != tc.want {
				t.Errorf("authorized() = %v，期望 %v", got, tc.want)
			}
		})
	}
}

// 抹除代理访问密钥必须按「整值相等」判断。旧实现用 strings.Contains，
// 访问密钥短一点就会把 User-Agent、业务头里恰好含该子串的值一并删掉。
func TestBuildUpstreamHeadersKeepsUnrelatedSubstrings(t *testing.T) {
	const accessKey = "ai"
	p := baseProvider()
	p.Keys = nil // 客户端持钥模式：认证头不会被覆盖，才会走抹除逻辑

	r := newTestRequest(t, "/v1/chat/completions", map[string]string{
		"User-Agent":     "my-ai-client/1.0",
		"X-Trace":        "trace-ai-42",
		"X-AI-Proxy-Key": accessKey,
	})
	got := buildUpstreamHeaders(r, p, model.APIKey{}, accessKey)
	if v := got.Get("X-AI-Proxy-Key"); v != "" {
		t.Errorf("代理访问密钥头必须始终剔除，实际 %q", v)
	}
	if v := got.Get("User-Agent"); v != "my-ai-client/1.0" {
		t.Errorf("User-Agent 只是含子串，不该被删，实际 %q", v)
	}
	if v := got.Get("X-Trace"); v != "trace-ai-42" {
		t.Errorf("业务头只是含子串，不该被删，实际 %q", v)
	}
}

// 代理访问密钥可能被客户端塞进 URL 查询串（Gemini SDK 的 ?key= 写法）。
// 不删就会连 URL 一起转发给上游，并原样落进请求日志。
func TestStripProxyKeyFromURL(t *testing.T) {
	const key = "proxy-secret"
	cases := []struct {
		name    string
		raw     string
		passKey string
		want    string
	}{
		{"只有代理密钥", "https://api.example.com/v1/models?key=" + key, key, "https://api.example.com/v1/models"},
		{"保留上游自己的参数", "https://api.example.com/v1/models?key=" + key + "&alt=sse", key, "https://api.example.com/v1/models?alt=sse"},
		{"其它 key 不动", "https://api.example.com/v1/models?key=upstream-key", key, "https://api.example.com/v1/models?key=upstream-key"},
		{"没有密钥配置时原样返回", "https://api.example.com/v1/models?key=" + key, "", "https://api.example.com/v1/models?key=" + key},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stripProxyKeyFromURL(c.raw, c.passKey); got != c.want {
				t.Errorf("stripProxyKeyFromURL(%q) = %q，期望 %q", c.raw, got, c.want)
			}
		})
	}
}

// 认证方案名大小写不敏感（RFC 7235），常量时间比较只用于 token 本身。
func TestIsBearerOfCaseInsensitive(t *testing.T) {
	if !isBearerOf("bearer secret", "secret") {
		t.Error("小写 bearer 也应被识别")
	}
	if !isBearerOf("Bearer secret", "secret") {
		t.Error("标准写法应被识别")
	}
	if isBearerOf("Bearer other", "secret") {
		t.Error("token 不匹配时不该通过")
	}
	if isBearerOf("secret", "secret") {
		t.Error("缺少 Bearer 前缀不该通过（裸值由单独的精确匹配负责）")
	}
}

// 旧实现为了剥离 usage 块连 Content-Encoding 一起删，客户端拿到的就是
// 「声明为明文、实际是 gzip」的乱码。
func TestForwardCompressedResponseKeepsEncoding(t *testing.T) {
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	if _, err := io.WriteString(zw, `{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`); err != nil {
		t.Fatalf("构造压缩响应失败: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("关闭 gzip 失败: %v", err)
	}
	payload := gz.Bytes()

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(payload)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	p := addProvider(t, st, upstream.URL, model.FormatOpenAI)
	p.StripUsageChunk = true
	if _, err := st.UpdateProvider(p); err != nil {
		t.Fatalf("打开剥离 usage 开关失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	resp := rec.Result()
	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("压缩响应的 Content-Encoding 不该被删，实际 %q", got)
	}
	if !bytes.Equal(rec.Body.Bytes(), payload) {
		t.Errorf("压缩响应体应原样转发，实际长度 %d，期望 %d", rec.Body.Len(), len(payload))
	}
	if cl := resp.Header.Get("Content-Length"); cl != strconv.Itoa(len(payload)) {
		t.Errorf("未改写响应体时应保留上游的 Content-Length（%d），实际 %q", len(payload), cl)
	}
}
