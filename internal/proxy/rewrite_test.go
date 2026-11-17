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

	out, mods := rewriteRequestBody(baseProvider(), st, body, modelNameFromBody(body))
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

	out, _ := rewriteRequestBody(baseProvider(), st, body, modelNameFromBody(body))
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

	out, mods := rewriteRequestBody(baseProvider(), st, body, modelNameFromBody(body))
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
		out, mods := rewriteRequestBody(p, st, body, modelNameFromBody(body))
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

	_, mods := rewriteRequestBody(baseProvider(), st, body, modelNameFromBody(body))
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
	_, mods := rewriteRequestBody(p, st, body, modelNameFromBody(body))
	if len(mods) != 0 {
		t.Errorf("供应商级 off 应覆盖全局默认，实际 %v", mods)
	}

	// 全局关闭但供应商显式开启。
	st.UsageInjectDefault = false
	p = baseProvider()
	p.UsageInjectMode = "on"
	_, mods = rewriteRequestBody(p, st, body, modelNameFromBody(body))
	if len(mods) != 1 {
		t.Errorf("供应商级 on 应覆盖全局默认，实际 %v", mods)
	}
}

// ---------- 严格上游兼容：developer 角色与 thinking 字段 ----------

func TestRewriteNormalizesDeveloperRole(t *testing.T) {
	st := model.DefaultSettings()
	body := []byte(`{"model":"m","stream":false,"messages":[{"role":"developer","content":"sys"},{"role":"user","content":"hi"}]}`)

	out, mods := rewriteRequestBody(baseProvider(), st, body, modelNameFromBody(body))
	if len(mods) != 1 || mods[0] != "role_developer_to_system" {
		t.Fatalf("应记录 role_developer_to_system，实际 %v", mods)
	}
	msgs := decode(t, out)["messages"].([]any)
	if role := msgs[0].(map[string]any)["role"]; role != "system" {
		t.Errorf("developer 应改成 system，实际 %v", role)
	}
	if role := msgs[1].(map[string]any)["role"]; role != "user" {
		t.Errorf("其它角色不该被改动，实际 %v", role)
	}
}

func TestRewriteNormalizesDeveloperRoleInResponsesInput(t *testing.T) {
	p := baseProvider()
	p.APIFormat = model.FormatResponses
	body := []byte(`{"model":"m","input":[{"type":"message","role":"developer","content":"sys"},{"type":"message","role":"user","content":"hi"}]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 1 || mods[0] != "role_developer_to_system" {
		t.Fatalf("Responses 的 input 也应归一化，实际 %v", mods)
	}
	items := decode(t, out)["input"].([]any)
	if role := items[0].(map[string]any)["role"]; role != "system" {
		t.Errorf("developer 应改成 system，实际 %v", role)
	}
}

func TestRewriteLeavesAnthropicDeveloperRoleAlone(t *testing.T) {
	p := baseProvider()
	p.APIFormat = model.FormatAnthropic
	body := []byte(`{"model":"m","messages":[{"role":"developer","content":"sys"}]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 0 {
		t.Errorf("非 OpenAI 系格式不该动角色，实际 %v", mods)
	}
	if string(out) != string(body) {
		t.Error("未发生改写时应原样返回原始字节")
	}
}

func TestRewriteStripsUnsupportedParamsWhenEnabled(t *testing.T) {
	p := baseProvider()
	p.StripUnsupportedParams = true
	body := []byte(`{"model":"m","stream":false,"messages":[],"thinking":{"type":"enabled"},"enable_thinking":true,"reasoning_effort":"low"}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 2 || mods[0] != "strip_thinking" || mods[1] != "strip_enable_thinking" {
		t.Fatalf("应记录两个 strip 改动，实际 %v", mods)
	}
	m := decode(t, out)
	if _, ok := m["thinking"]; ok {
		t.Error("thinking 应被删除")
	}
	if _, ok := m["enable_thinking"]; ok {
		t.Error("enable_thinking 应被删除")
	}
	if m["reasoning_effort"] != "low" {
		t.Errorf("标准字段不该被牵连: %v", m["reasoning_effort"])
	}
}

func TestRewriteStripsStoreForChat(t *testing.T) {
	p := baseProvider()
	p.StripUnsupportedParams = true
	// Mistral 的 chat schema 里没有 store，传 false 也一样 422。
	body := []byte(`{"model":"m","stream":false,"messages":[],"store":false}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 1 || mods[0] != "strip_store" {
		t.Fatalf("chat 格式应裁掉 store，实际 %v", mods)
	}
	if _, ok := decode(t, out)["store"]; ok {
		t.Error("store 应被删除")
	}
}

func TestRewriteStripsStoreForResponsesWhenContinuationOff(t *testing.T) {
	p := baseProvider()
	p.APIFormat = model.FormatResponses
	p.StripUnsupportedParams = true
	// Mistral 的 Responses API 是无状态的：store 只能为 false，客户端传 true 直接 400。
	// 续传没开的时候，留着这个字段只会把请求打穿，所以一并裁掉。
	body := []byte(`{"model":"m","input":"hi","store":true}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 1 || mods[0] != "strip_store" {
		t.Fatalf("Responses 在续传关闭时应裁掉 store，实际 %v", mods)
	}
	if _, ok := decode(t, out)["store"]; ok {
		t.Error("store 应被删除")
	}
}

func TestRewriteStripsPreviousResponseIDForStatelessResponses(t *testing.T) {
	p := baseProvider()
	p.APIFormat = model.FormatResponses
	p.StripUnsupportedParams = true
	// 无状态上游引用不到上一轮响应，带着 previous_response_id 只会 400。
	body := []byte(`{"model":"m","input":"hi","previous_response_id":"resp_1"}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 1 || mods[0] != "strip_previous_response_id" {
		t.Fatalf("应裁掉 previous_response_id，实际 %v", mods)
	}
	if _, ok := decode(t, out)["previous_response_id"]; ok {
		t.Error("previous_response_id 应被删除")
	}
}

func TestRewriteKeepsStoreForResponsesWhenContinuationOn(t *testing.T) {
	p := baseProvider()
	p.APIFormat = model.FormatResponses
	p.StripUnsupportedParams = true
	p.ContinuationMode = model.ContinuationOn
	// 续传真的要用 store，这时不能跟着 chat 一起裁，否则功能直接失效。
	body := []byte(`{"model":"m","input":"hi","store":true}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 0 {
		t.Errorf("续传开启时不该裁 store，实际 %v", mods)
	}
	if string(out) != string(body) {
		t.Error("未发生改写时应原样返回原始字节")
	}
}

func TestRewriteStripsStoreWhenResponsesBodyOnChatProvider(t *testing.T) {
	p := baseProvider() // 供应商配的是 openai，客户端却打了 /v1/responses
	p.StripUnsupportedParams = true
	body := []byte(`{"model":"m","input":"hi","store":true}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 1 || mods[0] != "strip_store" {
		t.Fatalf("Responses 结构的 body 在续传关闭时也应裁 store，实际 %v", mods)
	}
	if _, ok := decode(t, out)["store"]; ok {
		t.Error("store 应被删除")
	}
}

func TestRewriteKeepsUnsupportedParamsByDefault(t *testing.T) {
	body := []byte(`{"model":"m","stream":false,"messages":[],"thinking":{"type":"enabled"}}`)

	out, mods := rewriteRequestBody(baseProvider(), model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 0 {
		t.Errorf("默认不该裁剪 thinking，实际 %v", mods)
	}
	if string(out) != string(body) {
		t.Error("未发生改写时应原样返回原始字节")
	}
}

func TestRewriteKeepsNestedThinkingChunks(t *testing.T) {
	p := baseProvider()
	p.StripUnsupportedParams = true
	// 消息内容里的 {"type":"thinking"} 是上游正常结构，只能删顶层字段。
	body := []byte(`{"model":"m","stream":false,"messages":[{"role":"user","content":[{"type":"thinking","thinking":"推理内容"}]}]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 0 {
		t.Errorf("嵌套结构不该被裁掉，实际 %v", mods)
	}
	if string(out) != string(body) {
		t.Error("嵌套 thinking 场景应原样返回")
	}
}

func TestRewriteStripsReasoningContentFromAssistantMessages(t *testing.T) {
	p := baseProvider()
	p.StripUnsupportedParams = true
	// 回放过思维链的真实形状：assistant 消息顶层多带一个 reasoning_content。
	// Mistral 的 messages schema 是 extra="forbid"，带着就整条 422。
	body := []byte(`{"model":"m","stream":false,"messages":[
		{"role":"system","content":"sys"},
		{"role":"assistant","content":"第一轮","reasoning_content":"想一想"},
		{"role":"user","content":"接着说"},
		{"role":"assistant","content":[{"type":"text","text":"第二轮"}],"reasoning_content":"再想想","tool_calls":[]}
	]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	// 两条 assistant 消息各带一个，日志里只该留一个标记。
	if len(mods) != 1 || mods[0] != "strip_reasoning_content" {
		t.Fatalf("应只记录一个 strip_reasoning_content，实际 %v", mods)
	}

	msgs := decode(t, out)["messages"].([]any)
	for i, raw := range msgs {
		m := raw.(map[string]any)
		if _, has := m["reasoning_content"]; has {
			t.Errorf("messages[%d] 的 reasoning_content 应被删除", i)
		}
	}
	// 正文一个字都不能动。
	if got := msgs[1].(map[string]any)["content"]; got != "第一轮" {
		t.Errorf("assistant 正文被改动: %v", got)
	}
	if got := msgs[3].(map[string]any)["content"].([]any)[0].(map[string]any)["text"]; got != "第二轮" {
		t.Errorf("分块正文被改动: %v", got)
	}
	if _, ok := msgs[3].(map[string]any)["tool_calls"]; !ok {
		t.Error("tool_calls 是标准字段，不该被牵连")
	}
}

func TestRewriteStripsReasoningDetailsAndReasoning(t *testing.T) {
	p := baseProvider()
	p.StripUnsupportedParams = true
	body := []byte(`{"model":"m","stream":false,"messages":[
		{"role":"assistant","content":"hi","reasoning_details":[{"type":"summary","text":"x"}],"reasoning":"想"}
	]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 2 {
		t.Fatalf("应记录两个 strip 改动，实际 %v", mods)
	}
	msg := decode(t, out)["messages"].([]any)[0].(map[string]any)
	for _, key := range []string{"reasoning_details", "reasoning"} {
		if _, has := msg[key]; has {
			t.Errorf("%s 应被删除", key)
		}
	}
}

func TestRewriteStripsExtraFieldsOnAnyRole(t *testing.T) {
	p := baseProvider()
	p.StripUnsupportedParams = true
	// 这些字段在任何角色上都不是合法字段（schema 里只有 role/content 等），
	// 一律裁掉；name 只有 tool 消息允许。
	body := []byte(`{"model":"m","stream":false,"messages":[
		{"role":"user","content":"hi","reasoning_content":"x","name":"alice"},
		{"role":"tool","content":"ok","tool_call_id":"c1","name":"search"}
	]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	want := map[string]bool{"strip_reasoning_content": true, "strip_name": true}
	if len(mods) != len(want) {
		t.Fatalf("应记录两个 strip 改动，实际 %v", mods)
	}
	for _, m := range mods {
		if !want[m] {
			t.Errorf("意外的改动标记: %s", m)
		}
	}

	msgs := decode(t, out)["messages"].([]any)
	user := msgs[0].(map[string]any)
	if _, ok := user["reasoning_content"]; ok {
		t.Error("user 消息的 reasoning_content 应被删除")
	}
	if _, ok := user["name"]; ok {
		t.Error("user 消息的 name 应被删除")
	}
	tool := msgs[1].(map[string]any)
	if tool["name"] != "search" {
		t.Errorf("tool 消息的 name 是合法字段，不该被删: %v", tool["name"])
	}
}

func TestRewriteStripsAssistantLegacyFields(t *testing.T) {
	p := baseProvider()
	p.StripUnsupportedParams = true
	// OpenAI 的历史扩展在 Mistral 的 AssistantMessage 里都不存在。
	body := []byte(`{"model":"m","stream":false,"messages":[
		{"role":"assistant","content":"hi","refusal":null,"annotations":[],"audio":null,"function_call":{"name":"f"},"name":"bot"}
	]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 5 {
		t.Fatalf("应记录五个 strip 改动，实际 %v", mods)
	}
	msg := decode(t, out)["messages"].([]any)[0].(map[string]any)
	for _, key := range []string{"refusal", "annotations", "audio", "function_call", "name"} {
		if _, ok := msg[key]; ok {
			t.Errorf("%s 应被删除", key)
		}
	}
	if msg["content"] != "hi" {
		t.Errorf("正文不该被改动: %v", msg["content"])
	}
}

func TestRewriteKeepsReasoningFieldsByDefault(t *testing.T) {
	// 开关关着时一律不动：有些网关靠回放的思维链维持多轮推理的连贯性。
	body := []byte(`{"model":"m","stream":false,"messages":[{"role":"assistant","content":"hi","reasoning_content":"x"}]}`)

	out, mods := rewriteRequestBody(baseProvider(), model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 0 {
		t.Errorf("默认不该裁剪 reasoning_content，实际 %v", mods)
	}
	if string(out) != string(body) {
		t.Error("未发生改写时应原样返回原始字节")
	}
}

func TestRewriteKeepsReasoningFieldsInResponsesInput(t *testing.T) {
	p := baseProvider()
	p.APIFormat = model.FormatResponses
	p.StripUnsupportedParams = true
	// Responses 的 input 里 reasoning 项是合法输入，不能跟着 chat 一起裁。
	body := []byte(`{"model":"m","input":[{"type":"reasoning","id":"r1","summary":[]},{"type":"message","role":"assistant","content":"hi"}]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 0 {
		t.Errorf("Responses 的 input 不该被裁，实际 %v", mods)
	}
	if string(out) != string(body) {
		t.Error("未发生改写时应原样返回原始字节")
	}
}

// ---------- prompt_cache_key ----------

func TestRewriteInjectsPromptCacheKey(t *testing.T) {
	p := baseProvider()
	p.PromptCacheKeyMode = model.PromptCacheKeyAuto
	body := []byte(`{"model":"m","stream":false,"messages":[
		{"role":"system","content":"你是助手"},
		{"role":"user","content":"你好"}
	]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 1 || mods[0] != "prompt_cache_key" {
		t.Fatalf("应记录 prompt_cache_key 改动，实际 %v", mods)
	}
	key := cacheKeyOf(t, out)
	if !strings.HasPrefix(key, promptCacheKeyPrefix) {
		t.Errorf("注入的 key 应带 %s 前缀，实际 %q", promptCacheKeyPrefix, key)
	}
}

func TestPromptCacheKeyStableAcrossTurns(t *testing.T) {
	p := baseProvider()
	p.PromptCacheKeyMode = model.PromptCacheKeyAuto

	first := []byte(`{"model":"m","stream":false,"messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"第一轮"}]}`)
	// 第二轮只是尾部追加，前缀不变 —— 官方文档要求的正是这种「同一会话前缀稳定」。
	second := []byte(`{"model":"m","stream":false,"messages":[{"role":"system","content":"你是助手"},{"role":"user","content":"第一轮"},{"role":"assistant","content":"好"},{"role":"user","content":"第二轮"}]}`)

	k1 := cacheKeyOf(t, mustRewrite(t, p, first))
	k2 := cacheKeyOf(t, mustRewrite(t, p, second))
	if k1 == "" || k1 != k2 {
		t.Errorf("同一会话的多轮请求应共用同一个 key，实际 %q / %q", k1, k2)
	}

	// 换了系统提示就是另一条缓存链。
	other := []byte(`{"model":"m","stream":false,"messages":[{"role":"system","content":"另一个助手"},{"role":"user","content":"第一轮"}]}`)
	if k3 := cacheKeyOf(t, mustRewrite(t, p, other)); k3 == k1 {
		t.Errorf("不同会话不该共用 key，实际都是 %q", k1)
	}
}

func TestPromptCacheKeyUsesSessionSeed(t *testing.T) {
	p := baseProvider()
	p.PromptCacheKeyMode = model.PromptCacheKeyAuto
	body := []byte(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`)

	mk := func(seed string) string {
		out, _ := rewriteRequestBodyOpts(p, model.DefaultSettings(), body, modelNameFromBody(body),
			rewriteOptions{cacheKeySeed: seed})
		return cacheKeyOf(t, out)
	}
	if mk("session-a") != mk("session-a") {
		t.Error("同一个会话 id 必须得到同一个 key")
	}
	if mk("session-a") == mk("session-b") {
		t.Error("不同会话 id 应得到不同的 key")
	}
	if mk("session-a") == mk("") {
		t.Error("带会话头的 key 不该与指纹兜底的 key 相同")
	}
}

func TestRewriteKeepsClientPromptCacheKey(t *testing.T) {
	p := baseProvider()
	p.PromptCacheKeyMode = model.PromptCacheKeyAuto
	body := []byte(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}],"prompt_cache_key":"client-key"}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 0 {
		t.Errorf("客户端自带的 key 不该被覆盖，实际 %v", mods)
	}
	if got := cacheKeyOf(t, out); got != "client-key" {
		t.Errorf("key 被改动了: %q", got)
	}
}

func TestRewritePromptCacheKeyOffByDefault(t *testing.T) {
	body := []byte(`{"model":"m","stream":false,"messages":[{"role":"user","content":"hi"}]}`)

	out, mods := rewriteRequestBody(baseProvider(), model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 0 {
		t.Errorf("默认不该注入 prompt_cache_key，实际 %v", mods)
	}
	if string(out) != string(body) {
		t.Error("未发生改写时应原样返回原始字节")
	}
}

func TestRewritePromptCacheKeyOnResponses(t *testing.T) {
	p := baseProvider()
	p.APIFormat = model.FormatResponses
	p.PromptCacheKeyMode = model.PromptCacheKeyAuto
	// Responses 的请求体顶层同样有 prompt_cache_key（内置 input.type 修正也要一起生效）。
	body := []byte(`{"model":"m","input":[{"role":"user","content":"hi"}]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if !containsMod(mods, "prompt_cache_key") {
		t.Fatalf("Responses 也应注入 key，实际 %v", mods)
	}
	if cacheKeyOf(t, out) == "" {
		t.Error("body 里没写进 key")
	}
}

// cacheKeyOf 取出改写结果里的 prompt_cache_key。
func cacheKeyOf(t *testing.T, body []byte) string {
	t.Helper()
	key, _ := decode(t, body)["prompt_cache_key"].(string)
	return key
}

func mustRewrite(t *testing.T, p model.Provider, body []byte) []byte {
	t.Helper()
	out, _ := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	return out
}

func containsMod(mods []string, want string) bool {
	for _, m := range mods {
		if m == want {
			return true
		}
	}
	return false
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

	out, mods := rewriteRequestBody(baseProvider(), st, body, modelNameFromBody(body))
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

	out, _ := rewriteRequestBody(baseProvider(), st, body, modelNameFromBody(body))
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

	out, _ := rewriteRequestBody(baseProvider(), st, body, modelNameFromBody(body))
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

	out, _ := rewriteRequestBody(baseProvider(), st, body, modelNameFromBody(body))
	sys := decode(t, out)["messages"].([]any)[0].(map[string]any)
	if sys["content"] != "新提示" {
		t.Errorf("替换策略应完全覆盖原 system，实际 %v", sys["content"])
	}
}

func TestPromptPrependUser(t *testing.T) {
	st := promptSettings("前置说明", model.StrategyPrependUser)
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"原始提示"},{"role":"user","content":"hi"}]}`)

	out, _ := rewriteRequestBody(baseProvider(), st, body, modelNameFromBody(body))
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
	out, mods := rewriteRequestBody(p, st, []byte(`{"model":"m","system":"原有","messages":[]}`), modelNameFromBody([]byte(`{"model":"m","system":"原有","messages":[]}`)))
	if len(mods) != 1 {
		t.Fatalf("应发生注入，实际 %v", mods)
	}
	if decode(t, out)["system"] != "原有\n\n提示" {
		t.Errorf("Anthropic system 追加错误: %s", out)
	}

	// system 为 blocks 数组
	out, _ = rewriteRequestBody(p, st, []byte(`{"model":"m","system":[{"type":"text","text":"原有"}],"messages":[]}`), modelNameFromBody([]byte(`{"model":"m","system":[{"type":"text","text":"原有"}],"messages":[]}`)))
	blocks := decode(t, out)["system"].([]any)
	if len(blocks) != 2 {
		t.Errorf("应在 blocks 数组末尾追加一块，实际 %d 块", len(blocks))
	}

	// system 缺失时新建
	out, _ = rewriteRequestBody(p, st, []byte(`{"model":"m","messages":[]}`), modelNameFromBody([]byte(`{"model":"m","messages":[]}`)))
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

	out, mods := rewriteRequestBody(p, st, []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`), modelNameFromBody([]byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)))
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
	out, _ := rewriteRequestBody(p, model.DefaultSettings(), []byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"x"}]}`), modelNameFromBody([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":"x"}]}`)))
	msgs := decode(t, out)["messages"].([]any)
	if msgs[0].(map[string]any)["content"] != "GPT4 专用" {
		t.Errorf("模型规则未生效: %s", out)
	}

	// 未命中规则且全局提示词未启用，则不改写
	_, mods := rewriteRequestBody(p, model.DefaultSettings(), []byte(`{"model":"claude-3","messages":[]}`), modelNameFromBody([]byte(`{"model":"claude-3","messages":[]}`)))
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

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
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

	out, mods := rewriteRequestBody(p, st, body, modelNameFromBody(body))
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

	out, mods := rewriteRequestBody(baseProvider(), st, body, modelNameFromBody(body))
	if len(mods) != 0 {
		t.Errorf("解析失败时不应报告改写，实际 %v", mods)
	}
	if string(out) != string(body) {
		t.Error("解析失败时必须原样透传，绝不能破坏用户请求")
	}
}

func TestRewriteEmptyBody(t *testing.T) {
	out, mods := rewriteRequestBody(baseProvider(), model.DefaultSettings(), nil, modelNameFromBody(nil))
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
	// 上游把整流攒齐后一次性下发时，生成窗口会塌成几毫秒：58 个 token 落在 3ms 里
	// 能算出 19000 t/s。这不是吐字速度，必须按不可测处理（见 minPlausibleMsPerToken）。
	if got := computeTPS(58, 6131, 6134); got != 0 {
		t.Errorf("生成窗口塌缩时应记为不可测，实际 %v", got)
	}
	// 首 token 与响应结束落在同一毫秒（整流下发的极端情形）同样不可测：
	// 退回用总耗时做分母会算出「延迟的倒数」，看着正常其实不是速度。
	if got := computeTPS(37, 306, 306); got != 0 {
		t.Errorf("首 token 与结束同毫秒时应记为不可测，实际 %v", got)
	}
	// 正好等于可信上限的速度仍然保留：护栏只拦「超过」的值。
	if got := computeTPS(500, 0, 1000); got != 500 {
		t.Errorf("等于上限的速度应保留，实际 %v", got)
	}
}

// 真实的高速流式不能因为「速度超过 500」就被当成整流下发。
//
// 2026-11-15 的库里这类行被单一速度阈值全部判成 0，最近 200 条里 166 条 TPS
// 显示「—」。判据必须是「速度快」且「每 token 耗时短到不可能是逐 token 生成」
// 两个条件同时成立，样本取自 data/ai_proxy.db 的真实记录。
func TestComputeTPSKeepsRealFastStreams(t *testing.T) {
	cases := []struct {
		name              string
		tokens            int
		ttftMs, totalMs   int64
		wantTPS           float64
		wantApproximately bool
	}{
		// 21980 个 token 在 32 秒里持续到达：684 t/s 是真实吐字速度。
		{"21980 token / 32s", 21980, 2498, 34634, 684, true},
		// 18845 个 token 用了 35.8 秒，526 t/s，仅略高于旧上限。
		{"18845 token / 35.8s", 18845, 9241, 45050, 526, true},
		// 大响应的速度稳定在 500~800 区间，是这批数据的主力形态。
		{"17915 token / 32.5s", 17915, 2678, 35205, 551, true},
		// 高速上游（≈1000~2000 t/s）的正常表现：每 token 0.5~1.0ms。
		{"658 token / 522ms", 658, 1000, 1522, 1261, true},
	}
	for _, c := range cases {
		got := computeTPS(c.tokens, c.ttftMs, c.totalMs)
		if got == 0 {
			t.Errorf("%s：真实高速流不应被判为不可测", c.name)
			continue
		}
		if c.wantApproximately && (got < c.wantTPS*0.99 || got > c.wantTPS*1.01) {
			t.Errorf("%s：TPS 应约为 %.0f，实际 %.1f", c.name, c.wantTPS, got)
		}
	}
}

// 整流下发的判据是「每 token 耗时短到不可能逐 token 生成」，与 token 数无关。
// 这些样本同样取自 data/ai_proxy.db，算出 5000 t/s 以上，物理上不成立。
func TestComputeTPSRejectsBufferedDumps(t *testing.T) {
	cases := []struct {
		name            string
		tokens          int
		ttftMs, totalMs int64
	}{
		// 46 个 token 落在 2ms 里 = 23000 t/s。
		{"46 token / 2ms", 46, 100, 102},
		// 8 个 token 落在 2ms 里。
		{"8 token / 2ms", 8, 500, 502},
		// 492 个 token 落在 41ms 里 = 12000 t/s。
		{"492 token / 41ms", 492, 3000, 3041},
		// 324 个 token 落在 37ms 里。
		{"324 token / 37ms", 324, 800, 837},
	}
	for _, c := range cases {
		if got := computeTPS(c.tokens, c.ttftMs, c.totalMs); got != 0 {
			t.Errorf("%s：整流下发应记为不可测，实际 %.1f t/s", c.name, got)
		}
	}
}

// 输出 token 太少时，生成窗口由网络往返与事件间隔主导，不是吐字速度。
//
// 库里 8 个 token 的响应窗口从 2ms 到 581ms 连续分布、毫无断层（对应 4000~13.8 t/s），
// 同一批响应 reasoning_tokens 全是 8，即只吐了思维链就结束。这类短响应的「速度」
// 在两个方向上都不具参考价值，必须按不可测处理，否则平均值会被网络抖动带偏。
func TestComputeTPSRequiresEnoughTokens(t *testing.T) {
	// 8 个 token 落在 12ms 里：按每 token 0.75ms 本可通过整流判据，但输出量太少。
	if got := computeTPS(8, 100, 112); got != 0 {
		t.Errorf("输出量不足 %d 时应记为不可测，实际 %.1f t/s", minTPSMeasureTokens, got)
	}
	// 同样 8 个 token、窗口拉到 581ms，算出 13.8 t/s 看着「正常」，但同样不可采信。
	if got := computeTPS(8, 100, 681); got != 0 {
		t.Errorf("输出量不足时无论速度高低都应记为不可测，实际 %.1f t/s", got)
	}
	// 刚好达到门槛就正常计算。
	if got := computeTPS(minTPSMeasureTokens, 0, 1000); got != float64(minTPSMeasureTokens) {
		t.Errorf("达到门槛时应正常计算，实际 %.1f t/s", got)
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

// ---------- responses input 规范化 ----------

func responsesProvider() model.Provider {
	p := baseProvider()
	p.APIFormat = model.FormatResponses
	return p
}

// responsesInput 取出改写后请求体的 input 数组。
func responsesInput(t *testing.T, out []byte) []any {
	t.Helper()
	items, ok := decode(t, out)["input"].([]any)
	if !ok {
		t.Fatalf("改写结果没有 input 数组: %s", out)
	}
	return items
}

func TestResponsesInputTypeAddedForRoleItems(t *testing.T) {
	// 这就是实际踩到的载荷：assistant 项缺 type，system 项也缺。
	body := []byte(`{"model":"m","input":[` +
		`{"role":"system","content":"你是助手"},` +
		`{"role":"user","content":[{"type":"input_text","text":"看图"}]},` +
		`{"role":"assistant","content":[{"type":"output_text","text":"我这就读图"}]},` +
		`{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"ok"}]}`)

	out, mods := rewriteRequestBody(responsesProvider(), model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 1 || mods[0] != "responses_input_type" {
		t.Fatalf("应记录 responses_input_type 改动，实际 %v", mods)
	}

	items := responsesInput(t, out)
	for i := range 3 {
		item := items[i].(map[string]any)
		if item["type"] != "message" {
			t.Errorf("input[%d] 应补上 type=message，实际 %v", i, item["type"])
		}
	}
	// 内容片段与其他字段一律不动。
	if items[1].(map[string]any)["content"].([]any)[0].(map[string]any)["type"] != "input_text" {
		t.Error("内容片段不该被改动")
	}
	if items[2].(map[string]any)["content"].([]any)[0].(map[string]any)["type"] != "output_text" {
		t.Error("assistant 项的内容片段应保持 output_text")
	}
	// 本来就带 type 的项不该被覆盖。
	if items[3].(map[string]any)["type"] != "function_call" || items[4].(map[string]any)["type"] != "function_call_output" {
		t.Error("已有 type 的项被改动了")
	}
}

func TestResponsesInputTypeAlreadyPresentIsNoop(t *testing.T) {
	body := []byte(`{"model":"m","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"function_call","call_id":"c1","name":"read","arguments":"{}"}]}`)

	out, mods := rewriteRequestBody(responsesProvider(), model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 0 {
		t.Errorf("客户端已写对时不该记录改动，实际 %v", mods)
	}
	if string(out) != string(body) {
		t.Error("未发生改写时应原样返回原始字节")
	}
}

func TestResponsesInputSkipsUnclassifiableItems(t *testing.T) {
	// input 允许整体是字符串；项也可能既没有 type 也没有 role。
	plain := []byte(`{"model":"m","input":"hi"}`)
	out, mods := rewriteRequestBody(responsesProvider(), model.DefaultSettings(), plain, modelNameFromBody(plain))
	if len(mods) != 0 || string(out) != string(plain) {
		t.Errorf("input 为字符串时不该改动，mods=%v body=%s", mods, out)
	}

	mixed := []byte(`{"model":"m","input":[{"foo":"bar"}]}`)
	out, mods = rewriteRequestBody(responsesProvider(), model.DefaultSettings(), mixed, modelNameFromBody(mixed))
	if len(mods) != 0 {
		t.Errorf("既无 type 也无 role 的项不该猜着改，实际 %v", mods)
	}
	if item := responsesInput(t, out)[0].(map[string]any); item["type"] != nil {
		t.Errorf("不该给无从判断的项补 type，实际 %v", item["type"])
	}
}

func TestResponsesInputMalformedBodyIsPassedThrough(t *testing.T) {
	body := []byte(`{"model":"m","input":[`)
	out, mods := rewriteRequestBody(responsesProvider(), model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 0 {
		t.Errorf("坏 JSON 不该记录改动，实际 %v", mods)
	}
	if string(out) != string(body) {
		t.Error("坏 JSON 应原样透传")
	}
}

func TestResponsesInputFixAlsoCoversInjectedItem(t *testing.T) {
	// 前置提示词注入出来的 user 项同样会被补齐，最终 body 一律合规。
	st := promptSettings("前置说明", model.StrategyPrependUser)
	body := []byte(`{"model":"m","input":[{"role":"user","content":"hi"}]}`)

	out, mods := rewriteRequestBody(responsesProvider(), st, body, modelNameFromBody(body))
	if len(mods) != 2 || mods[0] != "prompt_prepend_user" || mods[1] != "responses_input_type" {
		t.Fatalf("应同时记录提示词与规范化两项改动，实际 %v", mods)
	}
	items := responsesInput(t, out)
	if len(items) != 2 {
		t.Fatalf("应多出一条前置 user 项，实际 %d 条", len(items))
	}
	for i, raw := range items {
		if item := raw.(map[string]any); item["type"] != "message" {
			t.Errorf("input[%d] 应补上 type=message，实际 %v", i, item["type"])
		}
	}
	if items[0].(map[string]any)["content"] != "前置说明" {
		t.Errorf("前置项的 content 不正确: %v", items[0])
	}
}

func TestResponsesInputFixIsResponsesOnly(t *testing.T) {
	// 其他格式的 body 结构完全不同，绝不能顺手改。
	body := []byte(`{"model":"m","messages":[{"role":"assistant","content":"hi"}]}`)
	for _, format := range []model.APIFormat{model.FormatOpenAI, model.FormatAnthropic, model.FormatGemini} {
		p := baseProvider()
		p.APIFormat = format
		out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
		if len(mods) != 0 {
			t.Errorf("%s 不该被 responses 规范化碰到，实际 %v", format, mods)
		}
		if string(out) != string(body) {
			t.Errorf("%s 的请求体不该被改动", format)
		}
	}
}

// ---------- 模型映射 ----------

// mappedProvider 返回一个配了模型映射的供应商。注意关掉 usage 注入，
// 免得 stream_options 也留下改写标记，把断言搅浑。
func mappedProvider(format model.APIFormat, from, to string) model.Provider {
	p := model.Provider{ID: 1, DisplayName: "映射测试", APIFormat: format}
	p.UsageInjectMode = "off"
	p.ModelMap = []model.ModelMapping{{From: from, To: to}}
	p.ApplyDefaults()
	return p
}

func TestRewriteAppliesModelMapping(t *testing.T) {
	st := model.DefaultSettings()
	body := []byte(`{"model":"claude-3-5-sonnet","messages":[]}`)
	p := mappedProvider(model.FormatOpenAI, "claude-3-5-sonnet", "anthropic/claude-3.5")

	out, mods := rewriteRequestBody(p, st, body, modelNameFromBody(body))
	if len(mods) != 1 || mods[0] != "model_map" {
		t.Fatalf("应记录 model_map 改动，实际 %v", mods)
	}
	if got := decode(t, out)["model"]; got != "anthropic/claude-3.5" {
		t.Errorf("model 应被改写成目标模型，实际 %v", got)
	}
}

func TestRewriteModelMappingNoMatchPassesThrough(t *testing.T) {
	st := model.DefaultSettings()
	body := []byte(`{"model":"gpt-4o","messages":[]}`)
	// 只映射 claude，请求的是 gpt-4o
	p := mappedProvider(model.FormatOpenAI, "claude-3-5-sonnet", "anthropic/claude-3.5")

	out, mods := rewriteRequestBody(p, st, body, modelNameFromBody(body))
	if len(mods) != 0 {
		t.Errorf("未命中映射时不该有任何改写，实际 %v", mods)
	}
	if string(out) != string(body) {
		t.Error("未命中映射必须原样透传字节")
	}
}

func TestRewriteModelMappingAcrossFormats(t *testing.T) {
	st := model.DefaultSettings()
	// 请求体里带 model 的几种格式都应被改写（Gemini 的 model 在 URL 上，不在此列）。
	for _, tc := range []struct {
		format model.APIFormat
		body   string
	}{
		{model.FormatOpenAI, `{"model":"req-model","messages":[]}`},
		{model.FormatResponses, `{"model":"req-model","input":"hi"}`},
		{model.FormatAnthropic, `{"model":"req-model","messages":[]}`},
	} {
		p := mappedProvider(tc.format, "req-model", "up-model")
		out, mods := rewriteRequestBody(p, st, []byte(tc.body), "req-model")
		found := false
		for _, m := range mods {
			if m == "model_map" {
				found = true
			}
		}
		if !found {
			t.Errorf("%s 应记录 model_map 改动，实际 %v", tc.format, mods)
			continue
		}
		if got := decode(t, out)["model"]; got != "up-model" {
			t.Errorf("%s 的 model 应被改写，实际 %v", tc.format, got)
		}
	}
}

// 没有 model 字段的请求（例如拉模型列表）不该被无中生有地塞一个 model 进去。
func TestRewriteModelMappingSkipsBodyWithoutModel(t *testing.T) {
	st := model.DefaultSettings()
	body := []byte(`{"query":"hi"}`)
	p := mappedProvider(model.FormatOpenAI, "req-model", "up-model")

	out, mods := rewriteRequestBody(p, st, body, "req-model")
	for _, m := range mods {
		if m == "model_map" {
			t.Fatal("请求体没有 model 字段时不该报告模型映射")
		}
	}
	if got := decode(t, out)["model"]; got != nil {
		t.Errorf("不该给请求体塞入 model 字段，实际 %v", got)
	}
}

// 提示词规则可以按「上游真名」写：客户端名没命中时，用目标模型名再匹配一次。
func TestRewritePromptRuleMatchesMappedModelName(t *testing.T) {
	st := model.DefaultSettings()
	st.GlobalPrompt.Enabled = false
	body := []byte(`{"model":"req-model","messages":[{"role":"user","content":"hi"}]}`)
	p := mappedProvider(model.FormatOpenAI, "req-model", "up-model")
	p.PromptMode = model.ModeCustom
	p.PromptRules = []model.PromptRule{{
		ID: "r1", Enabled: true, ModelPattern: "^up-model$", Text: "上游模型专用提示", Strategy: model.StrategyAppend,
	}}

	out, mods := rewriteRequestBody(p, st, body, "req-model")
	var sawMap, sawPrompt bool
	for _, m := range mods {
		if m == "model_map" {
			sawMap = true
		}
		if m == "prompt_append" {
			sawPrompt = true
		}
	}
	if !sawMap || !sawPrompt {
		t.Fatalf("应同时命中映射与按目标模型名的提示词规则，实际 %v", mods)
	}
	obj := decode(t, out)
	if obj["model"] != "up-model" {
		t.Errorf("model 应被改写，实际 %v", obj["model"])
	}
	msgs := obj["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("应追加一条 system 消息，实际 %v", msgs)
	}
}

// ---------- 严格上游的工具与输入项裁剪 ----------

// strictResponsesProvider 是开了「剔除非标准字段」的 Responses 供应商。
func strictResponsesProvider() model.Provider {
	p := responsesProvider()
	p.StripUnsupportedParams = true
	return p
}

// 真实踩到的载荷：Codex 带 web_search 内置工具，Mistral 整条 400
// （code 3051，tools: Tool types ['web_search'] are not supported）。
func TestStripStrictToolsDropsBuiltinTools(t *testing.T) {
	p := strictResponsesProvider()
	body := []byte(`{"model":"m","input":"hi","tool_choice":"auto","tools":[` +
		`{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{}}},` +
		`{"type":"namespace","name":"multi_agent_v1","description":"sub","tools":[` +
		`{"type":"function","name":"close_agent","parameters":{"type":"object","properties":{}}}]},` +
		`{"type":"web_search","external_web_access":false}]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 1 || mods[0] != "strip_tool_web_search" {
		t.Fatalf("应只记录 strip_tool_web_search，实际 %v", mods)
	}

	tools := decode(t, out)["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("函数工具与 namespace 都该保留，实际 %d 项: %s", len(tools), out)
	}
	if got := tools[0].(map[string]any)["name"]; got != "exec_command" {
		t.Errorf("函数工具被误删: %v", got)
	}
	ns := tools[1].(map[string]any)
	if ns["type"] != "namespace" || len(ns["tools"].([]any)) != 1 {
		t.Errorf("namespace 分组应原样保留: %v", ns)
	}
}

func TestStripStrictToolsPrunesNamespaceInner(t *testing.T) {
	p := strictResponsesProvider()
	// 内层混了内置工具：只删那一项，分组留下来。
	body := []byte(`{"model":"m","input":"hi","tools":[` +
		`{"type":"namespace","name":"ns","description":"d","tools":[` +
		`{"type":"function","name":"keep","parameters":{"type":"object","properties":{}}},` +
		`{"type":"web_search"}]}]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 1 || mods[0] != "strip_tool_web_search" {
		t.Fatalf("应记录内层 web_search 被删，实际 %v", mods)
	}
	inner := decode(t, out)["tools"].([]any)[0].(map[string]any)["tools"].([]any)
	if len(inner) != 1 || inner[0].(map[string]any)["name"] != "keep" {
		t.Errorf("内层应只剩函数工具，实际 %v", inner)
	}
}

func TestStripStrictToolsRemovesEmptiedNamespace(t *testing.T) {
	p := strictResponsesProvider()
	// 内层全是内置工具：分组删空后整组丢弃，否则上游会报
	// tools.N.FunctionTool.type 错误。
	body := []byte(`{"model":"m","input":"hi","tools":[` +
		`{"type":"namespace","name":"ns","description":"d","tools":[{"type":"web_search"}]},` +
		`{"type":"function","name":"f","parameters":{"type":"object","properties":{}}}]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 1 || mods[0] != "strip_tool_web_search" {
		t.Fatalf("应记录 strip_tool_web_search，实际 %v", mods)
	}
	tools := decode(t, out)["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != "f" {
		t.Errorf("空 namespace 应被整组删除，实际 %v", tools)
	}
}

func TestStripStrictToolsRemovesToolsKeyWhenEmpty(t *testing.T) {
	p := strictResponsesProvider()
	body := []byte(`{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 1 || mods[0] != "strip_tool_web_search" {
		t.Fatalf("应记录 strip_tool_web_search，实际 %v", mods)
	}
	if _, ok := decode(t, out)["tools"]; ok {
		t.Errorf("工具被删空后不应留下空数组: %s", out)
	}
}

func TestStripStrictToolsResetsToolChoice(t *testing.T) {
	p := strictResponsesProvider()
	cases := []struct {
		name  string
		body  string
		check func(t *testing.T, obj map[string]any)
	}{
		{
			// required 要求至少一个工具，裁空之后必须回落 auto。
			name: "required 无工具",
			body: `{"model":"m","input":"hi","tools":[{"type":"web_search"}],"tool_choice":"required"}`,
			check: func(t *testing.T, obj map[string]any) {
				if obj["tool_choice"] != "auto" {
					t.Errorf("tool_choice 应回落 auto，实际 %v", obj["tool_choice"])
				}
			},
		},
		{
			// 点名的函数被裁掉了，保留点名只会 400。
			name: "点名的函数不存在",
			body: `{"model":"m","input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{}}}],` +
				`"tool_choice":{"type":"function","name":"gone"}}`,
			check: func(t *testing.T, obj map[string]any) {
				if obj["tool_choice"] != "auto" {
					t.Errorf("tool_choice 应回落 auto，实际 %v", obj["tool_choice"])
				}
			},
		},
		{
			// 点名的函数还在，不能动。
			name: "点名的函数存在",
			body: `{"model":"m","input":"hi","tools":[{"type":"function","name":"f","parameters":{"type":"object","properties":{}}}],` +
				`"tool_choice":{"type":"function","name":"f"}}`,
			check: func(t *testing.T, obj map[string]any) {
				choice, _ := obj["tool_choice"].(map[string]any)
				if choice["name"] != "f" {
					t.Errorf("合法 tool_choice 不该被改动: %v", obj["tool_choice"])
				}
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out, _ := rewriteRequestBody(p, model.DefaultSettings(), []byte(c.body), modelNameFromBody([]byte(c.body)))
			c.check(t, decode(t, out))
		})
	}
}

func TestStripStrictResponsesInputDropsUnsupportedItems(t *testing.T) {
	p := strictResponsesProvider()
	body := []byte(`{"model":"m","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"web_search_call","id":"ws1","status":"completed"},` +
		`{"type":"reasoning","id":"rs1","summary":[]},` +
		`{"type":"function_call","call_id":"c1","name":"f","arguments":"{}"},` +
		`{"type":"function_call_output","call_id":"c1","output":"ok"},` +
		`{"type":"item_reference","id":"msg_1"}]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	want := map[string]bool{"strip_input_web_search_call": true, "strip_input_item_reference": true}
	if len(mods) != len(want) {
		t.Fatalf("应记录两项输入裁剪，实际 %v", mods)
	}
	for _, m := range mods {
		if !want[m] {
			t.Errorf("意外的改动标记: %s", m)
		}
	}

	items := responsesInput(t, out)
	if len(items) != 4 {
		t.Fatalf("应剩 4 项，实际 %d: %s", len(items), out)
	}
	for _, raw := range items {
		typ, _ := raw.(map[string]any)["type"].(string)
		if typ == "web_search_call" || typ == "item_reference" {
			t.Errorf("不支持的输入项没删干净: %v", raw)
		}
	}
}

func TestStripReasoningEncryptedContent(t *testing.T) {
	p := strictResponsesProvider()
	// Codex 会回放上一轮的密文，严格上游直接拒：
	// "Reasoning items with encrypted_content are not supported"。
	body := []byte(`{"model":"m","input":[` +
		`{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},` +
		`{"type":"reasoning","id":"rs1","summary":[],"encrypted_content":"gAAAA-secret"},` +
		`{"type":"reasoning","id":"rs2","summary":[],"encrypted_content":null}]}`)

	out, mods := rewriteRequestBody(p, model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 1 || mods[0] != "strip_reasoning_encrypted" {
		t.Fatalf("应记录 strip_reasoning_encrypted，实际 %v", mods)
	}

	items := responsesInput(t, out)
	if _, has := items[1].(map[string]any)["encrypted_content"]; has {
		t.Error("有值的 encrypted_content 应被删除")
	}
	// null 本来就能被接受，不必改动，也不该被记成改动。
	if v, has := items[2].(map[string]any)["encrypted_content"]; !has || v != nil {
		t.Errorf("null 的 encrypted_content 应原样保留，实际 %v", items[2])
	}
}

func TestStrictToolStripDisabledByDefault(t *testing.T) {
	// 开关关着时一律不动：认 web_search 的上游（OpenAI 官方）不能被误伤。
	body := []byte(`{"model":"m","input":"hi","tools":[{"type":"web_search"}]}`)

	out, mods := rewriteRequestBody(responsesProvider(), model.DefaultSettings(), body, modelNameFromBody(body))
	if len(mods) != 0 {
		t.Errorf("默认不该裁剪工具，实际 %v", mods)
	}
	if string(out) != string(body) {
		t.Error("未发生改写时应原样返回原始字节")
	}
}

// MapModel 的行为：只做精确匹配，去空白，未命中返回原值。
func TestProviderMapModel(t *testing.T) {
	p := model.Provider{ModelMap: []model.ModelMapping{
		{From: "", To: "ignored"},      // 空 from 无效
		{From: "a", To: ""},            // 空 to 无效
		{From: " claude ", To: " c3 "}, // 两侧去空白
		{From: "claude", To: "second"}, // 不会命中：上一条已经匹配
		{From: "exact", To: "target"},
	}}
	cases := []struct {
		in     string
		want   string
		mapped bool
	}{
		{"claude", "c3", true},
		{"  claude  ", "c3", true},
		{"exact", "target", true},
		{"claudex", "claudex", false}, // 不做前缀匹配
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := p.MapModel(c.in)
		if got != c.want || ok != c.mapped {
			t.Errorf("MapModel(%q) = (%q,%v)，期望 (%q,%v)", c.in, got, ok, c.want, c.mapped)
		}
	}
}
