package proxy

// 请求体内联图片压缩。
//
// 为什么值得做：多模态请求里图片常以 base64 内联在请求体里，一张手机截图
// 动辄 2–4 MB，而模型侧真正消费的分辨率远低于原始像素。把长边压到 2048、
// 重编码成 JPEG 后通常只剩原来的 10%–20%，首字节延迟和上行带宽都跟着降。
//
// 三条硬约束：
//  1. **确定性**。同一张输入图每次必须产出完全相同的字节。Go 的 jpeg/png 编码器
//     不写时间戳、不引入随机，所以只要输入和参数不变，输出就逐字节相同 ——
//     Responses 续传靠「input 项指纹」判断前缀是否接得上，一旦压缩结果抖动，
//     每轮指纹都会变，续传会全部失效。改这里时不要引入任何随机/时钟因素。
//  2. **只碰图片**。遍历整棵 JSON，但只处理「键名像图片」且「值确实是 data:image/
//     URL 或 base64 图片」的落点；正文文本里恰好出现的 data URL 不动。
//  3. **失败即放弃**。解不出来、太大、GIF、压完更大 —— 一律保留原图，
//     绝不因为一次压缩失败把用户的请求改坏。

import (
	"bytes"
	"encoding/base64"
	"image"
	"image/jpeg"
	"image/png"
	"sort"
	"strings"

	"ai_proxy/internal/model"

	"golang.org/x/image/draw"
	// 注册 webp 解码器：客户端会直接贴 webp 图片，不注册就只能整张跳过。
	_ "golang.org/x/image/webp"
)

const (
	// maxCompressPixels 是单张图片的像素上限（约 50MP）。超过就不解码 ——
	// 解码一张 100MP 的图要吃掉几百 MB 内存，而它大概率是攻击或误操作。
	maxCompressPixels = 50_000_000
	// maxCompressImages 是单次请求最多处理的图片张数。
	// 多图会话（Codex 一次贴十几张）很常见，但再多的请求本身就该被限流了。
	maxCompressImages = 16
	// maxCompressBase64 是单张图片 base64 串的长度上限（约 20 MB 原始字节）。
	maxCompressBase64 = 20 << 20
)

// imageKeys 是「值可能是图片」的键名。
//
// 覆盖到的真实形状：
//   - OpenAI chat: {"type":"image_url","image_url":{"url":"data:image/png;base64,…"}}
//     以及简写 {"image_url":"data:image/png;base64,…"}
//   - Responses: {"type":"input_image","image_url":"data:image/png;base64,…"}
//   - 部分网关: {"image":"data:image/png;base64,…"} / {"b64_json":"…"} / {"image_data":"…"}
//
// Anthropic 与 Gemini 的形状不同（base64 在 source.data / inlineData.data 里），
// 由下面单独的分支处理，不靠这张表。
var imageKeys = map[string]bool{
	"url":        true,
	"image_url":  true,
	"image":      true,
	"data":       true,
	"b64_json":   true,
	"image_data": true,
}

// hasInlineImage 是廉价的字节预筛：没命中就不必把整个 body 解析成 JSON。
//
// 多扫几个词的成本可以忽略；误命中时后面解析完发现没有可压缩的图片也不会改写。
// 标记要**足够具体**：早先这里有个裸的 `"source"`，而它在代码片段、工具 schema
// 里出现频率很高，会让大量纯文本请求白白走一遍 decodeJSONObject（128MB 上限下
// 内存放大好几倍）。改成只匹配 Anthropic 的实际形状。
func hasInlineImage(body []byte) bool {
	for _, marker := range []string{
		"data:image/", `"inlineData"`, `"inline_data"`,
		`"media_type"`, `"mediaType"`, `"b64_json"`, `"image_data"`,
		// Anthropic 的 source 必须带 base64 类型才可能是内联图；
		// 远程图（type:"url"）本来就不处理，没必要为它付出解析成本。
		`"type":"base64"`, `"type": "base64"`,
	} {
		if bytes.Contains(body, []byte(marker)) {
			return true
		}
	}
	return false
}

// compressStats 汇总一次请求里的压缩结果。
type compressStats struct {
	// Count 是实际被改写的图片张数。
	Count int
	// SavedBytes 是 base64 串长度的净减少量，也就是实际省下的请求字节。
	SavedBytes int64
}

// compressImagesInBody 递归遍历请求体，压缩其中所有内联图片。
//
// 直接原地改 obj，调用方负责序列化。返回的统计用于日志留痕。
func compressImagesInBody(obj map[string]any, opt model.ImageCompressSettings) compressStats {
	var st compressStats
	walkImages(obj, opt, &st)
	return st
}

// walkImages 递归遍历任意 JSON 节点。
//
// map 的键**必须排序后再遍历**：`maxCompressImages` 的截断是按遍历顺序生效的，
// 而 Go 的 map 迭代顺序是随机的。一旦请求里有超过 16 张图分布在同一个对象的
// 多个子键下，每次请求被压缩的子集就会不同 → 产出的 body 字节不同 →
// Responses 续传的 input 指纹每轮都变 → 续传全部失效。
// 这不是理论风险：多图会话正好踩在 16 这个边界上。
func walkImages(node any, opt model.ImageCompressSettings, st *compressStats) {
	if st.Count >= maxCompressImages {
		return
	}
	switch v := node.(type) {
	case map[string]any:
		// 先处理本层的三种「结构化图片」形状，再递归子节点。
		compressAnthropicImage(v, opt, st)
		compressGeminiImage(v, opt, st)

		keys := make([]string, 0, len(v))
		for key := range v {
			keys = append(keys, key)
		}
		sort.Strings(keys)

		for _, key := range keys {
			if st.Count >= maxCompressImages {
				return
			}
			child := v[key]
			if s, ok := child.(string); ok {
				if imageKeys[key] && isDataImageURL(s) {
					if out, ok := compressDataURL(s, opt); ok {
						v[key] = out
						st.Count++
						st.SavedBytes += int64(len(s) - len(out))
					}
				}
				continue
			}
			walkImages(child, opt, st)
		}
	case []any:
		for _, child := range v {
			if st.Count >= maxCompressImages {
				return
			}
			walkImages(child, opt, st)
		}
	}
}

// compressAnthropicImage 处理 Anthropic 的 {"type":"image","source":{…}} 形状。
//
// source.type 为 "url" 时是远程图片，不下载、不动；只处理 base64。
func compressAnthropicImage(node map[string]any, opt model.ImageCompressSettings, st *compressStats) {
	if st.Count >= maxCompressImages {
		return
	}
	src, ok := node["source"].(map[string]any)
	if !ok {
		return
	}
	if typ, _ := src["type"].(string); typ != "base64" {
		return
	}
	raw, ok := src["data"].(string)
	if !ok || raw == "" {
		return
	}
	mime, _ := src["media_type"].(string)
	if !strings.HasPrefix(mime, "image/") {
		return
	}
	out, outMime, ok := compressBase64Image(raw, mime, opt)
	if !ok {
		return
	}
	src["data"] = out
	src["media_type"] = outMime
	st.Count++
	st.SavedBytes += int64(len(raw) - len(out))
}

// compressGeminiImage 处理 Gemini 的 {"inlineData":{"mimeType":…,"data":…}} 形状。
//
// 注意 Gemini 的 data 字段**不带** data: 前缀，是裸 base64，
// 所以不能走 isDataImageURL 那条路。
func compressGeminiImage(node map[string]any, opt model.ImageCompressSettings, st *compressStats) {
	if st.Count >= maxCompressImages {
		return
	}
	for _, key := range []string{"inlineData", "inline_data"} {
		if st.Count >= maxCompressImages {
			return
		}
		inline, ok := node[key].(map[string]any)
		if !ok {
			continue
		}
		mime, _ := inline["mimeType"].(string)
		if mime == "" {
			mime, _ = inline["mime_type"].(string)
		}
		if !strings.HasPrefix(mime, "image/") {
			continue
		}
		raw, ok := inline["data"].(string)
		if !ok || raw == "" {
			continue
		}
		out, outMime, ok := compressBase64Image(raw, mime, opt)
		if !ok {
			continue
		}
		inline["data"] = out
		if _, has := inline["mimeType"]; has {
			inline["mimeType"] = outMime
		} else {
			inline["mime_type"] = outMime
		}
		st.Count++
		st.SavedBytes += int64(len(raw) - len(out))
	}
}

// isDataImageURL 判断字符串是不是 data:image/…;base64,… 形式。
func isDataImageURL(s string) bool {
	return strings.HasPrefix(s, "data:image/") && strings.Contains(s, ";base64,")
}

// compressDataURL 拆开 data URL、压缩图片、再拼回去。
func compressDataURL(s string, opt model.ImageCompressSettings) (string, bool) {
	comma := strings.IndexByte(s, ',')
	if comma < 0 {
		return s, false
	}
	header := s[len("data:"):comma] // image/png;base64
	payload := s[comma+1:]          // 裸 base64
	semi := strings.IndexByte(header, ';')
	if semi < 0 {
		return s, false
	}
	mime := header[:semi]
	out, outMime, ok := compressBase64Image(payload, mime, opt)
	if !ok {
		return s, false
	}
	return "data:" + outMime + ";base64," + out, true
}

// compressBase64Image 是压缩的核心：base64 进、base64 出。
//
// 返回的 ok 为 false 表示「保持原样」，调用方据此不做任何改动。
func compressBase64Image(b64, mime string, opt model.ImageCompressSettings) (string, string, bool) {
	if len(b64) > maxCompressBase64 {
		return b64, mime, false
	}
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		// 也接受 URL-safe 变体（少数客户端会用）。
		raw, err = base64.RawURLEncoding.DecodeString(strings.TrimRight(b64, "="))
		if err != nil {
			return b64, mime, false
		}
	}
	out, outMime, ok := compressImageBytes(raw, mime, opt)
	if !ok {
		return b64, mime, false
	}
	return base64.StdEncoding.EncodeToString(out), outMime, true
}

// compressImageBytes 把原始图片字节压成更小的字节。ok=false 表示保持原样。
func compressImageBytes(raw []byte, mime string, opt model.ImageCompressSettings) ([]byte, string, bool) {
	if len(raw) == 0 {
		return raw, mime, false
	}

	// 先只读头部拿尺寸：解压炸弹在这里就被拦下，不必真的解码。
	cfg, format, err := image.DecodeConfig(bytes.NewReader(raw))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 {
		return raw, mime, false
	}
	// 转 int64 再乘：PNG 头允许边长到 2^31-1，32 位平台上 int 相乘会溢出成负数，
	// 直接绕过下面的上限检查。
	if int64(cfg.Width)*int64(cfg.Height) > maxCompressPixels {
		return raw, mime, false
	}
	// GIF 跳过：重编码只会留下第一帧，把动图变成静图是语义损失。
	// （生产代码没有注册 gif 解码器，所以实际是靠 DecodeConfig 失败提前返回；
	// 这条分支是给「将来有人加上 _ "image/gif"」留的正确行为。）
	if format == "gif" {
		return raw, mime, false
	}

	oversize := len(raw) > opt.ThresholdBytes
	overEdge := opt.MaxEdge > 0 && maxInt(cfg.Width, cfg.Height) > opt.MaxEdge
	if !oversize && !overEdge {
		return raw, mime, false
	}

	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return raw, mime, false
	}

	if overEdge {
		img = scaleToMaxEdge(img, opt.MaxEdge)
	}

	hasAlpha := imageHasAlpha(img)
	var buf bytes.Buffer
	outMime := "image/jpeg"
	if hasAlpha {
		// 有透明通道就不能转 JPEG（透明区会变成黑块），保持 PNG。
		outMime = "image/png"
		enc := png.Encoder{CompressionLevel: png.BestCompression}
		if err := enc.Encode(&buf, img); err != nil {
			return raw, mime, false
		}
	} else {
		if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: opt.Quality}); err != nil {
			return raw, mime, false
		}
	}

	// 压完反而更大（小图、纯色图、已经是高压缩比的情况）就保留原图。
	if buf.Len() >= len(raw) {
		return raw, mime, false
	}
	return buf.Bytes(), outMime, true
}

// scaleToMaxEdge 按长边等比缩放到 maxEdge，使用 Catmull-Rom 重采样。
//
// 用 Catmull-Rom 而不是最近邻：截图里的文字在最近邻下会严重锯齿，
// 模型读小字的能力直接崩掉；Catmull-Rom 的开销对单张图可以忽略。
func scaleToMaxEdge(src image.Image, maxEdge int) image.Image {
	b := src.Bounds()
	w, h := b.Dx(), b.Dy()
	longest := maxInt(w, h)
	if longest <= maxEdge || maxEdge <= 0 {
		return src
	}
	nw := w * maxEdge / longest
	nh := h * maxEdge / longest
	if nw < 1 {
		nw = 1
	}
	if nh < 1 {
		nh = 1
	}
	dst := image.NewRGBA(image.Rect(0, 0, nw, nh))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, b, draw.Over, nil)
	return dst
}

// imageHasAlpha 判断图片是否真的带透明像素。
//
// 只看颜色模型不够，两个方向都要处理：
//   - 编码出来是 NRGBA 但 alpha 全 255 的图，转 JPEG 完全无损，没必要为了
//     「格式上可能有 alpha」白白保留大体积 PNG；
//   - 反过来，**调色板 PNG（color type 3）带 tRNS 时 alpha 藏在调色板里**，
//     只按类型白名单判断会把它当成不透明 → 转 JPEG → 透明区变黑块。
//     这类图（小图标、带透明背景的截图）很常见，所以 Paletted 必须逐像素查。
func imageHasAlpha(img image.Image) bool {
	switch img.(type) {
	case *image.NRGBA, *image.NRGBA64, *image.RGBA, *image.RGBA64,
		*image.Alpha, *image.Alpha16, *image.Paletted:
		// 需要逐像素确认。
	default:
		// YCbCr / Gray / Gray16 / CMYK 等模型本身没有 alpha 通道。
		return false
	}
	b := img.Bounds()
	// 大图上逐像素扫一遍是 O(n)，但只在真的要压缩时才走这里，且上限已由
	// maxCompressPixels 兜住（50MP 最坏约 200ms，可接受）。
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if _, _, _, a := img.At(x, y).RGBA(); a != 0xffff {
				return true
			}
		}
	}
	return false
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
