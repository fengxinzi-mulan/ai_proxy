package update

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"ai_proxy/internal/hub"
	"ai_proxy/internal/store"
)

// 自动检查的最小间隔。
//
// GitHub 未认证 API 是 60 次/小时/IP，而用户可能一天开十几次程序；
// 6 小时一次足够及时，又不会把配额耗在重复请求上。手动检查不受此限。
const minCheckInterval = 6 * time.Hour

// kvKey 是持久化「上次检查时间 + 已跳过版本」的键。
const kvKey = "update"

// 资产文件名。必须与 .github/workflows/release.yml 里产出的名字一致 ——
// 改名会让老版本客户端找不到升级包。
const (
	assetDesktop = "ai_proxy-desktop.exe"
	assetServer  = "ai_proxy-server.exe"
	assetSums    = "SHA256SUMS.txt"
)

// 安装阶段。前端据此推进度条与文案。
const (
	PhaseIdle        = "idle"
	PhaseChecking    = "checking"
	PhaseDownloading = "downloading"
	PhaseVerifying   = "verifying"
	PhaseApplying    = "applying"
	PhaseRestarting  = "restarting"
	PhaseFailed      = "failed"
)

// State 同时是管理接口的响应体。
type State struct {
	CurrentVersion string `json:"currentVersion"`
	LatestVersion  string `json:"latestVersion"`
	HasUpdate      bool   `json:"hasUpdate"`
	// Dev 表示当前是未注入版本号的开发构建，不参与比较。
	Dev bool `json:"dev"`
	// Skipped 表示这个版本被用户显式跳过：自动弹框不弹，手动检查仍能看见。
	Skipped        bool   `json:"skipped"`
	SkippedVersion string `json:"skippedVersion"`
	Notes          string `json:"notes"`
	HTMLURL        string `json:"htmlUrl"`
	PublishedAt    string `json:"publishedAt"`
	AssetName      string `json:"assetName"`
	AssetSize      int64  `json:"assetSize"`
	// CanAutoInstall 为 false 时界面把主按钮换成「打开下载页」。
	CanAutoInstall bool   `json:"canAutoInstall"`
	CheckedAt      string `json:"checkedAt"`

	Phase           string `json:"phase"`
	Progress        int    `json:"progress"`
	DownloadedBytes int64  `json:"downloadedBytes"`
	Error           string `json:"error"`
}

// persist 是持久化的小状态，与 model.Settings 分开存。
//
// 除了节流时间与跳过版本，还把**上次查到的发布信息**一起存下来：
// 6 小时节流期内重启进程时，Check 会直接返回，若状态只在内存里，
// 用户重开程序就看不到「有新版本」，必须手动点一次。
type persist struct {
	LastCheckAt    string `json:"lastCheckAt"`
	SkippedVersion string `json:"skippedVersion"`

	// 上次检查结果（用于节流期内恢复界面状态）。
	LatestVersion string `json:"latestVersion"`
	HasUpdate     bool   `json:"hasUpdate"`
	Notes         string `json:"notes"`
	HTMLURL       string `json:"htmlUrl"`
	PublishedAt   string `json:"publishedAt"`
	AssetName     string `json:"assetName"`
	AssetSize     int64  `json:"assetSize"`
}

// Options 构造 Manager 所需的全部输入。
type Options struct {
	Repo           string
	CurrentVersion string
	Desktop        bool
	Store          *store.Store
	Hub            *hub.Hub
	Logger         *slog.Logger
	// ClientFactory 返回一个遵循全局代理设置的 HTTP 客户端。
	ClientFactory func() (*http.Client, error)
	// BeforeRestart 在替换完成、拉起新进程之前调用：停监听、收托盘图标。
	BeforeRestart func()
	// Restart 拉起新进程并让当前进程退出。
	Restart func() error
}

// Manager 负责检测、下载、校验与安装。
//
// 同一时刻只允许一个安装流程在跑（mu 保护），否则连点两次「立即更新」
// 会下载两份并互相覆盖。
type Manager struct {
	repo    string
	current string
	desktop bool
	store   *store.Store
	hub     *hub.Hub
	logger  *slog.Logger

	clientFactory func() (*http.Client, error)
	beforeRestart func()
	restart       func() error
	// fetch 拉取最新发布。默认是 fetchLatest；测试注入假实现，
	// 这样不必为了跑单测去改包级常量或真的联网。
	// ctx 必须一路带到 http.Request 上，否则界面点了「取消」或服务关停后
	// 这个请求还会自己跑到超时。
	fetch func(context.Context, *http.Client, string, string) (Release, error)

	mu    sync.Mutex
	state State
	// latest 缓存最近一次拉到的发布信息，安装时不再重复请求。
	latest Release
	// installing 表示安装流程正在进行。
	installing bool
	// kvMu 保护 persist 的读改写。Check 与 Skip/ClearSkip 都会改它，
	// 不加锁必然丢更新（要么跳过被覆盖，要么节流时间被覆盖导致每次都请求 GitHub）。
	kvMu sync.Mutex
}

// New 构造 Manager。
func New(o Options) *Manager {
	if o.Repo == "" {
		o.Repo = DefaultRepo
	}
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	m := &Manager{
		repo:          o.Repo,
		current:       o.CurrentVersion,
		desktop:       o.Desktop,
		store:         o.Store,
		hub:           o.Hub,
		logger:        o.Logger,
		clientFactory: o.ClientFactory,
		beforeRestart: o.BeforeRestart,
		restart:       o.Restart,
		fetch:         fetchLatest,
	}
	m.state = State{
		CurrentVersion: o.CurrentVersion,
		Dev:            o.CurrentVersion == "" || o.CurrentVersion == "dev",
		Phase:          PhaseIdle,
		CanAutoInstall: m.canAutoInstall(),
	}
	return m
}

// State 返回当前状态快照。
func (m *Manager) State() State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state
}

// setState 在锁内更新状态并广播给界面。
func (m *Manager) setState(fn func(*State)) {
	m.mu.Lock()
	fn(&m.state)
	snapshot := m.state
	m.mu.Unlock()
	if m.hub != nil {
		m.hub.Broadcast("update", snapshot)
	}
}

// canAutoInstall 判断当前形态能否自动替换 exe。
//
// 三个前提：Windows、不是 go run 形态、有可用的重启钩子。
func (m *Manager) canAutoInstall() bool {
	if runtime.GOOS != "windows" {
		return false
	}
	if m.restart == nil {
		return false
	}
	exe, err := os.Executable()
	if err != nil {
		return false
	}
	dir := strings.ToLower(filepath.Dir(exe))
	temp := strings.ToLower(os.TempDir())
	if temp != "" && strings.HasPrefix(dir, temp) {
		return false
	}
	return !strings.Contains(dir, "go-build")
}

// assetName 返回当前形态对应的资产文件名。
func (m *Manager) assetName() string {
	if m.desktop {
		return assetDesktop
	}
	return assetServer
}

// loadPersist 读取持久化状态。
func (m *Manager) loadPersist() persist {
	m.kvMu.Lock()
	defer m.kvMu.Unlock()
	return m.loadPersistLocked()
}

func (m *Manager) loadPersistLocked() persist {
	var p persist
	if m.store == nil {
		return p
	}
	raw, err := m.store.GetKV(kvKey)
	if err != nil || raw == "" {
		return p
	}
	_ = json.Unmarshal([]byte(raw), &p)
	return p
}

// savePersist 写入持久化状态。
func (m *Manager) savePersist(p persist) {
	m.kvMu.Lock()
	defer m.kvMu.Unlock()
	m.savePersistLocked(p)
}

func (m *Manager) savePersistLocked(p persist) {
	if m.store == nil {
		return
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return
	}
	if err := m.store.PutKV(kvKey, string(raw)); err != nil {
		m.logger.Warn("保存更新状态失败", "err", err)
	}
}

// updatePersist 以读改写的方式更新持久化状态，整个操作在锁内完成。
func (m *Manager) updatePersist(fn func(*persist)) persist {
	m.kvMu.Lock()
	defer m.kvMu.Unlock()
	p := m.loadPersistLocked()
	fn(&p)
	m.savePersistLocked(p)
	return p
}

// client 返回一个遵循全局代理的 HTTP 客户端。
func (m *Manager) client() (*http.Client, error) {
	if m.clientFactory != nil {
		return m.clientFactory()
	}
	return &http.Client{Timeout: 30 * time.Second}, nil
}

// Check 检查是否有新版本。
//
// force=false 时受 minCheckInterval 限制（启动自动检查走这条）；
// 手动点「检查更新」传 true，跳过限制。
//
// 返回的 error 只用于日志与界面提示，调用方不该因为它阻断任何主流程。
func (m *Manager) Check(ctx context.Context, force bool) (State, error) {
	// 安装流程进行中不要碰 State：Phase/Progress 归 Install 推进，
	// 这里一覆盖就会出现「进度条跑着跑着跳回 idle」或者把失败原因抹掉。
	if m.Installing() {
		return m.State(), nil
	}

	p := m.loadPersist()
	if !force && p.LastCheckAt != "" {
		if last, err := time.Parse(time.RFC3339, p.LastCheckAt); err == nil {
			if time.Since(last) < minCheckInterval {
				// 节流期内也要把上次的结果恢复到 State 上：
				// 进程重启后 State 是空的，不恢复的话用户重开程序就看不到新版本。
				m.restoreFromPersist(p)
				return m.State(), nil
			}
		}
	}

	m.setState(func(s *State) {
		s.Phase = PhaseChecking
		s.Error = ""
	})

	client, err := m.client()
	if err != nil {
		return m.fail("初始化网络连接失败: " + err.Error())
	}

	rel, err := m.fetch(ctx, client, m.repo, m.current)
	if err != nil {
		return m.fail(err.Error())
	}

	// 拉取期间用户点了「立即更新」：此时 State 已经属于 Install，
	// 再把检查结果写回去会把下载进度顶掉。
	if m.Installing() {
		return m.State(), nil
	}

	asset, hasAsset := rel.AssetByName(m.assetName())
	hasUpdate := !m.State().Dev && CompareVersions(rel.Tag, m.current) > 0

	m.mu.Lock()
	m.latest = rel
	m.mu.Unlock()

	now := time.Now().UTC().Format(time.RFC3339)
	p = m.updatePersist(func(p *persist) {
		p.LastCheckAt = now
		p.LatestVersion = rel.Tag
		p.HasUpdate = hasUpdate
		p.Notes = rel.Notes
		p.HTMLURL = rel.HTMLURL
		p.PublishedAt = rel.PublishedAt.Format(time.RFC3339)
		if hasAsset {
			p.AssetName = asset.Name
			p.AssetSize = asset.Size
		} else {
			p.AssetName = ""
			p.AssetSize = 0
		}
	})

	m.setState(func(s *State) {
		s.Phase = PhaseIdle
		s.Error = ""
		s.LatestVersion = rel.Tag
		s.HasUpdate = hasUpdate
		s.Notes = rel.Notes
		s.HTMLURL = rel.HTMLURL
		s.PublishedAt = rel.PublishedAt.Format(time.RFC3339)
		s.CheckedAt = now
		s.SkippedVersion = p.SkippedVersion
		s.Skipped = hasUpdate && p.SkippedVersion != "" && CompareVersions(p.SkippedVersion, rel.Tag) == 0
		s.AssetName = p.AssetName
		s.AssetSize = p.AssetSize
		s.CanAutoInstall = m.canAutoInstall() && hasAsset
	})

	m.logger.Info("更新检查完成", "latest", rel.Tag, "current", m.current, "hasUpdate", hasUpdate)
	return m.State(), nil
}

// restoreFromPersist 把上次检查的结果恢复到内存状态。
//
// 只用于「节流期内不该重新请求」的场景；CanAutoInstall 现算，
// 因为它取决于当前 exe 位置与平台，不该跟着库里的旧值走。
//
// HasUpdate 也必须**按当前版本现算**，不能直接搬库里的值：
// 刚更新成功并重启时，库里存的还是「旧版本看到有新版本」的结论，
// 直接搬过来会让界面在已经是最新版本的情况下继续提示升级。
func (m *Manager) restoreFromPersist(p persist) {
	if p.LatestVersion == "" {
		return
	}
	hasUpdate := !m.State().Dev && CompareVersions(p.LatestVersion, m.current) > 0
	m.setState(func(s *State) {
		s.LatestVersion = p.LatestVersion
		s.HasUpdate = hasUpdate
		s.Notes = p.Notes
		s.HTMLURL = p.HTMLURL
		s.PublishedAt = p.PublishedAt
		s.CheckedAt = p.LastCheckAt
		s.SkippedVersion = p.SkippedVersion
		s.Skipped = hasUpdate && p.SkippedVersion != "" &&
			CompareVersions(p.SkippedVersion, p.LatestVersion) == 0
		s.AssetName = p.AssetName
		s.AssetSize = p.AssetSize
		s.CanAutoInstall = m.canAutoInstall() && p.AssetName != ""
	})
}

// fail 记录失败并返回当前状态。
func (m *Manager) fail(msg string) (State, error) {
	// 安装期间失败原因由 Install 负责写，这里不能把它覆盖掉。
	if m.Installing() {
		return m.State(), errors.New(msg)
	}
	m.setState(func(s *State) {
		s.Phase = PhaseFailed
		s.Error = msg
	})
	return m.State(), errors.New(msg)
}

// Skip 把某个版本加入「已跳过」。
func (m *Manager) Skip(version string) State {
	if version == "" {
		version = m.State().LatestVersion
	}
	m.updatePersist(func(p *persist) { p.SkippedVersion = version })

	m.setState(func(s *State) {
		s.SkippedVersion = version
		s.Skipped = version != "" && CompareVersions(version, s.LatestVersion) == 0
	})
	return m.State()
}

// ClearSkip 取消跳过（设置页的「不再跳过」）。
func (m *Manager) ClearSkip() State {
	m.updatePersist(func(p *persist) { p.SkippedVersion = "" })

	m.setState(func(s *State) {
		s.SkippedVersion = ""
		s.Skipped = false
	})
	return m.State()
}

// fetchReleaseForInstall 在安装流程内补拉一次 release 元数据。
//
// 只取 Release，不写 State：安装期间 Phase/Progress 归 Install 推进。
func (m *Manager) fetchReleaseForInstall(ctx context.Context) (Release, error) {
	client, err := m.client()
	if err != nil {
		return Release{}, err
	}
	return m.fetch(ctx, client, m.repo, m.current)
}

// Installing 报告安装流程是否正在进行。
func (m *Manager) Installing() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.installing
}

// Install 下载、校验、替换并重启。调用方应在独立 goroutine 里跑它。
//
// 任何一步失败都停在 PhaseFailed 并把原因写进 State.Error，
// 不会留下「装到一半」的状态：替换是「改名 → 写入 → 失败则改回」的原子操作。
func (m *Manager) Install(ctx context.Context) error {
	m.mu.Lock()
	if m.installing {
		m.mu.Unlock()
		return errors.New("更新已在安装中")
	}
	m.installing = true
	m.mu.Unlock()
	defer func() {
		m.mu.Lock()
		m.installing = false
		m.mu.Unlock()
	}()

	st := m.State()
	if !st.HasUpdate {
		return m.installFail("当前没有可安装的新版本")
	}
	if !st.CanAutoInstall {
		return m.installFail("当前形态不支持自动安装，请手动下载")
	}

	// 复用最近一次检查的结果；没有（例如刚重启）就现查一次。
	m.mu.Lock()
	rel := m.latest
	m.mu.Unlock()
	if rel.Tag == "" {
		// 不能走 Check：安装期间它刻意不碰 State、直接返回，m.latest 会一直是
		// 空的 —— 重启后从持久化恢复的状态只带 HasUpdate，用户点「立即更新」
		// 就必然报「发布里没有找到 ai_proxy-desktop.exe」。这里只补拉 Release。
		fetched, err := m.fetchReleaseForInstall(ctx)
		if err != nil {
			return m.installFail("重新获取发布信息失败: " + err.Error())
		}
		rel = fetched
		m.mu.Lock()
		m.latest = rel
		m.mu.Unlock()
	}

	asset, ok := rel.AssetByName(m.assetName())
	if !ok {
		return m.installFail("发布里没有找到 " + m.assetName())
	}

	client, err := m.client()
	if err != nil {
		return m.installFail("初始化网络连接失败: " + err.Error())
	}

	staged, err := m.download(ctx, client, rel, asset)
	if err != nil {
		return m.installFail(err.Error())
	}

	m.setState(func(s *State) {
		s.Phase = PhaseApplying
		s.Progress = 100
	})
	exe, err := os.Executable()
	if err != nil {
		return m.installFail("无法确定当前可执行文件路径: " + err.Error())
	}
	if err := Apply(exe, staged); err != nil {
		return m.installFail("替换可执行文件失败: " + err.Error())
	}

	m.setState(func(s *State) { s.Phase = PhaseRestarting })
	if m.beforeRestart != nil {
		m.beforeRestart()
	}
	if err := m.restart(); err != nil {
		// 走到这里说明新 exe 已经就位，只是没能拉起来。此时 beforeRestart
		// 已经把 HTTP 服务和数据库关了（Shutdown 不可逆），进程实际上已经
		// 是个空壳 —— 必须把话说明白，让用户手动重启，而不是留一个
		// 所有接口都报错的窗口在那儿。
		msg := "新版本已写入，但自动重启失败：" + err.Error() +
			"。请手动关闭本程序后重新打开，届时运行的已经是新版本。"
		return m.installFail(msg)
	}
	return nil
}

// installFail 把安装失败写进状态。
func (m *Manager) installFail(msg string) error {
	m.setState(func(s *State) {
		s.Phase = PhaseFailed
		s.Error = msg
	})
	m.logger.Warn("更新安装失败", "err", msg)
	return errors.New(msg)
}

// maxAssetBytes 是单个升级包的下载上限。
//
// 校验发生在下载**之后**，没有这个上限时，一个有问题的 Release（或中间人，
// 虽然 HTTPS 挡了一层）可以让程序往数据目录写几十 GB 才失败。
const maxAssetBytes = 200 << 20

// download 下载资产到暂存目录并校验哈希。
func (m *Manager) download(ctx context.Context, client *http.Client, rel Release, asset Asset) (string, error) {
	dir := filepath.Join(m.dataDir(), "update")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建暂存目录失败: %w", err)
	}
	// 先清掉上次失败留下的残片，避免磁盘越攒越多。
	cleanStaged(dir)

	staged := filepath.Join(dir, asset.Name+".new")

	m.setState(func(s *State) {
		s.Phase = PhaseDownloading
		s.Progress = 0
		s.DownloadedBytes = 0
		s.Error = ""
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "ai-proxy/"+m.current)
	// 客户端是按普通 API 请求配的（15～30 秒全局 Timeout），对几十 MB 的
	// 升级包来说太短：慢网下下载一定会在中途被当成失败，重试也永远装不完。
	// 这里浅拷贝一份并关掉它的硬超时，改由调用方传入的 ctx（10 分钟）兜底。
	client = clientOrDefault(client)
	dl := *client
	dl.Timeout = 0
	resp, err := dl.Do(req)
	if err != nil {
		return "", fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}

	f, err := os.Create(staged)
	if err != nil {
		return "", fmt.Errorf("创建临时文件失败: %w", err)
	}
	// 边下边算哈希，省一次全文件重读；同时用 LimitReader 卡住体积上限。
	written, sum, copyErr := copyWithProgress(
		f, io.LimitReader(resp.Body, maxAssetBytes+1), asset.Size, func(done int64) {
			pct := 0
			if asset.Size > 0 {
				pct = int(done * 100 / asset.Size)
				if pct > 99 {
					pct = 99
				}
			}
			m.setState(func(s *State) {
				s.Progress = pct
				s.DownloadedBytes = done
			})
		})
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(staged)
		return "", fmt.Errorf("下载中断: %w", copyErr)
	}
	if closeErr != nil {
		os.Remove(staged)
		return "", fmt.Errorf("写入临时文件失败: %w", closeErr)
	}
	if written == 0 {
		os.Remove(staged)
		return "", errors.New("下载到的文件为空")
	}
	if written > maxAssetBytes {
		os.Remove(staged)
		return "", fmt.Errorf("升级包超过 %d MB 上限，已拒绝", maxAssetBytes>>20)
	}

	m.setState(func(s *State) { s.Phase = PhaseVerifying })

	// 校验顺序：Release 里自带的 digest 最权威（GitHub 服务端算的），
	// 没有就回退到 SHA256SUMS.txt（我们自己发布的）。
	want := normalizeDigest(asset.Digest)
	if want == "" {
		if sumsAsset, ok := rel.AssetByName(assetSums); ok {
			if got, err := fetchChecksums(ctx, client, sumsAsset, m.current); err == nil {
				want = got[asset.Name]
			} else {
				m.logger.Warn("下载校验文件失败", "err", err)
			}
		}
	}
	if want == "" {
		// 没有任何可用的校验来源：宁可拒绝，也不装一个来源不明的 exe。
		os.Remove(staged)
		return "", errors.New("发布里没有可用的 SHA-256 校验值，已拒绝安装")
	}
	if !strings.EqualFold(want, sum) {
		os.Remove(staged)
		return "", fmt.Errorf("SHA-256 校验失败（期望 %s，实际 %s），已删除下载文件", short(want), short(sum))
	}

	return staged, nil
}

// dataDir 返回数据目录，暂存文件放在它下面的 update/。
func (m *Manager) dataDir() string {
	if m.store == nil {
		return os.TempDir()
	}
	return m.store.DataDir()
}

// clientOrDefault 兜住注入的 ClientFactory 返回 (nil, nil) 的情况。
func clientOrDefault(client *http.Client) *http.Client {
	if client == nil {
		return &http.Client{}
	}
	return client
}

// fetchChecksums 下载并解析 SHA256SUMS.txt。
func fetchChecksums(ctx context.Context, client *http.Client, asset Asset, version string) (map[string]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.URL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "ai-proxy/"+version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	return parseChecksums(string(raw)), nil
}

// copyWithProgress 复制数据并周期性回调进度。
//
// 每 256 KB 回调一次：再密就是拿界面重渲染换无意义的精度。
func copyWithProgress(dst io.Writer, src io.Reader, total int64, onProgress func(int64)) (int64, string, error) {
	h := newHasher()
	buf := make([]byte, 64<<10)
	var written int64
	var lastReport int64
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return written, "", werr
			}
			h.Write(buf[:n])
			written += int64(n)
			if written-lastReport >= 256<<10 {
				lastReport = written
				if onProgress != nil {
					onProgress(written)
				}
			}
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return written, "", err
		}
	}
	if onProgress != nil {
		onProgress(written)
	}
	return written, h.Sum(), nil
}

// short 截短哈希，只用于错误提示。
func short(s string) string {
	if len(s) > 12 {
		return s[:12] + "…"
	}
	return s
}

// cleanStaged 清掉暂存目录里上次失败留下的残片。
//
// 只删 .new / .tmp 这类中间文件，不动别的 —— 目录是程序自己建的，
// 但保守一点总没错。
func cleanStaged(dir string) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasSuffix(name, ".new") || strings.HasSuffix(name, ".tmp") {
			_ = os.Remove(filepath.Join(dir, name))
		}
	}
}
