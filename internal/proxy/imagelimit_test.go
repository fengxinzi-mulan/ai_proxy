package proxy

// 单请求图片数量上限的裁剪用例。
//
// 这一组测试盯三条硬约束：超限才动、保留最新、输出确定（续传指纹不能抖）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// chatImageBody 造一个 chat 请求体：n 张图平均分给若干条 user 消息。
//
// 用极小的 data URL 占位就行 —— 裁剪只看形状与张数，不解码图片。
func chatImageBody(n int) map[string]any {
	const url = "data:image/png;base64,iVBORw0KGgo="
	msgs := []any{
		map[string]any{"role": "system", "content": "系统提示"},
	}
	for i := 0; i < n; i++ {
		msgs = append(msgs, map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": fmt.Sprintf("第 %d 张", i)},
				map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}},
			},
		})
	}
	return map[string]any{"model": "m", "messages": msgs}
}

// countImageParts 数一遍 body 里还剩几个图片 part，顺便校验占位块的数量。
func countImageParts(t *testing.T, body map[string]any) (images, placeholders int) {
	t.Helper()
	var walk func(any)
	walk = func(node any) {
		switch v := node.(type) {
		case map[string]any:
			if typ, _ := v["type"].(string); typ == "image_url" || typ == "input_image" {
				images++
				return
			}
			if typ, _ := v["type"].(string); typ == "image" {
				images++
				return
			}
			if _, ok := v["inlineData"]; ok {
				images++
				return
			}
			if txt, ok := v["text"].(string); ok && strings.Contains(txt, "omitted") {
				placeholders++
			}
			for _, child := range v {
				walk(child)
			}
		case []any:
			for _, child := range v {
				walk(child)
			}
		}
	}
	walk(body)
	return images, placeholders
}

func TestTrimExcessImagesKeepsNewestAndCountsDropped(t *testing.T) {
	body := chatImageBody(25)
	if images, _ := countImageParts(t, body); images != 25 {
		t.Fatalf("构造的 body 应有 25 张图，实际 %d", images)
	}

	dropped := trimExcessImages(body, 20, 0)
	if dropped != 5 {
		t.Fatalf("应裁掉 5 张，实际 %d", dropped)
	}
	images, placeholders := countImageParts(t, body)
	if images != 20 {
		t.Errorf("裁剪后应剩 20 张图，实际 %d", images)
	}
	if placeholders != 5 {
		t.Errorf("应留 5 个占位块，实际 %d", placeholders)
	}

	// 「保留最新」：前 5 条 user 消息里的图被换成文本，后 20 条还在。
	msgs := body["messages"].([]any)
	for i := 1; i <= 5; i++ {
		msg := msgs[i].(map[string]any)
		parts := msg["content"].([]any)
		hasImage := false
		for _, raw := range parts {
			if _, ok := raw.(map[string]any)["image_url"]; ok {
				hasImage = true
			}
		}
		if hasImage {
			t.Errorf("第 %d 条消息里的图应已被换掉", i)
		}
	}
	last := msgs[len(msgs)-1].(map[string]any)
	if _, ok := last["content"].([]any)[1].(map[string]any)["image_url"]; !ok {
		t.Error("最新那张图必须保留")
	}
}

func TestTrimExcessImagesWithinLimitIsNoop(t *testing.T) {
	for _, n := range []int{1, 19, 20} {
		body := chatImageBody(n)
		if dropped := trimExcessImages(body, 20, 0); dropped != 0 {
			t.Errorf("%d 张（未超限）不该裁剪，实际裁了 %d 张", n, dropped)
		}
		if images, _ := countImageParts(t, body); images != n {
			t.Errorf("%d 张时图片数不该变，实际 %d", n, images)
		}
	}
}

// 输出确定是硬要求：同一输入多次裁剪必须逐字节相同，否则续传指纹每轮都变。
//
// 图必须摊在**多个 map 子键**下（每个子键一个数组），这样 map 迭代顺序的随机性
// 才会影响收集顺序 —— 如果只在单个数组里，数组本来就保序，测不出问题。
func TestTrimExcessImagesIsDeterministic(t *testing.T) {
	imgPart := func() map[string]any {
		return map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo="},
		}
	}
	// 6 个顶层键 × 各 5 张图 = 30 张，上限 20 要裁 10 张。
	// 「哪 10 张先被丢」由 map 键的遍历顺序决定，所以这一条同时验证排序生效。
	build := func() map[string]any {
		body := map[string]any{}
		for i := 0; i < 6; i++ {
			parts := make([]any, 0, 5)
			for j := 0; j < 5; j++ {
				parts = append(parts, imgPart())
			}
			body[fmt.Sprintf("bucket_%02d", i)] = parts
		}
		return body
	}

	var first []byte
	for i := 0; i < 8; i++ {
		body := build()
		if n := trimExcessImages(body, 20, 0); n != 10 {
			t.Fatalf("第 %d 次：应裁 10 张，实际 %d", i, n)
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
			t.Fatal("多次裁减产出的 body 不一致 —— map 迭代顺序破坏了确定性，会打断续传")
		}
	}
}

func TestTrimExcessImagesUnlimitedWhenMaxIsZero(t *testing.T) {
	body := chatImageBody(30)
	if dropped := trimExcessImages(body, 0, 0); dropped != 0 {
		t.Errorf("上限为 0 表示不限制，实际裁了 %d 张", dropped)
	}
	if images, _ := countImageParts(t, body); images != 30 {
		t.Errorf("不限制时图片数不该变，实际 %d", images)
	}
}

// Responses 的 input 文本块类型是 input_text，chat 是 text —— 猜错会被严格上游判非法。
func TestTrimExcessImagesUsesInputTextForResponses(t *testing.T) {
	items := []any{
		map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64,iVBORw0KGgo="},
			},
		},
	}
	for i := 0; i < 4; i++ {
		items = append(items, map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64,iVBORw0KGgo="},
			},
		})
	}
	body := map[string]any{"model": "m", "input": items}

	if n := trimExcessImages(body, 3, 0); n != 2 {
		t.Fatalf("应裁 2 张，实际 %d", n)
	}
	first := body["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if typ, _ := first["type"].(string); typ != "input_text" {
		t.Errorf("Responses 的占位块类型应为 input_text，实际 %q", typ)
	}
	if txt, _ := first["text"].(string); !strings.Contains(txt, "omitted") {
		t.Errorf("占位文本未写入，实际 %v", first)
	}
}

func TestTrimExcessImagesAnthropic(t *testing.T) {
	items := make([]any, 0, 25)
	for i := 0; i < 25; i++ {
		items = append(items, map[string]any{
			"type": "image",
			"source": map[string]any{
				"type": "base64", "media_type": "image/png", "data": "iVBORw0KGgo=",
			},
		})
	}
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": items},
	}}

	if n := trimExcessImages(body, 20, 0); n != 5 {
		t.Fatalf("应裁 5 张，实际 %d", n)
	}
	kept := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(kept) != 25 {
		t.Errorf("替换必须就地、不改数组长度，实际 %d", len(kept))
	}
	if typ, _ := kept[0].(map[string]any)["type"].(string); typ != "text" {
		t.Errorf("Anthropic 占位块类型应为 text，实际 %q", typ)
	}
}

func TestTrimExcessImagesGemini(t *testing.T) {
	parts := make([]any, 0, 25)
	for i := 0; i < 25; i++ {
		parts = append(parts, map[string]any{
			"inlineData": map[string]any{"mimeType": "image/png", "data": "iVBORw0KGgo="},
		})
	}
	body := map[string]any{"contents": []any{
		map[string]any{"role": "user", "parts": parts},
	}}

	if n := trimExcessImages(body, 20, 0); n != 5 {
		t.Fatalf("应裁 5 张，实际 %d", n)
	}
	got := body["contents"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if _, has := got["inlineData"]; has {
		t.Error("被裁掉的落点不该还留着 inlineData")
	}
	if txt, _ := got["text"].(string); !strings.Contains(txt, "omitted") {
		t.Errorf("Gemini 占位块应写成 text，实际 %v", got)
	}
}

// 远程 URL 图与内联图一起计数：上游数的是张数，不看来源。
func TestTrimExcessImagesCountsRemoteURLImages(t *testing.T) {
	const local = "data:image/png;base64,iVBORw0KGgo="
	parts := make([]any, 0, 22)
	for i := 0; i < 22; i++ {
		url := local
		if i%2 == 0 {
			url = fmt.Sprintf("https://example.com/%d.png", i)
		}
		parts = append(parts, map[string]any{
			"type":      "image_url",
			"image_url": map[string]any{"url": url},
		})
	}
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": parts},
	}}

	if n := trimExcessImages(body, 20, 0); n != 2 {
		t.Fatalf("远程图必须计入张数，应裁 2 张，实际 %d", n)
	}
}

// 简写形态（不给 type，直接挂 image_url）也要认。
func TestTrimExcessImagesRecognizesShorthand(t *testing.T) {
	parts := make([]any, 0, 22)
	for i := 0; i < 22; i++ {
		parts = append(parts, map[string]any{
			"image_url": "data:image/png;base64,iVBORw0KGgo=",
		})
	}
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": parts},
	}}

	if n := trimExcessImages(body, 20, 0); n != 2 {
		t.Fatalf("简写形态应被认作图片，实际裁了 %d 张", n)
	}
}

// 同一个 part 不能被数两次（否则「保留 20 张」实际只留 10 张）。
func TestTrimExcessImagesCountsEachPartOnce(t *testing.T) {
	// 一个 part 同时带 type 与 image_url：认得 type 之后就不能再递归进去。
	part := map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo="},
		"extra":     map[string]any{"image_url": "data:image/png;base64,iVBORw0KGgo="},
	}
	parts := make([]any, 0, 21)
	for i := 0; i < 21; i++ {
		dup := map[string]any{}
		for k, v := range part {
			dup[k] = v
		}
		parts = append(parts, dup)
	}
	body := map[string]any{"messages": []any{
		map[string]any{"role": "user", "content": parts},
	}}

	if n := trimExcessImages(body, 20, 0); n != 1 {
		t.Fatalf("21 个 part 应只裁 1 张，实际 %d（说明有 part 被数了两次）", n)
	}
}

// Gemini 的 fileData 里可能是 PDF 或音视频：那种不算「图片张数」，不能拿它凑数。
func TestTrimExcessImagesIgnoresNonImageFileData(t *testing.T) {
	parts := make([]any, 0, 25)
	for i := 0; i < 25; i++ {
		parts = append(parts, map[string]any{
			"inlineData": map[string]any{"mimeType": "image/png", "data": "iVBORw0KGgo="},
		})
	}
	// 再塞 3 个 PDF —— 它们不该被算进图片张数。
	for i := 0; i < 3; i++ {
		parts = append(parts, map[string]any{
			"fileData": map[string]any{"mimeType": "application/pdf", "fileUri": "gs://b/x.pdf"},
		})
	}
	body := map[string]any{"contents": []any{
		map[string]any{"role": "user", "parts": parts},
	}}

	if n := trimExcessImages(body, 20, 0); n != 5 {
		t.Fatalf("PDF 不该计入图片张数，应只裁 5 张，实际 %d", n)
	}
	// PDF 那几项必须原样保留。
	got := body["contents"].([]any)[0].(map[string]any)["parts"].([]any)
	for i := 25; i < 28; i++ {
		if _, ok := got[i].(map[string]any)["fileData"]; !ok {
			t.Errorf("第 %d 项是 PDF，不该被动过", i)
		}
	}
}

// Responses 的 input[] 顶层只能放「一条消息」；把图片项替换成裸的
// {"type":"input_text"} 会被判成非法 item type，必须写成 message。
func TestTrimExcessImagesTopLevelInputBecomesMessage(t *testing.T) {
	items := make([]any, 0, 23)
	for i := 0; i < 23; i++ {
		items = append(items, map[string]any{
			"type":      "input_image",
			"image_url": "data:image/png;base64,iVBORw0KGgo=",
		})
	}
	body := map[string]any{"model": "m", "input": items}

	if n := trimExcessImages(body, 20, 0); n != 3 {
		t.Fatalf("应裁 3 张，实际 %d", n)
	}
	first := body["input"].([]any)[0].(map[string]any)
	if typ, _ := first["type"].(string); typ != "message" {
		t.Fatalf("顶层 input 项应变成 message，实际 %q: %v", typ, first)
	}
	if role, _ := first["role"].(string); role != "user" {
		t.Errorf("message 的角色应为 user，实际 %q", role)
	}
	content, ok := first["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("message 应带一个 content 块，实际 %v", first["content"])
	}
	inner := content[0].(map[string]any)
	if typ, _ := inner["type"].(string); typ != "input_text" {
		t.Errorf("content 块类型应为 input_text，实际 %q", typ)
	}
}

// message 里的图片块（不是顶层 input 项）仍然用 input_text，不要误写成 message。
func TestTrimExcessImagesInputContentStaysInputText(t *testing.T) {
	items := make([]any, 0, 4)
	for i := 0; i < 4; i++ {
		items = append(items, map[string]any{
			"type": "message",
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_image", "image_url": "data:image/png;base64,iVBORw0KGgo="},
			},
		})
	}
	body := map[string]any{"model": "m", "input": items}

	if n := trimExcessImages(body, 3, 0); n != 1 {
		t.Fatalf("应裁 1 张，实际 %d", n)
	}
	inner := body["input"].([]any)[0].(map[string]any)["content"].([]any)[0].(map[string]any)
	if typ, _ := inner["type"].(string); typ != "input_text" {
		t.Errorf("message 内的块应是 input_text，实际 %q", typ)
	}
	if _, has := inner["role"]; has {
		t.Error("message 内的块不该长成 message 的形状")
	}
}

// 函数工具的参数名叫 image_url 很常见（图片抓取/搜索类工具）。那是 schema 里的
// 属性定义，不是图片 part —— 一旦把它当图片，替换会清空整个 properties，
// 工具定义直接消失，而 required 还引用着已经不存在的属性。
func TestTrimExcessImagesIgnoresToolSchema(t *testing.T) {
	imageURLPart := map[string]any{
		"type":      "image_url",
		"image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo="},
	}
	tool := map[string]any{
		"type": "function",
		"function": map[string]any{
			"name": "fetch_image",
			"parameters": map[string]any{
				"type": "object",
				"properties": map[string]any{
					// 这个 properties 里恰好有个叫 image_url 的参数。
					"image_url": map[string]any{"type": "string"},
				},
				"required": []any{"image_url"},
			},
		},
	}

	// 场景一：真图正好等于上限，工具不该让计数超限、更不该被改动。
	parts := make([]any, 0, 20)
	for i := 0; i < 20; i++ {
		parts = append(parts, imageURLPart)
	}
	body := map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": parts}},
		"tools":    []any{tool, tool},
	}
	if n := trimExcessImages(body, 20, 0); n != 0 {
		t.Errorf("工具 schema 不该被算作图片，真图 20 张时不该裁剪，实际裁了 %d 张", n)
	}

	// 场景二：确实超限时，裁的必须是真图，工具原样保留。
	body["messages"].([]any)[0].(map[string]any)["content"] = append(parts, imageURLPart)
	if n := trimExcessImages(body, 20, 0); n != 1 {
		t.Errorf("应只裁 1 张真图，实际 %d", n)
	}
	tools := body["tools"].([]any)
	for i, raw := range tools {
		fn := raw.(map[string]any)["function"].(map[string]any)
		props, ok := fn["parameters"].(map[string]any)["properties"].(map[string]any)
		if !ok || props["image_url"] == nil {
			t.Fatalf("第 %d 个工具的参数定义被裁坏了: %v", i, fn["parameters"])
		}
	}
}

// Gemini 的 inlineData 与 fileData 一样可能装 PDF / 音视频，不能计入图片张数。
func TestTrimExcessImagesIgnoresNonImageInlineData(t *testing.T) {
	parts := make([]any, 0, 6)
	for i := 0; i < 2; i++ {
		parts = append(parts, map[string]any{
			"inlineData": map[string]any{"mimeType": "image/png", "data": "iVBORw0KGgo="},
		})
	}
	parts = append(parts, map[string]any{
		"inlineData": map[string]any{"mimeType": "application/pdf", "data": "JVBERi0="},
	})
	body := map[string]any{"contents": []any{
		map[string]any{"role": "user", "parts": parts},
	}}

	// 2 张真图正好等于上限：内联 PDF 不该让计数变成 3 而裁掉一张真图。
	if n := trimExcessImages(body, 2, 0); n != 0 {
		t.Errorf("内联 PDF 不该计入图片张数，实际裁了 %d 张（真图会被误裁）", n)
	}
}

// 跨容器的顺序也要固定：图分布在多个顶层数组下时，裁哪几张必须每轮一致。
func TestTrimExcessImagesDeterministicAcrossContainers(t *testing.T) {
	build := func() map[string]any {
		zz := make([]any, 0, 10)
		for i := 0; i < 10; i++ {
			zz = append(zz, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo="},
			})
		}
		aa := make([]any, 0, 10)
		for i := 0; i < 10; i++ {
			aa = append(aa, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo="},
			})
		}
		// 键名故意让字典序（aa < zz）与直觉顺序不同，确认按 key 排序而不是按加入顺序。
		return map[string]any{"zz_bucket": zz, "aa_bucket": aa}
	}

	var first []byte
	for i := 0; i < 8; i++ {
		body := build()
		if n := trimExcessImages(body, 15, 0); n != 5 {
			t.Fatalf("第 %d 次：应裁 5 张，实际 %d", i, n)
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
			t.Fatal("跨容器场景下裁减结果不一致")
		}
	}
	// aa_bucket 排在前，所以被裁的 5 张应该全部落在 aa_bucket 里。
	body := build()
	trimExcessImages(body, 15, 0)
	if dropped := countOmitted(body["aa_bucket"].([]any)); dropped != 5 {
		t.Errorf("按 key 排序时 aa_bucket 应先被裁，实际裁了 %d 张", dropped)
	}
	if dropped := countOmitted(body["zz_bucket"].([]any)); dropped != 0 {
		t.Errorf("zz_bucket 排在后面，不该被裁，实际 %d 张", dropped)
	}
}

// countOmitted 数一个 parts 数组里有多少个占位块。
func countOmitted(parts []any) int {
	n := 0
	for _, raw := range parts {
		if txt, _ := raw.(map[string]any)["text"].(string); strings.Contains(txt, "omitted") {
			n++
		}
	}
	return n
}

// 部分网关把内联图直接挂在 {"image": …} 上。这种形状压缩侧会处理，
// 数量上限这条路径也必须认，否则「压缩生效、上限不触发」，会话照样被 400。
func TestTrimExcessImagesRecognizesImageKey(t *testing.T) {
	cases := []struct {
		name string
		val  any
		want int
	}{
		{"data URL 字符串", "data:image/png;base64,iVBORw0KGgo=", 1},
		{"远程 URL 字符串", "https://example.com/a.png", 1},
		{"url 对象", map[string]any{"url": "https://example.com/a.png"}, 1},
		// 恰好叫 image 的普通字段不能被当成图片。
		{"数字", float64(1), 0},
		{"任意对象", map[string]any{"width": float64(10)}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parts := make([]any, 0, 22)
			for i := 0; i < 20; i++ {
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": "data:image/png;base64,iVBORw0KGgo=",
				})
			}
			// 再塞两个待测值：若被认作图片，总数变 22、超限 2 张。
			parts = append(parts,
				map[string]any{"image": tc.val},
				map[string]any{"image": tc.val},
			)
			body := map[string]any{"messages": []any{
				map[string]any{"role": "user", "content": parts},
			}}
			if n := trimExcessImages(body, 20, 0); n != tc.want*2 {
				t.Errorf("%s：应裁 %d 张，实际 %d", tc.name, tc.want*2, n)
			}
		})
	}
}

// 分批替换的核心目的：图片数在 [max+1, max+batch] 这段窗口里增长时，
// 代理产出的 body 必须**逐字节一致**（除了尾部新增的图），否则前缀缓存每轮
// 都要从「第一张保留下来的图」那里断开、把后面十几张图全价重算。
func TestTrimExcessImagesBatchKeepsPrefixStableAcrossGrowth(t *testing.T) {
	const (
		maxImages = 20
		batch     = 5
	)

	// 图的张数 → 应丢弃的张数。窗口 [21,25] 内恒为 5，是缓存友好的关键。
	wantDropped := map[int]int{
		20: 0, 21: 5, 22: 5, 23: 5, 24: 5, 25: 5,
		26: 10, 27: 10, 30: 10,
		31: 15,
	}
	for n, want := range wantDropped {
		parts := make([]any, 0, n)
		for i := 0; i < n; i++ {
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo="},
			})
		}
		body := map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": parts},
		}}
		if got := trimExcessImages(body, maxImages, batch); got != want {
			t.Errorf("%d 张图（上限 %d、批 %d）应丢 %d 张，实际丢 %d 张",
				n, maxImages, batch, want, got)
		}
	}

	// 关键断言：同一批窗口内，**保留下来的图逐位置完全一致**。
	//
	// 这才是上游缓存能命中的条件。三种写出方式都验证过有区分力：
	//
	//   - 只比「前 drop 个占位块」：没区分力 —— drop 恒为 5 时前 5 个永远就是
	//     那 5 段同样的文本，丢错方向也照样通过；
	//   - 比整份 JSON 前缀：会因为数组末尾的 `]}]}` 必然不同而误报（实测 21 与
	//     22 张的公共字节前缀到 1682 / 1700 就断了，断点恰好是旧数组的收尾括号，
	//     内容其实一致）—— 上游按 token 计缓存，不按字节边界；
	//   - 比保留区逐位置（本条）：既不受收尾括号干扰，又能抓住位置漂移。
	//
	// 每张图的 URL 必须各不相同，否则位置漂移在字节上根本看不出来。
	makeParts := func(n int) []any {
		parts := make([]any, 0, n)
		for i := 0; i < n; i++ {
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": fmt.Sprintf("data:image/png;base64,IMG%03d=", i)},
			})
		}
		return parts
	}
	// keptImages 返回裁剪后仍保留的图片 URL 序列。
	keptImages := func(n int) []string {
		body := map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": makeParts(n)},
		}}
		trimExcessImages(body, maxImages, batch)
		content := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
		var out []string
		for _, raw := range content {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if iu, ok := part["image_url"].(map[string]any); ok {
				if u, ok := iu["url"].(string); ok {
					out = append(out, u)
				}
			}
		}
		return out
	}

	base := keptImages(21)
	if len(base) != 16 {
		t.Fatalf("21 张、上限 20、批 5 时应保留 16 张，实际 %d：%v", len(base), base)
	}
	// 方向断言：保留的必须是**最新**的那批（尾部），不是最老的。
	// 少了这一条，「丢掉最新的、保留最老的」这种方向反转在窗口内同样稳定，
	// 逐位置比较会照样通过 —— 变异测试确认过这个盲区。
	if want := "data:image/png;base64,IMG005="; base[0] != want {
		t.Errorf("21 张时保留下来的第一张应是第 6 张（IMG005），实际 %q", base[0])
	}
	if want := "data:image/png;base64,IMG020="; base[len(base)-1] != want {
		t.Errorf("21 张时保留下来的最后一张应是最新那张（IMG020），实际 %q", base[len(base)-1])
	}

	for _, n := range []int{22, 23, 24, 25} {
		got := keptImages(n)
		// 长的必须包含短的全部（短的是长的前缀序列），且逐位置一致。
		if len(got) < len(base) {
			t.Errorf("%d 张时保留了 %d 张，比 21 张的 %d 张还少 —— 丢弃边界不该往回退",
				n, len(got), len(base))
			continue
		}
		for i := range base {
			if got[i] != base[i] {
				t.Fatalf("%d 张时第 %d 张保留图变成了 %q（21 张时是 %q）—— "+
					"保留区起点漂移会让前缀缓存整段失效", n, i, got[i], base[i])
			}
		}
		// 每一轮保留下来的最后一张都必须是最新那张 —— 方向不能再错。
		if want := fmt.Sprintf("data:image/png;base64,IMG%03d=", n-1); got[len(got)-1] != want {
			t.Errorf("%d 张时保留的最后一张应是本轮最新图，实际 %q", n, got[len(got)-1])
		}
	}

	// 跨批那一轮（26 张）边界前移、保留区起点改变 ——
	// 这正是「每长满一批才失效一次」的那一次，属预期行为。
	if got := keptImages(26); len(got) > 0 && got[0] == base[0] {
		t.Error("26 张跨批之后保留区起点应当改变（否则说明边界没有前移）")
	}
}

// 逐张（batch=0/1）与分批的丢弃数量差异，是这次改动的取舍本身，明确记录下来。
func TestQuantizeDrop(t *testing.T) {
	cases := []struct {
		excess int
		batch  int
		want   int
	}{
		{0, 5, 0},
		{1, 5, 5},
		{4, 5, 5},
		{5, 5, 5},
		{6, 5, 10},
		{1, 1, 1},
		{1, 0, 1},
		{8, 3, 9},
		{1, 1000, 1000},
		{-3, 5, 0},
	}
	for _, tc := range cases {
		if got := quantizeDrop(tc.excess, tc.batch); got != tc.want {
			t.Errorf("quantizeDrop(%d, %d) = %d，期望 %d", tc.excess, tc.batch, got, tc.want)
		}
	}
}

// 批大小大于上限时不能越界、不能把图全丢光。
//
// 这是用户手填能凑出来的组合（「上限 20 / 批 500」），曾经会 panic：
// quantizeDrop 算出的丢弃数超过实际图片数，replaceWithText(slots[i]) 越界。
// 越界是进程内 panic，连接被直接掐断，客户端看到的是 EOF 而不是可解释的 400。
func TestTrimExcessImagesBatchLargerThanMaxDoesNotPanic(t *testing.T) {
	cases := []struct{ max, batch, images int }{
		{20, 21, 21},
		{20, 50, 21},
		{20, 500, 21},
		{3, 5, 4},
		{1, 1000, 2},
		{1, 1000, 50},
		{20, 1000, 200},
	}
	for _, tc := range cases {
		parts := make([]any, 0, tc.images)
		for i := 0; i < tc.images; i++ {
			parts = append(parts, map[string]any{
				"type":      "image_url",
				"image_url": map[string]any{"url": "data:image/png;base64,iVBORw0KGgo="},
			})
		}
		body := map[string]any{"messages": []any{
			map[string]any{"role": "user", "content": parts},
		}}

		// 不 recover —— 越界会直接把测试打挂，这正是我们要的失败方式。
		dropped := trimExcessImages(body, tc.max, tc.batch)

		left := tc.images - dropped
		if left < 1 {
			t.Errorf("上限 %d、批 %d、图 %d 张：丢光后只剩 %d 张 —— 至少要保留最新那一张",
				tc.max, tc.batch, tc.images, left)
		}
		if dropped < 0 || dropped > tc.images {
			t.Errorf("上限 %d、批 %d、图 %d 张：丢弃数 %d 不合法",
				tc.max, tc.batch, tc.images, dropped)
		}
		// 留下来的图必须真的是「最后 left 张」，不能丢错方向。
		content := body["messages"].([]any)[0].(map[string]any)["content"].([]any)
		for i := 0; i < left; i++ {
			idx := len(content) - 1 - i
			if _, ok := content[idx].(map[string]any)["image_url"]; !ok {
				t.Fatalf("上限 %d、批 %d、图 %d 张：第 %d 个（倒数第 %d）本该是保留的图，却不是",
					tc.max, tc.batch, tc.images, idx, i+1)
				break
			}
		}
	}
}

func TestLooksLikeItHasImages(t *testing.T) {
	cases := []struct {
		name string
		body string
		want bool
	}{
		{"chat 图片 part", `{"messages":[{"content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}]}`, true},
		{"responses 图片 part", `{"input":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}`, true},
		{"简写", `{"messages":[{"content":[{"image_url":"https://x/a.png"}]}]}`, true},
		{"Anthropic", `{"messages":[{"content":[{"type":"image","source":{"type":"base64"}}]}]}`, true},
		{"Gemini", `{"contents":[{"parts":[{"inlineData":{"data":"AA=="}}]}]}`, true},
		{"纯文本", `{"messages":[{"role":"user","content":"把图片处理一下"}]}`, false},
		{"工具 schema 里的 image 字样", `{"tools":[{"name":"image_search","description":"搜索图片"}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := looksLikeItHasImages([]byte(tc.body)); got != tc.want {
				t.Errorf("looksLikeItHasImages(%s) = %v，期望 %v", tc.body, got, tc.want)
			}
		})
	}
}
