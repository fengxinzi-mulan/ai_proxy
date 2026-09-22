package proxy

import (
	"strings"
	"testing"

	"ai_proxy/internal/model"
)

// feedSSE 按转发层的真实路径驱动解析器：先组帧，再逐事件回调。
// 传入的 chunks 会被依次投喂，用于模拟 TCP 分片。
func feedSSE(a *analyzer, chunks ...string) {
	framer := newSSEFramer(func(_ []byte, event, data string) {
		a.OnEvent(event, data)
	})
	for _, c := range chunks {
		framer.Feed([]byte(c))
	}
	framer.Flush()
}

func TestAnalyzerOpenAIStream(t *testing.T) {
	body := "data: {\"id\":\"1\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"世界\"},\"finish_reason\":null}]}\n\n" +
		"data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: {\"id\":\"1\",\"choices\":[],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":50,\"total_tokens\":150,\"prompt_tokens_details\":{\"cached_tokens\":20}}}\n\n" +
		"data: [DONE]\n\n"

	a := newAnalyzer(model.FormatOpenAI, nil, "text/event-stream")
	if !a.Streaming() {
		t.Fatal("Content-Type 为 text/event-stream 时应按流式解析")
	}
	feedSSE(a, body)
	a.Finish()

	u := a.Result()
	if !u.Found {
		t.Fatal("应识别出上游返回的 usage")
	}
	if u.PromptTokens != 100 || u.CompletionTokens != 50 || u.CachedTokens != 20 {
		t.Errorf("用量解析错误: prompt=%d completion=%d cached=%d", u.PromptTokens, u.CompletionTokens, u.CachedTokens)
	}
	if u.TotalTokens != 150 {
		t.Errorf("total 应为 150，实际 %d", u.TotalTokens)
	}
	if a.Model() != "gpt-4o" {
		t.Errorf("模型名应为 gpt-4o，实际 %q", a.Model())
	}
	if !a.HasContent() {
		t.Error("收到了 content delta，应标记已出现内容")
	}
	if !a.StreamFinished() {
		t.Error("收到 [DONE] 应视为流正常结束")
	}
	// 只带 usage 的收尾块不能被当成内容，否则首 token 时间会被算到流末尾。
	if a.EstimateOutputTokens() == 0 {
		t.Error("输出 token 估算不应为 0")
	}
}

// TestAnalyzerChunkBoundary 模拟任意 TCP 分片，结果必须与整块投喂一致。
func TestAnalyzerChunkBoundary(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-3\",\"usage\":{\"input_tokens\":25,\"cache_read_input_tokens\":100,\"cache_creation_input_tokens\":10,\"output_tokens\":1}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":40}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	// 整块投喂作为基准。
	whole := newAnalyzer(model.FormatAnthropic, nil, "text/event-stream")
	feedSSE(whole, body)
	whole.Finish()

	// 逐字节投喂，覆盖所有可能的分片边界。
	byteWise := newAnalyzer(model.FormatAnthropic, nil, "text/event-stream")
	framer := newSSEFramer(func(_ []byte, event, data string) { byteWise.OnEvent(event, data) })
	for i := 0; i < len(body); i++ {
		framer.Feed([]byte{body[i]})
	}
	framer.Flush()

	if whole.Result() != byteWise.Result() {
		t.Errorf("逐字节分片与整块结果不一致: %+v vs %+v", whole.Result(), byteWise.Result())
	}
	if byteWise.Model() != "claude-3" {
		t.Errorf("应从 message_start 中取到模型名，实际 %q", byteWise.Model())
	}
	if !byteWise.StreamFinished() {
		t.Error("收到 message_stop 应视为流正常结束")
	}
}

func TestAnalyzerAnthropicStreamUsageMerge(t *testing.T) {
	// Anthropic 的输入用量在 message_start、输出用量在 message_delta，
	// 必须按字段合并：早期实现里后者会把前者覆盖成 0。
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":25,\"cache_read_input_tokens\":100,\"cache_creation_input_tokens\":10}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":40}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	a := newAnalyzer(model.FormatAnthropic, nil, "text/event-stream")
	feedSSE(a, body)

	u := a.Result()
	// Anthropic 的 input_tokens 不含缓存部分，统一口径后应为 25+100+10。
	if u.PromptTokens != 135 {
		t.Errorf("输入总量应为 135（含缓存读写），实际 %d", u.PromptTokens)
	}
	if u.CachedTokens != 100 {
		t.Errorf("缓存命中应为 100，实际 %d", u.CachedTokens)
	}
	if u.CacheWriteTokens != 10 {
		t.Errorf("缓存写入应为 10，实际 %d", u.CacheWriteTokens)
	}
	if u.CompletionTokens != 40 {
		t.Errorf("输出应为 40，实际 %d", u.CompletionTokens)
	}
}

func TestAnalyzerReasoningCountsAsContent(t *testing.T) {
	// 推理模型的第一批 token 是思维链：它也应该触发首 token 打点，
	// 否则 TTFT 会被算到正文出现时，严重虚高。
	body := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"让我想想\"}}]}\n\n" +
		"data: [DONE]\n\n"

	a := newAnalyzer(model.FormatOpenAI, nil, "text/event-stream")
	feedSSE(a, body)

	if !a.HasContent() {
		t.Error("仅收到 reasoning_content 时也应视为已出现内容")
	}
}

func TestAnalyzerOpenAINonStreaming(t *testing.T) {
	body := `{"id":"1","model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12}}`

	a := newAnalyzer(model.FormatOpenAI, nil, "application/json")
	if a.Streaming() {
		t.Fatal("application/json 不应按流式解析")
	}
	a.Feed([]byte(body))
	a.Finish()

	u := a.Result()
	if u.PromptTokens != 10 || u.CompletionTokens != 2 {
		t.Errorf("用量解析错误: %+v", u)
	}
	if a.Model() != "gpt-4o-mini" {
		t.Errorf("模型名解析错误: %q", a.Model())
	}
}

func TestAnalyzerAnthropicNonStreaming(t *testing.T) {
	body := `{"id":"msg_1","model":"claude-sonnet-4","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":25,"output_tokens":40,"cache_read_input_tokens":100,"cache_creation_input_tokens":10}}`

	a := newAnalyzer(model.FormatAnthropic, nil, "application/json")
	a.Feed([]byte(body))
	a.Finish()

	u := a.Result()
	if u.PromptTokens != 135 || u.CachedTokens != 100 || u.CacheWriteTokens != 10 || u.CompletionTokens != 40 {
		t.Errorf("Anthropic 非流式用量解析错误: %+v", u)
	}
	if !a.StreamFinished() {
		t.Error("非流式响应带 stop_reason，应视为正常结束")
	}
}

func TestAnalyzerGeminiArrayResponse(t *testing.T) {
	// Gemini 不开 alt=sse 时返回 JSON 数组，用量在最后一个元素里。
	body := `[{"candidates":[{"content":{"parts":[{"text":"你"}]}}]},
	          {"candidates":[{"content":{"parts":[{"text":"好"}]},"finishReason":"STOP"}],
	           "usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3,"totalTokenCount":11,"cachedContentTokenCount":2}}]`

	a := newAnalyzer(model.FormatGemini, nil, "application/json")
	a.Feed([]byte(body))
	a.Finish()

	u := a.Result()
	if u.PromptTokens != 8 || u.CompletionTokens != 3 || u.CachedTokens != 2 {
		t.Errorf("Gemini 数组响应用量解析错误: %+v", u)
	}
	// Gemini 的 promptTokenCount 已含缓存，不应再叠加。
	if u.TotalTokens != 11 {
		t.Errorf("total 应为 11，实际 %d", u.TotalTokens)
	}
	if !a.StreamFinished() {
		t.Error("带 finishReason 应视为正常结束")
	}
}

func TestAnalyzerResponsesStream(t *testing.T) {
	body := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"he\"}\n\n" +
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"llo\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5\",\"usage\":{\"input_tokens\":30,\"output_tokens\":7,\"total_tokens\":37,\"input_tokens_details\":{\"cached_tokens\":5}}}}\n\n"

	a := newAnalyzer(model.FormatResponses, nil, "text/event-stream")
	feedSSE(a, body)

	u := a.Result()
	if u.PromptTokens != 30 || u.CompletionTokens != 7 || u.CachedTokens != 5 {
		t.Errorf("responses 格式用量解析错误: %+v", u)
	}
	if a.Model() != "gpt-5" {
		t.Errorf("应从 response 对象里取到模型名，实际 %q", a.Model())
	}
	if !a.StreamFinished() {
		t.Error("收到 response.completed 应视为流正常结束")
	}
}

func TestAnalyzerCustomMapping(t *testing.T) {
	cu := &model.CustomUsageMapping{
		PromptTokens:     "meta.in",
		CachedTokens:     "meta.cached",
		CompletionTokens: "meta.out",
		Model:            "meta.model",
	}
	body := `{"meta":{"in":11,"out":4,"cached":3,"model":"my-model"},"reply":"ok"}`

	a := newAnalyzer(model.FormatCustom, cu, "application/json")
	a.Feed([]byte(body))
	a.Finish()

	u := a.Result()
	if u.PromptTokens != 11 || u.CompletionTokens != 4 || u.CachedTokens != 3 {
		t.Errorf("自定义路径用量解析错误: %+v", u)
	}
	if a.Model() != "my-model" {
		t.Errorf("自定义模型路径解析错误: %q", a.Model())
	}
}

func TestAnalyzerErrorBody(t *testing.T) {
	body := `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`
	a := newAnalyzer(model.FormatOpenAI, nil, "application/json")
	a.Feed([]byte(body))
	a.Finish()

	if got := a.ErrMsg(); got != "Incorrect API key provided" {
		t.Errorf("应提取出错误描述，实际 %q", got)
	}
	if a.Result().Found {
		t.Error("错误响应不应被认为包含用量")
	}
}

// TestUsageOnlyChunkDetection 覆盖「剥离 usage 块」的判定逻辑。
func TestUsageOnlyChunkDetection(t *testing.T) {
	a := newAnalyzer(model.FormatOpenAI, nil, "text/event-stream")

	cases := []struct {
		name   string
		data   string
		expect bool
	}{
		{"只带 usage 的收尾块", `{"choices":[],"usage":{"prompt_tokens":1}}`, true},
		{"带内容的正常块", `{"choices":[{"delta":{"content":"x"}}]}`, false},
		{"choices 缺失但有 usage", `{"usage":{"prompt_tokens":1}}`, true},
		{"空 choices 无 usage", `{"choices":[]}`, false},
		{"DONE 标记", `[DONE]`, false},
	}
	for _, c := range cases {
		if got := a.IsUsageOnlyChunk("", c.data); got != c.expect {
			t.Errorf("%s: 期望 %v，实际 %v", c.name, c.expect, got)
		}
	}

	// 其他格式不参与剥离，避免误删正文。
	other := newAnalyzer(model.FormatAnthropic, nil, "text/event-stream")
	if other.IsUsageOnlyChunk("message_delta", `{"choices":[],"usage":{}}`) {
		t.Error("非 OpenAI 格式不应判定为 usage 专用块")
	}
}

// TestSSEFramerVerbatim 保证组帧后拼回的字节与原始输入完全一致。
func TestSSEFramerVerbatim(t *testing.T) {
	raw := ": ping\n\n" +
		"event: content_block_delta\r\ndata: {\"a\":1}\r\n\r\n" +
		"data: line1\ndata: line2\n\n" +
		"data: [DONE]\n\n" +
		"data: tail-without-trailing-blank"

	var rebuilt strings.Builder
	seen := []string{}
	framer := newSSEFramer(func(b []byte, event, data string) {
		rebuilt.Write(b)
		seen = append(seen, event+"|"+data)
	})
	framer.Feed([]byte(raw))
	framer.Flush()

	if rebuilt.String() != raw {
		t.Errorf("重组字节与原始输入不一致:\n原始 %q\n重组 %q", raw, rebuilt.String())
	}

	want := []string{
		"|", // 注释行后跟空行，data 为空
		"content_block_delta|{\"a\":1}",
		"|line1\nline2",
		"|[DONE]",
		"|tail-without-trailing-blank",
	}
	if len(seen) != len(want) {
		t.Fatalf("事件数应为 %d，实际 %d: %v", len(want), len(seen), seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("第 %d 个事件: 期望 %q，实际 %q", i, want[i], seen[i])
		}
	}
}

func TestEstimateTokens(t *testing.T) {
	if got := estimateTokens(""); got != 0 {
		t.Errorf("空串应估算为 0，实际 %d", got)
	}
	// 中文按字计数。
	if got := estimateTokens("你好世界"); got != 4 {
		t.Errorf("四个汉字应估算为 4，实际 %d", got)
	}
	// 英文按 4 字符 1 token。
	if got := estimateTokens("abcdefgh"); got != 2 {
		t.Errorf("8 个英文字符应估算为 2，实际 %d", got)
	}
}

func TestEstimatePromptTokens(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"你好世界"}]}`)
	got := estimatePromptTokens(model.FormatOpenAI, body)
	// 4 个汉字 + 1 条消息的结构开销 4。
	if got != 8 {
		t.Errorf("应估算为 8，实际 %d", got)
	}

	anthropicBody := []byte(`{"model":"m","system":"你好","messages":[{"role":"user","content":"世界"}]}`)
	// system 2 字 + 消息正文 2 字，共 4，再加 1 条消息的结构开销 4。
	if got := estimatePromptTokens(model.FormatAnthropic, anthropicBody); got != 8 {
		t.Errorf("Anthropic 结构应估算为 8（含 system 与消息开销），实际 %d", got)
	}
}
