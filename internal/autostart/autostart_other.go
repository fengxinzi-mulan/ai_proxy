//go:build !windows

package autostart

// 非 Windows 平台暂不提供开机自启。保留实现是为了让
// `go build` 在 Linux/macOS 上正常工作，并把原因如实告诉界面，
// 而不是给一个点了没反应的开关。

type unsupportedBackend struct{}

func systemBackend() Backend { return unsupportedBackend{} }

func (unsupportedBackend) Supported() (bool, string) {
	return false, "当前平台不支持开机自启（目前仅 Windows）"
}

func (unsupportedBackend) Get(string) (string, bool, error) { return "", false, nil }

func (unsupportedBackend) Set(string, string) error { return ErrUnsupported }

func (unsupportedBackend) Delete(string) error { return nil }
