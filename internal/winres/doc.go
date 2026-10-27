// Package winres 只承载 Windows 资源（图标与版本信息）。
//
// 资源通过 rsrc_windows_amd64.syso 打包，由 windres 从 build/windows/app.rc
// 编译而来；它只被桌面入口空引入，服务端构建不会带上。
//
// 重新生成：
//
//	go run scripts/gen-icon
//	go run scripts/gen-rc/main.go
//	cd build/windows && windres -i app.rc -o ../../internal/winres/rsrc_windows_amd64.syso -O coff
package winres
