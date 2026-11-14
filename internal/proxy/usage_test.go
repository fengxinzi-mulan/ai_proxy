package proxy

import (
	"strings"
	"testing"
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
