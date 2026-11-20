//go:build desktop && windows

package main

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"ai_proxy/internal/desktop"
)

// testDesktopApp 构造一个只用于关闭流程判断的 desktopApp。
//
// 不启动 Wails 与真实服务：这里要验证的恰恰是「没有服务/没有托盘时」的放行逻辑。
func testDesktopApp(t *testing.T) *desktopApp {
	t.Helper()
	return &desktopApp{
		logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
		dataDir: t.TempDir(),
		ctx:     context.Background(),
	}
}

// Bug 31：没有服务或没有托盘时，窗口关闭必须放行。
//
// 修复前「每次询问」分支在 svc==nil 时直接 return false（阻止关闭），
// 而确认框永远发不出去 —— 用户点关闭没有任何反应，只能用任务管理器杀进程。
func TestHandleCloseAllowsWhenNothingToAsk(t *testing.T) {
	for _, action := range []desktop.CloseAction{desktop.CloseAsk, desktop.CloseTray} {
		t.Run(string(action), func(t *testing.T) {
			a := testDesktopApp(t)
			cfg := desktop.LoadConfig(a.dataDir)
			cfg.CloseAction = action
			if err := desktop.SaveConfig(a.dataDir, cfg); err != nil {
				t.Fatalf("写入桌面配置失败: %v", err)
			}

			if !a.handleClose(context.Background()) {
				t.Fatalf("closeAction=%s 且无服务/托盘时必须允许关闭", action)
			}
			if !a.quitting.Load() {
				t.Errorf("放行关闭时应置位 quitting")
			}
		})
	}
}

func TestHandleCloseQuitActionAlwaysAllows(t *testing.T) {
	a := testDesktopApp(t)
	cfg := desktop.LoadConfig(a.dataDir)
	cfg.CloseAction = desktop.CloseQuit
	if err := desktop.SaveConfig(a.dataDir, cfg); err != nil {
		t.Fatal(err)
	}
	if !a.handleClose(context.Background()) {
		t.Fatal("closeAction=quit 时应允许关闭")
	}
}

// 窗口还没就绪（ctx 未注入）时收到关闭请求也必须放行。
func TestHandleCloseAllowsBeforeWindowReady(t *testing.T) {
	a := testDesktopApp(t)
	a.ctx = nil
	if !a.handleClose(context.Background()) {
		t.Fatal("窗口未就绪时应允许关闭")
	}
}

// Bug 30：单实例释放包装必须幂等 —— 重启路径先调一次，main 的 defer 再调一次。
func TestReleaseSingleInstanceIsIdempotent(t *testing.T) {
	calls := 0
	release := releaseSingleInstance(func() { calls++ })

	release()
	release()
	release()
	if calls != 1 {
		t.Fatalf("互斥量应只释放一次，实际 %d 次", calls)
	}
}
