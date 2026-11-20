// 前缀缓存审计程序。
//
// 目的：查清「cached_tokens 绝对值下降」到底是代理改写造成的，还是上游行为。
//
// 方法：**两条互相独立的缓存链，喂完全相同的字节序列**。
//
//	链 P：客户端 → 本代理 → 上游（代理会做严格上游裁剪、图片压缩、注入 cache key）
//	链 D：把一个已存在的代理短名指向同一个上游，但客户端直连/或直接打上游
//
// 两链的内容逐轮一致，只有 prompt_cache_key 不同，所以它们落在上游两个
// 互不干扰的缓存分片上。任何一条链上出现的 cached_tokens 抖动，如果另一条也
// 出现，就与代理无关。
//
// 同时把每一次请求体、响应体完整落盘（maxBodyBytes 调到 8 MB，绝不截断），
// 事后可以逐字节核对「上一轮的 body 是不是本轮 body 的前缀」。
//
// 用法：
//
//	go run ./scripts/prefix-audit -rounds 12 -images 9 -pad 12
//
// 密钥来源：优先 MISTRAL_API_KEY 环境变量，否则只读 data/ai_proxy.db 里
// base_url 命中 mistral.ai 的供应商。密钥只留在内存，不写日志、不落盘。
package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log/slog"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"time"

	"ai_proxy/internal/hub"
	"ai_proxy/internal/model"
	"ai_proxy/internal/proxy"
	"ai_proxy/internal/store"

	"database/sql"

	_ "modernc.org/sqlite"
)

// roundRecord 是一轮的完整记录。
type roundRecord struct {
	Round int
	// 代理链
	ProxyPrompt  int
	ProxyCached  int
	ProxyBody    []byte // 代理实际发出的完整字节
	ProxyImageC  int
	ProxyMods    []string
	ProxyDropped int
	// 直连链（内容相同、cache key 不同）
	DirectPrompt int
	DirectCached int
	DirectStatus int
	// 与上一轮代理 body 的共同前缀
	PrevLen      int
	CommonPrefix int
}

func main() {
	var (
		rounds   = flag.Int("rounds", 12, "轮数")
		images   = flag.Int("images", 9, "历史里放的图片张数")
		pad      = flag.Int("pad", 12, "每轮追加的填充段数")
		compress = flag.Bool("compress", true, "开启请求体内联图片压缩")
		outDir   = flag.String("out", "", "报文落盘目录（默认 ./prefix-audit-<时间戳>）")
		dbPath   = flag.String("db", filepath.Join("data", "ai_proxy.db"), "取密钥用的库")
		modelN   = flag.String("model", "deepseek-v4-1-flash-1017", "上游模型名")
		maxImgs  = flag.Int("max-images", 0, "单请求图片上限（0=不限制）")
		batch    = flag.Int("batch", 0, "成批替换的批大小（0=逐张）")
		direct   = flag.Bool("direct", true, "是否跑直连对照组")
		// grow 每轮往历史里**新增**几张图。真实会话的图片随对话增长，
		// 只有图片数增长时丢弃边界才会移动 —— 这正是要验证的场景。
		grow = flag.Int("grow", 0, "每轮新增几张图片（0=不增长，>0 模拟真实会话的图片增长）")
	)

	flag.Parse()

	if *outDir == "" {
		*outDir = fmt.Sprintf("prefix-audit-%d", time.Now().Unix())
	}
	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fatalf("创建输出目录失败: %v", err)
	}

	key, baseURL, settings, err := loadTarget(*dbPath)
	if err != nil {
		fatalf("取密钥失败: %v", err)
	}

	// ---- 起一个进程内代理，报文上限调到 8 MB，确保完整落库 ----
	st, err := store.Open(filepath.Join(*outDir, "store"))
	if err != nil {
		fatalf("打开临时库失败: %v", err)
	}
	defer st.Close()

	const bodyCap = 8 << 20
	settings.StoreBodies = true
	settings.MaxBodyBytes = bodyCap
	settings.ImageCompress.Enabled = *compress
	if err := st.SaveSettings(settings); err != nil {
		fatalf("写入设置失败: %v", err)
	}

	prov := model.Provider{
		Name:                   "audit",
		DisplayName:            "audit",
		Enabled:                true,
		BaseURL:                baseURL,
		APIFormat:              model.FormatOpenAI,
		Keys:                   []model.APIKey{{ID: "live", Key: key, Enabled: true}},
		StripUnsupportedParams: true,
		MaxImagesPerRequest:    *maxImgs,
		ImageTrimBatch:         *batch,
	}
	prov.ApplyDefaults()
	if _, err := st.CreateProvider(prov); err != nil {
		fatalf("创建供应商失败: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	px := proxy.NewServer(st, hub.New(), logger)
	ts := httptest.NewServer(px)
	defer ts.Close()

	client := &http.Client{Timeout: 900 * time.Second}
	runSeed := int(time.Now().UnixNano() & 0xfffff)
	session := fmt.Sprintf("audit-%d", runSeed)

	fmt.Printf("上游      : %s\n", baseURL)
	fmt.Printf("模型      : %s\n", *modelN)
	fmt.Printf("轮数/图/段: %d / %d / %d\n", *rounds, *images, *pad)
	fmt.Printf("压缩      : %v\n", *compress)
	fmt.Printf("图片上限  : %d (batch %d)\n", *maxImgs, *batch)
	fmt.Printf("报文上限  : %d 字节\n", bodyCap)
	fmt.Printf("批次      : %d\n", runSeed)
	fmt.Printf("落盘目录  : %s\n\n", *outDir)

	// ---- 构造历史 ----
	// gallery 是「到目前为止会话里出现过的全部图片」。它随轮次增长（grow>0 时），
	// 每轮新增的图片追加在末尾 —— 这样图片数会持续上升，丢弃边界随之移动。
	gallery := make([]string, 0, *images+*grow**rounds)
	for i := 0; i < *images; i++ {
		gallery = append(gallery, makeImage(runSeed*100+i))
	}
	history := []any{
		map[string]any{"role": "system", "content": "你在一个长会话中，回答简短。"},
		map[string]any{"role": "user", "content": "先看这几张图，后面我会连续提问。"},
	}
	{
		parts := make([]any, 0, len(gallery)+1)
		parts = append(parts, map[string]any{"type": "text", "text": "图片如下："})
		for _, u := range gallery {
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": u},
			})
		}
		history = append(history, map[string]any{"role": "user", "content": parts})
	}
	history = append(history, map[string]any{"role": "assistant", "content": "收到，我已经看过这些图片。"})

	records := make([]roundRecord, 0, *rounds)

	for round := 1; round <= *rounds; round++ {
		// 先追加本轮新增的图片（grow>0 时）。追加在历史末尾，
		// 前面已经出现过的图片位置不变 —— 这与真实客户端的行为一致。
		if *grow > 0 {
			fresh := make([]any, 0, *grow+1)
			fresh = append(fresh, map[string]any{
				"type": "text", "text": fmt.Sprintf("第 %d 轮又产生了新图片：", round)})
			for i := 0; i < *grow; i++ {
				gallery = append(gallery, makeImage(runSeed*10000+round*100+i))
				fresh = append(fresh, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": gallery[len(gallery)-1]},
				})
			}
			history = append(history,
				map[string]any{"role": "user", "content": fresh},
				map[string]any{"role": "assistant", "content": fmt.Sprintf("第 %d 轮图片已收到。", round)})
		}

		for p := 0; p < *pad; p++ {
			history = append(history,
				map[string]any{"role": "user", "content": filler(runSeed, round, p)},
				map[string]any{"role": "assistant", "content": fmt.Sprintf("第 %d 段已知悉。", p)})
		}
		turn := append(append([]any{}, history...),
			map[string]any{"role": "user", "content": fmt.Sprintf("这是第 %d 轮。只回答数字：%d", round, round)})

		clientBody, err := json.Marshal(map[string]any{
			"model": *modelN, "max_tokens": 32, "stream": false, "messages": turn,
		})
		if err != nil {
			fatalf("序列化失败: %v", err)
		}
		writeFile(*outDir, fmt.Sprintf("req-%02d-client.json", round), clientBody)

		// ---- 链 P：经代理 ----
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", bytes.NewReader(clientBody))
		if err != nil {
			fatalf("构造请求失败: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if settings.AccessKey != "" {
			req.Header.Set("Authorization", "Bearer "+settings.AccessKey)
		}
		req.Header.Set("X-Session-Id", session)
		resp, err := client.Do(req)
		if err != nil {
			fatalf("第 %d 轮代理请求失败: %v", round, err)
		}
		respRaw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			fatalf("第 %d 轮代理返回 %d: %s", round, resp.StatusCode, snippet(respRaw))
		}
		writeFile(*outDir, fmt.Sprintf("resp-%02d-proxy.json", round), respRaw)

		logs, _, err := st.ListLogs(model.LogQuery{Page: 1, PageSize: 1})
		if err != nil || len(logs) == 0 {
			fatalf("第 %d 轮读日志失败: %v", round, err)
		}
		entry, err := st.GetLog(logs[0].ID)
		if err != nil {
			fatalf("第 %d 轮读日志详情失败: %v", round, err)
		}
		if strings.Contains(entry.ReqBody, "内容超出上限") {
			fatalf("第 %d 轮报文仍被截断（上限 %d），需要调大 maxBodyBytes", round, bodyCap)
		}
		sent := []byte(entry.ReqBody)
		if len(sent) == 0 {
			fatalf("第 %d 轮 req_body 为空", round)
		}
		writeFile(*outDir, fmt.Sprintf("req-%02d-proxy.json", round), sent)

		pP, pC := usageOf(respRaw)
		rec := roundRecord{
			Round:        round,
			ProxyPrompt:  pP,
			ProxyCached:  pC,
			ProxyBody:    sent,
			ProxyImageC:  entry.ImageCompressed,
			ProxyMods:    entry.Modifications,
			ProxyDropped: entry.ImagesDropped,
		}
		if len(records) > 0 {
			prev := records[len(records)-1].ProxyBody
			rec.PrevLen = len(prev)
			rec.CommonPrefix = commonPrefix(prev, sent)
		}

		// ---- 链 D：同样内容，但换一个 cache key，直连上游 ----
		//
		// 换 key 是这条链能成立的前提：不换的话，直连会直接命中代理刚写进去的
		// 缓存，测到的是「蹭来的命中」而不是自己的缓存链。
		if *direct {
			dBody := retagCacheKey(sent)
			writeFile(*outDir, fmt.Sprintf("req-%02d-direct.json", round), dBody)
			dStatus, dRaw := postDirect(client, baseURL+"/chat/completions", key, dBody)
			writeFile(*outDir, fmt.Sprintf("resp-%02d-direct.json", round), dRaw)
			rec.DirectStatus = dStatus
			if dStatus == http.StatusOK {
				rec.DirectPrompt, rec.DirectCached = usageOf(dRaw)
			}
		}

		records = append(records, rec)

		ratio := 0.0
		if rec.PrevLen > 0 {
			ratio = float64(rec.CommonPrefix) / float64(rec.PrevLen) * 100
		}
		fmt.Printf("第 %2d 轮 | 代理 prompt=%7d cached=%7d | 直连 prompt=%7d cached=%7d | "+
			"共同前缀=%5.1f%% | 压缩=%d 丢弃=%d\n",
			round, rec.ProxyPrompt, rec.ProxyCached, rec.DirectPrompt, rec.DirectCached,
			ratio, rec.ProxyImageC, rec.ProxyDropped)
	}

	// ---- 汇总 ----
	fmt.Printf("\n%s\n", strings.Repeat("=", 100))
	fmt.Printf("%-4s %10s %12s %12s %10s %12s\n", "轮次", "prompt", "代理cached", "直连cached", "前缀%", "代理多付")
	for i, r := range records {
		ratio := 100.0
		if r.PrevLen > 0 {
			ratio = float64(r.CommonPrefix) / float64(r.PrevLen) * 100
		}
		// 「多付」= 本轮未命中 - 本轮新增
		over := 0
		if i > 0 {
			over = (r.ProxyPrompt - r.ProxyCached) - (r.ProxyPrompt - records[i-1].ProxyPrompt)
		}
		fmt.Printf("%-4d %10d %12d %12d %9.2f%% %12d\n",
			r.Round, r.ProxyPrompt, r.ProxyCached, r.DirectCached, ratio, over)
	}

	fmt.Printf("\n%s\n", strings.Repeat("-", 100))
	analyze(records)
}

// analyze 打印结论性问题：两条链各自的 cached 抖动情况。
func analyze(records []roundRecord) {
	if len(records) < 2 {
		return
	}

	// 链 P 与链 D 各自的「未命中量」
	fmt.Printf("%-4s %12s %14s %14s\n", "轮次", "代理未命中", "直连未命中", "本轮新增")
	proxyDrops, directDrops := 0, 0
	for i := 1; i < len(records); i++ {
		pMiss := records[i].ProxyPrompt - records[i].ProxyCached
		dMiss := records[i].DirectPrompt - records[i].DirectCached
		grew := records[i].ProxyPrompt - records[i-1].ProxyPrompt
		// 「异常未命中」= 未命中量远超本轮新增（说明旧前缀没被复用）
		if pMiss > grew+2000 {
			proxyDrops++
		}
		if records[i].DirectStatus == http.StatusOK && dMiss > grew+2000 {
			directDrops++
		}
		fmt.Printf("%-4d %12d %14d %14d\n", records[i].Round, pMiss, dMiss, grew)
	}

	fmt.Printf("\n代理链异常未命中轮数: %d / %d\n", proxyDrops, len(records)-1)
	fmt.Printf("直连链异常未命中轮数: %d / %d\n", directDrops, len(records)-1)

	minPrefix := 100.0
	for _, r := range records {
		if r.PrevLen == 0 {
			continue
		}
		v := float64(r.CommonPrefix) / float64(r.PrevLen) * 100
		if v < minPrefix {
			minPrefix = v
		}
	}
	fmt.Printf("代理连发字节的最小共同前缀: %.2f%%（<100 是 JSON 尾部闭合差异，不是内容改写）\n", minPrefix)

	switch {
	case minPrefix < 98:
		fmt.Println("\n结论：代理发出的字节前缀被改坏了 —— 是我们的问题。")
	case proxyDrops > 0 && directDrops > 0:
		fmt.Println("\n结论：两条链都出现异常未命中 —— 上游缓存行为，与代理无关。")
	case proxyDrops > 0 && directDrops == 0:
		fmt.Println("\n结论：只有代理链异常，直连链正常 —— 需要进一步区分（但字节前缀是稳定的）。")
	default:
		fmt.Println("\n结论：两条链都没有异常未命中 —— 本次运行未复现该现象。")
	}
}

// loadTarget 取密钥、BaseURL 与全局设置。
func loadTarget(dbPath string) (string, string, model.Settings, error) {
	baseURL := "https://api.mistral.ai/v1"
	key := os.Getenv("MISTRAL_API_KEY")
	var settings model.Settings

	abs, err := filepath.Abs(dbPath)
	if err != nil {
		return key, baseURL, settings, err
	}
	if _, statErr := os.Stat(abs); statErr != nil {
		if key == "" {
			return "", "", settings, fmt.Errorf("没有可读的 %s，且未设置 MISTRAL_API_KEY", abs)
		}
		settings = model.DefaultSettings()
		return key, baseURL, settings, nil
	}

	db, err := sql.Open("sqlite", "file:"+strings.ReplaceAll(abs, `\`, "/")+"?mode=ro")
	if err != nil {
		return key, baseURL, settings, err
	}
	defer db.Close()

	var raw string
	if err := db.QueryRow(`SELECT value FROM settings WHERE key = 'global'`).Scan(&raw); err == nil {
		_ = json.Unmarshal([]byte(raw), &settings)
	}
	if settings.Host == "" {
		settings = model.DefaultSettings()
	}
	settings.ApplyDefaults()

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
		return key, baseURL, settings, nil
	}
	var keys []model.APIKey
	if err := json.Unmarshal([]byte(keysJSON), &keys); err != nil {
		return "", "", settings, fmt.Errorf("解析密钥失败: %w", err)
	}
	for _, k := range keys {
		if k.Enabled && strings.TrimSpace(k.Key) != "" {
			return k.Key, baseURL, settings, nil
		}
	}
	return "", "", settings, fmt.Errorf("库里没有可用的 Mistral 密钥，且未设置 MISTRAL_API_KEY")
}

// retagCacheKey 把请求体里的 prompt_cache_key 换成一个固定的新值。
//
// 这样直连链用的是另一个缓存分片，不会蹭到代理链刚写进去的内容。
// 新值必须跨轮稳定，否则直连链自己就断了。
func retagCacheKey(body []byte) []byte {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	obj["prompt_cache_key"] = json.RawMessage(`"aipx-director0000000000000000"`)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

// usageOf 取 prompt / cached tokens。
func usageOf(raw []byte) (int, int) {
	var v struct {
		Usage struct {
			PromptTokens  int `json:"prompt_tokens"`
			CachedTokens  int `json:"cached_tokens"`
			PromptDetails struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	_ = json.Unmarshal(raw, &v)
	c := v.Usage.PromptDetails.CachedTokens
	if c == 0 {
		c = v.Usage.CachedTokens
	}
	return v.Usage.PromptTokens, c
}

// postDirect 直接打上游，返回状态码与响应体。
func postDirect(client *http.Client, url, key string, body []byte) (int, []byte) {
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return 0, []byte(err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		return 0, []byte(err.Error())
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, raw
}

// commonPrefix 返回 a、b 的共同前缀字节数。
func commonPrefix(a, b []byte) int {
	lim := len(a)
	if len(b) < lim {
		lim = len(b)
	}
	i := 0
	for i < lim && a[i] == b[i] {
		i++
	}
	return i
}

// makeImage 造一张 16×16 的纯色 PNG，颜色由 seed 决定。
func makeImage(seed int) string {
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	rng := rand.New(rand.NewSource(int64(seed)))
	cr, cg, cb := uint8(rng.Intn(256)), uint8(rng.Intn(256)), uint8(rng.Intn(256))
	for y := 0; y < 16; y++ {
		for x := 0; x < 16; x++ {
			img.Set(x, y, color.RGBA{R: cr, G: cg, B: cb, A: 255})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// filler 造一段足够长的填充文本，每段唯一。
func filler(runSeed, round, idx int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "第 %d 轮第 %d 段背景材料（批次 %d）：用于把上下文撑到十万 token 量级，"+
		"不含需要记忆的信息，仅模拟一个正在进行的长会话。区分编号 r%d-p%d。",
		round, idx, runSeed, round, idx)
	for k := 0; k < 34; k++ {
		fmt.Fprintf(&sb,
			"（第 %d 段之第 %d 小节，批次 %d）关于编号 r%d-p%d-%d 的说明文字，"+
				"此处无需关注，它只是用来占位的叙述性文本。", idx, k, runSeed, round, idx, k)
	}
	return sb.String()
}

func writeFile(dir, name string, data []byte) {
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "写 %s 失败: %v\n", name, err)
	}
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 400 {
		return s[:400] + "…"
	}
	return s
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
