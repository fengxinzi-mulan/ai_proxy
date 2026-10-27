//go:build desktop && !windows

package main

import "fmt"

// 非 Windows 平台暂不提供桌面窗口；保留入口是为了让
// `go build -tags desktop` 在 Linux/macOS 上给出明确提示而不是编译错误。

func acquireSingleInstance() (func(), bool) { return func() {}, true }

func activateExistingWindow() {}

func confirmPortFallback(port int, cause error) bool { return false }

func dialogsAvailable() bool { return false }

func webView2Installed() bool { return false }

func fatalDialog(title, msg string) {
	fmt.Printf("%s: %s\n", title, msg)
}

type desktopTray struct{}

func startTray(onShow, onQuit func()) (*desktopTray, error) { return &desktopTray{}, nil }

func (t *desktopTray) stop() {}

func (t *desktopTray) notify(title, text string) {}

func hideMainWindow() {}

func showMainWindow() {}
