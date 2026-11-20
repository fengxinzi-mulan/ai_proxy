package proxy

import (
	"strings"
	"testing"

	"ai_proxy/internal/model"
)

// decodeObject 解析测试用的 JSON 文本，失败直接结束用例。
func decodeObject(t *testing.T, body string) map[string]any {
	t.Helper()
	obj, err := decodeJSONObject([]byte(body))
	if err != nil {
		t.Fatalf("测试体不是合法 JSON: %v\n%s", err, body)
	}
	return obj
}

// ---------- 错误描述提取 ----------

func TestExtractErrorFromOpenAIErrorObject(t *testing.T) {
	obj := decodeObject(t, `{"error":{"message":"Incorrect API key provided","type":"invalid_request_error"}}`)
	if got := extractError(obj); got != "Incorrect API key provided" {
		t.Errorf("应取 error.message，实际 %q", got)
	}
}

func TestExtractErrorFromPydanticDetailArray(t *testing.T) {
	// Mistral（Pydantic extra="forbid"）的 422 形状。以前只认字符串 detail，
	// 这类响应在日志里就是一条没有原因的红色记录。
	obj := decodeObject(t, `{"detail":[
		{"type":"extra_forbidden","loc":["body","messages",5,"assistant","reasoning_content"],
		 "msg":"Extra inputs are not permitted","input":"这是被回放的思维链"}
	]}`)
	want := "body.messages[5].assistant.reasoning_content: Extra inputs are not permitted"
	if got := extractError(obj); got != want {
		t.Errorf("解析结果不对\n实际 %q\n期望 %q", got, want)
	}
}

func TestExtractErrorFromPydanticDetailTruncates(t *testing.T) {
	obj := decodeObject(t, `{"detail":[
		{"loc":["body","messages",1,"a"],"msg":"m1"},
		{"loc":["body","messages",2,"b"],"msg":"m2"},
		{"loc":["body","messages",3,"c"],"msg":"m3"},
		{"loc":["body","messages",4,"d"],"msg":"m4"},
		{"loc":["body","messages",5,"e"],"msg":"m5"}
	]}`)
	got := extractError(obj)
	if !strings.Contains(got, "m3") {
		t.Errorf("应展开前 %d 条，实际 %q", maxValidationDetails, got)
	}
	if strings.Contains(got, "m4") || strings.Contains(got, "m5") {
		t.Errorf("超出的条目应折叠，实际 %q", got)
	}
	if !strings.Contains(got, "等 2 项") {
		t.Errorf("应提示剩余条数，实际 %q", got)
	}
}

func TestExtractErrorFromDetailStringArray(t *testing.T) {
	obj := decodeObject(t, `{"detail":["字段 store 不允许为 true","字段 input 缺失"]}`)
	got := extractError(obj)
	if !strings.Contains(got, "字段 store 不允许为 true") || !strings.Contains(got, "字段 input 缺失") {
		t.Errorf("字符串数组形状也应可读，实际 %q", got)
	}
}

func TestExtractErrorFromStringDetail(t *testing.T) {
	obj := decodeObject(t, `{"detail":"Invalid API key"}`)
	if got := extractError(obj); got != "Invalid API key" {
		t.Errorf("字符串 detail 应原样返回，实际 %q", got)
	}
}

func TestExtractErrorEmptyWhenNoErrorFields(t *testing.T) {
	obj := decodeObject(t, `{"model":"m","choices":[]}`)
	if got := extractError(obj); got != "" {
		t.Errorf("正常响应不该被识别成错误，实际 %q", got)
	}
}

// ---------- JSON 解析 ----------

// JSON 的 null 会"成功"解成 nil map，调用方拿到它再读键就是 panic。
// 这种情况必须退化成解析错误。
func TestDecodeJSONObjectRejectsNull(t *testing.T) {
	for _, body := range []string{"null", "null\n", " null "} {
		obj, err := decodeJSONObject([]byte(body))
		if err == nil {
			t.Errorf("decodeJSONObject(%q) 应返回错误，实际得到 %#v", body, obj)
		}
		if obj != nil {
			t.Errorf("decodeJSONObject(%q) 不该返回 nil map 之外的取值: %#v", body, obj)
		}
	}
}

// 空对象是合法的，不能被当成 null 拒掉。
func TestDecodeJSONObjectAcceptsEmptyObject(t *testing.T) {
	obj, err := decodeJSONObject([]byte(`{}`))
	if err != nil {
		t.Fatalf("空对象应解析成功: %v", err)
	}
	if obj == nil || len(obj) != 0 {
		t.Errorf("应得到空 map，实际 %#v", obj)
	}
}

// ---------- Gemini 用量口径 ----------

// Gemini 的 candidatesTokenCount 不含思考 token。CompletionTokens 是
// 「输出含思维链」的统一口径，必须把 thoughtsTokenCount 加回来 ——
// 否则 normalize 会把 ReasoningTokens 收敛到 CompletionTokens 以内，
// 思考 token 被静默丢掉，TPS 算得偏低。
func TestExtractUsageGeminiAddsThoughtsToCompletion(t *testing.T) {
	obj := decodeObject(t, `{"usageMetadata":{"promptTokenCount":100,
		"candidatesTokenCount":40,"thoughtsTokenCount":60,
		"totalTokenCount":200,"cachedContentTokenCount":20}}`)

	u := extractUsageFromObject(model.FormatGemini, nil, obj)
	if !u.Found {
		t.Fatal("应识别出用量")
	}
	if u.PromptTokens != 100 {
		t.Errorf("输入应为 100，实际 %d", u.PromptTokens)
	}
	if u.CompletionTokens != 100 {
		t.Errorf("输出应含思考 token（40+60=100），实际 %d", u.CompletionTokens)
	}
	if u.ReasoningTokens != 60 {
		t.Errorf("思考 token 应为 60，实际 %d", u.ReasoningTokens)
	}
	if u.TotalTokens != 200 {
		t.Errorf("total 应沿用上游的 200，实际 %d", u.TotalTokens)
	}
}

// 没有思考 token 时行为不变。
func TestExtractUsageGeminiWithoutThoughts(t *testing.T) {
	obj := decodeObject(t, `{"usageMetadata":{"promptTokenCount":8,"candidatesTokenCount":3,"totalTokenCount":11}}`)
	u := extractUsageFromObject(model.FormatGemini, nil, obj)
	if u.CompletionTokens != 3 || u.ReasoningTokens != 0 {
		t.Errorf("无思考 token 时输出应为 3、思考应为 0，实际 %+v", u)
	}
}
