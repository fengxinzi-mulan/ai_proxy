package proxy

// 前缀审计：把「代理实际发出的字节」与「上游的缓存命中量」逐轮对齐。
//
// 起因是一个具体的怀疑：真实日志里出现 cached_tokens **绝对值下降**的现象
// （例如 prompt 从 158K 涨到 168K，cached 却从 156K 掉到 65K），而且这些轮次
// 都恰好带 image_compress。65K 附近是个固定值，说明前缀在某个固定位置断掉了，
// 不像随机的上游抖动。
//
// 这个用例把三件事摆在一起看，缺一不可：
//
//  1. **代理实际发出的字节**（从日志的 req_body 取，即真正的线上字节），
//     与上一轮相比「共同前缀到哪儿」—— 这是判断「我们有没有改坏前缀」的唯一依据；
//  2. **同一批字节直接打上游**（对照组，绕过代理），看 cached_tokens 是否稳定；
//  3. **图片压缩开 / 关两组**，隔离「压缩是不是元凶」。
//
// 判定方式：
//   - 若代理字节逐轮几乎是上一轮的严格延长（共同前缀到上一轮的 99%+），
//     而上游 cached_tokens 仍然掉 → 上游的问题；
//   - 若共同前缀在中途就断（比如恰好断在第一张图的位置）→ 我们的问题，
//     而且断点位置直接指出是哪一项改写干的。
//
// 默认跳过（会消耗真实额度，几十万 token 量级）：
//
//	MISTRAL_LIVE=1 PREFIX_AUDIT_LIVE=1 \
//	  go test ./internal/proxy -run TestLiveMistralPrefixAudit -v -count=1 -timeout 2400s
//
// 可调：
//
//	PREFIX_AUDIT_ROUNDS=12   轮数（默认 12）
//	PREFIX_AUDIT_IMAGES=9    历史里放的图片张数（默认 9，对齐真实日志的 9 张）
//	PREFIX_AUDIT_PAD=12      每轮追加的填充段数（默认 12，约 27K tokens/轮）

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"ai_proxy/internal/hub"
	"ai_proxy/internal/model"
	"ai_proxy/internal/store"
)

// auditRound 是一轮的审计记录。
type auditRound struct {
	Round int
	// 代理侧
	ProxyPrompt int
	ProxyCached int
	SentBody    []byte
	ImageComp   int
	Mods        []string
	// 与上一轮发出的字节相比
	CommonPrefix int // 共同前缀长度（字节）
	PrevLen      int // 上一轮总长
	FirstDiff    string
	// 对照组：同一批字节直接打上游
	DirectPrompt int
	DirectCached int
}

// prefixAuditHeader 让直接请求与代理请求在会话标识上对齐。
//
// 代理会把客户端的 X-Session-Id 派生成 prompt_cache_key 注入请求体，
// 所以对照组直接复用「代理发出的整份 body」即可 —— 里面已经带着那个 key。
func TestLiveMistralPrefixAudit(t *testing.T) {
	if os.Getenv("PREFIX_AUDIT_LIVE") != "1" {
		t.Skip("未设置 PREFIX_AUDIT_LIVE=1，跳过前缀审计")
	}
	if os.Getenv("MISTRAL_LIVE") != "1" {
		t.Skip("未设置 MISTRAL_LIVE=1，跳过真实 Mistral 验证")
	}

	key, baseURL, settings := liveMistralTarget(t)
	modelName := os.Getenv("MISTRAL_LIVE_MODEL")
	if modelName == "" {
		modelName = "deepseek-v4-1-flash-1017"
	}
	rounds := cacheLiveEnvInt("PREFIX_AUDIT_ROUNDS", 12)
	imageCount := cacheLiveEnvInt("PREFIX_AUDIT_IMAGES", 9)
	padCount := cacheLiveEnvInt("PREFIX_AUDIT_PAD", 12)

	// 两组：图片压缩开 / 关。其余配置完全一致。
	for _, compress := range []bool{true, false} {
		name := "压缩关"
		if compress {
			name = "压缩开"
		}
		t.Run(name, func(t *testing.T) {
			runPrefixAudit(t, key, baseURL, settings, modelName, rounds, imageCount, padCount, compress)
		})
	}
}

// runPrefixAudit 跑一组完整的多轮审计。
func runPrefixAudit(t *testing.T, key, baseURL string, settings model.Settings,
	modelName string, rounds, imageCount, padCount int, compress bool) {
	t.Helper()

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// 必须落报文：req_body 是我们唯一能拿到「线上真实字节」的途径。
	// 上限给到 4MB —— 100K+ 上下文的请求体有几百 KB，默认 64KB 会被截断，
	// 截断后的字节没法用来比对前缀。
	settings.StoreBodies = true
	settings.MaxBodyBytes = 4 << 20
	if err := st.SaveSettings(settings); err != nil {
		t.Fatalf("写入设置失败: %v", err)
	}

	prov := model.Provider{
		Name:                   "audit-mistral",
		DisplayName:            "audit-mistral",
		Enabled:                true,
		BaseURL:                baseURL,
		APIFormat:              model.FormatOpenAI,
		Keys:                   []model.APIKey{{ID: "live", Key: key, Enabled: true}},
		StripUnsupportedParams: true,
		// 不设图片上限：本组要验证的就是「没超限时缓存是否稳定」。
		MaxImagesPerRequest: 0,
	}
	prov.ApplyDefaults()
	if _, err := st.CreateProvider(prov); err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}

	// 图片压缩这个变量由本组决定。
	stg, _ := st.GetSettings()
	stg.ImageCompress.Enabled = compress
	if err := st.SaveSettings(stg); err != nil {
		t.Fatalf("写压缩开关失败: %v", err)
	}

	srv := NewServer(st, hub.New(), quietLogger())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	client := &http.Client{Timeout: 600 * time.Second}

	runSeed := int(time.Now().UnixNano() & 0xfffff)
	session := fmt.Sprintf("audit-%d-%d", runSeed, time.Now().UnixNano())
	t.Logf("模型=%s 轮数=%d 图片=%d 压缩=%v 批次=%d", modelName, rounds, imageCount, compress, runSeed)

	// 图片只造一次；真实客户端每轮回放的是同一份字节。
	images := make([]string, 0, imageCount)
	for i := 0; i < imageCount; i++ {
		images = append(images, auditImage(t, runSeed*100+i))
	}

	history := []any{
		map[string]any{"role": "system", "content": "你在一个长会话中，回答简短。"},
		map[string]any{"role": "user", "content": "先看这几张图，后面我会连续提问。"},
	}
	{
		parts := make([]any, 0, len(images)+1)
		parts = append(parts, map[string]any{"type": "text", "text": "图片如下："})
		for _, url := range images {
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": url},
			})
		}
		history = append(history, map[string]any{"role": "user", "content": parts})
	}
	history = append(history, map[string]any{"role": "assistant", "content": "收到，我已经看过这些图片。"})

	var records []auditRound

	for round := 1; round <= rounds; round++ {
		for p := 0; p < padCount; p++ {
			history = append(history,
				map[string]any{"role": "user", "content": auditFiller(runSeed, round, p)},
				map[string]any{"role": "assistant", "content": fmt.Sprintf("第 %d 段已知悉。", p)})
		}
		turn := append(append([]any{}, history...),
			map[string]any{"role": "user", "content": fmt.Sprintf("这是第 %d 轮。只回答数字：%d", round, round)})

		body, err := json.Marshal(map[string]any{
			"model":      modelName,
			"max_tokens": 32,
			"stream":     false,
			"messages":   turn,
		})
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}

		// ---- 经代理发一次 ----
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/chat/completions", bytes.NewReader(body))
		if err != nil {
			t.Fatalf("构造请求失败: %v", err)
		}
		req.Header.Set("Content-Type", "application/json")
		if settings.AccessKey != "" {
			req.Header.Set("Authorization", "Bearer "+settings.AccessKey)
		}
		req.Header.Set("X-Session-Id", session)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatalf("第 %d 轮代理请求失败: %v", round, err)
		}
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 轮代理返回 %d: %s", round, resp.StatusCode, liveSnippet(raw))
		}
		proxyPrompt, proxyCached := parseUsage(raw)

		entry := lastLog(t, st)
		if !entry.RequestModified && entry.ReqBody == "" {
			t.Fatalf("第 %d 轮没有拿到 req_body —— 无法审计线上字节", round)
		}
		sent := []byte(entry.ReqBody)
		// req_body 落库时可能带截断标记，出现就说明上限不够，结论不可信。
		if strings.Contains(entry.ReqBody, "内容超出上限") || strings.Contains(entry.ReqBody, "已截断") {
			t.Fatalf("第 %d 轮的 req_body 被截断（%d 字节）—— 提高 MaxBodyBytes 后重跑",
				round, len(entry.ReqBody))
		}
		if len(sent) == 0 {
			t.Fatalf("第 %d 轮的 req_body 为空", round)
		}

		rec := auditRound{
			Round:       round,
			ProxyPrompt: proxyPrompt,
			ProxyCached: proxyCached,
			SentBody:    sent,
			ImageComp:   entry.ImageCompressed,
			Mods:        entry.Modifications,
		}

		// ---- 与上一轮发出的字节比对共同前缀 ----
		if len(records) > 0 {
			prev := records[len(records)-1].SentBody
			rec.PrevLen = len(prev)
			rec.CommonPrefix, rec.FirstDiff = commonPrefixInfo(prev, sent)
		}

		records = append(records, rec)

		// 共同前缀占上一轮的比例：接近 100% 说明我们没改坏前缀。
		stable := 0.0
		if rec.PrevLen > 0 {
			stable = float64(rec.CommonPrefix) / float64(rec.PrevLen) * 100
		}
		t.Logf("第 %2d 轮 代理: prompt=%7d cached=%7d | 上轮body=%6d 共同前缀=%6d (%5.1f%%) 压缩=%d",
			rec.Round, rec.ProxyPrompt, rec.ProxyCached, rec.PrevLen, rec.CommonPrefix, stable, rec.ImageComp)
		if rec.FirstDiff != "" && stable < 99.9 {
			t.Logf("         ↳ 首个差异处：%s", rec.FirstDiff)
		}
	}

	// ---- 对照组：把所有轮次的字节重放一遍，直连上游 ----
	//
	// 关键设计：**代理全部跑完之后**才做直连重放，而且是同一批字节按同样顺序。
	// 直连第 N 轮之前，缓存里已经有「代理第 N 轮」和「直连第 N-1 轮」两份，
	// 但两轮的字节是同一份、prompt_cache_key 也相同，所以它们指向同一个缓存链，
	// 直连看到的命中量就是「这份字节本身能命中多少」。
	//
	// 反过来做（第 N 轮先直连再代理）就没意义了：直连刚刚写热了缓存，代理
	// 在第二轮拿到的高命中来自直连，而不是自己写进去的。
	t.Log("────────── 对照组：同样的字节直连上游（代理跑完之后重放）──────────")
	for i := range records {
		r := &records[i]
		dPrompt, dCached := directPostChat(t, client, baseURL+"/chat/completions", key, r.SentBody)
		r.DirectPrompt, r.DirectCached = dPrompt, dCached
		t.Logf("第 %2d 轮 直连: prompt=%7d cached=%7d | 代理 cached=%7d | 差=%d",
			r.Round, dPrompt, dCached, r.ProxyCached, dCached-r.ProxyCached)
	}

	// ---- 汇总 ----
	t.Log("───────────────────────────────────────────────────────────────")
	t.Log("轮次  prompt   代理cached  直连cached  代理/上轮prompt  共同前缀%  压缩")
	minStable := 101.0
	for i := range records {
		r := &records[i]
		st := 100.0
		if r.PrevLen > 0 {
			st = float64(r.CommonPrefix) / float64(r.PrevLen) * 100
		}
		if r.PrevLen > 0 && st < minStable {
			minStable = st
		}
		// 代理侧「命中量 / 上一轮 prompt」——健康时应当接近 100%，
		// 因为上一轮的全部内容都在这一轮的历史里。
		ratio := 0.0
		if i > 0 && records[i-1].ProxyPrompt > 0 {
			ratio = float64(r.ProxyCached) / float64(records[i-1].ProxyPrompt) * 100
		}
		t.Logf("%4d %8d %10d %11d %14.1f%% %10.2f%% %4d",
			r.Round, r.ProxyPrompt, r.ProxyCached, r.DirectCached, ratio, st, r.ImageComp)
	}

	// 判定 1：我们发出的字节逐轮应当几乎是上一轮的严格延长。
	// 只允许尾部那点 JSON 闭合差异（实测远小于 1%）。
	if minStable < 98 {
		t.Errorf("代理发出的字节在某一轮只与上一轮共享 %.2f%% 的前缀 —— 前缀被我们改坏了",
			minStable)
	}

	// 判定 2：代理与直连的缓存表现应当一致。
	// 若代理 cached 明显低于直连 cached，说明是代理引入的差异；
	// 若两者都掉，是上游行为。
	for _, r := range records[1:] {
		diff := r.DirectCached - r.ProxyCached
		if diff > 2000 {
			t.Logf("注意：第 %d 轮直连 cached(%d) 比代理 cached(%d) 高 %d —— 代理侧引入了差异",
				r.Round, r.DirectCached, r.ProxyCached, diff)
		}
	}
}

// parseUsage 从响应里取出 prompt / cached tokens。
func parseUsage(raw []byte) (prompt, cached int) {
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
	cached = v.Usage.PromptDetails.CachedTokens
	if cached == 0 {
		cached = v.Usage.CachedTokens
	}
	return v.Usage.PromptTokens, cached
}

// directPostChat 把一份现成的 body 直接打上游（不经代理），返回 usage。
func directPostChat(t *testing.T, client *http.Client, url, key string, body []byte) (int, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("构造直连请求失败: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("直连请求失败: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		t.Logf("直连返回 %d: %s", resp.StatusCode, liveSnippet(raw))
		return 0, 0
	}
	return parseUsage(raw)
}

// commonPrefixInfo 返回两份字节的共同前缀长度，以及差异处的上下文片段。
func commonPrefixInfo(a, b []byte) (int, string) {
	lim := len(a)
	if len(b) < lim {
		lim = len(b)
	}
	i := 0
	for i < lim && a[i] == b[i] {
		i++
	}
	if i >= lim {
		// 短的那份整个都是长的那份的前缀。
		return i, ""
	}
	lo := i - 40
	if lo < 0 {
		lo = 0
	}
	hiA := i + 40
	if hiA > len(a) {
		hiA = len(a)
	}
	hiB := i + 40
	if hiB > len(b) {
		hiB = len(b)
	}
	return i, fmt.Sprintf("offset=%d\n          上轮: %q\n          本轮: %q",
		i, string(a[lo:hiA]), string(b[lo:hiB]))
}

// auditImage 造一张小 PNG 的 data URL。
func auditImage(t *testing.T, seed int) string {
	t.Helper()
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
		t.Fatalf("编码 PNG 失败: %v", err)
	}
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(buf.Bytes())
}

// auditFiller 造一段足够长的填充文本（每段唯一）。
func auditFiller(runSeed, round, idx int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "第 %d 轮第 %d 段背景材料（批次 %d）：用于把上下文撑到十万 token 量级，"+
		"不含需要记忆的信息，仅模拟一个正在进行的长会话。区分编号 r%d-p%d。",
		round, idx, runSeed, round, idx)
	for k := 0; k < 34; k++ {
		fmt.Fprintf(&sb,
			"（第 %d 段之第 %d 小节，批次 %d）关于编号 r%d-p%d-%d 的说明文字，"+
				"此处无需关注，它只是用来占位的叙述性文本，目的在于让整份历史足够长，"+
				"便于观察长上下文下前缀缓存的实际表现。", idx, k, runSeed, round, idx, k)
	}
	return sb.String()
}
