package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"path/filepath"
	"testing"
	"testing/fstest"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError}))
}

func testWebFS() fstest.MapFS {
	return fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte("<html>ok</html>")},
	}
}

func TestResolveDataDirExplicitWins(t *testing.T) {
	explicit := filepath.Join(t.TempDir(), "custom")
	got, err := ResolveDataDir(explicit, true, true)
	if err != nil {
		t.Fatal(err)
	}
	if got != explicit {
		t.Fatalf("got %q, want %q", got, explicit)
	}
}

func TestResolveDataDirServerModeKeepsRelative(t *testing.T) {
	got, err := ResolveDataDir("data", false, false)
	if err != nil {
		t.Fatal(err)
	}
	if got != "data" {
		t.Fatalf("命令行模式应保持相对路径 data，got %q", got)
	}
}

func TestResolveDataDirDesktopIsAbsolute(t *testing.T) {
	// 无法在测试里替换 os.Executable 的返回值，这里覆盖「找不到同级 data 时
	// 退化到用户配置目录」这条分支，结果必须是绝对路径且落在 ai_proxy 目录。
	got, err := ResolveDataDir("data", false, true)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(got) {
		t.Fatalf("桌面模式应返回绝对路径，got %q", got)
	}
	if filepath.Base(got) != "ai_proxy" {
		t.Fatalf("桌面模式默认目录名应为 ai_proxy，got %q", got)
	}
}

// 端口被占用时，桌面模式必须返回可识别的 ListenError，方便上层提示换端口。
func TestStartReportsListenError(t *testing.T) {
	blocker, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close()
	port := blocker.Addr().(*net.TCPAddr).Port

	_, err = Start(Options{
		Host:        "127.0.0.1",
		Port:        port,
		DataDir:     t.TempDir(),
		WebFS:       testWebFS(),
		Placeholder: []byte("placeholder"),
		Logger:      testLogger(),
		DesktopMode: true,
	})
	if err == nil {
		t.Fatal("端口被占用时应当返回错误")
	}
	var le *ListenError
	if !errors.As(err, &le) {
		t.Fatalf("期望 *ListenError，got %T: %v", err, err)
	}
	if le.Port != port {
		t.Fatalf("ListenError.Port=%d, want %d", le.Port, port)
	}
}

// 关闭服务时，正在进行的 SSE 请求必须立刻结束，而不是等到 Shutdown 超时。
func TestShutdownEndsSSERequest(t *testing.T) {
	s, err := Start(Options{
		Host:        "127.0.0.1",
		Port:        0,
		DataDir:     t.TempDir(),
		WebFS:       testWebFS(),
		Placeholder: []byte("placeholder"),
		Logger:      testLogger(),
	})
	if err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(s.URL() + "api/events")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("SSE 状态码 %d", resp.StatusCode)
	}

	// 读响应体，关闭时它应当很快读到 EOF。
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, resp.Body)
		close(done)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := s.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown 之后 SSE 连接仍未结束")
	}
}

// 服务启动后，/api/meta 必须回报真实端口，而不是设置里的默认值。
func TestMetaReportsActualPort(t *testing.T) {
	s, err := Start(Options{
		Host:        "127.0.0.1",
		Port:        0,
		DataDir:     t.TempDir(),
		WebFS:       testWebFS(),
		Placeholder: []byte("placeholder"),
		Logger:      testLogger(),
		Version:     "9.9.9",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()

	resp, err := http.Get(s.URL() + "api/meta")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	var meta struct {
		Listen struct {
			Host string `json:"host"`
			Port int    `json:"port"`
		} `json:"listen"`
		Version string `json:"version"`
		Desktop bool   `json:"desktop"`
	}
	if err := json.Unmarshal(body, &meta); err != nil {
		t.Fatalf("解析 meta 失败: %v (%s)", err, body)
	}
	if meta.Listen.Port != s.Port() || meta.Listen.Port == 0 {
		t.Fatalf("meta.listen.port=%d, 实际端口=%d", meta.Listen.Port, s.Port())
	}
	if meta.Version != "9.9.9" {
		t.Fatalf("meta.version=%q", meta.Version)
	}
	if meta.Desktop {
		t.Fatal("命令行模式不应报告 desktop=true")
	}
}

func TestStartFailsWithoutWebFS(t *testing.T) {
	_, err := Start(Options{
		Host:    "127.0.0.1",
		Port:    0,
		DataDir: t.TempDir(),
		Logger:  testLogger(),
		Version: "dev",
	})
	if err == nil {
		t.Fatal("缺少前端资源时应当报错")
	}
}

// 负数端口不覆盖设置，走设置里的默认值；确认这条路径不会 panic 且端口有效。
func TestNegativePortFallsBackToSetting(t *testing.T) {
	s, err := Start(Options{
		Host:        "127.0.0.1",
		Port:        -1,
		DataDir:     t.TempDir(),
		WebFS:       testWebFS(),
		Placeholder: []byte("placeholder"),
		Logger:      testLogger(),
	})
	if err != nil {
		t.Skipf("默认端口不可用，跳过: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = s.Shutdown(ctx)
	}()
	if s.Port() == 0 {
		t.Fatal("端口不应为 0")
	}
}
