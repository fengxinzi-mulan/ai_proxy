package proxy

// 图片数量上限对真实 Mistral 上游的验证。默认跳过，只在显式设置了密钥时运行。
//
// 这个用例存在的原因：上限裁剪的四个关键假设全都无法从代码里证明，只能打真实上游：
//
//  1. **上限真的是 20**（上游文档只说「取决于模型」，20 是错误原文里的数）；
//  2. **占位块真的是合法 content**（{"type":"text","text":…} 要被 extra="forbid"
//     的严格 schema 接受）；
//  3. **裁剪后真的能过**（计数口径与上游一致：我按 part 数，上游可能还数别的）；
//  4. **分批真的保住了前缀缓存**（同一批窗口内 usage 的 cached_tokens 应该保持高位，
//     而不是每轮掉到接近 0）。
//
// 用法（密钥从库或环境变量取）：
//
//	MISTRAL_LIVE=1 MISTRAL_LIVE_MODEL=mistral-small-latest \
//	  go test ./internal/proxy -run TestLiveMistralImageLimit -v
//
// 未设 MISTRAL_LIVE=1 时整条跳过。用例会**消耗真实额度**：总共约 6~8 次小请求，
// 每次带 20 张左右的极小图片，输入量在几万 token 量级。

import (
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ai_proxy/internal/hub"
	"ai_proxy/internal/model"
	"ai_proxy/internal/store"
)

// liveMistralTarget 取 Mistral 的密钥、BaseURL 与全局设置。
//
// 优先环境变量 MISTRAL_API_KEY，否则只读打开 data/ai_proxy.db 里 base_url 命中
// mistral.ai 的那个供应商。密钥只留在内存里，不写文件、不打日志。
func liveMistralTarget(t *testing.T) (key, baseURL string, settings model.Settings) {
	t.Helper()
	baseURL = "https://api.mistral.ai/v1"
	key = os.Getenv("MISTRAL_API_KEY")

	path := os.Getenv("AI_PROXY_DB")
	if path == "" {
		path = filepath.Join("..", "..", "data", "ai_proxy.db")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("解析库路径失败: %v", err)
	}
	db, err := sql.Open("sqlite", "file:"+strings.ReplaceAll(abs, `\`, "/")+"?mode=ro")
	if err != nil {
		if key == "" {
			t.Skipf("打不开 %s（%v）且未设置 MISTRAL_API_KEY，跳过", abs, err)
		}
		return key, baseURL, settings
	}
	defer db.Close()

	var raw string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = 'global'`).Scan(&raw); err == nil {
		_ = json.Unmarshal([]byte(raw), &settings)
		settings.ApplyDefaults()
	}

	var dbBase, keysJSON string
	if err := db.QueryRow(
		`SELECT base_url, keys_json FROM providers
		 WHERE base_url LIKE '%mistral%' AND enabled = 1
		 ORDER BY active DESC, sort_order, id LIMIT 1`).Scan(&dbBase, &keysJSON); err == nil {
		if strings.TrimSpace(dbBase) != "" {
			baseURL = dbBase
		}
	}
	if key != "" {
		return key, baseURL, settings
	}
	var keys []model.APIKey
	if err := json.Unmarshal([]byte(keysJSON), &keys); err != nil {
		t.Skipf("解析 Mistral 密钥失败（%v）且未设置 MISTRAL_API_KEY，跳过", err)
	}
	for _, k := range keys {
		if k.Enabled && strings.TrimSpace(k.Key) != "" {
			return k.Key, baseURL, settings
		}
	}
	t.Skip("库里没有可用的 Mistral 密钥且未设置 MISTRAL_API_KEY，跳过")
	return "", "", settings
}

// liveTinyPNG 造一张极小的纯色 PNG，返回 data URL。
//
// 用纯色而不是噪声：这张图的目的是「占一个图片名额」，不是考验视觉能力。
// 纯色图 base64 后只有一百多字节，20 张的请求体也就几 KB，真实额度消耗最小。
//
// seed 会混进像素值。**这一点对缓存实验至关重要**：上游的 prompt cache 是跨请求
// 持久的，如果每次运行都用同一张图，那么第二次运行时缓存里已经躺着上一次的全部
// body，cached_tokens 会直接拉满、把「前缀是否复用」这件事完全掩盖掉
// （实测：同一份测试连跑两次，逐张策略的命中率从 0% 变成 99%）。
// 用 seed 保证每次运行的前缀都是全新的，测出来的才是真实的前缀复用行为。
func liveTinyPNG(t *testing.T, r, g, b uint8, seed int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 8, 8))
	for y := 0; y < 8; y++ {
		for x := 0; x < 8; x++ {
			// 把 seed 的低位混进像素，让整张图的字节随 seed 变化。
			img.Set(x, y, color.RGBA{
				R: r ^ uint8(seed),
				G: g ^ uint8(seed>>8),
				B: b ^ uint8(seed>>16),
				A: 255,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("编码 PNG 失败: %v", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// liveChatBody 拼一个带 n 张同样图片的 chat 请求体。
//
// seed 决定图片内容，用来隔离上游缓存（见 liveTinyPNG 的注释）。
func liveChatBody(t *testing.T, modelName string, n, seed int) []byte {
	t.Helper()
	url := liveTinyPNG(t, 200, 30, 30, seed)
	parts := make([]any, 0, n+1)
	parts = append(parts, map[string]any{
		"type": "text",
		"text": "这是一串图片。只回答一个词：ok",
	})
	for i := 0; i < n; i++ {
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": url},
		})
	}
	body := map[string]any{
		"model":      modelName,
		"max_tokens": 16,
		"messages": []any{
			map[string]any{"role": "user", "content": parts},
		},
	}
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化请求体失败: %v", err)
	}
	return out
}

// livePostChat 直接向上游发一次 chat 请求，返回状态码、响应体与 usage。
func livePostChat(t *testing.T, client *http.Client, target, key string, body []byte) (int, []byte, liveUsage) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("构造请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("请求上游失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))

	var probe struct {
		Usage struct {
			PromptTokens     int `json:"prompt_tokens"`
			CompletionTokens int `json:"completion_tokens"`
			PromptDetails    struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(raw, &probe)
	return resp.StatusCode, raw, liveUsage{
		PromptTokens: probe.Usage.PromptTokens,
		CachedTokens: probe.Usage.PromptDetails.CachedTokens,
		Completion:   probe.Usage.CompletionTokens,
	}
}

type liveUsage struct {
	PromptTokens int
	CachedTokens int
	Completion   int
}

// cachedRate 返回缓存命中率（百分比）。输入为 0 时返回 0。
func (u liveUsage) cachedRate() float64 {
	if u.PromptTokens <= 0 {
		return 0
	}
	return float64(u.CachedTokens) / float64(u.PromptTokens) * 100
}

// TestLiveMistralImageLimit 打真实 Mistral，验证上限与裁剪的四个假设。
func TestLiveMistralImageLimit(t *testing.T) {
	if os.Getenv("MISTRAL_LIVE") != "1" {
		t.Skip("未设置 MISTRAL_LIVE=1，跳过真实 Mistral 图片上限验证")
	}

	key, baseURL, settings := liveMistralTarget(t)
	modelName := os.Getenv("MISTRAL_LIVE_MODEL")
	if modelName == "" {
		// 上游真名是 v4-1（下划线在客户端那一侧，靠 modelMap 映射过去）。
		// 这里直接填上游真名，避免又多一层映射带来的干扰。
		modelName = "deepseek-v4-1-flash-1017"
	}

	// 走与转发同一套网络配置（含全局代理），否则墙内环境连不上。
	prov := model.Provider{
		Name:      "live-mistral",
		Enabled:   true,
		BaseURL:   baseURL,
		APIFormat: model.FormatOpenAI,
		Keys:      []model.APIKey{{ID: "live", Key: key, Enabled: true}},
	}
	prov.ApplyDefaults()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	srv := NewServer(st, hub.New(), quietLogger())
	netcfg := resolveNet(prov, settings)
	client, err := srv.clients.client(netcfg)
	if err != nil {
		t.Fatalf("初始化 client 失败: %v", err)
	}
	target, err := buildUpstreamURL(prov, "/v1/chat/completions", "")
	if err != nil {
		t.Fatalf("拼接上游地址失败: %v", err)
	}
	// 总时长给足：20 张图的预填充本身就很慢。
	client.Timeout = 240 * time.Second

	t.Logf("上游=%s 代理=%v(%s) 模型=%s", target, netcfg.ProxyInUse, netcfg.ProxyDesc, modelName)

	// 每次运行用一个独立 seed：上游 prompt cache 跨请求持久，复用同一批图会让
	// 后续运行直接吃到上一次留下的缓存，测不出「前缀是否被复用」。
	seed := int(time.Now().UnixNano() & 0xffffff)
	t.Logf("本次运行的图片 seed=%d（用于隔离上游缓存）", seed)

	// ---- 假设 1：上游真的会拒绝 21 张 ----
	t.Run("基准_超限被拒", func(t *testing.T) {
		body := liveChatBody(t, modelName, 21, seed)
		status, raw, _ := livePostChat(t, client, target, key, body)
		t.Logf("21 张 → HTTP %d %s", status, liveSnippet(raw))
		if status < 400 {
			t.Errorf("上游接受了 21 张图（HTTP %d）—— 20 这个上限可能已经变了，需要重新核对", status)
		}
		if !strings.Contains(strings.ToLower(string(raw)), "image") {
			t.Logf("注意：错误信息里没有 image 字样，可能是别的原因：%s", liveSnippet(raw))
		}
	})

	// ---- 假设 2：20 张能被接受（说明基准本身是通的）----
	t.Run("基准_刚好20张", func(t *testing.T) {
		body := liveChatBody(t, modelName, 20, seed)
		status, raw, usage := livePostChat(t, client, target, key, body)
		t.Logf("20 张 → HTTP %d usage=%+v %s", status, usage, liveSnippet(raw))
		if status != http.StatusOK {
			t.Fatalf("20 张就被拒了（HTTP %d）—— 上限可能比 20 更小：%s", status, liveSnippet(raw))
		}
	})

	// ---- 假设 3：裁剪后（占位形状）真的能过 ----
	t.Run("裁剪后能过", func(t *testing.T) {
		// 手工模拟 trimExcessImages 的产出：21 张里前 1 张换成占位文本。
		url := liveTinyPNG(t, 200, 30, 30, seed+1)
		parts := []any{
			map[string]any{"type": "text", "text": "这是一串图片。只回答一个词：ok"},
			map[string]any{
				"type": "text",
				"text": "[an earlier image was omitted: this request may carry at most 20 images]",
			},
		}
		for i := 0; i < 20; i++ {
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": url},
			})
		}
		body, _ := json.Marshal(map[string]any{
			"model":      modelName,
			"max_tokens": 16,
			"messages": []any{
				map[string]any{"role": "user", "content": parts},
			},
		})
		status, raw, usage := livePostChat(t, client, target, key, body)
		t.Logf("1 占位 + 20 图 → HTTP %d usage=%+v %s", status, usage, liveSnippet(raw))
		if status != http.StatusOK {
			t.Errorf("裁剪后的形状被拒（HTTP %d）—— 占位块可能不合法：%s", status, liveSnippet(raw))
		}
	})

	// ---- 假设 4：分批真的保住了前缀缓存 ----
	//
	// 直接对比两种策略在同一批窗口内的缓存命中率：
	//   A. 逐张（batch=0）：21→22→23 张，每轮丢弃数 1→2→3，边界每轮前移；
	//   B. 分批（batch=5）：21→22→23 张，每轮丢弃数恒定 5，边界不动。
	// 期望 B 在第二轮起 cached_tokens 明显高于 A。
	//
	// **两种策略必须用不同的 seed**：上游缓存是跨请求持久的，如果两种策略共用
	// 同一批图，后跑的那个会蹭到先跑的留下的缓存，测出来全是高命中率。
	// 实测踩过：没有 seed 隔离时连跑两次，逐张策略从 0% 变成 99%。
	t.Run("分批保住缓存", func(t *testing.T) {
		run := func(batch int, largeSeed int) []liveUsage {
			var got []liveUsage
			for Round := 0; Round < 3; Round++ {
				body := liveChatBodyTrimmed(t, modelName, 21+Round, 20, batch, largeSeed)
				status, raw, usage := livePostChat(t, client, target, key, body)
				if status != http.StatusOK {
					t.Fatalf("batch=%d 第 %d 轮被拒（HTTP %d）：%s", batch, Round, status, liveSnippet(raw))
				}
				got = append(got, usage)
				t.Logf("batch=%d 第 %d 轮（%d 张图）→ prompt=%d cached=%d (%.1f%%)",
					batch, Round, 21+Round, usage.PromptTokens, usage.CachedTokens, usage.cachedRate())
				// 上游写缓存需要一点时间，给它一个短暂的窗口。
				time.Sleep(2 * time.Second)
			}
			return got
		}

		// 两组用互不相同的 seed，确保任何一组都不会蹭到另一组的缓存。
		sequential := run(0, seed+100)
		batched := run(5, seed+200)

		// 取后两轮的平均命中率比较（第一轮通常 no-cache）。
		avg := func(us []liveUsage) float64 {
			if len(us) < 3 {
				return 0
			}
			return (us[1].cachedRate() + us[2].cachedRate()) / 2
		}
		seqRate, batRate := avg(sequential), avg(batched)
		t.Logf("后两轮平均缓存命中率：逐张 %.1f%% / 分批 %.1f%%", seqRate, batRate)

		// 两条断言，方向相反：
		//   - 分批必须真的命中（绝对门限），否则这个特性等于没做；
		//   - 分批不能比逐张还差（相对比较），兜住「上游缓存抖动」这种环境因素。
		// 只写相对比较的话，两边都 0% 也能通过 —— 那就完全丢了判别力。
		if batRate < 50 {
			t.Errorf("分批的缓存命中率只有 %.1f%%（期望 > 50%%）—— 量化没有保住前缀缓存，"+
				"逐张是 %.1f%%", batRate, seqRate)
		}
		if batRate+1 < seqRate {
			t.Errorf("分批的缓存命中率（%.1f%%）低于逐张（%.1f%%）—— 量化反而更差",
				batRate, seqRate)
		}
	})
}

// liveChatBodyTrimmed 造一个 n 张图的请求体，并按 max/batch 走一次真实的裁剪。
//
// 直接调用被测函数而不是手工拼：这样「测试打出去的是什么」与「代理实际发出去的
// 是什么」是同一段代码，不会因为手工模拟而与实现漂移。
//
// seed 决定图片内容：缓存实验里必须让每组的前缀互不相同，否则测到的是别人
// 留下的缓存而不是本轮的前缀复用。
func liveChatBodyTrimmed(t *testing.T, modelName string, n, maxImages, batch, seed int) []byte {
	t.Helper()
	url := liveTinyPNG(t, 200, 30, 30, seed)
	parts := make([]any, 0, n+1)
	parts = append(parts, map[string]any{
		"type": "text",
		"text": "这是一串图片。只回答一个词：ok",
	})
	for i := 0; i < n; i++ {
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": url},
		})
	}
	body := map[string]any{
		"model":      modelName,
		"max_tokens": 16,
		"messages": []any{
			map[string]any{"role": "user", "content": parts},
		},
	}
	dropped := trimExcessImages(body, maxImages, batch)
	t.Logf("  [裁剪] %d 张 → 丢 %d 张 → 剩 %d 张", n, dropped, n-dropped)
	out, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("序列化失败: %v", err)
	}
	return out
}

// liveSnippet 截断响应体用于日志，避免把整个错误页刷进测试输出。
func liveSnippet(raw []byte) string {
	s := strings.TrimSpace(string(raw))
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}
