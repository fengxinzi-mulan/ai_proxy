package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ai_proxy/internal/hub"
	"ai_proxy/internal/model"
	"ai_proxy/internal/store"
)

// ---------- 测试脚手架 ----------

func newTestRequest(t *testing.T, path string, headers map[string]string) *http.Request {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader("{}"))
	r.RemoteAddr = "127.0.0.1:12345"
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	return r
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	srv := NewServer(st, hub.New(), quietLogger())
	return srv, st
}

// addProvider 建一个指向指定上游的供应商并设为生效。
func addProvider(t *testing.T, st *store.Store, baseURL string, format model.APIFormat) model.Provider {
	t.Helper()
	p := model.Provider{
		Name:        "test",
		DisplayName: "测试上游",
		Enabled:     true,
		BaseURL:     baseURL,
		APIFormat:   format,
		Keys:        []model.APIKey{{ID: "k1", Key: "sk-test", Enabled: true}},
	}
	created, err := st.CreateProvider(p)
	if err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}
	return created
}

func saveSettings(t *testing.T, st *store.Store, mutate func(*model.Settings)) {
	t.Helper()
	stg, err := st.GetSettings()
	if err != nil {
		t.Fatalf("读取设置失败: %v", err)
	}
	mutate(&stg)
	if err := st.SaveSettings(stg); err != nil {
		t.Fatalf("保存设置失败: %v", err)
	}
}

func lastLog(t *testing.T, st *store.Store) model.RequestLog {
	t.Helper()
	logs, _, err := st.ListLogs(model.LogQuery{Page: 1, PageSize: 1})
	if err != nil {
		t.Fatalf("查询日志失败: %v", err)
	}
	if len(logs) == 0 {
		t.Fatal("没有留下请求日志")
	}
	full, err := st.GetLog(logs[0].ID)
	if err != nil {
		t.Fatalf("读取日志详情失败: %v", err)
	}
	return full
}

// ---------- 非流式转发 ----------

func TestForwardNonStreaming(t *testing.T) {
	var gotAuth, gotPath, gotBody string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":120,"completion_tokens":30,"total_tokens":150,"prompt_tokens_details":{"cached_tokens":64}}}`)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	addProvider(t, st, upstream.URL, model.FormatOpenAI)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","messages":[{"role":"user","content":"hi"}]}`))
	req.RemoteAddr = "10.0.0.9:5555"
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d: %s", rec.Code, rec.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("上游收到的路径错误: %s", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("上游应收到供应商配置的密钥，实际 %q", gotAuth)
	}
	if !strings.Contains(gotBody, `"hi"`) {
		t.Errorf("请求体未被完整转发: %s", gotBody)
	}
	// 响应必须原样回给客户端。
	if !strings.Contains(rec.Body.String(), `"cached_tokens":64`) {
		t.Error("响应体未被原样转发")
	}

	entry := lastLog(t, st)
	if !entry.Success {
		t.Errorf("应记为成功，错误信息: %s", entry.ErrorMsg)
	}
	if entry.PromptTokens != 120 || entry.CompletionTokens != 30 || entry.CachedTokens != 64 || entry.TotalTokens != 150 {
		t.Errorf("用量记录错误: %+v", entry)
	}
	if entry.Model != "gpt-4o" || entry.ModelResponse != "gpt-4o" {
		t.Errorf("模型记录错误: 请求=%q 响应=%q", entry.Model, entry.ModelResponse)
	}
	if entry.ClientIP != "10.0.0.9" {
		t.Errorf("客户端 IP 记录错误: %s", entry.ClientIP)
	}
	if entry.HTTPStatus != 200 || entry.TotalMs < 0 {
		t.Errorf("状态与耗时记录错误: %+v", entry)
	}
	if entry.TokensEstimated {
		t.Error("上游返回了用量，不应标记为估算值")
	}
	if entry.Stream {
		t.Error("非流式请求不应标记为流式")
	}
	// 非流式没有首 token 的概念，也不计算 TPS —— 逐 token 速度无从测量。
	if entry.TTFTMs != 0 {
		t.Errorf("非流式请求的 TTFT 应为 0，实际 %d", entry.TTFTMs)
	}
	if entry.TPS != 0 {
		t.Errorf("非流式请求不应计算 TPS，实际 %v", entry.TPS)
	}
}

// ---------- 流式转发 ----------

func TestForwardStreaming(t *testing.T) {
	chunks := []string{
		"data: {\"model\":\"gpt-4o\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{\"content\":\"，世界\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":80,\"completion_tokens\":40,\"total_tokens\":120}}\n\n",
		"data: [DONE]\n\n",
	}

	var receivedStreamOptions map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if so, ok := body["stream_options"].(map[string]any); ok {
			receivedStreamOptions = so
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for _, c := range chunks {
			io.WriteString(w, c)
			flusher.Flush()
			time.Sleep(2 * time.Millisecond) // 制造真实的首 token 间隔
		}
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	addProvider(t, st, upstream.URL, model.FormatOpenAI)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-4o","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d", rec.Code)
	}
	// 响应的字节必须与上游发出的完全一致。
	want := strings.Join(chunks, "")
	if rec.Body.String() != want {
		t.Errorf("流式响应未被原样转发:\n期望 %q\n实际 %q", want, rec.Body.String())
	}
	if receivedStreamOptions == nil || receivedStreamOptions["include_usage"] != true {
		t.Errorf("流式请求应注入 stream_options.include_usage，实际 %v", receivedStreamOptions)
	}

	entry := lastLog(t, st)
	if !entry.Success {
		t.Errorf("应记为成功，错误信息: %s", entry.ErrorMsg)
	}
	if !entry.Stream {
		t.Error("应标记为流式请求")
	}
	if entry.PromptTokens != 80 || entry.CompletionTokens != 40 {
		t.Errorf("流式用量记录错误: %+v", entry)
	}
	if entry.TTFTMs <= 0 {
		t.Errorf("流式请求应记录到首 token 耗时，实际 %d", entry.TTFTMs)
	}
	if entry.TPS <= 0 {
		t.Errorf("流式请求应算出 TPS，实际 %v", entry.TPS)
	}
	if !entry.RequestModified {
		t.Error("注入了 stream_options，应标记请求被改写")
	}
	if len(entry.Modifications) != 1 || entry.Modifications[0] != "usage_inject" {
		t.Errorf("改写清单错误: %v", entry.Modifications)
	}
	if entry.ReqBodyOriginal == "" {
		t.Error("发生改写时应保留改写前的请求体，供前端对比")
	}
	if !strings.Contains(entry.RespBody, "你好") {
		t.Error("应记录响应报文以便排查")
	}
}

func TestForwardStreamingTruncated(t *testing.T) {
	// 上游只推了内容就断开，没有 [DONE]：这种半截的 200 不能算成功，
	// 否则成功率指标会失真。
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"半截\"}}]}\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	addProvider(t, st, upstream.URL, model.FormatOpenAI)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	entry := lastLog(t, st)
	if entry.Success {
		t.Error("流未正常结束，不应记为成功")
	}
	if !strings.Contains(entry.ErrorMsg, "提前关闭") {
		t.Errorf("应说明中断原因，实际 %q", entry.ErrorMsg)
	}
}

// ---------- 失败路径 ----------

func TestForwardUpstreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		io.WriteString(w, `{"error":{"message":"Incorrect API key provided"}}`)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	addProvider(t, st, upstream.URL, model.FormatOpenAI)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("状态码应原样回传，实际 %d", rec.Code)
	}
	entry := lastLog(t, st)
	if entry.Success {
		t.Error("401 不应记为成功")
	}
	if entry.HTTPStatus != 401 {
		t.Errorf("应记录上游状态码 401，实际 %d", entry.HTTPStatus)
	}
	if !strings.Contains(entry.ErrorMsg, "Incorrect API key") {
		t.Errorf("应记录上游返回的错误描述，实际 %q", entry.ErrorMsg)
	}
}

func TestForwardNoProviderConfigured(t *testing.T) {
	srv, st := newTestServer(t)
	// 刻意不配置任何供应商。
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("没有可用供应商时应返回 502，实际 %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "供应商") {
		t.Errorf("错误信息应提示去配置供应商，实际 %s", rec.Body.String())
	}
	entry := lastLog(t, st)
	if entry.Success {
		t.Error("失败请求不应记为成功")
	}
}

func TestForwardUpstreamTimeout(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	p := addProvider(t, st, upstream.URL, model.FormatOpenAI)
	// 把总超时压到 200ms，触发网关超时路径。
	p.TimeoutSeconds = 1
	p.ConnectTimeoutSeconds = 1
	if _, err := st.UpdateProvider(p); err != nil {
		t.Fatalf("更新供应商失败: %v", err)
	}
	saveSettings(t, st, func(s *model.Settings) { s.DefaultTimeoutSeconds = 1 })

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	ctx, cancel := context.WithTimeout(req.Context(), 200*time.Millisecond)
	defer cancel()
	req = req.WithContext(ctx)

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	entry := lastLog(t, st)
	if entry.Success {
		t.Error("超时不应记为成功")
	}
	if entry.ErrorMsg == "" {
		t.Error("应记录超时原因")
	}
}

func TestForwardClientDisconnect(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer upstream.Close()
	defer close(release)

	srv, st := newTestServer(t)
	addProvider(t, st, upstream.URL, model.FormatOpenAI)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	ctx, cancel := context.WithCancel(req.Context())
	req = req.WithContext(ctx)
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	entry := lastLog(t, st)
	if !entry.Cancelled {
		t.Errorf("客户端断开应标记为已取消，实际 errorMsg=%q", entry.ErrorMsg)
	}
	if entry.Success {
		t.Error("被取消的请求不应记为成功")
	}
	if entry.HTTPStatus != statusClientClosedRequest {
		t.Errorf("客户端断开应记为 %d，实际 %d", statusClientClosedRequest, entry.HTTPStatus)
	}
}

// ---------- 路由 ----------

func TestRouteByProviderName(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		io.WriteString(w, `{"ok":true}`)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	addProvider(t, st, upstream.URL, model.FormatOpenAI) // 短名固定为 test

	req := httptest.NewRequest(http.MethodPost, "/p/test/v1/chat/completions", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if gotPath != "/v1/chat/completions" {
		t.Errorf("应剥掉 /p/<短名> 前缀后转发，实际路径 %s", gotPath)
	}

	// 未知短名应给出明确错误，而不是被当作普通路径转发出去。
	req = httptest.NewRequest(http.MethodPost, "/p/nope/v1/chat/completions", strings.NewReader(`{}`))
	rec = httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Errorf("未知供应商短名应返回 502，实际 %d", rec.Code)
	}
}

func TestActiveProviderSwitch(t *testing.T) {
	var hitA, hitB bool
	upA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitA = true
		io.WriteString(w, `{"from":"a"}`)
	}))
	defer upA.Close()
	upB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitB = true
		io.WriteString(w, `{"from":"b"}`)
	}))
	defer upB.Close()

	srv, st := newTestServer(t)
	a := addProvider(t, st, upA.URL, model.FormatOpenAI)
	b, err := st.CreateProvider(model.Provider{
		Name: "b", DisplayName: "上游 B", Enabled: true, BaseURL: upB.URL, APIFormat: model.FormatOpenAI,
	})
	if err != nil {
		t.Fatalf("创建供应商 B 失败: %v", err)
	}
	_ = a

	// 默认走第一个创建的（A）。
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	srv.ServeHTTP(httptest.NewRecorder(), req)
	if !hitA || hitB {
		t.Error("默认应转发到第一个供应商")
	}

	hitA, hitB = false, false
	if err := st.ActivateProvider(b.ID); err != nil {
		t.Fatalf("切换生效供应商失败: %v", err)
	}
	req = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	srv.ServeHTTP(httptest.NewRecorder(), req)
	if hitA || !hitB {
		t.Error("切换后应转发到新供应商")
	}
}

// ---------- 提示词注入端到端 ----------

func TestPromptInjectionEndToEnd(t *testing.T) {
	var mu sync.Mutex
	var received map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		received = body
		mu.Unlock()
		io.WriteString(w, `{"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	p := addProvider(t, st, upstream.URL, model.FormatOpenAI)
	p.PromptMode = model.ModeCustom
	p.Prompt = model.PromptConfig{Enabled: true, Text: "只回答中文", Strategy: model.StrategyAppend}
	if _, err := st.UpdateProvider(p); err != nil {
		t.Fatalf("更新供应商失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"system","content":"原始"},{"role":"user","content":"hi"}]}`))
	srv.ServeHTTP(httptest.NewRecorder(), req)

	mu.Lock()
	defer mu.Unlock()
	messages, ok := received["messages"].([]any)
	if !ok || len(messages) != 2 {
		t.Fatalf("消息结构被破坏: %v", received["messages"])
	}
	sys := messages[0].(map[string]any)
	if sys["content"] != "原始\n\n只回答中文" {
		t.Errorf("提示词未按预期追加: %v", sys["content"])
	}

	entry := lastLog(t, st)
	if len(entry.Modifications) != 1 || entry.Modifications[0] != "prompt_append" {
		t.Errorf("改写清单错误: %v", entry.Modifications)
	}
}

// TestResponseStripping 覆盖「回传客户端前剥离 usage 块」这一可选行为。
func TestResponseStripping(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":1,\"total_tokens\":6}}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	srv, st := newTestServer(t)
	p := addProvider(t, st, upstream.URL, model.FormatOpenAI)
	p.StripUsageChunk = true
	if _, err := st.UpdateProvider(p); err != nil {
		t.Fatalf("更新供应商失败: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true}`))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if strings.Contains(rec.Body.String(), "usage") {
		t.Errorf("开启剥离后客户端不应看到 usage 块: %s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "hi") {
		t.Error("正文必须保留")
	}
	// 剥离的是回传内容，统计仍然要准。
	entry := lastLog(t, st)
	if entry.PromptTokens != 5 || entry.CompletionTokens != 1 {
		t.Errorf("剥离 usage 块不应影响统计，实际 %+v", entry)
	}
	if !entry.ResponseModified {
		t.Error("改动了响应就应标记出来")
	}
}
