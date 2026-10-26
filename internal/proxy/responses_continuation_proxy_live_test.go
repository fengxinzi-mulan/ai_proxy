package proxy

// 经 ai_proxy 的真实续传验证：同一组 12 张图，分别走「续传关闭」和
// 「续传开启」两个供应商，比较第二轮 TTFT、日志模式和回答正确性。
//
// 这是 Phase 0 的实验在代理层的延伸，默认跳过：
//
//	CONTINUATION_LIVE=1 CONTINUATION_PROXY_LIVE=1 \
//	RESPONSES_LIVE_MODEL=grok-4.7 \
//	  go test ./internal/proxy -run TestLiveOpenCodeResponsesContinuationViaProxy -v
//
// 只读 data/ai_proxy.db 取 opencode 密钥与代理设置；用临时库启动一个
// 进程内 ai_proxy，不碰正在运行的实例。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"ai_proxy/internal/hub"
	"ai_proxy/internal/model"
	"ai_proxy/internal/store"
)

func TestLiveOpenCodeResponsesContinuationViaProxy(t *testing.T) {
	if os.Getenv("CONTINUATION_LIVE") != "1" || os.Getenv("CONTINUATION_PROXY_LIVE") != "1" {
		t.Skip("未设置 CONTINUATION_LIVE=1 与 CONTINUATION_PROXY_LIVE=1，跳过代理层续传实验")
	}

	key, baseURL, settings := liveOpenCodeResponsesTarget(t)
	modelName := os.Getenv("RESPONSES_LIVE_MODEL")
	if modelName == "" {
		modelName = "grok-4.7"
	}

	st, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("打开临时库失败: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.SaveSettings(settings); err != nil {
		t.Fatalf("写入临时设置失败: %v", err)
	}

	create := func(name, mode string) {
		p := model.Provider{
			Name:               name,
			DisplayName:        name,
			Enabled:            true,
			BaseURL:            baseURL,
			APIFormat:          model.FormatResponses,
			ContinuationMode:   mode,
			ContinuationModels: []string{modelName},
			Keys:               []model.APIKey{{ID: "live", Key: key, Enabled: true}},
		}
		if _, err := st.CreateProvider(p); err != nil {
			t.Fatalf("创建供应商 %s 失败: %v", name, err)
		}
	}
	create("proxy-cont-on", model.ContinuationOn)
	create("proxy-cont-off", model.ContinuationOff)

	srv := NewServer(st, hub.New(), quietLogger())
	ts := httptest.NewServer(srv)
	defer ts.Close()
	client := &http.Client{Timeout: 240 * time.Second}

	imgs := generatedImagesForRep(t, 1)
	question := "第 7 张图（按图片顺序从 1 开始数）的主色是什么？只回答颜色名称，不要解释。"
	expected := "青色"

	run := func(path, session string) (continuationResult, continuationResult) {
		ref := path + "|" + session
		body1, input1 := continuationTurn1Body(modelName, session, imgs)
		body1 = continuationProxyStoreFalse(t, body1)
		first := sendContinuationRequest(t, client, ts.URL+path,
			continuationProxyHeaders(settings.AccessKey, session), body1)
		if first.status != http.StatusOK {
			t.Fatalf("%s 首轮返回 %d: %s", ref, first.status, continuationTrim(first.body, 500))
		}
		if first.id == "" {
			t.Fatalf("%s 首轮没有返回 response.id", ref)
		}
		output := first.output
		if len(output) == 0 {
			if first.text == "" {
				t.Fatalf("%s 首轮没有内容输出", ref)
			}
			output = []json.RawMessage{mustJSON(contAssistant(first.text))}
		}
		full := continuationFullTurn2Body(modelName, session, input1, output, question, false)
		second := sendContinuationRequest(t, client, ts.URL+path,
			continuationProxyHeaders(settings.AccessKey, session), full)
		if second.status != http.StatusOK {
			t.Fatalf("%s 第二轮返回 %d: %s", ref, second.status, continuationTrim(second.body, 500))
		}
		return first, second
	}

	onFirst, onSecond := run("/p/proxy-cont-on/v1/responses", "proxy-cont-on-session")
	onLog := lastLog(t, st)
	offFirst, offSecond := run("/p/proxy-cont-off/v1/responses", "proxy-cont-off-session")
	offLog := lastLog(t, st)

	t.Logf("续传开启：首轮 ttft=%dms total=%dms bytes=%d；第二轮 ttft=%dms total=%dms bytes=%d mode=%s reason=%s saved=%d",
		onFirst.ttft.Milliseconds(), onFirst.total.Milliseconds(), onFirst.sentBytes,
		onSecond.ttft.Milliseconds(), onSecond.total.Milliseconds(), onSecond.sentBytes,
		onLog.ContinuationMode, onLog.ContinuationReason, onLog.SavedBytes)
	t.Logf("续传关闭：首轮 ttft=%dms total=%dms bytes=%d；第二轮 ttft=%dms total=%dms bytes=%d mode=%q",
		offFirst.ttft.Milliseconds(), offFirst.total.Milliseconds(), offFirst.sentBytes,
		offSecond.ttft.Milliseconds(), offSecond.total.Milliseconds(), offSecond.sentBytes,
		offLog.ContinuationMode)

	if onLog.ContinuationMode != "continued" {
		t.Errorf("续传开启的第二轮应记 continued，实际 %q（原因 %q）", onLog.ContinuationMode, onLog.ContinuationReason)
	}
	if onLog.SavedBytes <= 0 {
		t.Errorf("续传开启应记录节省字节，实际 %d", onLog.SavedBytes)
	}
	if !onLog.RequestModified {
		t.Error("续传开启应标记 RequestModified")
	}
	if offLog.ContinuationMode != "" || offLog.RequestModified {
		t.Errorf("续传关闭不应改写，实际 mode=%q modified=%v", offLog.ContinuationMode, offLog.RequestModified)
	}
	if !continuationMatchesColor(onSecond.text, expected) {
		t.Errorf("续传第二轮回答应包含 %s，实际 %q", expected, continuationTrim(onSecond.text, 120))
	}
	if !continuationMatchesColor(offSecond.text, expected) {
		t.Errorf("全量第二轮回答应包含 %s，实际 %q", expected, continuationTrim(offSecond.text, 120))
	}
	if onSecond.ttft > 0 && offSecond.ttft > 0 && onSecond.ttft >= offSecond.ttft {
		t.Logf("警告：本次续传 TTFT 未低于全量（续传=%dms 全量=%dms），可能是网络抖动或模型侧波动",
			onSecond.ttft.Milliseconds(), offSecond.ttft.Milliseconds())
	}
}

func continuationProxyStoreFalse(t *testing.T, body []byte) []byte {
	t.Helper()
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		t.Fatalf("解析首轮请求体失败: %v", err)
	}
	obj["store"] = false
	out, err := json.Marshal(obj)
	if err != nil {
		t.Fatalf("重新编码首轮请求体失败: %v", err)
	}
	return out
}

func continuationProxyHeaders(accessKey, session string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/json")
	h.Set("Accept", "text/event-stream")
	h.Set("X-Session-Id", session)
	h.Set("X-Opencode-Session", session)
	if accessKey != "" {
		h.Set("X-AI-Proxy-Key", accessKey)
	}
	return h
}
