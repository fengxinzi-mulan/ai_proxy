// Package desktop 提供桌面窗口宿主。
//
// 设计取舍：窗口不是通过 Wails 的 AssetServer 直接加载前端，而是导航到
// 进程内已经跑起来的本地 HTTP 服务。原因是现有前端使用相对 /api、EventSource
// 的 SSE、以及绝对 /assets 路径，只有同源加载才能全部零改动工作。
package desktop

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
)

// CloseAction 决定点击窗口关闭按钮时的行为。
type CloseAction string

const (
	// CloseAsk 每次弹确认框（默认）。
	CloseAsk CloseAction = "ask"
	// CloseQuit 直接退出。
	CloseQuit CloseAction = "quit"
	// CloseTray 隐藏到系统托盘，代理继续运行。
	CloseTray CloseAction = "tray"
)

// legacyCloseMinimize 是旧版本「隐藏到任务栏」的值。读取时自动迁移到托盘，
// 否则老用户的 desktop.json 会落到未知值而被重置成「每次询问」。
const legacyCloseMinimize CloseAction = "minimize"

// Config 是桌面壳的持久化配置，与数据库里的代理设置分开：
// 它是「窗口怎么表现」的问题，跟转发逻辑无关，也不该被 API 客户端读到。
type Config struct {
	CloseAction CloseAction `json:"closeAction"`
}

// DefaultConfig 返回默认配置。
func DefaultConfig() Config {
	return Config{CloseAction: CloseAsk}
}

// normalize 把非法值收敛到默认值，避免配置文件被手改坏之后界面卡死。
func (c *Config) normalize() {
	switch c.CloseAction {
	case CloseAsk, CloseQuit, CloseTray:
	case legacyCloseMinimize:
		c.CloseAction = CloseTray
	default:
		c.CloseAction = CloseAsk
	}
}

// configPath 返回配置文件位置。
func configPath(dataDir string) string {
	return filepath.Join(dataDir, "desktop.json")
}

// LoadConfig 读取配置；文件不存在或损坏时返回默认值。
func LoadConfig(dataDir string) Config {
	cfg := DefaultConfig()
	b, err := os.ReadFile(configPath(dataDir))
	if err != nil {
		return cfg
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return DefaultConfig()
	}
	cfg.normalize()
	return cfg
}

// SaveConfig 原子写入配置。
func SaveConfig(dataDir string, cfg Config) error {
	cfg.normalize()
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	tmp := configPath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, configPath(dataDir))
}

// ErrUnsupported 表示当前平台没有桌面实现。
var ErrUnsupported = errors.New("当前平台暂不支持桌面模式")
