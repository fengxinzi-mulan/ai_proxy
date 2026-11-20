package proxy

// 多轮大上下文的真实缓存验证：经本程序代理，上下文 > 100K、轮数 > 10。
//
// 这个用例要回答一个具体的怀疑：**图片没超过 20 张时，缓存命中率为什么不是 100%。**
//
// 做法是尽量贴近真实客户端：每轮把完整历史连同其中的图片重新回放，只在尾部
// 追加新一轮的问答。图片放在历史**中段**（前面先垫一段文本），而不是开头 ——
// 真实会话里图片通常是中途才出现的，这个位置对前缀缓存最敏感：如果任何一步
// 改写了图片之前的字节，缓存就会整段失效。
//
// 每轮记录 usage 里的 prompt_tokens / cached_tokens，并同时读回本代理写下的
// 日志（改写清单、压缩张数、丢弃张数），把「上游表现」与「我们做了什么」对齐。
//
// 默认跳过，只在显式开启时运行（会消耗真实额度，几十万 token 量级）：
//
//	MISTRAL_LIVE=1 CACHE_MULTI_LIVE=1 \
//	  go test ./internal/proxy -run TestLiveMistralMultiTurnCache -v -timeout 1800s
//
// 可调：
//
//	CACHE_LIVE_ROUNDS=12      轮数（默认 12）
//	CACHE_LIVE_TARGET=110000  每轮后期望的输入 token 量级（默认 110000）
//	CACHE_LIVE_IMAGES=8       历史里放的图片张数（默认 8，刻意低于 20）

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
	"strconv"
	"strings"
	"testing"
	"time"

	"ai_proxy/internal/hub"
	"ai_proxy/internal/model"
	"ai_proxy/internal/store"
)

// cacheTurnUsage 是一轮的用量与耗时。
type cacheTurnUsage struct {
	Round        int
	PromptTokens int
	CachedTokens int
	Rate         float64
	LatencyMs    int64
	SentBytes    int
	// 代理侧记录，用于对齐「我们改了什么」。
	Modifications   []string
	ImageCompressed int
	ImagesDropped   int
}

// cacheLiveEnvInt 读一个整数环境变量，缺省或非法时用默认值。
func cacheLiveEnvInt(name string, def int) int {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return def
}

// cacheFillerParagraph 造一段填充文本。
//
// 用无意义但**每段不同**的句子：上下文要真的涨到 100K，同时不能因为每轮文本
// 都一样而让上游把不同轮次混为一谈。
//
// 单段要足够长：目标 110K tokens 除以轮数×每轮段数，反推出每段大约需要多长。
// 经验值：中文约 1.5 字/token，所以每段 2000 字 ≈ 1300 tokens。
//
// runSeed 混进每一段：上游 prompt cache 跨请求持久，如果两次运行的填充文本
// 完全一样，后一次运行的第一轮就会直接命中前一次留下的缓存（实测：首轮命中
// 99.6%），「第一轮应当无缓存」这个基准也就不成立了。
func cacheFillerParagraph(runSeed, round, idx int) string {
	var sb strings.Builder
	fmt.Fprintf(&sb,
		"第 %d 轮的第 %d 段背景材料（批次 %d）：本段用于把上下文撑到十万 token 量级，"+
			"内容本身不含任何需要记忆的信息，仅用于模拟一个正在进行的长会话。"+
			"为了避免重复内容影响分词统计，这里插入一个区分编号 r%d-p%d。",
		round, idx, runSeed, round, idx)
	// 继续把这个段落撑长，保证 12 轮 × 12 段足以达到 100K。
	for k := 0; k < 34; k++ {
		fmt.Fprintf(&sb,
			"（第 %d 段之第 %d 小节，批次 %d）关于编号 r%d-p%d-%d 的说明文字，此处不需要任何关注，"+
				"它只是一段用来占位的叙述性文本，目的在于让整份历史达到足够长的规模，"+
				"以便观察长上下文下前缀缓存的实际表现。", idx, k, runSeed, round, idx, k)
	}
	return sb.String()
}

// cacheTinyImage 造一张小 PNG 的 data URL，颜色由 seed 决定。
func cacheTinyImage(t *testing.T, seed int) string {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 16, 16))
	rng := rand.New(rand.NewSource(int64(seed)))
	cr := uint8(rng.Intn(256))
	cg := uint8(rng.Intn(256))
	cb := uint8(rng.Intn(256))
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

// TestLiveMistralMultiTurnCache 模拟一个 100K+、10+ 轮的真实会话，逐轮观察缓存。
func TestLiveMistralMultiTurnCache(t *testing.T) {
	if os.Getenv("CACHE_MULTI_LIVE") != "1" {
		t.Skip("未设置 CACHE_MULTI_LIVE=1，跳过多轮缓存验证")
	}
	if os.Getenv("MISTRAL_LIVE") != "1" {
		t.Skip("未设置 MISTRAL_LIVE=1，跳过真实 Mistral 验证")
	}

	key, baseURL, settings := liveMistralTarget(t)
	modelName := os.Getenv("MISTRAL_LIVE_MODEL")
	if modelName == "" {
		modelName = "deepseek-v4-1-flash-1017"
	}
	rounds := cacheLiveEnvInt("CACHE_LIVE_ROUNDS", 12)
	targetTokens := cacheLiveEnvInt("CACHE_LIVE_TARGET", 110000)
	imageCount := cacheLiveEnvInt("CACHE_LIVE_IMAGES", 8)

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	// 用真实库里的供应商配置（含 modelMap、严格上游兼容等），只把地址与密钥换成真的。
	prov := model.Provider{
		Name:                   "cache-mistral",
		DisplayName:            "cache-mistral",
		Enabled:                true,
		BaseURL:                baseURL,
		APIFormat:              model.FormatOpenAI,
		Keys:                   []model.APIKey{{ID: "live", Key: key, Enabled: true}},
		StripUnsupportedParams: true,
		// 刻意的设置：不开启图片数量上限（图片只有 8 张，本就不到 20），
		// 只开图片压缩 —— 这正是用户截图里的配置形态。
		MaxImagesPerRequest: 0,
		ImageTrimBatch:      0,
	}
	prov.ApplyDefaults()
	if _, err := st.CreateProvider(prov); err != nil {
		t.Fatalf("创建供应商失败: %v", err)
	}
	if err := st.SaveSettings(settings); err != nil {
		t.Fatalf("写入设置失败: %v", err)
	}

	srv := NewServer(st, hub.New(), quietLogger())
	ts := httptest.NewServer(srv)
	defer ts.Close()

	client := &http.Client{Timeout: 600 * time.Second}
	// runSeed 让两次运行的填充文本与图片都不同：上游缓存跨请求持久，
	// 不隔离的话第一轮就会蹭到上一次的缓存，基准不成立。
	runSeed := int(time.Now().UnixNano() & 0xfffff)
	session := fmt.Sprintf("cache-multi-%d-%d", runSeed, time.Now().UnixNano())
	t.Logf("上游=%s 模型=%s 轮数=%d 目标上下文=%d tokens 历史图片=%d 张 批次=%d",
		baseURL, modelName, rounds, targetTokens, imageCount, runSeed)

	// 图片只造一次：客户端每轮回放的是同一批图片的**同一份字节**
	// （真实客户端也不会每轮重新编码），这是缓存能命中的前提。
	images := make([]string, 0, imageCount)
	for i := 0; i < imageCount; i++ {
		images = append(images, cacheTinyImage(t, runSeed*100+i))
	}

	// history 是不断增长的消息数组，每轮把它完整发出去。
	// 结构：system → 一段文本 → 图片（中段）→ 若干问答 → 本轮新问题。
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
	history = append(history,
		map[string]any{"role": "assistant", "content": "收到，我已经看过这些图片。"})

	var logIDs []int64
	records := make([]cacheTurnUsage, 0, rounds)

	for round := 1; round <= rounds; round++ {
		// 每轮往历史里垫一些填充材料，让上下文稳步增长到目标量级。
		// 填充放在「图片之后」，这样图片位置固定，任何前缀失效都不可能来自填充。
		//
		// 段数按目标反推：每段约 3.4K 字 ≈ 2.2K tokens，所以每轮垫 12 段 ≈ 26K tokens，
		// 第 4 轮就能过 100K，后面 8 轮都在 100K 以上 —— 保证绝大多数轮次落在
		// 「大上下文」这个前提里。
		padCount := 0
		for padCount < 12 {
			history = append(history,
				map[string]any{"role": "user", "content": cacheFillerParagraph(runSeed, round, padCount)})
			history = append(history,
				map[string]any{"role": "assistant", "content": fmt.Sprintf("第 %d 段已知悉。", padCount)})
			padCount++
		}
		question := fmt.Sprintf("这是第 %d 轮。请只回答数字：%d", round, round)
		turn := append(append([]any{}, history...),
			map[string]any{"role": "user", "content": question})

		body, err := json.Marshal(map[string]any{
			"model":      modelName,
			"max_tokens": 32,
			"stream":     false,
			"messages":   turn,
		})
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}

		start := time.Now()
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
			t.Fatalf("第 %d 轮请求失败: %v", round, err)
		}
		raw, _ := readAllLimited(resp.Body, 1<<20)
		resp.Body.Close()
		latency := time.Since(start).Milliseconds()

		if resp.StatusCode != http.StatusOK {
			t.Fatalf("第 %d 轮返回 %d: %s", round, resp.StatusCode, liveSnippet(raw))
		}

		var parsed struct {
			Usage struct {
				PromptTokens  int `json:"prompt_tokens"`
				CachedTokens  int `json:"cached_tokens"`
				PromptDetails struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			t.Fatalf("第 %d 轮响应不是合法 JSON: %v", round, err)
		}
		cached := parsed.Usage.PromptDetails.CachedTokens
		if cached == 0 {
			cached = parsed.Usage.CachedTokens
		}

		entry := lastLog(t, st)
		rec := cacheTurnUsage{
			Round:           round,
			PromptTokens:    parsed.Usage.PromptTokens,
			CachedTokens:    cached,
			LatencyMs:       latency,
			SentBytes:       len(body),
			Modifications:   entry.Modifications,
			ImageCompressed: entry.ImageCompressed,
			ImagesDropped:   entry.ImagesDropped,
		}
		if rec.PromptTokens > 0 {
			rec.Rate = float64(rec.CachedTokens) / float64(rec.PromptTokens) * 100
		}
		records = append(records, rec)
		logIDs = append(logIDs, entry.ID)

		t.Logf("第 %2d 轮：prompt=%7d cached=%7d 命中=%5.1f%% 耗时=%5dms 压缩=%d 丢弃=%d 改写=%v",
			round, rec.PromptTokens, rec.CachedTokens, rec.Rate, latency,
			rec.ImageCompressed, rec.ImagesDropped, rec.Modifications)
	}

	// ---- 汇总分析 ----
	//
	// 关键认知：`cached_tokens / prompt_tokens` 这个比值**不能**用来判断缓存是否健康。
	// 分母是「全部历史 + 本轮新增」，分子只是「历史」，所以只要每轮还在往历史里
	// 加东西，这个比值就必然小于 100%，且上下文越长它越高 —— 它衡量的是
	// 「新增内容占比」，不是「缓存有没有被打断」。
	//
	// 真正该看的是**历史复用率**：本轮命中的量，是否等于上一轮的全部 prompt。
	// 如果前缀被完整复用，cached_tokens 应当 ≈ 上一轮的 prompt_tokens，
	// 缺口只应该等于「本轮新增的那部分」。
	last := records[len(records)-1]
	t.Logf("最终上下文 %d tokens（目标 %d）", last.PromptTokens, targetTokens)
	if last.PromptTokens < 100000 {
		t.Errorf("上下文没有达到 100K（实际 %d tokens）—— 这条用例的前提没满足，结论不可信",
			last.PromptTokens)
	}
	if len(records) <= 10 {
		t.Errorf("轮数不足 10（实际 %d）", len(records))
	}

	// 逐轮相邻对比：缺口 = 未命中量 - 本轮新增量。
	// 缺口接近 0 就说明「上一轮的全部历史都被复用了」，缓存完全健康。
	//
	// 第一轮是基准：本轮没有任何历史可复用，命中率必须接近 0。它不为 0 就说明
	// 这次运行蹭到了外部缓存（上一次运行、或别的客户端），后面的结论全部作废。
	if records[0].Rate > 20 {
		t.Fatalf("第 1 轮命中率 %.1f%% —— 首轮不该有缓存，说明本次运行蹭到了外部缓存，"+
			"结论不可信（需要换一个 runSeed 重跑）", records[0].Rate)
	}
	var maxGap int
	var worst cacheTurnUsage
	var worstGap int
	for i := 1; i < len(records); i++ {
		prev, cur := records[i-1], records[i]
		missed := cur.PromptTokens - cur.CachedTokens
		grew := cur.PromptTokens - prev.PromptTokens
		gap := missed - grew
		if gap < 0 {
			gap = 0
		}
		if gap > maxGap {
			maxGap = gap
			worst = cur
			worstGap = gap
		}
		t.Logf("  第 %d→%d 轮：新增 %d，未命中 %d，多付 %d（0 = 历史被完整复用）",
			prev.Round, cur.Round, grew, missed, gap)
	}

	t.Logf("逐轮最大「多付」= %d tokens（第 %d 轮）", maxGap, worst.Round)
	// 允许多付一点点：分词边界、上游缓存按 token 块对齐都会带来几十 token 的误差。
	// 但绝不该出现「多付几千甚至上万」—— 那才是前缀真的断了。
	if maxGap > 2000 {
		t.Errorf("有一轮多付了 %d tokens（第 %d 轮，上下文 %d）—— 前缀缓存被打断："+
			"未命中 %d，而本轮新增只有 %d",
			worstGap, worst.Round, worst.PromptTokens,
			worst.PromptTokens-worst.CachedTokens,
			worst.PromptTokens-records[worst.Round-2].PromptTokens)
	}

	// 全程不该有任何改写：图片只有 8 张（未超上限），本用例就是要验证
	// 「代理什么都不做时，缓存是健康的」。
	for _, r := range records {
		if r.ImagesDropped != 0 {
			t.Errorf("第 %d 轮被裁了 %d 张图 —— 图片数没超上限，不该触发裁剪",
				r.Round, r.ImagesDropped)
		}
		if len(r.Modifications) != 0 {
			t.Errorf("第 %d 轮发生了改写 %v —— 本用例的供应商没有开任何改写项，不该有",
				r.Round, r.Modifications)
		}
	}
}

// readAllLimited 读响应体，最多 limit 字节（避免异常响应把内存吃穿）。
func readAllLimited(r io.Reader, limit int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, limit))
}
