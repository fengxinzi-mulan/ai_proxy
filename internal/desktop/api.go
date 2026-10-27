package desktop

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ai_proxy/internal/store"
)

// DesktopAPI 是只暴露给桌面窗口的接口。
//
// 服务端模式不注册任何 /api/desktop/* 路由，所以这些能力天然不存在于
// 浏览器/远程访问场景；界面用 /api/meta 的 desktop 字段决定是否显示入口。
type DesktopAPI struct {
	dataDir string
	// onClose 由桌面壳注入：用户在关闭确认框里做出选择后执行退出或隐藏。
	onClose func(action string)
}

// NewDesktopAPI 构造桌面接口；dataDir 用来读写 desktop.json 与打开目录。
func NewDesktopAPI(dataDir string, onClose func(action string)) *DesktopAPI {
	return &DesktopAPI{dataDir: dataDir, onClose: onClose}
}

// Register 把桌面接口挂到 mux 上。store 参数当前未用，保留给未来需要
// 直接读库的场景（例如展示日志目录体积）。
func (d *DesktopAPI) Register(mux *http.ServeMux, _ *store.Store) {
	mux.HandleFunc("GET /api/desktop/settings", d.handleGet)
	mux.HandleFunc("PUT /api/desktop/settings", d.handlePut)
	mux.HandleFunc("POST /api/desktop/close", d.handleClose)
	mux.HandleFunc("POST /api/desktop/open", d.handleOpen)
	mux.HandleFunc("GET /api/desktop/info", d.handleInfo)
}

type settingsPayload struct {
	CloseAction CloseAction `json:"closeAction"`
}

func (d *DesktopAPI) handleGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, LoadConfig(d.dataDir))
}

func (d *DesktopAPI) handlePut(w http.ResponseWriter, r *http.Request) {
	var p settingsPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	cfg := LoadConfig(d.dataDir)
	cfg.CloseAction = p.CloseAction
	cfg.normalize()
	if err := SaveConfig(d.dataDir, cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "保存桌面设置失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

type closePayload struct {
	Action   string `json:"action"`
	Remember bool   `json:"remember"`
}

// handleClose 处理前端关闭确认框的选择。
//
// 响应先返回，再延迟执行退出/隐藏：退出会直接停掉这个 HTTP 服务，
// 同步执行的话前端可能收不到响应。
func (d *DesktopAPI) handleClose(w http.ResponseWriter, r *http.Request) {
	var p closePayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	switch p.Action {
	case "quit", "tray", "cancel":
	default:
		writeError(w, http.StatusBadRequest, "未知的关闭动作")
		return
	}
	if p.Remember && p.Action != "cancel" {
		cfg := LoadConfig(d.dataDir)
		cfg.CloseAction = CloseAction(p.Action)
		if err := SaveConfig(d.dataDir, cfg); err != nil {
			writeError(w, http.StatusInternalServerError, "保存关闭行为失败: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
	if d.onClose != nil {
		action := p.Action
		go func() {
			time.Sleep(120 * time.Millisecond)
			d.onClose(action)
		}()
	}
}

type openPayload struct {
	Target string `json:"target"`
}

// handleOpen 用系统文件管理器打开数据目录或日志目录。
//
// 只接受两个固定 target，绝不把请求里的路径拼进命令，避免命令注入。
func (d *DesktopAPI) handleOpen(w http.ResponseWriter, r *http.Request) {
	var p openPayload
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeError(w, http.StatusBadRequest, "请求体不是合法 JSON")
		return
	}
	var dir string
	switch p.Target {
	case "data":
		dir = d.dataDir
	case "logs":
		dir = filepath.Join(d.dataDir, "logs")
	default:
		writeError(w, http.StatusBadRequest, "未知的打开目标")
		return
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		writeError(w, http.StatusInternalServerError, "目录不可用: "+err.Error())
		return
	}
	if err := openInFileManager(dir); err != nil {
		writeError(w, http.StatusInternalServerError, "打开目录失败: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "path": dir})
}

// handleInfo 返回界面要展示的本地路径信息。
func (d *DesktopAPI) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"dataDir": d.dataDir,
		"logDir":  filepath.Join(d.dataDir, "logs"),
		"dbPath":  filepath.Join(d.dataDir, "ai_proxy.db"),
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": strings.TrimSpace(msg)})
}
