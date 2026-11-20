package logging

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newTestWriter(t *testing.T) (*rotatingWriter, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ai_proxy.log")
	w, err := newRotatingWriter(path)
	if err != nil {
		t.Fatalf("打开日志文件失败: %v", err)
	}
	return w, path
}

// Bug 41：Rename 失败一次之后日志不能永久瘫痪。
//
// 修复前的实现先 Close 再 Rename，Rename 失败直接 return，w.f 还指着一个
// 已关闭的文件 —— 之后每次 Write 都报 "file already closed"，只能重启进程。
func TestRotateRenameFailureKeepsLogging(t *testing.T) {
	w, path := newTestWriter(t)
	defer w.Close()

	// 用目录占住 .1 与 .2：备份移位（.1 → .2）会先失败，当前文件改名
	// （path → .1）也因为同名目录存在而失败 —— 这才是真正的「Rename 失败」路径。
	// 只占住 .1 时，移位会把目录挪到 .2，随后改名反而成功，测的是正常轮转。
	for _, target := range []string{path + ".1", path + ".2"} {
		if err := os.Mkdir(target, 0o755); err != nil {
			t.Fatalf("准备冲突目录失败: %v", err)
		}
	}
	// 直接把体积标记推到阈值，省得真写 5 MB。
	w.mu.Lock()
	w.size = maxFileSize
	w.mu.Unlock()

	if _, err := w.Write([]byte("first\n")); err != nil {
		t.Fatalf("轮转失败后仍应能继续写日志: %v", err)
	}
	// 关键断言：第二次写入必须也能成功（修复前这里是永久失败）。
	if _, err := w.Write([]byte("second\n")); err != nil {
		t.Fatalf("第二次写入失败（日志永久瘫痪）: %v", err)
	}
	// 失败后 size 要归零：否则下一条日志会立刻重试整套轮转，
	// 占用持续时每条日志都付一次「关闭 + 4 次改名」的开销。
	w.mu.Lock()
	sizeAfter := w.size
	w.mu.Unlock()
	if sizeAfter >= maxFileSize {
		t.Errorf("轮转失败后 size 应回落到阈值以下，实际 %d", sizeAfter)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取日志失败: %v", err)
	}
	if !strings.Contains(string(raw), "second") {
		t.Errorf("日志内容缺失，实际 %q", raw)
	}
}

// 正常轮转：旧内容进 .1，新文件从空开始。
func TestRotateCreatesBackupAndNewFile(t *testing.T) {
	w, path := newTestWriter(t)
	if _, err := w.Write([]byte("old\n")); err != nil {
		t.Fatal(err)
	}
	w.mu.Lock()
	w.size = maxFileSize
	w.mu.Unlock()
	if _, err := w.Write([]byte("new\n")); err != nil {
		t.Fatalf("轮转后写入失败: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("关闭日志失败: %v", err)
	}

	backup, err := os.ReadFile(path + ".1")
	if err != nil {
		t.Fatalf("轮转没有生成 .1 备份: %v", err)
	}
	if !strings.Contains(string(backup), "old") {
		t.Errorf("备份文件应保留旧内容，实际 %q", backup)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(current), "old") {
		t.Errorf("轮转后的新文件不该含旧内容，实际 %q", current)
	}
	if !strings.Contains(string(current), "new") {
		t.Errorf("新内容没有写入，实际 %q", current)
	}
}

// Close 之后不应再有人写；此时必须返回错误而不是解引用 nil 句柄。
func TestWriteAfterCloseReturnsError(t *testing.T) {
	w, _ := newTestWriter(t)
	if err := w.Close(); err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if _, err := w.Write([]byte("x")); err == nil {
		t.Fatal("Close 后写入应返回错误")
	}
	// 重复 Close 必须安全（New 的 closer 可能被调用多次）。
	if err := w.Close(); err != nil {
		t.Errorf("重复 Close 不该报错: %v", err)
	}
}
