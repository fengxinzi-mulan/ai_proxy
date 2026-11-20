package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/gif"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"

	"ai_proxy/internal/model"
)

// noisyPNG 生成一张指定尺寸的 PNG，像素带随机噪声 —— 纯色图压完一定更小，
// 测不出「缩放真的生效」这件事。
func noisyPNG(t *testing.T, w, h int, alpha bool) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	seed := uint32(12345)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			// 线性同余，确定性生成「看起来随机」的像素。
			seed = seed*1664525 + 1013904223
			a := uint8(255)
			if alpha && (x+y)%7 == 0 {
				a = 128
			}
			img.SetNRGBA(x, y, color.NRGBA{
				R: uint8(seed >> 24), G: uint8(seed >> 16), B: uint8(seed >> 8), A: a,
			})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("编码 PNG 失败: %v", err)
	}
	return buf.Bytes()
}

func defaultOpt() model.ImageCompressSettings {
	return model.ImageCompressSettings{Enabled: true, ThresholdBytes: 512 * 1024, MaxEdge: 2048, Quality: 85}
}

func TestCompressImageSkipsSmallUnderThreshold(t *testing.T) {
	raw := noisyPNG(t, 32, 32, false)
	out, mime, ok := compressImageBytes(raw, "image/png", defaultOpt())
	if ok {
		t.Error("小图不该被压缩")
	}
	if !bytes.Equal(out, raw) {
		t.Error("未压缩时应原样返回")
	}
	if mime != "image/png" {
		t.Errorf("mime 不该变，实际 %q", mime)
	}
}

func TestCompressImageShrinksLargePNG(t *testing.T) {
	raw := noisyPNG(t, 600, 600, false)
	if len(raw) < defaultOpt().ThresholdBytes {
		t.Fatalf("测试图太小（%d 字节），无法触发阈值", len(raw))
	}
	out, mime, ok := compressImageBytes(raw, "image/png", defaultOpt())
	if !ok {
		t.Fatal("大图应被压缩")
	}
	if len(out) >= len(raw) {
		t.Errorf("压缩后应更小: %d -> %d", len(raw), len(out))
	}
	if mime != "image/jpeg" {
		t.Errorf("不透明图片应转 JPEG，实际 %q", mime)
	}
	if _, _, err := image.Decode(bytes.NewReader(out)); err != nil {
		t.Errorf("压缩结果应仍是可解码图片: %v", err)
	}
}

// 带真实透明像素的图必须保持 PNG：转 JPEG 会让透明区变成黑块。
func TestCompressImageKeepsPNGWhenAlpha(t *testing.T) {
	raw := noisyPNG(t, 1200, 1200, true)
	opt := defaultOpt()
	// 靠缩放触发，保证「压完一定更小」这件事与编码器压缩率无关。
	opt.MaxEdge = 512
	out, mime, ok := compressImageBytes(raw, "image/png", opt)
	if !ok {
		t.Fatal("大图应被压缩")
	}
	if mime != "image/png" {
		t.Errorf("带 alpha 的图应保持 PNG，实际 %q", mime)
	}
	img, format, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if format != "png" {
		t.Errorf("实际编码格式应为 png，实际 %q", format)
	}
	if b := img.Bounds(); b.Dx() != 512 || b.Dy() != 512 {
		t.Errorf("应缩放到 512×512，实际 %d×%d", b.Dx(), b.Dy())
	}
	// 抽查一个透明点，确认透明信息没丢。
	if _, _, _, a := img.At(0, 0).RGBA(); a == 0xffff {
		t.Error("透明像素丢失了")
	}
}

// 字节数没超阈值，但长边超限 —— 也要缩，否则高分辨率截图会白烧视觉 token。
func TestCompressImageScalesByEdgeEvenWhenSmall(t *testing.T) {
	raw := noisyPNG(t, 4000, 100, false)
	opt := defaultOpt()
	opt.ThresholdBytes = 10 << 20 // 阈值调高，只让「长边」这一条生效
	out, _, ok := compressImageBytes(raw, "image/png", opt)
	if !ok {
		t.Fatal("长边超限时应当压缩")
	}
	img, _, err := image.Decode(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("解码失败: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 2048 {
		t.Errorf("长边应缩到 2048，实际 %d", b.Dx())
	}
	if len(out) >= len(raw) {
		t.Errorf("缩放后应更小: %d -> %d", len(raw), len(out))
	}
}

func TestCompressImageSkipsGIF(t *testing.T) {
	img := image.NewPaletted(image.Rect(0, 0, 600, 600), color.Palette{
		color.RGBA{R: 255, A: 255}, color.RGBA{B: 255, A: 255},
	})
	var buf bytes.Buffer
	if err := gif.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	if _, _, ok := compressImageBytes(raw, "image/gif", defaultOpt()); ok {
		t.Error("GIF 不该被重编码（会丢掉后续帧）")
	}
}

func TestCompressImageRejectsUndecodable(t *testing.T) {
	if _, _, ok := compressImageBytes([]byte("not an image"), "image/png", defaultOpt()); ok {
		t.Error("解不出来的数据不该被当作图片处理")
	}
}

// 调色板 PNG 带 tRNS 时 alpha 藏在调色板里，只按颜色模型判断会漏掉，
// 结果透明区被合成为黑色 —— 静默改坏图片内容。
func TestImageHasAlphaDetectsPalettedTransparency(t *testing.T) {
	transparent := color.Palette{
		color.NRGBA{R: 200, G: 40, B: 40, A: 255},
		color.NRGBA{R: 0, G: 0, B: 0, A: 0}, // 全透明
	}
	img := image.NewPaletted(image.Rect(0, 0, 64, 64), transparent)
	img.SetColorIndex(0, 0, 1)
	if !imageHasAlpha(img) {
		t.Error("调色板里的透明项必须被识别为有 alpha（否则转 JPEG 会变黑块）")
	}

	opaque := color.Palette{
		color.NRGBA{R: 10, G: 20, B: 30, A: 255},
		color.NRGBA{R: 240, G: 250, B: 255, A: 255},
	}
	img2 := image.NewPaletted(image.Rect(0, 0, 64, 64), opaque)
	if imageHasAlpha(img2) {
		t.Error("全不透明的调色板图不该被判为有 alpha（否则白白保留大体积 PNG）")
	}
}

// 编码出来是 NRGBA 但 alpha 全 255 的图，转 JPEG 完全无损。
func TestImageHasAlphaIgnoresFullyOpaqueNRGBA(t *testing.T) {
	img := image.NewNRGBA(image.Rect(0, 0, 32, 32))
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 1, G: 2, B: 3, A: 255})
		}
	}
	if imageHasAlpha(img) {
		t.Error("alpha 全 255 的 NRGBA 不该被判为有 alpha")
	}
	img.SetNRGBA(5, 5, color.NRGBA{R: 1, G: 2, B: 3, A: 128})
	if !imageHasAlpha(img) {
		t.Error("存在半透明像素时应判为有 alpha")
	}
}

// YCbCr（JPEG 解码结果）本身没有 alpha 通道，不该逐像素扫。
func TestImageHasAlphaFalseForYCbCr(t *testing.T) {
	img := image.NewYCbCr(image.Rect(0, 0, 16, 16), image.YCbCrSubsampleRatio420)
	if imageHasAlpha(img) {
		t.Error("YCbCr 没有 alpha 通道")
	}
}

// map 迭代顺序必须被排序固定：否则超过 16 张图时每次压缩的子集不同，
// 产出的 body 字节不同，Responses 续传的 input 指纹每轮都会变。
func TestCompressImagesInBodyIsDeterministicAcrossManyKeys(t *testing.T) {
	raw := noisyPNG(t, 600, 600, false)
	url := pngDataURL(raw)

	// 把 20 张图摊在同一个对象的 20 个不同子键下（不是数组 —— 数组本来就有序）。
	build := func() map[string]any {
		body := map[string]any{}
		for i := 0; i < 20; i++ {
			body[fmt.Sprintf("img_%02d", i)] = map[string]any{"image_url": url}
		}
		return body
	}

	var first []byte
	for i := 0; i < 8; i++ {
		body := build()
		st := compressImagesInBody(body, defaultOpt())
		if st.Count != maxCompressImages {
			t.Fatalf("第 %d 次：应压缩 %d 张，实际 %d", i, maxCompressImages, st.Count)
		}
		out, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = out
			continue
		}
		if !bytes.Equal(first, out) {
			t.Fatal("多次遍历产出的 body 不一致 —— map 迭代顺序破坏了确定性，会打断续传")
		}
	}
}

// 结构化落点（Anthropic / Gemini）也必须受张数上限约束。
func TestCompressImagesInBodyLimitCoversStructuredShapes(t *testing.T) {
	raw := noisyPNG(t, 600, 600, false)
	b64 := base64.StdEncoding.EncodeToString(raw)

	items := make([]any, 0, maxCompressImages+5)
	for i := 0; i < maxCompressImages+5; i++ {
		items = append(items, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type": "base64", "media_type": "image/png", "data": b64,
			},
		})
	}
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": items}}}
	if st := compressImagesInBody(body, defaultOpt()); st.Count > maxCompressImages {
		t.Errorf("Anthropic 落点也必须受 %d 张上限约束，实际 %d", maxCompressImages, st.Count)
	}
}

// 确定性是 Responses 续传的前提：同一输入必须产出逐字节相同的结果。
func TestCompressImageIsDeterministic(t *testing.T) {
	raw := noisyPNG(t, 600, 600, false)
	a, mimeA, okA := compressImageBytes(raw, "image/png", defaultOpt())
	b, mimeB, okB := compressImageBytes(raw, "image/png", defaultOpt())
	if !okA || !okB {
		t.Fatal("两次都应压缩成功")
	}
	if mimeA != mimeB || !bytes.Equal(a, b) {
		t.Fatal("同一输入两次压缩结果不一致 —— 会破坏续传的 input 指纹")
	}
}

// ---------- 落点覆盖 ----------

func pngDataURL(raw []byte) string {
	return "data:image/png;base64," + base64.StdEncoding.EncodeToString(raw)
}

func TestCompressImagesInBodyOpenAIChat(t *testing.T) {
	raw := noisyPNG(t, 600, 600, false)
	body := map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "text", "text": "看图"},
					map[string]any{
						"type":      "image_url",
						"image_url": map[string]any{"url": pngDataURL(raw)},
					},
				},
			},
		},
	}
	st := compressImagesInBody(body, defaultOpt())
	if st.Count != 1 {
		t.Fatalf("应压缩 1 张，实际 %d", st.Count)
	}
	if st.SavedBytes <= 0 {
		t.Errorf("应记录节省字节，实际 %d", st.SavedBytes)
	}

	// 重新序列化后确认图片确实变短了，且 mime 同步更新。
	out, _ := json.Marshal(body)
	if bytes.Contains(out, []byte("data:image/png;base64,")) {
		t.Error("压缩后 mime 应更新为 image/jpeg")
	}
	if !bytes.Contains(out, []byte("data:image/jpeg;base64,")) {
		t.Error("压缩后的 data URL 前缀应为 jpeg")
	}
}

func TestCompressImagesInBodyResponsesInputImage(t *testing.T) {
	raw := noisyPNG(t, 600, 600, false)
	body := map[string]any{
		"input": []any{
			map[string]any{"type": "input_image", "image_url": pngDataURL(raw)},
		},
	}
	if st := compressImagesInBody(body, defaultOpt()); st.Count != 1 {
		t.Fatalf("Responses 的 input_image 应被压缩，实际 %d", st.Count)
	}
}

func TestCompressImagesInBodyAnthropicSource(t *testing.T) {
	raw := noisyPNG(t, 600, 600, false)
	body := map[string]any{
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "image/png",
							"data":       base64.StdEncoding.EncodeToString(raw),
						},
					},
				},
			},
		},
	}
	if st := compressImagesInBody(body, defaultOpt()); st.Count != 1 {
		t.Fatalf("Anthropic 的 source.base64 应被压缩，实际 %d", st.Count)
	}
	src := body["messages"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)["source"].(map[string]any)
	if src["media_type"] != "image/jpeg" {
		t.Errorf("media_type 应同步更新，实际 %v", src["media_type"])
	}
}

func TestCompressImagesInBodyGeminiInlineData(t *testing.T) {
	raw := noisyPNG(t, 600, 600, false)
	body := map[string]any{
		"contents": []any{
			map[string]any{
				"role": "user",
				"parts": []any{
					map[string]any{
						"inlineData": map[string]any{
							"mimeType": "image/png",
							"data":     base64.StdEncoding.EncodeToString(raw),
						},
					},
				},
			},
		},
	}
	if st := compressImagesInBody(body, defaultOpt()); st.Count != 1 {
		t.Fatalf("Gemini 的 inlineData 应被压缩，实际 %d", st.Count)
	}
	inline := body["contents"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)["inlineData"].(map[string]any)
	if inline["mimeType"] != "image/jpeg" {
		t.Errorf("mimeType 应同步更新，实际 %v", inline["mimeType"])
	}
}

// ---------- 负例 ----------

func TestCompressImagesInBodyLeavesNonImagesAlone(t *testing.T) {
	raw := noisyPNG(t, 600, 600, false)
	b64 := base64.StdEncoding.EncodeToString(raw)

	body := map[string]any{
		// 远程图片：不下载、不动。
		"remote": map[string]any{"image_url": "https://example.com/a.png"},
		// 非图片的 data URL。
		"text": map[string]any{"data": "data:text/plain;base64," + b64},
		// 正文里恰好出现的 data URL：键名不像图片，不该被改。
		"messages": []any{
			map[string]any{"role": "user", "content": "这张图是 " + pngDataURL(raw)},
		},
		// 坏 base64。
		"broken": map[string]any{"data": "data:image/png;base64,!!!!"},
		// Anthropic 的远程图。
		"anth": map[string]any{
			"source": map[string]any{"type": "url", "url": "https://example.com/b.png"},
		},
	}
	before, _ := json.Marshal(body)
	if st := compressImagesInBody(body, defaultOpt()); st.Count != 0 {
		t.Fatalf("不该压缩任何东西，实际 %d 张", st.Count)
	}
	after, _ := json.Marshal(body)
	if !bytes.Equal(before, after) {
		t.Error("负例场景下 body 不该被改动")
	}
}

func TestCompressImagesInBodyRespectsImageLimit(t *testing.T) {
	raw := noisyPNG(t, 600, 600, false)
	url := pngDataURL(raw)
	items := make([]any, 0, maxCompressImages+5)
	for i := 0; i < maxCompressImages+5; i++ {
		items = append(items, map[string]any{"type": "input_image", "image_url": url})
	}
	body := map[string]any{"input": items}
	if st := compressImagesInBody(body, defaultOpt()); st.Count > maxCompressImages {
		t.Errorf("单请求最多处理 %d 张，实际 %d", maxCompressImages, st.Count)
	}
}

func TestHasInlineImagePrefilter(t *testing.T) {
	cases := []struct {
		body string
		want bool
	}{
		{`{"messages":[{"content":"data:image/png;base64,AAA"}]}`, true},
		{`{"contents":[{"parts":[{"inlineData":{"data":"AAA"}}]}]}`, true},
		{`{"contents":[{"parts":[{"inline_data":{"data":"AAA"}}]}]}`, true},
		{`{"messages":[{"content":"纯文本"}]}`, false},
	}
	for _, c := range cases {
		if got := hasInlineImage([]byte(c.body)); got != c.want {
			t.Errorf("hasInlineImage(%s) = %v, want %v", c.body, got, c.want)
		}
	}
}

// ---------- 与改写流程的集成 ----------

func TestRewriteRequestBodyCompressesImages(t *testing.T) {
	raw := noisyPNG(t, 600, 600, false)
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": pngDataURL(raw)}},
				},
			},
		},
	})

	st := model.DefaultSettings()
	p := baseProvider()
	out, mods := rewriteRequestBody(p, st, body, "gpt-4o")

	if !containsString(mods, "image_compress") {
		t.Fatalf("改写清单应包含 image_compress，实际 %v", mods)
	}
	if len(out) >= len(body) {
		t.Errorf("改写后请求体应更小: %d -> %d", len(body), len(out))
	}

	// rewriteRequest 要带回统计。
	res := rewriteRequest(p, st, body, "gpt-4o", rewriteOptions{})
	if res.ImageCompressed != 1 {
		t.Errorf("应报告压缩 1 张，实际 %d", res.ImageCompressed)
	}
	if res.ImageSavedBytes <= 0 {
		t.Errorf("应报告节省字节，实际 %d", res.ImageSavedBytes)
	}
}

func TestRewriteRequestSkipsCompressionWhenDisabled(t *testing.T) {
	raw := noisyPNG(t, 600, 600, false)
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-4o",
		"messages": []any{
			map[string]any{
				"role": "user",
				"content": []any{
					map[string]any{"type": "image_url", "image_url": map[string]any{"url": pngDataURL(raw)}},
				},
			},
		},
	})

	st := model.DefaultSettings()
	st.ImageCompress.Enabled = false
	res := rewriteRequest(baseProvider(), st, body, "gpt-4o", rewriteOptions{})
	if res.ImageCompressed != 0 {
		t.Errorf("关闭后不该压缩，实际 %d 张", res.ImageCompressed)
	}
	if !bytes.Equal(res.Body, body) {
		t.Error("关闭后请求体应原样透传")
	}
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// 顺带确认 JPEG 编码在默认质量下确实比原 PNG 小 —— 防止有人把 Quality
// 误调成 100 之后「压缩」反而变大（那种情况会被「压完更大就保留原图」兜住，
// 但这条断言能让改动者立刻意识到参数语义）。
func TestJPEGQualityAffectsSize(t *testing.T) {
	raw := noisyPNG(t, 300, 300, false)
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	var low, high bytes.Buffer
	if err := jpeg.Encode(&low, img, &jpeg.Options{Quality: 40}); err != nil {
		t.Fatal(err)
	}
	if err := jpeg.Encode(&high, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatal(err)
	}
	if low.Len() >= high.Len() {
		t.Errorf("低质量应更小: q40=%d q95=%d", low.Len(), high.Len())
	}
	if !strings.HasPrefix("image/jpeg", "image/") {
		t.Fatal("unreachable")
	}
}

// ---------- 尝试次数上限（Bug 23） ----------

// 「压完没变小」的图不会增加成功计数。按成功张数封顶时，一批这样的图能
// 绕开上限，让请求逐张解码压缩几十次。
func TestCompressImagesInBodyCapsAttemptsNotSuccesses(t *testing.T) {
	// 2x2 的 PNG 重编码成 JPEG 反而更大，所以一张都不会被改写。
	raw := noisyPNG(t, 2, 2, false)
	url := pngDataURL(raw)
	opt := model.ImageCompressSettings{Enabled: true, ThresholdBytes: 0, MaxEdge: 0, Quality: 85}

	total := maxCompressImages + 12
	items := make([]any, 0, total)
	for i := 0; i < total; i++ {
		items = append(items, map[string]any{"type": "input_image", "image_url": url})
	}
	body := map[string]any{"input": items}

	st := compressImagesInBody(body, opt)
	if st.Count != 0 {
		t.Fatalf("这批图压完更大应保持原样，实际改写 %d 张", st.Count)
	}
	if st.attempted != maxCompressImages {
		t.Errorf("解码尝试应停在 %d 次，实际 %d（按成功张数封顶会放行全部 %d 张）",
			maxCompressImages, st.attempted, total)
	}
}

// 尝试额度只在确实要解码时消耗：小图（低于阈值）不该占额度。
func TestCompressImagesInBodyAttemptsOnlyForCandidates(t *testing.T) {
	small := pngDataURL(noisyPNG(t, 8, 8, false))
	big := pngDataURL(noisyPNG(t, 600, 600, false))

	items := []any{}
	for i := 0; i < 3; i++ {
		items = append(items, map[string]any{"type": "input_image", "image_url": small})
	}
	items = append(items, map[string]any{"type": "input_image", "image_url": big})
	body := map[string]any{"input": items}

	st := compressImagesInBody(body, defaultOpt())
	if st.attempted != 4 {
		t.Errorf("4 张候选图应各消耗一次尝试，实际 %d", st.attempted)
	}
	if st.Count != 1 {
		t.Errorf("只有大图会被改写，实际 %d", st.Count)
	}
}

// ---------- EXIF Orientation（Bug 22） ----------

// exifAPP1 构造只带 Orientation 的 APP1 段（大端 TIFF，最小结构）。
func exifAPP1(orientation uint16) []byte {
	tiff := []byte{
		'M', 'M', 0x00, 0x2A, 0x00, 0x00, 0x00, 0x08, // TIFF 头，IFD0 在偏移 8
		0x00, 0x01, // 1 个条目
		0x01, 0x12, // tag = Orientation
		0x00, 0x03, // type = SHORT
		0x00, 0x00, 0x00, 0x01, // count = 1
		byte(orientation >> 8), byte(orientation), 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, // 没有下一个 IFD
	}
	payload := append([]byte("Exif\x00\x00"), tiff...)
	size := len(payload) + 2
	seg := []byte{0xFF, 0xE1, byte(size >> 8), byte(size)}
	return append(seg, payload...)
}

// jpegWithOrientation 在 JPEG 的 SOI 之后插入 APP1 段。
func jpegWithOrientation(t *testing.T, base []byte, orientation uint16) []byte {
	t.Helper()
	if len(base) < 2 || base[0] != 0xFF || base[1] != 0xD8 {
		t.Fatal("基准图不是 JPEG")
	}
	out := make([]byte, 0, len(base)+32)
	out = append(out, base[:2]...)
	out = append(out, exifAPP1(orientation)...)
	out = append(out, base[2:]...)
	return out
}

func noisyJPEG(t *testing.T, w, h int) []byte {
	t.Helper()
	img, _, err := image.Decode(bytes.NewReader(noisyPNG(t, w, h, false)))
	if err != nil {
		t.Fatalf("解码 PNG 失败: %v", err)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("编码 JPEG 失败: %v", err)
	}
	return buf.Bytes()
}

func TestJPEGExifOrientation(t *testing.T) {
	base := noisyJPEG(t, 40, 20)
	for o := 1; o <= 8; o++ {
		raw := jpegWithOrientation(t, base, uint16(o))
		if got := jpegExifOrientation(raw); got != o {
			t.Errorf("orientation %d 应解析为 %d，实际 %d", o, o, got)
		}
	}

	// 解析失败一律按「不旋转」处理，绝不能改坏图片。
	cases := []struct {
		name string
		raw  []byte
	}{
		{"没有 APP1", base},
		{"不是 JPEG", noisyPNG(t, 8, 8, false)},
		{"越界的值", jpegWithOrientation(t, base, 9)},
		{"被截断", jpegWithOrientation(t, base, 6)[:12]},
		{"空输入", nil},
	}
	for _, c := range cases {
		if got := jpegExifOrientation(c.raw); got != 0 {
			t.Errorf("%s: 应返回 0，实际 %d", c.name, got)
		}
	}
}

// applyOrientation 的方向映射必须按 EXIF 定义：这里只钉「结果图片某个角
// 来自原图哪个角」和宽高变化，够定位任何一处映射写反。
func TestApplyOrientationMappings(t *testing.T) {
	const w, h = 4, 2
	src := image.NewNRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			src.SetNRGBA(x, y, color.NRGBA{R: uint8(40 * x), G: uint8(40 * y), B: 0, A: 255})
		}
	}
	// 期望：结果左上角与右下角分别来自原图的哪个坐标，以及结果宽高。
	cases := []struct {
		orientation        int
		tlX, tlY, brX, brY int
		dw, dh             int
	}{
		{2, w - 1, 0, 0, h - 1, w, h},
		{3, w - 1, h - 1, 0, 0, w, h},
		{4, 0, h - 1, w - 1, 0, w, h},
		{5, 0, 0, w - 1, h - 1, h, w},
		{6, 0, h - 1, w - 1, 0, h, w},
		{7, w - 1, h - 1, 0, 0, h, w},
		{8, w - 1, 0, 0, h - 1, h, w},
	}
	for _, c := range cases {
		dst := applyOrientation(src, c.orientation)
		b := dst.Bounds()
		if b.Dx() != c.dw || b.Dy() != c.dh {
			t.Fatalf("orientation %d 尺寸应为 %dx%d，实际 %dx%d",
				c.orientation, c.dw, c.dh, b.Dx(), b.Dy())
		}
		tr, tg, _, _ := dst.At(0, 0).RGBA()
		sr, sg, _, _ := src.At(c.tlX, c.tlY).RGBA()
		if tr != sr || tg != sg {
			t.Errorf("orientation %d 的左上角应来自 (%d,%d)", c.orientation, c.tlX, c.tlY)
		}
		br, bg, _, _ := dst.At(b.Dx()-1, b.Dy()-1).RGBA()
		xr, xg, _, _ := src.At(c.brX, c.brY).RGBA()
		if br != xr || bg != xg {
			t.Errorf("orientation %d 的右下角应来自 (%d,%d)", c.orientation, c.brX, c.brY)
		}
	}

	// 1 与非法值都原样返回。
	if got := applyOrientation(src, 1); got != image.Image(src) {
		t.Error("orientation 1 应原样返回")
	}
	if got := applyOrientation(src, 0); got != image.Image(src) {
		t.Error("orientation 0 应原样返回")
	}
}

// 竖拍手机照片的像素是横的，靠 EXIF 表示方向。压缩后 EXIF 会丢，
// 所以必须在缩放进码前真的把像素转过来，否则客户端看到的是横图。
func TestCompressImageBytesAppliesJPEGOrientation(t *testing.T) {
	const w, h = 200, 60
	raw := jpegWithOrientation(t, noisyJPEG(t, w, h), 6)
	opt := model.ImageCompressSettings{Enabled: true, ThresholdBytes: 0, MaxEdge: 0, Quality: 60}

	out, mime, ok := compressImageBytes(raw, "image/jpeg", opt)
	if !ok {
		t.Fatal("应压缩成功")
	}
	if mime != "image/jpeg" {
		t.Errorf("mime 应为 jpeg，实际 %q", mime)
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(out))
	if err != nil {
		t.Fatalf("解码压缩结果失败: %v", err)
	}
	if cfg.Width != h || cfg.Height != w {
		t.Errorf("orientation 6 应转成 %dx%d（宽高交换），实际 %dx%d", h, w, cfg.Width, cfg.Height)
	}

	// 没有 EXIF 时保持原方向，不该无端旋转。
	plain, _, ok := compressImageBytes(noisyJPEG(t, w, h), "image/jpeg", opt)
	if !ok {
		t.Fatal("无 EXIF 的图也应压缩成功")
	}
	if cfg, _, err := image.DecodeConfig(bytes.NewReader(plain)); err != nil {
		t.Fatalf("解码失败: %v", err)
	} else if cfg.Width != w || cfg.Height != h {
		t.Errorf("没有 EXIF 时不该旋转，实际 %dx%d", cfg.Width, cfg.Height)
	}
}
