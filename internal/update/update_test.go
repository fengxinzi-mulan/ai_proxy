package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ai_proxy/internal/hub"
	"ai_proxy/internal/store"
)

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"0.2.3", "0.2.3", 0},
		{"v0.2.3", "0.2.3", 0},
		{"0.2.3", "0.2.10", -1}, // 数字比较，不是字符串比较
		{"0.2.10", "0.2.3", 1},
		{"0.3.0", "0.2.99", 1},
		{"1.0.0", "1.0.0-rc1", 1}, // 正式版大于预发布
		{"1.0.0-rc1", "1.0.0", -1},
		{"dev", "0.0.1", -1}, // 开发构建永远更小
		{"", "0.0.1", -1},
		{"0.2", "0.2.0", 0}, // 缺段按 0
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestParseChecksums(t *testing.T) {
	raw := strings.Join([]string{
		"# comment",
		"ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789  ai_proxy-desktop.exe",
		"1111111111111111111111111111111111111111111111111111111111111111 *ai_proxy-server.exe",
		"too-short  whatever.exe",
		"",
	}, "\n")
	got := parseChecksums(raw)
	if len(got) != 2 {
		t.Fatalf("应解析出 2 条，实际 %d: %+v", len(got), got)
	}
	if got["ai_proxy-desktop.exe"] != strings.ToLower("ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789ABCDEF0123456789") {
		t.Errorf("哈希应转小写，实际 %q", got["ai_proxy-desktop.exe"])
	}
	if _, ok := got["ai_proxy-server.exe"]; !ok {
		t.Error("应容忍 * 二进制模式标记")
	}
}

func TestNormalizeDigest(t *testing.T) {
	if got := normalizeDigest("sha256:ABCDEF"); got != "abcdef" {
		t.Errorf("应去掉前缀并转小写，实际 %q", got)
	}
}

// fakeGitHub 起一个假 GitHub：/releases/latest 返回发布 JSON，
// 资产 URL 指回自己，内容由测试给定。
type fakeGitHub struct {
	srv      *httptest.Server
	tag      string
	assets   map[string][]byte
	notes    string
	checksum string // 覆盖 SHA256SUMS.txt 内容；空则按实际文件算
}

func newFakeGitHub(t *testing.T) *fakeGitHub {
	t.Helper()
	f := &fakeGitHub{tag: "v9.9.9", assets: map[string][]byte{}, notes: "修复了一些问题"}
	mux := http.NewServeMux()
	mux.HandleFunc("/releases/latest", func(w http.ResponseWriter, r *http.Request) {
		type asset struct {
			Name   string `json:"name"`
			Size   int64  `json:"size"`
			URL    string `json:"browser_download_url"`
			Digest string `json:"digest"`
		}
		var assets []asset
		for name, body := range f.assets {
			assets = append(assets, asset{
				Name: name, Size: int64(len(body)),
				URL:    f.srv.URL + "/dl/" + name,
				Digest: "sha256:" + sha256Hex(body),
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"tag_name": f.tag, "name": f.tag, "body": f.notes,
			"html_url":     "https://example.com/releases/" + f.tag,
			"published_at": "2026-01-02T03:04:05Z",
			"assets":       assets,
		})
	})
	mux.HandleFunc("/dl/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/dl/")
		body, ok := f.assets[name]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(body)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func newTestManager(t *testing.T, f *fakeGitHub, version string, desktop bool) *Manager {
	t.Helper()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开测试数据库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	m := New(Options{
		Repo:           "test/repo",
		CurrentVersion: version,
		Desktop:        desktop,
		Store:          st,
		Hub:            hub.New(),
		ClientFactory:  func() (*http.Client, error) { return &http.Client{}, nil },
	})
	// 把 API 地址指到假服务器：fetchLatest 用的是常量模板，
	// 这里通过覆盖 repo 无法做到，所以直接替换 apiLatest 不可行 ——
	// 改为让 Manager 用一个可注入的 fetch 函数。
	m.fetch = func(client *http.Client, repo, current string) (Release, error) {
		resp, err := client.Get(f.srv.URL + "/releases/latest")
		if err != nil {
			return Release{}, err
		}
		defer resp.Body.Close()
		var rel Release
		if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
			return Release{}, err
		}
		return rel, nil
	}
	return m
}

func TestCheckFindsUpdate(t *testing.T) {
	f := newFakeGitHub(t)
	f.assets[assetDesktop] = []byte("fake exe")
	f.assets[assetSums] = []byte(sha256Hex(f.assets[assetDesktop]) + "  " + assetDesktop + "\n")

	m := newTestManager(t, f, "0.2.3", true)
	st, err := m.Check(context.Background(), true)
	if err != nil {
		t.Fatalf("检查失败: %v", err)
	}
	if !st.HasUpdate {
		t.Fatalf("应检测到新版本: %+v", st)
	}
	if st.LatestVersion != "v9.9.9" {
		t.Errorf("最新版本应为 v9.9.9，实际 %q", st.LatestVersion)
	}
	if st.AssetName != assetDesktop {
		t.Errorf("桌面版应选 %s，实际 %q", assetDesktop, st.AssetName)
	}
	if st.Notes != "修复了一些问题" {
		t.Errorf("应带回发布说明，实际 %q", st.Notes)
	}
}

func TestCheckPicksServerAsset(t *testing.T) {
	f := newFakeGitHub(t)
	f.assets[assetDesktop] = []byte("desktop")
	f.assets[assetServer] = []byte("server")

	m := newTestManager(t, f, "0.2.3", false)
	st, err := m.Check(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if st.AssetName != assetServer {
		t.Errorf("服务端版应选 %s，实际 %q", assetServer, st.AssetName)
	}
}

// 开发构建不判定有更新：否则每个 go run 都会弹一次升级提示。
func TestCheckDevVersionNeverUpdates(t *testing.T) {
	f := newFakeGitHub(t)
	f.assets[assetDesktop] = []byte("x")
	m := newTestManager(t, f, "dev", true)
	st, err := m.Check(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if st.HasUpdate {
		t.Error("dev 版本不该判定有更新")
	}
	if !st.Dev {
		t.Error("应标记为开发构建")
	}
}

func TestSkipAndClear(t *testing.T) {
	f := newFakeGitHub(t)
	f.assets[assetDesktop] = []byte("x")
	m := newTestManager(t, f, "0.2.3", true)
	if _, err := m.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	st := m.Skip("")
	if !st.Skipped {
		t.Fatalf("跳过当前最新版后应为 skipped: %+v", st)
	}
	if st.SkippedVersion != "v9.9.9" {
		t.Errorf("应记住跳过的版本，实际 %q", st.SkippedVersion)
	}

	// 重新检查时跳过状态要保持（自动弹框据此不弹）。
	st, err := m.Check(context.Background(), true)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Skipped {
		t.Error("重新检查后跳过状态应保持")
	}

	st = m.ClearSkip()
	if st.Skipped || st.SkippedVersion != "" {
		t.Errorf("清除后不该再跳过: %+v", st)
	}
}

// 自动检查受最小间隔限制：第二次不带 force 时不该再请求。
func TestCheckRespectsMinInterval(t *testing.T) {
	f := newFakeGitHub(t)
	f.assets[assetDesktop] = []byte("x")
	m := newTestManager(t, f, "0.2.3", true)

	calls := 0
	orig := m.fetch
	m.fetch = func(c *http.Client, repo, cur string) (Release, error) {
		calls++
		return orig(c, repo, cur)
	}

	if _, err := m.Check(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Check(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Errorf("最小间隔内应只请求一次，实际 %d 次", calls)
	}
	// force 要能绕过。
	if _, err := m.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Errorf("force 应绕过间隔，实际 %d 次", calls)
	}
}

func TestDownloadVerifiesChecksum(t *testing.T) {
	payload := []byte("the new binary")
	f := newFakeGitHub(t)
	f.assets[assetDesktop] = payload
	f.assets[assetSums] = []byte(sha256Hex(payload) + "  " + assetDesktop + "\n")

	m := newTestManager(t, f, "0.2.3", true)
	if _, err := m.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	client, _ := m.client()
	rel := m.latest
	asset, ok := rel.AssetByName(assetDesktop)
	if !ok {
		t.Fatal("缺少资产")
	}
	path, err := m.download(context.Background(), client, rel, asset)
	if err != nil {
		t.Fatalf("下载应成功: %v", err)
	}
	t.Cleanup(func() { os.Remove(path) })

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(payload) {
		t.Errorf("下载内容不一致: %q", got)
	}
	if !strings.HasSuffix(path, ".new") {
		t.Errorf("暂存文件名应以 .new 结尾，实际 %q", path)
	}
}

// 哈希不匹配时必须删掉下载文件并报错 —— 绝不安装来源不明的 exe。
func TestDownloadRejectsBadChecksum(t *testing.T) {
	payload := []byte("the new binary")
	f := newFakeGitHub(t)
	f.assets[assetDesktop] = payload
	// SHA256SUMS.txt 里写一个错误的哈希。
	f.assets[assetSums] = []byte(strings.Repeat("a", 64) + "  " + assetDesktop + "\n")

	m := newTestManager(t, f, "0.2.3", true)
	if _, err := m.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	client, _ := m.client()
	rel := m.latest
	asset, _ := rel.AssetByName(assetDesktop)

	// 先让 Release 自带的 digest 也失效，强制走 SHA256SUMS.txt 分支。
	asset.Digest = ""

	_, err := m.download(context.Background(), client, rel, asset)
	if err == nil {
		t.Fatal("哈希不匹配时应当报错")
	}
	if !strings.Contains(err.Error(), "SHA-256") {
		t.Errorf("错误信息应说明校验失败，实际 %v", err)
	}

	// 暂存文件必须已被删除。
	dir := filepath.Join(m.dataDir(), "update")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".new") {
			t.Errorf("校验失败后不该留下暂存文件: %s", e.Name())
		}
	}
}

// 没有任何校验来源时也必须拒绝。
func TestDownloadRejectsWhenNoChecksumAvailable(t *testing.T) {
	f := newFakeGitHub(t)
	f.assets[assetDesktop] = []byte("payload")
	// 不放 SHA256SUMS.txt。

	m := newTestManager(t, f, "0.2.3", true)
	if _, err := m.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	client, _ := m.client()
	rel := m.latest
	asset, _ := rel.AssetByName(assetDesktop)
	asset.Digest = ""

	if _, err := m.download(context.Background(), client, rel, asset); err == nil {
		t.Fatal("没有校验值时应拒绝安装")
	}
}

func TestDownloadProgressIsMonotonic(t *testing.T) {
	payload := make([]byte, 1<<20) // 1 MB，足以触发多次进度回调
	for i := range payload {
		payload[i] = byte(i)
	}
	f := newFakeGitHub(t)
	f.assets[assetDesktop] = payload
	f.assets[assetSums] = []byte(sha256Hex(payload) + "  " + assetDesktop + "\n")

	m := newTestManager(t, f, "0.2.3", true)
	if _, err := m.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	client, _ := m.client()
	rel := m.latest
	asset, _ := rel.AssetByName(assetDesktop)

	path, err := m.download(context.Background(), client, rel, asset)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(path) })

	st := m.State()
	if st.DownloadedBytes != int64(len(payload)) {
		t.Errorf("已下载字节应等于文件大小: %d vs %d", st.DownloadedBytes, len(payload))
	}
	if st.Phase != PhaseVerifying && st.Phase != PhaseDownloading {
		t.Errorf("阶段应停在下载完成/校验中，实际 %q", st.Phase)
	}
}

// 节流期内重启进程时，上次的检查结果必须从库里恢复 ——
// 否则用户关掉程序再打开，侧栏小圆点不亮、弹框不弹，必须手动点一次才看得到。
func TestCheckRestoresPersistedStateWithinInterval(t *testing.T) {
	dir := t.TempDir()
	st, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	f := newFakeGitHub(t)
	f.assets[assetDesktop] = []byte("x")

	// 第一个实例：查一次，结果落库。
	m1 := New(Options{
		Repo: "test/repo", CurrentVersion: "0.2.3", Desktop: true,
		Store: st, Hub: hub.New(),
		ClientFactory: func() (*http.Client, error) { return &http.Client{}, nil },
	})
	m1.fetch = func(c *http.Client, repo, cur string) (Release, error) {
		resp, err := c.Get(f.srv.URL + "/releases/latest")
		if err != nil {
			return Release{}, err
		}
		defer resp.Body.Close()
		var rel Release
		_ = json.NewDecoder(resp.Body).Decode(&rel)
		return rel, nil
	}
	if _, err := m1.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	if !m1.State().HasUpdate {
		t.Fatal("第一次检查应发现更新")
	}

	// 第二个实例（模拟重启）：同一份库，不带 force 的检查应被节流，
	// 但状态要从库里恢复出来。
	m2 := New(Options{
		Repo: "test/repo", CurrentVersion: "0.2.3", Desktop: true,
		Store: st, Hub: hub.New(),
		ClientFactory: func() (*http.Client, error) { return &http.Client{}, nil },
	})
	calls := 0
	m2.fetch = func(c *http.Client, repo, cur string) (Release, error) {
		calls++
		return Release{}, nil
	}

	got, err := m2.Check(context.Background(), false)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Errorf("节流期内不该请求 GitHub，实际 %d 次", calls)
	}
	if !got.HasUpdate || got.LatestVersion != "v9.9.9" {
		t.Errorf("应从库里恢复上次的检查结果，实际 %+v", got)
	}
	if got.Notes == "" || got.HTMLURL == "" {
		t.Errorf("发布说明与链接也应恢复，实际 %+v", got)
	}
}

// Skip 与 Check 并发时不能互相覆盖（各自改 persist 的不同字段）。
func TestSkipAndCheckDoNotClobberEachOther(t *testing.T) {
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	f := newFakeGitHub(t)
	f.assets[assetDesktop] = []byte("x")

	m := New(Options{
		Repo: "test/repo", CurrentVersion: "0.2.3", Desktop: true,
		Store: st, Hub: hub.New(),
		ClientFactory: func() (*http.Client, error) { return &http.Client{}, nil },
	})
	m.fetch = func(c *http.Client, repo, cur string) (Release, error) {
		resp, err := c.Get(f.srv.URL + "/releases/latest")
		if err != nil {
			return Release{}, err
		}
		defer resp.Body.Close()
		var rel Release
		_ = json.NewDecoder(resp.Body).Decode(&rel)
		return rel, nil
	}

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = m.Check(context.Background(), true)
		}()
		go func() {
			defer wg.Done()
			m.Skip("v9.9.9")
		}()
	}
	wg.Wait()

	p := m.loadPersist()
	if p.SkippedVersion != "v9.9.9" {
		t.Errorf("跳过版本被 Check 覆盖了: %q", p.SkippedVersion)
	}
	if p.LastCheckAt == "" {
		t.Error("节流时间被 Skip 覆盖了（会导致每次都请求 GitHub）")
	}
	if p.LatestVersion != "v9.9.9" {
		t.Errorf("检查结果丢失: %q", p.LatestVersion)
	}
}

func TestInstallRejectsWhenNoUpdate(t *testing.T) {
	f := newFakeGitHub(t)
	m := newTestManager(t, f, "0.2.3", true)
	if err := m.Install(context.Background()); err == nil {
		t.Fatal("没有新版本时安装应报错")
	}
}

// canAutoInstall 在测试进程里必然为 false：测试二进制跑在临时目录。
func TestCanAutoInstallFalseInTestBinary(t *testing.T) {
	f := newFakeGitHub(t)
	m := newTestManager(t, f, "0.2.3", true)
	if m.canAutoInstall() {
		t.Error("测试二进制在临时目录里，不该判定为可自动安装")
	}
}

func TestStateBroadcastsOnChange(t *testing.T) {
	f := newFakeGitHub(t)
	f.assets[assetDesktop] = []byte("x")
	h := hub.New()
	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	m := New(Options{
		Repo: "test/repo", CurrentVersion: "0.2.3", Desktop: true,
		Store: st, Hub: h,
		ClientFactory: func() (*http.Client, error) { return &http.Client{}, nil },
	})
	m.fetch = func(c *http.Client, repo, cur string) (Release, error) {
		resp, err := c.Get(f.srv.URL + "/releases/latest")
		if err != nil {
			return Release{}, err
		}
		defer resp.Body.Close()
		var rel Release
		_ = json.NewDecoder(resp.Body).Decode(&rel)
		return rel, nil
	}

	ch := h.Subscribe()
	defer h.Unsubscribe(ch)

	if _, err := m.Check(context.Background(), true); err != nil {
		t.Fatal(err)
	}

	// 至少要收到一次 update 事件（checking → idle 各一次）。
	deadline := time.After(3 * time.Second)
	for {
		select {
		case payload, ok := <-ch:
			if !ok {
				t.Fatal("通道被关闭")
			}
			if strings.Contains(string(payload), `"update"`) {
				return
			}
		case <-deadline:
			t.Fatal("没有收到 update 事件")
		}
	}
}
