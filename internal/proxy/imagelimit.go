package proxy

// 请求体图片数量上限的兜底裁剪。
//
// 为什么需要它：上游按「整个请求里的图片总数」硬性拒绝，Mistral 超过就整条 400：
//
//	{"message":"Total number of images exceeds the maximum allowed of 20."}
//
// 而客户端（DSH、Codex）每一轮都把历史消息连同其中的图片原样回放。图片数量随
// 会话自然增长，完全不受代理控制 —— 攒够上限之后这个会话就再也发不出去，
// 客户端侧也没有任何开关能只回放文本。唯一能做的兜底就是这里：
// 超出上限时把**更早**的图片替换成一行占位文本，让会话继续可用。
//
// 四条硬约束（与 imagecompress.go 同一套纪律）：
//  1. **确定性**。占位文本只嵌「上限值」这一个常量，绝不嵌「本次丢了几张」这类
//     随请求变化的内容；遍历顺序也固定（数组保序、map 键排序）。一旦输出抖动，
//     Responses 续传的 input 指纹每轮都会变，续传全部失效。
//  2. **超限才动**。总数不超过上限时一个字节都不改，调用方据此不做任何标记。
//  3. **留占位、不删项**。就地替换成文本块，parent 数组的长度与顺序都不变 ——
//     删项会让后续所有图片的索引前移，前缀指纹跟着全变。
//  4. **失败即放弃**。认不出的形状不猜、不计、不动，宁可少裁一张。
//
// 与 Responses 续传的交互（不是缺陷，但要知道）：裁剪一旦开始生效，「被替换的
// 位置集合」会随会话增长而扩大 —— 本轮丢第 1 张，下一轮丢第 1、2 张。那些位置
// 在前缀里逐轮变化，续传的 prefix 匹配会持续落空，退化为全量。占位文本本身是
// 稳定的（只嵌上限常量），所以不会出现「同一轮内指纹抖动」那种更糟的情况。
// 实际影响很小：Mistral 预设本来就把续传关掉了（它的 Responses API 无状态）。

import (
	"bytes"
	"fmt"
	"sort"
	"strings"
)

// imageTrimPlaceholder 是图片被换掉之后留在原地的文本。
//
// 只嵌上限值：上限不变则这条文本每轮逐字节相同（见文件头的约束 1）。
// 内容要让模型知道自己漏了东西 —— 完全静默地少一张图，模型会理直气壮地
// 答「你没给过这张图」，反而更难排查。
const imageTrimPlaceholder = "[an earlier image was omitted: this request may carry at most %d images]"

// imageCountMarkers 是「请求体里可能有图片」的字节预筛。
//
// 命中任一条才值得把整个 body 解析成 JSON。标记要足够具体：裸的 "image"
// 在代码片段、工具描述里出现频率很高，会让大量纯文本请求白白走一遍解析。
// 远程 URL 图（"image_url":"https://…"）同样命中 —— 上游数的是张数，
// 不区分内联还是远程，所以这里不能只认 data:image。
//
// 与 imagecompress.go 的 imageKeys 保持同一口径：那边认的落点这里都要能预筛到，
// 否则「压缩会处理、数量上限却不触发」，该救的会话照样被上游 400。
var imageCountMarkers = []string{
	`"image_url"`,
	`"input_image"`,
	`"inlineData"`,
	`"inline_data"`,
	`"fileData"`,
	`"file_data"`,
	`"type":"image"`,
	`"type": "image"`,
	`"image_data"`,
	`"b64_json"`,
	// 部分网关直接用 {"image":"data:image/…"} 或 {"image":{"url":…}}。
	// 这个标记宽一点，误命中只是多解析一次 body。
	`"image":`,
	`"image" :`,
}

// looksLikeItHasImages 是廉价的字节预筛：没命中就不必解析 body。
func looksLikeItHasImages(body []byte) bool {
	for _, marker := range imageCountMarkers {
		if bytes.Contains(body, []byte(marker)) {
			return true
		}
	}
	return false
}

// imageSlotKind 是图片落点的形状，决定替换成什么样的文本块。
type imageSlotKind int

const (
	// slotChatImage 是 OpenAI Chat / Responses 的图片 part：
	// {"type":"image_url","image_url":{...}} 以及简写 {"image_url":"data:…"}。
	slotChatImage imageSlotKind = iota
	// slotAnthropicImage 是 {"type":"image","source":{"type":"base64"/"url",…}}。
	slotAnthropicImage
	// slotGeminiImage 是 {"inlineData":{…}} / {"fileData":{…}}。
	slotGeminiImage
)

// imageSlot 是一个可以就地替换的图片落点。
//
// 持 map 引用而不是「父容器 + 下标」：map 是引用类型，就地改它的键不需要
// 回写父节点，父数组里的位置天然不变。
type imageSlot struct {
	node map[string]any
	kind imageSlotKind
	// topLevelInput 表示这个落点直接挂在 Responses 的 input[] 上（而不是
	// 某条 message 的 content[] 里）。两种位置的「文本块」写法不一样：
	// content 里是 {"type":"input_text",…}，而顶层 item 必须写成一条 message。
	topLevelInput bool
}

// trimExcessImages 把超出 maxImages 的**更早**图片替换成占位文本，返回被替换的张数。
//
// 顺序：先按确定顺序收集全部图片落点（数组保序、map 键排序），再丢弃前若干个。
// 多轮会话里历史图必然排在当前轮之前，所以「保留最后 N 个」等价于「丢最老的那几张」。
//
// **丢弃数量是量化过的**（见 quantizeDrop）：逐张推进会让丢弃边界每轮前移一格，
// 「第一张保留下来的图」的位置跟着变，前缀缓存从这里断开、该轮后面十几张图全价
// 重算。按 batch 量化后，图片数在 [max+1, max+batch] 这段窗口里丢的是同一批、
// 位置也相同 —— **保留区起点不动，所以共同前缀逐字节一致**，只有尾部在增长。
//
// maxImages <= 0 表示不限制，直接返回 0（调用方本就不该调它）。
func trimExcessImages(obj map[string]any, maxImages, batch int) int {
	if obj == nil || maxImages <= 0 {
		return 0
	}
	// 批大小不能超过上限，否则 quantizeDrop 算出的丢弃数会超过实际图片数
	// （上限 20、批 500、图 21 张 → 丢弃 500 张 → 越界 panic）。
	// 用户手填就能凑出这种组合，必须在这里夹死。
	if batch > maxImages {
		batch = maxImages
	}
	// Responses 的输入项文本类型是 input_text，chat 是 text。
	// 猜错会被严格上游判成非法 item，所以在进入遍历前按 body 形状定一次。
	textType := "text"
	if isResponsesShaped(obj) {
		textType = "input_text"
	}

	var slots []imageSlot
	// 只有**请求体顶层**的 input[] 才算「Responses 的顶层输入项」。
	// 不能靠「键名叫 input」在任意深度判断：工具 schema 里也有 input，
	// 那里面的东西被当成顶层项处理就会写出错形状。
	if input, ok := obj["input"].([]any); ok && isResponsesShaped(obj) {
		for _, item := range input {
			collectImageSlots(item, &slots, true, true)
		}
		for _, key := range sortedKeys(obj) {
			if key == "input" || skipTrimField(key) {
				continue
			}
			collectImageSlots(obj[key], &slots, false, false)
		}
	} else {
		// 顶层直接按字段遍历：tools / response_format 这些不是对话内容，
		// 里面的图片是工具定义或响应格式，替换掉会弄坏请求本身。
		for _, key := range sortedKeys(obj) {
			if skipTrimField(key) {
				continue
			}
			collectImageSlots(obj[key], &slots, false, false)
		}
	}
	if len(slots) <= maxImages {
		return 0
	}

	drop := quantizeDrop(len(slots)-maxImages, batch)
	// 兜底夹一次：上面已经把 batch 夹到 <= maxImages，理论上 drop 恒 < len(slots)，
	// 但「绝不越界」值得在循环边界再明确一次 —— 越界是 panic，会直接掐断连接，
	// 比少裁几张图糟糕得多。
	//
	// 上限取 len(slots)-1 而不是 len(slots)：至少要留下最新那一张。全丢光会让
	// 模型完全看不到图，「保留最新 N 张」这个契约就破了。
	if drop > len(slots)-1 {
		drop = len(slots) - 1
	}
	placeholder := fmt.Sprintf(imageTrimPlaceholder, maxImages)
	for i := 0; i < drop; i++ {
		replaceWithText(slots[i], placeholder, textType)
	}
	return drop
}

// quantizeDrop 把「按张算出的超出量」向上取整到 batch 的整数倍。
//
// 这一步是缓存友好的关键。举例（上限 20、batch 5）：
//
//	图数 21 → 超出 1 → 丢弃 5   → 保留 16 张
//	图数 22 → 超出 2 → 丢弃 5   → 保留 17 张
//	图数 25 → 超出 5 → 丢弃 5   → 保留 20 张
//	图数 26 → 超出 6 → 丢弃 10  → 保留 16 张
//
// 图数 21 到 25 这一段里丢弃数量恒为 5、被替换的位置也完全相同，所以代理产出的
// body 与上一轮逐字节一致（除了尾部新增），前缀缓存全部命中。只有每长满 batch 张、
// 边界前移的那一轮才失效一次 —— 失效频率从「每张一次」降到「每 batch 张一次」。
//
// 代价是可能多丢几张本来还能留的图（最多 batch-1 张）。这是刻意换的：
// 那几张图的 token 成本，远小于每轮把保留区全部重算一遍。
//
// batch <= 1 时退化成逐张（不额外丢弃），保持「最小语义损失」的默认行为。
func quantizeDrop(excess, batch int) int {
	if excess <= 0 {
		return 0
	}
	if batch <= 1 {
		return excess
	}
	return ((excess + batch - 1) / batch) * batch
}

// sortedKeys 返回排好序的键名 —— map 迭代顺序是随机的，遍历顺序必须自己定死。
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// collectImageSlots 按确定顺序收集图片落点。
//
// 认出一个 map 是一整个图片 part 之后不再递归进去：同一个 part 被数两次会让
// 「保留 N 张」实际只留 N/2 张。
//
// two 个上下文标志都只为「别把工具 schema 当成图片」服务：
//
//   - inArray：当前节点是某个数组的元素。**所有真实协议里的图片 part 都是数组元素**
//     （messages[].content[]、input[]、parts[]、function_call_output.output[]），
//     而函数工具的 parameters.properties 是普通对象、它的子节点不是数组元素。
//     靠这一条就能把「参数名叫 image_url 的工具定义」挡在外面 —— 否则那个
//     properties 会被当成图片 part 整块替换掉，工具定义直接消失。
//   - topLevelInput：当前节点直接挂在 Responses 的顶层 input[] 上。把那里的图片项
//     替换成裸的文本块会产生非法 item type，得写成一条 message。
func collectImageSlots(node any, out *[]imageSlot, inArray, topLevelInput bool) {
	switch v := node.(type) {
	case map[string]any:
		if inArray {
			if kind, ok := classifyImagePart(v); ok {
				*out = append(*out, imageSlot{node: v, kind: kind, topLevelInput: topLevelInput})
				return
			}
		}
		for _, key := range sortedKeys(v) {
			collectImageSlots(v[key], out, false, false)
		}
	case []any:
		for _, child := range v {
			collectImageSlots(child, out, true, topLevelInput)
		}
	}
}

// skipTrimField 报告顶层字段不属于对话内容，裁剪时不应改动。
//
// tools / tool_choice / response_format / metadata 是请求的配置部分：里面出现的
// 图片属于工具定义或结构化输出约束，把它们换成占位文本会直接让上游判非法，
// 而它们本来也不计入上游的图片张数。
func skipTrimField(key string) bool {
	switch key {
	case "tools", "tool_choice", "response_format", "metadata":
		return true
	}
	return false
}

// classifyImagePart 判断这个 map 是不是一个图片 part，以及属于哪种形状。
//
// 与 imagecompress.go 认的落点集合同一套，区别是**不做值校验**：压缩只处理能
// 得到字节的内联图，而计数必须把远程 URL 图（{"url":"https://…"}）也算上 ——
// 上游数的是张数。
//
// 两个例外都来自「Gemini 的同一对字段也可能装非图片」：inlineData / fileData
// 里可能是 PDF、音频、视频，上游对它们不按图片张数算。把它们计进图片会误裁掉
// 真正该留的图（实测：2 张图 + 1 个内联 PDF、上限 2 时，会把一张真图裁掉）。
// 所以这两个都只在 mime 明确是 image/* 时才算，认不出 mime 就不算。
//
// 调用方保证只在「数组元素」上调用本函数（见 collectImageSlots），所以这里的
// 简写分支不会碰到函数工具 parameters.properties 那种普通对象。
func classifyImagePart(m map[string]any) (imageSlotKind, bool) {
	typ, _ := m["type"].(string)
	switch typ {
	case "image_url", "input_image", "image_data":
		// image_data 是部分网关（以及图片生成接口的回填）用的类型名，
		// 压缩侧 imageKeys 同样认这个键。
		return slotChatImage, true
	case "image":
		// Anthropic 的 {"type":"image","source":{…}}。没有 source/data/url
		// 之一就不认 —— 别把某个工具 schema 里的 "type":"image" 当成图片。
		if _, ok := m["source"]; ok {
			return slotAnthropicImage, true
		}
		if _, ok := m["data"]; ok {
			return slotAnthropicImage, true
		}
		if _, ok := m["url"]; ok {
			return slotAnthropicImage, true
		}
		return 0, false
	}

	// 简写形态：不给 type，直接挂图片键（部分网关与旧客户端这么发，
	// 压缩侧 imageKeys 同样认这几个）。
	if _, ok := m["image_url"]; ok {
		return slotChatImage, true
	}
	// {"image": "data:image/…"} 或 {"image": {"url": …}}。
	// 不像图片就不认：{image} 也可能是个普通字段。
	if v, ok := m["image"]; ok && looksLikeImageValue(v) {
		return slotChatImage, true
	}
	// {"b64_json": "…"}：图片生成接口的内联返回。
	if _, ok := m["b64_json"]; ok {
		return slotChatImage, true
	}
	// {"image_data": "data:image/…"}：不带 type 的简写，与压缩侧的 imageKeys 对齐。
	if v, ok := m["image_data"]; ok && looksLikeImageValue(v) {
		return slotChatImage, true
	}
	for _, key := range []string{"inlineData", "inline_data", "fileData", "file_data"} {
		blob, ok := m[key].(map[string]any)
		if !ok {
			continue
		}
		mime, _ := blob["mimeType"].(string)
		if mime == "" {
			mime, _ = blob["mime_type"].(string)
		}
		if strings.HasPrefix(strings.ToLower(mime), "image/") {
			return slotGeminiImage, true
		}
	}
	return 0, false
}

// looksLikeImageValue 判断 {"image": v} 里的 v 像不像一张图。
//
// 内联图是 data:image/… 的字符串；远程图是 http(s) 地址，或 {"url": …} 对象。
// 其它形态（数字、布尔、任意对象）一律不算 —— 宁可少认一张，也不要把一个
// 恰好叫 image 的普通字段换掉。
func looksLikeImageValue(v any) bool {
	switch val := v.(type) {
	case string:
		return strings.HasPrefix(val, "data:image/") ||
			strings.HasPrefix(val, "http://") || strings.HasPrefix(val, "https://")
	case map[string]any:
		if _, ok := val["url"]; ok {
			return true
		}
		if _, ok := val["b64_json"]; ok {
			return true
		}
	}
	return false
}

// replaceWithText 就地清空一个图片 part 并写成文本块。
//
// 清空而不是只删图片相关的键：一个 part 里可能同时挂着上游不认的字段，
// 留一半比整个换掉更容易撞上 extra="forbid"。
func replaceWithText(slot imageSlot, placeholder, textType string) {
	for key := range slot.node {
		delete(slot.node, key)
	}
	switch {
	case slot.kind == slotGeminiImage:
		// Gemini 的 parts 用 {"text": …}，没有 type 字段。
		slot.node["text"] = placeholder
	case slot.topLevelInput:
		// Responses 的 input[] 顶层只能放「一条消息」，没有裸的文本 item ——
		// 凭空塞一个 {"type":"input_text"} 会被判成非法 item type。
		slot.node["type"] = "message"
		slot.node["role"] = "user"
		slot.node["content"] = []any{
			map[string]any{"type": "input_text", "text": placeholder},
		}
	default:
		slot.node["type"] = textType
		slot.node["text"] = placeholder
	}
}
