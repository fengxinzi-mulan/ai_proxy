// Package service 负责把「代理服务」从进程启动流程里独立出来。
//
// 抽出来的动机是可复用：命令行模式只需要「起服务 → 等信号 → 关服务」，
// 桌面模式需要「起服务 → 拿真实地址交给窗口 → 窗口关闭时关服务」。
// 两边共用同一套监听、路由装配与后台任务生命周期，避免两份实现漂移。
package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"ai_proxy/internal/api"
	"ai_proxy/internal/autostart"
	"ai_proxy/internal/hub"
	"ai_proxy/internal/model"
	"ai_proxy/internal/proxy"
	"ai_proxy/internal/store"
	"ai_proxy/internal/update"
)

// Options 描述一次服务启动所需的全部输入。
type Options struct {
	// Host / Port 是「期望」的监听地址，0 端口表示沿用设置里的值。
	Host string
	Port int
	// DataDir 是 SQLite 数据库所在目录。
	DataDir string
	// WebFS 是前端构建产物（web/dist）的根，Placeholder 是未构建时的提示页。
	WebFS       fs.FS
	Placeholder []byte
	Logger      *slog.Logger

	// ExtraRoutes 允许调用方在鉴权链内追加路由（桌面模式用它挂 /api/desktop/*）。
	// 传 nil 表示没有额外路由。
	ExtraRoutes func(*http.ServeMux, *store.Store)

	// DesktopMode 为 true 时，服务会把「监听失败」的原始错误交回调用方，
	// 由调用方决定是提示用户换端口还是退出。
	DesktopMode bool
	// Version 是构建版本，随 /api/meta 下发给界面显示。
	Version string
	// PreferEphemeral 为 true 时忽略 Host/Port 与设置，交给系统分配端口。
	// 桌面模式在用户选择「自动换端口」且顺序探测失败时用它兜底。
	PreferEphemeral bool

	// Autostart 描述本进程写登录启动项时用的名字与命令行。
	Autostart autostart.Spec
	// BeforeRestart / Restart 由入口注入：更新安装完成后先停服务，再拉起新进程。
	// 两者都为 nil 时，界面只能「打开下载页」，不能自动安装。
	BeforeRestart func()
	Restart       func() error
}

// Service 是一个已启动的代理服务。
type Service struct {
	store  *store.Store
	hub    *hub.Hub
	srv    *http.Server
	ln     net.Listener
	logger *slog.Logger

	host string
	port int

	// cancel 取消后后台任务与 SSE 请求都会退出；done 等它们真正结束。
	cancel context.CancelFunc
	done   chan struct{}
	once   sync.Once
}

// Start 打开数据库、装配路由并开始监听。
//
// 必须先 net.Listen 再构造 handler：这样端口冲突会在返回前暴露，
// 而不是先打印「服务已启动」再异步失败；同时真实端口可以回填给管理接口，
// 使界面展示的客户端接入地址永远与实际一致。
func Start(o Options) (*Service, error) {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	st, err := store.Open(o.DataDir)
	if err != nil {
		return nil, err
	}

	settings, err := st.GetSettings()
	if err != nil {
		st.Close()
		return nil, err
	}
	// 命令行 / 调用方传入的值优先于库里的设置，方便临时换端口调试。
	if o.Host != "" {
		settings.Host = o.Host
	}
	if o.Port != 0 {
		settings.Port = o.Port
	}
	settings.ApplyDefaults()

	ln, err := net.Listen("tcp", net.JoinHostPort(settings.Host, fmt.Sprint(settings.Port)))
	if err != nil && o.PreferEphemeral {
		// 顺序探测失败后退回系统分配：端口只要可用就行，不写回设置。
		ln, err = net.Listen("tcp", net.JoinHostPort(settings.Host, "0"))
	}
	if err != nil {
		st.Close()
		if o.DesktopMode {
			return nil, &ListenError{Host: settings.Host, Port: settings.Port, Err: err}
		}
		return nil, fmt.Errorf("监听 %s 失败: %w", settings.Host, err)
	}
	actualPort := ln.Addr().(*net.TCPAddr).Port

	h := hub.New()
	proxySrv := proxy.NewServer(st, h, o.Logger)
	apiSrv := api.New(st, h, proxySrv, o.Logger, settings.Host, actualPort)
	apiSrv.SetBuildInfo(o.Version, o.DesktopMode)
	apiSrv.SetAutostart(o.Autostart)

	// 更新管理器：检测走全局代理设置，这样墙内也能查到新版本。
	updater := update.New(update.Options{
		CurrentVersion: o.Version,
		Desktop:        o.DesktopMode,
		Store:          st,
		Hub:            h,
		Logger:         o.Logger,
		ClientFactory: func() (*http.Client, error) {
			cur, err := st.GetSettings()
			if err != nil {
				return nil, err
			}
			return proxy.NewHTTPClient(cur.Proxy, 15*time.Second)
		},
		BeforeRestart: o.BeforeRestart,
		Restart:       o.Restart,
	})
	apiSrv.SetUpdater(updater)

	// protected 覆盖除静态资源之外的一切：/api/* 是管理接口，其余路径转发给上游。
	//
	// 静态资源（管理界面的 bundle）刻意不要求密钥 —— 否则一旦设置了访问密钥，
	// 连界面本身都打不开，也就没有任何地方能让你输入密钥。界面本身不含任何
	// 隐私数据，真正的数据都在 /api 后面。
	protected := http.NewServeMux()
	apiSrv.Register(protected)
	if o.ExtraRoutes != nil {
		o.ExtraRoutes(protected, st)
	}

	// 未命中的 /api/ 路径要明确报错，绝不能落到转发逻辑里被送到上游。
	protected.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"error":"接口不存在"}`))
	})
	protected.Handle("/", proxySrv)

	root, err := newStaticHandler(o.WebFS, o.Placeholder, apiSrv.Auth(protected), o.Logger)
	if err != nil {
		ln.Close()
		st.Close()
		return nil, err
	}

	// 根 context：取消它等于「应用正在退出」。
	// 挂到 BaseContext 上，SSE handler 里 r.Context() 会立刻结束，
	// 不用等 10 秒 Shutdown 超时。
	rootCtx, cancel := context.WithCancel(context.Background())
	srv := &http.Server{
		Handler: root,
		// 不设 WriteTimeout：AI 流式响应动辄几分钟，写超时会直接掐断正常请求。
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return rootCtx },
	}

	s := &Service{
		store:  st,
		hub:    h,
		srv:    srv,
		ln:     ln,
		logger: o.Logger,
		host:   settings.Host,
		port:   actualPort,
		cancel: cancel,
		done:   make(chan struct{}),
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		runLogCleanup(rootCtx, st, o.Logger)
	}()
	go func() {
		defer wg.Done()
		proxy.NewUsageRefresher(st, proxySrv, o.Logger).Run(rootCtx)
	}()
	// 启动后的自动检查也要计入后台任务组：它读过数据库、写过 KV，
	// 不纳入 wg 的话 Shutdown 会在它还在跑的时候就把库关掉。
	go func() {
		defer wg.Done()
		checkUpdatesOnStartup(rootCtx, updater, o.Logger)
	}()
	go func() {
		wg.Wait()
		close(s.done)
	}()

	go func() {
		printBanner(settings, actualPort, o.Logger)
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			o.Logger.Error("HTTP 服务异常退出", "err", err)
		}
	}()

	return s, nil
}

// 启动后延迟多久做首次更新检查：先把服务与界面拉起来，别让一次网络请求
// 拖慢启动。失败只记 debug —— 离线、被墙、限流都是常态。
const (
	startupUpdateDelay   = 8 * time.Second
	startupUpdateTimeout = 30 * time.Second
)

// checkUpdatesOnStartup 是启动时的一次性自动检查，受最小间隔节流。
func checkUpdatesOnStartup(ctx context.Context, updater *update.Manager, logger *slog.Logger) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(startupUpdateDelay):
	}
	checkCtx, cancel := context.WithTimeout(ctx, startupUpdateTimeout)
	defer cancel()
	if _, err := updater.Check(checkCtx, false); err != nil {
		logger.Debug("启动时检查更新失败", "err", err)
	}
}

// ListenError 表示端口监听失败，桌面模式据此区分「端口被占用」和别的错误。
type ListenError struct {
	Host string
	Port int
	Err  error
}

func (e *ListenError) Error() string {
	return fmt.Sprintf("监听 %s:%d 失败: %v", e.Host, e.Port, e.Err)
}

func (e *ListenError) Unwrap() error { return e.Err }

// Host 返回实际监听的地址。
func (s *Service) Host() string { return s.host }

// Port 返回实际监听的端口（可能不同于设置里写的值）。
func (s *Service) Port() int { return s.port }

// URL 返回窗口/浏览器应当访问的地址。
//
// 监听通配地址时不能把 0.0.0.0 交给浏览器，统一折回回环地址。
// 主机部分必须经 net.JoinHostPort 拼装：IPv6 字面量不带方括号时
// "http://::1:8080/" 是无效 URL，窗口会直接打不开。
func (s *Service) URL() string {
	return "http://" + net.JoinHostPort(displayHost(s.host), strconv.Itoa(s.port)) + "/"
}

// Store 暴露底层存储，桌面模式读写桌面配置时需要。
func (s *Service) Store() *store.Store { return s.store }

// SubscriberCount 返回管理界面当前的 SSE 订阅者数量。
//
// 桌面壳用它判断「关闭确认框能不能真的送到用户眼前」：没有订阅者时
// 广播出去的事件没人接收，继续阻止关闭只会让窗口永久卡住。
func (s *Service) SubscriberCount() int {
	if s.hub == nil {
		return 0
	}
	return s.hub.SubscriberCount()
}

// Broadcast 把事件推给管理界面的 SSE 订阅者。
//
// 桌面壳用它通知前端弹出关闭确认框；服务端模式下没有调用方，行为与普通事件一致。
func (s *Service) Broadcast(eventType string, data any) {
	if s.hub != nil {
		s.hub.Broadcast(eventType, data)
	}
}

// Shutdown 停掉后台任务与 HTTP 服务，并等待它们真正结束。
//
// 顺序很重要：先 cancel 根 context，让 SSE 与后台循环退出，
// 再 Shutdown 等普通请求收尾；超时后退回 Close 强制断开。
func (s *Service) Shutdown(ctx context.Context) error {
	var err error
	s.once.Do(func() {
		s.cancel()
		err = s.srv.Shutdown(ctx)
		if err != nil {
			_ = s.srv.Close()
		}
		// 后台任务最多再等 2 秒；它们都监听 ctx，正常应当立刻返回。
		select {
		case <-s.done:
		case <-time.After(2 * time.Second):
		}
		if cerr := s.store.Close(); cerr != nil && err == nil {
			err = cerr
		}
	})
	return err
}

// 自动回收空间的触发门槛。
//
// 保留策略每小时删一批行，但 DELETE 只是把页面挂进 freelist，文件不会变小；
// 空闲页少的时候重建整库不划算，所以只在「删得多、空闲比例高」时动一次，
// 且一天最多一次 —— VACUUM 期间独占连接（MaxOpenConns=1），不能太频繁。
const (
	autoVacuumMinFreePages int64 = 1000
	autoVacuumMinFreeRatio       = 0.2
	autoVacuumInterval           = 24 * time.Hour
)

// runLogCleanup 按保留策略定期清理历史日志。
func runLogCleanup(ctx context.Context, st *store.Store, logger *slog.Logger) {
	// 启动后先等一会儿再清理，避免和启动阶段抢数据库。
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()

	var lastVacuum time.Time

	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}

		settings, err := st.GetSettings()
		if err == nil {
			if n, err := st.CleanupLogs(settings.RetentionDays, settings.MaxLogs); err != nil {
				logger.Warn("清理历史日志失败", "err", err)
			} else if n > 0 {
				logger.Info("已清理历史日志", "count", n)
				maybeAutoVacuum(st, logger, &lastVacuum)
			}
		}
		timer.Reset(time.Hour)
	}
}

// maybeAutoVacuum 在清理之后判断要不要顺手回收磁盘。
func maybeAutoVacuum(st *store.Store, logger *slog.Logger, last *time.Time) {
	if time.Since(*last) < autoVacuumInterval {
		return
	}
	stats, err := st.PageStats()
	if err != nil {
		logger.Warn("读取数据库页统计失败", "err", err)
		return
	}
	if stats.FreeCount < autoVacuumMinFreePages || stats.FreeRatio < autoVacuumMinFreeRatio {
		return
	}
	reclaimed, err := st.Vacuum()
	if err != nil {
		logger.Warn("清理后回收数据库空间失败", "err", err)
		return
	}
	*last = time.Now()
	if reclaimed > 0 {
		logger.Info("已回收数据库空间", "reclaimedBytes", reclaimed, "freeRatio", stats.FreeRatio)
	}
}

// printBanner 打印启动信息，把最常用的几个地址直接给出来。
func printBanner(settings model.Settings, actualPort int, logger *slog.Logger) {
	base := "http://" + net.JoinHostPort(displayHost(settings.Host), strconv.Itoa(actualPort))
	logger.Info("服务已启动",
		"管理界面", base+"/",
		"代理入口", base+"/v1/…",
		"按名指定供应商", base+"/p/<供应商短名>/v1/…",
		"监听", listenAddr(settings.Host, actualPort),
	)
	if settings.Host != "127.0.0.1" && settings.Host != "localhost" && settings.AccessKey == "" {
		logger.Warn("正在监听非本机地址但未设置访问密钥，同网段的任何人都能使用本代理")
	}
}

// displayHost 把「不该原样交给浏览器」的监听地址折回回环地址。
//
// 空串、"0.0.0.0"、"::" 都是「监听所有网卡」，浏览器访问它们没有意义；
// 已经带方括号的 IPv6 字面量（用户手填）去掉括号，交给 JoinHostPort 重新加。
func displayHost(host string) string {
	host = strings.TrimSpace(host)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	}
	switch host {
	case "", "0.0.0.0", "::", "*":
		return "127.0.0.1"
	}
	return host
}

// listenAddr 把真实监听主机与端口拼成可读地址。
//
// 与 displayHost 不同，它保留通配地址（"::" 要打印成 "[::]:port" 而不是
// 折回收环地址）——这里展示的是「实际监听在哪」。IPv6 的方括号由
// JoinHostPort 统一补，用户手填的 "[::1]" 必须先脱括号，否则会变成
// "[[::1]]:8080" 这种非法地址。
func listenAddr(host string, port int) string {
	host = strings.TrimSpace(host)
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

// staticHandler 负责把前端资源与其余请求分发到正确的处理器。
//
// 分发规则只有一条：请求路径能对应到嵌入资源里的真实文件就返回该文件，
// 否则交给 fallback（已经套好鉴权的管理接口 + 转发入口）。这样无需维护路径白名单 ——
// /v1/chat/completions、/v1beta/models/… 之类都会被正确转发。
type staticHandler struct {
	fileServer http.Handler
	fallback   http.Handler
	fsys       fs.FS
	// built 为 false 时说明前端还没构建，根路径改吐提示页而不是去请求上游。
	built       bool
	placeholder []byte
}

func newStaticHandler(webFS fs.FS, placeholder []byte, fallback http.Handler, logger *slog.Logger) (*staticHandler, error) {
	if webFS == nil {
		return nil, errors.New("未提供前端资源")
	}
	// 有真实的 index.html 才算前端已构建成功。
	_, statErr := fs.Stat(webFS, "index.html")
	built := statErr == nil
	if !built {
		logger.Warn("前端资源未构建，根路径将显示提示页；代理转发功能不受影响")
	}
	return &staticHandler{
		fileServer:  http.FileServer(http.FS(webFS)),
		fallback:    fallback,
		fsys:        webFS,
		built:       built,
		placeholder: placeholder,
	}, nil
}

func (s *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.built && (r.URL.Path == "/" || r.URL.Path == "/index.html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(s.placeholder)
		return
	}
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		if name, ok := s.resolveAsset(r.URL.Path); ok {
			// Vite 产出的资源文件名自带内容哈希，可以长期缓存；
			// 而 index.html 引用的是那些哈希名，必须每次校验 ——
			// 否则升级二进制之后，浏览器会拿旧 index.html 去请求已经被删掉的
			// 旧资源，直接白屏。
			if name == "index.html" {
				w.Header().Set("Cache-Control", "no-cache")
			} else {
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			}
			s.fileServer.ServeHTTP(w, r)
			return
		}
	}
	s.fallback.ServeHTTP(w, r)
}

// resolveAsset 判断请求路径是否对应一个真实的嵌入资源。
func (s *staticHandler) resolveAsset(urlPath string) (string, bool) {
	name := strings.TrimPrefix(urlPath, "/")
	if name == "" {
		return "index.html", true
	}
	// 目录请求不做重定向，直接交给转发逻辑，避免把上游的 /v1/ 当成目录。
	info, err := fs.Stat(s.fsys, name)
	if err != nil || info.IsDir() {
		return "", false
	}
	return name, true
}
