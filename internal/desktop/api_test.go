package desktop

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func newDesktopHandler(t *testing.T, dataDir string) *http.ServeMux {
	t.Helper()
	mux := http.NewServeMux()
	NewDesktopAPI(dataDir, nil).Register(mux, nil)
	return mux
}

func postJSON(t *testing.T, h http.Handler, path, body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// Bug 33：交给文件管理器的必须是绝对路径。
//
// explorer.exe / xdg-open 按**自己的**工作目录解析相对路径，dataDir 是相对
// 形式时会打开 System32 之类的地方；这里用 openDir 钩子拦住真实调用。
func TestHandleOpenPassesAbsolutePath(t *testing.T) {
	dataDir := t.TempDir()
	var got string
	orig := openDir
	openDir = func(dir string) error {
		got = dir
		return nil
	}
	t.Cleanup(func() { openDir = orig })

	h := newDesktopHandler(t, dataDir)
	status, body := postJSON(t, h, "/api/desktop/open", `{"target":"logs"}`)
	if status != http.StatusOK {
		t.Fatalf("打开目录应成功，实际 %d: %s", status, body)
	}

	want := filepath.Join(dataDir, "logs")
	if got != want {
		t.Errorf("传给文件管理器的路径应为 %q，实际 %q", want, got)
	}
	if !filepath.IsAbs(got) {
		t.Errorf("路径必须是绝对路径，实际 %q", got)
	}

	var resp struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("响应不是合法 JSON: %v (%s)", err, body)
	}
	if resp.Path != want {
		t.Errorf("响应路径应为 %q，实际 %q", want, resp.Path)
	}
}

func TestHandleOpenRejectsUnknownTarget(t *testing.T) {
	h := newDesktopHandler(t, t.TempDir())
	status, body := postJSON(t, h, "/api/desktop/open", `{"target":"C:\\Windows"}`)
	if status != http.StatusBadRequest {
		t.Fatalf("未知目标应返回 400，实际 %d: %s", status, body)
	}
}
