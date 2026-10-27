//go:build ignore

// 生成 Windows 资源脚本（app.rc），供 windres 编译进 exe。
//
// 单独放一个生成器而不是手写 .rc，是因为版本号来自构建参数，
// 手写会出现「exe 属性里的版本和界面显示的版本不一致」。
// 用法：go run ./scripts/gen-rc -version 0.2.1 然后 windres 编译。
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// defaultVersion 仅用于本地直接运行生成器；构建脚本应显式传 -version。
const defaultVersion = "0.2.0"

func main() {
	version := flag.String("version", defaultVersion, "版本号，例如 0.2.1")
	flag.Parse()
	appVersion := strings.TrimSpace(*version)
	if appVersion == "" {
		appVersion = defaultVersion
	}

	parts := strings.Split(appVersion, ".")
	for len(parts) < 4 {
		parts = append(parts, "0")
	}
	comma := strings.Join(parts, ",")

	rc := fmt.Sprintf(`#include <winver.h>

1 ICON "app.ico"

VS_VERSION_INFO VERSIONINFO
 FILEVERSION %s
 PRODUCTVERSION %s
 FILEFLAGSMASK 0x3fL
 FILEFLAGS 0x0L
 FILEOS VOS_NT_WINDOWS32
 FILETYPE VFT_APP
 FILESUBTYPE 0x0L
BEGIN
    BLOCK "StringFileInfo"
    BEGIN
        BLOCK "080404b0"
        BEGIN
            VALUE "CompanyName", "AI Proxy"
            VALUE "FileDescription", "AI Proxy 桌面版"
            VALUE "FileVersion", "%s"
            VALUE "InternalName", "ai_proxy"
            VALUE "OriginalFilename", "ai_proxy-desktop.exe"
            VALUE "ProductName", "AI Proxy"
            VALUE "ProductVersion", "%s"
        END
    END
    BLOCK "VarFileInfo"
    BEGIN
        VALUE "Translation", 0x804, 1200
    END
END
`, comma, comma, appVersion, appVersion)

	out := filepath.Join("build", "windows", "app.rc")
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(out, []byte(rc), 0o644); err != nil {
		log.Fatal(err)
	}
	fmt.Printf("wrote %s (version %s)\n", out, appVersion)
}
