package proxy

import (
	"bytes"
	"encoding/json"
	"strings"

	"ai_proxy/internal/model"
)

// maxOutputCapture 已不再需要：输出只做增量 token 估算，不留原文。
// 保留 parseBodyLimit 作为非流式响应体的解析上限。
const parseBodyLimit = 8 << 20

// analyzer 一边把响应体原样交给客户端，一边旁路解析出用量、模型、错误、
// 首 token 出现时机等指标。
//
// 它只读不写：除了「剥离 usage 块」这个显式开关外，不参与任何响应改写。
//
// 流式路径下组帧由转发层（sseFramer）负责，转发层逐个事件回调 OnEvent；
// 非流式路径下由转发层把字节流交给 Feed，结束时调用 Finish。
type analyzer struct {
	format    model.APIFormat
	cu        *model.CustomUsageMapping
	streaming bool // 响应 Content-Type 是否为 text/event-stream

	body     bytes.Buffer
	bodyOver bool

	usage        Usage
	rawInput     int // Anthropic 的 input_tokens 原文，最后统一补齐口径
	model        string
	errMsg       string
	finishReason string
	gotContent   bool
	done         bool // 收到 OpenAI 的 [DONE]
	terminal     bool // 收到任何表示「流正常结束」的终止信号

	// estTokens 是增量累积的输出 token 估算值。
	// 增量累加而不是留全文再估算，长响应下内存占用是常数级。
	estTokens int
}

// newAnalyzer 依据响应类型选择解析路径。
func newAnalyzer(format model.APIFormat, cu *model.CustomUsageMapping, contentType string) *analyzer {
	return &analyzer{
		format:    format,
		cu:        cu,
		streaming: isEventStream(contentType),
	}
}

// isEventStream 判断响应是否为 SSE。
func isEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

// Streaming 报告本次响应是否按 SSE 处理。
func (a *analyzer) Streaming() bool { return a.streaming }

// Feed 消费非流式响应体的一段字节。流式响应由 OnEvent 驱动，此处为空操作。
func (a *analyzer) Feed(p []byte) {
	if a.streaming || a.bodyOver {
		return
	}
	// 非流式响应体要整体解析才能拿到 usage，但不能无上限缓存，
	// 超出 parseBodyLimit 后放弃解析，统计退化为本地估算。
	if a.body.Len()+len(p) > parseBodyLimit {
		a.bodyOver = true
		a.body.Reset()
		return
	}
	a.body.Write(p)
}

// Finish 通知解析器非流式响应体已结束。
func (a *analyzer) Finish() {
	if a.streaming {
		return
	}
	a.parseWholeBody(a.body.Bytes())
}

// parseBodyLimit 见文件顶部常量。

func (a *analyzer) parseWholeBody(body []byte) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return
	}
	// 少数网关（如 Gemini 未开 alt=sse）把流式响应当作 JSON 数组返回，
	// 此时数组的最后一个元素才带累计用量。
	if trimmed[0] == '[' {
		var arr []json.RawMessage
		if err := json.Unmarshal(trimmed, &arr); err != nil {
			return
		}
		for _, raw := range arr {
			if obj, err := decodeJSONObject(raw); err == nil {
				a.handleObject("", obj)
			}
		}
		return
	}
	if obj, err := decodeJSONObject(trimmed); err == nil {
		a.handleObject("", obj)
	}
}

// OnEvent 由转发层对每个完整的 SSE 事件回调。
func (a *analyzer) OnEvent(event string, data string) {
	if data == "" {
		return
	}
	if data == "[DONE]" {
		a.done = true
		a.terminal = true
		return
	}
	obj, err := decodeJSONObject([]byte(data))
	if err != nil {
		return
	}
	a.handleObject(event, obj)
}

// HasContent 报告是否已经出现过模型生成的内容。
//
// 判定口径：只要出现正文 delta 或思维链 delta 就算。这是「首 token」的
// 标准定义（对推理模型来说，第一个推理 token 就是第一个 token），
// role-only 包、ping、以及只带 usage 的收尾包都不算。
func (a *analyzer) HasContent() bool { return a.gotContent }

// Model 返回上游回显的模型名。
func (a *analyzer) Model() string { return a.model }

// ErrMsg 返回响应体里携带的错误描述。
func (a *analyzer) ErrMsg() string { return a.errMsg }

// FinishReason 返回结束原因。
func (a *analyzer) FinishReason() string { return a.finishReason }

// Done 报告流是否以 [DONE] 正常收尾。
func (a *analyzer) Done() bool { return a.done }

// StreamFinished 报告是否收到了「本次流正常结束」的终止信号。
//
// 各家格式的终止信号不同：OpenAI 是 [DONE] 或 finish_reason，
// Anthropic 是 message_stop 或 stop_reason，Gemini 是 finishReason，
// responses 接口是 response.completed。任一出现即认为流是完整的；
// 一条有内容却始终没有终止信号的流，基本可以判定被中途切断了。
func (a *analyzer) StreamFinished() bool { return a.terminal }

// Truncated 报告非流式响应体是否因超限而放弃了解析。
func (a *analyzer) Truncated() bool { return a.bodyOver }

// EstimateOutputTokens 返回增量累积的输出 token 估算值。
func (a *analyzer) EstimateOutputTokens() int { return a.estTokens }

// Result 返回收敛后的用量。
//
// handleAnthropic 在处理每个 usage 对象时已经把输入侧按统一口径补齐，
// 这里只需做总账一致性收敛。
func (a *analyzer) Result() Usage {
	a.usage.normalize()
	return a.usage
}

// EstimatePromptTokens 在缺少上游用量时估算输入 token，
// 单纯转发到包级函数，保留方法形式只是为了调用点读起来对称。
func (a *analyzer) EstimatePromptTokens(format model.APIFormat, reqBody []byte) int {
	return estimatePromptTokens(format, reqBody)
}

// IsUsageOnlyChunk 判断一个 OpenAI 流式块是否是「只带用量、没有内容」的收尾块。
//
// 这类块是本次注入 stream_options.include_usage 带来的副产物，
// 部分严格的客户端会因为它 choices 为空而报错，因此提供按需剥离的能力。
func (a *analyzer) IsUsageOnlyChunk(event, data string) bool {
	if a.format != model.FormatOpenAI || data == "" || data == "[DONE]" {
		return false
	}
	obj, err := decodeJSONObject([]byte(data))
	if err != nil {
		return false
	}
	if choices, ok := obj["choices"].([]any); ok && len(choices) > 0 {
		return false
	}
	_, hasUsage := obj["usage"]
	return hasUsage
}

// handleObject 是所有格式共用的单对象处理入口。
// event 仅在 SSE 路径下非空，部分格式需要靠它区分事件类型。
func (a *analyzer) handleObject(event string, obj map[string]any) {
	if m := extractModel(a.format, a.cu, obj); m != "" {
		a.model = m
	}
	if e := extractError(obj); e != "" {
		a.errMsg = e
	}

	switch a.format {
	case model.FormatOpenAI:
		a.handleOpenAI(obj)
	case model.FormatResponses:
		a.handleResponses(event, obj)
	case model.FormatAnthropic:
		a.handleAnthropic(event, obj)
	case model.FormatGemini:
		a.handleGemini(obj)
	case model.FormatCustom:
		if u := extractUsageFromObject(model.FormatCustom, a.cu, obj); u.Found {
			a.usage = u
		}
	}
}

func (a *analyzer) handleOpenAI(obj map[string]any) {
	if u := extractUsageFromObject(model.FormatOpenAI, nil, obj); u.Found {
		a.usage = u
	}
	choices, ok := obj["choices"].([]any)
	if !ok || len(choices) == 0 {
		return
	}
	c, ok := choices[0].(map[string]any)
	if !ok {
		return
	}
	if fr, ok := getString(c, "finish_reason"); ok && fr != "" {
		a.finishReason = fr
		a.terminal = true
	}
	// 流式在 delta 里，非流式在 message 里，两处都要看。
	for _, key := range []string{"delta", "message"} {
		container, ok := getMap(c, key)
		if !ok {
			continue
		}
		// content 可能是字符串，也可能是分块数组。
		if s, ok := container["content"].(string); ok {
			a.addOutput(s)
		} else if arr, ok := container["content"].([]any); ok {
			var sb strings.Builder
			appendTextValue(arr, &sb)
			a.addOutput(sb.String())
		}
		// 各家推理字段命名不统一，能识别的都算内容。
		for _, key := range []string{"reasoning_content", "reasoning"} {
			if s, ok := container[key].(string); ok {
				a.addOutput(s)
			}
		}
	}
}

func (a *analyzer) handleResponses(event string, obj map[string]any) {
	if !a.streaming {
		// 非流式响应体：usage 在顶层，正文在 output 数组里。
		if u := extractUsageFromObject(model.FormatResponses, nil, obj); u.Found {
			a.usage = u
		}
		var sb strings.Builder
		appendTextValue(obj["output"], &sb)
		a.addOutput(sb.String())
		if fr, ok := getString(obj, "status"); ok && fr != "" {
			a.finishReason = fr
		}
		return
	}

	switch event {
	case "response.completed", "response.incomplete":
		if u := extractUsageFromObject(model.FormatResponses, nil, obj); u.Found {
			a.usage = u
		}
		a.terminal = true
	case "response.output_text.delta", "response.reasoning_summary_text.delta",
		"response.reasoning_text.delta":
		if s, ok := getString(obj, "delta"); ok {
			a.addOutput(s)
		}
	case "response.failed":
		if e := extractError(obj); e != "" {
			a.errMsg = e
		}
	}
}

func (a *analyzer) handleAnthropic(event string, obj map[string]any) {
	// Anthropic 的输入用量与输出用量分布在两个事件里，
	// 必须按字段合并，不能整体覆盖。
	usagePath := "usage"
	if event == "message_start" {
		usagePath = "message.usage"
		if m, ok := getMap(obj, "message"); ok {
			if s, ok := getString(m, "model"); ok && s != "" {
				a.model = s
			}
		}
	}
	if usage, ok := getMap(obj, usagePath); ok {
		a.usage.Found = true
		if n, ok := getPath(usage, "input_tokens"); ok {
			a.rawInput = n
		}
		if n, ok := getPath(usage, "cache_read_input_tokens"); ok {
			a.usage.CachedTokens = n
		}
		if n, ok := getPath(usage, "cache_creation_input_tokens"); ok {
			a.usage.CacheWriteTokens = n
		}
		if n, ok := getPath(usage, "output_tokens"); ok {
			a.usage.CompletionTokens = n // message_delta 给的是累计值
		}
		a.usage.PromptTokens = a.rawInput + a.usage.CachedTokens + a.usage.CacheWriteTokens
	}

	switch event {
	case "content_block_delta":
		if delta, ok := getMap(obj, "delta"); ok {
			for _, key := range []string{"text", "thinking"} {
				if s, ok := delta[key].(string); ok {
					a.addOutput(s)
				}
			}
		}
	case "message_delta":
		if delta, ok := getMap(obj, "delta"); ok {
			if fr, ok := getString(delta, "stop_reason"); ok && fr != "" {
				a.finishReason = fr
				a.terminal = true
			}
		}
	case "message_stop":
		a.terminal = true
	case "error":
		if e := extractError(obj); e != "" {
			a.errMsg = e
		}
	}

	// 非流式响应体：usage 在顶层，正文在 content 数组里。
	if event == "" {
		if arr, ok := obj["content"].([]any); ok {
			var sb strings.Builder
			appendTextValue(arr, &sb)
			a.addOutput(sb.String())
		}
		if fr, ok := getString(obj, "stop_reason"); ok && fr != "" {
			a.finishReason = fr
			a.terminal = true
		}
	}
}

func (a *analyzer) handleGemini(obj map[string]any) {
	// Gemini 流式每个块都带累计的 usageMetadata，直接取最新即可。
	if u := extractUsageFromObject(model.FormatGemini, nil, obj); u.Found {
		a.usage = u
	}
	candidates, ok := obj["candidates"].([]any)
	if !ok || len(candidates) == 0 {
		return
	}
	c, ok := candidates[0].(map[string]any)
	if !ok {
		return
	}
	if fr, ok := getString(c, "finishReason"); ok && fr != "" {
		a.finishReason = fr
		a.terminal = true
	}
	if content, ok := getMap(c, "content"); ok {
		if parts, ok := content["parts"].([]any); ok {
			var sb strings.Builder
			appendTextValue(parts, &sb)
			a.addOutput(sb.String())
		}
	}
}

// addOutput 累积输出文本的 token 估算，同时标记已出现内容。
//
// 只累加估算值、不保留原文：长响应下内存占用是常数级的，
// 而估算值恰好是「上游没给用量」时唯一需要的东西。
func (a *analyzer) addOutput(s string) {
	if s == "" {
		return
	}
	a.gotContent = true
	a.estTokens += estimateTokens(s)
}

// ---------- SSE 组帧 ----------

// sseFramer 把字节流切成一个个完整的 SSE 事件。
//
// 它同时重建每个事件的原始字节，这样转发路径可以逐事件原样透传，
// 既不改变客户端看到的内容，又能在事件粒度上做解析与按需剥离。
//
// 回调收到的 raw 是内部复用缓冲区的一段切片，仅在回调返回前有效，
// 调用方需要保留时必须自行拷贝。
type sseFramer struct {
	raw   bytes.Buffer    // 当前事件的原始字节（含换行）
	line  bytes.Buffer    // 当前行的原始字节
	event string          // 当前事件名
	data  strings.Builder // 当前事件的 data 累积
	cb    func(raw []byte, event, data string)
}

func newSSEFramer(cb func(raw []byte, event, data string)) *sseFramer {
	return &sseFramer{cb: cb}
}

// Feed 消费字节流。
func (f *sseFramer) Feed(p []byte) {
	for _, b := range p {
		f.line.WriteByte(b)
		f.raw.WriteByte(b)
		if b == '\n' {
			f.lineDone()
		}
	}
}

// Flush 处理流末尾没有以空行结束的残余事件。
func (f *sseFramer) Flush() {
	if f.line.Len() > 0 {
		f.lineDone()
	}
	if f.raw.Len() > 0 {
		f.dispatch()
	}
}

func (f *sseFramer) lineDone() {
	line := f.line.Bytes()
	f.line.Reset()

	// 去掉行尾换行与可能的 \r
	line = bytes.TrimRight(line, "\r\n")
	if len(line) == 0 {
		// 空行表示事件结束。
		f.dispatch()
		return
	}
	if line[0] == ':' {
		return // 注释行（含心跳），保留在 raw 里原样转发
	}

	field, value, found := bytes.Cut(line, []byte(":"))
	if !found {
		return
	}
	// 规范规定冒号后若紧跟一个空格则删除该空格。
	value = bytes.TrimPrefix(value, []byte(" "))
	switch string(field) {
	case "event":
		f.event = string(value)
	case "data":
		if f.data.Len() > 0 {
			f.data.WriteByte('\n')
		}
		f.data.Write(value)
	}
}

func (f *sseFramer) dispatch() {
	if f.raw.Len() == 0 {
		return
	}
	event, data := f.event, f.data.String()
	if f.cb != nil {
		// 传切片而非拷贝：回调只在本次调用内使用它，转发会立即写出。
		f.cb(f.raw.Bytes(), event, data)
	}
	f.raw.Reset()
	f.event = ""
	f.data.Reset()
}
