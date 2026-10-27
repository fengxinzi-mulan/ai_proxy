// Package version 保存构建时注入的版本号。
//
// 通过 -ldflags "-X ai_proxy/internal/version.Version=0.2.0" 覆盖；
// 不加 ldflags 时保持 dev，便于区分开发构建。
package version

// Version 是当前构建的版本号。
var Version = "dev"
