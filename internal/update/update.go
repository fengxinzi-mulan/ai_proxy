// Package update 通过 GitHub Release 检测并安装新版本。
//
// 为什么用 GitHub Release 而不是自建更新服务器：发布流程（.github/workflows/release.yml）
// 本来就把两个 exe 和 SHA256SUMS.txt 传到 Release 上，这里直接复用，
// 不需要额外维护一个分发端点，也不会出现「Release 和更新源不一致」。
//
// 三条边界：
//  1. 检测失败绝不影响代理运行 —— 断网、被墙、限流都是常态，只记日志。
//  2. 替换 exe 前必须校验 SHA-256，校验不过一律不落盘。
//  3. 非 Windows 与 go run 形态只提示、不自动安装，退回「打开下载页」。
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultRepo 是发布所在的仓库。换仓库只改这一处。
const DefaultRepo = "fengxinzi-mulan/ai_proxy"

// apiLatest 是 GitHub 的「最新正式版」接口。
//
// 用它而不是 /releases：后者包含预发布版与草稿，而自动更新只该跟进正式版。
const apiLatest = "https://api.github.com/repos/%s/releases/latest"

// Asset 是一个发布附件。
type Asset struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	URL  string `json:"browser_download_url"`
	// Digest 是 GitHub 从 2025 年起给附件算的哈希，形如 "sha256:abcd…"。
	// 可能为空（旧 Release），所以还要能回退到 SHA256SUMS.txt。
	Digest string `json:"digest"`
}

// Release 是一次发布。
type Release struct {
	Tag         string    `json:"tag_name"`
	Name        string    `json:"name"`
	Notes       string    `json:"body"`
	HTMLURL     string    `json:"html_url"`
	Draft       bool      `json:"draft"`
	Prerelease  bool      `json:"prerelease"`
	PublishedAt time.Time `json:"published_at"`
	Assets      []Asset   `json:"assets"`
}

// AssetByName 按文件名找附件。
func (r Release) AssetByName(name string) (Asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return Asset{}, false
}

// CompareVersions 比较两个版本号，返回 -1 / 0 / 1。
//
// 只处理本项目实际会出现的形态：可选 v 前缀、点分数字、可选的预发布后缀。
// 不引入 semver 库 —— 依赖越少越好，而这里的规则简单到能一眼看懂。
// 非数字段（例如 "dev"）按 0 处理，所以开发构建永远比任何正式版小。
func CompareVersions(a, b string) int {
	an, apre := splitVersion(a)
	bn, bpre := splitVersion(b)
	for i := 0; i < len(an) || i < len(bn); i++ {
		var x, y int
		if i < len(an) {
			x = an[i]
		}
		if i < len(bn) {
			y = bn[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	// 数字部分相同：没有预发布后缀的更大（1.0.0 > 1.0.0-rc1）。
	switch {
	case apre == "" && bpre != "":
		return 1
	case apre != "" && bpre == "":
		return -1
	case apre == bpre:
		return 0
	case apre < bpre:
		return -1
	default:
		return 1
	}
}

// splitVersion 把 "v1.2.3-rc1" 拆成 [1,2,3] 与 "rc1"。
func splitVersion(v string) ([]int, string) {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	pre := ""
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		pre = v[i+1:]
		v = v[:i]
	}
	if v == "" {
		return nil, pre
	}
	parts := strings.Split(v, ".")
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		n, err := strconv.Atoi(strings.TrimSpace(p))
		if err != nil {
			// "dev" 这类非数字段按 0 处理。
			n = 0
		}
		nums = append(nums, n)
	}
	return nums, pre
}

// fetchLatest 拉取最新正式版。
func fetchLatest(ctx context.Context, client *http.Client, repo, currentVersion string) (Release, error) {
	url := fmt.Sprintf(apiLatest, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	// GitHub 要求带 User-Agent，否则直接 403。
	req.Header.Set("User-Agent", "ai-proxy/"+currentVersion)

	resp, err := client.Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("连接 GitHub 失败: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return Release{}, fmt.Errorf("GitHub 返回 HTTP %d%s", resp.StatusCode, rateLimitHint(resp))
	}
	var rel Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&rel); err != nil {
		return Release{}, fmt.Errorf("解析发布信息失败: %w", err)
	}
	return rel, nil
}

// rateLimitHint 把限流响应翻译成用户能理解的一句话。
func rateLimitHint(resp *http.Response) string {
	switch resp.StatusCode {
	case http.StatusForbidden, http.StatusTooManyRequests:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			if reset := resp.Header.Get("X-RateLimit-Reset"); reset != "" {
				if ts, err := strconv.ParseInt(reset, 10, 64); err == nil {
					return "（GitHub API 限流，约 " + time.Until(time.Unix(ts, 0)).Round(time.Minute).String() + "后恢复）"
				}
			}
			return "（GitHub API 限流，请稍后再试）"
		}
	}
	return ""
}

// hasher 是「边写边算 SHA-256、最后取十六进制」的小包装。
type hasher struct{ h hash.Hash }

func newHasher() *hasher { return &hasher{h: sha256.New()} }

func (w *hasher) Write(p []byte) (int, error) { return w.h.Write(p) }

// Sum 返回小写十六进制摘要。
func (w *hasher) Sum() string { return hex.EncodeToString(w.h.Sum(nil)) }

// normalizeDigest 把 "sha256:ABCD" 规整成小写裸十六进制。
func normalizeDigest(d string) string {
	d = strings.TrimSpace(d)
	d = strings.TrimPrefix(d, "sha256:")
	return strings.ToLower(d)
}

// parseChecksums 解析 SHA256SUMS.txt。
//
// 格式是 sha256sum 的标准输出："<64位小写十六进制>  <文件名>"（两个空格）。
// 也容忍单空格与 *文件名（二进制模式标记）。
func parseChecksums(raw string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		sum := strings.ToLower(fields[0])
		name := strings.TrimPrefix(fields[len(fields)-1], "*")
		if len(sum) != 64 {
			continue
		}
		out[name] = sum
	}
	return out
}
