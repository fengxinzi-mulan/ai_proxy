//go:build !windows

package main

// useUTF8Console 在非 Windows 平台上是空操作：
// 终端默认就是 UTF-8，不需要额外处理。
func useUTF8Console() {}
