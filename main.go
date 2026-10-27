//go:build !desktop

// ai_proxy 是一个面向 AI 模型供应商的透明代理转发程序。
//
// 它做三件事：
//  1. 把请求原样转发到上游供应商，只按用户显式开启的项目做最小改写；
//  2. 旁路解析响应，记录每次请求的 token、首 token 耗时、TPS、状态等指标；
//  3. 提供一个嵌入在二进制里的 Web 管理界面。
//
// 编译产物是单个可执行文件：前端资源通过 go:embed 打进二进制，无需额外部署。
//
// 本文件是「命令行 / 服务器」入口；桌面版入口见 main_desktop.go（build tag: desktop）。
package main

import (
	"context"
	"embed"
	"flag"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"ai_proxy/internal/logging"
	"ai_proxy/internal/service"
	"ai_proxy/internal/version"
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

// webRoot 返回嵌入的 web/dist 子文件系统。
func webRoot() (fs.FS, error) {
	return fs.Sub(distFS, "web/dist")
}

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
	logger, closer := logging.New(*dataDir, level)
	defer closer.Close()

	if err := run(*host, *port, *dataDir, logger); err != nil {
		logger.Error("启动失败", "err", err)
		os.Exit(1)
	}
}

func run(hostFlag string, portFlag int, dataDir string, logger *slog.Logger) error {
	web, err := webRoot()
	if err != nil {
		return err
	}

	s, err := service.Start(service.Options{
		Host:        hostFlag,
		Port:        portFlag,
		DataDir:     dataDir,
		WebFS:       web,
		Placeholder: placeholderHTML,
		Logger:      logger,
		Version:     version.Version,
	})
	if err != nil {
		return err
	}

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(stop)

	<-stop
	logger.Info("收到退出信号，正在关闭服务…")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return s.Shutdown(ctx)
}
