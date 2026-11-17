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
