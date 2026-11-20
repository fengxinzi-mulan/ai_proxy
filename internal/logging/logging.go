// Package logging 提供带轮转的文件日志。
//
// 桌面版用 -H windowsgui 构建后没有控制台，stdout 会被丢弃；
// 没有文件日志就等于所有故障都不可见，因此这里是最低限度的排障入口。
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
)

// 轮转阈值与保留份数。日志只用于排障，不需要保留太久。
const (
	maxFileSize = 5 << 20 // 5 MB
	maxBackups  = 5
)

// New 构造一个同时写 stdout 和 <dataDir>/logs/ai_proxy.log 的 logger。
//
// 目录或文件不可写时退回只写 stdout，并且不报错 —— 日志系统自身故障
// 不应该阻止代理启动。返回的 io.Closer 用于进程退出前刷盘。
func New(dataDir string, level slog.Level) (*slog.Logger, io.Closer) {
	stdout := slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: level})

	dir := filepath.Join(dataDir, "logs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return slog.New(stdout), nopCloser{}
	}

	w, err := newRotatingWriter(filepath.Join(dir, "ai_proxy.log"))
	if err != nil {
		return slog.New(stdout), nopCloser{}
	}

	fileHandler := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	return slog.New(teeHandler{a: stdout, b: fileHandler}), w
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

// teeHandler 把同一条日志分发给两个 handler。
type teeHandler struct {
	a, b slog.Handler
}

func (t teeHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return t.a.Enabled(ctx, l) || t.b.Enabled(ctx, l)
}

func (t teeHandler) Handle(ctx context.Context, r slog.Record) error {
	var first error
	if t.a.Enabled(ctx, r.Level) {
		if err := t.a.Handle(ctx, r); err != nil {
			first = err
		}
	}
	if t.b.Enabled(ctx, r.Level) {
		if err := t.b.Handle(ctx, r); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (t teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return teeHandler{a: t.a.WithAttrs(attrs), b: t.b.WithAttrs(attrs)}
}

func (t teeHandler) WithGroup(name string) slog.Handler {
	return teeHandler{a: t.a.WithGroup(name), b: t.b.WithGroup(name)}
}

// rotatingWriter 是只追加的文件日志，超过阈值时把文件轮转为 .1 … .N。
type rotatingWriter struct {
	mu     sync.Mutex
	path   string
	f      *os.File
	size   int64
	closed bool
}

func newRotatingWriter(path string) (*rotatingWriter, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &rotatingWriter{path: path, f: f, size: info.Size()}, nil
}

func (w *rotatingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		// Close 之后不应再有写入；返回错误而不是 panic（nil 指针解引用）。
		return 0, os.ErrClosed
	}
	if w.f == nil {
		// 上次轮转/重开失败留下的空句柄：先尝试重新打开追加，而不是让日志
		// 永久停摆到进程重启（一次瞬时占用不该造成不可恢复的故障）。
		if err := w.reopen(false); err != nil {
			return 0, err
		}
	}
	if w.size+int64(len(p)) > maxFileSize {
		if err := w.rotate(); err != nil {
			return 0, err
		}
	}
	n, err := w.f.Write(p)
	w.size += int64(n)
	return n, err
}

func (w *rotatingWriter) rotate() error {
	// 先把旧文件关掉并立刻把句柄置 nil，再动文件。
	//
	// 之前是先 Close 再 Rename，Rename 失败就 return 了，w.f 还指着一个
	// 已经关闭的 *os.File —— 之后每次 Write 都返回 "file already closed"，
	// 日志从此永久瘫痪（而且是个只能重启才能恢复的故障）。
	if w.f != nil {
		_ = w.f.Close()
		w.f = nil
	}
	// ai_proxy.log.4 → .5，…，.1 → .2，最后把当前文件改名为 .1。
	// 中间这些失败只影响备份份数，不影响写入，忽略即可。
	for i := maxBackups - 1; i >= 1; i-- {
		from := fmt.Sprintf("%s.%d", w.path, i)
		to := fmt.Sprintf("%s.%d", w.path, i+1)
		_ = os.Rename(from, to)
	}
	if err := os.Rename(w.path, w.path+".1"); err != nil && !os.IsNotExist(err) {
		// 改名失败（Windows 上常见于杀毒软件/索引服务短暂占用文件）：
		// 退回追加原文件，而不是让日志永久停摆。
		if rerr := w.reopen(false); rerr != nil {
			return rerr
		}
		// reopen 会把 size 设成真实文件大小（已超过阈值）。不归零的话，
		// 下一条日志立刻又走一遍「关闭 + 4 次备份改名 + 当前文件改名 + 重开」，
		// 占用持续时每条日志都付这份代价。归零等于「再攒满一个文件的量才重试」。
		w.size = 0
		return nil
	}
	return w.reopen(true)
}

// reopen 重新打开日志文件。truncate 为 true 用于轮转后的新文件，
// false 用于「轮转失败、继续追加原文件」。
func (w *rotatingWriter) reopen(truncate bool) error {
	flags := os.O_CREATE | os.O_WRONLY | os.O_APPEND
	if truncate {
		flags = os.O_CREATE | os.O_WRONLY | os.O_TRUNC
	}
	f, err := os.OpenFile(w.path, flags, 0o644)
	if err != nil {
		return err
	}
	info, err := f.Stat()
	if err != nil {
		f.Close()
		return err
	}
	w.f = f
	w.size = info.Size()
	return nil
}

func (w *rotatingWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	f := w.f
	w.f = nil
	w.size = 0
	w.closed = true
	if f == nil {
		return nil
	}
	return f.Close()
}
