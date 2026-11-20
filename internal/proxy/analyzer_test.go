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

func TestAnalyzerTerminalEvent(t *testing.T) {
	// 终止事件 ≠ StreamFinished：chat 的 finish_reason 之后还有 usage 块和 [DONE]，
	// 提前收工会把用量丢掉，所以只有 [DONE] 才算「客户端收到的最后一个事件」。
	cases := []struct {
		name   string
		format model.APIFormat
		event  string
		data   string
		want   bool
	}{
		{"chat 的 DONE", model.FormatOpenAI, "", "[DONE]", true},
		{"chat 的普通数据块", model.FormatOpenAI, "", `{"choices":[]}`, false},
		{"chat 的 usage 块", model.FormatOpenAI, "", `{"choices":[],"usage":{}}`, false},
		{"anthropic 的 message_stop", model.FormatAnthropic, "message_stop", `{}`, true},
		{"anthropic 的 message_delta", model.FormatAnthropic, "message_delta", `{}`, false},
		{"responses 不走这里", model.FormatResponses, "response.completed", `{}`, false},
		{"gemini 不走这里", model.FormatGemini, "", `{"candidates":[]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := newAnalyzer(tc.format, nil, "text/event-stream")
			if got := a.TerminalEvent(tc.event, tc.data); got != tc.want {
				t.Errorf("TerminalEvent(%q, %q) = %v，期望 %v", tc.event, tc.data, got, tc.want)
			}
		})
	}
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

func TestAnalyzerToolCallsCountAsContent(t *testing.T) {
	// 纯工具调用的一轮没有正文，但工具参数分片就是模型吐出的第一批 token。
	// 不标记内容的话首 token 永远打不上点（列表里表现为「—」），TPS 也会跟着缺失。
	body := "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"function\":{\"name\":\"read\",\"arguments\":\"\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"{\\\"path\\\":\"}}]}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
		"data: [DONE]\n\n"

	a := newAnalyzer(model.FormatOpenAI, nil, "text/event-stream")
	feedSSE(a, body)

	if !a.HasContent() {
		t.Error("仅收到 tool_calls 时也应视为已出现内容")
	}
	// 参数是结构化 JSON，参与估算会让「上游没给用量」时的输出量严重虚高。
	if got := a.EstimateOutputTokens(); got != 0 {
		t.Errorf("工具参数不应参与 token 估算，实际估出 %d", got)
	}
	if !a.StreamFinished() {
		t.Error("finish_reason 与 [DONE] 都应视为流正常结束")
	}
}

func TestAnalyzerAnthropicToolUseCountsAsContent(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude\",\"usage\":{\"input_tokens\":10,\"output_tokens\":1}}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"toolu_1\",\"name\":\"read\",\"input\":{}}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"a\\\":1}\"}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	a := newAnalyzer(model.FormatAnthropic, nil, "text/event-stream")
	feedSSE(a, body)

	if !a.HasContent() {
		t.Error("tool_use 块应视为已出现内容")
	}
	if got := a.EstimateOutputTokens(); got != 0 {
		t.Errorf("工具参数不应参与 token 估算，实际估出 %d", got)
	}
	if !a.StreamFinished() {
		t.Error("message_stop 应视为流正常结束")
	}
}

func TestAnalyzerGeminiFunctionCallCountsAsContent(t *testing.T) {
	body := "data: {\"candidates\":[{\"content\":{\"parts\":[{\"functionCall\":{\"name\":\"read\",\"args\":{\"path\":\"a.txt\"}}}],\"role\":\"model\"},\"finishReason\":\"STOP\"}]}\n\n"

	a := newAnalyzer(model.FormatGemini, nil, "text/event-stream")
	feedSSE(a, body)

	if !a.HasContent() {
		t.Error("functionCall 分片应视为已出现内容")
	}
	if !a.StreamFinished() {
		t.Error("finishReason 应视为流正常结束")
	}
}

func TestAnalyzerResponsesFunctionCallCountsAsContent(t *testing.T) {
	body := "event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"type\":\"function_call\",\"name\":\"read\"}}\n\n" +
		"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"delta\":\"{\\\"a\\\":1}\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":10,\"output_tokens\":5}}}\n\n"

	a := newAnalyzer(model.FormatResponses, nil, "text/event-stream")
	feedSSE(a, body)

	if !a.HasContent() {
		t.Error("function_call 项应视为已出现内容")
	}
	if !a.StreamFinished() {
		t.Error("response.completed 应视为流正常结束")
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

func TestAnalyzerResponsesOutcomes(t *testing.T) {
	for _, tc := range []struct {
		status string
		detail string
		reason string
	}{
		{"completed", `"error":null`, ""},
		{"failed", `"error":{"message":"overloaded"}`, "overloaded"},
		{"failed", `"error":null`, "Responses"},
		{"incomplete", `"incomplete_details":{"reason":"max_output_tokens"}`, "max_output_tokens"},
		{"incomplete", `"incomplete_details":null`, "Responses"},
	} {
		for _, streaming := range []bool{true, false} {
			name := tc.status + "/json"
			contentType := "application/json"
			if streaming {
				name = tc.status + "/sse"
				contentType = "text/event-stream"
			}
			t.Run(name+"/"+tc.reason, func(t *testing.T) {
				a := newAnalyzer(model.FormatResponses, nil, contentType)
				body := `{"status":"` + tc.status + `","usage":{"input_tokens":30,"output_tokens":7},` + tc.detail + `}`
				if streaming {
					feedSSE(a, "data: {\"type\":\"response."+tc.status+"\",\"response\":"+body+"}\n\n")
				} else {
					a.Feed([]byte(body))
					a.Finish()
				}
				if a.FinishReason() != tc.status || !a.StreamFinished() {
					t.Fatalf("missing terminal status: %q", a.FinishReason())
				}
				if tc.reason == "" && a.ErrMsg() != "" || tc.reason != "" && !strings.Contains(a.ErrMsg(), tc.reason) {
					t.Fatalf("wrong error: %q", a.ErrMsg())
				}
				if usage := a.Result(); !usage.Found || usage.PromptTokens != 30 || usage.CompletionTokens != 7 {
					t.Fatalf("usage was lost: %+v", usage)
				}
			})
		}
	}
}

func TestAnalyzerResponsesEventNamePrecedence(t *testing.T) {
	a := newAnalyzer(model.FormatResponses, nil, "text/event-stream")
	feedSSE(a, "event: response.output_text.delta\ndata: {\"type\":\"response.completed\",\"delta\":\"hi\"}\n\n")
	if a.StreamFinished() || !a.HasContent() {
		t.Fatal("JSON type must only be used when event name is absent")
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
		// `usage: null` 是「有键没值」，同样不是 usage 块；按键名判会把该转发的
		// 空块剥掉。
		{"usage 为 null", `{"choices":[],"usage":null}`, false},
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

	// Responses 的 input 可以是一整段字符串（{"input":"hi"} 最简写法）。
	// 不认这种形态会把全部输入算成 0。
	responsesBody := []byte(`{"model":"m","input":"你好世界"}`)
	if got := estimatePromptTokens(model.FormatResponses, responsesBody); got != 8 {
		t.Errorf("Responses 字符串 input 应估算为 8（4 字 + 1 条消息开销），实际 %d", got)
	}
}

// Anthropic 的 SSE 有些网关不写 `event:` 行，只在 data 里带 type。
// 只认事件名会让 usage 与正文全部丢失。
func TestAnalyzerAnthropicSSEWithoutEventLines(t *testing.T) {
	body := "data: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-3\",\"usage\":{\"input_tokens\":25,\"cache_read_input_tokens\":100,\"cache_creation_input_tokens\":10}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\" there\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":40}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n"

	a := newAnalyzer(model.FormatAnthropic, nil, "text/event-stream")
	feedSSE(a, body)

	u := a.Result()
	if !u.Found {
		t.Fatal("缺 event 行时也应从 data.type 推出事件类型并读到用量")
	}
	if u.PromptTokens != 135 || u.CachedTokens != 100 || u.CacheWriteTokens != 10 {
		t.Errorf("输入侧用量解析错误: %+v", u)
	}
	if u.CompletionTokens != 40 {
		t.Errorf("输出应为 40，实际 %d", u.CompletionTokens)
	}
	if a.Model() != "claude-3" {
		t.Errorf("模型名应来自 message_start，实际 %q", a.Model())
	}
	if !a.HasContent() {
		t.Error("正文 delta 应被计入输出")
	}
	if !a.StreamFinished() {
		t.Error("data 里的 message_stop 应视为流正常结束")
	}
	// 终止事件的判定同样要认 data.type。
	if !a.TerminalEvent("", `{"type":"message_stop"}`) {
		t.Error("缺 event 行时 message_stop 也应判为终止事件")
	}
	if a.TerminalEvent("", `{"type":"content_block_delta"}`) {
		t.Error("普通事件不该判为终止事件")
	}
}

// 非流式 Anthropic 响应同样带 type:"message"，不能按 SSE 逐行解析后当成事件。
func TestAnalyzerAnthropicNonStreamingKeepsUsage(t *testing.T) {
	body := `{"type":"message","model":"claude-sonnet-4","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn",
		"usage":{"input_tokens":10,"output_tokens":5}}`

	a := newAnalyzer(model.FormatAnthropic, nil, "application/json")
	a.Feed([]byte(body))
	a.Finish()

	u := a.Result()
	if u.PromptTokens != 10 || u.CompletionTokens != 5 || !u.Found {
		t.Errorf("非流式用量解析错误: %+v", u)
	}
	if !a.HasContent() {
		t.Error("非流式正文应计入输出")
	}
}

// 流式输出估算不能逐块向上取整：8 个单字符块合起来是 2 token，
// 逐块 (n+3)/4 会算成 8 token。
func TestAnalyzerEstimateOutputNoPerChunkInflation(t *testing.T) {
	a := newAnalyzer(model.FormatOpenAI, nil, "text/event-stream")
	var sb strings.Builder
	for i := 0; i < 8; i++ {
		sb.WriteString("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
	}
	feedSSE(a, sb.String())

	if got := a.EstimateOutputTokens(); got != 2 {
		t.Errorf("8 个字符应估算为 2 token（不能逐块向上取整），实际 %d", got)
	}
}
