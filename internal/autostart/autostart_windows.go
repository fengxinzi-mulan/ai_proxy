//go:build windows

package autostart

import (
	"errors"

	"golang.org/x/sys/windows/registry"
)

// runKeyPath 是「当前用户登录时运行」的标准位置。
//
// 用 CURRENT_USER 而不是 LOCAL_MACHINE：写 HKCU 不需要管理员权限，
// 而且「我这个用户登录时启动」本来就是正确的语义 —— 换个人登录不该
// 自动跑起别人的代理。
const runKeyPath = `Software\Microsoft\Windows\CurrentVersion\Run`

// registryBackend 是注册表实现。keyPath 可注入，测试指向临时键。
type registryBackend struct {
	keyPath string
}

func systemBackend() Backend { return &registryBackend{keyPath: runKeyPath} }

func (b *registryBackend) Supported() (bool, string) { return true, "" }

func (b *registryBackend) Get(name string) (string, bool, error) {
	k, err := registry.OpenKey(registry.CURRENT_USER, b.keyPath, registry.QUERY_VALUE)
	if err != nil {
		// 键不存在等价于「没有任何启动项」，不是错误。
		if errors.Is(err, registry.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	defer k.Close()

	v, _, err := k.GetStringValue(name)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return "", false, nil
		}
		return "", false, err
	}
	return v, true, nil
}

func (b *registryBackend) Set(name, command string) error {
	// CREATE_KEY 允许键不存在时自动创建（全新用户配置里 Run 键可能还没建）。
	k, _, err := registry.CreateKey(registry.CURRENT_USER, b.keyPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	return k.SetStringValue(name, command)
}

func (b *registryBackend) Delete(name string) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, b.keyPath, registry.SET_VALUE)
	if err != nil {
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}
	defer k.Close()

	if err := k.DeleteValue(name); err != nil && !errors.Is(err, registry.ErrNotExist) {
		return err
	}
	return nil
}
