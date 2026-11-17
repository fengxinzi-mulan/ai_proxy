package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandLineQuotesSpaces(t *testing.T) {
	cases := []struct {
		exe  string
		args []string
		want string
	}{
		{`C:\Tools\ai_proxy-desktop.exe`, nil, `C:\Tools\ai_proxy-desktop.exe`},
		{
			`C:\Program Files\AI Proxy\ai_proxy-desktop.exe`,
			[]string{"-autostart"},
			`"C:\Program Files\AI Proxy\ai_proxy-desktop.exe" -autostart`,
		},
		{
			`C:\Tools\ai_proxy-desktop.exe`,
			[]string{"-autostart", "-data", `C:\Users\me\App Data\ai_proxy`},
			`C:\Tools\ai_proxy-desktop.exe -autostart -data "C:\Users\me\App Data\ai_proxy"`,
		},
		{`C:\Tools\a.exe`, []string{""}, `C:\Tools\a.exe ""`},
	}
	for _, tc := range cases {
		got := CommandLine(tc.exe, tc.args)
		if got != tc.want {
			t.Errorf("CommandLine(%q, %v) = %q, want %q", tc.exe, tc.args, got, tc.want)
		}
	}
}

// 开发调试形态（go run 把二进制放进临时目录）不该写启动项。
func TestIsUsableExecutableRejectsTempDir(t *testing.T) {
	temp := os.TempDir()
	ok, reason := IsUsableExecutable(filepath.Join(temp, "go-build123", "b001", "exe", "ai_proxy.exe"))
	if ok {
		t.Fatalf("临时目录里的 exe 应被拒绝，实际 ok=true")
	}
	if !strings.Contains(reason, "go run") {
		t.Errorf("原因应说明是 go run 形态，实际 %q", reason)
	}

	if ok, reason := IsUsableExecutable(""); ok {
		t.Errorf("空路径应被拒绝，实际 ok=true")
	} else if reason == "" {
		t.Error("空路径应给出原因")
	}
}

func TestIsUsableExecutableAcceptsNormalPath(t *testing.T) {
	ok, reason := IsUsableExecutable(`C:\Tools\ai_proxy-desktop.exe`)
	if !ok {
		t.Fatalf("普通路径应可用，实际被拒: %s", reason)
	}
}

// fakeBackend 是内存实现，用来验证 Spec 的状态机而不碰真实注册表。
type fakeBackend struct {
	supported bool
	reason    string
	values    map[string]string
}

func newFake() *fakeBackend {
	return &fakeBackend{supported: true, values: map[string]string{}}
}

func (f *fakeBackend) Supported() (bool, string) { return f.supported, f.reason }

func (f *fakeBackend) Get(name string) (string, bool, error) {
	v, ok := f.values[name]
	return v, ok, nil
}

func (f *fakeBackend) Set(name, command string) error {
	f.values[name] = command
	return nil
}

func (f *fakeBackend) Delete(name string) error {
	delete(f.values, name)
	return nil
}

func TestSpecSetAndRead(t *testing.T) {
	be := newFake()
	spec := Spec{Name: "AIProxy-Desktop", Command: `"C:\a b\x.exe" -autostart`, Backend: be}

	st := spec.Read()
	if !st.Supported || st.Enabled || st.Stale {
		t.Fatalf("初始状态应为「支持但未开启」，实际 %+v", st)
	}

	st, err := spec.Set(true)
	if err != nil {
		t.Fatalf("开启失败: %v", err)
	}
	if !st.Enabled || st.Registered != spec.Command || st.Stale {
		t.Fatalf("开启后状态不对: %+v", st)
	}

	st, err = spec.Set(false)
	if err != nil {
		t.Fatalf("关闭失败: %v", err)
	}
	if st.Enabled || st.Registered != "" {
		t.Fatalf("关闭后状态不对: %+v", st)
	}
}

// exe 被挪到别的目录后，注册的命令与当前应有的不一致，界面要能提示「更新启动项」。
func TestSpecDetectsStaleCommand(t *testing.T) {
	be := newFake()
	be.values["AIProxy-Desktop"] = `C:\old\ai_proxy-desktop.exe -autostart`
	spec := Spec{Name: "AIProxy-Desktop", Command: `C:\new\ai_proxy-desktop.exe -autostart`, Backend: be}

	st := spec.Read()
	if !st.Enabled || !st.Stale {
		t.Fatalf("应识别为「已开启但过期」，实际 %+v", st)
	}
	if st.Registered != `C:\old\ai_proxy-desktop.exe -autostart` {
		t.Errorf("应回显旧命令，实际 %q", st.Registered)
	}
}

func TestSpecUnsupportedBackend(t *testing.T) {
	be := newFake()
	be.supported = false
	be.reason = "当前平台不支持开机自启（目前仅 Windows）"
	spec := Spec{Name: "AIProxy-Server", Command: `x -autostart`, Backend: be}

	st := spec.Read()
	if st.Supported {
		t.Fatal("后端不支持时 Supported 应为 false")
	}
	if st.Reason != be.reason {
		t.Errorf("应透传后端给出的原因，实际 %q", st.Reason)
	}

	if _, err := spec.Set(true); err == nil {
		t.Fatal("不支持时开启应当报错")
	}
}

func TestSpecReasonSkipsBackend(t *testing.T) {
	be := newFake()
	spec := Spec{Name: "x", Command: "y", Reason: "调试形态不支持", Backend: be}

	st := spec.Read()
	if st.Supported || st.Reason != "调试形态不支持" {
		t.Fatalf("Reason 非空时应直接判为不支持，实际 %+v", st)
	}
	if _, err := spec.Set(true); err == nil {
		t.Fatal("Reason 非空时开启应当报错")
	}
	if len(be.values) != 0 {
		t.Error("不应该碰后端")
	}
}
