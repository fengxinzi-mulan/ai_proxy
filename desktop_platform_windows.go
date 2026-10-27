//go:build desktop && windows

package main

import (
	"fmt"
	"net"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

// 单实例与窗口激活全部用 Win32 直接实现，而不是 Wails 的 SingleInstanceLock。
//
// 原因是 Wails 的锁在 wails.Run 内部才生效，那时端口已经绑好了 ——
// 第二个实例会先撞上「端口被占用」，用户看到的原因完全不对。

// mutexName 是命名互斥量，换值会让新旧版本互不识别。
const mutexName = `Local\ai_proxy-desktop-8f4c1d2e`

// windowTitle 必须与 options.App.Title 完全一致，激活窗口靠它查找。
const windowTitle = "AI Proxy"

// webView2GUID 是 Evergreen WebView2 Runtime 在注册表里的固定标识。
const webView2GUID = `{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`

var (
	deskKernel32      = windows.NewLazySystemDLL("kernel32.dll")
	deskUser32        = windows.NewLazySystemDLL("user32.dll")
	procCreateMutexW  = deskKernel32.NewProc("CreateMutexW")
	procFindWindowW   = deskUser32.NewProc("FindWindowW")
	procShowWindow    = deskUser32.NewProc("ShowWindow")
	procSetForegroundWindow = deskUser32.NewProc("SetForegroundWindow")
	procIsIconic      = deskUser32.NewProc("IsIconic")
	procMessageBoxW   = deskUser32.NewProc("MessageBoxW")
)

// acquireSingleInstance 尝试拿到单实例锁。
//
// 返回的 release 必须在进程退出时调用；first 为 false 表示已有实例在跑。
func acquireSingleInstance() (release func(), first bool) {
	name, err := windows.UTF16PtrFromString(mutexName)
	if err != nil {
		return func() {}, true
	}
	h, _, callErr := procCreateMutexW.Call(0, 0, uintptr(unsafe.Pointer(name)))
	// CreateMutexW 失败时返回 0；此时不阻止启动，宁可多开也不要打不开。
	if h == 0 {
		return func() {}, true
	}
	// ERROR_ALREADY_EXISTS 说明互斥量已存在 —— 已有实例在运行。
	const errorAlreadyExists = syscall.Errno(183)
	if errno, ok := callErr.(syscall.Errno); ok && errno == errorAlreadyExists {
		_ = windows.CloseHandle(windows.Handle(h))
		return func() {}, false
	}
	return func() { _ = windows.CloseHandle(windows.Handle(h)) }, true
}

// activateExistingWindow 把已运行实例的窗口叫到前台。
func activateExistingWindow() {
	title, err := windows.UTF16PtrFromString(windowTitle)
	if err != nil {
		return
	}
	hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(title)))
	if hwnd == 0 {
		return
	}
	// 最小化时先还原（SW_RESTORE=9），否则直接显示（SW_SHOW=5）。
	if iconic, _, _ := procIsIconic.Call(hwnd); iconic != 0 {
		procShowWindow.Call(hwnd, 9)
	} else {
		procShowWindow.Call(hwnd, 5)
	}
	procSetForegroundWindow.Call(hwnd)
}

// confirmPortFallback 在窗口尚未创建时询问是否自动换端口。
//
// 这里不能用 Wails 的 MessageDialog：它要求窗口生命周期上下文，
// 而此时 wails.Run 还没被调用，传任何 context 都会报 invalid context。
func confirmPortFallback(port int, cause error) bool {
	msg := fmt.Sprintf(
		"端口 %d 已被其它程序占用。\n\n是否自动改用其它可用端口启动？\n界面上的客户端接入地址会显示实际端口。\n\n原始错误：%v",
		port, cause)
	return messageBoxYesNo("端口被占用", msg)
}

// messageBoxYesNo 弹一个「是/否」原生对话框，是=true。
func messageBoxYesNo(title, msg string) bool {
	const (
		mbYesNo        = 0x00000004
		mbIconQuestion = 0x00000020
		idYes          = 6
	)
	t, err1 := windows.UTF16PtrFromString(title)
	m, err2 := windows.UTF16PtrFromString(msg)
	if err1 != nil || err2 != nil {
		return false
	}
	ret, _, _ := procMessageBoxW.Call(0,
		uintptr(unsafe.Pointer(m)),
		uintptr(unsafe.Pointer(t)),
		uintptr(mbYesNo|mbIconQuestion))
	return int(ret) == idYes
}

// dialogsAvailable 在桌面模式下恒为 true：原生弹窗不依赖窗口已创建。
func dialogsAvailable() bool { return true }

// fatalDialog 在窗口尚未创建时给出最后的可见提示。
func fatalDialog(title, msg string) {
	const mbIconError = 0x00000010
	t, err1 := windows.UTF16PtrFromString(title)
	m, err2 := windows.UTF16PtrFromString(msg)
	if err1 != nil || err2 != nil {
		return
	}
	procMessageBoxW.Call(0,
		uintptr(unsafe.Pointer(m)),
		uintptr(unsafe.Pointer(t)),
		mbIconError)
}

// webView2Installed 检查机器上是否装了 WebView2 Runtime。
func webView2Installed() bool {
	paths := []string{
		`SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\` + webView2GUID,
		`SOFTWARE\Microsoft\EdgeUpdate\Clients\` + webView2GUID,
	}
	for _, p := range paths {
		if keyHasVersion(registry.LOCAL_MACHINE, p) {
			return true
		}
	}
	return keyHasVersion(registry.CURRENT_USER, `Software\Microsoft\EdgeUpdate\Clients\`+webView2GUID)
}

func keyHasVersion(root registry.Key, path string) bool {
	k, err := registry.OpenKey(root, path, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	v, _, err := k.GetStringValue("pv")
	if err != nil {
		return false
	}
	// 空版本号或 "0.0.0.0" 表示只登记了组件但没装运行时。
	return v != "" && v != "0.0.0.0"
}

// findFreePort 从 start 起尝试 count 个端口，返回第一个可绑定的端口。
// 找不到时返回 0，调用方据此退回系统分配。
func findFreePort(host string, start, count int) int {
	for p := start; p < start+count && p <= 65535; p++ {
		ln, err := net.Listen("tcp", net.JoinHostPort(host, fmt.Sprint(p)))
		if err == nil {
			_ = ln.Close()
			return p
		}
	}
	return 0
}
