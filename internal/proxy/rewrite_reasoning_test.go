package proxy

import (
	"strings"
	"testing"

	"ai_proxy/internal/model"
)

// 还原后的 assistant 消息必须能被 Mistral 直接接受：thinking 块在前，
// 正文（如果有）作为普通 text 块跟在后面，顶层不再有 reasoning_content。
func TestRewriteReplaysReasoningAsThinkChunk(t *testing.T) {
	cases := []struct {
		name      string
		message   string
		wantTexts []string // content 数组里各块的 text，按顺序
		wantThink int      // thinking 块数量
	}{
		{
			name:      "content 为 null",
			message:   `{"role":"assistant","reasoning_content":"先想一步","tool_calls":[{"id":"call_1","type":"function","function":{"name":"f","arguments":"{}"}}]}`,
			wantTexts: []string{"先想一步"},
			wantThink: 1,
		},
		{
			name:      "content 为空串",
			message:   `{"role":"assistant","content":"","reasoning_content":"先想一步"}`,
			wantTexts: []string{"先想一步"},
			wantThink: 1,
		},
		{
			name:      "content 为字符串",
			message:   `{"role":"assistant","content":"正文","reasoning_content":"先想一步"}`,
			wantTexts: []string{"先想一步", "正文"},
			wantThink: 1,
		},
		{
			name:      "content 为数组",
			message:   `{"role":"assistant","content":[{"type":"text","text":"正文"}],"reasoning_content":"先想一步"}`,
			wantTexts: []string{"先想一步", "正文"},
			wantThink: 1,
		},
		{
			name:      "已有 thinking 块时不重复插入",
			message:   `{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"旧思考"}],"closed":true},{"type":"text","text":"正文"}],"reasoning_content":"先想一步"}`,
			wantTexts: []string{"旧思考", "正文"},
			wantThink: 1,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := baseProvider()
			p.ReplayReasoningAsThinkChunk = true
			p.StripUnsupportedParams = true
			body := []byte(`{"model":"m","messages":[` + tc.message + `]}`)

			out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
			if !hasMod(mods, "reasoning_to_think_chunk") {
				t.Fatalf("应记录 reasoning_to_think_chunk，实际 %v", mods)
			}

			msg := firstMessage(t, out)
			if _, has := msg["reasoning_content"]; has {
				t.Error("还原后必须删掉顶层 reasoning_content，否则 Mistral 会 422")
			}
			parts := contentParts(t, msg)
			var texts []string
			think := 0
			for _, raw := range parts {
				m, ok := raw.(map[string]any)
				if !ok {
					t.Fatalf("content 块不是对象: %v", raw)
				}
				typ, _ := m["type"].(string)
				if typ == "thinking" {
					think++
					inner, _ := m["thinking"].([]any)
					if len(inner) != 1 {
						t.Fatalf("thinking 块应只有一个 text 子块，实际 %v", m["thinking"])
					}
					text, _ := inner[0].(map[string]any)
					texts = append(texts, text["text"].(string))
					if m["closed"] != true {
						t.Error("thinking 块必须 closed:true，Mistral 才认")
					}
					continue
				}
				texts = append(texts, m["text"].(string))
			}
			if think != tc.wantThink {
				t.Errorf("thinking 块数量 = %d，期望 %d", think, tc.wantThink)
			}
			if strings.Join(texts, "|") != strings.Join(tc.wantTexts, "|") {
				t.Errorf("content 块文本 = %v，期望 %v", texts, tc.wantTexts)
			}
		})
	}
}

// 还原只针对 assistant：其他角色的 reasoning_content 仍由严格裁剪删除，
// 不能把客户的字段塞进 content 数组。
func TestRewriteReplayOnlyTouchesAssistant(t *testing.T) {
	p := baseProvider()
	p.ReplayReasoningAsThinkChunk = true
	p.StripUnsupportedParams = true
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":"hi","reasoning_content":"不该还原"},
		{"role":"assistant","content":"ok","reasoning_content":""}]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if hasMod(mods, "reasoning_to_think_chunk") {
		t.Errorf("没有可还原的 assistant 思维链时不该记录改动，实际 %v", mods)
	}
	msgs := decode(t, out)["messages"].([]any)
	user := msgs[0].(map[string]any)
	if _, has := user["reasoning_content"]; has {
		t.Error("user 消息的 reasoning_content 应被严格裁剪删除")
	}
	if user["content"] != "hi" {
		t.Errorf("user 的 content 不该被改写，实际 %v", user["content"])
	}
	assistant := msgs[1].(map[string]any)
	if _, has := assistant["reasoning_content"]; has {
		t.Error("空 reasoning_content 也应删除")
	}
	if assistant["content"] != "ok" {
		t.Errorf("空思维链不该往 content 里插 thinking 块，实际 %v", assistant["content"])
	}
}

// 开关关着时必须保持旧行为：只剥离、不还原，避免给不支持 ThinkChunk 的网关
// 发出非标 content 数组。
func TestRewriteReplayRequiresSwitch(t *testing.T) {
	p := baseProvider()
	p.StripUnsupportedParams = true
	body := []byte(`{"model":"m","messages":[{"role":"assistant","content":"正文","reasoning_content":"先想"}]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if hasMod(mods, "reasoning_to_think_chunk") {
		t.Errorf("开关关闭时不该还原，实际 %v", mods)
	}
	msg := firstMessage(t, out)
	if _, has := msg["reasoning_content"]; has {
		t.Error("开关关闭时仍应剥离 reasoning_content")
	}
	if msg["content"] != "正文" {
		t.Errorf("开关关闭时 content 应原样保留，实际 %v", msg["content"])
	}
}

// prefilter 是按引号精确匹配的字节预筛。严格裁剪表里的类型名（如
// web_search_call）如果没有进预筛清单，整项裁剪会被静默跳过 —— 这个回归
// 让 Mistral 收到不支持的 input 项直接 400。
func TestPrefilterCoversStrictResponsesInputTypes(t *testing.T) {
	p := responsesProvider()
	p.StripUnsupportedParams = true
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"web_search_call","id":"ws1"},
		{"type":"computer_call","id":"cu1"},
		{"type":"mcp_list_tools","id":"mcp1"}]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	for _, want := range []string{"strip_input_web_search_call", "strip_input_computer_call", "strip_input_mcp_list_tools"} {
		if !hasMod(mods, want) {
			t.Fatalf("应记录 %s，实际 %v（预筛漏项会让整项裁剪被静默跳过）", want, mods)
		}
	}
	items := responsesInput(t, out)
	if len(items) != 1 {
		t.Fatalf("只应保留 message 项，实际 %d 项: %s", len(items), out)
	}
}

// prefilter 同理必须覆盖工具类型（含 namespace 内层之外的顶层内置工具）。
func TestPrefilterCoversStrictToolTypes(t *testing.T) {
	p := responsesProvider()
	p.StripUnsupportedParams = true
	body := []byte(`{"model":"m","input":"hi","tools":[
		{"type":"web_search"},
		{"type":"function","name":"f","parameters":{}}]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if !hasMod(mods, "strip_tool_web_search") {
		t.Fatalf("应记录工具裁剪，实际改动 %v: %s", mods, out)
	}
	tools, _ := decode(t, out)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("只应保留 function 工具，实际 %v", tools)
	}
	if typ, _ := tools[0].(map[string]any)["type"].(string); typ != "function" {
		t.Errorf("保留的工具类型错误: %v", tools[0])
	}
}

// /v1/responses 打到配置成 openai 的供应商时，不能注入 chat 专有的
// stream_options，否则上游会因未知字段整条 400。
func TestRewriteDoesNotInjectStreamOptionsIntoResponses(t *testing.T) {
	p := baseProvider() // APIFormat = openai
	body := []byte(`{"model":"m","stream":true,"input":[{"role":"user","content":"hi"}]}`)

	out, _ := rewriteRequestBodyOpts(p, model.DefaultSettings(), body, "m", rewriteOptions{
		clientPath: "/v1/responses",
		format:     model.FormatResponses,
	})
	obj := decode(t, out)
	if _, has := obj["stream_options"]; has {
		t.Errorf("Responses 请求不该出现 stream_options: %s", out)
	}
	// 顺带验证 Responses 的 input 规整同样按实际协议生效。
	items, _ := obj["input"].([]any)
	if len(items) != 1 {
		t.Fatalf("input 解析失败: %s", out)
	}
	if typ, _ := items[0].(map[string]any)["type"].(string); typ != "message" {
		t.Errorf("input 项应补上 type:message，实际 %v", items[0])
	}
}

// 实际协议由真正发往上游的路径决定，customPath 会接管客户端路径。
func TestEffectiveFormatFollowsActualEndpoint(t *testing.T) {
	cases := []struct {
		fallback model.APIFormat
		path     string
		want     model.APIFormat
	}{
		{model.FormatResponses, "/v1/chat/completions", model.FormatOpenAI},
		{model.FormatOpenAI, "/v1/responses", model.FormatResponses},
		{model.FormatOpenAI, "/v1/responses/", model.FormatResponses},
		{model.FormatGemini, "/v1beta/models/gemini:generateContent", model.FormatGemini},
		{model.FormatOpenAI, "/v1/embeddings", model.FormatOpenAI},
	}
	for _, c := range cases {
		if got := effectiveFormat(c.fallback, c.path); got != c.want {
			t.Errorf("effectiveFormat(%s, %q) = %s，期望 %s", c.fallback, c.path, got, c.want)
		}
	}
}

// ---------- 工具函数 ----------

func hasMod(mods []string, want string) bool {
	for _, m := range mods {
		if m == want || strings.HasPrefix(m, want) {
			return true
		}
	}
	return false
}

func firstMessage(t *testing.T, body []byte) map[string]any {
	t.Helper()
	msgs, ok := decode(t, body)["messages"].([]any)
	if !ok || len(msgs) == 0 {
		t.Fatalf("改写结果缺少 messages: %s", body)
	}
	msg, ok := msgs[0].(map[string]any)
	if !ok {
		t.Fatalf("messages[0] 不是对象: %s", body)
	}
	return msg
}

func contentParts(t *testing.T, msg map[string]any) []any {
	t.Helper()
	parts, ok := msg["content"].([]any)
	if !ok {
		t.Fatalf("content 应被改写为数组，实际 %v", msg["content"])
	}
	return parts
}
