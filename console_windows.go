//go:build windows

package main

import (
	"syscall"
	"unsafe"
)

var (
	kernel32            = syscall.NewLazyDLL("kernel32.dll")
	procGetConsoleMode  = kernel32.NewProc("GetConsoleMode")
	procSetConsoleCP    = kernel32.NewProc("SetConsoleCP")
	procSetConsoleOutCP = kernel32.NewProc("SetConsoleOutputCP")
)

// useUTF8Console 把 Windows 控制台的输入输出代码页切成 UTF-8。
//
// 默认的 cmd.exe 用 GBK 代码页，直接写 UTF-8 字节会显示成乱码。
// 这里只在 stdout 确实连着控制台时切换，输出被重定向到文件或管道时不动 ——
// 那种情况下字节本来就是给程序读的，改代码页没有意义还可能影响父进程的终端。
func useUTF8Console() {
	var mode uint32
	if r, _, _ := procGetConsoleMode.Call(uintptr(syscall.Stdout), uintptr(unsafe.Pointer(&mode))); r == 0 {
		return
	}
	const utf8CodePage = 65001
	procSetConsoleCP.Call(utf8CodePage)
	procSetConsoleOutCP.Call(utf8CodePage)
}
