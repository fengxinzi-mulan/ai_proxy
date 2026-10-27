//go:build windows

package desktop

import (
	"os/exec"
	"syscall"
)

// openInFileManager 用资源管理器打开目录。
func openInFileManager(dir string) error {
	cmd := exec.Command("explorer.exe", dir)
	// explorer.exe 打开目录后不会自己退出，等它会挂住调用方，所以不等。
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
