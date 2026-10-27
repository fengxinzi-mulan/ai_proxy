package service

import (
	"os"
	"path/filepath"
)

// ResolveDataDir 决定数据目录的最终位置。
//
// 桌面模式与命令行模式的默认行为不同，原因是「当前工作目录」对快捷方式启动
// 完全不可控（可能落在 System32）：桌面版必须落到确定的地方。
//
// 桌面模式的优先级：
//  1. 显式传入的 -data（非空且非默认值）；
//  2. exe 同目录下的 data/ai_proxy.db 已存在 —— 直接沿用，避免把老用户的库搬走；
//  3. 用户配置目录下的 ai_proxy（Windows 为 %APPDATA%\ai_proxy）。
//
// 命令行模式保持历史行为：默认相对当前工作目录的 data。
func ResolveDataDir(explicit string, explicitSet bool, desktopMode bool) (string, error) {
	if explicitSet && explicit != "" {
		return explicit, nil
	}
	if !desktopMode {
		return explicit, nil
	}

	if exe, err := os.Executable(); err == nil {
		near := filepath.Join(filepath.Dir(exe), "data")
		if _, statErr := os.Stat(filepath.Join(near, "ai_proxy.db")); statErr == nil {
			return near, nil
		}
	}

	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "ai_proxy"), nil
}
