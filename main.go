// ai_proxy 是一个面向 AI 模型供应商的透明代理转发程序。
//
// 它做三件事：
//  1. 把请求原样转发到上游供应商，只按用户显式开启的项目做最小改写；
//  2. 旁路解析响应，记录每次请求的 token、首 token 耗时、TPS、状态等指标；
//  3. 提供一个嵌入在二进制里的 Web 管理界面。
//
// 编译产物是单个可执行文件：前端资源通过 go:embed 打进二进制，无需额外部署。
package main

import (
	"context"
	"embed"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"ai_proxy/internal/api"
	"ai_proxy/internal/hub"
	"ai_proxy/internal/model"
	"ai_proxy/internal/proxy"
	"ai_proxy/internal/store"
)

// distFS 嵌入前端构建产物。
//
// 用 all: 前缀是为了把以 _ 或 . 开头的文件也一并打包（Vite 偶尔会产出这类资源）。
// 只嵌 web/dist 而不是 web/ 全部内容，构建产物之外的源码不进二进制。
//
//go:embed all:web/dist
var distFS embed.FS

// placeholderHTML 是前端尚未构建时展示的提示页。
//
// 单独嵌入一个仓库内的固定文件，而不是在 web/dist 里放占位 index.html ——
// 后者会被前端构建覆盖，变成「改了但没提交」的噪声。
//
//go:embed web/placeholder/index.html
var placeholderHTML []byte

func main() {
	var (
		host    = flag.String("host", "", "监听地址，留空则使用设置里的值（默认 127.0.0.1）")
		port    = flag.Int("port", 0, "监听端口，留空则使用设置里的值（默认 8080）")
		dataDir = flag.String("data", "data", "数据目录，存放 SQLite 数据库")
		debug   = flag.Bool("debug", false, "输出调试日志")
	)
	flag.Parse()

	// 先把控制台切成 UTF-8，否则中文日志在 Windows 的 cmd 里是乱码。
	useUTF8Console()

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	if err := run(*host, *port, *dataDir, logger); err != nil {
		logger.Error("启动失败", "err", err)
		os.Exit(1)
	}
}

func run(hostFlag string, portFlag int, dataDir string, logger *slog.Logger) error {
	st, err := store.Open(dataDir)
	if err != nil {
		return err
	}
	defer st.Close()

	settings, err := st.GetSettings()
	if err != nil {
		return err
	}
	// 命令行参数优先于库里的设置，方便临时换端口调试。
	if hostFlag != "" {
		settings.Host = hostFlag
	}
	if portFlag != 0 {
		settings.Port = portFlag
	}
	settings.ApplyDefaults()

	h := hub.New()
	proxySrv := proxy.NewServer(st, h, logger)
	// 把实际生效的监听地址交给管理接口，界面据此展示客户端接入地址。
	apiSrv := api.New(st, h, proxySrv, logger, settings.Host, settings.Port)

	// protected 覆盖除静态资源之外的一切：/api/* 是管理接口，其余路径转发给上游。
	//
	// 静态资源（管理界面的 bundle）刻意不要求密钥 —— 否则一旦设置了访问密钥，
	// 连界面本身都打不开，也就没有任何地方能让你输入密钥。界面本身不含任何
	// 隐私数据，真正的数据都在 /api 后面。
	protected := http.NewServeMux()
	apiSrv.Register(protected)

	// 未命中的 /api/ 路径要明确报错，绝不能落到转发逻辑里被送到上游。
	protected.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.WriteHeader(http.StatusNotFound)
		io.WriteString(w, `{"error":"接口不存在"}`)
	})
	protected.Handle("/", proxySrv)

	root, err := newStaticHandler(apiSrv.Auth(protected), logger)
	if err != nil {
		return err
	}

	addr := fmt.Sprintf("%s:%d", settings.Host, settings.Port)
	srv := &http.Server{
		Addr:    addr,
		Handler: root,
		// 不设 WriteTimeout：AI 流式响应动辄几分钟，写超时会直接掐断正常请求。
		ReadHeaderTimeout: 30 * time.Second,
		IdleTimeout:       120 * time.Second,
		// SSE 心跳依赖连接不被复用池关掉，交给 IdleTimeout 控制即可。
	}

	cleanupCtx, stopCleanup := context.WithCancel(context.Background())
	defer stopCleanup()
	go runLogCleanup(cleanupCtx, st, logger)
	// 用量的定时刷新挂在同一个「进程生命周期」上下文上：它和日志清理一样，
	// 是常驻后台任务，进程退出时一起停。
	go proxy.NewUsageRefresher(st, proxySrv, logger).Run(cleanupCtx)

	errCh := make(chan error, 1)
	go func() {
		printBanner(settings, logger)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			errCh <- err
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-errCh:
		return err
	case <-stop:
		logger.Info("收到退出信号，正在关闭服务…")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}

// printBanner 打印启动信息，把最常用的几个地址直接给出来。
func printBanner(settings model.Settings, logger *slog.Logger) {
	base := fmt.Sprintf("http://%s:%d", displayHost(settings.Host), settings.Port)
	logger.Info("服务已启动",
		"管理界面", base+"/",
		"代理入口", base+"/v1/…",
		"按名指定供应商", base+"/p/<供应商短名>/v1/…",
		"监听", settings.Host,
	)
	if settings.Host != "127.0.0.1" && settings.Host != "localhost" && settings.AccessKey == "" {
		logger.Warn("正在监听非本机地址但未设置访问密钥，同网段的任何人都能使用本代理")
	}
}

func displayHost(host string) string {
	if host == "0.0.0.0" || host == "" {
		return "127.0.0.1"
	}
	return host
}

// runLogCleanup 按保留策略定期清理历史日志。
func runLogCleanup(ctx context.Context, st *store.Store, logger *slog.Logger) {
	// 启动后先等一会儿再清理，避免和启动阶段抢数据库。
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()

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
			}
		}
		timer.Reset(time.Hour)
	}
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
	built bool
}

func newStaticHandler(fallback http.Handler, logger *slog.Logger) (*staticHandler, error) {
	sub, err := fs.Sub(distFS, "web/dist")
	if err != nil {
		return nil, fmt.Errorf("读取嵌入的前端资源失败: %w", err)
	}
	// 有真实的 index.html 才算前端已构建成功。
	_, statErr := fs.Stat(sub, "index.html")
	built := statErr == nil
	if !built {
		logger.Warn("前端资源未构建，根路径将显示提示页；代理转发功能不受影响")
	}
	return &staticHandler{
		fileServer: http.FileServer(http.FS(sub)),
		fallback:   fallback,
		fsys:       sub,
		built:      built,
	}, nil
}

func (s *staticHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.built && (r.URL.Path == "/" || r.URL.Path == "/index.html") {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(placeholderHTML)
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
