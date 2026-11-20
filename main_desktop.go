//go:build desktop && windows

// 桌面版入口（Windows）。
//
// 窗口加载的不是 Wails 的 AssetServer，而是进程内已经跑起来的本地 HTTP 服务：
// 现有前端用相对 /api、EventSource SSE 与绝对 /assets 路径，只有同源加载
// 才能全部零改动工作。内嵌的 bootstrap 页只负责一次 location.replace 跳转。
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"sync/atomic"
	"syscall"
	"testing/fstest"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"ai_proxy/internal/autostart"
	"ai_proxy/internal/desktop"
	"ai_proxy/internal/logging"
	"ai_proxy/internal/service"
	"ai_proxy/internal/update"
	"ai_proxy/internal/version"
	// 空引入：把图标与版本信息链接进桌面版 exe。服务端入口不引这个包。
	_ "ai_proxy/internal/winres"
)

//go:embed all:web/dist
var distFS embed.FS

//go:embed web/placeholder/index.html
var placeholderHTML []byte

func main() {
	var (
		host    = flag.String("host", "", "监听地址，留空则使用设置里的值（默认 127.0.0.1）")
		port    = flag.Int("port", 0, "监听端口，留空则使用设置里的值（默认 8080）")
		dataArg = flag.String("data", "", "数据目录；留空时优先沿用 exe 同目录的 data，其次用户配置目录")
		debug   = flag.Bool("debug", false, "输出调试日志")
		// 由启动项带上的标记：窗口不显示，直接收进托盘。
		fromAutostart = flag.Bool("autostart", false, "由开机自启动拉起，启动后隐藏到托盘")
	)
	flag.Parse()

	useUTF8Console()

	// 单实例检查必须放在一切初始化之前：否则第二个实例会先尝试绑定端口，
	// 用户看到的会是「端口被占用」而不是「程序已在运行」。
	release, first := acquireSingleInstance()
	if !first {
		activateExistingWindow()
		return
	}
	// 自动更新重启时必须先放掉互斥量，否则新进程会被判成「已有实例在运行」
	// 然后立刻退出 —— 用户看到的就是「更新完程序不见了」。
	// 用只生效一次的包装：重启路径提前调用，defer 再调一次不能有副作用。
	releaseLock := releaseSingleInstance(release)
	defer releaseLock()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}

	dataSet := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == "data" {
			dataSet = true
		}
	})
	dataDir, err := service.ResolveDataDir(*dataArg, dataSet, true)
	if err != nil {
		fatalDialog("无法确定数据目录", err.Error())
		return
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		fatalDialog("无法创建数据目录", err.Error())
		return
	}

	logger, closer := logging.New(dataDir, level)
	defer closer.Close()
	logger.Info("桌面版启动", "version", version.Version, "dataDir", dataDir, "autostart", *fromAutostart)

	if !webView2Installed() {
		// 不阻断启动：Wails 自己会给出更准确的错误，这里只提前提示原因。
		logger.Warn("未检测到 WebView2 Runtime，窗口可能无法创建")
	}

	web, err := fs.Sub(distFS, "web/dist")
	if err != nil {
		fatalDialog("读取前端资源失败", err.Error())
		return
	}

	// 上次更新留下的 .old-* 文件此刻已解锁，顺手清掉。
	if exe, err := os.Executable(); err == nil {
		update.CleanupOld(exe)
	}

	app := &desktopApp{
		logger:      logger,
		dataDir:     dataDir,
		host:        *host,
		port:        *port,
		web:         web,
		autostart:   *fromAutostart,
		releaseLock: releaseLock,
	}

	if err := app.launch(); err != nil {
		logger.Error("桌面窗口启动失败", "err", err)
		fatalDialog("启动失败", err.Error())
	}
}

// releaseSingleInstance 把互斥量释放包装成幂等函数。
//
// 释放点有两个：自动更新拉起新进程之前（必须提前放），以及 main 的 defer。
// 两个都必须能安全调用，而 Windows 上重复 CloseHandle 会返回错误句柄，
// 所以统一收敛到 sync.Once。
func releaseSingleInstance(release func()) func() {
	var once sync.Once
	return func() { once.Do(release) }
}

// desktopApp 持有窗口生命周期内需要的全部状态。
type desktopApp struct {
	logger  *slog.Logger
	dataDir string
	host    string
	port    int
	web     fs.FS
	// autostart 表示本次是被登录启动项拉起的：窗口不显示，直接收进托盘。
	autostart bool

	// releaseLock 释放单实例互斥量。可以重复调用（内部用 sync.Once 包过），
	// 自动更新拉起新进程前必须先调用它。
	releaseLock func()

	// mu 保护 svc / ctx / tray。
	//
	// 这几项会被不同 goroutine 同时碰到：Wails 的回调在 UI 线程，
	// 关闭确认来自 HTTP 处理协程，更新安装在另一个 goroutine，
	// 托盘回调又在自己的消息循环里。不加锁就会出现「关闭流程读到一半 svc 被置空」
	// 或者两个 goroutine 同时 stop 同一个托盘。
	//
	// 约定：持有 mu 时只做字段读写，绝不调用 svc/tray 上的方法 ——
	// 那些方法可能反过来触发关闭回调，持锁就会死锁。
	mu   sync.Mutex
	svc  *service.Service
	ctx  context.Context
	tray *desktopTray

	// quitting 表示用户已经确认退出，此时 OnBeforeClose 必须放行。
	quitting atomic.Bool
	// closePromptOpen 表示关闭确认框已经推给前端，避免重复点击关闭时重复弹窗。
	closePromptOpen atomic.Bool
	// shuttingDown 防止 OnShutdown 重入。
	shuttingDown atomic.Bool
	// restarting 表示自动更新正在收摊/拉起新进程：这段时间禁止关窗，
	// 否则进程先退出、新进程永远不会被拉起。见 prepareRestart。
	restarting atomic.Bool
}

// getService / getContext / getTray 在锁内取快照，避免读字段与写字段竞态。
func (a *desktopApp) getService() *service.Service {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.svc
}

func (a *desktopApp) getContext() context.Context {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.ctx
}

func (a *desktopApp) getTray() *desktopTray {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.tray
}

func (a *desktopApp) setService(svc *service.Service) {
	a.mu.Lock()
	a.svc = svc
	a.mu.Unlock()
}

func (a *desktopApp) setContext(ctx context.Context) {
	a.mu.Lock()
	a.ctx = ctx
	a.mu.Unlock()
}

func (a *desktopApp) setTray(t *desktopTray) {
	a.mu.Lock()
	a.tray = t
	a.mu.Unlock()
}

// detachRuntime 一次性摘走服务与托盘，后续的关闭动作在锁外做。
func (a *desktopApp) detachRuntime() (*desktopTray, *service.Service) {
	a.mu.Lock()
	defer a.mu.Unlock()
	tray, svc := a.tray, a.svc
	a.tray, a.svc = nil, nil
	return tray, svc
}

// stopTray 撤掉托盘图标（如果还在）。
func (a *desktopApp) stopTray() {
	if tray := a.getTray(); tray != nil {
		tray.stop()
	}
}

// launch 启动 HTTP 服务与窗口。
func (a *desktopApp) launch() error {
	// wails.Run 失败（WebView2 缺失、窗口创建失败）时 launch 会直接返回错误，
	// 调用方只弹个对话框就退出。没有这层 defer，服务与数据库连接就泄漏在进程里，
	// 表现为端口被占、下次启动报「已在运行」。
	defer a.shutdown()

	svc, err := a.startService(false)
	if err != nil {
		var le *service.ListenError
		if errors.As(err, &le) {
			switch {
			case a.autostart:
				// 自启场景不弹原生确认框：登录时没人盯着屏幕，弹窗只会把
				// 进程挂在那里等一个永远不会来的点击。直接交给系统分配端口。
				a.logger.Info("自启启动时端口被占用，改由系统分配", "port", le.Port)
				svc, err = a.startService(true)
			case dialogsAvailable() && confirmPortFallback(le.Port, le.Err):
				// 先顺序探测：换一个固定端口，客户端配置只要改一次就能长期用；
				// 探测不到再交给系统分配（端口每次启动都会变，属于最后手段）。
				if free := findFreePort(le.Host, le.Port+1, 20); free != 0 {
					a.port = free
					a.logger.Info("端口被占用，改用顺序探测到的端口", "port", free)
					svc, err = a.startService(false)
				} else {
					a.logger.Info("顺序探测未找到空端口，改由系统分配")
					svc, err = a.startService(true)
				}
			}
		}
	}
	if err != nil {
		return err
	}
	a.setService(svc)
	a.logger.Info("代理服务已就绪", "url", svc.URL())

	target := svc.URL()
	bootstrap := fstest.MapFS{
		"index.html": &fstest.MapFile{
			// meta refresh 是 JS 被禁用或 CSP 拦截时的兜底；
			// 正常情况下 location.replace 更快且不会在历史里留下空页。
			Data: []byte(fmt.Sprintf(
				`<!doctype html><html><head><meta charset="utf-8">`+
					`<meta http-equiv="refresh" content="0;url=%s">`+
					`<title>AI Proxy</title></head><body style="margin:0;background:#0f1115">`+
					`<script>location.replace(%q)</script></body></html>`, target, target)),
		},
	}

	go a.watchSignals()

	err = wails.Run(&options.App{
		Title:     "AI Proxy",
		Width:     1440,
		Height:    900,
		MinWidth:  1000,
		MinHeight: 680,
		// 自启拉起时不显示窗口：用户登录后不该被一个弹窗拦住。
		// Wails 在 StartHidden 下仍会正常加载 WebView，托盘双击照样能唤出。
		StartHidden: a.autostart,
		AssetServer: &assetserver.Options{
			Assets: bootstrap,
		},
		// 沿用系统标题栏：这是「原生桌面工具风」的一部分。
		Frameless: false,
		BackgroundColour: &options.RGBA{
			R: 15, G: 17, B: 21, A: 255,
		},
		Windows: &windows.Options{
			Theme:            windows.SystemDefault,
			DisablePinchZoom: true,
		},
		OnStartup: func(ctx context.Context) {
			a.setContext(ctx)
			tray, err := startTray(showMainWindow, a.quit)
			if err != nil {
				a.logger.Warn("创建系统托盘失败", "err", err)
				// 托盘没建起来就没有任何入口能唤出窗口了。自启场景下必须显式
				// 把窗口显示出来，否则用户会得到一个「看不见但占着端口」的进程。
				if a.autostart {
					runtime.WindowShow(ctx)
				}
			} else {
				a.setTray(tray)
			}
		},
		OnBeforeClose: func(ctx context.Context) bool {
			return !a.handleClose(ctx)
		},
		OnShutdown: func(context.Context) {
			// 先撤托盘再关服务：托盘图标在新进程创建自己的图标之前必须消失，
			// 否则会留下一个点不动的幽灵图标。
			a.stopTray()
			a.shutdown()
		},
	})
	if err != nil {
		return err
	}
	// wails.Run 返回即窗口已关闭；确保服务也停了（defer 里还有一层，幂等）。
	a.shutdown()
	return nil
}

// startService 启动代理服务，DesktopMode 下监听失败会返回 *service.ListenError。
func (a *desktopApp) startService(ephemeral bool) (*service.Service, error) {
	api := desktop.NewDesktopAPI(a.dataDir, a.applyCloseChoice)
	return service.Start(service.Options{
		Host:            a.host,
		Port:            a.port,
		DataDir:         a.dataDir,
		WebFS:           a.web,
		Placeholder:     placeholderHTML,
		Logger:          a.logger,
		Version:         version.Version,
		DesktopMode:     true,
		PreferEphemeral: ephemeral,
		ExtraRoutes:     api.Register,
		Autostart:       a.autostartSpec(),
		BeforeRestart:   a.prepareRestart,
		Restart:         a.restartNewVersion,
	})
}

// autostartSpec 描述桌面形态的启动项。
//
// -data 只在用户显式传过时才写进启动项：留空时 exe 会按「同级 data →
// %APPDATA%」重新解析，把数据目录焊死在命令行里反而会让用户挪动 exe 后失效。
func (a *desktopApp) autostartSpec() autostart.Spec {
	const name = "AIProxy-Desktop"
	exe, err := os.Executable()
	if err != nil {
		return autostart.Spec{Name: name, Reason: "无法确定可执行文件路径"}
	}
	if ok, reason := autostart.IsUsableExecutable(exe); !ok {
		return autostart.Spec{Name: name, Reason: reason}
	}

	args := []string{"-autostart"}
	flag.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "host":
			args = append(args, "-host", f.Value.String())
		case "port":
			args = append(args, "-port", f.Value.String())
		case "data":
			// 必须转绝对路径：用户很可能写 `-data data`，而登录时的 CWD 是
			// System32，相对路径会让新进程在系统目录下建一个空库 ——
			// 表现就是「自启之后供应商和日志全没了」。
			if v := f.Value.String(); v != "" {
				if abs, err := filepath.Abs(v); err == nil {
					args = append(args, "-data", abs)
				} else {
					args = append(args, "-data", v)
				}
			}
		}
	})

	return autostart.Spec{
		Name:    name,
		Command: autostart.CommandLine(exe, args),
		Backend: autostart.System(),
	}
}

// prepareRestart 在替换 exe 之前收摊：放单实例锁、停托盘图标、关服务。
//
// 顺序不能改：先放互斥量，再拉起新进程。新进程启动第一件事就是抢同一把锁，
// 锁还在的话它会被判成「已有实例」直接退出，更新看上去就是程序凭空消失。
//
// 托盘图标必须先撤掉：新进程会创建自己的图标，旧进程不撤会留下一个
// 点不动的幽灵图标，直到用户把鼠标划过去才消失。
func (a *desktopApp) prepareRestart() {
	// 从这一刻到新进程拉起之间，服务与托盘都已经被收掉。若用户此时点关闭，
	// 窗口会正常退出，安装线程随进程一起消失 —— 新版本写进了磁盘却没有
	// 被拉起，看起来就是「更新完程序不见了」。所以这个窗口期内禁止关窗。
	a.restarting.Store(true)
	if a.releaseLock != nil {
		a.releaseLock()
	}
	tray, svc := a.detachRuntime()
	if tray != nil {
		tray.stop()
	}
	if svc != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := svc.Shutdown(ctx); err != nil {
			a.logger.Warn("更新前关闭代理服务出错", "err", err)
		}
	}
}

// restartNewVersion 用同样的参数拉起新进程，然后让当前进程退出。
func (a *desktopApp) restartNewVersion() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	// 再放一次锁兜底（幂等）：任何绕开 prepareRestart 的调用路径都不能
	// 让新进程卡在「已有实例」上。
	if a.releaseLock != nil {
		a.releaseLock()
	}
	if err := update.Restart(exe, os.Args[1:]); err != nil {
		// 拉起失败：解除关窗保护，否则窗口永远关不掉（服务已经关了）。
		a.restarting.Store(false)
		return err
	}
	a.quitting.Store(true)
	// 服务与托盘已经在 prepareRestart 里收掉了，直接退出即可。
	os.Exit(0)
	return nil
}

// handleClose 处理窗口关闭请求，返回 true 表示允许退出。
//
// 语义按用户配置：
//   - 直接退出：放行；
//   - 托盘运行：隐藏窗口并阻止退出，代理继续跑；
//   - 每次询问（默认）：通知前端弹出现代化确认框，用户选择后再执行。
//
// 所有「没法把询问送到用户眼前」的情况都必须放行，否则窗口永远关不掉。
func (a *desktopApp) handleClose(ctx context.Context) bool {
	if a.quitting.Load() {
		return true
	}
	if a.restarting.Load() {
		// 更新正在收摊：放行会让进程先退出，新版本起不来。
		// restartNewVersion 拉起失败时会清掉这个标记，窗口不会永久关不掉。
		a.logger.Info("自动更新重启中，暂不关闭窗口")
		return false
	}
	if a.getContext() == nil {
		// 窗口还没就绪就收到关闭请求（例如启动失败），直接放行。
		return true
	}
	cfg := desktop.LoadConfig(a.dataDir)
	switch cfg.CloseAction {
	case desktop.CloseQuit:
		a.quitting.Store(true)
		return true
	case desktop.CloseTray:
		if a.getTray() == nil {
			// 没有托盘就没有任何入口能把窗口叫回来，躲进托盘等于让程序
			// 变成「看不见也关不掉」的僵尸；直接放行退出。
			a.logger.Warn("关闭配置为最小化到托盘，但托盘不可用，直接退出")
			a.quitting.Store(true)
			return true
		}
		a.minimizeToTray()
		return false
	}

	svc := a.getService()
	if svc == nil {
		// 服务已经收摊（更新安装会先关服务）或从未起来：确认框依赖 SSE，
		// 没有服务就没有通道，阻止关闭只会让窗口永久卡住。
		a.logger.Warn("关闭时代理服务已不可用，直接退出窗口")
		a.quitting.Store(true)
		return true
	}
	if svc.SubscriberCount() == 0 {
		// 没有任何 SSE 订阅者（例如前端崩溃、页面被关掉）意味着确认框发不出去。
		// 这里不占用 closePromptOpen，直接退出，避免「点关闭毫无反应」。
		a.logger.Info("关闭时没有界面订阅者，直接退出窗口")
		a.quitting.Store(true)
		return true
	}

	// 每次询问：把弹窗交给前端（NModal + 图标），在用户做出选择前阻止关闭。
	if !a.closePromptOpen.CompareAndSwap(false, true) {
		return false
	}
	svc.Broadcast("desktop-close-ask", nil)
	return false
}

// minimizeToTray 隐藏主窗口，代理服务保持运行。
func (a *desktopApp) minimizeToTray() {
	hideMainWindow()
	if tray := a.getTray(); tray != nil {
		tray.notify("AI Proxy", "已最小化到系统托盘，代理继续在后台运行。双击托盘图标可重新打开。")
	}
}

// applyCloseChoice 接收前端关闭确认框的结果。cancel 只释放弹窗占用。
func (a *desktopApp) applyCloseChoice(action string) {
	a.closePromptOpen.Store(false)
	switch action {
	case string(desktop.CloseQuit):
		a.quit()
	case string(desktop.CloseTray):
		a.minimizeToTray()
	}
}

// quit 由托盘菜单和系统信号调用。先置位，避免 OnBeforeClose 再次询问。
func (a *desktopApp) quit() {
	a.quitting.Store(true)
	if ctx := a.getContext(); ctx != nil {
		runtime.Quit(ctx)
	}
}

// shutdown 停掉代理服务，幂等。
func (a *desktopApp) shutdown() {
	if !a.shuttingDown.CompareAndSwap(false, true) {
		return
	}
	// 摘走引用再关：prepareRestart 可能已经把 svc 收走了，两边不能都关一次。
	a.mu.Lock()
	svc := a.svc
	a.svc = nil
	a.mu.Unlock()
	if svc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := svc.Shutdown(ctx); err != nil {
		a.logger.Warn("关闭代理服务时出错", "err", err)
	}
	a.logger.Info("桌面版已退出")
}

// watchSignals 把系统信号转成窗口退出。
func (a *desktopApp) watchSignals() {
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	a.quit()
}
