//go:build !windows

package update

import "errors"

// 非 Windows 平台不自动替换可执行文件：Linux/macOS 上程序通常由包管理器
// 或 systemd 管理，替用户改二进制是越界的。界面会退回「打开下载页」。

// Apply 在非 Windows 上一律拒绝。
func Apply(string, string) error {
	return errors.New("当前平台不支持自动安装，请手动下载")
}

// CleanupOld 在非 Windows 上没有需要清理的中间产物。
func CleanupOld(string) {}

// Restart 在非 Windows 上不自动重启。
func Restart(string, []string) error {
	return errors.New("当前平台不支持自动重启")
}
