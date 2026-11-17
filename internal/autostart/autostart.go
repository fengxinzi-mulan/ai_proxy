// Package autostart 管理「登录时自动启动」这一项系统级设置。
//
// 设计取舍：状态只存在系统里（Windows 是注册表 Run 键），不在数据库里留副本。
// 两份状态一定会分叉 —— 用户在任务管理器里关掉了启动项、或者 exe 被挪走，
// 库里的「已开启」就成了谎话。界面读到的永远是系统里的真实现状。
package autostart

import (
	"os"
	"path/filepath"
	"strings"
)

// Backend 是平台相关的启动项读写实现。
//
// 做成接口而不是直接调注册表，是为了让测试能指到一个临时键，
// 不至于每跑一次单测就往用户真实的启动项里塞一条。
type Backend interface {
	// Supported 报告当前平台能否管理启动项；不能时给出给用户看的原因。
	Supported() (bool, string)
	// Get 读取某个名字当前注册的命令；不存在时 found=false。
	Get(name string) (command string, found bool, err error)
	// Set 写入或覆盖某个名字的启动命令。
	Set(name, command string) error
	// Delete 删除某个名字的启动项；本来就不存在时不算错误。
	Delete(name string) error
}

// Spec 描述「本进程以什么身份写入启动项」。
type Spec struct {
	// Name 是启动项的名字，两种形态各用一个，互不覆盖。
	Name string
	// Command 是完整命令行（已含引号与 -autostart）。
	Command string
	// Reason 非空表示当前形态不支持自启，内容直接下发给界面显示。
	Reason string

	Backend Backend
}

// System 返回当前平台的启动项后端。
func System() Backend { return systemBackend() }

// CommandLine 把 exe 与参数拼成一条可直接写进启动项的命令行。
//
// exe 一律加引号：Program Files 下的路径必然带空格，不加引号 Windows 会把
// 空格前那一段当成可执行文件名，启动直接失败。参数同理，只在含空格时才加，
// 保持可读性。
func CommandLine(exe string, args []string) string {
	var b strings.Builder
	b.WriteString(quoteIfNeeded(exe))
	for _, a := range args {
		b.WriteString(" ")
		b.WriteString(quoteIfNeeded(a))
	}
	return b.String()
}

func quoteIfNeeded(s string) string {
	if s == "" {
		return `""`
	}
	if strings.ContainsAny(s, " \t") && !strings.HasPrefix(s, `"`) {
		return `"` + s + `"`
	}
	return s
}

// IsUsableExecutable 判断这个 exe 路径是否适合写进启动项。
//
// `go run` 会把二进制丢进临时目录，路径随每次运行变化，写进启动项等于
// 留下一条指向垃圾的启动命令；开发调试形态直接拒绝，并告诉用户原因。
func IsUsableExecutable(exe string) (bool, string) {
	if exe == "" {
		return false, "无法确定可执行文件路径"
	}
	dir := strings.ToLower(filepath.Dir(exe))
	temp := strings.ToLower(os.TempDir())
	if temp != "" && strings.HasPrefix(dir, temp) {
		return false, "当前是 go run 调试形态（可执行文件在临时目录），不支持开机自启"
	}
	// go-build 的临时目录在部分环境里不在 os.TempDir() 下，再兜一层。
	if strings.Contains(dir, "go-build") {
		return false, "当前是 go run 调试形态（可执行文件在临时目录），不支持开机自启"
	}
	return true, ""
}

// State 是下发给界面的启动项状态。
type State struct {
	// Supported 为 false 时 Reason 说明原因，界面据此禁用开关。
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
	// Enabled 表示系统里确实存在这条启动项。
	Enabled bool `json:"enabled"`
	// Name / Command 是本进程「应该」写入的值。
	Name    string `json:"name"`
	Command string `json:"command"`
	// Registered 是系统里当前实际注册的命令；Enabled=false 时为空。
	Registered string `json:"registered"`
	// Stale 表示已开启但注册的命令与本进程当前应有的命令不一致
	// （典型场景：把 exe 挪到了别的目录，启动项还指着旧路径）。
	Stale bool `json:"stale"`
}

// Read 读取当前状态。
func (s Spec) Read() State {
	st := State{Name: s.Name, Command: s.Command}
	if s.Reason != "" || s.Backend == nil {
		st.Reason = s.Reason
		if st.Reason == "" {
			st.Reason = "当前形态不支持开机自启"
		}
		return st
	}
	ok, reason := s.Backend.Supported()
	if !ok {
		st.Reason = reason
		return st
	}
	st.Supported = true
	cmd, found, err := s.Backend.Get(s.Name)
	if err != nil {
		st.Reason = "读取启动项失败: " + err.Error()
		st.Supported = false
		return st
	}
	st.Enabled = found
	st.Registered = cmd
	st.Stale = found && cmd != s.Command
	return st
}

// Set 打开或关闭启动项，返回操作后的状态。
func (s Spec) Set(enabled bool) (State, error) {
	if s.Reason != "" || s.Backend == nil {
		return s.Read(), ErrUnsupported
	}
	if ok, reason := s.Backend.Supported(); !ok {
		return s.Read(), &UnsupportedError{Reason: reason}
	}
	var err error
	if enabled {
		err = s.Backend.Set(s.Name, s.Command)
	} else {
		err = s.Backend.Delete(s.Name)
	}
	if err != nil {
		return s.Read(), err
	}
	return s.Read(), nil
}

// UnsupportedError 表示当前平台/形态不支持自启，Reason 可直接展示给用户。
type UnsupportedError struct{ Reason string }

func (e *UnsupportedError) Error() string { return e.Reason }

// ErrUnsupported 是没有具体原因的「不支持」。
var ErrUnsupported = &UnsupportedError{Reason: "当前形态不支持开机自启"}
