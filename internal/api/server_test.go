package api

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ai_proxy/internal/hub"
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
