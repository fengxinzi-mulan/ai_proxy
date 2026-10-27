//go:build desktop && windows

package main

// 系统托盘直接用 Win32 实现，不引入 systray 之类的 cgo 依赖。
//
// 托盘图标挂在自建的隐藏消息窗口上，只负责「显示主界面 / 退出」两件事；
// 主窗口仍然归 Wails 管，显示与隐藏按窗口标题查找，复用单实例激活的同一套逻辑。

import (
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unicode/utf16"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	wmApp           = 0x8000
	wmTray          = wmApp + 1
	wmDestroy       = 0x0002
	wmClose         = 0x0010
	wmCommand       = 0x0111
	wmNull          = 0x0000
	wmLButtonUp     = 0x0202
	wmLButtonDblClk = 0x0203
	wmRButtonUp     = 0x0205

	nimAdd    = 0x00000000
	nimModify = 0x00000001
	nimDelete = 0x00000002

	nifMessage = 0x00000001
	nifIcon    = 0x00000002
	nifTip     = 0x00000004
	nifInfo    = 0x00000010
	niifInfo   = 0x00000001

	imageIcon      = 1
	lrDefaultSize  = 0x00000040
	lrShared       = 0x00008000
	idiApplication = 32512

	mfString    = 0x00000000
	mfSeparator = 0x00000800

	tpmRightButton = 0x0002
	tpmBottomAlign = 0x0020

	swHide    = 0
	swRestore = 9

	trayMenuShow = 1
	trayMenuQuit = 2
)

var (
	deskShell32 = windows.NewLazySystemDLL("shell32.dll")

	procGetModuleHandleW = deskKernel32.NewProc("GetModuleHandleW")
	procShellNotifyIconW = deskShell32.NewProc("Shell_NotifyIconW")
	procRegisterClassExW = deskUser32.NewProc("RegisterClassExW")
	procCreateWindowExW  = deskUser32.NewProc("CreateWindowExW")
	procDefWindowProcW   = deskUser32.NewProc("DefWindowProcW")
	procGetMessageW      = deskUser32.NewProc("GetMessageW")
	procTranslateMessage = deskUser32.NewProc("TranslateMessage")
	procDispatchMessageW = deskUser32.NewProc("DispatchMessageW")
	procPostQuitMessage  = deskUser32.NewProc("PostQuitMessage")
	procDestroyWindow    = deskUser32.NewProc("DestroyWindow")
	procPostMessageW     = deskUser32.NewProc("PostMessageW")
	procLoadImageW       = deskUser32.NewProc("LoadImageW")
	procLoadIconW        = deskUser32.NewProc("LoadIconW")
	procCreatePopupMenu  = deskUser32.NewProc("CreatePopupMenu")
	procAppendMenuW      = deskUser32.NewProc("AppendMenuW")
	procDestroyMenu      = deskUser32.NewProc("DestroyMenu")
	procTrackPopupMenu   = deskUser32.NewProc("TrackPopupMenu")
	procGetCursorPos     = deskUser32.NewProc("GetCursorPos")

	trayWndProcCB = syscall.NewCallback(trayWndProc)
	trayCurrent   *desktopTray
)

type point struct {
	X, Y int32
}

type winMsg struct {
	HWnd     uintptr
	Message  uint32
	WParam   uintptr
	LParam   uintptr
	Time     uint32
	Pt       point
	LPrivate uint32
}

type wndClassEx struct {
	CbSize        uint32
	Style         uint32
	LpfnWndProc   uintptr
	CbClsExtra    int32
	CbWndExtra    int32
	HInstance     windows.Handle
	HIcon         windows.Handle
	HCursor       windows.Handle
	HbrBackground windows.Handle
	LpszMenuName  *uint16
	LpszClassName *uint16
	HIconSm       windows.Handle
}

type notifyIconData struct {
	CbSize           uint32
	HWnd             uintptr
	UID              uint32
	UFlags           uint32
	UCallbackMessage uint32
	HIcon            uintptr
	SzTip            [128]uint16
	DwState          uint32
	DwStateMask      uint32
	SzInfo           [256]uint16
	UVersion         uint32
	SzInfoTitle      [64]uint16
	DwInfoFlags      uint32
	GuidItem         windows.GUID
	HBalloonIcon     uintptr
}

type desktopTray struct {
	mu       sync.Mutex
	hwnd     uintptr
	icon     uintptr
	onShow   func()
	onQuit   func()
	notified bool
}

// startTray 创建托盘图标并启动消息循环。失败不影响主窗口启动。
func startTray(onShow, onQuit func()) (*desktopTray, error) {
	t := &desktopTray{onShow: onShow, onQuit: onQuit}
	trayCurrent = t
	ready := make(chan uintptr, 1)
	errCh := make(chan error, 1)
	go t.run(ready, errCh)
	select {
	case hwnd := <-ready:
		t.mu.Lock()
		t.hwnd = hwnd
		t.mu.Unlock()
		return t, nil
	case err := <-errCh:
		return nil, err
	case <-time.After(3 * time.Second):
		return nil, fmt.Errorf("创建托盘窗口超时")
	}
}

func (t *desktopTray) run(ready chan<- uintptr, errCh chan<- error) {
	// 消息循环必须固定在同一个 OS 线程上。
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hInstance, _, _ := procGetModuleHandleW.Call(0)
	className, _ := windows.UTF16PtrFromString("AIProxyTrayWindow")
	wc := wndClassEx{
		CbSize:        uint32(unsafe.Sizeof(wndClassEx{})),
		LpfnWndProc:   trayWndProcCB,
		HInstance:     windows.Handle(hInstance),
		LpszClassName: className,
	}
	if atom, _, callErr := procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc))); atom == 0 {
		errCh <- fmt.Errorf("RegisterClassExW: %v", callErr)
		return
	}
	hwnd, _, callErr := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		0,
		0,
		0, 0, 0, 0,
		0, 0,
		hInstance,
		0,
	)
	if hwnd == 0 {
		errCh <- fmt.Errorf("CreateWindowExW: %v", callErr)
		return
	}

	icon := loadTrayIcon(hInstance)
	t.mu.Lock()
	t.icon = icon
	t.mu.Unlock()

	nid := notifyIconData{
		CbSize:           uint32(unsafe.Sizeof(notifyIconData{})),
		HWnd:             hwnd,
		UID:              1,
		UFlags:           nifMessage | nifIcon | nifTip,
		UCallbackMessage: wmTray,
		HIcon:            icon,
	}
	copyUTF16(nid.SzTip[:], "AI Proxy · 代理运行中")
	if ok, _, callErr := procShellNotifyIconW.Call(nimAdd, uintptr(unsafe.Pointer(&nid))); ok == 0 {
		procDestroyWindow.Call(hwnd)
		errCh <- fmt.Errorf("Shell_NotifyIcon(NIM_ADD): %v", callErr)
		return
	}
	ready <- hwnd

	var m winMsg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			return
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func loadTrayIcon(hInstance uintptr) uintptr {
	// 资源 ID 1 是 exe 里由 windres 编进去的应用图标；失败时退回系统默认图标。
	if h, _, _ := procLoadImageW.Call(hInstance, 1, imageIcon, 0, 0, lrDefaultSize|lrShared); h != 0 {
		return h
	}
	h, _, _ := procLoadIconW.Call(0, idiApplication)
	return h
}

func trayWndProc(hwnd uintptr, message uint32, wParam, lParam uintptr) uintptr {
	t := trayCurrent
	switch message {
	case wmTray:
		if t == nil {
			return 0
		}
		switch lParam {
		case wmLButtonUp, wmLButtonDblClk:
			if t.onShow != nil {
				t.onShow()
			}
		case wmRButtonUp:
			t.showMenu(hwnd)
		}
		return 0
	case wmCommand:
		if t != nil {
			switch wParam & 0xffff {
			case trayMenuShow:
				if t.onShow != nil {
					t.onShow()
				}
			case trayMenuQuit:
				if t.onQuit != nil {
					t.onQuit()
				}
			}
		}
		return 0
	case wmClose:
		procDestroyWindow.Call(hwnd)
		return 0
	case wmDestroy:
		t.removeIcon(hwnd)
		procPostQuitMessage.Call(0)
		return 0
	}
	ret, _, _ := procDefWindowProcW.Call(hwnd, uintptr(message), wParam, lParam)
	return ret
}

func (t *desktopTray) showMenu(hwnd uintptr) {
	hMenu, _, _ := procCreatePopupMenu.Call()
	if hMenu == 0 {
		return
	}
	defer procDestroyMenu.Call(hMenu)

	showText, _ := windows.UTF16PtrFromString("显示主界面")
	quitText, _ := windows.UTF16PtrFromString("退出 AI Proxy")
	procAppendMenuW.Call(hMenu, mfString, trayMenuShow, uintptr(unsafe.Pointer(showText)))
	procAppendMenuW.Call(hMenu, mfSeparator, 0, 0)
	procAppendMenuW.Call(hMenu, mfString, trayMenuQuit, uintptr(unsafe.Pointer(quitText)))

	var pt point
	procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt)))
	// 菜单弹出前先把拥有者设为前台，否则点菜单以外的地方不会自动关闭。
	procSetForegroundWindow.Call(hwnd)
	procTrackPopupMenu.Call(hMenu, tpmRightButton|tpmBottomAlign, uintptr(pt.X), uintptr(pt.Y), 0, hwnd, 0)
	procPostMessageW.Call(hwnd, wmNull, 0, 0)
}

func (t *desktopTray) removeIcon(hwnd uintptr) {
	nid := notifyIconData{
		CbSize: uint32(unsafe.Sizeof(notifyIconData{})),
		HWnd:   hwnd,
		UID:    1,
	}
	procShellNotifyIconW.Call(nimDelete, uintptr(unsafe.Pointer(&nid)))
}

// notify 首次最小化到托盘时弹一次气泡提示，避免用户以为程序退出了。
func (t *desktopTray) notify(title, text string) {
	t.mu.Lock()
	if t.notified || t.hwnd == 0 {
		t.mu.Unlock()
		return
	}
	t.notified = true
	hwnd := t.hwnd
	t.mu.Unlock()

	nid := notifyIconData{
		CbSize:      uint32(unsafe.Sizeof(notifyIconData{})),
		HWnd:        hwnd,
		UID:         1,
		UFlags:      nifInfo,
		DwInfoFlags: niifInfo,
	}
	copyUTF16(nid.SzInfoTitle[:], title)
	copyUTF16(nid.SzInfo[:], text)
	procShellNotifyIconW.Call(nimModify, uintptr(unsafe.Pointer(&nid)))
}

func (t *desktopTray) stop() {
	t.mu.Lock()
	hwnd := t.hwnd
	t.hwnd = 0
	t.mu.Unlock()
	if hwnd != 0 {
		procPostMessageW.Call(hwnd, wmClose, 0, 0)
	}
}

func copyUTF16(dst []uint16, s string) {
	u := utf16.Encode([]rune(s))
	n := copy(dst, u)
	if n < len(dst) {
		dst[n] = 0
	} else {
		dst[len(dst)-1] = 0
	}
}

func mainWindowHandle() uintptr {
	title, err := windows.UTF16PtrFromString(windowTitle)
	if err != nil {
		return 0
	}
	hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(title)))
	return hwnd
}

func hideMainWindow() {
	if hwnd := mainWindowHandle(); hwnd != 0 {
		procShowWindow.Call(hwnd, swHide)
	}
}

func showMainWindow() {
	hwnd := mainWindowHandle()
	if hwnd == 0 {
		return
	}
	procShowWindow.Call(hwnd, swRestore)
	procSetForegroundWindow.Call(hwnd)
}
