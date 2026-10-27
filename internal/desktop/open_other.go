//go:build !windows

package desktop

import "os/exec"

// openInFileManager 用 xdg-open 打开目录（Linux 预留给后续桌面支持）。
func openInFileManager(dir string) error {
	return exec.Command("xdg-open", dir).Start()
}
