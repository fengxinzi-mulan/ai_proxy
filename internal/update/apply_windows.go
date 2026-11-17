//go:build windows

package update

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Apply 用暂存文件替换正在运行的可执行文件。
//
// Windows 的规则很关键：**运行中的 exe 不允许被覆盖，但允许被改名**。
// 所以流程是「把旧的改名挪开 → 把新的放到原路径 → 失败就把旧的改回来」。
// 三步里任何一步失败都回到原状，不会留下一个半截的安装。
//
// 被挪开的旧文件不能就地删（此刻还锁着），交给下次启动的 CleanupOld。
func Apply(exePath, stagedFile string) error {
	if exePath == "" || stagedFile == "" {
		return fmt.Errorf("缺少可执行文件路径")
	}
	if _, err := os.Stat(stagedFile); err != nil {
		return fmt.Errorf("暂存文件不可用: %w", err)
	}

	// 暂存文件在数据目录、exe 可能在另一个盘时，os.Rename 会返回
	// ERROR_NOT_SAME_DEVICE（MoveFileEx 不跨卷）。先把文件搬到 exe 同目录，
	// 保证后面的 rename 是同卷操作。
	staged := stagedFile
	if !sameVolume(stagedFile, exePath) {
		local := exePath + ".new"
		if err := copyFile(stagedFile, local); err != nil {
			return fmt.Errorf("无法把升级包复制到程序目录（可能没有写权限）: %w", err)
		}
		staged = local
	}

	backup := exePath + ".old-" + fmt.Sprint(time.Now().Unix())

	if err := os.Rename(exePath, backup); err != nil {
		if staged != stagedFile {
			os.Remove(staged)
		}
		return fmt.Errorf("无法移动旧版本（可能没有写权限）: %w", err)
	}
	if err := os.Rename(staged, exePath); err != nil {
		// 回滚：把旧文件放回原位，让程序仍能按原路径启动。
		if rbErr := os.Rename(backup, exePath); rbErr != nil {
			return fmt.Errorf("写入新版本失败: %w（回滚也失败: %v，请手动把 %s 改名为 %s）",
				err, rbErr, backup, filepath.Base(exePath))
		}
		if staged != stagedFile {
			os.Remove(staged)
		}
		return fmt.Errorf("写入新版本失败: %w", err)
	}
	return nil
}

// sameVolume 判断两个路径是否在同一个盘上。
//
// 只比较盘符/卷名：Windows 上跨卷 rename 一定失败，同卷则一定能 rename。
func sameVolume(a, b string) bool {
	va := filepath.VolumeName(a)
	vb := filepath.VolumeName(b)
	return strings.EqualFold(va, vb)
}

// copyFile 复制文件并刷盘，保证 rename 之前数据真的落盘。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		os.Remove(dst)
		return err
	}
	return out.Close()
}

// CleanupOld 清理上次更新留下的 .old-* 文件。
//
// 这些文件在替换当时还锁着删不掉，等进程退出后才可删；
// 所以在启动时扫一遍。删不掉就留着，下次再试 —— 绝不能因此影响启动。
func CleanupOld(exePath string) {
	if exePath == "" {
		return
	}
	dir := filepath.Dir(exePath)
	base := filepath.Base(exePath)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base+".old-") {
			continue
		}
		_ = os.Remove(filepath.Join(dir, name))
	}
}

// Restart 用同样的参数拉起新进程。
//
// 参数必须原样带上：桌面版自启时带着 -autostart，服务端版可能带着
// -data 绝对路径，丢了任何一个，新进程的行为就和原来不一样了。
//
// 刻意**不设 cmd.Dir**：exec.Command 默认继承父进程的工作目录，而这正是
// 参数语义的基准 —— 用户从 D:\work 执行 `ai_proxy-server.exe -data data`，
// 重启后新进程的 `-data data` 必须仍解析到 D:\work\data。若把 CWD 改成
// exe 所在目录，相对路径会静默指向另一个位置，用户看到的是「更新后数据没了」。
func Restart(exePath string, args []string) error {
	cmd := exec.Command(exePath, args...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动新版本失败: %w", err)
	}
	// 不 Wait：新进程要独立存活，当前进程随后退出。
	_ = cmd.Process.Release()
	return nil
}
