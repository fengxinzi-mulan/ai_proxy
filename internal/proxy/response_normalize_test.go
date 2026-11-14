package proxy

import (
	"encoding/json"
	"testing"
)

func decodeChunk(t *testing.T, data string) map[string]any {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal([]byte(data), &obj); err != nil {
		t.Fatalf("归一化结果不是合法 JSON: %v\n%s", err, data)
	}
	return obj
}

func chunkDelta(t *testing.T, data string) map[string]any {
	t.Helper()
	obj := decodeChunk(t, data)
	choices, ok := obj["choices"].([]any)
	if !ok || len(choices) == 0 {
		t.Fatalf("chunk 没有 choices: %s", data)
	}
	delta, ok := choices[0].(map[string]any)["delta"].(map[string]any)
	if !ok {
		t.Fatalf("chunk 没有 delta: %s", data)
	}
	return delta
}

// 上游把 delta.content 写成 thinking 分块数组时，客户端按 OpenAI 规范会直接
// 类型校验失败（ZCode 的 invalid_union、DSH 的 text-delta must be a string）。
func TestNormalizeChatChunkThinkingArray(t *testing.T) {
	data := `{"id":"x","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"index":0,"content":[{"type":"thinking","thinking":[{"type":"text","text":"The"}],"closed":true}]},"finish_reason":null}]}`

	out, changed := normalizeChatChunkData(data)
	if !changed {
		t.Fatal("含数组 content 的块应被归一化")
	}
	delta := chunkDelta(t, out)
	if delta["content"] != "" {
		t.Errorf("content 应变成空字符串，实际 %#v", delta["content"])
	}
	if delta["reasoning_content"] != "The" {
		t.Errorf("思考内容应挪到 reasoning_content，实际 %#v", delta["reasoning_content"])
	}
}

func TestNormalizeChatChunkTextArray(t *testing.T) {
	data := `{"choices":[{"index":0,"delta":{"content":[{"type":"text","text":"你好"},{"type":"text","text":"，世界"}]}}]}`

	out, changed := normalizeChatChunkData(data)
	if !changed {
		t.Fatal("文本数组应被归一化")
	}
	if got := chunkDelta(t, out)["content"]; got != "你好，世界" {
		t.Errorf("文本应拼成字符串，实际 %#v", got)
	}
}

func TestNormalizeChatChunkMixedArray(t *testing.T) {
	data := `{"choices":[{"delta":{"content":[{"type":"thinking","thinking":[{"type":"text","text":"想一想"}]},{"type":"text","text":"答案"}]}}]}`

	out, _ := normalizeChatChunkData(data)
	delta := chunkDelta(t, out)
	if delta["content"] != "答案" {
		t.Errorf("content 只应保留正文，实际 %#v", delta["content"])
	}
	if delta["reasoning_content"] != "想一想" {
		t.Errorf("思考内容应单独给出，实际 %#v", delta["reasoning_content"])
	}
}

func TestNormalizeChatChunkStringUntouched(t *testing.T) {
	data := `{"choices":[{"delta":{"content":"你好"}}]}`
	out, changed := normalizeChatChunkData(data)
	if changed || out != data {
		t.Errorf("字符串 content 不该被改写: changed=%v out=%s", changed, out)
	}
}

func TestNormalizeChatChunkDoneUntouched(t *testing.T) {
	for _, data := range []string{"[DONE]", "", "not-json"} {
		out, changed := normalizeChatChunkData(data)
		if changed || out != data {
			t.Errorf("%q 不该被改写", data)
		}
	}
}

// 非流式响应里的 message.content 同样是分块数组。
func TestNormalizeChatResponseBody(t *testing.T) {
	body := []byte(`{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"thinking","thinking":[{"type":"text","text":"先想"}],"closed":true},{"type":"text","text":"再答"}]},"finish_reason":"stop"}]}`)

	out, changed := normalizeChatResponseBody(body)
	if !changed {
		t.Fatal("非流式数组 content 应被归一化")
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("输出不是合法 JSON: %v", err)
	}
	msg := obj["choices"].([]any)[0].(map[string]any)["message"].(map[string]any)
	if msg["content"] != "再答" {
		t.Errorf("content 应只留正文，实际 %#v", msg["content"])
	}
	if msg["reasoning_content"] != "先想" {
		t.Errorf("思考内容应单独给出，实际 %#v", msg["reasoning_content"])
	}
}

// SSE 事件重建：只在没有 event 名时动手，且保持标准 data 行格式。
func TestNormalizeChatSSEEvent(t *testing.T) {
	data := `{"choices":[{"delta":{"content":[{"type":"text","text":"hi"}]}}]}`
	raw, newData, changed := normalizeChatSSEEvent("", data)
	if !changed {
		t.Fatal("应返回改写后的事件")
	}
	if string(raw) != "data: "+newData+"\n\n" {
		t.Errorf("事件字节格式不对: %q", raw)
	}

	if _, _, changed := normalizeChatSSEEvent("message", data); changed {
		t.Error("带 event 名的事件不该按 chat 规则改写")
	}
}
