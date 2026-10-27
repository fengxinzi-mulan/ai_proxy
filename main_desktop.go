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
	"sync/atomic"
	"syscall"
	"testing/fstest"
	"time"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
	"github.com/wailsapp/wails/v2/pkg/options/windows"
	"github.com/wailsapp/wails/v2/pkg/runtime"

	"ai_proxy/internal/desktop"
	"ai_proxy/internal/logging"
	"ai_proxy/internal/service"
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
	defer release()

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
	logger.Info("桌面版启动", "version", version.Version, "dataDir", dataDir)

	if !webView2Installed() {
		// 不阻断启动：Wails 自己会给出更准确的错误，这里只提前提示原因。
		logger.Warn("未检测到 WebView2 Runtime，窗口可能无法创建")
	}

	web, err := fs.Sub(distFS, "web/dist")
	if err != nil {
		fatalDialog("读取前端资源失败", err.Error())
		return
	}

	app := &desktopApp{
		logger:  logger,
		dataDir: dataDir,
		host:    *host,
		port:    *port,
		web:     web,
	}

	if err := app.launch(); err != nil {
		logger.Error("桌面窗口启动失败", "err", err)
		fatalDialog("启动失败", err.Error())
	}
}

// desktopApp 持有窗口生命周期内需要的全部状态。
type desktopApp struct {
	logger  *slog.Logger
	dataDir string
	host    string
	port    int
	web     fs.FS

	svc  *service.Service
	ctx  context.Context
	tray *desktopTray

	// quitting 表示用户已经确认退出，此时 OnBeforeClose 必须放行。
	quitting atomic.Bool
	// closePromptOpen 表示关闭确认框已经推给前端，避免重复点击关闭时重复弹窗。
	closePromptOpen atomic.Bool
	// shuttingDown 防止 OnShutdown 重入。
	shuttingDown atomic.Bool
}

// launch 启动 HTTP 服务与窗口。
func (a *desktopApp) launch() error {
	svc, err := a.startService(false)
	if err != nil {
		var le *service.ListenError
		if errors.As(err, &le) && dialogsAvailable() && confirmPortFallback(le.Port, le.Err) {
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
	if err != nil {
		return err
	}
	a.svc = svc
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
			a.ctx = ctx
			tray, err := startTray(showMainWindow, a.quit)
			if err != nil {
				a.logger.Warn("创建系统托盘失败", "err", err)
			} else {
				a.tray = tray
			}
		},
		OnBeforeClose: func(ctx context.Context) bool {
			return !a.handleClose(ctx)
		},
		OnShutdown: func(context.Context) {
			if a.tray != nil {
				a.tray.stop()
			}
			a.shutdown()
		},
	})
	if err != nil {
		return err
	}
	// wails.Run 返回即窗口已关闭；确保服务也停了。
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
	})
}

// handleClose 处理窗口关闭请求，返回 true 表示允许退出。
//
// 语义按用户配置：
//   - 直接退出：放行；
//   - 托盘运行：隐藏窗口并阻止退出，代理继续跑；
//   - 每次询问（默认）：通知前端弹出现代化确认框，用户选择后再执行。
func (a *desktopApp) handleClose(ctx context.Context) bool {
	if a.quitting.Load() {
		return true
	}
	if a.ctx == nil {
		// 窗口还没就绪就收到关闭请求（例如启动失败），直接放行。
		return true
	}
	cfg := desktop.LoadConfig(a.dataDir)
	switch cfg.CloseAction {
	case desktop.CloseQuit:
		a.quitting.Store(true)
		return true
	case desktop.CloseTray:
		a.minimizeToTray()
		return false
	}

	// 每次询问：把弹窗交给前端（NModal + 图标），在用户做出选择前阻止关闭。
	if !a.closePromptOpen.CompareAndSwap(false, true) {
		return false
	}
	if a.svc == nil {
		a.closePromptOpen.Store(false)
		return false
	}
	a.svc.Broadcast("desktop-close-ask", nil)
	return false
}

// minimizeToTray 隐藏主窗口，代理服务保持运行。
func (a *desktopApp) minimizeToTray() {
	hideMainWindow()
	if a.tray != nil {
		a.tray.notify("AI Proxy", "已最小化到系统托盘，代理继续在后台运行。双击托盘图标可重新打开。")
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
	if a.ctx != nil {
		runtime.Quit(a.ctx)
	}
}

// shutdown 停掉代理服务，幂等。
func (a *desktopApp) shutdown() {
	if !a.shuttingDown.CompareAndSwap(false, true) || a.svc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.svc.Shutdown(ctx); err != nil {
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
