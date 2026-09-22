package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"ai_proxy/internal/model"
)

// baseProvider 返回一个默认开启 usage 注入、未配置提示词的供应商。
func baseProvider() model.Provider {
	p := model.Provider{ID: 1, DisplayName: "测试", APIFormat: model.FormatOpenAI}
	p.ApplyDefaults()
	return p
}

func decode(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("改写结果不是合法 JSON: %v\n%s", err, body)
	}
	return m
}

func TestRewriteInjectsIncludeUsage(t *testing.T) {
	st := model.DefaultSettings() // UsageInjectDefault = true
	body := []byte(`{"model":"m","stream":true,"messages":[]}`)

	out, mods := rewriteRequestBody(baseProvider(), st, body)
	if len(mods) != 1 || mods[0] != "usage_inject" {
		t.Fatalf("应记录 usage_inject 改动，实际 %v", mods)
	}
	so, ok := decode(t, out)["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Errorf("未正确注入 stream_options.include_usage: %s", out)
	}
}

func TestRewritePreservesExistingStreamOptions(t *testing.T) {
	st := model.DefaultSettings()
	body := []byte(`{"model":"m","stream":true,"stream_options":{"other_flag":123}}`)

	out, _ := rewriteRequestBody(baseProvider(), st, body)
	so := decode(t, out)["stream_options"].(map[string]any)
	if so["include_usage"] != true {
		t.Error("应在保留原键的基础上补上 include_usage")
	}
	// 数字要按原值保留，不能被浮点化。
	if so["other_flag"] != float64(123) {
		t.Errorf("原有键被破坏: %v", so["other_flag"])
	}
}

func TestRewriteSkipsNonStreaming(t *testing.T) {
	st := model.DefaultSettings()
	body := []byte(`{"model":"m","stream":false,"messages":[]}`)

	out, mods := rewriteRequestBody(baseProvider(), st, body)
	if len(mods) != 0 {
		t.Errorf("非流式请求不该注入 usage 参数，实际 %v", mods)
	}
	if string(out) != string(body) {
		t.Error("未发生改写时应原样返回原始字节")
	}
}

func TestRewriteSkipsNonOpenAIFormats(t *testing.T) {
	st := model.DefaultSettings()
	body := []byte(`{"model":"m","stream":true,"messages":[]}`)

	for _, format := range []model.APIFormat{model.FormatAnthropic, model.FormatGemini, model.FormatResponses} {
		p := baseProvider()
		p.APIFormat = format
		out, mods := rewriteRequestBody(p, st, body)
		if len(mods) != 0 {
			t.Errorf("%s 不该注入 stream_options，实际 %v", format, mods)
		}
		if string(out) != string(body) {
			t.Errorf("%s 不该改动请求体", format)
		}
	}
}

func TestRewriteAlreadyEnabled(t *testing.T) {
	st := model.DefaultSettings()
	body := []byte(`{"model":"m","stream":true,"stream_options":{"include_usage":true}}`)

	_, mods := rewriteRequestBody(baseProvider(), st, body)
	// 客户端本来就开了，就不算我们改的，不该留下改写标记。
	if len(mods) != 0 {
		t.Errorf("已开启时不应记录改动，实际 %v", mods)
	}
}

func TestRewriteUsageInjectModeOverride(t *testing.T) {
	st := model.DefaultSettings()

	p := baseProvider()
	p.UsageInjectMode = "off"
	body := []byte(`{"model":"m","stream":true}`)
	_, mods := rewriteRequestBody(p, st, body)
	if len(mods) != 0 {
		t.Errorf("供应商级 off 应覆盖全局默认，实际 %v", mods)
	}

	// 全局关闭但供应商显式开启。
	st.UsageInjectDefault = false
	p = baseProvider()
	p.UsageInjectMode = "on"
	_, mods = rewriteRequestBody(p, st, body)
	if len(mods) != 1 {
		t.Errorf("供应商级 on 应覆盖全局默认，实际 %v", mods)
	}
}

// ---------- 提示词注入 ----------

func promptSettings(text, strategy string) model.Settings {
	st := model.DefaultSettings()
	st.UsageInjectDefault = false // 隔离变量，只观察提示词注入
	st.GlobalPrompt = model.PromptConfig{Enabled: true, Text: text, Strategy: strategy}
	return st
}

func TestPromptAppendOpenAIStringContent(t *testing.T) {
	st := promptSettings("全局提示", model.StrategyAppend)
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"原始提示"},{"role":"user","content":"hi"}]}`)

	out, mods := rewriteRequestBody(baseProvider(), st, body)
	if len(mods) != 1 || mods[0] != "prompt_append" {
		t.Fatalf("应记录 prompt_append，实际 %v", mods)
	}
	msgs := decode(t, out)["messages"].([]any)
	sys := msgs[0].(map[string]any)
	if sys["content"] != "原始提示\n\n全局提示" {
		t.Errorf("追加结果不符合预期: %v", sys["content"])
	}
	if len(msgs) != 2 {
		t.Errorf("消息条数不应变化，实际 %d", len(msgs))
	}
}

func TestPromptAppendOpenAIContentParts(t *testing.T) {
	// content 是分块数组时必须追加一个文本块，而不是整体替换。
	st := promptSettings("全局提示", model.StrategyAppend)
	body := []byte(`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"原始"}]}]}`)

	out, _ := rewriteRequestBody(baseProvider(), st, body)
	parts := decode(t, out)["messages"].([]any)[0].(map[string]any)["content"].([]any)
	if len(parts) != 2 {
		t.Fatalf("应在分块数组中追加一块，实际 %d 块", len(parts))
	}
	if parts[1].(map[string]any)["text"] != "全局提示" {
		t.Errorf("追加的文本块内容错误: %v", parts[1])
	}
}

func TestPromptAppendWithoutSystemMessage(t *testing.T) {
	st := promptSettings("全局提示", model.StrategyAppend)
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`)

	out, _ := rewriteRequestBody(baseProvider(), st, body)
	msgs := decode(t, out)["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("应插入一条 system 消息，实际 %d 条", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "system" || first["content"] != "全局提示" {
		t.Errorf("插入的 system 消息不正确: %v", first)
	}
}

func TestPromptReplace(t *testing.T) {
	st := promptSettings("新提示", model.StrategyReplace)
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"原始提示"},{"role":"user","content":"hi"}]}`)

	out, _ := rewriteRequestBody(baseProvider(), st, body)
	sys := decode(t, out)["messages"].([]any)[0].(map[string]any)
	if sys["content"] != "新提示" {
		t.Errorf("替换策略应完全覆盖原 system，实际 %v", sys["content"])
	}
}

func TestPromptPrependUser(t *testing.T) {
	st := promptSettings("前置说明", model.StrategyPrependUser)
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"原始提示"},{"role":"user","content":"hi"}]}`)

	out, _ := rewriteRequestBody(baseProvider(), st, body)
	msgs := decode(t, out)["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("应多出一条 user 消息，实际 %d 条", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "前置说明" {
		t.Errorf("前置消息不正确: %v", first)
	}
	// 原有的 system 消息应保持不动。
	if msgs[1].(map[string]any)["content"] != "原始提示" {
		t.Error("原有 system 消息不应被修改")
	}
}

func TestPromptAnthropicSystemField(t *testing.T) {
	p := baseProvider()
	p.APIFormat = model.FormatAnthropic
	p.PromptMode = model.ModeCustom
	p.Prompt = model.PromptConfig{Enabled: true, Text: "提示", Strategy: model.StrategyAppend}

	st := model.DefaultSettings()
	st.UsageInjectDefault = false

	// system 为字符串
	out, mods := rewriteRequestBody(p, st, []byte(`{"model":"m","system":"原有","messages":[]}`))
	if len(mods) != 1 {
		t.Fatalf("应发生注入，实际 %v", mods)
	}
	if decode(t, out)["system"] != "原有\n\n提示" {
		t.Errorf("Anthropic system 追加错误: %s", out)
	}

	// system 为 blocks 数组
	out, _ = rewriteRequestBody(p, st, []byte(`{"model":"m","system":[{"type":"text","text":"原有"}],"messages":[]}`))
	blocks := decode(t, out)["system"].([]any)
	if len(blocks) != 2 {
		t.Errorf("应在 blocks 数组末尾追加一块，实际 %d 块", len(blocks))
	}

	// system 缺失时新建
	out, _ = rewriteRequestBody(p, st, []byte(`{"model":"m","messages":[]}`))
	if decode(t, out)["system"] != "提示" {
		t.Errorf("system 缺失时应新建: %s", out)
	}
}

func TestPromptGeminiSystemInstruction(t *testing.T) {
	p := baseProvider()
	p.APIFormat = model.FormatGemini
	p.PromptMode = model.ModeCustom
	p.Prompt = model.PromptConfig{Enabled: true, Text: "提示", Strategy: model.StrategyAppend}

	st := model.DefaultSettings()
	st.UsageInjectDefault = false

	out, mods := rewriteRequestBody(p, st, []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	if len(mods) != 1 {
		t.Fatalf("应发生注入，实际 %v", mods)
	}
	si := decode(t, out)["systemInstruction"].(map[string]any)
	parts := si["parts"].([]any)
	if parts[0].(map[string]any)["text"] != "提示" {
		t.Errorf("Gemini systemInstruction 不正确: %s", out)
	}
}

func TestPromptRuleByModelRegex(t *testing.T) {
	p := baseProvider()
	p.APIFormat = model.FormatOpenAI
	p.PromptRules = []model.PromptRule{
		{ID: "1", Enabled: true, ModelPattern: `^gpt-4`, Text: "GPT4 专用", Strategy: model.StrategyAppend},
	}

	// 命中规则
	out, _ := rewriteRequestBody(p, model.DefaultSettings(), []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"x"}]}`))
	msgs := decode(t, out)["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "GPT4 专用" {
		t.Errorf("模型规则未生效: %s", out)
	}

	// 未命中规则且全局提示词未启用，则不改写
	_, mods := rewriteRequestBody(p, model.DefaultSettings(), []byte(`{"model":"claude-3","messages":[]}`))
	if len(mods) != 0 {
		t.Errorf("模型不匹配时不应注入，实际 %v", mods)
	}
}

func TestPromptInvalidRegexIsIgnored(t *testing.T) {
	p := baseProvider()
	p.PromptRules = []model.PromptRule{
		{ID: "1", Enabled: true, ModelPattern: `([`, Text: "坏规则", Strategy: model.StrategyAppend},
	}
	body := []byte(`{"model":"gpt-4o","messages":[]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body)
	// 非法正则不应该 panic，也不应该注入；此时应原样返回。
	if len(mods) != 0 {
		t.Errorf("非法正则规则应被跳过，实际 %v", mods)
	}
	if string(out) != string(body) {
		t.Error("非法正则导致请求体被改动")
	}
}

func TestPromptModeDisable(t *testing.T) {
	p := baseProvider()
	p.PromptMode = model.ModeDisable

	st := promptSettings("全局提示", model.StrategyAppend)
	body := []byte(`{"model":"m","messages":[]}`)

	out, mods := rewriteRequestBody(p, st, body)
	if len(mods) != 0 {
		t.Errorf("供应商级 disable 应阻止全局提示词注入，实际 %v", mods)
	}
	if string(out) != string(body) {
		t.Error("禁用状态下不应改动请求体")
	}
}

func TestRewriteMalformedBodyIsPassedThrough(t *testing.T) {
	st := model.DefaultSettings()     // usage 注入默认开启，会触发解析
	body := []byte(`{"stream":true,`) // 故意截断的 JSON

	out, mods := rewriteRequestBody(baseProvider(), st, body)
	if len(mods) != 0 {
		t.Errorf("解析失败时不应报告改写，实际 %v", mods)
	}
	if string(out) != string(body) {
		t.Error("解析失败时必须原样透传，绝不能破坏用户请求")
	}
}

func TestRewriteEmptyBody(t *testing.T) {
	out, mods := rewriteRequestBody(baseProvider(), model.DefaultSettings(), nil)
	if out != nil || mods != nil {
		t.Error("空请求体应直接返回")
	}
}

func TestExtractReasoningEffort(t *testing.T) {
	cases := []struct {
		format model.APIFormat
		body   string
		want   string
	}{
		{model.FormatOpenAI, `{"reasoning_effort":"high"}`, "high"},
		{model.FormatResponses, `{"reasoning":{"effort":"low"}}`, "low"},
		{model.FormatAnthropic, `{"thinking":{"type":"enabled","budget_tokens":4096}}`, "budget:4096"},
		{model.FormatGemini, `{"generationConfig":{"thinkingConfig":{"thinkingBudget":2048}}}`, "budget:2048"},
		{model.FormatOpenAI, `{"model":"m"}`, ""},
	}
	for _, c := range cases {
		if got := extractReasoningEffort(c.format, []byte(c.body)); got != c.want {
			t.Errorf("%s %s: 期望 %q，实际 %q", c.format, c.body, c.want, got)
		}
	}
}

// ---------- 上游地址拼接 ----------

func TestBuildUpstreamURL(t *testing.T) {
	cases := []struct {
		name       string
		baseURL    string
		customPath string
		path       string
		query      string
		want       string
	}{
		{"BaseURL 不含路径", "https://api.openai.com", "", "/v1/chat/completions", "", "https://api.openai.com/v1/chat/completions"},
		{"BaseURL 含 /v1 且请求路径重复", "https://api.openai.com/v1", "", "/v1/chat/completions", "", "https://api.openai.com/v1/chat/completions"},
		{"BaseURL 含结尾斜杠", "https://api.openai.com/v1/", "", "/v1/chat/completions", "", "https://api.openai.com/v1/chat/completions"},
		{"Anthropic", "https://api.anthropic.com", "", "/v1/messages", "", "https://api.anthropic.com/v1/messages"},
		{"Gemini v1beta 去重", "https://generativelanguage.googleapis.com/v1beta", "", "/v1beta/models/gemini-pro:generateContent", "", "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent"},
		{"Gemini 不带路径", "https://generativelanguage.googleapis.com", "", "/v1beta/models/gemini-pro:generateContent", "", "https://generativelanguage.googleapis.com/v1beta/models/gemini-pro:generateContent"},
		{"保留查询串", "https://api.openai.com/v1", "", "/v1/models", "limit=10", "https://api.openai.com/v1/models?limit=10"},
		{"自定义路径接管", "https://gw.example.com/v1", "/openai/chat", "/v1/chat/completions", "", "https://gw.example.com/v1/openai/chat"},
		{"裸 /v1 请求", "https://api.openai.com/v1", "", "/v1", "", "https://api.openai.com/v1"},
		{"带前缀的自建网关", "https://gw.example.com/api/openai/v1", "", "/v1/chat/completions", "", "https://gw.example.com/api/openai/v1/chat/completions"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p := model.Provider{DisplayName: "t", BaseURL: c.baseURL, CustomPath: c.customPath}
			got, err := buildUpstreamURL(p, c.path, c.query)
			if err != nil {
				t.Fatalf("拼接失败: %v", err)
			}
			if got != c.want {
				t.Errorf("期望 %s\n实际 %s", c.want, got)
			}
		})
	}
}

func TestBuildUpstreamURLRejectsInvalidBase(t *testing.T) {
	p := model.Provider{DisplayName: "t", BaseURL: "不是网址"}
	if _, err := buildUpstreamURL(p, "/v1/chat", ""); err == nil {
		t.Error("非法 BaseURL 应返回错误")
	}
}

// ---------- 请求头 ----------

func TestBuildUpstreamHeaders(t *testing.T) {
	p := baseProvider()
	p.AuthHeader = ""
	p.AuthPrefix = ""
	p.Keys = []model.APIKey{{ID: "k1", Key: "sk-secret", Enabled: true}}
	p.ExtraHeaders = map[string]string{"X-Custom": "v"}

	r := newTestRequest(t, "/v1/chat/completions", map[string]string{
		"Authorization":   "Bearer client-key",
		"Accept-Encoding": "gzip",
		"Connection":      "keep-alive, X-Dropped",
		"X-Dropped":       "should-be-removed",
		"X-Keep":          "yes",
		"Content-Type":    "application/json",
	})
	key, _ := firstProviderKey(p)
	got := buildUpstreamHeaders(r, p, model.APIKey{ID: "k1", Key: key}, "")

	if got.Get("Authorization") != "Bearer sk-secret" {
		t.Errorf("配置了密钥时应覆盖认证头，实际 %q", got.Get("Authorization"))
	}
	if got.Get("X-Keep") != "yes" {
		t.Error("普通业务头应被保留")
	}
	if got.Get("X-Dropped") != "" {
		t.Error("Connection 中列出的头应被剔除")
	}
	if got.Get("Accept-Encoding") != "" {
		t.Error("应剔除 Accept-Encoding，避免压缩后无法解析用量")
	}
	if got.Get("X-Custom") != "v" {
		t.Error("自定义请求头应被写入")
	}
	if got.Get("Connection") != "" || got.Get("Host") != "" {
		t.Error("逐跳头与 Host 不应出现在转发头里")
	}
}

func TestBuildUpstreamHeadersPassesThroughWhenNoKey(t *testing.T) {
	// 没配置密钥时，客户端自带的认证信息应当放行，
	// 这样「客户端持钥、代理只记账」的用法也能成立。
	p := baseProvider()
	r := newTestRequest(t, "/v1/chat/completions", map[string]string{"Authorization": "Bearer client-key"})

	got := buildUpstreamHeaders(r, p, model.APIKey{}, "")
	if got.Get("Authorization") != "Bearer client-key" {
		t.Errorf("无密钥配置时应透传客户端认证头，实际 %q", got.Get("Authorization"))
	}
}

func TestDefaultAuthForFormat(t *testing.T) {
	cases := map[model.APIFormat][2]string{
		model.FormatOpenAI:    {"Authorization", "Bearer "},
		model.FormatAnthropic: {"x-api-key", ""},
		model.FormatGemini:    {"x-goog-api-key", ""},
	}
	for format, want := range cases {
		h, prefix := model.DefaultAuthForFormat(format)
		if h != want[0] || prefix != want[1] {
			t.Errorf("%s 的默认认证头应为 %v，实际 (%s, %q)", format, want, h, prefix)
		}
	}
}

func TestMetrics(t *testing.T) {
	if got := computeTPS(100, 1000, 3000); got < 49.9 || got > 50.1 {
		t.Errorf("TPS 应为 50（100 token / 2 秒），实际 %v", got)
	}
	// 非流式场景没有首 token，分母退化为总耗时。
	if got := computeTPS(100, 0, 2000); got != 50 {
		t.Errorf("非流式 TPS 应为 50，实际 %v", got)
	}
	if got := computeTPS(0, 100, 200); got != 0 {
		t.Errorf("无输出 token 时 TPS 应为 0，实际 %v", got)
	}
}

func TestMaskSecret(t *testing.T) {
	got := maskSecret("Authorization", "Bearer sk-abcdefghijklmn")
	if !strings.HasPrefix(got, "Bearer ") {
		t.Errorf("应保留认证方式前缀，实际 %q", got)
	}
	if strings.Contains(got, "cdefghij") {
		t.Errorf("密钥中段应被遮蔽，实际 %q", got)
	}
	// 非敏感头保持原样。
	if got := maskSecret("Content-Type", "application/json"); got != "application/json" {
		t.Errorf("非敏感头不应被遮蔽，实际 %q", got)
	}
}

// TestBuildUpstreamHeadersStripsProxyAccessKey 覆盖一条凭据泄露路径：
// 客户端为了通过本代理的鉴权，常把代理访问密钥填进 API key 字段；
// 供应商没配密钥（「客户端持钥」模式）时，这个值会被原样转发给上游。
func TestBuildUpstreamHeadersStripsProxyAccessKey(t *testing.T) {
	const accessKey = "proxy-secret-key"

	p := baseProvider()
	// 供应商不配密钥，模拟「客户端持钥」模式 —— 此时认证头不会被覆盖。
	p.Keys = nil

	// 客户端用 Authorization 送代理密钥
	withBearer := newTestRequest(t, "/v1/chat/completions",
		map[string]string{"Authorization": "Bearer " + accessKey, "X-Keep": "yes"})
	got := buildUpstreamHeaders(withBearer, p, model.APIKey{}, accessKey)
	if v := got.Get("Authorization"); v != "" {
		t.Errorf("代理访问密钥不应转发给上游，实际 Authorization=%q", v)
	}
	if got.Get("X-Keep") != "yes" {
		t.Error("普通业务头不应被误删")
	}

	// 客户端用无前缀的自定义头送代理密钥
	bare := newTestRequest(t, "/v1/chat/completions",
		map[string]string{"X-Api-Key": accessKey, "Authorization": "Bearer upstream-real-key"})
	got = buildUpstreamHeaders(bare, p, model.APIKey{}, accessKey)
	if v := got.Get("X-Api-Key"); v != "" {
		t.Errorf("带访问密钥的自定义头也应被删掉，实际 %q", v)
	}
	// 与访问密钥无关的凭据要保留：那才是客户端真正想给上游的
	if v := got.Get("Authorization"); v != "Bearer upstream-real-key" {
		t.Errorf("上游自己的凭据不该被删，实际 %q", v)
	}

	// 没配访问密钥时不做任何删改
	plain := newTestRequest(t, "/v1/chat/completions",
		map[string]string{"Authorization": "Bearer " + accessKey})
	got = buildUpstreamHeaders(plain, p, model.APIKey{}, "")
	if v := got.Get("Authorization"); v != "Bearer "+accessKey {
		t.Errorf("未配置访问密钥时应原样透传，实际 %q", v)
	}
}
