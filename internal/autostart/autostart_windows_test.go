//go:build windows

package autostart

import "testing"

// 用临时注册表键做真实读写往返，验证 registryBackend 本身（而不是 fake）。
// 键名带随机后缀，跑完即删，绝不碰用户真实的 Run 键。
func TestRegistryBackendRoundTrip(t *testing.T) {
	be := &registryBackend{keyPath: `Software\AIProxyTest\Run`}
	t.Cleanup(func() { _ = be.Delete("RoundTrip") })

	const command = `"C:\Program Files\AI Proxy\ai_proxy-desktop.exe" -autostart`

	if _, found, err := be.Get("RoundTrip"); err != nil {
		t.Fatalf("读取不存在的值应返回 nil 错误: %v", err)
	} else if found {
		t.Fatal("值不该存在")
	}

	if err := be.Set("RoundTrip", command); err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	got, found, err := be.Get("RoundTrip")
	if err != nil || !found {
		t.Fatalf("写入后应能读到: found=%v err=%v", found, err)
	}
	if got != command {
		t.Errorf("读到 %q，期望 %q", got, command)
	}

	if err := be.Delete("RoundTrip"); err != nil {
		t.Fatalf("删除失败: %v", err)
	}
	if _, found, _ := be.Get("RoundTrip"); found {
		t.Error("删除后不该还能读到")
	}
	// 重复删除不是错误。
	if err := be.Delete("RoundTrip"); err != nil {
		t.Errorf("重复删除应幂等: %v", err)
	}
}
