package api

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ai_proxy/internal/autostart"
	"ai_proxy/internal/hub"
	"ai_proxy/internal/model"
	"ai_proxy/internal/proxy"
	"ai_proxy/internal/store"
)

func newTestAPI(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxySrv := proxy.NewServer(st, hub.New(), logger)
	srv := New(st, hub.New(), proxySrv, logger, "127.0.0.1", 8080)

	mux := http.NewServeMux()
	srv.Register(mux)
	return srv, mux
}

func doJSON(t *testing.T, h http.Handler, method, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// TestCreateProviderIgnoresClientTimestamps 覆盖一个真实的线上故障：
// 前端把表单里的 createdAt/updatedAt 一起提交，新建时它们是空串，
// 而 Go 的 time.Time 解析不了空串，导致整个请求 400「请求体不是合法 JSON」。
// 时间戳由服务端管理，客户端传什么都不该让创建失败。
func TestCreateProviderIgnoresClientTimestamps(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{
			name: "空字符串时间戳",
			body: `{"name":"cc","displayName":"commandcode","enabled":true,
				"baseUrl":"https://api.example.com/v1","apiFormat":"openai",
				"createdAt":"","updatedAt":""}`,
		},
		{
			name: "完全不传时间戳",
			body: `{"name":"cc","enabled":true,"baseUrl":"https://api.example.com/v1"}`,
		},
		{
			name: "传了合法时间戳也应被忽略",
			body: `{"name":"cc","enabled":true,"baseUrl":"https://api.example.com/v1",
				"createdAt":"2020-01-01T00:00:00Z","updatedAt":"2020-01-01T00:00:00Z"}`,
		},
		{
			name: "传了格式错误的非空时间戳",
			body: `{"name":"cc","enabled":true,"baseUrl":"https://api.example.com/v1",
				"createdAt":"昨天","updatedAt":"-1"}`,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// 每个子用例用独立的库，避免短名重复互相干扰。
			_, h := newTestAPI(t)

			status, resp := doJSON(t, h, http.MethodPost, "/api/providers", c.body)
			if status != http.StatusOK {
				t.Fatalf("创建应成功，实际 HTTP %d: %s", status, resp)
			}
			var created struct {
				ID        int64  `json:"id"`
				CreatedAt string `json:"createdAt"`
			}
			if err := json.Unmarshal([]byte(resp), &created); err != nil {
				t.Fatalf("响应不是合法 JSON: %v", err)
			}
			if created.ID == 0 {
				t.Error("应返回自增 ID")
			}
			// 时间戳必须以服务端写入的值为准，而不是客户端塞进来的。
			if created.CreatedAt == "" || strings.HasPrefix(created.CreatedAt, "2020-01-01") {
				t.Errorf("时间戳应由服务端生成，实际 %q", created.CreatedAt)
			}
		})
	}
}

func TestUpdateProviderIgnoresClientTimestamps(t *testing.T) {
	_, h := newTestAPI(t)

	status, resp := doJSON(t, h, http.MethodPost, "/api/providers",
		`{"name":"p1","enabled":true,"baseUrl":"https://api.example.com/v1"}`)
	if status != http.StatusOK {
		t.Fatalf("准备数据失败: HTTP %d %s", status, resp)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal([]byte(resp), &created)

	// 模拟前端「编辑后保存」：把列表里读到的对象整体回传，含空时间戳。
	status, resp = doJSON(t, h, http.MethodPut, "/api/providers/1",
		`{"id":1,"name":"p1","displayName":"改过名字","enabled":true,
		  "baseUrl":"https://api.example.com/v2","createdAt":"","updatedAt":""}`)
	if status != http.StatusOK {
		t.Fatalf("更新应成功，实际 HTTP %d: %s", status, resp)
	}
	if !strings.Contains(resp, "改过名字") || !strings.Contains(resp, "/v2") {
		t.Errorf("更新未生效: %s", resp)
	}
	// 创建时间不应被客户端覆盖成零值。
	if strings.Contains(resp, "0001-01-01") {
		t.Errorf("创建时间被客户端覆盖: %s", resp)
	}
}

// TestCreateProviderValidation 确认校验失败返回的是客户端错误码与可读信息。
func TestCreateProviderValidation(t *testing.T) {
	_, h := newTestAPI(t)

	status, resp := doJSON(t, h, http.MethodPost, "/api/providers",
		`{"name":"","enabled":true,"baseUrl":"https://api.example.com/v1"}`)
	if status != http.StatusBadRequest {
		t.Errorf("缺少短名应返回 400，实际 %d: %s", status, resp)
	}
	if !strings.Contains(resp, "短名") {
		t.Errorf("错误信息应说明缺少短名，实际 %s", resp)
	}

	doJSON(t, h, http.MethodPost, "/api/providers",
		`{"name":"dup","enabled":true,"baseUrl":"https://api.example.com/v1"}`)
	status, resp = doJSON(t, h, http.MethodPost, "/api/providers",
		`{"name":"dup","enabled":true,"baseUrl":"https://api.example.com/v1"}`)
	if status != http.StatusConflict {
		t.Errorf("重名短名应返回 409，实际 %d: %s", status, resp)
	}
	if !strings.Contains(resp, "已存在") {
		t.Errorf("错误信息应说明重名，实际 %s", resp)
	}
}

func TestGetMissingProviderReturns404(t *testing.T) {
	_, h := newTestAPI(t)
	status, resp := doJSON(t, h, http.MethodGet, "/api/providers/999", "")
	if status != http.StatusNotFound {
		t.Errorf("不存在的供应商应返回 404，实际 %d: %s", status, resp)
	}
}

// ---------- 供应商优化预设 ----------

// createProvider 建一个供应商并返回落库后的对象。
func createProvider(t *testing.T, h http.Handler, body string) model.Provider {
	t.Helper()
	status, resp := doJSON(t, h, http.MethodPost, "/api/providers", body)
	if status != http.StatusOK {
		t.Fatalf("创建供应商失败: %d %s", status, resp)
	}
	var p model.Provider
	if err := json.Unmarshal([]byte(resp), &p); err != nil {
		t.Fatalf("创建结果不是合法 JSON: %v %s", err, resp)
	}
	return p
}

func TestListProviderPresets(t *testing.T) {
	_, h := newTestAPI(t)
	status, resp := doJSON(t, h, http.MethodGet, "/api/provider-presets", "")
	if status != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d: %s", status, resp)
	}
	var presets []model.ProviderPreset
	if err := json.Unmarshal([]byte(resp), &presets); err != nil {
		t.Fatalf("结果不是合法 JSON: %v %s", err, resp)
	}
	if len(presets) == 0 || presets[0].ID != "mistral" {
		t.Fatalf("应内置 mistral 预设，实际 %+v", presets)
	}
	if len(presets[0].Hosts) == 0 || len(presets[0].Changes) == 0 {
		t.Errorf("预设缺少域名或变更说明: %+v", presets[0])
	}
}

func TestApplyProviderPreset(t *testing.T) {
	_, h := newTestAPI(t)
	p := createProvider(t, h, `{"name":"mistral","displayName":"Mistral",
		"enabled":true,"baseUrl":"https://api.mistral.ai/v1","apiFormat":"openai"}`)

	status, resp := doJSON(t, h, http.MethodPost, fmt.Sprintf("/api/providers/%d/apply-preset", p.ID),
		`{"presetId":"mistral"}`)
	if status != http.StatusOK {
		t.Fatalf("应用预设应返回 200，实际 %d: %s", status, resp)
	}
	var result struct {
		Provider model.Provider `json:"provider"`
		Applied  []string       `json:"applied"`
	}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		t.Fatalf("结果不是合法 JSON: %v %s", err, resp)
	}
	if len(result.Applied) != 7 {
		t.Errorf("应改动七项，实际 %v", result.Applied)
	}
	if !result.Provider.StripUnsupportedParams ||
		!result.Provider.ReplayReasoningAsThinkChunk ||
		result.Provider.PromptCacheKeyMode != model.PromptCacheKeyAuto ||
		result.Provider.UsageInjectMode != "off" ||
		result.Provider.TimeoutSeconds != 900 ||
		result.Provider.MaxImagesPerRequest != 20 ||
		result.Provider.ImageTrimBatch != 5 {
		t.Errorf("预设未正确落库: %+v", result.Provider)
	}

	// 落库后重新读取，确认不是只改了内存。
	status, resp = doJSON(t, h, http.MethodGet, fmt.Sprintf("/api/providers/%d", p.ID), "")
	if status != http.StatusOK {
		t.Fatalf("读取供应商失败: %d %s", status, resp)
	}
	var stored model.Provider
	if err := json.Unmarshal([]byte(resp), &stored); err != nil {
		t.Fatalf("供应商不是合法 JSON: %v", err)
	}
	if stored.PromptCacheKeyMode != model.PromptCacheKeyAuto || stored.TimeoutSeconds != 900 {
		t.Errorf("重新读取后配置没变: %+v", stored)
	}

	// 幂等：第二次应用没有新改动。
	status, resp = doJSON(t, h, http.MethodPost, fmt.Sprintf("/api/providers/%d/apply-preset", p.ID),
		`{"presetId":"mistral"}`)
	if status != http.StatusOK {
		t.Fatalf("重复应用应返回 200，实际 %d: %s", status, resp)
	}
	if !strings.Contains(resp, `"applied":[]`) {
		t.Errorf("重复应用不该再有改动，实际 %s", resp)
	}
}

func TestApplyProviderPresetRejectsWrongHost(t *testing.T) {
	_, h := newTestAPI(t)
	p := createProvider(t, h, `{"name":"other","displayName":"别家",
		"enabled":true,"baseUrl":"https://api.example.com/v1","apiFormat":"openai"}`)

	status, resp := doJSON(t, h, http.MethodPost, fmt.Sprintf("/api/providers/%d/apply-preset", p.ID),
		`{"presetId":"mistral"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("域名不匹配应拒绝，实际 %d: %s", status, resp)
	}
	if !strings.Contains(resp, "force=true") {
		t.Errorf("错误信息应提示可以强制应用，实际 %s", resp)
	}

	// 显式强制时允许应用（自建反代的场景）。
	status, resp = doJSON(t, h, http.MethodPost, fmt.Sprintf("/api/providers/%d/apply-preset", p.ID),
		`{"presetId":"mistral","force":true}`)
	if status != http.StatusOK {
		t.Fatalf("force=true 时应允许应用，实际 %d: %s", status, resp)
	}
}

func TestApplyProviderPresetUnknownID(t *testing.T) {
	_, h := newTestAPI(t)
	p := createProvider(t, h, `{"name":"mistral","enabled":true,
		"baseUrl":"https://api.mistral.ai/v1","apiFormat":"openai"}`)
	status, resp := doJSON(t, h, http.MethodPost, fmt.Sprintf("/api/providers/%d/apply-preset", p.ID),
		`{"presetId":"nope"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("未知预设应返回 400，实际 %d: %s", status, resp)
	}
}

// ---------- 数据库空间 ----------

func TestDBStatsAndVacuum(t *testing.T) {
	_, h := newTestAPI(t)
	status, resp := doJSON(t, h, http.MethodGet, "/api/stats/db", "")
	if status != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d: %s", status, resp)
	}
	var stats store.DBStats
	if err := json.Unmarshal([]byte(resp), &stats); err != nil {
		t.Fatalf("结果不是合法 JSON: %v %s", err, resp)
	}
	if stats.PageSize <= 0 || stats.PageCount <= 0 {
		t.Errorf("页统计异常: %+v", stats)
	}

	status, resp = doJSON(t, h, http.MethodPost, "/api/stats/vacuum", "")
	if status != http.StatusOK {
		t.Fatalf("回收空间应返回 200，实际 %d: %s", status, resp)
	}
	if !strings.Contains(resp, "reclaimed") {
		t.Errorf("应返回回收字节数，实际 %s", resp)
	}
}

// ---------- 开机自启 ----------

// fakeAutostart 是内存后端，避免单测往真实 Run 键里写东西。
type fakeAutostart struct {
	supported bool
	reason    string
	values    map[string]string
}

func (f *fakeAutostart) Supported() (bool, string) { return f.supported, f.reason }

func (f *fakeAutostart) Get(name string) (string, bool, error) {
	v, ok := f.values[name]
	return v, ok, nil
}

func (f *fakeAutostart) Set(name, command string) error {
	f.values[name] = command
	return nil
}

func (f *fakeAutostart) Delete(name string) error {
	delete(f.values, name)
	return nil
}

func newAutostartAPI(t *testing.T, spec autostart.Spec) http.Handler {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	proxySrv := proxy.NewServer(st, hub.New(), logger)
	srv := New(st, hub.New(), proxySrv, logger, "127.0.0.1", 8080)
	srv.SetAutostart(spec)

	mux := http.NewServeMux()
	srv.Register(mux)
	return mux
}

func TestAutostartToggle(t *testing.T) {
	be := &fakeAutostart{supported: true, values: map[string]string{}}
	h := newAutostartAPI(t, autostart.Spec{
		Name:    "AIProxy-Desktop",
		Command: `"C:\a b\x.exe" -autostart`,
		Backend: be,
	})

	status, resp := doJSON(t, h, http.MethodGet, "/api/autostart", "")
	if status != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d: %s", status, resp)
	}
	var state autostart.State
	if err := json.Unmarshal([]byte(resp), &state); err != nil {
		t.Fatalf("结果不是合法 JSON: %v %s", err, resp)
	}
	if !state.Supported || state.Enabled {
		t.Fatalf("初始应为「支持但未开启」，实际 %+v", state)
	}

	status, resp = doJSON(t, h, http.MethodPut, "/api/autostart", `{"enabled":true}`)
	if status != http.StatusOK {
		t.Fatalf("开启应返回 200，实际 %d: %s", status, resp)
	}
	if err := json.Unmarshal([]byte(resp), &state); err != nil {
		t.Fatal(err)
	}
	if !state.Enabled || state.Registered != `"C:\a b\x.exe" -autostart` {
		t.Fatalf("开启后状态不对: %+v", state)
	}
	if be.values["AIProxy-Desktop"] == "" {
		t.Error("应真的写进后端")
	}

	status, resp = doJSON(t, h, http.MethodPut, "/api/autostart", `{"enabled":false}`)
	if status != http.StatusOK {
		t.Fatalf("关闭应返回 200，实际 %d: %s", status, resp)
	}
	if err := json.Unmarshal([]byte(resp), &state); err != nil {
		t.Fatal(err)
	}
	if state.Enabled {
		t.Errorf("关闭后不该还是开启: %+v", state)
	}
	if len(be.values) != 0 {
		t.Error("应真的从后端删掉")
	}
}

func TestAutostartStaleDetection(t *testing.T) {
	be := &fakeAutostart{supported: true, values: map[string]string{
		"AIProxy-Desktop": `C:\old\x.exe -autostart`,
	}}
	h := newAutostartAPI(t, autostart.Spec{
		Name:    "AIProxy-Desktop",
		Command: `C:\new\x.exe -autostart`,
		Backend: be,
	})

	_, resp := doJSON(t, h, http.MethodGet, "/api/autostart", "")
	var state autostart.State
	if err := json.Unmarshal([]byte(resp), &state); err != nil {
		t.Fatal(err)
	}
	if !state.Enabled || !state.Stale {
		t.Fatalf("应识别为「已开启但过期」，实际 %+v", state)
	}
}

func TestAutostartUnsupported(t *testing.T) {
	be := &fakeAutostart{supported: false, reason: "仅 Windows 支持", values: map[string]string{}}
	h := newAutostartAPI(t, autostart.Spec{Name: "x", Command: "y", Backend: be})

	_, resp := doJSON(t, h, http.MethodGet, "/api/autostart", "")
	var state autostart.State
	if err := json.Unmarshal([]byte(resp), &state); err != nil {
		t.Fatal(err)
	}
	if state.Supported || state.Reason != "仅 Windows 支持" {
		t.Fatalf("不支持时应透传原因，实际 %+v", state)
	}

	status, resp := doJSON(t, h, http.MethodPut, "/api/autostart", `{"enabled":true}`)
	if status != http.StatusBadRequest {
		t.Fatalf("不支持时开启应返回 400，实际 %d: %s", status, resp)
	}
	if !strings.Contains(resp, "仅 Windows 支持") {
		t.Errorf("错误信息应说明原因，实际 %s", resp)
	}
}

// ---------- 版本更新 ----------

// 没有注入 updater 时，更新接口应明确回报「不提供」，而不是 500。
func TestUpdateEndpointsWithoutUpdater(t *testing.T) {
	_, h := newTestAPI(t)
	for _, path := range []string{"/api/update", "/api/update/check", "/api/update/download", "/api/update/skip"} {
		method := http.MethodGet
		if path != "/api/update" {
			method = http.MethodPost
		}
		status, resp := doJSON(t, h, method, path, "")
		if status != http.StatusNotImplemented {
			t.Errorf("%s 应返回 501，实际 %d: %s", path, status, resp)
		}
	}
}

// ---------- 设置的向后兼容 ----------

// 省略 imageCompress 的 PUT 不该把用户的「开启」静默改成关闭。
func TestPutSettingsKeepsImageCompressWhenOmitted(t *testing.T) {
	_, h := newTestAPI(t)

	// 先显式关掉，再发一个不含 imageCompress 的请求。
	status, resp := doJSON(t, h, http.MethodPut, "/api/settings",
		`{"host":"127.0.0.1","port":8080,"imageCompress":{"enabled":false,"thresholdBytes":262144,"maxEdge":1024,"quality":70}}`)
	if status != http.StatusOK {
		t.Fatalf("保存失败: %d %s", status, resp)
	}
	var saved model.Settings
	if err := json.Unmarshal([]byte(resp), &saved); err != nil {
		t.Fatal(err)
	}
	if saved.ImageCompress.Enabled {
		t.Fatal("显式 false 应被保存")
	}

	// 不带 imageCompress 的请求。
	status, resp = doJSON(t, h, http.MethodPut, "/api/settings",
		`{"host":"127.0.0.1","port":8080}`)
	if status != http.StatusOK {
		t.Fatalf("保存失败: %d %s", status, resp)
	}
	var after model.Settings
	if err := json.Unmarshal([]byte(resp), &after); err != nil {
		t.Fatal(err)
	}
	if after.ImageCompress.Enabled {
		t.Error("省略 imageCompress 时不该把 Enabled 顶回 true")
	}
	if after.ImageCompress.ThresholdBytes != 262144 || after.ImageCompress.Quality != 70 {
		t.Errorf("省略时应保持原值，实际 %+v", after.ImageCompress)
	}

	// 显式开启仍然生效。
	status, resp = doJSON(t, h, http.MethodPut, "/api/settings",
		`{"host":"127.0.0.1","port":8080,"imageCompress":{"enabled":true,"thresholdBytes":262144,"maxEdge":1024,"quality":70}}`)
	if status != http.StatusOK {
		t.Fatalf("保存失败: %d %s", status, resp)
	}
	if err := json.Unmarshal([]byte(resp), &after); err != nil {
		t.Fatal(err)
	}
	if !after.ImageCompress.Enabled {
		t.Error("显式 true 应被保存")
	}
}

// ---------- Bug 37：访问密钥可以显式清除 ----------

// 空串曾长期表示「保持原值」，导致用户永远关不掉访问密钥。
func TestPutSettingsAccessKeyCanBeCleared(t *testing.T) {
	_, h := newTestAPI(t)

	status, resp := doJSON(t, h, http.MethodPut, "/api/settings",
		`{"host":"127.0.0.1","port":8080,"accessKey":"s3cret"}`)
	if status != http.StatusOK {
		t.Fatalf("保存失败: %d %s", status, resp)
	}
	if !strings.Contains(resp, `"accessKey":"s3cret"`) {
		t.Fatalf("访问密钥应写入: %s", resp)
	}

	// 不传 accessKey：保持原值（脚本/旧页面不会回传密钥）。
	status, resp = doJSON(t, h, http.MethodPut, "/api/settings",
		`{"host":"127.0.0.1","port":8080}`)
	if status != http.StatusOK {
		t.Fatalf("保存失败: %d %s", status, resp)
	}
	if !strings.Contains(resp, `"accessKey":"s3cret"`) {
		t.Errorf("省略 accessKey 时应保持原值: %s", resp)
	}

	// 显式 null 也按「没传」处理，不能被当成清空。
	status, resp = doJSON(t, h, http.MethodPut, "/api/settings",
		`{"host":"127.0.0.1","port":8080,"accessKey":null}`)
	if status != http.StatusOK {
		t.Fatalf("保存失败: %d %s", status, resp)
	}
	if !strings.Contains(resp, `"accessKey":"s3cret"`) {
		t.Errorf("accessKey=null 应保持原值: %s", resp)
	}

	// 显式空串：清除。
	status, resp = doJSON(t, h, http.MethodPut, "/api/settings",
		`{"host":"127.0.0.1","port":8080,"accessKey":""}`)
	if status != http.StatusOK {
		t.Fatalf("保存失败: %d %s", status, resp)
	}
	if !strings.Contains(resp, `"accessKey":""`) {
		t.Errorf("显式空串应清除访问密钥: %s", resp)
	}
}

// ---------- Bug 38 / 39：日志删除与清空 ----------

func insertTestLog(t *testing.T, srv *Server) {
	t.Helper()
	now := time.Now()
	entry := model.RequestLog{
		TSStart: now, TSEnd: now,
		ProviderID: 1, ProviderName: "上游", Model: "gpt-4o",
		APIFormat: "openai", HTTPStatus: 200, Success: true,
		Modifications: []string{},
	}
	if _, err := srv.store.InsertLog(&entry); err != nil {
		t.Fatalf("插入测试日志失败: %v", err)
	}
}

// 无条件 DELETE 曾因为 parseLogQuery 的默认 24 小时窗口静默删掉一天的数据；
// range=all 则会生成恒真的 ts_start <= now，同样绕过全表删保护。
func TestDeleteLogsRequiresExplicitConditions(t *testing.T) {
	srv, h := newTestAPI(t)
	insertTestLog(t, srv)

	for _, path := range []string{"/api/logs", "/api/logs?range=all"} {
		status, resp := doJSON(t, h, http.MethodDelete, path, "")
		if status != http.StatusBadRequest {
			t.Errorf("%s 应返回 400，实际 %d: %s", path, status, resp)
		}
	}

	// 日志必须还在。
	status, resp := doJSON(t, h, http.MethodGet, "/api/logs", "")
	if status != http.StatusOK {
		t.Fatalf("列表失败: %d %s", status, resp)
	}
	var list struct {
		Total int `json:"total"`
	}
	if err := json.Unmarshal([]byte(resp), &list); err != nil {
		t.Fatal(err)
	}
	if list.Total != 1 {
		t.Fatalf("无条件删除不该生效，剩余 %d 条", list.Total)
	}

	// 带条件才放行（这里没有匹配的供应商，删 0 条也是成功）。
	status, resp = doJSON(t, h, http.MethodDelete, "/api/logs?providerId=99", "")
	if status != http.StatusOK {
		t.Fatalf("带条件删除应放行，实际 %d: %s", status, resp)
	}
	if !strings.Contains(resp, `"deleted":0`) {
		t.Errorf("不应误删日志: %s", resp)
	}
}

// 非法 JSON 只能写一次响应：修复前 decode 写 400 后，确认校验分支又写一次，
// 响应体变成两段 JSON，前端的错误提示反而拿不到原因。
func TestClearLogsInvalidJSONWritesSingleError(t *testing.T) {
	_, h := newTestAPI(t)
	req := httptest.NewRequest(http.MethodPost, "/api/logs/clear", bytes.NewBufferString(`{"confirm":`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("非法 JSON 应返回 400，实际 %d", rec.Code)
	}
	dec := json.NewDecoder(strings.NewReader(rec.Body.String()))
	var first map[string]any
	if err := dec.Decode(&first); err != nil {
		t.Fatalf("响应体应为 JSON: %v (%q)", err, rec.Body.String())
	}
	if first["error"] == nil {
		t.Errorf("响应应包含错误信息: %q", rec.Body.String())
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		t.Errorf("响应体只应有一段 JSON，实际 %q", rec.Body.String())
	}
}

// ---------- Bug 42：接口形态补齐 ----------

// providerPayload 被嵌入匿名结构体后，空/非法时间戳仍必须能解析成功。
func TestProviderPayloadEmbeddedDecode(t *testing.T) {
	var body struct {
		providerPayload
		ProbeModel string `json:"probeModel"`
	}
	raw := `{"name":"p","baseUrl":"https://x/v1","createdAt":"","updatedAt":"昨天","probeModel":"m"}`
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("时间戳不该导致解析失败: %v", err)
	}
	if body.BaseURL != "https://x/v1" || body.ProbeModel != "m" {
		t.Errorf("字段解析异常: baseUrl=%q probeModel=%q", body.BaseURL, body.ProbeModel)
	}
}

// 激活动作要和列表接口返回同一形态，前端才能整份替换。
func TestActivateProviderReturnsListView(t *testing.T) {
	_, h := newTestAPI(t)
	p := createProvider(t, h, `{"name":"p1","enabled":true,"baseUrl":"https://api.example.com/v1"}`)

	status, resp := doJSON(t, h, http.MethodPost, fmt.Sprintf("/api/providers/%d/activate", p.ID), "")
	if status != http.StatusOK {
		t.Fatalf("激活应成功，实际 %d: %s", status, resp)
	}
	var views []struct {
		ID        int64           `json:"id"`
		KeyHealth json.RawMessage `json:"keyHealth"`
		Usage     json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal([]byte(resp), &views); err != nil {
		t.Fatalf("应返回供应商数组: %v (%s)", err, resp)
	}
	if len(views) != 1 || views[0].ID != p.ID {
		t.Fatalf("返回内容不对: %s", resp)
	}
	if len(views[0].KeyHealth) == 0 {
		t.Errorf("激活动作的响应缺少 keyHealth: %s", resp)
	}
}

// 应用预设会改动转发相关字段，必须清续传状态并广播 providers 事件。
func TestApplyProviderPresetBroadcastsProviders(t *testing.T) {
	srv, h := newTestAPI(t)
	p := createProvider(t, h, `{"name":"mistral","enabled":true,
		"baseUrl":"https://api.mistral.ai/v1","apiFormat":"openai"}`)

	ch := srv.hub.Subscribe()
	defer srv.hub.Unsubscribe(ch)

	status, resp := doJSON(t, h, http.MethodPost, fmt.Sprintf("/api/providers/%d/apply-preset", p.ID),
		`{"presetId":"mistral"}`)
	if status != http.StatusOK {
		t.Fatalf("应用预设失败: %d %s", status, resp)
	}

	deadline := time.After(2 * time.Second)
	for {
		select {
		case payload, ok := <-ch:
			if !ok {
				t.Fatal("事件通道被关闭")
			}
			if strings.Contains(string(payload), `"providers"`) {
				return
			}
		case <-deadline:
			t.Fatal("应用预设后没有广播 providers 事件")
		}
	}
}

// autoBucket 不能因为 to 是零值就算出负数跨度而退化成按天。
func TestAutoBucketHandlesZeroTo(t *testing.T) {
	now := time.Now()
	if got := autoBucket(time.Time{}, now); got != store.BucketDay {
		t.Errorf("from 为零时应按天，实际 %v", got)
	}
	if got := autoBucket(now.Add(-time.Hour), time.Time{}); got != store.BucketMinute {
		t.Errorf("to 为零时应按当前时间补齐，实际 %v", got)
	}
	if got := autoBucket(now.Add(time.Hour), time.Time{}); got != store.BucketDay {
		t.Errorf("跨度为负时应退回按天，实际 %v", got)
	}
	if got := autoBucket(now.Add(-30*time.Hour), now); got != store.BucketHour {
		t.Errorf("30 小时应按小时，实际 %v", got)
	}
}
